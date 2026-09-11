package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/web"
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
	// On a board with no monitor this page is the whole diagnostic surface;
	// a playback error must point at the one screen that can fix it, by
	// name. Checked as the whole sentence — hint text, link, and the page's
	// current label together — rather than as a bare href="/audio" or a
	// bare label: layout.html's navigation links to /audio on every page
	// too, under the exact same label, so either alone would prove nothing
	// about this row specifically, and the full sentence is what a plain
	// href="/audio" check cannot tell from a hint that stopped naming the
	// page at all. It is checked once per layout (the mobile <li> and the
	// table <td> each render their own copy), so both count.
	hint := `Перевірте вихідний пристрій на сторінці «<a href="/audio" class="underline">Аудіовихід</a>».`
	if got := strings.Count(body, hint); got != 2 {
		t.Errorf("hint %q appears %d times, want 2 (one per layout); body:\n%s", hint, got, body)
	}
}

// A failure that has nothing to do with the output device — a source
// unreachable, say — must not send an operator to /audio looking for a
// problem that is not there.
func TestLogDoesNotPointNonPlaybackFailuresAtAudio(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := lg.Append(eventlog.Entry{
		Kind: eventlog.KindSourceFailure, OK: false, Err: "context deadline exceeded",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "Перевірте вихідний пристрій") {
		t.Error("a non-playback failure points at /audio anyway")
	}
}

// The page shows the most recent rows out of a database that holds many times
// as many. Listing them with nothing said reads as "this is everything that
// ever happened", which is precisely the wrong conclusion to draw when the
// event being looked for has already scrolled off.
func TestLogPageSaysItIsTruncated(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	appendAlert(t, lg, "siren.wav")

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "найновіших записів") {
		t.Errorf("the log page does not say that it shows only the most recent rows; body: %q", body)
	}
	if !strings.Contains(body, "200") {
		t.Error("the log page does not say how many rows it shows")
	}
}

// --- Clearing ------------------------------------------------------------
//
// The most destructive control on the device: the log is the record of what
// it did during real air raids, there is no undo, and nothing else holds a
// copy.

// clearForm posts to /log/clear the way the page's form does, through the
// whole handler stack so the session and same-origin checks are exercised
// rather than bypassed.
func clearForm(t *testing.T, srv *web.Server, cookie *http.Cookie, confirm string) *httptest.ResponseRecorder {
	t.Helper()
	return eventsPostForm(t, srv, cookie, "/log/clear", url.Values{"confirm": {confirm}})
}

func TestLogClearEmptiesTheLog(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")
	appendAlert(t, lg, "all-clear.wav")

	rec := clearForm(t, srv, cookie, "ОЧИСТИТИ")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	entries, err := lg.Recent(100)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	for _, e := range entries {
		if e.Kind == eventlog.KindAlert {
			t.Fatalf("an alert entry survived the clear: %+v", e)
		}
	}
	// The re-rendered page has to show the emptied log, not the rows the
	// operator asked to be rid of, and it has to say what just happened —
	// a page that looks the same as before is a page that leaves an
	// operator pressing the button again.
	body := rec.Body.String()
	if strings.Contains(body, "siren.wav") {
		t.Errorf("the page still lists a cleared entry; body:\n%s", body)
	}
	if !strings.Contains(body, "Журнал подій очищено") {
		t.Errorf("the page does not confirm the clear; body:\n%s", body)
	}
}

// An empty log cannot tell "this device has never done anything" from
// "somebody erased the record", and on this device that difference is the
// whole reason a log exists. The entry the clear leaves behind has to be
// visible on the page, not only in the database.
func TestLogClearLeavesARecordOfItselfOnThePage(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")
	rec := clearForm(t, srv, cookie, "ОЧИСТИТИ")

	entries, err := lg.Recent(100)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != eventlog.KindLogCleared {
		t.Fatalf("the log holds %+v, want exactly one %q entry", entries, eventlog.KindLogCleared)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Журнал очищено") {
		t.Errorf("the page does not show that the log was cleared; body:\n%s", body)
	}
}

// The confirmation is the only thing between a mis-tap on a phone and the
// permanent loss of the device's record of an air raid.
func TestLogClearWithoutConfirmationDoesNothing(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	for _, confirm := range []string{"", "   ", "ОЧИСТ", "yes", "ОЧИСТИТИ ЖУРНАЛ", "clear"} {
		t.Run(confirm, func(t *testing.T) {
			rec := clearForm(t, srv, cookie, confirm)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("confirm=%q: status = %d, want 400", confirm, rec.Code)
			}

			n, err := lg.Count()
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if n != 1 {
				t.Fatalf("confirm=%q: the log holds %d entries, want the 1 it started with — nothing may be written", confirm, n)
			}
			if body := rec.Body.String(); !strings.Contains(body, "Журнал не очищено") {
				t.Errorf("confirm=%q: the page does not say the log was left alone; body:\n%s", confirm, body)
			}
		})
	}
}

// A phone keyboard that lowercases the first letter must not turn a
// considered decision into a refusal the operator cannot explain. The
// deliberateness is in having typed the word at all.
func TestLogClearAcceptsTheConfirmationInAnyCase(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	rec := clearForm(t, srv, cookie, "  очистити  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	entries, err := lg.Recent(100)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != eventlog.KindLogCleared {
		t.Fatalf("the log holds %+v, want exactly the entry recording the clear", entries)
	}
}

func TestLogClearRejectsCrossOrigin(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	form := url.Values{"confirm": {"ОЧИСТИТИ"}}
	req := httptest.NewRequest(http.MethodPost, "/log/clear", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	n, err := lg.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("the log holds %d entries, want the 1 it started with — a rejected request must not clear", n)
	}
}

// A prefetch, a link preview or a bookmark sync must never erase the record
// of an air raid.
func TestLogClearIsRejectedOnGet(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	req := httptest.NewRequest(http.MethodGet, "/log/clear?confirm=%D0%9E%D0%A7%D0%98%D0%A1%D0%A2%D0%98%D0%A2%D0%98", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("GET /log/clear = 200; a prefetch would erase the log")
	}
	n, err := lg.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("the log holds %d entries, want the 1 it started with — a GET must not clear", n)
	}
}

func TestLogClearRequiresASession(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	req := httptest.NewRequest(http.MethodPost, "/log/clear", strings.NewReader("confirm=%D0%9E%D0%A7%D0%98%D0%A1%D0%A2%D0%98%D0%A2%D0%98"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("POST /log/clear with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	n, err := lg.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("the log holds %d entries, want the 1 it started with", n)
	}
}

// A button that cannot do anything teaches an operator to distrust the page.
// A device whose log failed to open — a corrupt SQLite file, a read-only SD
// card — has nothing to clear and must not offer to.
func TestLogPageOffersNoClearFormWithoutALog(t *testing.T) {
	srv, _ := newServerWithStore(t) // Deps.Log is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `action="/log/clear"`) {
		t.Error("the page offers to clear a log it does not have")
	}
}

// The safeguard is a form, not a script. A confirm() dialog would stop
// working the moment JavaScript did, and on this device the page has to be
// fully usable without it — the same rule the upload form on /sounds follows.
func TestLogPageClearFormWorksWithoutJavaScript(t *testing.T) {
	srv, _, lg := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	appendAlert(t, lg, "siren.wav")

	req := httptest.NewRequest(http.MethodGet, "/log", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`<form method="post" action="/log/clear"`,
		`name="confirm"`,
		`type="submit"`,
		"ОЧИСТИТИ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the clear form is missing %q; body:\n%s", want, body)
		}
	}
	// It must say what it destroys before the operator presses anything.
	if !strings.Contains(body, "Відновити їх буде неможливо") {
		t.Errorf("the page does not warn that clearing cannot be undone; body:\n%s", body)
	}
}
