package engine_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	rate func(retryAfter time.Duration) // 0 disables rate-limiting
	hits func() int                     // number of HTTP requests the source has served
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, alertstate.New(), clock.System{Loc: time.UTC})
}

// newHarnessWithMachine builds a harness around a caller-supplied machine, so
// a test can exercise a machine that was Restore'd before the first poll —
// the real boot path in cmd/spoloh, which a bare alertstate.New() does not
// reproduce.
func newHarnessWithMachine(t *testing.T, m *alertstate.Machine) *harness {
	t.Helper()
	return newHarnessWith(t, m, clock.System{Loc: time.UTC})
}

// newHarnessWithClock builds a harness driven by the given clock, for tests
// that exercise Tick's scheduling (poll gating, backoff, the backwards-clock
// guard) rather than just PollOnce.
func newHarnessWithClock(t *testing.T, clk clock.Clock) *harness {
	t.Helper()
	return newHarnessWith(t, alertstate.New(), clk)
}

func newHarnessWith(t *testing.T, machine *alertstate.Machine, clk clock.Clock) *harness {
	t.Helper()

	var (
		mu         sync.Mutex
		alert      bool
		broken     bool
		retryAfter time.Duration
	)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		mu.Lock()
		a, b, ra := alert, broken, retryAfter
		mu.Unlock()
		if ra > 0 {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(ra.Seconds())))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
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
		Machine:   machine,
		Player:    pl,
		Library:   library.New(soundsDir, library.FFmpeg{}),
		Log:       lg,
		Clock:     clk,
		Sync:      &clock.FakeSync{Value: true},
	})

	return &harness{
		eng:  eng,
		rec:  rec,
		log:  lg,
		set:  func(v bool) { mu.Lock(); alert = v; mu.Unlock() },
		fail: func(v bool) { mu.Lock(); broken = v; mu.Unlock() },
		rate: func(d time.Duration) { mu.Lock(); retryAfter = d; mu.Unlock() },
		hits: func() int { return int(atomic.LoadInt32(&hits)) },
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

// Restore is the real boot path (cmd/spoloh calls it unconditionally), and it
// deliberately leaves the machine un-baselined — a bare alertstate.New() does
// not exercise that at all. This is the "restored state agrees with the
// first poll" case; on its own it does NOT discriminate a Restore that wrongly
// baselines the machine, because when persisted and polled states already
// match, Apply returns NoChange either way. See
// TestBootWithRestoredActiveAlertIgnoresContradictingFirstPoll below for the
// case that only a genuinely un-baselined machine can pass.
func TestBootWithRestoredActiveAlertStaysSilent(t *testing.T) {
	m := alertstate.New()
	m.Restore(alertstate.Persisted{Alert: true, Since: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)})

	h := newHarnessWithMachine(t, m)
	h.set(true)

	if err := h.eng.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if got := h.rec.Played(); len(got) != 0 {
		t.Fatalf("played %v after restoring an already-active alert, want nothing", got)
	}
}

// TestBootWithRestoredActiveAlertIgnoresContradictingFirstPoll is the
// discriminating case the test above cannot be: it restores an active alert,
// then polls a state that DISAGREES with it. A correctly un-baselined machine
// treats this as a silent baseline, same as any first poll. A Restore that
// (incorrectly) set baselined too would instead see a mismatch against the
// restored alert and fire alertstate.Ended — a phantom all-clear announced
// on every reboot whose alert has since ended.
func TestBootWithRestoredActiveAlertIgnoresContradictingFirstPoll(t *testing.T) {
	m := alertstate.New()
	m.Restore(alertstate.Persisted{Alert: true, Since: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)})

	h := newHarnessWithMachine(t, m)
	h.set(false) // the fresh poll disagrees with the restored state

	if err := h.eng.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if got := h.rec.Played(); len(got) != 0 {
		t.Fatalf("played %v after a first poll contradicting the restored state, want nothing (silent baseline, not a phantom all-clear)", got)
	}
}

// TestTickPollsWhenDueNotBefore exercises Tick's own gating rather than
// calling PollOnce directly: a poll happens once due, and not before.
func TestTickPollsWhenDueNotBefore(t *testing.T) {
	base := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	h := newHarnessWithClock(t, clk)
	ctx := context.Background()

	h.eng.Tick(ctx) // nextPoll starts at the zero time: always due
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after the first tick = %d, want 1", got)
	}

	h.eng.Tick(ctx) // same instant: the next poll is a few seconds out
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after an immediate second tick = %d, want 1 (not due yet)", got)
	}

	clk.Set(base.Add(7 * time.Second)) // past the max jittered 5s+1s interval
	h.eng.Tick(ctx)
	if got := h.hits(); got != 2 {
		t.Fatalf("hits after advancing past the interval = %d, want 2", got)
	}
}

// TestTickAttemptResetsAfterRecovery drives two separate failure streaks
// through Tick and checks, via the observable backoff timing, that a
// recovery resets the attempt counter rather than letting the second
// streak's backoff continue growing from the first.
func TestTickAttemptResetsAfterRecovery(t *testing.T) {
	base := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	h := newHarnessWithClock(t, clk)
	ctx := context.Background()

	h.eng.Tick(ctx) // baseline success
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after baseline = %d, want 1", got)
	}

	h.fail(true)
	firstFailAt := base.Add(7 * time.Second)
	clk.Set(firstFailAt)
	h.eng.Tick(ctx) // first failure: attempt=1, backoff=Backoff(1)=15s (deterministic, no jitter)
	if got := h.hits(); got != 2 {
		t.Fatalf("hits after the first failure = %d, want 2", got)
	}

	clk.Set(firstFailAt.Add(14 * time.Second)) // still inside the 15s backoff
	h.eng.Tick(ctx)
	if got := h.hits(); got != 2 {
		t.Fatalf("hits at +14s = %d, want 2 (should not have polled yet)", got)
	}

	h.fail(false)
	recoverAt := firstFailAt.Add(16 * time.Second) // past the 15s backoff
	clk.Set(recoverAt)
	h.eng.Tick(ctx) // recovery poll: attempt resets to 0
	if got := h.hits(); got != 3 {
		t.Fatalf("hits after the recovery poll = %d, want 3", got)
	}

	// A second failure right after recovery must restart the backoff at 15s,
	// not continue from the first streak's attempt count (which would give
	// 30s). Fire it well past any jittered poll interval (max 6s).
	h.fail(true)
	secondFailAt := recoverAt.Add(7 * time.Second)
	clk.Set(secondFailAt)
	h.eng.Tick(ctx)
	if got := h.hits(); got != 4 {
		t.Fatalf("hits after the second failure = %d, want 4", got)
	}

	// Let the next poll succeed, so the +20s check below can only be
	// explained by the backoff scheduled after the second failure, not by a
	// third failure happening to also be due by then.
	h.fail(false)

	// 20s later is past a reset 15s backoff but short of a grown 30s one.
	clk.Set(secondFailAt.Add(20 * time.Second))
	h.eng.Tick(ctx)
	if got := h.hits(); got != 5 {
		t.Fatalf("hits at +20s after the second failure = %d, want 5 — "+
			"the attempt counter did not reset after the recovery poll, "+
			"so backoff kept growing", got)
	}
}

// TestTickRetryAfterOverridesBackoff proves the brief's requirement that an
// upstream Retry-After longer than the computed backoff wins.
func TestTickRetryAfterOverridesBackoff(t *testing.T) {
	base := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	h := newHarnessWithClock(t, clk)
	ctx := context.Background()

	h.eng.Tick(ctx) // baseline success
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after baseline = %d, want 1", got)
	}

	h.rate(40 * time.Second) // Retry-After well past Backoff(1) = 15s
	failAt := base.Add(7 * time.Second)
	clk.Set(failAt)
	h.eng.Tick(ctx) // rate limited
	if got := h.hits(); got != 2 {
		t.Fatalf("hits after the 429 = %d, want 2", got)
	}

	clk.Set(failAt.Add(20 * time.Second)) // past the 15s backoff, short of the 40s Retry-After
	h.eng.Tick(ctx)
	if got := h.hits(); got != 2 {
		t.Fatalf("hits at +20s = %d, want 2 — a Retry-After longer than the "+
			"computed backoff must still be honoured", got)
	}

	h.rate(0)
	clk.Set(failAt.Add(41 * time.Second)) // past the 40s Retry-After
	h.eng.Tick(ctx)
	if got := h.hits(); got != 3 {
		t.Fatalf("hits at +41s = %d, want 3", got)
	}
}

// TestTickBackwardsClockStepDoesNotStallPolling is the regression test for
// the case where an NTP correction steps the wall clock backwards (the
// failure mode on a board with no RTC, which is exactly what clock.System
// is exposed to: time.Time.In strips the monotonic reading, so an NTP step
// is a pure backwards jump in the values Tick compares). The remaining wait
// can never legitimately exceed the wait that was actually scheduled, so a
// gap larger than that means the clock moved, not that polling is early.
func TestTickBackwardsClockStepDoesNotStallPolling(t *testing.T) {
	base := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	h := newHarnessWithClock(t, clk)
	ctx := context.Background()

	h.eng.Tick(ctx) // baseline poll, schedules the next one a few seconds out
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after the first tick = %d, want 1", got)
	}

	clk.Set(base.Add(-1 * time.Hour)) // an NTP correction steps the clock back an hour
	h.eng.Tick(ctx)
	if got := h.hits(); got != 2 {
		t.Fatalf("hits after the backwards clock step = %d, want 2 — polling stalled silently", got)
	}
}
