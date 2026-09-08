package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"path/filepath"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
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

// connectProbe records the state of the context Connect was handed. It embeds
// no Fake because only Connect matters here.
type connectProbe struct {
	netmgr.Fake
	sawCancelled bool
	hadDeadline  bool
	called       bool
}

func (p *connectProbe) Connect(ctx context.Context, ssid, psk string) error {
	p.called = true
	p.sawCancelled = ctx.Err() != nil
	_, p.hadDeadline = ctx.Deadline()
	return nil
}

// Joining the school network is what drops the access point the operator
// submitted this form over, so their browser losing the connection is the
// expected outcome of a successful join. Go cancels a request's context the
// moment the client goes away and nmcli runs under exec.CommandContext, so a
// join carrying r.Context() would kill itself at exactly the moment it began
// to work — and leave the device on neither network.
func TestNetworkConnectSurvivesTheClientDisconnecting(t *testing.T) {
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	probe := &connectProbe{}
	srv, err := web.New(web.Deps{Config: store, Network: probe})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	form := url.Values{"ssid": {"ShkolaWiFi"}, "password": {"sp0lohTest42"}, "region": {alertsource.Regions()[0]}}
	req := httptest.NewRequest(http.MethodPost, "/network", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)

	// The phone lost the access point before the handler reached Connect.
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if !probe.called {
		t.Fatal("Connect was never attempted for a request whose client had gone away")
	}
	if probe.sawCancelled {
		t.Error("Connect ran on an already-cancelled context; the join would be killed the moment it dropped the access point")
	}
	if !probe.hadDeadline {
		t.Error("Connect ran with no deadline; a wedged NetworkManager would hold this goroutine forever")
	}
}

// There is no settings screen (Plan 2b has not shipped): /network is the
// only place Alerts.Region is ever written anywhere in this codebase. If
// Connect succeeds but the config write that follows it does not, the
// success page must say so distinctly — otherwise the operator is told the
// device is connected while the one value it needs to do its job was never
// saved, with no reason to think they need to come back.
func TestNetworkConnectSuccessWarnsWhenTheRegionCouldNotBeSaved(t *testing.T) {
	dir := t.TempDir()
	store, err := configstore.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	fake := &netmgr.Fake{}
	srv, err := web.New(web.Deps{Config: store, Network: fake})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Region

	// config.Save writes atomically through a temp file in the same
	// directory (see internal/atomicfile), so a directory the process
	// cannot write to is what makes Update fail deterministically here — the
	// same trick internal/configstore's own tests use for a read-only SD
	// card. This runs after setup/login, which already had to write
	// successfully to get this far.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"Чернігівська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the join itself succeeded", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Пристрій підключено") {
		t.Fatal("the page does not show the success view for a join that actually succeeded")
	}
	if !strings.Contains(body, "не збереглася") {
		t.Error("the success page does not warn that the region failed to save")
	}
	if got := store.Get().Alerts.Region; got != before {
		t.Fatalf("stored region = %q, want unchanged %q — the write genuinely failed", got, before)
	}
}

// The warning above is for the one case where the config write fails after a
// successful join; an ordinary success must not carry it, or the operator
// would not be able to tell the two apart.
func TestNetworkConnectPlainSuccessCarriesNoRegionWarning(t *testing.T) {
	srv, _, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"Чернігівська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "не збереглася") {
		t.Error("a plain, fully successful connect carries the region-not-saved warning")
	}
}

// gateProbe is a netmgr.Manager that answers the one question this test has:
// was the join gate held at the moment Connect was actually running? The
// supervisor's whole reason for consulting the gate is that window and no
// other, so observing it from inside Connect is the only assertion that means
// anything.
type gateProbe struct {
	*netmgr.Fake
	gate *netmgr.JoinGate
	held bool
}

func (p *gateProbe) Connect(ctx context.Context, ssid, psk string) error {
	p.held = p.gate.Held(time.Now())
	return p.Fake.Connect(ctx, ssid, psk)
}

// newServerWithManager wires an arbitrary netmgr.Manager and join gate in,
// for the tests that need to observe the manager from the inside rather than
// script it from the outside.
func newServerWithManager(t *testing.T, mgr netmgr.Manager, gate *netmgr.JoinGate) (*web.Server, *configstore.Store) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store, Network: mgr, JoinGate: gate})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store
}

// The join is what takes the radio off the access point and onto the school
// network, and for the tens of seconds that takes the device looks to the
// supervisor like a device with no network at all. Unless this handler holds
// the gate for the length of the join, the supervisor raises an access point
// on top of it.
func TestNetworkConnectHoldsTheJoinGateForTheJoin(t *testing.T) {
	gate := &netmgr.JoinGate{}
	probe := &gateProbe{Fake: &netmgr.Fake{}, gate: gate}
	srv, _ := newServerWithManager(t, probe, gate)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if gate.Held(time.Now()) {
		t.Fatal("setup: the gate is held before any join was started")
	}

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"schoolpass1"}, "region": {"Львівська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !probe.held {
		t.Error("the join gate was not held while Connect was running; the supervisor is free to raise an access point mid-join")
	}
	if gate.Held(time.Now()) {
		t.Error("the join gate is still held after the join returned; it must be released on the way out, not left to expire")
	}
}

// A join that fails releases the gate too. Leaving it held would keep the
// supervisor from raising the access point that is the operator's way back
// after exactly the failure that needs one.
func TestNetworkConnectReleasesTheJoinGateOnFailure(t *testing.T) {
	gate := &netmgr.JoinGate{}
	probe := &gateProbe{Fake: &netmgr.Fake{ConnectErr: errors.New("bad password")}, gate: gate}
	srv, _ := newServerWithManager(t, probe, gate)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"ShkolaWiFi"}, "password": {"wrong"}, "region": {"Львівська область"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a failed join", rec.Code)
	}
	if !probe.held {
		t.Error("the join gate was not held while Connect was running")
	}
	if gate.Held(time.Now()) {
		t.Error("a failed join left the join gate held")
	}
}
