package web_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// authedBody logs in and returns the rendered page at path. The shell is what
// these tests are about, so they read several pages each and the four-line
// request dance once per page buried what was being checked.
func authedBody(t *testing.T, srv *web.Server, path string) string {
	t.Helper()
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200", path, rec.Code)
	}
	return rec.Body.String()
}

// newServerAtVersion builds a server stamped with a particular build string.
// No helper in server_test.go can: they all leave Deps.Version empty, which
// is the one case the footer must not be checked against alone.
func newServerAtVersion(t *testing.T, version string) *web.Server {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store, Version: version})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

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

	// The signal is the addresses themselves when there are any — their
	// presence is what "on the network" looks like from inside the daemon —
	// and a named absence when there are none. Either is a health line; a
	// page carrying neither tells the operator nothing about which half of
	// the path is broken.
	body := rec.Body.String()
	addrs := web.LocalAddresses()
	if len(addrs) == 0 {
		if !strings.Contains(body, "не в мережі") {
			t.Error("the home page does not report holding no address as being off the network")
		}
		return
	}
	for _, addr := range addrs {
		if !strings.Contains(body, addr) {
			t.Errorf("the home page does not carry the address %s, which is its network-health signal", addr)
		}
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

// The header's measure must follow the page's, not pin its own. Three
// screens widen past the default, and a nav bar that stays narrow while the
// cards beneath it run wider reads as a rendering fault. Both widths are
// asserted from one request each, because the failure this catches is the
// two disagreeing — checking either alone would pass with the header
// hardcoded.
func TestHeaderWidthTracksThePageWidth(t *testing.T) {
	srv := newServer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, tc := range []struct {
		path  string
		width string
	}{
		{"/", "max-w-5xl"},       // home.html defines "width"
		{"/alerts", "max-w-3xl"}, // no override: the layout's default
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		header := `<div class="mx-auto flex ` + tc.width + ` flex-wrap items-center`
		if !strings.Contains(rec.Body.String(), header) {
			t.Errorf("%s: header is not %s; body:\n%s", tc.path, tc.width, rec.Body.String())
		}
	}
}

// The operator asked for the alert state in the browser tab — «Тривога |
// Сполох», «Спокійно | Сполох». The value is a dashboard that can be read off
// the tab strip while it sits behind other windows, so the state has to lead:
// a row of tabs all beginning «Сполох» answers nothing.
//
// Every page, not just the dashboard, and in both states. One page in one
// state would pass with the word hardcoded, which is exactly the failure this
// is here to catch.
func TestEveryPageTitleLeadsWithTheAlertState(t *testing.T) {
	calm := func(t *testing.T) *web.Server { t.Helper(); return newServer(t) }

	for _, tc := range []struct {
		name string
		srv  func(*testing.T) *web.Server
		path string
		want string
	}{
		{"calm dashboard", calm, "/", "<title>Спокійно | Сполох — "},
		{"alerting dashboard", newServerInAlert, "/", "<title>Тривога | Сполох — "},
		{"calm sources", calm, "/alerts", "<title>Спокійно | Джерела — Сполох</title>"},
		{"alerting sources", newServerInAlert, "/alerts", "<title>Тривога | Джерела — Сполох</title>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := authedBody(t, tc.srv(t), tc.path)
			if !strings.Contains(body, tc.want) {
				t.Errorf("%s does not carry %q in its tab title", tc.path, tc.want)
			}
		})
	}
}

// /setup and /login answer anyone who can reach the device, before any
// session exists. What the device is currently seeing, and exactly which
// build it runs, are answers for somebody who has logged in — so the bare
// shell carries neither. Checked against a device that is actually in the
// alert state, where a leak would show.
func TestUnauthenticatedPagesSayNothingAboutTheDeviceState(t *testing.T) {
	srv := newServerInAlert(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "<title>Налаштування — Сполох</title>") {
		t.Errorf("/setup does not carry its own bare title")
	}
	for _, word := range []string{"Тривога", "Спокійно"} {
		if strings.Contains(body, word) {
			t.Errorf("/setup tells an unauthenticated visitor the device's alert state (%q)", word)
		}
	}
	if strings.Contains(body, "<footer") {
		t.Error("/setup tells an unauthenticated visitor which build the device runs")
	}
}

// Which build is on the wall, on every authenticated page. The question is
// asked exactly when nobody can get a shell on the device to answer it —
// over the phone, by whoever is standing in front of it — so it is on the
// screen they are already looking at rather than on one page they have to
// find.
func TestEveryPageCarriesTheRunningVersion(t *testing.T) {
	for _, path := range []string{"/", "/alerts"} {
		body := authedBody(t, newServerAtVersion(t, "v1.4.2"), path)
		if !strings.Contains(body, "<footer") {
			t.Errorf("%s has no footer", path)
		}
		if !strings.Contains(body, "Версія: v1.4.2") {
			t.Errorf("%s does not print the running version", path)
		}
	}
}

// A build nobody stamped has to say so in words. A footer reading «Версія:»
// and then nothing looks like a rendering fault rather than an answer, and
// the operator reading it out over the phone needs to tell "there is no
// version" from "I cannot see one".
//
// Both spellings of unstamped: Deps.Version left empty, and the "dev" that
// cmd/spoloh's main.version carries when the binary is built without the
// Makefile's -ldflags.
func TestTheFooterSaysWhenTheBuildIsUnstamped(t *testing.T) {
	for _, version := range []string{"", "dev"} {
		body := authedBody(t, newServerAtVersion(t, version), "/")
		if !strings.Contains(body, "Версія: збірка для розробки") {
			t.Errorf("Deps.Version = %q: the footer does not say the build is unstamped", version)
		}
		if strings.Contains(body, "Версія: </footer>") {
			t.Errorf("Deps.Version = %q: the footer renders an empty version", version)
		}
	}
}

// A local `make build` stamps `git describe --tags --always --dirty`, which
// on a checkout with no tag is a bare commit hash. That is a real identity
// somebody can look up, not a placeholder, so it is printed as it stands
// rather than swallowed by the unstamped case above.
func TestTheFooterPrintsACommitHashVersionAsItIs(t *testing.T) {
	body := authedBody(t, newServerAtVersion(t, "2d8c163-dirty"), "/")
	if !strings.Contains(body, "Версія: 2d8c163-dirty") {
		t.Error("a development build's commit-hash version is not printed as it stands")
	}
}

// The footer follows the page's measure the same way the header does, so on
// the screens that widen it sits under the content instead of off to one side
// of it. Both widths from one request each, for the reason
// TestHeaderWidthTracksThePageWidth gives: either alone would pass with the
// footer hardcoded.
func TestFooterWidthTracksThePageWidth(t *testing.T) {
	for _, tc := range []struct {
		path  string
		width string
	}{
		{"/", "max-w-5xl"},       // home.html defines "width"
		{"/alerts", "max-w-3xl"}, // no override: the layout's default
	} {
		body := authedBody(t, newServer(t), tc.path)
		footer := `<footer class="mx-auto w-full ` + tc.width + ` p-4 text-sm text-muted-foreground">`
		if !strings.Contains(body, footer) {
			t.Errorf("%s: footer is not %s", tc.path, tc.width)
		}
	}
}

// Nine entries wrapped to two rows on a phone, and six of the nine were
// configuration. What stays at the top level is what an operator looks at
// while the device is working and what they reach for when it is not.
func TestNavIsFourEntriesAndKeepsNetworkAtTheTop(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	nav := body[strings.Index(body, `<nav aria-label="Розділи"`):]
	nav = nav[:strings.Index(nav, "</nav>")]

	for _, want := range []string{`href="/"`, `href="/log"`, `href="/network"`, `href="/settings"`} {
		if !strings.Contains(nav, want) {
			t.Errorf("the nav does not carry %s", want)
		}
	}
	// Six screens moved behind /settings. Left in the nav they made it wrap
	// to two rows on a phone.
	for _, gone := range []string{`href="/alerts"`, `href="/events"`, `href="/sounds"`, `href="/audio"`, `href="/webhooks"`, `href="/time"`} {
		if strings.Contains(nav, gone) {
			t.Errorf("the nav still carries %s; it belongs behind /settings", gone)
		}
	}
}

func TestTheMovedScreensAreStillReachableDirectly(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	// A bookmark, a link in a note, a URL somebody wrote down. Moving a
	// screen in the nav must not move the screen.
	for _, path := range []string{"/alerts", "/events", "/sounds", "/audio", "/webhooks", "/time"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
	}
}
