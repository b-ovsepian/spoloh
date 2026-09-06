package engine_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/alertsource"
	"github.com/spoloh/spoloh/internal/alertstate"
	"github.com/spoloh/spoloh/internal/clock"
	"github.com/spoloh/spoloh/internal/config"
	"github.com/spoloh/spoloh/internal/engine"
	"github.com/spoloh/spoloh/internal/eventlog"
	"github.com/spoloh/spoloh/internal/library"
	"github.com/spoloh/spoloh/internal/player"
)

type recorder struct {
	mu     sync.Mutex
	played []string
	ch     chan string
}

func newRecorder() *recorder { return &recorder{ch: make(chan string, 8)} }

func (r *recorder) SetVolume(context.Context, string, int) error { return nil }

func (r *recorder) Play(_ context.Context, _, path string) error {
	r.mu.Lock()
	r.played = append(r.played, filepath.Base(path))
	r.mu.Unlock()
	r.ch <- filepath.Base(path)
	return nil
}

func (r *recorder) Played() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.played))
	copy(out, r.played)
	return out
}

func snapshotBody(alert bool) string {
	return fmt.Sprintf(`{"source":"test","cachedat":"2026-09-06 15:39:05",
		"states":{"м. Київ":{"alertnow":%t,"changed":"2026-09-06 15:38:00"}}}`, alert)
}

type harness struct {
	eng  *engine.Engine
	rec  *recorder
	log  *eventlog.Log
	set  func(alert bool)
	fail func(bool)
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	var (
		mu     sync.Mutex
		alert  bool
		broken bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		a, b := alert, broken
		mu.Unlock()
		if b {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, snapshotBody(a))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	soundsDir := filepath.Join(dir, "sounds")
	if err := os.MkdirAll(soundsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	riff := make([]byte, 64)
	copy(riff, "RIFF")
	copy(riff[8:], "WAVE")
	for _, name := range []string{"siren.wav", "all-clear.wav", "metronome.wav"} {
		if err := os.WriteFile(filepath.Join(soundsDir, name), riff, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	lg, err := eventlog.Open(filepath.Join(dir, "spoloh.db"), 100)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { lg.Close() })

	rec := newRecorder()
	pl := player.New(rec, "default")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pl.Run(ctx)

	cfg := config.Default()
	cfg.Alerts.Region = "м. Київ"
	cfg.Alerts.Sources = []string{"default"}

	eng := engine.New(engine.Deps{
		Config:    func() *config.Config { return cfg },
		StatePath: filepath.Join(dir, "state.json"),
		Source:    alertsource.New(srv.URL, cfg.Alerts.Sources, srv.Client()),
		Machine:   alertstate.New(),
		Player:    pl,
		Library:   library.New(soundsDir, library.FFmpeg{}),
		Log:       lg,
		Clock:     clock.System{Loc: time.UTC},
		Sync:      &clock.FakeSync{Value: true},
	})

	return &harness{
		eng:  eng,
		rec:  rec,
		log:  lg,
		set:  func(v bool) { mu.Lock(); alert = v; mu.Unlock() },
		fail: func(v bool) { mu.Lock(); broken = v; mu.Unlock() },
	}
}

func waitPlayed(t *testing.T, r *recorder) string {
	t.Helper()
	select {
	case p := <-r.ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for playback")
		return ""
	}
}

func TestAlertStartPlaysSiren(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.eng.PollOnce(ctx); err != nil { // quiet baseline
		t.Fatalf("PollOnce: %v", err)
	}
	h.set(true)
	if err := h.eng.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want siren.wav", got)
	}
}

func TestAlertEndPlaysAllClear(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx)
	h.set(true)
	h.eng.PollOnce(ctx)
	waitPlayed(t, h.rec)

	h.set(false)
	h.eng.PollOnce(ctx)
	if got := waitPlayed(t, h.rec); got != "all-clear.wav" {
		t.Fatalf("played %q, want all-clear.wav", got)
	}
}

// The behaviour from §5.2 of the spec, verified end to end: a device that boots
// into a running alert stays quiet.
func TestBootDuringActiveAlertStaysSilent(t *testing.T) {
	h := newHarness(t)
	h.set(true)

	if err := h.eng.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if got := h.rec.Played(); len(got) != 0 {
		t.Fatalf("played %v on boot, want nothing", got)
	}
}

func TestSustainedAlertPlaysOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx)
	h.set(true)
	for i := 0; i < 12; i++ {
		if err := h.eng.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)

	if got := h.rec.Played(); len(got) != 1 {
		t.Fatalf("played %v, want exactly one siren", got)
	}
}

func TestSourceFailureIsLoggedAndDoesNotPlay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx)
	h.fail(true)
	if err := h.eng.PollOnce(ctx); err == nil {
		t.Fatal("expected a poll error")
	}
	time.Sleep(200 * time.Millisecond)

	if got := h.rec.Played(); len(got) != 0 {
		t.Fatalf("played %v on a source failure, want nothing", got)
	}
	entries, err := h.log.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Kind == eventlog.KindSourceFailure {
			found = true
		}
	}
	if !found {
		t.Fatalf("no source failure recorded: %+v", entries)
	}
}

// A failure must not corrupt the baseline: recovery still reports the change.
func TestRecoveryAfterFailureStillFires(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx)
	h.fail(true)
	h.eng.PollOnce(ctx)

	h.fail(false)
	h.set(true)
	if err := h.eng.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want siren.wav", got)
	}
}

func TestPlayManual(t *testing.T) {
	h := newHarness(t)

	if err := h.eng.PlayManual("siren.wav", 100); err != nil {
		t.Fatalf("PlayManual: %v", err)
	}
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want siren.wav", got)
	}
}

func TestPlayManualRejectsUnknownSound(t *testing.T) {
	h := newHarness(t)
	if err := h.eng.PlayManual("nope.wav", 100); err == nil {
		t.Fatal("expected an error for a sound that is not in the library")
	}
}
