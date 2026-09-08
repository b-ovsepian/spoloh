package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// The device's own access point is reachable to anyone within radio range, so
// this page must not be reachable without a session like every other private
// route — a public /network would let a stranger redirect the device onto
// their own network.
func TestNetworkRequiresSession(t *testing.T) {
	srv, _, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/network", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /network with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

// Strongest signal first, and a hidden network (an empty SSID, which nmcli
// itself can report for one in range) never becomes a row: it cannot be
// chosen from a list, so rendering it blank would only confuse the operator.
func TestNetworkListsScannedNetworksStrongestFirstAndHidesHidden(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.Networks = []netmgr.Network{
		{SSID: "WeakNet", Signal: 20},
		{SSID: "", Signal: 95}, // hidden: strongest of all, but must not render
		{SSID: "StrongNet", Signal: 80},
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	strong := strings.Index(body, ">StrongNet<")
	weak := strings.Index(body, ">WeakNet<")
	if strong < 0 || weak < 0 {
		t.Fatalf("StrongNet at %d, WeakNet at %d: both scanned networks must be on the page", strong, weak)
	}
	if strong > weak {
		t.Error("the weaker network is listed before the stronger one")
	}
	if strings.Contains(body, `value=""`) {
		t.Error("the hidden network (empty SSID) got a row on the page")
	}
}

// An operator who lost the installer's printed output has nowhere else to
// find the access point's own password; this page displays it deliberately,
// behind the session this test logs in for.
func TestNetworkShowsAccessPointPassword(t *testing.T) {
	srv, store, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	if err := store.Update(func(c *config.Config) error {
		c.Network.APPassword = "hotspot-secret-1"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, ">Spoloh-Setup<") {
		t.Error("the page does not show the access point's SSID")
	}
	if !strings.Contains(body, ">hotspot-secret-1<") {
		t.Error("the page does not show the access point's own password")
	}
}

// The page tolerates a missing network manager the way every other page
// tolerates a missing dependency — Task 5 is what wires Deps.Network up, so
// production runs with it nil until then.
func TestNetworkPageRendersWithNilNetworkManager(t *testing.T) {
	srv, _ := newServerWithStore(t) // Deps.Network is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no network manager", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<form") {
		t.Error("the page rendered no form at all with a nil network manager")
	}
}

// A submission against a device that has no network manager wired up yet
// (production, before Task 5) must be rejected cleanly, not panic on a nil
// interface — the same tolerance every other page's optional dependency
// gets.
func TestNetworkConnectWithNilNetworkManagerDoesNotPanic(t *testing.T) {
	srv, store := newServerWithStore(t) // Deps.Network is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Region

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"м. Київ"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection with no network manager wired up", rec.Code)
	}
	if got := store.Get().Alerts.Region; got != before {
		t.Fatalf("stored region = %q, want unchanged %q", got, before)
	}
}

// These two routes need no cookie at all — the whole reason CSRF matters
// here, same as /setup and /login — so a blind cross-site form POST must be
// rejected using the project's one sameOrigin helper.
func TestNetworkConnectRejectsCrossOrigin(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"м. Київ"},
	})
	req.AddCookie(cookie)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("a cross-site /network submission was accepted")
	}
	if len(fake.Calls()) != 0 {
		t.Fatal("a cross-site submission still reached the network manager")
	}
}

// A successful connect must call Connect with exactly the submitted SSID and
// password, and only then store the chosen oblast — never before, and never
// with anything else.
func TestNetworkConnectSucceedsAndStoresRegion(t *testing.T) {
	srv, store, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"Львівська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (this page never redirects the operator away)", rec.Code)
	}

	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Method != "Connect" {
		t.Fatalf("calls = %+v, want exactly one Connect call", calls)
	}
	if calls[0].SSID != "ShkolaWiFi" || calls[0].PSK != "schoolpass1" {
		t.Fatalf("Connect called with SSID=%q PSK=%q, want the exact submitted values", calls[0].SSID, calls[0].PSK)
	}
	if got := store.Get().Alerts.Region; got != "Львівська область" {
		t.Fatalf("stored region = %q, want the submitted oblast", got)
	}
}

// A half-applied submission is worse than a rejected one: if Connect fails,
// the page must show the manager's own error and the stored oblast must not
// move, even though a different, valid oblast was submitted alongside it.
func TestNetworkConnectFailureLeavesRegionUnchanged(t *testing.T) {
	srv, store, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Region

	fake.ConnectErr = errors.New("Secrets flag is not valid")

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"wrongpass"}, "region": {"Одеська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "Пристрій підключено") {
		t.Fatal("the page reports success for a Connect that failed")
	}
	if !strings.Contains(rec.Body.String(), "Secrets flag is not valid") {
		t.Error("the page does not show the manager's own error")
	}
	if got := store.Get().Alerts.Region; got != before {
		t.Fatalf("stored region changed to %q after a failed connect, want unchanged %q", got, before)
	}
}

// A crafted form could submit a region key that matches no known oblast,
// which would silence the device permanently while the dashboard still
// looked configured. This must be rejected before anything is stored, and
// before the operator's own Wi-Fi connection is even touched.
func TestNetworkConnectRejectsAnUnknownRegion(t *testing.T) {
	srv, store, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Region

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"Марсіанська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection for an unknown region", rec.Code)
	}
	if got := store.Get().Alerts.Region; got != before {
		t.Fatalf("stored region = %q, want unchanged %q", got, before)
	}
	// The rejected page re-scans to show a retryable list, so Scan is
	// expected here; Connect is not — the region is invalid before the
	// device is ever asked to join anything.
	for _, c := range fake.Calls() {
		if c.Method == "Connect" {
			t.Error("the device tried to connect before the region was even validated")
		}
	}
}

// The operator is about to lose the connection they are using; the response
// they land on after a successful submit must repeat the device's addresses
// rather than redirecting them somewhere they can no longer load.
func TestNetworkConnectSuccessShowsAddresses(t *testing.T) {
	srv, _, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"Харківська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, addr := range web.LocalAddresses() {
		if !strings.Contains(body, addr) {
			t.Errorf("the success page does not repeat address %q", addr)
		}
	}
}

// The school Wi-Fi password the operator types must never come back out of
// the server, in any form — this project has already had one finding about a
// query parameter echoed onto a page.
func TestNetworkConnectNeverEchoesThePassword(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.ConnectErr = errors.New("bad password")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	const secret = "SuperSecretSchoolPassword1"
	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {secret}, "region": {"Сумська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the submitted Wi-Fi password was echoed back onto the page")
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, secret) {
		t.Fatal("the submitted Wi-Fi password ended up in a redirect URL")
	}
}
