package web

import (
	"net/http"

	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/scheduler"
)

// homeView is what home.html reads. Every time is pre-formatted in the
// device's configured location: the template escapes these into a
// JavaScript literal for Alpine's initial state, and a time.Time there would
// render as a Go struct.
type homeView struct {
	Status          engine.Status
	Addresses       []string
	Hostname        string
	SinceText       string
	LastSuccessText string
	NextEventAtText string
}

// handleHome renders the dashboard: the page an operator actually looks at.
// Every value is rendered server-side so the page is correct with
// JavaScript disabled or before Alpine has loaded — the SSE stream wired up
// in home.html only keeps it fresh afterward.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Get()
	loc := s.location()

	// Engine is nil until the daemon wires the dashboard up (Task 9) and in
	// this package's own tests; a zero-value Status renders a page with
	// every engine-derived field empty rather than panicking, the same
	// guard sse.go uses for the event stream.
	var st engine.Status
	if s.deps.Engine != nil {
		st = s.deps.Engine.Status()
	}
	// Region and the next scheduled event are pure functions of the
	// configuration, not of engine state, so they are always shown even
	// before the engine exists. NextUp is documented as safe for exactly
	// this: a fresh, stateless Runner reads the same configured events the
	// live scheduler would.
	st.Region = cfg.Alerts.Region
	runner := scheduler.NewRunner(nil, s.deps.Config.Get, nil)
	if ev, at, ok := runner.NextUp(s.now().In(loc)); ok {
		st.NextEventName = ev.Name
		if st.NextEventName == "" {
			st.NextEventName = ev.ID
		}
		st.NextEventAt = at
	}

	view := homeView{
		Status:    st,
		Addresses: LocalAddresses(),
		Hostname:  cfg.Device.Name,
	}
	if !st.Since.IsZero() {
		view.SinceText = st.Since.In(loc).Format("2006-01-02 15:04")
	}
	if !st.LastSuccess.IsZero() {
		view.LastSuccessText = st.LastSuccess.In(loc).Format("15:04:05")
	}
	if !st.NextEventAt.IsZero() {
		view.NextEventAtText = st.NextEventAt.In(loc).Format("2006-01-02 15:04")
	}

	s.render(w, http.StatusOK, "home.html", view)
}
