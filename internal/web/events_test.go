package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// newServerWithLibrary wires a real library.Library backed by a temp
// directory holding the given sound names (empty WAV headers — good enough
// for library.Has, which only stats the path), for events_test.go's exercise
// of the library-membership rule.
func newServerWithLibrary(t *testing.T, sounds ...string) (*web.Server, *configstore.Store) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	dir := t.TempDir()
	for _, name := range sounds {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("RIFF____WAVE"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	lib := library.New(dir, library.FFmpeg{})
	srv, err := web.New(web.Deps{Config: store, Library: lib})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store
}

func eventsPostForm(t *testing.T, srv *web.Server, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEventsPageRequiresSession(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /events with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

// The default config ships alert-start, alert-end and the minute-of-silence
// scheduled event; this proves all three render with their real values, not
// just that the page returns 200.
func TestEventsPageListsEveryEvent(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`value="siren.wav" selected`,
		`value="all-clear.wav" selected`,
		`value="metronome.wav" selected`,
		`value="100"`, // alert-start's volume
		`value="90"`,  // alert-end's volume
		`value="85"`,  // silence's volume
		`Хвилина мовчання`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q; every event's fields must be in the markup", want)
		}
	}
	// alert-start and alert-end are both enabled by default; each row's
	// "enabled" checkbox must say so.
	if got := strings.Count(body, `name="enabled" checked`); got < 2 {
		t.Errorf("found %d checked \"enabled\" boxes, want at least 2 (alert-start, alert-end)", got)
	}
}

// Each row's repeat field must render the event's own stored value, wired to
// its own row by id — not a shared default that happens to look right on a
// freshly shipped config where every event's repeat is 1.
func TestEventsPageShowsEachEventsRepeatCount(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		for i := range c.Events {
			if c.Events[i].ID == "alert-start" {
				c.Events[i].Repeat = 4
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	want := `id="repeat-alert-start" name="repeat" value="4"`
	if !strings.Contains(body, want) {
		t.Errorf("page does not contain %q; alert-start's repeat count must be in its own row's markup", want)
	}
}

func TestEventsSaveRejectsCrossOrigin(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Events[0]

	req := httptest.NewRequest(http.MethodPost, "/events/save", strings.NewReader(url.Values{
		"id": {"alert-start"}, "sound": {"other.wav"}, "volume": {"50"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := store.Get().Events[0]; !reflect.DeepEqual(got, before) {
		t.Errorf("event changed despite the cross-origin rejection: %+v -> %+v", before, got)
	}
}

func TestEventsSaveWritesSoundVolumeAndEnabled(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "klaxon.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"klaxon.wav"}, "volume": {"42"}, "repeat": {"3"}, "enabled": {"on"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	idx := -1
	for i, ev := range store.Get().Events {
		if ev.ID == "alert-start" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("alert-start disappeared")
	}
	ev := store.Get().Events[idx]
	if ev.Sound != "klaxon.wav" || ev.Volume != 42 || ev.Repeat != 3 || !ev.Enabled {
		t.Fatalf("alert-start = %+v, want sound klaxon.wav, volume 42, repeat 3, enabled true", ev)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `value="klaxon.wav" selected`) {
		t.Error("the re-rendered page does not show the new sound selected")
	}
	if !strings.Contains(body, `value="42"`) {
		t.Error("the re-rendered page does not show the new volume")
	}
}

func TestEventsSaveRejectsASoundNotInTheLibrary(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Events

	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"nonexistent.wav"}, "volume": {"100"}, "repeat": {"1"}, "enabled": {"on"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nonexistent.wav") {
		t.Error("the rejection message does not name the missing sound")
	}
	if got := store.Get().Events; !eventsEqual(got, before) {
		t.Errorf("events changed despite the rejection: %+v -> %+v", before, got)
	}
}

func TestEventsSaveRejectsAnOutOfRangeVolume(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Events

	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"siren.wav"}, "volume": {"150"}, "enabled": {"on"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := store.Get().Events; !eventsEqual(got, before) {
		t.Errorf("events changed despite the rejection: %+v -> %+v", before, got)
	}
}

// rules.ForEvent treats event volume 0 as an explicit, silent mute — the
// same effect as disabling the event, but invisible to homeView's
// AlertsDisabled warning (see handlers.go's alertStartDisabled). The form
// refuses it so that disabling the event stays the one, visible way to
// silence it, and says so rather than just rejecting the number.
func TestEventsSaveRejectsZeroVolume(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Events

	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"siren.wav"}, "volume": {"0"}, "enabled": {"on"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Увімкнено") {
		t.Error("the rejection does not point to disabling the event as the way to silence it")
	}
	if got := store.Get().Events; !eventsEqual(got, before) {
		t.Errorf("events changed despite the rejection: %+v -> %+v", before, got)
	}
}

// Repeat is validated the same way volume is — see parseRepeat — and out of
// range means outside 1..10, not merely non-numeric.
func TestEventsSaveRejectsAnOutOfRangeRepeat(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Events

	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"siren.wav"}, "volume": {"100"}, "repeat": {"11"}, "enabled": {"on"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := store.Get().Events; !eventsEqual(got, before) {
		t.Errorf("events changed despite the rejection: %+v -> %+v", before, got)
	}
}

// Unlike volume, repeat has no legal zero: config.Event.Repeat's floor is 1,
// so a submitted 0 is refused as out of range rather than accepted or given
// a meaning of its own the way parseVolume's zero has.
func TestEventsSaveRejectsAZeroRepeat(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Events

	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"siren.wav"}, "volume": {"100"}, "repeat": {"0"}, "enabled": {"on"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := store.Get().Events; !eventsEqual(got, before) {
		t.Errorf("events changed despite the rejection: %+v -> %+v", before, got)
	}
}

func TestEventsAddRejectsZeroVolume(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav", "bell.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := len(store.Get().Events)
	rec := eventsPostForm(t, srv, cookie, "/events/add", url.Values{
		"name": {"Дзвінок"}, "time": {"08:30"}, "weekdays": {"1"}, "sound": {"bell.wav"}, "volume": {"0"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := len(store.Get().Events); got != before {
		t.Errorf("event count %d -> %d, want unchanged", before, got)
	}
}

func TestEventsAddCreatesAScheduledEvent(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav", "bell.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := len(store.Get().Events)
	rec := eventsPostForm(t, srv, cookie, "/events/add", url.Values{
		"name": {"Дзвінок на урок"}, "time": {"08:30"}, "weekdays": {"1", "2", "3", "4", "5"},
		"sound": {"bell.wav"}, "volume": {"70"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	events := store.Get().Events
	if len(events) != before+1 {
		t.Fatalf("event count %d -> %d, want +1", before, len(events))
	}
	var added *config.Event
	for i := range events {
		if events[i].Name == "Дзвінок на урок" {
			added = &events[i]
		}
	}
	if added == nil {
		t.Fatal("the new event is not in the stored config")
	}
	if added.Trigger != config.TriggerScheduled || added.Time != "08:30" || added.Sound != "bell.wav" || added.Volume != 70 {
		t.Errorf("added event = %+v, want trigger scheduled, time 08:30, sound bell.wav, volume 70", *added)
	}
	if len(added.Weekdays) != 5 {
		t.Errorf("added event weekdays = %v, want 5 days", added.Weekdays)
	}

	if !strings.Contains(rec.Body.String(), "Дзвінок на урок") {
		t.Error("the re-rendered page does not list the new event")
	}
}

// The add form has no repeat field of its own — like Enabled, a freshly
// added event starts at the shipped default and is edited afterward from the
// row it produces, rather than asking the operator to commit to it before
// the event exists.
func TestEventsAddCreatesAnEventWithDefaultRepeatOfOne(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav", "bell.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/events/add", url.Values{
		"name": {"Збір"}, "time": {"08:30"}, "weekdays": {"1"}, "sound": {"bell.wav"}, "volume": {"70"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	events := store.Get().Events
	var added *config.Event
	for i := range events {
		if events[i].Name == "Збір" {
			added = &events[i]
		}
	}
	if added == nil {
		t.Fatal("the new event is not in the stored config")
	}
	if added.Repeat != 1 {
		t.Errorf("repeat = %d, want 1", added.Repeat)
	}
}

func TestEventsAddRejectsNoWeekdaySelected(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav", "bell.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := len(store.Get().Events)
	rec := eventsPostForm(t, srv, cookie, "/events/add", url.Values{
		"name": {"Дзвінок"}, "time": {"08:30"}, "sound": {"bell.wav"}, "volume": {"70"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := len(store.Get().Events); got != before {
		t.Errorf("event count %d -> %d, want unchanged", before, got)
	}
	if rec.Body.Len() == 0 {
		t.Error("the rejection re-rendered an empty page")
	}
}

func TestEventsAddRejectsABadTime(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav", "bell.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := len(store.Get().Events)
	rec := eventsPostForm(t, srv, cookie, "/events/add", url.Values{
		"name": {"Дзвінок"}, "time": {"not-a-time"}, "weekdays": {"1"}, "sound": {"bell.wav"}, "volume": {"70"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := len(store.Get().Events); got != before {
		t.Errorf("event count %d -> %d, want unchanged", before, got)
	}
}

func TestEventsDeleteRemovesAScheduledEvent(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/events/delete", url.Values{"id": {"silence"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, ev := range store.Get().Events {
		if ev.ID == "silence" {
			t.Fatal("the scheduled event is still in the stored config")
		}
	}
}

// alert-start and alert-end are why this device exists. They are editable and
// they may be disabled, but a dashboard that can delete them is a dashboard
// that can quietly turn the site's announcer into an ornament.
func TestEventsRefusesToDeleteAnAlertEvent(t *testing.T) {
	for _, id := range []string{"alert-start", "alert-end"} {
		srv, store := newServerWithStore(t)
		setPassword(t, srv, "correct horse battery staple")
		cookie := login(t, srv, "correct horse battery staple")

		before := len(store.Get().Events)
		form := url.Values{"id": {id}}
		req := httptest.NewRequest(http.MethodPost, "/events/delete", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+req.Host)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if got := len(store.Get().Events); got != before {
			t.Errorf("%s: event count %d -> %d, want unchanged", id, before, got)
		}
		var found bool
		for _, ev := range store.Get().Events {
			if ev.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was deleted", id)
		}
	}
}

func TestEventsSaveCanDisableAlertStart(t *testing.T) {
	srv, store := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	// No "enabled" field at all: an unchecked checkbox submits nothing.
	rec := eventsPostForm(t, srv, cookie, "/events/save", url.Values{
		"id": {"alert-start"}, "sound": {"siren.wav"}, "volume": {"100"}, "repeat": {"1"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	for _, ev := range store.Get().Events {
		if ev.ID == "alert-start" && ev.Enabled {
			t.Fatal("alert-start is still enabled after being unchecked")
		}
	}
}

func TestEventsQuietHoursSavesEnabledFromToAndTheCategoryList(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/events/quiet-hours", url.Values{
		"enabled": {"on"}, "from": {"23:00"}, "to": {"06:30"},
		"applies_to": {"scheduled"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	qh := store.Get().QuietHours
	if !qh.Enabled || qh.From != "23:00" || qh.To != "06:30" {
		t.Fatalf("quiet hours = %+v, want enabled, 23:00-06:30", qh)
	}
	if len(qh.AppliesTo) != 1 || qh.AppliesTo[0] != config.CategoryScheduled {
		t.Fatalf("quiet_hours.applies_to = %v, want only %q", qh.AppliesTo, config.CategoryScheduled)
	}
	if qh.SilencesAlerts() {
		t.Fatal("a submission that did not ask for alerts left the device silencing them")
	}
}

// The guard the whole feature was granted under. A form that would extend
// quiet hours over air raid alerts, without the typed word, changes nothing
// at all \u2014 not the categories, not the hours, not the enabled flag. A phone
// in a pocket cannot do this.
func TestEventsQuietHoursRefusesToCoverAlertsWithoutTheTypedWord(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().QuietHours
	for _, confirm := range []string{"", "\u0442\u0430\u043a", "\u041c\u041e\u0412\u0427\u0410\u041d\u041d\u042f"} {
		form := url.Values{
			"enabled": {"on"}, "from": {"22:00"}, "to": {"07:00"},
			"applies_to": {"scheduled", "alerts"},
		}
		if confirm != "" {
			form.Set("confirm_alerts", confirm)
		}
		rec := eventsPostForm(t, srv, cookie, "/events/quiet-hours", form)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("confirm %q: status = %d, want 400", confirm, rec.Code)
		}
		got := store.Get().QuietHours
		if got.SilencesAlerts() {
			t.Fatalf("confirm %q: the device is now silencing air raid alerts without the confirmation", confirm)
		}
		if !reflect.DeepEqual(got, before) {
			t.Fatalf("confirm %q: quiet hours changed on a refused submission: %+v -> %+v", confirm, before, got)
		}
	}
}

// And the other side of it: an operator who does type the word gets exactly
// what they asked for, and the page says so rather than reporting a bare
// "saved". Lower case counts \u2014 see quietHoursAlertConfirmation for why a
// phone keyboard must not be able to turn a considered decision into a
// refusal nobody can explain.
func TestEventsQuietHoursCoversAlertsWithTheTypedWord(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/events/quiet-hours", url.Values{
		"enabled": {"on"}, "from": {"22:00"}, "to": {"07:00"},
		"applies_to": {"scheduled", "alerts"}, "confirm_alerts": {" \u043c\u043e\u0432\u0447\u0430\u0442\u0438 "},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	qh := store.Get().QuietHours
	if !qh.SilencesAlerts() {
		t.Fatalf("quiet hours = %+v, want a window that silences alerts", qh)
	}
	if !qh.Covers(config.CategoryScheduled) {
		t.Fatalf("quiet hours = %+v, want scheduled still covered too", qh)
	}
	// The page an operator lands on has to state the consequence, not just
	// acknowledge the save.
	body := rec.Body.String()
	if !strings.Contains(body, "\u041d\u0415 \u043f\u043e\u0434\u0430\u0432\u0430\u0442\u0438\u043c\u0435 \u0441\u0438\u0433\u043d\u0430\u043b") {
		t.Error("the page did not say in plain words that alerts will not sound")
	}
	if !strings.Contains(body, "22:00") || !strings.Contains(body, "07:00") {
		t.Error("the warning did not name the hours it applies to")
	}
}

// Turning it back off, and moving the hours while it is on, need no word.
// A confirmation demanded for making this device louder is a confirmation an
// operator learns to type without reading.
func TestEventsQuietHoursNeedsNoWordToStopCoveringAlertsOrToMoveTheHours(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.QuietHours = config.QuietHours{
			Enabled: true, From: "22:00", To: "07:00",
			AppliesTo: []string{config.CategoryScheduled, config.CategoryAlerts},
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rec := eventsPostForm(t, srv, cookie, "/events/quiet-hours", url.Values{
		"enabled": {"on"}, "from": {"23:00"}, "to": {"06:00"},
		"applies_to": {"scheduled", "alerts"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("moving the hours: status = %d, want 200", rec.Code)
	}
	if qh := store.Get().QuietHours; qh.From != "23:00" || !qh.SilencesAlerts() {
		t.Fatalf("quiet hours = %+v, want 23:00 and alerts still covered", qh)
	}

	rec = eventsPostForm(t, srv, cookie, "/events/quiet-hours", url.Values{
		"enabled": {"on"}, "from": {"23:00"}, "to": {"06:00"},
		"applies_to": {"scheduled"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("uncovering alerts: status = %d, want 200", rec.Code)
	}
	if qh := store.Get().QuietHours; qh.SilencesAlerts() {
		t.Fatalf("quiet hours = %+v, want alerts no longer covered", qh)
	}
}

// While the state is on, the form itself has to say so on a plain GET \u2014 not
// only in the flash message of the save that turned it on. An operator
// opening this page a week later must read the consequence off it.
func TestEventsPageStatesThatAlertsWillNotSound(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "\u041d\u0415 \u043f\u043e\u0434\u0430\u0432\u0430\u0442\u0438\u043c\u0435 \u0441\u0438\u0433\u043d\u0430\u043b") {
		t.Fatal("the page warns that alerts will not sound on a device that sounds them")
	}

	if err := store.Update(func(c *config.Config) error {
		c.QuietHours = config.QuietHours{
			Enabled: true, From: "22:00", To: "07:00",
			AppliesTo: []string{config.CategoryAlerts},
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "\u041d\u0415 \u043f\u043e\u0434\u0430\u0432\u0430\u0442\u0438\u043c\u0435 \u0441\u0438\u0433\u043d\u0430\u043b") {
		t.Error("the page does not say that this device will not sound an air raid alert")
	}
	if !strings.Contains(body, "22:00") || !strings.Contains(body, "07:00") {
		t.Error("the page does not name the hours during which it will not sound")
	}
	if !strings.Contains(body, `value="alerts" checked`) {
		t.Error("the alerts category checkbox does not read back as checked")
	}
}

// An empty form must not blank out a previously configured schedule — the
// same guarantee leak_test.go's whole-route walk relies on for every POST.
func TestEventsQuietHoursRejectsAnEmptyForm(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().QuietHours
	rec := eventsPostForm(t, srv, cookie, "/events/quiet-hours", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := store.Get().QuietHours; !reflect.DeepEqual(got, before) {
		t.Errorf("quiet hours changed on an empty submission: %+v -> %+v", before, got)
	}
}

func TestEventsPageRendersWithNilLibrary(t *testing.T) {
	srv, _ := newServerWithStore(t) // Deps.Library is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no library", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<form") {
		t.Error("the page rendered no form at all with a nil library")
	}
}

func eventsEqual(a, b []config.Event) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}
