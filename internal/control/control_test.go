package control_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/control"
)

// A unix socket path is capped at around 104 bytes by the kernel, and
// t.TempDir() names its directory after the test — which for the names in
// this file is long enough to cross that on macOS, with an error
// ("invalid argument") that says nothing about why. A short temporary
// directory of our own keeps the path well inside the limit, and the check
// below turns a future long name into a message that explains itself.
//
// The socket is local: it opens nothing on the network, so the constraint
// against sockets in tests does not reach it.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	if len(path) > 90 {
		t.Fatalf("socket path %q is %d bytes; the kernel's limit is about 104 and Listen appends a pid suffix", path, len(path))
	}
	return path
}

type fakeDevice struct {
	mu      sync.Mutex
	status  control.Status
	sounds  []string
	entries []control.Entry
	version control.Version

	played  []string
	playErr error
	stops   int
	askedN  int
}

func (d *fakeDevice) Status() control.Status { return d.status }
func (d *fakeDevice) Sounds() ([]string, error) {
	return d.sounds, nil
}
func (d *fakeDevice) Play(sound string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.played = append(d.played, sound)
	return d.playErr
}
func (d *fakeDevice) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops++
}
func (d *fakeDevice) Log(n int) ([]control.Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.askedN = n
	return d.entries, nil
}
func (d *fakeDevice) Version() control.Version { return d.version }

// kyiv is a fixed offset rather than a loaded zone: the point of these tests
// is that whatever location the daemon put on a time survives the JSON and
// reaches the terminal, not what Europe/Kyiv was doing in September.
var kyiv = time.FixedZone("EET", 3*60*60)

func fullDevice() *fakeDevice {
	return &fakeDevice{
		status: control.Status{
			Alert:       true,
			Since:       time.Date(2026, 9, 12, 14, 3, 11, 0, kyiv),
			Region:      "Львівська область",
			Source:      "default",
			LastUpdate:  time.Date(2026, 9, 12, 14, 5, 0, 0, kyiv),
			ClockSynced: true,
			Output:      "plughw:1,0",
			Playing:     "siren.wav",
		},
		sounds: []string{"all-clear.wav", "siren.wav"},
		entries: []control.Entry{
			{At: time.Date(2026, 9, 12, 14, 3, 11, 0, kyiv), Kind: "alert", Event: "alert_start", Sound: "siren.wav", OK: true},
			{At: time.Date(2026, 9, 12, 13, 0, 0, 0, kyiv), Kind: "playback_error", Sound: "chime.wav", Err: "audio open error"},
		},
		version: control.Version{Version: "v1.4.0", Arch: "linux-arm64"},
	}
}

// serveTest starts a daemon on its own socket and returns the path. It is the
// whole server-plus-client round trip: nothing here stubs the transport, so a
// mistake in the protocol shows up as wrong output rather than as a passing
// test against a mock that agreed with it.
func serveTest(t *testing.T, dev control.Device) string {
	t.Helper()
	path := tempSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- control.Serve(ctx, path, dev, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after the context was cancelled")
		}
	})
	waitForSocket(t, path)
	return path
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the daemon never started listening on %s", path)
}

// The one test that pins every command's shape end to end. The exact bytes
// matter: a script at a site reads these lines, and a field that changes
// spelling or disappears when the device is quiet breaks it silently.
func TestEachCommandPrintsItsOwnShape(t *testing.T) {
	dev := fullDevice()
	socket := serveTest(t, dev)

	for _, tc := range []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{
			name: "status",
			cmd:  control.CmdStatus,
			want: "alert: yes\n" +
				"since: 2026-09-12T14:03:11+03:00\n" +
				"region: Львівська область\n" +
				"source: default\n" +
				"last_update: 2026-09-12T14:05:00+03:00\n" +
				"poll_paused: no\n" +
				"source_error: -\n" +
				"clock_synced: yes\n" +
				"output: plughw:1,0\n" +
				"playing: siren.wav\n",
		},
		{
			name: "sounds",
			cmd:  control.CmdSounds,
			want: "all-clear.wav\nsiren.wav\n",
		},
		{
			name: "play",
			cmd:  control.CmdPlay,
			args: []string{"siren.wav"},
			want: "playing siren.wav\n",
		},
		{
			name: "stop",
			cmd:  control.CmdStop,
			want: "stopped\n",
		},
		{
			name: "log",
			cmd:  control.CmdLog,
			want: "2026-09-12T14:03:11+03:00\talert\tok\talert_start\tsiren.wav\t-\n" +
				"2026-09-12T13:00:00+03:00\tplayback_error\tfailed\t-\tchime.wav\taudio open error\n",
		},
		{
			name: "version",
			cmd:  control.CmdVersion,
			want: "version: v1.4.0\narch: linux-arm64\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			args := append([]string{"-socket", socket}, tc.args...)
			if code := control.Run(tc.cmd, args, &out); code != 0 {
				t.Fatalf("exit %d, output:\n%s", code, out.String())
			}
			if out.String() != tc.want {
				t.Errorf("output was:\n%q\nwant:\n%q", out.String(), tc.want)
			}
		})
	}

	dev.mu.Lock()
	defer dev.mu.Unlock()
	if len(dev.played) != 1 || dev.played[0] != "siren.wav" {
		t.Errorf("the daemon was asked to play %v, want [siren.wav]", dev.played)
	}
	if dev.stops != 1 {
		t.Errorf("the daemon was stopped %d times, want 1", dev.stops)
	}
}

// A quiet device must print the same fields as a loud one, so `spoloh status
// | grep ^alert:` does not start returning nothing the moment the all-clear
// sounds.
func TestStatusPrintsEveryFieldOnAQuietDevice(t *testing.T) {
	socket := serveTest(t, &fakeDevice{status: control.Status{Region: "Київська область"}})

	var out strings.Builder
	if code := control.Run(control.CmdStatus, []string{"-socket", socket}, &out); code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, out.String())
	}
	want := "alert: no\n" +
		"since: -\n" +
		"region: Київська область\n" +
		"source: -\n" +
		"last_update: -\n" +
		"poll_paused: no\n" +
		"source_error: -\n" +
		"clock_synced: no\n" +
		"output: -\n" +
		"playing: -\n"
	if out.String() != want {
		t.Errorf("output was:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestLogPassesTheRequestedCount(t *testing.T) {
	dev := &fakeDevice{}
	socket := serveTest(t, dev)

	var out strings.Builder
	if code := control.Run(control.CmdLog, []string{"-socket", socket, "-n", "5"}, &out); code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, out.String())
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.askedN != 5 {
		t.Errorf("the daemon was asked for %d entries, want 5", dev.askedN)
	}
}

// A sound that is not in the library is the daemon's answer, not the client's
// guess: the client never builds a path or checks one, so there is only one
// place that decides what is playable.
func TestAPlayTheDaemonRefusesIsReportedAndExitsNonZero(t *testing.T) {
	socket := serveTest(t, &fakeDevice{playErr: errors.New(`sound "nope.wav" is not in the library`)})

	var out strings.Builder
	code := control.Run(control.CmdPlay, []string{"-socket", socket, "nope.wav"}, &out)
	if code == 0 {
		t.Fatalf("a refused play exited 0, output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "not in the library") {
		t.Errorf("the refusal did not say why:\n%s", out.String())
	}
}

// The message when nothing is listening is the whole of the operator's
// support experience for this feature, so it is pinned: it has to name the
// socket and point at the one command that says what the daemon is doing.
func TestTheClientSaysTheDaemonIsNotRunning(t *testing.T) {
	// tempSocket names a path and creates nothing at it, which is exactly
	// the state a stopped daemon leaves behind on a tmpfs /run.
	socket := tempSocket(t)

	var out strings.Builder
	code := control.Run(control.CmdStatus, []string{"-socket", socket}, &out)
	if code != 1 {
		t.Fatalf("exit %d, want 1; output:\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, socket) {
		t.Errorf("the failure did not name the socket:\n%s", got)
	}
	if !strings.Contains(got, "is not running") || !strings.Contains(got, "systemctl status spoloh") {
		t.Errorf("the failure did not say what to do about it:\n%s", got)
	}
}

// Socket permissions are the authorisation model, so being refused by them is
// the expected experience for every account that has not been added to the
// group — including the operator's own, the first time. The message has to
// carry the fix, because nothing else on the device will tell them.
func TestTheClientSaysHowToBeAllowedWhenItIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by file permissions")
	}
	socket := serveTest(t, &fakeDevice{})
	// 0000: the running user is not the owner-with-write the socket needs,
	// which is exactly the kernel's answer to an account outside the group.
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	code := control.Run(control.CmdStatus, []string{"-socket", socket}, &out)
	if code != 1 {
		t.Fatalf("exit %d, want 1; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "usermod -aG spoloh") {
		t.Errorf("a permission denial did not say how to get permission:\n%s", out.String())
	}
}

// The socket is created 0660 and nothing else. A mode that let the world
// connect would hand "sound the siren" to every account on the board; one
// that did not let the group connect would make the CLI unusable by the
// people it exists for, because connecting to a unix socket needs write.
func TestTheSocketIsReadableAndWritableOnlyByItsOwnerAndGroup(t *testing.T) {
	socket := serveTest(t, &fakeDevice{})
	fi, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o660 {
		t.Errorf("the socket is mode %04o, want 0660", got)
	}
}

// The stale case: a socket file with nothing behind it, which is what an
// unclean shutdown leaves. It must not stop the daemon coming back.
func TestAStaleSocketFileDoesNotStopTheDaemon(t *testing.T) {
	path := tempSocket(t)

	// A real leftover, not an imitation: bind a listener, tell it not to
	// unlink on close, then close it. What is on disk afterwards is exactly
	// what a killed daemon leaves — the right file type, the right name,
	// and no process behind it. A plain file would be a more permissive
	// fake than the real thing.
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the leftover socket was not left behind: %v", err)
	}

	ln, err := control.Listen(path)
	if err != nil {
		t.Fatalf("a stale socket file stopped the daemon listening: %v", err)
	}
	defer ln.Close()

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the replaced socket does not answer: %v", err)
	}
	c.Close()
}

// The other half of the same decision, and the dangerous one: a socket with a
// live daemon behind it must never be replaced. Doing so would leave that
// daemon serving an inode no CLI can reach — a device that looks managed and
// is not.
func TestListenRefusesToReplaceALiveSocket(t *testing.T) {
	path := tempSocket(t)
	first, err := control.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := control.Listen(path)
	if err == nil {
		second.Close()
		t.Fatal("a second listener took over a socket another daemon was serving")
	}
	if !errors.Is(err, control.ErrAlreadyListening) {
		t.Errorf("refused with %v, want ErrAlreadyListening", err)
	}

	// The live listener is still the one on the path, and still accepting.
	accepted := make(chan struct{})
	go func() {
		c, err := first.Accept()
		if err == nil {
			c.Close()
		}
		close(accepted)
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the live daemon's socket stopped answering: %v", err)
	}
	c.Close()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Error("the live listener never saw the connection; its socket was replaced")
	}
}

// Anything at the path that is not a socket belongs to somebody else. The
// daemon refuses rather than deleting a file it did not create.
func TestListenRefusesToReplaceSomethingThatIsNotASocket(t *testing.T) {
	path := tempSocket(t)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := control.Listen(path)
	if err == nil {
		ln.Close()
		t.Fatal("Listen replaced a regular file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file was removed anyway: %v", err)
	}
}

// A daemon asked to stop must stop, and must not leave the accept loop
// reporting the closed listener as a failure worth retrying.
func TestServeReturnsCleanlyWhenTheContextIsCancelled(t *testing.T) {
	path := tempSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- control.Serve(ctx, path, &fakeDevice{}, slog.New(slog.DiscardHandler)) }()
	waitForSocket(t, path)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v on a clean shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

func TestIsCommandKnowsOnlyItsOwnVerbs(t *testing.T) {
	for _, verb := range []string{"status", "sounds", "play", "stop", "log", "version"} {
		if !control.IsCommand(verb) {
			t.Errorf("IsCommand(%q) = false", verb)
		}
	}
	for _, other := range []string{"", "update", "-config", "Status", "restart"} {
		if control.IsCommand(other) {
			t.Errorf("IsCommand(%q) = true", other)
		}
	}
}
