package web

import (
	"net/http"

	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/scheduler"
)

// recentLogEntries caps how much of the event log a single page load reads:
// enough history to be useful, not so much that a full table on an SD card
// becomes the slow part of the request.
const recentLogEntries = 200

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

// logEntryView is what log.html reads for one row. At is pre-formatted in
// the device's configured location, same as homeView's time fields — the two
// pages must not disagree about how a timestamp looks.
type logEntryView struct {
	AtText  string
	Kind    string
	EventID string
	Sound   string
	OK      bool
	Err     string
}

// logView is what log.html reads. Notice carries an operator-facing
// explanation for why Entries is empty — a log that failed to open, a log
// whose Recent call errored, or a log that is genuinely empty yet — so the
// three cases read differently on the page instead of all looking like "no
// history".
type logView struct {
	Entries []logEntryView
	Notice  string
}

// kindLabel translates an eventlog.Kind constant into the Ukrainian label
// shown on the page. Anything unrecognised is shown as-is rather than
// hidden, since a future kind is more useful visible than blank.
func kindLabel(kind string) string {
	switch kind {
	case eventlog.KindAlert:
		return "Тривога"
	case eventlog.KindScheduled:
		return "За розкладом"
	case eventlog.KindManual:
		return "Вручну"
	case eventlog.KindSourceFailure:
		return "Збій джерела"
	case eventlog.KindConfigChange:
		return "Зміна налаштувань"
	case eventlog.KindPlaybackError:
		return "Помилка відтворення"
	default:
		return kind
	}
}

// handleLog renders the event log: what the device actually did, in its own
// timezone. Deps.Log is deliberately allowed to be nil — a corrupt SQLite
// file or a read-only SD card is not fatal to the daemon, so it must not be
// fatal to this page either.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	loc := s.location()
	var view logView

	switch {
	case s.deps.Log == nil:
		view.Notice = "Журнал подій недоступний на цьому пристрої."
	default:
		entries, err := s.deps.Log.Recent(recentLogEntries)
		switch {
		case err != nil:
			s.deps.Logger.Error("event log: recent", "err", err)
			view.Notice = "Не вдалося завантажити журнал подій."
		case len(entries) == 0:
			view.Notice = "Записів поки немає."
		default:
			view.Entries = make([]logEntryView, len(entries))
			for i, e := range entries {
				view.Entries[i] = logEntryView{
					AtText:  e.At.In(loc).Format("2006-01-02 15:04:05"),
					Kind:    kindLabel(e.Kind),
					EventID: e.EventID,
					Sound:   e.Sound,
					OK:      e.OK,
					Err:     e.Err,
				}
			}
		}
	}

	s.render(w, http.StatusOK, "log.html", view)
}
