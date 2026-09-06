package player_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/player"
)

type fakeBackend struct {
	mu      sync.Mutex
	calls   []string
	started chan string
	hold    chan struct{}
}

func newFake() *fakeBackend {
	return &fakeBackend{started: make(chan string, 8), hold: make(chan struct{})}
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

func (f *fakeBackend) SetVolume(_ context.Context, _ string, pct int) error {
	f.record(fmt.Sprintf("volume=%d", pct))
	return nil
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
