package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/web"
)

func newServer(t *testing.T) *web.Server {
	t.Helper()
	srv, err := web.New(web.Deps{})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	srv := newServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Fatalf("content type = %q, want a CSS type", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the stylesheet is empty — was it built and committed?")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	srv := newServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The dashboard is served over plain HTTP on a LAN, so it must not be
// embeddable in someone else's page and must not leak its URLs onward.
func TestSecurityHeadersArePresent(t *testing.T) {
	srv := newServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}
