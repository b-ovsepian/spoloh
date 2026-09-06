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

// The home page must not advertise an address nothing answers. It used to
// print "http://spoloh.local" unconditionally, and this test used to pin that
// — but no part of this branch publishes mDNS: the unit depends on no
// avahi-daemon and there is no publisher anywhere. That is Plan 3. It was the
// only memorable address on the page and therefore the first thing an
// operator tried, and it did not resolve.
//
// When Plan 3 publishes the name, this test flips back to asserting the line
// is present and stays ASCII: a Cyrillic hostname becomes punycode and stops
// being readable over the phone.
func TestTheHomePageAdvertisesNoUnpublishedMDNSName(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), ".local") {
		t.Error("the home page advertises a .local address that nothing in this branch publishes")
	}
}

// Spec §9 wants network health shown separately from source health: a source
// failure alone cannot tell "the router is down" from "Ubilling is down", and
// those need different actions from whoever is standing in front of the
// device. Holding no address at all is what the first one looks like.
func TestTheHomePageReportsNetworkHealth(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Мережа:") {
		t.Error("the home page carries no network-health line")
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
