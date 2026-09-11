package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/library"
)

// This is the screen spec §5.5 describes: alerts and scheduled events are one
// list, distinguished only by Trigger, edited by one form shape and queued by
// one rule engine. TriggerAlertStart and TriggerAlertEnd rows are the two
// fixed events every device ships with (config.Default); TriggerScheduled
// rows, the minute of silence included, are ordinary entries an operator can
// add, edit and remove like any other.

// errEventNotFound and errEventProtected are returned from inside a
// Store.Update mutator, never surfaced to an operator directly: the caller
// distinguishes errEventProtected (which does get a specific, translated
// message — see handleEventsDelete) from everything else, which falls back
// to the generic rejection message on the theory that config.Validate's
// checks should never actually be reached by a form this page renders
// correctly in the first place.
var (
	errEventNotFound  = errors.New("event not found")
	errEventProtected = errors.New("alert-start and alert-end can be disabled but not removed")
)

// weekdayOption is one checkbox in the weekday picker. Value is the ISO
// weekday number config.Event.Weekdays stores (Monday = 1); Label is what an
// operator reads.
type weekdayOption struct {
	Value int
	Label string
}

// weekdayOptions is fixed, Monday first, matching how a Ukrainian calendar
// week reads.
var weekdayOptions = []weekdayOption{
	{1, "Пн"}, {2, "Вт"}, {3, "Ср"}, {4, "Чт"}, {5, "Пт"}, {6, "Сб"}, {7, "Нд"},
}

// triggerLabel translates a trigger constant into the Ukrainian label shown
// on the page. Anything unrecognised is shown as-is rather than hidden,
// matching kindLabel's reasoning in handlers.go.
func triggerLabel(trigger string) string {
	switch trigger {
	case config.TriggerAlertStart:
		return "Початок тривоги"
	case config.TriggerAlertEnd:
		return "Кінець тривоги"
	case config.TriggerScheduled:
		return "За розкладом"
	default:
		return trigger
	}
}

// eventRowView is what one row of events.html reads. Scheduled gates both
// the name/time/weekday inputs and the delete button: alert-start and
// alert-end are the only triggers that are not TriggerScheduled, and they are
// exactly the ones spec §5.5 (and handleEventsDelete) says can never be
// removed, only disabled.
type eventRowView struct {
	ID           string
	Trigger      string
	TriggerLabel string
	Name         string
	Time         string
	Weekdays     []int
	Sound        string
	Volume       int
	Enabled      bool
	Scheduled    bool
}

// eventsView is what events.html reads.
type eventsView struct {
	Rows []eventRowView

	// Sounds lists what an event's Sound field may be set to. Empty (rather
	// than nil-and-panicking) when Deps.Library is nil or its List call
	// fails, matching homeView.Sounds — see LibraryNotice for what the page
	// says instead in that case.
	Sounds        []library.Sound
	LibraryNotice string

	QuietHours     config.QuietHours
	WeekdayOptions []weekdayOption

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once — every handler sets exactly one
	// before rendering.
	Error   string
	Message string

	// AddName, AddTime, AddSound and AddWeekdays re-populate the "add an
	// event" form after a rejected submission, the way network.go's
	// SelectedSSID re-selects a rejected network: an operator who mistyped
	// the volume should not have to retype the name and pick the weekdays
	// again too.
	AddName     string
	AddTime     string
	AddSound    string
	AddWeekdays []int
}

// hasWeekday reports whether day is present in days. It backs the
// "hasWeekday" template function events.html uses to mark a weekday
// checkbox checked.
func hasWeekday(days []int, day int) bool {
	for _, d := range days {
		if d == day {
			return true
		}
	}
	return false
}

// indexOfEvent returns the index of the event with the given id, or -1. It is
// used both outside Store.Update (to render a fresh view) and inside it (to
// find the exact entry a mutator is about to change), so a lookup and the
// change it guards always agree about which row they mean.
func indexOfEvent(events []config.Event, id string) int {
	for i, e := range events {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// newEventID names a freshly added scheduled event. It never collides with
// an existing id: config.Validate rejects a duplicate id outright, and a
// silently-dropped "add" because two events happened to get the same
// generated name would be a worse bug than this loop guards against.
func newEventID(existing []config.Event) string {
	for i := 0; ; i++ {
		id := fmt.Sprintf("scheduled-%d", time.Now().UnixNano()+int64(i))
		if indexOfEvent(existing, id) < 0 {
			return id
		}
	}
}

// soundMissing reports whether sound is one the library does not have.
// config.Validate cannot make this check at all — the library lives on disk,
// not in the configuration — so an event pointing at a missing file would
// otherwise only fail at the one moment it matters: when the engine tries to
// play it. A blank sound is left to config.Validate's "sound is required"
// instead, so this never produces a message naming an empty string. With no
// Library wired up (nil, same tolerance every other page here gives it),
// nothing can be verified, so nothing is refused on this ground — a device
// without a library wired up has no evidence to reject with.
func (s *Server) soundMissing(sound string) bool {
	return sound != "" && s.deps.Library != nil && !s.deps.Library.Has(sound)
}

// volumeErrorMessage is shown for any volume this form will not accept:
// unparseable (an empty form submission, most notably — config.Validate's
// 0..100 range would otherwise happily accept Atoi's zero value for a blank
// field, letting an empty POST here mutate stored state, which leak_test.go's
// walk of every route assumes cannot happen), zero, negative, or over 100.
// Zero gets a message of its own reason rather than "outside 1..100" alone:
// rules.ForEvent treats event volume 0 as an explicit, silent mute — the
// same effect as disabling the event, but invisible on the home screen's
// AlertsDisabled warning (see alertStartDisabled in handlers.go). Refusing
// it here keeps "disable the event" the one, visible way to silence it,
// rather than leaving a second, hidden one.
const volumeErrorMessage = "Гучність має бути від 1 до 100. Щоб вимкнути подію, зніміть позначку «Увімкнено» — гучність 0 для цього не призначена."

// parseVolume parses and range-checks the "volume" form field. It refuses
// anything config.Validate would not itself need to catch: a blank or
// non-numeric value, and — stricter than config.Validate's own 0..100 range —
// zero, for the reason volumeErrorMessage explains.
func parseVolume(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 100 {
		return 0, false
	}
	return v, true
}

// parseWeekdays reads the (possibly empty, possibly repeated) "weekdays"
// form values into ISO weekday numbers. A value that fails to parse is
// dropped rather than rejected outright: config.Validate already refuses an
// out-of-range or, after dropping garbage, an empty weekday list, and this
// keeps that the one place the message comes from.
func parseWeekdays(values []string) []int {
	var out []int
	for _, v := range values {
		if n, err := strconv.Atoi(v); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// eventsBaseView builds the view a fresh GET, a success and a rejection all
// start from, so the three never drift apart in what they show for the parts
// of the page a given POST did not touch.
func (s *Server) eventsBaseView(cfg *config.Config) eventsView {
	view := eventsView{
		QuietHours:     cfg.QuietHours,
		WeekdayOptions: weekdayOptions,
	}
	for _, ev := range cfg.Events {
		view.Rows = append(view.Rows, eventRowView{
			ID:           ev.ID,
			Trigger:      ev.Trigger,
			TriggerLabel: triggerLabel(ev.Trigger),
			Name:         ev.Name,
			Time:         ev.Time,
			Weekdays:     ev.Weekdays,
			Sound:        ev.Sound,
			Volume:       ev.Volume,
			Enabled:      ev.Enabled,
			Scheduled:    ev.Trigger == config.TriggerScheduled,
		})
	}

	// Library is nil until Task 9 wires the dashboard up and in this
	// package's own tests that don't need it — same guard handleHome uses
	// for the manual-playback form.
	if s.deps.Library == nil {
		view.LibraryNotice = "Бібліотека звуків недоступна на цьому пристрої."
		return view
	}
	sounds, err := s.deps.Library.List()
	if err != nil {
		s.deps.Logger.Error("library: list", "err", err)
		view.LibraryNotice = "Не вдалося завантажити список звуків."
		return view
	}
	view.Sounds = sounds
	return view
}

// renderEventsPage re-renders the whole screen from the store's current
// state, then lets mutate adjust the view — a rejection's Error and any
// re-populated "add" fields, or a success's Message. Every POST handler ends
// through here rather than redirecting, the same shape handleNetworkConnect
// uses for a rejected submission: the operator is shown the result directly,
// with nothing they typed lost.
func (s *Server) renderEventsPage(w http.ResponseWriter, status int, mutate func(*eventsView)) {
	view := s.eventsBaseView(s.deps.Config.Get())
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "events.html", view)
}

// handleEventsPage renders the events screen: every alert and scheduled
// event in one list, one editor per row, plus quiet hours.
func (s *Server) handleEventsPage(w http.ResponseWriter, r *http.Request) {
	s.renderEventsPage(w, http.StatusOK, nil)
}

// handleEventsSave edits an existing event's sound, volume and enabled state
// — and, for a TriggerScheduled row, its name, time and weekdays too.
// alert-start and alert-end have no time or weekdays to edit, so those
// fields are only read and applied for the trigger that has them; this is
// also what makes disabling alert-start (the home-screen warning's subject)
// go through the exact same form as everything else, rather than a special
// case.
func (s *Server) handleEventsSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	id := r.FormValue("id")
	sound := r.FormValue("sound")
	enabled := r.FormValue("enabled") != ""
	name := r.FormValue("name")
	timeStr := r.FormValue("time")
	weekdays := parseWeekdays(r.Form["weekdays"])

	volume, ok := parseVolume(r.FormValue("volume"))
	if !ok {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = volumeErrorMessage
		})
		return
	}
	if s.soundMissing(sound) {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = fmt.Sprintf("Звук «%s» відсутній у бібліотеці.", sound)
		})
		return
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		idx := indexOfEvent(c.Events, id)
		if idx < 0 {
			return fmt.Errorf("%w: %q", errEventNotFound, id)
		}
		ev := &c.Events[idx]
		ev.Sound = sound
		ev.Volume = volume
		ev.Enabled = enabled
		if ev.Trigger == config.TriggerScheduled {
			ev.Name = name
			ev.Time = timeStr
			ev.Weekdays = weekdays
		}
		return nil
	})
	if err != nil {
		s.deps.Logger.Error("events: save rejected", "id", id, "err", err)
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = "Не вдалося зберегти подію. Перевірте введені дані."
		})
		return
	}
	s.renderEventsPage(w, http.StatusOK, func(v *eventsView) { v.Message = "Подію збережено." })
}

// handleEventsAdd creates a new TriggerScheduled event. alert-start and
// alert-end are not created here — config.Default() is the only place either
// is ever created — so every event this handler produces is one an operator
// can also delete.
func (s *Server) handleEventsAdd(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	timeStr := r.FormValue("time")
	sound := r.FormValue("sound")
	weekdays := parseWeekdays(r.Form["weekdays"])

	reject := func(message string) {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = message
			v.AddName = name
			v.AddTime = timeStr
			v.AddSound = sound
			v.AddWeekdays = weekdays
		})
	}

	volume, ok := parseVolume(r.FormValue("volume"))
	if !ok {
		reject(volumeErrorMessage)
		return
	}
	if name == "" {
		reject("Вкажіть назву події.")
		return
	}
	if s.soundMissing(sound) {
		reject(fmt.Sprintf("Звук «%s» відсутній у бібліотеці.", sound))
		return
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.Events = append(c.Events, config.Event{
			ID:       newEventID(c.Events),
			Trigger:  config.TriggerScheduled,
			Name:     name,
			Time:     timeStr,
			Weekdays: weekdays,
			Sound:    sound,
			Volume:   volume,
			Enabled:  true,
		})
		return nil
	})
	if err != nil {
		s.deps.Logger.Error("events: add rejected", "err", err)
		reject("Не вдалося додати подію. Перевірте введені дані.")
		return
	}
	s.renderEventsPage(w, http.StatusOK, func(v *eventsView) { v.Message = "Подію додано." })
}

// handleEventsDelete removes a scheduled event. alert-start and alert-end
// are refused: they are why this device exists, and a dashboard that can
// delete them is one that can quietly turn a school's announcer into an
// ornament. They can still be disabled — see handleEventsSave — which is
// the reversible way to get the same effect.
func (s *Server) handleEventsDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	id := r.FormValue("id")
	err := s.deps.Config.Update(func(c *config.Config) error {
		idx := indexOfEvent(c.Events, id)
		if idx < 0 {
			return fmt.Errorf("%w: %q", errEventNotFound, id)
		}
		if t := c.Events[idx].Trigger; t == config.TriggerAlertStart || t == config.TriggerAlertEnd {
			return errEventProtected
		}
		c.Events = append(c.Events[:idx], c.Events[idx+1:]...)
		return nil
	})
	if err != nil {
		message := "Не вдалося видалити подію."
		switch {
		case errors.Is(err, errEventProtected):
			message = "Цю подію не можна видалити — її можна лише вимкнути."
		default:
			s.deps.Logger.Error("events: delete rejected", "id", id, "err", err)
		}
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) { v.Error = message })
		return
	}
	s.renderEventsPage(w, http.StatusOK, func(v *eventsView) { v.Message = "Подію видалено." })
}

// handleEventsQuietHours saves quiet hours. AppliesTo is never read from the
// form: spec §5.1 says quiet hours must never silence an alert, so rather
// than validate a submitted category list against that rule, the form never
// offers one, and this handler always writes the one category quiet hours
// are allowed to affect. From and To are required and validated as HH:MM
// regardless of Enabled — config.Validate itself only checks them when
// Enabled is true, but leaving them acceptably blank while disabled would
// let an empty form POST (which leak_test.go sends to every route) silently
// blank out a previously-configured schedule.
func (s *Server) handleEventsQuietHours(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	enabled := r.FormValue("enabled") != ""
	from := r.FormValue("from")
	to := r.FormValue("to")

	if _, err := config.ParseHM(from); err != nil {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = "Вкажіть час у форматі ГГ:ХХ."
		})
		return
	}
	if _, err := config.ParseHM(to); err != nil {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = "Вкажіть час у форматі ГГ:ХХ."
		})
		return
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.QuietHours = config.QuietHours{
			Enabled:   enabled,
			From:      from,
			To:        to,
			AppliesTo: []string{config.CategoryScheduled},
		}
		return nil
	})
	if err != nil {
		s.deps.Logger.Error("events: quiet hours rejected", "err", err)
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = "Не вдалося зберегти тихі години. Перевірте введені дані."
		})
		return
	}
	s.renderEventsPage(w, http.StatusOK, func(v *eventsView) { v.Message = "Тихі години збережено." })
}
