// Package player owns audio output. There is exactly one voice: a device with
// two sounds overlapping is worse than useless during an emergency.
package player

import (
	"context"
	"sync"
)

type Priority int

const (
	PriorityScheduled Priority = iota
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

type current struct {
	req    Request
	cancel context.CancelFunc
}

type Player struct {
	backend Backend
	device  string

	// OnError reports a failed stage ("volume" or "play"). Set it before Run.
	OnError func(r Request, stage string, err error)

	mu    sync.Mutex
	queue []Request
	cur   *current
	wake  chan struct{}
}

func New(b Backend, device string) *Player {
	return &Player{backend: b, device: device, wake: make(chan struct{}, 1)}
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
		p.cur.cancel()
	}
	p.mu.Unlock()
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
	return r, ctx, cancel, true
}

func (p *Player) playOne(ctx context.Context, cancel context.CancelFunc, r Request) {
	defer func() {
		cancel()
		p.mu.Lock()
		p.cur = nil
		p.mu.Unlock()
	}()

	// A card without a Master control must not stop the sound from playing.
	if err := p.backend.SetVolume(ctx, p.device, r.Volume); err != nil && ctx.Err() == nil {
		p.reportError(r, "volume", err)
	}
	// Cancelled while setting the volume (a preemption or Stop landed in that
	// window): the request is dead, do not hand it to Play at all.
	if ctx.Err() != nil {
		return
	}
	if err := p.backend.Play(ctx, p.device, r.Path); err != nil && ctx.Err() == nil {
		p.reportError(r, "play", err)
	}
}

func (p *Player) reportError(r Request, stage string, err error) {
	if p.OnError != nil {
		p.OnError(r, stage, err)
	}
}
