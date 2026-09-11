package web

import (
	"net/http"
	"strings"

	"github.com/b-ovsepian/spoloh/internal/eventlog"
)

// This file is the /log screen: what the device actually did, and the one
// control that can erase it. It was carved out of handlers.go when the clear
// button arrived — every other screen here (sounds.go, events.go, audio.go)
// already owns its own file, and the log had outgrown sitting alongside the
// dashboard.

// recentLogEntries caps how much of the event log a single page load reads:
// enough history to be useful, not so much that a full table on an SD card
// becomes the slow part of the request.
const recentLogEntries = 200

// logClearConfirmation is the word an operator has to type before the log is
// erased.
//
// A typed word rather than a two-step submit, and the reason is the phone.
// This device is set up and checked from one, standing next to it, and a
// two-step submit is two taps in roughly the same place — which is what a
// mis-tap is. Typing eight Cyrillic letters is not something a thumb does by
// accident. It also needs nothing but an <input> and a submit button, so the
// safeguard survives JavaScript being off, which a confirm() dialog would
// not.
//
// Matched case-insensitively (Unicode folding, so «очистити» counts): the
// deliberateness is in having typed the word at all, and a phone keyboard
// that helpfully lowercases the first letter must not turn a considered
// decision into a refusal the operator cannot explain.
const logClearConfirmation = "ОЧИСТИТИ"

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

	// Error is a rejected clear's reason; Message is a completed one's
	// confirmation. Never both at once — the same contract soundsView and
	// eventsView keep. Both are distinct from Notice above, which explains
	// why there are no rows rather than what the operator just did.
	Error   string
	Message string

	// Clearable is false when there is no log to clear — nothing opened one,
	// or it could not be read. The form is hidden rather than shown and
	// refused: a button that cannot do anything is a button that teaches an
	// operator to distrust the page.
	Clearable bool
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
	case eventlog.KindPlaybackTruncated:
		return "Відтворення перервано"
	case eventlog.KindAlertSuppressed:
		return "Тривогу не озвучено"
	case eventlog.KindLogCleared:
		return "Журнал очищено"
	default:
		return kind
	}
}

// logBaseView reads the log and builds the view a fresh GET, a refused clear
// and a completed one all start from. Deps.Log is deliberately allowed to be
// nil — a corrupt SQLite file or a read-only SD card is not fatal to the
// daemon, so it must not be fatal to this page either.
func (s *Server) logBaseView() logView {
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
			view.Clearable = true
			view.Notice = "Записів поки немає."
		default:
			view.Clearable = true
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
	return view
}

// renderLogPage re-renders the whole screen from the log's current state,
// then lets mutate adjust the view. Every handler here ends through this
// rather than redirecting, the same shape /sounds and /events use: after a
// clear the operator sees the emptied log itself, which is the only honest
// confirmation that it worked.
func (s *Server) renderLogPage(w http.ResponseWriter, status int, mutate func(*logView)) {
	view := s.logBaseView()
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "log.html", view)
}

// handleLog renders the event log: what the device actually did, in its own
// timezone.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	s.renderLogPage(w, http.StatusOK, nil)
}

// handleLogClear erases the event log, on an operator's deliberate command.
//
// This is the most destructive control on the device. The log is the record
// of what it did during real air raids, there is no undo, and nothing else
// holds a copy — so the confirmation above is checked before anything is
// touched, and an empty or wrong one is a no-op that says so rather than a
// silent refusal.
//
// POST, behind the session, behind sameOrigin — the same three the rest of
// the mutating handlers here stand behind. A GET must never reach it: a
// prefetch, a link preview or a bookmark sync would otherwise erase the
// record of an air raid without anyone touching the page.
func (s *Server) handleLogClear(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// Deps.Log is nil on a device whose log failed to open — a corrupt
	// SQLite file, a read-only SD card. There is nothing to clear, and the
	// page says so rather than reporting a success it did not have.
	if s.deps.Log == nil {
		s.renderLogPage(w, http.StatusServiceUnavailable, func(v *logView) {
			v.Error = "Журнал подій недоступний на цьому пристрої."
		})
		return
	}

	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), logClearConfirmation) {
		s.renderLogPage(w, http.StatusBadRequest, func(v *logView) {
			v.Error = "Журнал не очищено. Щоб підтвердити, введіть «" + logClearConfirmation + "» у полі підтвердження."
		})
		return
	}

	if err := s.deps.Log.Clear(s.now()); err != nil {
		s.deps.Logger.Error("event log: clear", "err", err)
		s.renderLogPage(w, http.StatusInternalServerError, func(v *logView) {
			v.Error = "Не вдалося очистити журнал подій."
		})
		return
	}

	s.renderLogPage(w, http.StatusOK, func(v *logView) {
		v.Message = "Журнал подій очищено. Залишився один запис про саме очищення."
	})
}
