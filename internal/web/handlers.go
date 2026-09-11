package web

import (
	"net/http"
	"net/url"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
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
	Status engine.Status
	// Addresses is both the "what do I type into a browser" answer and the
	// page's network-health signal: holding no address at all is what "not on
	// the network" looks like from inside the daemon.
	Addresses []string
	// MDNS is the name an operator can type instead of an address, empty
	// when nothing publishes one or the device holds no address for it to
	// resolve to — see MDNSName and reachableMDNSName. It sits beside
	// Addresses because it answers the same question and fails the same way.
	MDNS            string
	SinceText       string
	LastSuccessText string
	NextEventAtText string
	// Sounds lists what an operator may choose from the manual-playback
	// form. Empty (rather than nil-and-panicking) when Deps.Library is nil
	// or its List call fails, so the rest of the page still renders.
	Sounds []library.Sound
	// Flash is a one-shot notice carried across the redirect from /play or
	// /stop, so the page an operator lands back on after pressing a button
	// visibly says what just happened instead of looking unchanged.
	Flash string
	// AlertsDisabled is true when the alert-start event is disabled, or
	// silenced the other way an operator (or a hand-edited config.json) can
	// reach: volume 0. Either leaves the device unable to announce an air
	// raid, and it is the one warning on this whole dashboard that must be
	// impossible not to notice: an operator who disables it for a ten-minute
	// PA test and forgets is the worst outcome this product has, and
	// home.html renders it above everything else, unconditionally, whenever
	// this is true.
	AlertsDisabled bool
}

// alertStartDisabled reports whether the configured alert-start event will
// not be heard: disabled, volume 0, or missing outright. events.go's
// /events forms refuse a volume of 0 for exactly this reason — disabling
// the event is meant to be the one, visible way to silence it — but nothing
// stops a volume of 0 reaching config.json by hand, and that is exactly the
// person who most needs this page to say so.
//
// It walks cfg.Events by Trigger rather than assuming a fixed index or a
// fixed ID, since nothing about the events list's shape guarantees either.
// A config missing an alert-start event entirely reports true — the second
// of two independent lines of defence against that: config.Load restores
// one from config.Default whenever it reads a config missing it, so this
// case should never actually be reached in production, but a *Config built
// some other way — a test, an import, a future migration that constructs
// one without going through Load — must not read as "fine" here just
// because the loop found nothing to object to.
func alertStartDisabled(cfg *config.Config) bool {
	for _, ev := range cfg.Events {
		if ev.Trigger == config.TriggerAlertStart {
			return !ev.Enabled || ev.Volume == 0
		}
	}
	return true
}

// status returns the engine's live status with two fields already filled in
// even when there is no engine at all: Region and the next scheduled event
// are pure functions of the configuration, not of engine state, so they are
// always shown, before the engine exists or in this package's own tests.
// NextUp is documented as safe for exactly this: a fresh, stateless Runner
// reads the same configured events the live scheduler would.
//
// Both handleHome and the SSE stream in sse.go call this one method rather
// than each computing Region and NextEvent themselves, so the page an
// operator loaded and the frames that keep it updated can never disagree
// about what the daemon has no engine for yet.
func (s *Server) status(loc *time.Location) engine.Status {
	// Engine is nil until the daemon wires the dashboard up (Task 9) and in
	// this package's own tests; a zero-value Status renders a page with
	// every engine-derived field empty rather than panicking.
	var st engine.Status
	if s.deps.Engine != nil {
		st = s.deps.Engine.Status()
	}
	st.Region = s.deps.Config.Get().Alerts.Region
	runner := scheduler.NewRunner(nil, s.deps.Config.Get, nil)
	if ev, at, ok := runner.NextUp(s.now().In(loc)); ok {
		st.NextEventName = ev.Name
		if st.NextEventName == "" {
			st.NextEventName = ev.ID
		}
		st.NextEventAt = at
	}
	return st
}

// handleHome renders the dashboard: the page an operator actually looks at.
// Every value is rendered server-side so the page is correct with
// JavaScript disabled or before Alpine has loaded — the SSE stream wired up
// in home.html only keeps it fresh afterward.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	loc := s.location()
	st := s.status(loc)

	addrs := LocalAddresses()
	view := homeView{
		Status:         st,
		Addresses:      addrs,
		MDNS:           reachableMDNSName(MDNSName(), addrs),
		AlertsDisabled: alertStartDisabled(s.deps.Config.Get()),
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

	// Library is nil until Task 9 wires the dashboard up and in this
	// package's own tests that don't need it — same guard as Engine above.
	if s.deps.Library != nil {
		sounds, err := s.deps.Library.List()
		if err != nil {
			s.deps.Logger.Error("library: list", "err", err)
		} else {
			view.Sounds = sounds
		}
	}
	view.Flash = s.flashMessage(r)

	s.render(w, http.StatusOK, "home.html", view)
}

// flashMessage turns the query parameters /play and /stop redirect back with
// into the Ukrainian notice shown on the page. Query parameters rather than a
// cookie: the message is meant to be seen exactly once, by the browser that
// is about to render this response, and never needs to survive past it.
//
// The "played" value is never echoed unless Library confirms it names a real
// sound. It is otherwise attacker-controllable: anyone who gets an already
// logged-in operator to open a crafted "?played=..." link could plant
// arbitrary text on the device's own trusted dashboard. html/template would
// still escape it — this is not an injection fix — but an unvalidated string
// has no business looking like something the device said.
func (s *Server) flashMessage(r *http.Request) string {
	q := r.URL.Query()
	switch {
	case q.Has("stopped"):
		return "Відтворення зупинено."
	case q.Has("played"):
		sound := q.Get("played")
		// Library is nil in this package's own tests and before Task 9
		// wires the dashboard up — same guard as handleHome's Sounds
		// lookup above. With no library to confirm against, the name
		// cannot be trusted, so it falls back to the generic message too.
		if s.deps.Library != nil && s.deps.Library.Has(sound) {
			return "Відтворюється: " + sound
		}
		return "Відтворення почалося."
	default:
		return ""
	}
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
	// PlaybackError marks a row the player itself failed on — as opposed to
	// a source failure or a rejected manual request — so log.html can point
	// at /audio right where the error is read. On a board with no monitor
	// this log page is the whole diagnostic surface, and the raw aplay
	// error alone (device already named — see player.ALSA.Play) does not
	// say where to go to fix it.
	PlaybackError bool
}

// logView is what log.html reads. Notice carries an operator-facing
// explanation for why Entries is empty — a log that failed to open, a log
// whose Recent call errored, or a log that is genuinely empty yet — so the
// three cases read differently on the page instead of all looking like "no
// history".
type logView struct {
	Entries []logEntryView
	Notice  string
	// Limit is how many rows a page load reads. It is shown, because the
	// page otherwise implies that what it lists is the whole history — and
	// the database holds ten times as much.
	Limit int
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
	view := logView{Limit: recentLogEntries}

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
					AtText:        e.At.In(loc).Format("2006-01-02 15:04:05"),
					Kind:          kindLabel(e.Kind),
					EventID:       e.EventID,
					Sound:         e.Sound,
					OK:            e.OK,
					Err:           e.Err,
					PlaybackError: e.Kind == eventlog.KindPlaybackError,
				}
			}
		}
	}

	s.render(w, http.StatusOK, "log.html", view)
}

// handlePlay is spec §5.6's first-class manual path: the one that still works
// when upstream is down but the threat is real. Registered as POST /play so
// requireAuth protects it by default and a GET — a prefetch, a link preview,
// a bookmark sync — can never reach it, and sameOrigin closes the one gap
// SameSite cookies leave open for a state-changing POST.
//
// The sound name is handed to Engine.PlayManual exactly as the operator sent
// it: PlayManual already refuses anything that is not a real library entry,
// and building or checking a filesystem path here would only duplicate that
// check with a second chance to get it wrong.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// Engine is nil in this package's own tests and before Task 9 wires the
	// dashboard up — same guard as handleHome and sse.go.
	if s.deps.Engine == nil {
		http.Error(w, "Відтворення недоступне на цьому пристрої.", http.StatusServiceUnavailable)
		return
	}

	sound := r.FormValue("sound")
	volume := s.deps.Config.Get().Audio.MasterVolume
	if err := s.deps.Engine.PlayManual(sound, volume); err != nil {
		s.deps.Logger.Warn("manual playback rejected", "sound", sound, "err", err)
		http.Error(w, "Такого звука немає в бібліотеці.", http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, "/?"+url.Values{"played": {sound}}.Encode(), http.StatusSeeOther)
}

// handleStop is manual playback's other half: the button an operator reaches
// for in a hurry, so it must silence whatever is on the speaker right now,
// not merely clear what was queued behind it.
//
// It goes through Engine.StopManual rather than Player.Stop for the same
// reason /play goes through PlayManual: the engine is what records the
// operator's action in the event log. Calling the player directly left the
// log showing that someone started the siren and never that they stopped it.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// Engine is nil in this package's own tests and before Task 9 wires the
	// dashboard up — same guard as handlePlay and sse.go. There is then
	// nothing to stop, which is a safe no-op rather than an error: unlike
	// /play, a stop that finds nothing playing has already achieved what the
	// operator wanted.
	if s.deps.Engine != nil {
		s.deps.Engine.StopManual()
	}
	http.Redirect(w, r, "/?stopped=1", http.StatusSeeOther)
}
