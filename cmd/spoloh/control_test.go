package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/control"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/player"
)

// A control socket that cannot be created is a device with no command line.
// It is not a device that stops announcing air raid alerts, and it is not a
// device that has to be restarted before the command line comes back — a
// read-only /run, a permission problem or a leftover file all stop being true
// the moment somebody fixes them, and this has to notice.
func TestTheControlSocketFailingDoesNotStopTheDaemon(t *testing.T) {
	// A path that cannot be a socket directory, because a regular file is in
	// the way of its parent. Every attempt fails, for a reason the operator
	// could plausibly create.
	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, []byte("a file where a directory should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocked, "spoloh", "control.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		serveControl(ctx, path, &recordingDevice{}, discardLogger())
		close(returned)
	}()

	// Long enough to cover the first back-off, so a serveControl that gave
	// up after one failure is caught rather than raced past.
	select {
	case <-returned:
		t.Fatal("serveControl gave up when the socket could not be created; the daemon would have lost its command line until a restart")
	case <-time.After(1500 * time.Millisecond):
	}

	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("serveControl kept retrying after the context was cancelled")
	}
}

// fakeBackend is a player backend that blocks in Play until it is released,
// so a test can look at what the player is holding while it holds it. It
// records nothing it is not asked about: what matters here is which request
// reached the single voice, not what ALSA would have done with it.
type fakeBackend struct {
	playing chan string
	release chan struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{playing: make(chan string, 4), release: make(chan struct{})}
}

func (b *fakeBackend) SetVolume(ctx context.Context, device string, pct int) error { return nil }

func (b *fakeBackend) Play(ctx context.Context, device, path string) error {
	b.playing <- path
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

// `spoloh play` must not be a privileged bypass. It has to reach the speaker
// through Engine.PlayManual — the same call the dashboard's /play button
// makes — so there is one voice, one priority queue and one event log, and a
// sound started from a shell behaves like a sound started from the browser.
//
// A second playback path would be a device that can be made deaf to an air
// raid from a terminal, and nothing in the queue would know.
func TestControlPlayGoesThroughTheEngineAndItsQueue(t *testing.T) {
	dir := t.TempDir()
	sounds := filepath.Join(dir, "sounds")
	if err := os.MkdirAll(sounds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sounds, "siren.wav"), []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := library.New(sounds, nil)

	lg, err := eventlog.Open(filepath.Join(dir, "spoloh.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	backend := newFakeBackend()
	pl := player.New(backend, "plughw:1,0")

	cfg := config.Default()
	cfg.Audio.MasterVolume = 73
	cfg.Audio.Device = "plughw:1,0"

	eng := engine.New(engine.Deps{
		Config:  func() *config.Config { return cfg },
		Player:  pl,
		Library: lib,
		Log:     lg,
		Clock:   clock.NewFake(time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)),
		Logger:  discardLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pl.Run(ctx)

	dev := controlDevice{engine: eng, lib: lib, log: lg, cfg: func() *config.Config { return cfg }}
	if err := dev.Play("siren.wav"); err != nil {
		t.Fatalf("play: %v", err)
	}

	select {
	case got := <-backend.playing:
		if got != filepath.Join(sounds, "siren.wav") {
			t.Errorf("the speaker got %q, want the library's siren.wav", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the player; `spoloh play` did not go through the engine")
	}

	// The request the single voice is holding is the engine's, at the
	// priority the engine assigns a manual play — not one this package built
	// for itself.
	cur, ok := pl.Current()
	if !ok {
		t.Fatal("the player is not holding a request")
	}
	if cur.EventID != "manual" {
		t.Errorf("the player is holding event %q, want \"manual\"; the request did not come from Engine.PlayManual", cur.EventID)
	}
	if cur.Priority != player.PriorityAlert {
		t.Errorf("the request is at priority %v, want %v; it was not queued the way an announcement is", cur.Priority, player.PriorityAlert)
	}
	if cur.Volume != 73 {
		t.Errorf("the request is at volume %d, want the configured master volume 73", cur.Volume)
	}

	// And the site's record of it. A play that left no entry would be a
	// siren nobody can account for afterwards.
	entries, err := lg.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Kind == eventlog.KindManual && e.Sound == "siren.wav" {
			found = true
		}
	}
	if !found {
		t.Errorf("the event log has no manual entry for siren.wav; entries: %+v", entries)
	}

	close(backend.release)
}

// Stop is the other half, and it goes through the engine for the same reason:
// stopping is the one command that makes this device go quiet, so it has to
// be in the log.
func TestControlStopIsRecorded(t *testing.T) {
	dir := t.TempDir()
	lg, err := eventlog.Open(filepath.Join(dir, "spoloh.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	cfg := config.Default()
	eng := engine.New(engine.Deps{
		Config: func() *config.Config { return cfg },
		Log:    lg,
		Clock:  clock.NewFake(time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)),
		Logger: discardLogger(),
	})

	controlDevice{engine: eng, log: lg, cfg: func() *config.Config { return cfg }}.Stop()

	entries, err := lg.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != eventlog.KindManual || entries[0].EventID != "stop" {
		t.Errorf("a stop from the command line left %+v in the log, want one manual stop entry", entries)
	}
}

// Adding subcommands must not change how systemd starts this binary. The
// daemon is started by deploy/spoloh.service with flags only, and
// subcommand() takes a verb only from a bare first argument — so the unit
// that has been in the field since before any of this existed still starts a
// daemon rather than running a command and exiting.
//
// The unit file is read rather than assumed: a later edit that put a verb in
// ExecStart would turn every boot into a one-shot CLI invocation, and nothing
// else in this repository would notice.
func TestTheControlVerbsDidNotChangeHowSystemdStartsTheDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"spoloh"},
		{"spoloh", "-config", "/etc/spoloh/config.json", "-data", "/var/lib/spoloh"},
		{"spoloh", "-socket", "/run/spoloh/control.sock"},
	} {
		if got := subcommand(args); got != "" {
			t.Errorf("subcommand(%q) = %q; the daemon would not start", args, got)
		}
	}
	for _, verb := range []string{"status", "sounds", "play", "stop", "log", "version"} {
		if got := subcommand([]string{"spoloh", verb}); got != verb {
			t.Errorf("subcommand(spoloh %s) = %q", verb, got)
		}
		if !control.IsCommand(verb) {
			t.Errorf("main would not dispatch %q to the control socket", verb)
		}
	}

	unit, err := os.ReadFile("../../deploy/spoloh.service")
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	m := regexp.MustCompile(`(?m)^ExecStart=(.*)$`).FindStringSubmatch(string(unit))
	if m == nil {
		t.Fatal("the unit has no ExecStart=")
	}
	// ExecStart's fields have the same shape as os.Args — the binary, then
	// its arguments — so the production dispatcher can be asked directly
	// what it would make of the line systemd actually runs.
	fields := strings.Fields(m[1])
	if got := subcommand(fields); got != "" {
		t.Errorf("ExecStart reads as the %q command (%q); systemd would run a one-shot CLI invocation on every boot instead of starting the daemon", got, m[1])
	}
}

// recordingDevice is the minimum control.Device for the retry test above,
// which never gets as far as a request.
type recordingDevice struct {
	mu sync.Mutex
}

func (d *recordingDevice) Status() control.Status           { return control.Status{} }
func (d *recordingDevice) Sounds() ([]string, error)        { return nil, nil }
func (d *recordingDevice) Play(string) error                { return errors.New("not wired") }
func (d *recordingDevice) Stop()                            {}
func (d *recordingDevice) Log(int) ([]control.Entry, error) { return nil, nil }
func (d *recordingDevice) Version() control.Version         { return control.Version{} }

// `spoloh status` is what somebody with a shell on the board reads when the
// dashboard is not in front of them, and last_update is the line they read
// first. Forty minutes old means one thing on a device that cannot reach its
// sources and something else entirely on one that has stopped trying on
// purpose; without poll_paused beside it the two are the same line, and one
// of them is a fault.
//
// The mapping is what this pins, not the printing. internal/control's own
// tests hand the printer a Status they built by hand, so a field that stops
// being filled in here would still print — as "no", on a device that is
// paused.
func TestControlStatusCarriesThePausedPoll(t *testing.T) {
	cfg := config.Default()
	feed := &netmgr.StatusFeed{}
	clk := clock.NewFake(time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC))
	eng := engine.New(engine.Deps{
		Config:  func() *config.Config { return cfg },
		Machine: alertstate.New(),
		// Never Run: engine.Status only asks the player what is on the
		// speaker, which on one that was never started is nothing.
		Player:  player.New(nil, "plughw:1,0"),
		Network: feed,
		Clock:   clk,
		Logger:  discardLogger(),
	})
	dev := controlDevice{engine: eng, cfg: func() *config.Config { return cfg }}

	if dev.Status().PollPaused {
		t.Error("a device nobody has reported a network for says its poll is paused")
	}

	// The reading a supervisor takes on a device that lost its network,
	// waited out the grace period and raised its own access point.
	feed.Publish(netmgr.Status{HotspotUp: true}, clk.Now())
	eng.Tick(context.Background())

	if !dev.Status().PollPaused {
		t.Error("the engine stopped polling and `spoloh status` does not say so")
	}
}
