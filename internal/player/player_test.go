package player_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/player"
)

type fakeBackend struct {
	mu      sync.Mutex
	calls   []string
	started chan string
	hold    chan struct{}

	volStarted chan struct{}
	volHold    chan struct{}
}

func newFake() *fakeBackend {
	closed := make(chan struct{})
	close(closed)
	return &fakeBackend{
		started:    make(chan string, 8),
		hold:       make(chan struct{}),
		volStarted: make(chan struct{}, 8),
		volHold:    closed, // SetVolume does not block by default
	}
}

func (f *fakeBackend) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeBackend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// blockVolume makes the next SetVolume call park until its context is
// cancelled. It must be set up before the Play request that should be held is
// submitted. Only the one call that consumes
// the block is affected; SetVolume resets to non-blocking for calls after it.
func (f *fakeBackend) blockVolume() {
	f.mu.Lock()
	f.volHold = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeBackend) SetVolume(ctx context.Context, _ string, pct int) error {
	f.record(fmt.Sprintf("volume=%d", pct))

	f.mu.Lock()
	hold := f.volHold
	closed := make(chan struct{})
	close(closed)
	f.volHold = closed // only the call that grabbed `hold` blocks
	f.mu.Unlock()

	select {
	case f.volStarted <- struct{}{}:
	default:
	}

	select {
	case <-ctx.Done():
		f.record(fmt.Sprintf("volcancelled=%d", pct))
		return ctx.Err()
	case <-hold:
		return nil
	}
}

func (f *fakeBackend) Play(ctx context.Context, _, path string) error {
	f.record("play=" + path)
	f.started <- path
	select {
	case <-ctx.Done():
		f.record("cancelled=" + path)
		return ctx.Err()
	case <-f.hold:
		f.record("finished=" + path)
		return nil
	}
}

func waitStart(t *testing.T, f *fakeBackend) string {
	t.Helper()
	select {
	case p := <-f.started:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for playback to start")
		return ""
	}
}

// waitStartFor drains f.started until it sees want, ignoring any other path
// (a request whose volume stage was cancelled still makes a fleeting, already
// cancelled entry into Play).
func waitStartFor(t *testing.T, f *fakeBackend, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case p := <-f.started:
			if p == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q to start", want)
		}
	}
}

func waitVolStart(t *testing.T, f *fakeBackend) {
	t.Helper()
	select {
	case <-f.volStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SetVolume to start")
	}
}

func TestVolumeIsSetBeforePlay(t *testing.T) {
	f := newFake()
	close(f.hold) // every play completes immediately
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85})
	waitStart(t, f)
	time.Sleep(50 * time.Millisecond)

	calls := f.Calls()
	if len(calls) < 2 || calls[0] != "volume=85" || calls[1] != "play=/s/metronome.wav" {
		t.Fatalf("calls = %v, want volume then play", calls)
	}
}

// An alert must interrupt whatever is playing. A scheduled chime that blocks
// the siren would defeat the purpose of the device.
func TestAlertPreemptsScheduled(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85, Priority: player.PriorityScheduled})
	if got := waitStart(t, f); got != "/s/metronome.wav" {
		t.Fatalf("first play = %q", got)
	}

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	if got := waitStart(t, f); got != "/s/siren.wav" {
		t.Fatalf("second play = %q, want the siren to have preempted", got)
	}

	var sawCancel bool
	for _, c := range f.Calls() {
		if c == "cancelled=/s/metronome.wav" {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatalf("the scheduled sound was not cancelled: %v", f.Calls())
	}
}

// A scheduled event arriving during an alert waits its turn rather than
// cutting the siren short.
func TestScheduledDoesNotPreemptAlert(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStart(t, f)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85, Priority: player.PriorityScheduled})
	time.Sleep(100 * time.Millisecond)

	for _, c := range f.Calls() {
		if c == "cancelled=/s/siren.wav" {
			t.Fatalf("the siren must not be cancelled by a scheduled event: %v", f.Calls())
		}
	}
	close(f.hold)
	if got := waitStart(t, f); got != "/s/metronome.wav" {
		t.Fatalf("queued play = %q, want the metronome after the siren", got)
	}
}

func TestStopCancelsCurrentAndClearsQueue(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStart(t, f)
	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85})

	p.Stop()
	time.Sleep(100 * time.Millisecond)

	for _, c := range f.Calls() {
		if c == "play=/s/metronome.wav" {
			t.Fatalf("Stop must drop the queue: %v", f.Calls())
		}
	}
	if _, ok := p.Current(); ok {
		t.Fatal("nothing should be playing after Stop")
	}
}

func TestCurrentReportsPlayingRequest(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if _, ok := p.Current(); ok {
		t.Fatal("nothing should be playing before the first request")
	}
	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStart(t, f)

	got, ok := p.Current()
	if !ok || got.EventID != "alert-start" {
		t.Fatalf("Current() = %+v, %v", got, ok)
	}
}

// Regression test for a logic race between popping a request off the queue
// and installing it as current: if that step is not atomic, a concurrent
// Play with a higher priority can find nothing installed to cancel and the
// popped request plays to completion regardless.
func TestAlertPreemptsWhileSettingVolume(t *testing.T) {
	f := newFake()
	f.blockVolume()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85, Priority: player.PriorityScheduled})
	waitVolStart(t, f) // scheduled request is now current, parked in SetVolume

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})

	waitStartFor(t, f, "/s/siren.wav")

	var sawCancel bool
	for _, c := range f.Calls() {
		if c == "volcancelled=85" {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatalf("the scheduled request's volume stage was not cancelled: %v", f.Calls())
	}
}

// Same root cause, from Stop's side: a request parked in its volume stage
// must still be cancelled by Stop rather than being allowed to reach Play.
func TestStopCancelsWhileSettingVolume(t *testing.T) {
	f := newFake()
	f.blockVolume()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85})
	waitVolStart(t, f) // request is now current, parked in SetVolume

	p.Stop()

	select {
	case path := <-f.started:
		t.Fatalf("Stop must prevent playback, but got play=%q", path)
	case <-time.After(200 * time.Millisecond):
	}
}

// A stuck aplay — a disconnected USB card, a PCM another process holds open —
// used to silence the device permanently: preemption only cancels the current
// request for a strictly HIGHER priority, so a wedged alert_start blocked the
// alert_end behind it and every alert after that, while systemd, pinged from
// the engine's tick rather than from here, still saw a healthy daemon.
func TestStuckPlaybackTimesOutAndTheQueueMovesOn(t *testing.T) {
	f := newFake() // f.hold is never closed: every Play blocks until cancelled
	p := player.New(f, "default")
	p.MaxPlayback = 100 * time.Millisecond

	var (
		mu     sync.Mutex
		stages []string
	)
	p.OnError = func(_ player.Request, stage string, _ error) {
		mu.Lock()
		stages = append(stages, stage)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	// Same priority as the first, so preemption cannot be what rescues this.
	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStart(t, f)
	p.Play(player.Request{EventID: "alert-end", Path: "/s/all-clear.wav", Volume: 90, Priority: player.PriorityAlert})

	waitStartFor(t, f, "/s/all-clear.wav")

	// And the timeout must be reported under a stage of its own, so that a
	// wedged card is distinguishable in the log from one that errored.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), stages...)
		mu.Unlock()
		for _, s := range got {
			if s == "play_timeout" {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("no play_timeout reported, got stages %v", got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The timeout must not turn an ordinary preemption into a timeout report: a
// cancelled request is not a wedged one.
func TestPreemptionIsNotReportedAsATimeout(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")
	p.MaxPlayback = 5 * time.Second

	var (
		mu     sync.Mutex
		stages []string
	)
	p.OnError = func(_ player.Request, stage string, _ error) {
		mu.Lock()
		stages = append(stages, stage)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85, Priority: player.PriorityScheduled})
	waitStart(t, f)
	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStartFor(t, f, "/s/siren.wav")

	mu.Lock()
	defer mu.Unlock()
	if len(stages) != 0 {
		t.Fatalf("a preemption reported %v, want nothing", stages)
	}
}

// The defect this pins: a preview started from a settings page used to share
// PriorityAlert with a real announcement, and Play preempts only on strictly
// higher priority — so an air raid alert arriving mid-preview queued behind
// it for up to MaxPlayback, five minutes.
func TestAnAlertPreemptsAPreview(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "manual", Path: "/s/metronome.wav", Volume: 80, Priority: player.PriorityPreview})
	if got := waitStart(t, f); got != "/s/metronome.wav" {
		t.Fatalf("first play = %q, want the preview", got)
	}

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	if got := waitStart(t, f); got != "/s/siren.wav" {
		t.Fatalf("second play = %q, want the alert to have preempted the preview", got)
	}

	var sawCancel bool
	for _, c := range f.Calls() {
		if c == "cancelled=/s/metronome.wav" {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatalf("the preview was not cancelled by the alert: %v", f.Calls())
	}
}

// A scheduled chime must also outrank a preview: anything the device decided
// to do on its own beats someone finding out what a file sounds like.
func TestAScheduledEventPreemptsAPreview(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "manual", Path: "/s/metronome.wav", Volume: 80, Priority: player.PriorityPreview})
	if got := waitStart(t, f); got != "/s/metronome.wav" {
		t.Fatalf("first play = %q, want the preview", got)
	}

	p.Play(player.Request{EventID: "silence", Path: "/s/chime.wav", Volume: 85, Priority: player.PriorityScheduled})
	if got := waitStart(t, f); got != "/s/chime.wav" {
		t.Fatalf("second play = %q, want the scheduled event to have preempted the preview", got)
	}
}

// cutRecorder collects OnTruncated reports for the tests below.
type cutRecorder struct {
	mu   sync.Mutex
	cuts []string // "eventID:reason"
}

func (c *cutRecorder) hook(r player.Request, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cuts = append(c.cuts, r.EventID+":"+reason)
}

// waitCut waits for want to be reported, or fails saying what was reported
// instead. A callback fired from the player's goroutine is what is being
// waited on, so this polls rather than sleeping for a guessed interval.
func (c *cutRecorder) waitCut(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		c.mu.Lock()
		got := append([]string(nil), c.cuts...)
		c.mu.Unlock()
		for _, g := range got {
			if g == want {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("%q was never reported as cut short; got %v", want, got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *cutRecorder) Cuts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.cuts...)
}

// A preemption is not a fault and deliberately produces no OnError, which
// used to mean it produced nothing at all — so the event log kept the row
// engine.play wrote when the sound was queued and went on saying the minute
// of silence played. This is the signal that lets it say otherwise.
func TestPreemptionIsReportedAsCutShort(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	var cuts cutRecorder
	p.OnTruncated = cuts.hook
	var errs []string
	p.OnError = func(_ player.Request, stage string, _ error) { errs = append(errs, stage) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85, Priority: player.PriorityScheduled})
	waitStart(t, f)
	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStartFor(t, f, "/s/siren.wav")

	cuts.waitCut(t, "silence:"+player.CutPreempted)
	if len(errs) != 0 {
		t.Fatalf("a preemption reported errors %v; it is the device doing the right thing, not a fault", errs)
	}
}

// Stop is the operator cutting a sound off, which reads differently in the
// log from a siren doing it, so the two must not arrive under one name.
func TestStopIsReportedAsCutShortByTheOperator(t *testing.T) {
	f := newFake()
	p := player.New(f, "default")

	var cuts cutRecorder
	p.OnTruncated = cuts.hook

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "manual", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStart(t, f)
	p.Stop()

	cuts.waitCut(t, "manual:"+player.CutStopped)
}

// The other half: a sound that ran to the end must not be reported as cut
// short, or the log would call every announcement truncated and the
// distinction would be worth nothing.
func TestACompletedRequestIsNotReportedAsCutShort(t *testing.T) {
	f := newFake()
	close(f.hold) // every Play returns immediately, as a finished sound does
	p := player.New(f, "default")

	var cuts cutRecorder
	p.OnTruncated = cuts.hook

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "silence", Path: "/s/metronome.wav", Volume: 85, Priority: player.PriorityScheduled})
	waitStart(t, f)

	// Wait for the request to be let go of before looking: Current() is
	// cleared in the same cleanup that reports a cut, so an empty list read
	// too early would pass against anything.
	deadline := time.After(2 * time.Second)
	for {
		if _, playing := p.Current(); !playing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the request never finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := cuts.Cuts(); len(got) != 0 {
		t.Fatalf("a sound that played to the end was reported as %v, want nothing", got)
	}
}

// A wedged card that hits MaxPlayback is already reported as a *_timeout
// error. It must not also arrive as a truncation: "something more important
// interrupted this" and "nothing could get this to finish" send an operator
// to two different places.
func TestATimeoutIsNotReportedAsCutShort(t *testing.T) {
	f := newFake() // f.hold is never closed: Play blocks until cancelled
	p := player.New(f, "default")
	p.MaxPlayback = 50 * time.Millisecond

	var cuts cutRecorder
	p.OnTruncated = cuts.hook
	stages := make(chan string, 4)
	p.OnError = func(_ player.Request, stage string, _ error) { stages <- stage }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	waitStart(t, f)

	select {
	case got := <-stages:
		if got != "play_timeout" {
			t.Fatalf("stage = %q, want play_timeout", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wedged request was never abandoned")
	}
	if got := cuts.Cuts(); len(got) != 0 {
		t.Fatalf("a timeout was reported as cut short (%v); it is a fault, and already has its own error", got)
	}
}
