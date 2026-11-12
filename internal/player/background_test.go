package player_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/player"
)

// streamBackend is a Backend that can also stream, which is what makes it a
// player.Streamer. Every call is reported on events, in order, so a test can
// wait for the moment it is about rather than sleep.
type streamBackend struct {
	events chan string

	mu        sync.Mutex
	streamErr error         // non-nil: Stream fails at once with it
	volErr    error         // non-nil: SetVolume fails with it, as amixer does on HDMI
	playHold  chan struct{} // Play returns when this closes, or on cancel
	onStream  func()        // runs inside Stream before it blocks or fails
}

func newStreamBackend() *streamBackend {
	return &streamBackend{events: make(chan string, 1024), playHold: make(chan struct{})}
}

func (b *streamBackend) SetVolume(_ context.Context, _ string, pct int) error {
	b.events <- "volume=" + itoa(pct)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.volErr
}

func (b *streamBackend) Play(ctx context.Context, _, path string) error {
	b.events <- "play=" + path
	b.mu.Lock()
	hold := b.playHold
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-hold:
		return nil
	}
}

func (b *streamBackend) Stream(ctx context.Context, _, url string, gain int) error {
	b.events <- "stream=" + url
	b.events <- "gain=" + itoa(gain)
	b.mu.Lock()
	err, hook := b.streamErr, b.onStream
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}
	<-ctx.Done()
	b.events <- "streamcut=" + url
	return ctx.Err()
}

func (b *streamBackend) finishPlays() {
	b.mu.Lock()
	close(b.playHold)
	b.mu.Unlock()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

// expect waits for want to arrive on events, failing if anything listed in
// refuse arrives first.
func expect(t *testing.T, b *streamBackend, want string, refuse ...string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-b.events:
			if got == want {
				return
			}
			if slices.Contains(refuse, got) {
				t.Fatalf("%q arrived before %q", got, want)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// quiet fails if anything matching one of these arrives within a short wait.
func quiet(t *testing.T, b *streamBackend, refuse ...string) {
	t.Helper()
	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case got := <-b.events:
			if slices.Contains(refuse, got) {
				t.Fatalf("%q arrived, want nothing of the kind", got)
			}
		case <-deadline:
			return
		}
	}
}

const station = "https://online.hitfm.ua/HitFM"

func runPlayer(t *testing.T, p *player.Player) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
}

// The radio plays when nothing else is. The card is set to Volume — the
// master volume, which no announcement exceeds — and the radio is made quieter
// in software by Gain. Were the card set to the radio's own level instead, an
// announcement whose own volume stage failed would play at the radio's level,
// which may be nothing at all: a siren at 0% is a silent device in a raid.
func TestTheBackgroundPlaysWhenNothingElseIs(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "volume=80")
	expect(t, b, "stream="+station)
	expect(t, b, "gain=50")
}

// A card with no mixer control — HDMI on a Pi has none — still plays the
// radio, as it still plays an announcement.
func TestTheBackgroundPlaysOnACardWithNoMixer(t *testing.T) {
	b := newStreamBackend()
	b.volErr = errors.New("amixer: Unable to find simple control 'Master',0")
	p := player.New(b, "default")
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)
	quiet(t, b, "volume=80") // not retried in a loop
}

// The window between the queue being found empty and the radio starting: a
// siren arriving there must not find the radio starting on top of it with
// nothing left to cut it. OnIdle runs in exactly that window, so a request
// made from it is a request made there.
func TestASoundArrivingAsTheRadioIsAboutToStartIsPlayed(t *testing.T) {
	b := newStreamBackend()
	close(b.playHold)
	p := player.New(b, "default")
	var once sync.Once
	p.OnIdle = func() {
		once.Do(func() {
			p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
		})
	}
	runPlayer(t, p)

	// The radio first, so that when the chime ends the radio is what would
	// start next — and the siren, asked for from OnIdle, has to win.
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)
	p.Play(player.Request{EventID: "chime", Path: "/s/chime.wav", Volume: 80, Priority: player.PriorityScheduled})
	expect(t, b, "play=/s/chime.wav")
	expect(t, b, "play=/s/siren.wav")
}

// "Зупинити звук" means the speaker goes quiet. An operator who stops the
// siren to take the PA must not have music start under them a moment later,
// so Stop holds the radio off — until they resume it, or until what the
// radio should be playing changes: the window closes, the station changes.
func TestStopHoldsTheBackgroundOffUntilResumed(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	runPlayer(t, p)
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)

	p.Stop()
	expect(t, b, "streamcut="+station)
	quiet(t, b, "stream="+station)
	if !p.BackgroundHeld() {
		t.Fatal("BackgroundHeld = false after Stop")
	}
	// The engine says the same thing every second; that is not a resume.
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	quiet(t, b, "stream="+station)

	p.ResumeBackground()
	expect(t, b, "stream="+station)
}

func TestStopDuringASoundKeepsTheBackgroundOff(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	runPlayer(t, p)
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)

	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	expect(t, b, "play=/s/siren.wav")
	p.Stop()
	quiet(t, b, "stream="+station)
}

// A different background is a new instruction, and lifts a Stop's hold: the
// window closing and opening again tomorrow brings the radio back.
func TestAChangedBackgroundLiftsTheHold(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	runPlayer(t, p)
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)
	p.Stop()
	expect(t, b, "streamcut="+station)

	p.SetBackground(player.Background{})
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)
}

// The last failure is kept for the radio page to show: the journal says so
// once in half an hour, and "грає" on a page over a stream that fails every
// five minutes is a page that lies.
func TestTheLastStreamFailureIsReported(t *testing.T) {
	b := newStreamBackend()
	b.streamErr = errors.New("Server returned 404 Not Found")
	p := player.New(b, "default")
	p.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)
	deadline := time.After(2 * time.Second)
	for {
		if err, _ := p.BackgroundError(); err != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("BackgroundError stayed nil after a failed stream")
		case <-time.After(5 * time.Millisecond):
		}
	}
	p.SetBackground(player.Background{})
	if err, _ := p.BackgroundError(); err != nil {
		t.Fatalf("BackgroundError = %v after the background was cleared", err)
	}
}

// Every sound outranks the radio — a preview included, which sits below
// everything real. The radio gives way at once and comes back after.
func TestEverySoundCutsTheBackgroundAndItResumesAfter(t *testing.T) {
	for _, pri := range []player.Priority{player.PriorityPreview, player.PriorityScheduled, player.PriorityAlert} {
		b := newStreamBackend()
		p := player.New(b, "default")
		runPlayer(t, p)
		p.SetBackground(player.Background{URL: station, Volume: 40})
		expect(t, b, "stream="+station)

		p.Play(player.Request{EventID: "e", Path: "/s/siren.wav", Volume: 100, Priority: pri})
		expect(t, b, "streamcut="+station)
		expect(t, b, "play=/s/siren.wav", "stream="+station)

		b.finishPlays()
		expect(t, b, "stream="+station)
	}
}

// The radio is not the voice. A device playing music has nothing to say:
// nothing pending, nothing current, and when an announcement ends the voice
// has fallen silent even though the radio comes straight back — because the
// after-webhook that unmutes whatever the announcement muted is waiting on
// exactly that.
func TestTheBackgroundIsNotTheVoice(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	idle := newIdleWatch()
	p.OnIdle = idle.note
	var truncated []string
	var mu sync.Mutex
	p.OnTruncated = func(r player.Request, reason string) {
		mu.Lock()
		truncated = append(truncated, r.EventID)
		mu.Unlock()
	}
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 40})
	expect(t, b, "stream="+station)
	if n := p.Pending(); n != 0 {
		t.Fatalf("Pending = %d while only the radio plays, want 0", n)
	}
	if _, ok := p.Current(); ok {
		t.Fatal("Current reports a request while only the radio plays")
	}
	idle.refuse(t, "the radio starting is not the voice falling silent")

	p.Play(player.Request{EventID: "chime", Path: "/s/chime.wav", Volume: 80, Priority: player.PriorityScheduled})
	expect(t, b, "play=/s/chime.wav")
	b.finishPlays()
	idle.wait(t, "the chime ended, and the radio returning does not change that")
	expect(t, b, "stream="+station)

	mu.Lock()
	defer mu.Unlock()
	if len(truncated) != 0 {
		t.Fatalf("OnTruncated reported %v: the radio giving way is not a sound cut short", truncated)
	}
}

func TestClearingTheBackgroundStopsIt(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 40})
	expect(t, b, "stream="+station)
	p.SetBackground(player.Background{})
	expect(t, b, "streamcut="+station)
	quiet(t, b, "stream="+station, "stream=", "volume=40")
}

// The engine sets the background every second. Saying the same thing again
// must not restart a stream mid-song; saying something new must.
func TestOnlyAChangedBackgroundRestarts(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 40})
	expect(t, b, "stream="+station)
	p.SetBackground(player.Background{URL: station, Volume: 40})
	quiet(t, b, "streamcut="+station)

	const other = "https://radio.ukr.radio/ur2-mp3"
	p.SetBackground(player.Background{URL: other, Volume: 40})
	expect(t, b, "streamcut="+station)
	expect(t, b, "stream="+other)
}

// A stream that will not play — the station moved, the internet is down —
// is retried, each time further apart, up to a ceiling. Without the backoff
// a dead URL is ffmpeg started several times a second, forever, on a board
// that also has to poll for alerts.
func TestAFailingStreamBacksOff(t *testing.T) {
	b := newStreamBackend()
	b.streamErr = errors.New("Server returned 404 Not Found")
	p := player.New(b, "default")

	var mu sync.Mutex
	var waits []time.Duration
	p.After = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	errs := make(chan error, 64)
	p.OnBackgroundError = func(_ player.Background, err error) {
		select {
		case errs <- err:
		default:
		}
	}
	runPlayer(t, p)
	p.SetBackground(player.Background{URL: station, Volume: 40})

	select {
	case err := <-errs:
		if err.Error() != "Server returned 404 Not Found" {
			t.Fatalf("OnBackgroundError got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a failing stream was never reported")
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := append([]time.Duration(nil), waits...)
		mu.Unlock()
		if len(got) >= 9 {
			want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second}
			if !slices.Equal(got[:8], want) {
				t.Fatalf("retry waits = %v, want %v then 5 minutes thereafter", got[:8], want)
			}
			p.SetBackground(player.Background{})
			return
		}
		select {
		case <-deadline:
			t.Fatalf("only %d retries happened: %v", len(got), got)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A stream that played for a good while and then dropped is not a dead URL.
// It is retried at the shortest wait, not at whatever an outage hours ago
// had grown the wait to.
func TestAStreamThatPlayedAWhileRetriesAtTheShortestWait(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "default")

	var mu sync.Mutex
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var waits []time.Duration
	release := make(chan time.Time)
	p.After = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return release
	}
	// Two quick failures grow the wait; then one that played for an hour.
	fails := 0
	b.streamErr = errors.New("connection reset")
	b.onStream = func() {
		mu.Lock()
		defer mu.Unlock()
		fails++
		if fails == 3 {
			now = now.Add(time.Hour)
		}
	}
	runPlayer(t, p)
	p.SetBackground(player.Background{URL: station, Volume: 40})

	for i := 0; i < 3; i++ {
		expect(t, b, "stream="+station)
		if i < 2 {
			release <- time.Time{}
		}
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := append([]time.Duration(nil), waits...)
		mu.Unlock()
		if len(got) == 3 {
			if want := []time.Duration{5 * time.Second, 10 * time.Second, 5 * time.Second}; !slices.Equal(got, want) {
				t.Fatalf("waits = %v, want %v", got, want)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("waits = %v, want three", got)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A sound arriving while the radio waits out a retry is played at once, and
// the retry wait does not hold it up.
func TestASoundIsNotHeldUpByABackgroundRetry(t *testing.T) {
	b := newStreamBackend()
	b.streamErr = errors.New("connection refused")
	p := player.New(b, "default")
	p.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) } // never
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 40})
	expect(t, b, "stream="+station)
	p.Play(player.Request{EventID: "alert-start", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	expect(t, b, "play=/s/siren.wav")
}

// noStream is a Backend that cannot stream. A player built on one plays every
// sound as before and simply has no radio.
type noStream struct{ *fakeBackend }

func TestABackendThatCannotStreamPlaysNoBackground(t *testing.T) {
	f := newFake()
	close(f.hold)
	p := player.New(noStream{f}, "default")
	runPlayer(t, p)

	p.SetBackground(player.Background{URL: station, Volume: 40})
	p.Play(player.Request{EventID: "e", Path: "/s/siren.wav", Volume: 100, Priority: player.PriorityAlert})
	if got := waitStart(t, f); got != "/s/siren.wav" {
		t.Fatalf("played %q", got)
	}
}

// /audio probes an output by opening it. A card the radio is streaming to is
// held by ffmpeg, and opening it fails with "busy" — which the page would
// show as a broken output, about the one that works. The page asks this
// first, and leaves that card alone.
func TestStreamingCardNamesTheCardTheRadioHolds(t *testing.T) {
	b := newStreamBackend()
	p := player.New(b, "plughw:CARD=Headphones,DEV=0")
	runPlayer(t, p)
	if p.StreamingCard("plughw:CARD=Headphones,DEV=0") {
		t.Fatal("StreamingCard is true before anything streams")
	}
	p.SetBackground(player.Background{URL: station, Volume: 80, Gain: 50})
	expect(t, b, "stream="+station)

	for _, name := range []string{"plughw:CARD=Headphones,DEV=0", "hw:CARD=Headphones,DEV=0", "sysdefault:CARD=Headphones"} {
		if !p.StreamingCard(name) {
			t.Errorf("StreamingCard(%q) = false while the radio streams to that card", name)
		}
	}
	if p.StreamingCard("plughw:CARD=vc4hdmi,DEV=0") {
		t.Error("StreamingCard is true for a card the radio is not on")
	}
	p.SetBackground(player.Background{})
	expect(t, b, "streamcut="+station)
	deadline := time.After(2 * time.Second)
	for p.StreamingCard("plughw:CARD=Headphones,DEV=0") {
		select {
		case <-deadline:
			t.Fatal("StreamingCard stayed true after the radio stopped")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
