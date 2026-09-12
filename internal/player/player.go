// Package player owns audio output. There is exactly one voice: a device with
// two sounds overlapping is worse than useless during an emergency.
package player

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// defaultMaxPlayback caps how long one request may hold the single voice.
// aplay can block indefinitely on a disconnected USB card or a PCM another
// process has open, and preemption is no rescue: Play only cancels the current
// request for a strictly higher priority, so a wedged alert_start would block
// the alert_end behind it and every alert after that. The device would then be
// silent forever while systemd, which is pinged from the engine's tick and not
// from here, still saw a healthy daemon. Library sounds are at most a minute
// long, so five minutes is generous by any measure.
const defaultMaxPlayback = 5 * time.Minute

type Priority int

const (
	// PriorityPreview is a sound played to find out what it sounds like or
	// whether the speaker works — the /sounds preview button, the /audio
	// test, `spoloh play`. It sits below everything real on purpose.
	//
	// It used to share PriorityAlert, because it shares a code path with
	// manual firing. Play preempts only on strictly higher priority, so a
	// preview started from a settings page could hold the single voice for
	// up to MaxPlayback — five minutes — with a real air raid alert waiting
	// behind it in the queue. Nobody who clicks "listen" is asking for
	// that.
	PriorityPreview Priority = iota
	PriorityScheduled
	// PriorityAlert is both an air raid alert and the dashboard's manual
	// fire, which is deliberately the same thing: manual playback exists
	// as the path that still works when upstream is down and the threat is
	// real, so it must not be preempted by a scheduled chime.
	PriorityAlert
)

// Request is one thing to play.
type Request struct {
	EventID  string
	Path     string
	Volume   int // 0..100, already combined with the master volume
	Priority Priority
}

// Backend performs the actual output. ALSA is the production implementation;
// tests use a recorder.
type Backend interface {
	SetVolume(ctx context.Context, device string, pct int) error
	Play(ctx context.Context, device, path string) error
}

// Reasons handed to OnTruncated. They name what cut the request short, not a
// fault, and they are English like every other identifier that crosses this
// boundary — the operator-facing wording is the event log's to write.
const (
	CutPreempted = "preempted"
	CutStopped   = "stopped"
)

type current struct {
	req    Request
	cancel context.CancelFunc

	// stopped distinguishes the two ways cancel gets called: Stop sets it,
	// a preemption does not. Both cut the sound off, and the log has to be
	// able to say which, because "a siren interrupted the minute of
	// silence" and "somebody pressed stop" are different answers to the
	// same question.
	stopped bool
}

type Player struct {
	backend Backend
	device  string

	// OnError reports a failed stage: "volume" or "play", or "volume_timeout"
	// or "play_timeout" when the stage was abandoned for exceeding
	// MaxPlayback. Set it before Run.
	OnError func(r Request, stage string, err error)

	// OnTruncated reports a request that had started and did not finish:
	// something of higher priority preempted it (CutPreempted), or Stop
	// cancelled it (CutStopped). Set it before Run.
	//
	// Deliberately not OnError. A preemption is the device doing the right
	// thing — reportStageError stays silent about it on purpose — and a row
	// that reads as a fault sends an operator to check the speaker wiring
	// over a chime correctly dropped for a siren. But silence is not right
	// either: the engine writes an announcement's log row when the sound is
	// queued, so without this the log says the minute of silence played when
	// nobody heard it.
	//
	// Only a request that reached the output is reported. Requests still
	// queued when Stop drops them never started, and never claimed to.
	OnTruncated func(r Request, reason string)

	// OnIdle reports that the voice has fallen silent: the request being
	// played has ended and there is nothing queued behind it. Set it before
	// Run.
	//
	// It is about the voice, not about a request, and that difference is
	// the whole reason it exists next to OnTruncated rather than inside it.
	// A siren preempting a chime ends the chime's request and does not end
	// the sound — the queue still holds the siren, the device never stops
	// speaking — so this does not fire, and OnTruncated does. A request that
	// Stop cancelled with nothing behind it fires both: it was cut short,
	// and the device is now silent. The two answer different questions and
	// neither is the negation of the other.
	//
	// Every other ending reaches it: a sound played to the end, a sound
	// abandoned after MaxPlayback, a stage that failed outright, a Stop that
	// also dropped the queue. The rule is the voice and not the outcome,
	// because of what the caller does with it — whatever was switched so
	// this device could be heard is only safe to switch back once it has
	// stopped being heard, and after every one of those it has.
	OnIdle func()

	// MaxPlayback caps one request. Zero means defaultMaxPlayback. Set it
	// before Run; tests inject a short limit.
	MaxPlayback time.Duration

	mu    sync.Mutex
	queue []Request
	cur   *current
	wake  chan struct{}

	// spoke records that something has actually reached the output since the
	// queue was last found empty, so OnIdle reports the fall into silence
	// rather than every trip around an idle loop.
	spoke bool
}

func New(b Backend, device string) *Player {
	return &Player{backend: b, device: device, wake: make(chan struct{}, 1)}
}

// SetDevice changes the output every later request plays through.
//
// The device was a construction-time value until an operator changed the
// output on /audio and found the next sound still going to the old one:
// the config was written, but this Player kept the device it was built
// with until the daemon restarted. On a board whose default output is HDMI
// with nothing plugged into it, that is the difference between a device
// that announces and one that stays silent, and "reboot it" is not an
// instruction this product gets to give.
//
// A request already playing is left alone. Changing the output underneath
// a siren that is mid-announcement would cut it off, and the operator
// pressing save on a settings page has not asked for that.
func (p *Player) SetDevice(device string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.device = device
}

// deviceName reads the output under the lock. playOne runs outside it, so
// it cannot touch p.device directly without racing SetDevice.
func (p *Player) deviceName() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.device
}

// Play enqueues a request. A request of higher priority than what is playing
// cancels it and jumps ahead of anything of lower priority already waiting.
func (p *Player) Play(r Request) {
	p.mu.Lock()
	pos := len(p.queue)
	for i, q := range p.queue {
		if r.Priority > q.Priority {
			pos = i
			break
		}
	}
	p.queue = append(p.queue, Request{})
	copy(p.queue[pos+1:], p.queue[pos:])
	p.queue[pos] = r

	if p.cur != nil && r.Priority > p.cur.req.Priority {
		p.cur.cancel()
	}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Stop cancels whatever is playing and drops everything queued.
func (p *Player) Stop() {
	p.mu.Lock()
	p.queue = nil
	if p.cur != nil {
		p.cur.stopped = true
		p.cur.cancel()
	}
	p.mu.Unlock()
}

// Pending reports how much the player still has to say: the request being
// played, if any, plus everything queued behind it. Zero means the device is
// making no sound and has none lined up.
//
// It exists so a caller keeping its own idea of whether this device is
// speaking can confirm it against the player's, under that caller's own
// lock. OnIdle is a report of a moment that has already passed by the time it
// is delivered; this is the question asked now.
func (p *Player) Pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.queue)
	if p.cur != nil {
		n++
	}
	return n
}

// fellSilent reports whether the queue has just run dry having played
// something, and arms the next report. Read under the lock so that it cannot
// fire twice for one silence.
func (p *Player) fellSilent() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.spoke {
		return false
	}
	p.spoke = false
	return true
}

// Current reports the request being played, if any.
func (p *Player) Current() (Request, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return Request{}, false
	}
	return p.cur.req, true
}

// Run drives the queue until ctx is cancelled. Call it in its own goroutine.
func (p *Player) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		r, playCtx, cancel, ok := p.start(ctx)
		if !ok {
			// Nothing playing and nothing waiting: the device has stopped
			// making a sound. Reported here rather than at the end of
			// playOne because that is the only place the two facts are
			// known together — a request that ended with another behind it
			// did not end the sound, and a preemption is exactly that case.
			if p.fellSilent() && p.OnIdle != nil {
				p.OnIdle()
			}
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			}
			continue
		}
		p.playOne(playCtx, cancel, r)
	}
}

// start pops the next request off the queue and installs it as current in a
// single lock hold. Popping and installing must be atomic: a request that is
// off the queue but not yet current is invisible to both Play's preemption
// check and Stop, so it would play to completion no matter which of the two
// is called in that gap.
func (p *Player) start(parent context.Context) (Request, context.Context, context.CancelFunc, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return Request{}, nil, nil, false
	}
	r := p.queue[0]
	p.queue = p.queue[1:]
	ctx, cancel := context.WithCancel(parent)
	p.cur = &current{req: r, cancel: cancel}
	p.spoke = true
	return r, ctx, cancel, true
}

func (p *Player) playOne(ctx context.Context, cancel context.CancelFunc, r Request) {
	// Decided in the body below, reported by the cleanup. It cannot be
	// derived in the cleanup itself: both the cancel() there and the
	// expire() deferred further down cancel the context this reads, so by
	// then every request that ran to the end looks cancelled.
	cut := false
	defer func() {
		cancel()
		p.mu.Lock()
		// Read while p.cur is still ours, in the hold that clears it.
		stopped := p.cur != nil && p.cur.stopped
		p.cur = nil
		p.mu.Unlock()
		if cut && p.OnTruncated != nil {
			reason := CutPreempted
			if stopped {
				reason = CutStopped
			}
			p.OnTruncated(r, reason)
		}
	}()

	limit := p.MaxPlayback
	if limit <= 0 {
		limit = defaultMaxPlayback
	}
	ctx, expire := context.WithTimeout(ctx, limit)
	defer expire()

	// A card without a Master control must not stop the sound from playing.
	device := p.deviceName()
	if err := p.backend.SetVolume(ctx, device, r.Volume); err != nil {
		p.reportStageError(r, "volume", limit, ctx.Err(), err)
	}
	// Cancelled while setting the volume (a preemption, a Stop, or the limit
	// above): the request is dead, do not hand it to Play at all. It is
	// still a request that was cut short and made no sound, which is the
	// outcome the log most needs to hear about; a timeout is not, because it
	// already leaves a volume_timeout error behind.
	if ctx.Err() != nil {
		cut = errors.Is(ctx.Err(), context.Canceled)
		return
	}
	if err := p.backend.Play(ctx, device, r.Path); err != nil {
		p.reportStageError(r, "play", limit, ctx.Err(), err)
	}
	cut = errors.Is(ctx.Err(), context.Canceled)
}

// reportStageError reports a stage failure, except when the request was
// deliberately cancelled by a preemption or Stop — there the error is the
// cancellation itself and says nothing. A timeout gets its own stage name:
// it means something is wedged in front of the speaker, which is a different
// diagnosis from a card that answered with an error, and the operator has to
// be able to tell the two apart.
func (p *Player) reportStageError(r Request, stage string, limit time.Duration, ctxErr, err error) {
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		p.reportError(r, stage+"_timeout", fmt.Errorf("the %s stage exceeded %s and was abandoned: %w", stage, limit, err))
	case ctxErr == nil:
		p.reportError(r, stage, err)
	}
}

func (p *Player) reportError(r Request, stage string, err error) {
	if p.OnError != nil {
		p.OnError(r, stage, err)
	}
}
