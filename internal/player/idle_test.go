package player_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/player"
)

// idleWatch collects OnIdle calls. It is a channel rather than a counter
// because the report arrives on the player's own goroutine and every
// assertion below is about a moment, not a total.
type idleWatch struct {
	ch chan struct{}
}

func newIdleWatch() *idleWatch { return &idleWatch{ch: make(chan struct{}, 8)} }

func (w *idleWatch) note() {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

func (w *idleWatch) wait(t *testing.T, why string) {
	t.Helper()
	select {
	case <-w.ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("the voice never reported falling silent: %s", why)
	}
}

func (w *idleWatch) refuse(t *testing.T, why string) {
	t.Helper()
	select {
	case <-w.ch:
		t.Fatalf("the voice reported falling silent: %s", why)
	case <-time.After(150 * time.Millisecond):
	}
}

// failingBackend plays nothing and says so. The real one does this whenever
// the output cannot be opened, which on a board whose default PCM is an HDMI
// socket with nothing in it is every single request.
type failingBackend struct {
	mu      sync.Mutex
	started chan string
}

func (b *failingBackend) SetVolume(context.Context, string, int) error { return nil }

func (b *failingBackend) Play(_ context.Context, _, path string) error {
	b.mu.Lock()
	ch := b.started
	b.mu.Unlock()
	ch <- path
	return errors.New("audio open error: Unknown error 524")
}

func TestTheVoiceReportsFallingSilentWhenTheQueueRunsDry(t *testing.T) {
	f := newFake()
	close(f.hold) // every play completes immediately
	w := newIdleWatch()
	p := player.New(f, "default")
	p.OnIdle = w.note

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "noon", Path: "/s/metronome.wav", Volume: 50})
	waitStart(t, f)
	w.wait(t, "a sound played to the end and nothing was queued behind it")
}

// The whole reason OnIdle is not the opposite of OnTruncated. A siren cutting
// across a chime ends the chime's request and does not end the sound — the
// queue still holds the siren — so whatever was switched so this device could
// be heard must stay switched. Reporting silence here would put it back for
// the length of the gap, in the middle of a raid.
func TestAPreemptedSoundDoesNotReportSilenceWhileTheSirenIsStillToCome(t *testing.T) {
	f := newFake() // f.hold is open: the chime parks inside Play
	w := newIdleWatch()
	p := player.New(f, "default")
	p.OnIdle = w.note

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "noon", Path: "/s/metronome.wav", Priority: player.PriorityScheduled})
	waitStartFor(t, f, "/s/metronome.wav")

	p.Play(player.Request{EventID: "alert", Path: "/s/siren.wav", Priority: player.PriorityAlert})
	waitStartFor(t, f, "/s/siren.wav")

	w.refuse(t, "the chime was cut short but the siren had the voice")

	// And it does report once the siren itself is done, because only then
	// has the device actually stopped speaking.
	close(f.hold)
	w.wait(t, "the siren finished and nothing was queued behind it")
}

// Stop cancels the request and drops the queue, so there is nothing left to
// play and nothing left to put the far end back. Both callbacks fire for the
// one request, which is the point: it was cut short, and the device is now
// silent, and those are two different facts.
func TestStopReportsBothTruncationAndSilence(t *testing.T) {
	f := newFake() // parks inside Play
	w := newIdleWatch()
	cut := make(chan string, 4)
	p := player.New(f, "default")
	p.OnIdle = w.note
	p.OnTruncated = func(_ player.Request, reason string) { cut <- reason }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "alert", Path: "/s/siren.wav", Priority: player.PriorityAlert})
	p.Play(player.Request{EventID: "alert", Path: "/s/siren.wav", Priority: player.PriorityAlert})
	waitStart(t, f)

	p.Stop()

	select {
	case reason := <-cut:
		if reason != player.CutStopped {
			t.Fatalf("truncation reason = %q, want %q", reason, player.CutStopped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stopped sound was never reported as cut short")
	}
	w.wait(t, "Stop cancelled the sound and dropped everything behind it")
}

// The relay was told to switch; the sound never came out. Something still has
// to say the announcement is over, or a feed stays muted and a corridor stays
// quiet for the rest of the night.
func TestTheVoiceReportsFallingSilentWhenPlaybackFailsOutright(t *testing.T) {
	b := &failingBackend{started: make(chan string, 4)}
	w := newIdleWatch()
	errs := make(chan string, 4)
	p := player.New(b, "default")
	p.OnIdle = w.note
	p.OnError = func(_ player.Request, stage string, _ error) { errs <- stage }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "alert", Path: "/s/siren.wav"})
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the failing backend to be asked to play")
	}
	select {
	case stage := <-errs:
		if stage != "play" {
			t.Fatalf("failed stage = %q, want %q", stage, "play")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a failed play was never reported")
	}
	w.wait(t, "playback failed, so the device is silent and something must say so")
}

func TestPendingCountsWhatIsPlayingAndWhatIsQueuedBehindIt(t *testing.T) {
	f := newFake() // parks inside Play
	p := player.New(f, "default")

	if got := p.Pending(); got != 0 {
		t.Fatalf("Pending on a fresh player = %d, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.Play(player.Request{EventID: "alert", Path: "/s/siren.wav"})
	p.Play(player.Request{EventID: "alert", Path: "/s/siren.wav"})
	waitStart(t, f)

	if got := p.Pending(); got != 2 {
		t.Fatalf("Pending with one playing and one queued = %d, want 2", got)
	}
}
