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
		r, ok := p.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			}
			continue
		}
		p.playOne(ctx, r)
	}
}

func (p *Player) pop() (Request, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return Request{}, false
	}
	r := p.queue[0]
	p.queue = p.queue[1:]
	return r, true
}

func (p *Player) playOne(parent context.Context, r Request) {
	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	p.cur = &current{req: r, cancel: cancel}
	p.mu.Unlock()

	defer func() {
		cancel()
		p.mu.Lock()
		p.cur = nil
		p.mu.Unlock()
	}()

	// A card without a Master control must not stop the sound from playing.
	if err := p.backend.SetVolume(ctx, p.device, r.Volume); err != nil {
		p.reportError(r, "volume", err)
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
