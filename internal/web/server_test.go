package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/web"
)

func newServerWithStore(t *testing.T) (*web.Server, *configstore.Store) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store
}

func newServer(t *testing.T) *web.Server {
	t.Helper()
	srv, _ := newServerWithStore(t)
	return srv
}

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func setPassword(t *testing.T, srv *web.Server, password string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{"password": {password}, "confirm": {password}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup failed: status %d", rec.Code)
	}
}

func login(t *testing.T, srv *web.Server, password string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {password}}))
	return sessionCookie(t, rec)
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "spoloh_session" {
			return c
		}
	}
	t.Fatal("no session cookie was issued")
	return nil
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
