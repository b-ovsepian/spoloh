package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
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

// Len reports how many subscribers are listening right now. publishLoop uses
// it to skip sampling altogether when nobody is, which on a wall-mounted
// appliance is most of the time.
func (b *Broadcaster) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
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
	// LevelLabel and LevelNote are the banner's severity line, already in
	// words — see handlers.go's alertLevel, which is the only place the
	// level is ever branched on. The page does not receive the raw level
	// and must not: "is this red" is Level.TreatAsRed's answer, and a
	// second implementation of it in JavaScript is exactly where a raid
	// upstream never named a level for would come out as the lesser
	// warning.
	//
	// In the frame rather than rendered once, for the reason the all-clear
	// counter below is: an escalation is a raid changing severity under
	// somebody who is already looking at the screen, and a level frozen at
	// page-load would still be saying yellow while the siren says otherwise.
	LevelLabel string `json:"level_label"`
	LevelNote  string `json:"level_note"`
	// AllClearSeen and AllClearNeeded carry an all-clear upstream has
	// reported and this device is not announcing yet — see
	// engine.Status's own comment for why the screen has to say so.
	//
	// They are in the frame rather than rendered once, and that is the
	// whole point of putting them here: the run is at most
	// alertstate.EndConfirmations polls long and it is counted while
	// somebody is standing in front of the screen asking why the thing is
	// still red. A number that froze at whatever it was when the page
	// loaded would be stuck during exactly the half-minute it exists for,
	// which would make "my device looks stuck" worse rather than better.
	AllClearSeen   int    `json:"all_clear_seen"`
	AllClearNeeded int    `json:"all_clear_needed"`
	ClockSynced    bool   `json:"clock_synced"`
	NextEvent      string `json:"next_event"`
	NextEventAt    string `json:"next_event_at"`
	Playing        string `json:"playing"`
}

// location returns the device's configured time zone, falling back to UTC if
// it is unset or fails to load rather than letting a bad configuration value
// take down the status stream.
//
// The result is cached, keyed by the configured name so that changing the
// zone in the dashboard still takes effect. time.LoadLocation caches nothing
// of its own: it re-reads and re-parses the zoneinfo on every call, off the
// SD card whenever the OS has a zoneinfo directory of its own. Doing that
// once a second forever, for a value that changes about never, is the kind
// of write-adjacent churn spec §5.7 exists to avoid. Caching the failed
// lookup as UTC is deliberate too — a misconfigured zone must not turn into
// a retry loop.
func (s *Server) location() *time.Location {
	tz := s.deps.Config.Get().Device.Timezone

	s.locMu.Lock()
	defer s.locMu.Unlock()
	if s.loc != nil && s.locTZ == tz {
		return s.loc
	}
	loc := time.UTC
	if tz != "" {
		if resolved, err := time.LoadLocation(tz); err == nil {
			loc = resolved
		}
	}
	s.locTZ, s.loc = tz, loc
	return loc
}

func (s *Server) currentFrame() ([]byte, error) {
	loc := s.location()
	// status() fills in Region and the next scheduled event from the
	// configuration even when Engine is nil — the same fallback handleHome
	// uses — so the stream and the page it keeps fresh never disagree about
	// what the daemon has no engine for yet.
	st := s.status(loc)
	level := alertLevel(st)

	f := statusFrame{
		Region:    st.Region,
		Alert:     st.Alert,
		LastError: st.LastError,
		Source:    st.SourceName,
		Failing:   st.Failing,

		LevelLabel: level.Label,
		LevelNote:  level.Note,

		AllClearSeen:   st.AllClearSeen,
		AllClearNeeded: st.AllClearNeeded,

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
			// With nobody subscribed there is nobody to publish to, and
			// sampling anyway is not free: currentFrame clones the
			// configuration, resolves a time zone, walks the event list,
			// then allocates and marshals a frame — once a second, for the
			// whole life of a daemon that spends most of its time with no
			// browser attached.
			if s.events.Len() == 0 {
				continue
			}
			if frame, err := s.currentFrame(); err == nil {
				s.events.Publish(frame)
			}
		}
	}
}
