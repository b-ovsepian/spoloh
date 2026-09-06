package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/eventlog"
)

func TestLogPageRendersEntries(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "siren.wav") {
		t.Error("the entry is not on the page")
	}
}

// A device whose event log failed to open still has to render this page.
func TestLogPageWithoutALog(t *testing.T) {
	srv, _ := newServerWithStore(t) // Deps.Log is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no event log", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Журнал подій недоступний на цьому пристрої.") {
		t.Error("a nil log should say the log is unavailable, distinctly from an empty or broken one")
	}
}

// This is the regression the whole task exists to prevent: a timestamp
// rendered in the wrong zone. Since eventlog.Recent no longer forces UTC,
// the failure mode that actually threatens this page is handleLog dropping
// its own explicit .In(loc) call: time.UnixMilli would then fall back to
// time.Local, the *process's* zone — which, inconveniently, is already
// Europe/Kyiv on some machines (this repo's dev sandbox included), so a
// naive .Format() would pass by accident there without the .In(loc) call
// doing anything at all. Pinning time.Local to UTC for this test's duration
// removes that accident: the assertion below only passes if the handler
// actually converts through the configured location, regardless of what
// zone the machine running the suite happens to be in.
func TestLogPageRendersTimesInTheConfiguredZone(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })

	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	at := time.Date(2026, 9, 6, 15, 41, 0, 0, time.UTC)
	if err := lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindAlert, Sound: "siren.wav", OK: true}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// 15:41 UTC is 18:41 in Kyiv (UTC+3 in September) — the device's
	// default configured zone. With time.Local pinned to UTC above, a
	// handler that fell back to time.Local instead of the configured zone
	// would show 15:41 here instead.
	body := rec.Body.String()
	if !strings.Contains(body, "2026-09-06 18:41:00") {
		t.Fatalf("body does not contain the Kyiv wall clock 2026-09-06 18:41:00 for a 15:41 UTC entry:\n%s", body)
	}
	if strings.Contains(body, "2026-09-06 15:41:00") {
		t.Fatal("body shows the UTC/Local wall clock instead of the configured Kyiv zone")
	}
}

// A log that opened fine but has nothing in it yet must read differently
// from a log that could not be opened at all: "nothing happened" and "we
// can't tell what happened" lead an operator to opposite conclusions.
func TestLogPageShowsEmptyNotice(t *testing.T) {
	srv, _, _ := newServerWithLog(t) // real log, opened, never appended to
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Записів поки немає.") {
		t.Error("an empty-but-open log should say so, distinctly from a missing or broken one")
	}
}

// A log that fails mid-request (a corrupt file, a lost SD card) must still
// render 200 with an explanation, distinct from "nil" and "empty".
func TestLogPageShowsLoadFailureNotice(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	// Closing the underlying *sql.DB makes the handler's own Recent call
	// fail without needing a real corrupt file on disk.
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when Recent fails", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Не вдалося завантажити журнал подій.") {
		t.Error("a log that failed to load should say so, distinctly from an empty or missing one")
	}
}

// A failure must be tellable from a success by more than the row's colour:
// the word and a mark in front of it carry it too.
func TestLogMarksFailuresWithMoreThanColour(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := lg.Append(eventlog.Entry{
		Kind: eventlog.KindPlaybackError, EventID: "alert-start", Sound: "siren.wav",
		OK: false, Err: "aplay: device or resource busy",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "✕ Помилка") {
		t.Error("a failed entry is not marked with anything but colour")
	}
	if !strings.Contains(body, "aplay: device or resource busy") {
		t.Error("the failure's reason is not on the page")
	}
	// The stacked rendering is what a phone gets; the table is hidden there.
	if !strings.Contains(body, "sm:hidden") || !strings.Contains(body, "hidden sm:block") {
		t.Error("the log has only one layout, so a phone gets the six-column table")
	}
}
