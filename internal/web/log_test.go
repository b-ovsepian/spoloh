package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
}
