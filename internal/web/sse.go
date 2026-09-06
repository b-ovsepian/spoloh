package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/b-ovsepian/spoloh/internal/engine"
)

// subscriberBuffer is small on purpose: the stream is a liveness display, not
// a transcript. A subscriber that falls more than a few frames behind is
// better served by dropping the stale ones — the next frame carries the
// whole state.
const subscriberBuffer = 4

// publishInterval is how often the status is sampled. One second matches the
// engine's tick, which is the fastest anything on the page can change.
const publishInterval = time.Second

// Broadcaster fans a byte frame out to every subscriber currently listening.
// It is a small, reusable piece — Plan 3's access-point setup page wants one
// too — so it is exported rather than kept private to the SSE handler.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[chan []byte]struct{}{}}
}

// Subscribe registers a new listener and returns the channel frames arrive
// on. The caller must eventually call Unsubscribe with the same channel.
func (b *Broadcaster) Subscribe() chan []byte {
	ch := make(chan []byte, subscriberBuffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[ch] = struct{}{}
	return ch
}

// Unsubscribe removes ch and closes it. It is safe to call more than once
// (or concurrently with Publish) with the same channel: after the first call
// removes it from subs, later calls find nothing there and do nothing, so
// the channel is never closed twice and Publish never sends on a channel
// that Unsubscribe has already taken away.
func (b *Broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}

// Publish delivers to every subscriber that can take it right now and drops
// the frame for any that cannot. It never blocks, so one stalled subscriber
// can never wedge delivery to the rest.
func (b *Broadcaster) Publish(frame []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- frame:
		default:
		}
	}
}

// statusFrame is the shape the page's Alpine bindings read. It is
// deliberately flat and pre-formatted: the browser does no date maths and no
// logic.
type statusFrame struct {
	Region      string `json:"region"`
	Alert       bool   `json:"alert"`
	Since       string `json:"since"`
	LastSuccess string `json:"last_success"`
	LastError   string `json:"last_error"`
	Source      string `json:"source"`
	Failing     bool   `json:"failing"`
	ClockSynced bool   `json:"clock_synced"`
	NextEvent   string `json:"next_event"`
	NextEventAt string `json:"next_event_at"`
	Playing     string `json:"playing"`
}

// location returns the device's configured time zone, falling back to UTC if
// it is unset or fails to load rather than letting a bad configuration value
// take down the status stream.
func (s *Server) location() *time.Location {
	if tz := s.deps.Config.Get().Device.Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

func (s *Server) currentFrame() ([]byte, error) {
	// Engine is nil until the daemon wires the dashboard up (Task 9) and in
	// this package's own tests; a zero-value Status renders a frame with
	// every field empty rather than panicking.
	var st engine.Status
	if s.deps.Engine != nil {
		st = s.deps.Engine.Status()
	}
	loc := s.location()

	f := statusFrame{
		Region:      st.Region,
		Alert:       st.Alert,
		LastError:   st.LastError,
		Source:      st.SourceName,
		Failing:     st.Failing,
		ClockSynced: st.ClockSynced,
		NextEvent:   st.NextEventName,
		Playing:     st.PlayingEventID,
	}
	if !st.Since.IsZero() {
		f.Since = st.Since.In(loc).Format("2006-01-02 15:04")
	}
	if !st.LastSuccess.IsZero() {
		f.LastSuccess = st.LastSuccess.In(loc).Format("15:04:05")
	}
	if !st.NextEventAt.IsZero() {
		f.NextEventAt = st.NextEventAt.In(loc).Format("2006-01-02 15:04")
	}
	return json.Marshal(f)
}

// handleEvents streams status frames over Server-Sent Events until the
// client disconnects or the request's context is cancelled. Every exit path
// — early return on an unsupported ResponseWriter, the context-done case,
// and the subscriber channel closing under it — runs through the deferred
// Unsubscribe, so the goroutine that ServeHTTP is running on never leaks a
// subscription behind it.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")

	ch := s.events.Subscribe()
	defer s.events.Unsubscribe(ch)

	// Send the current state immediately so the page is correct before the
	// first tick rather than a second later.
	if frame, err := s.currentFrame(); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", frame)
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
		}
	}
}

// publishLoop samples the status once a second and fans it out. It exits
// with ctx so shutdown does not leave it running past the server it belongs
// to.
func (s *Server) publishLoop(ctx context.Context) {
	t := time.NewTicker(publishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if frame, err := s.currentFrame(); err == nil {
				s.events.Publish(frame)
			}
		}
	}
}
