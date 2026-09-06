package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every authenticated page must offer a way to every other one. Before this
// task an operator who opened /log could only get back by editing the URL.
func TestEveryPageCarriesNavigation(t *testing.T) {
	srv, _, _ := newServerWithLog(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, path := range []string{"/", "/log"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, "<nav") {
			t.Errorf("%s has no navigation", path)
		}
		for _, target := range []string{`href="/"`, `href="/log"`} {
			if !strings.Contains(body, target) {
				t.Errorf("%s does not link to %s", path, target)
			}
		}
		if !strings.Contains(body, `action="/logout"`) {
			t.Errorf("%s offers no way to log out", path)
		}
	}
}

// The appliance is Ukrainian-language; its name is spelled in Cyrillic.
func TestPagesAreNamedInCyrillic(t *testing.T) {
	srv, _ := newServerWithStore(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Сполох") {
		t.Error("the page does not carry the application's name")
	}
	if strings.Contains(body, "Spoloh") {
		t.Error("the page still shows the Latin spelling in user-facing text")
	}
}

// The mDNS name is an identifier, not a word, and must stay ASCII — a Cyrillic
// hostname becomes punycode and stops being readable over the phone.
func TestTheMDNSNameStaysLatin(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "spoloh.local") {
		t.Error("the home page no longer shows the ASCII mDNS name")
	}
}

// The unauthenticated pages are reached before a session exists, so the shell
// they must not carry is the one whose links all lead behind the login.
func TestUnauthenticatedPagesCarryNoShell(t *testing.T) {
	srv, _ := newServerWithStore(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if body := rec.Body.String(); strings.Contains(body, "<nav") {
		t.Error("/setup offers navigation to pages a visitor cannot reach yet")
	}

	setPassword(t, srv, "correct horse battery staple")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if body := rec.Body.String(); strings.Contains(body, "<nav") {
		t.Error("/login offers navigation to pages a visitor cannot reach yet")
	}
}
