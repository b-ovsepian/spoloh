package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
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
// on the page. Unlike kindLabel in handlers.go (whose unrecognised case is a
// developer-facing log constant, harmless as-is), a Trigger comes from
// config.json — echoing it verbatim would put raw, possibly English, text on
// a page every other word of which is Ukrainian, so anything unrecognised
// gets a Ukrainian fallback instead of the trigger string itself.
func triggerLabel(trigger string) string {
	switch trigger {
	case config.TriggerAlertStart:
		return "Початок тривоги"
	case config.TriggerAlertEnd:
		return "Кінець тривоги"
	case config.TriggerScheduled:
		return "За розкладом"
	default:
		return "Невідома подія"
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
	Repeat       int
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

	// QuietScheduled and QuietAlerts are QuietHours.AppliesTo unpacked into
	// the two checkboxes the form draws. They are precomputed rather than
	// asked for from the template, because the helper that would answer
	// "is this string in that slice" would have to be registered in
	// render.go's FuncMap, and this page is the only caller there would
	// ever be for it.
	QuietScheduled bool
	QuietAlerts    bool

	// AlertsSilenced is config.QuietHours.SilencesAlerts: the window is on
	// and it covers alerts, so this device will not sound an air raid alert
	// between those hours. It is the exact condition the home page's
	// standing warning tests, so the two screens cannot come to disagree
	// about whether the state is on.
	AlertsSilenced bool

	// AlertConfirmation is the word an operator must type to switch alert
	// suppression on, rendered into the form so the page and the handler
	// quote the same string.
	AlertConfirmation string

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

// The three ways parseVolume refuses a submission each say something
// different, rather than sharing one message tuned for only one of them: a
// blank or non-numeric field is a different mistake from typing 150, and
// both are different from typing 0, which is the one case with an actual
// alternative to point at — see parseVolume.
const (
	volumeInvalidMessage    = "Вкажіть гучність числом від 1 до 100."
	volumeOutOfRangeMessage = "Гучність має бути від 1 до 100."
	// rules.ForEvent treats event volume 0 as an explicit, silent mute — the
	// same effect as disabling the event, but invisible on the home
	// screen's AlertsDisabled warning (see alertStartDisabled in
	// handlers.go). Refusing it here keeps "disable the event" the one,
	// visible way to silence it, rather than leaving a second, hidden one.
	volumeZeroMessage = "Гучність не може бути 0. Щоб вимкнути подію, зніміть позначку «Увімкнено»."
)

// parseVolume parses and range-checks the "volume" form field. On failure it
// returns the message that explains why: unparseable (an empty form
// submission, most notably — config.Validate's 0..100 range would otherwise
// happily accept Atoi's zero value for a blank field, letting an empty POST
// here mutate stored state, which leak_test.go's walk of every route assumes
// cannot happen), zero specifically, or out of range otherwise. An empty
// return value means volume is valid.
func parseVolume(s string) (int, string) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, volumeInvalidMessage
	}
	if v == 0 {
		return 0, volumeZeroMessage
	}
	if v < 0 || v > 100 {
		return 0, volumeOutOfRangeMessage
	}
	return v, ""
}

// The two ways parseRepeat refuses a submission, the model for both being
// parseVolume above. Unlike volume, repeat has no legal zero to give its own
// message: config.Event.Repeat's floor is 1 (see its doc comment and
// config.Validate), so an operator who clears the field or types 0 gets the
// same out-of-range message as one who types 11.
const (
	repeatInvalidMessage    = "Вкажіть кількість повторів числом від 1 до 10."
	repeatOutOfRangeMessage = "Кількість повторів має бути від 1 до 10."
)

// parseRepeat parses and range-checks the "repeat" form field. An empty
// return value means repeat is valid.
func parseRepeat(s string) (int, string) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, repeatInvalidMessage
	}
	if v < 1 || v > 10 {
		return 0, repeatOutOfRangeMessage
	}
	return v, ""
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
		QuietHours:        cfg.QuietHours,
		WeekdayOptions:    weekdayOptions,
		QuietScheduled:    cfg.QuietHours.Covers(config.CategoryScheduled),
		QuietAlerts:       cfg.QuietHours.Covers(config.CategoryAlerts),
		AlertsSilenced:    cfg.QuietHours.SilencesAlerts(),
		AlertConfirmation: quietHoursAlertConfirmation,
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
			Repeat:       ev.Repeat,
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

	volume, volumeErr := parseVolume(r.FormValue("volume"))
	if volumeErr != "" {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = volumeErr
		})
		return
	}
	repeat, repeatErr := parseRepeat(r.FormValue("repeat"))
	if repeatErr != "" {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = repeatErr
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
		ev.Repeat = repeat
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

	volume, volumeErr := parseVolume(r.FormValue("volume"))
	if volumeErr != "" {
		reject(volumeErr)
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
			// A freshly added event starts at the same repeat count every
			// device ships with, editable afterward from the row this
			// produces — the same treatment Enabled below already gets;
			// neither is a choice this form makes the operator commit to
			// before the event exists.
			Repeat:  1,
			Enabled: true,
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
// delete them is one that can quietly turn the site's announcer into an
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

// quietHoursAlertConfirmation is the word an operator has to type before
// quiet hours may be extended to cover air raid alerts.
//
// A typed word rather than a second checkbox or a two-step submit, copying
// /log's clear button (see logClearConfirmation) because the reasoning is
// the same one: this device is set up from a phone held next to it, and two
// taps in roughly the same place — or one checkbox next to four others — is
// exactly what a mis-tap looks like. Typing seven Cyrillic letters is not
// something a thumb does by accident. It needs nothing but an <input> and a
// submit button, so the safeguard survives JavaScript being off, which this
// device requires of every control it has.
//
// "МОВЧАТИ" and not "ТАК": the word an operator types should be the
// thing they are agreeing to. Matched case-insensitively, with Unicode
// folding, so «мовчати» counts — the deliberateness is in having typed
// the word, and a phone keyboard that lowercases the first letter must not
// turn a considered decision into a refusal nobody can explain.
const quietHoursAlertConfirmation = "МОВЧАТИ"

// quietHoursAlertConfirmationMessage is what a submission that would have
// silenced alerts without the word gets back. It names the word and says
// what was and was not changed, because the checkbox the operator ticked is
// re-rendered unticked and a page that simply looked unchanged would read as
// a bug.
const quietHoursAlertConfirmationMessage = "Тихі години не змінено. Щоб вони поширювалися на сигнал тривоги, введіть «" +
	quietHoursAlertConfirmation + "» у поле підтвердження."

// parseCategories reads the (possibly empty, possibly repeated) "applies_to"
// form values into a quiet-hours category list.
//
// It is an allow-list, and the order is this function's, not the form's, so
// what lands in config.json is the same two-element vocabulary however a
// browser happened to serialise the checkboxes. A value this build does not
// recognise is dropped rather than refused: an unknown category silences
// nothing (config.QuietHours.Covers matches on equality), and storing one
// would put a value in config.json that no screen shows and no operator can
// clear again.
func parseCategories(values []string) []string {
	var out []string
	for _, known := range []string{config.CategoryAlerts, config.CategoryScheduled} {
		if slices.Contains(values, known) {
			out = append(out, known)
		}
	}
	return out
}

// handleEventsQuietHours saves quiet hours, including which categories they
// cover.
//
// AppliesTo used to be ignored here and overwritten with the one category
// quiet hours were permitted to affect. The operator asked for the list to
// mean what it says, alerts included, so this now reads it — and guards the
// one transition that matters.
//
// The guard is on becoming able to silence an alert, not on the checkbox.
// Two different submissions reach that state: ticking "alerts" on a window
// that is already on, and switching a window on that already lists alerts
// (which a hand-edited config.json can leave behind). Both are asked for the
// word; neither can happen by one tap. Everything in the other direction —
// unticking alerts, switching the window off, moving the hours while alerts
// stay covered — goes through untouched, because a confirmation demanded for
// making the device louder is a confirmation an operator learns to type
// without reading.
//
// From and To are required and validated as HH:MM regardless of Enabled —
// config.Validate itself only checks them when Enabled is true, but leaving
// them acceptably blank while disabled would let an empty form POST (which
// leak_test.go sends to every route) silently blank out a previously
// configured schedule.
func (s *Server) handleEventsQuietHours(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	enabled := r.FormValue("enabled") != ""
	from := r.FormValue("from")
	to := r.FormValue("to")
	appliesTo := parseCategories(r.Form["applies_to"])

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

	// Read the stored window before the update so the transition is judged
	// against what the device is doing now, not against what this form is
	// asking for. Store.Update serialises writers, so the worst a racing
	// save can do is make this handler ask for the word when the other
	// writer had just switched alert coverage on — an extra confirmation,
	// never a skipped one.
	willSilenceAlerts := enabled && slices.Contains(appliesTo, config.CategoryAlerts)
	if willSilenceAlerts && !s.deps.Config.Get().QuietHours.SilencesAlerts() &&
		!strings.EqualFold(strings.TrimSpace(r.FormValue("confirm_alerts")), quietHoursAlertConfirmation) {
		s.renderEventsPage(w, http.StatusBadRequest, func(v *eventsView) {
			v.Error = quietHoursAlertConfirmationMessage
		})
		return
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.QuietHours = config.QuietHours{
			Enabled:   enabled,
			From:      from,
			To:        to,
			AppliesTo: appliesTo,
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

	// The success line says which state the device is now in rather than
	// just "saved". Turning this on is the one save on this page that
	// changes what happens during an air raid, and the operator should read
	// that back from the page they are already looking at.
	message := "Тихі години збережено."
	if willSilenceAlerts {
		message = "Тихі години збережено. З " + from + " до " + to +
			" цей пристрій НЕ подаватиме сигнал повітряної тривоги."
	}
	s.renderEventsPage(w, http.StatusOK, func(v *eventsView) { v.Message = message })
}
