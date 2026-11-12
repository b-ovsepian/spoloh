package player

import (
	"context"
	"time"
)

// Streamer is a Backend that can also play a stream until it is cancelled.
// It is a separate interface rather than a third method on Backend so that a
// backend without it — every test double written before the radio existed —
// still plays every announcement exactly as before, and simply has no radio.
type Streamer interface {
	// Stream plays url into device at gain percent of full scale, in
	// software, until ctx is cancelled or the stream ends.
	Stream(ctx context.Context, device, url string, gain int) error
}

// Background is what the player streams while it has nothing to say: the
// radio. The zero value is no background.
//
// It is not a Request and never enters the queue. Every request outranks it,
// a preview included — a preview already sits below everything real, and the
// radio sits below that — and it is cut the moment one arrives, not at the
// end of a song. It is not the voice either: it is never Current, never
// Pending, never cut short in OnTruncated's sense, and its starting again
// after an announcement does not stop OnIdle reporting that the announcement
// is over. Whatever an announcement's before-webhook muted has to be told it
// may unmute, and "the radio is back" is exactly when.
//
// Volume is what the card's mixer is set to and Gain is how loud the radio is
// within that, applied in software. The engine sets Volume to the master
// volume, which no announcement exceeds, and that is the point of the split:
// an announcement whose own volume stage fails plays at whatever the mixer
// was left at, and were that the radio's own level — which an operator may set
// to almost nothing — a siren would play at almost nothing.
type Background struct {
	URL    string
	Volume int
	Gain   int
}

// Retry waits for a stream that will not play. A dead URL or a dropped
// connection must not become ffmpeg started several times a second on a
// board that also has to poll for alerts, and a stream that played for a
// good while and then dropped is not a dead URL.
const (
	backgroundRetryMin = 5 * time.Second
	backgroundRetryMax = 5 * time.Minute
	backgroundHealthy  = time.Minute
)

// SetBackground sets what plays while nothing else does. Setting what is
// already set does nothing — the engine says it every second, and a stream
// must not restart mid-song for being described twice. Anything else stops
// the stream playing now, if one is, and starts the new one in its place.
//
// A change also lifts a hold Stop put on the background: it is a new
// instruction — the window closed, the station changed — not the old one
// repeated.
func (p *Player) SetBackground(b Background) {
	p.mu.Lock()
	if b == p.bg {
		p.mu.Unlock()
		return
	}
	p.bg = b
	p.bgWait = 0
	p.bgHeld = false
	p.bgErr = nil
	if p.bgCancel != nil {
		p.bgCancel()
	}
	p.mu.Unlock()
	p.poke()
}

// ResumeBackground lifts the hold Stop put on the background.
func (p *Player) ResumeBackground() {
	p.mu.Lock()
	p.bgHeld = false
	p.mu.Unlock()
	p.poke()
}

// BackgroundHeld reports whether Stop is holding the background off.
func (p *Player) BackgroundHeld() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bgHeld
}

// BackgroundError reports how the most recent attempt to stream the current
// background failed, and when, or nil if none has since it was set. A stream
// that is playing right now may have failed before, so this is "the last
// failure", never "it is failing".
func (p *Player) BackgroundError() (error, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bgErr, p.bgErrAt
}

// playBackground streams the background until something cancels it or the
// stream ends, and reports whether it tried: false means there is no
// background to play, or a request arrived first, and Run should go back to
// waiting or to the queue.
func (p *Player) playBackground(ctx context.Context) bool {
	s, ok := p.backend.(Streamer)
	if !ok {
		return false
	}
	p.mu.Lock()
	bg, wait := p.bg, p.bgWait
	// The queue is checked in the same hold that installs bgCancel, so a
	// request cannot slip in between: either it is already queued and the
	// radio does not start, or Play finds bgCancel and cuts it.
	if bg.URL == "" || p.bgHeld || len(p.queue) > 0 {
		p.mu.Unlock()
		return false
	}
	bgCtx, cancel := context.WithCancel(ctx)
	p.bgCancel = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.bgCancel = nil
		p.mu.Unlock()
		cancel()
	}()

	if wait > 0 {
		select {
		case <-bgCtx.Done():
			p.settleBackground(bg, 0, nil, time.Time{}, false)
			return true
		case <-p.after(wait):
		}
	}

	device := p.deviceName()
	// A card with no mixer control plays on, as an announcement does.
	_ = p.backend.SetVolume(bgCtx, device, bg.Volume)
	if bgCtx.Err() != nil {
		p.settleBackground(bg, 0, nil, time.Time{}, false)
		return true
	}
	began := p.now()
	p.setStreaming(device)
	err := s.Stream(bgCtx, device, bg.URL, bg.Gain)
	p.setStreaming("")
	ended := p.now()
	if bgCtx.Err() != nil {
		// It gave way — to a sound, to a different background, to Stop, or
		// to the daemon stopping. Not a fault, and after a sound it comes
		// straight back. One that had been playing a while is evidence the
		// stream works, and clears an older failure.
		p.settleBackground(bg, 0, nil, ended, ended.Sub(began) >= backgroundHealthy)
		return true
	}
	p.settleBackground(bg, nextRetry(wait, ended.Sub(began)), err, ended, false)
	if err != nil && p.OnBackgroundError != nil {
		p.OnBackgroundError(bg, err)
	}
	return true
}

// settleBackground records how long to wait before bg is streamed again and
// how its last attempt failed, unless the background has been changed
// meanwhile: SetBackground has already reset both for the new one.
func (p *Player) settleBackground(bg Background, wait time.Duration, err error, at time.Time, worked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bg != bg {
		return
	}
	p.bgWait = wait
	switch {
	case err != nil:
		p.bgErr, p.bgErrAt = err, at
	case worked:
		p.bgErr = nil
	}
}

func nextRetry(prev, ran time.Duration) time.Duration {
	if ran >= backgroundHealthy || prev <= 0 {
		return backgroundRetryMin
	}
	return min(prev*2, backgroundRetryMax)
}

func (p *Player) after(d time.Duration) <-chan time.Time {
	if p.After != nil {
		return p.After(d)
	}
	return time.After(d)
}

func (p *Player) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// poke wakes Run if it is waiting.
func (p *Player) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Background reports what SetBackground last asked for — what the player
// streams when it has nothing to say, not whether a stream is playing at this
// moment.
func (p *Player) Background() Background {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bg
}

// StreamingCard reports whether a background stream is holding the card the
// PCM name opens, right now. While it is, opening that card from anywhere
// else fails as busy, which says nothing about whether the card works.
func (p *Player) StreamingCard(name string) bool {
	p.mu.Lock()
	dev := p.bgDevice
	p.mu.Unlock()
	if dev == "" {
		return false
	}
	if name == dev {
		return true
	}
	card := ctlDevice(name)
	return card != "" && card == ctlDevice(dev)
}

// setStreaming records which device a stream holds, "" for none; Stream
// returning means ffmpeg has exited and the card is free again.
func (p *Player) setStreaming(device string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bgDevice = device
}
