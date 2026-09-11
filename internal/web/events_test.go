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
		"id": {"alert-start"}, "sound": {"klaxon.wav"}, "volume": {"42"}, "enabled": {"on"},
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
	if ev.Sound != "klaxon.wav" || ev.Volume != 42 || !ev.Enabled {
		t.Fatalf("alert-start = %+v, want sound klaxon.wav, volume 42, enabled true", ev)
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
		"id": {"alert-start"}, "sound": {"nonexistent.wav"}, "volume": {"100"}, "enabled": {"on"},
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
// that can quietly turn a school's announcer into an ornament.
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
		"id": {"alert-start"}, "sound": {"siren.wav"}, "volume": {"100"},
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

func TestEventsQuietHoursSavesEnabledFromToAndAppliesToScheduledOnly(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/events/quiet-hours", url.Values{
		"enabled": {"on"}, "from": {"23:00"}, "to": {"06:30"},
		// A crafted or stale form might still send "alerts"; the handler
		// must ignore it rather than let quiet hours ever silence an alert.
		"applies_to": {"alerts", "scheduled"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	qh := store.Get().QuietHours
	if !qh.Enabled || qh.From != "23:00" || qh.To != "06:30" {
		t.Fatalf("quiet hours = %+v, want enabled, 23:00-06:30", qh)
	}
	if len(qh.AppliesTo) != 1 || qh.AppliesTo[0] != config.CategoryScheduled {
		t.Fatalf("quiet_hours.applies_to = %v, want only %q — quiet hours must never silence an alert", qh.AppliesTo, config.CategoryScheduled)
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
