package web_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	srv, _ := newServerWithStore(t) // Deps.Network is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"SiteWiFi"}, "password": {"sitepass1"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection with no network manager wired up", rec.Code)
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
		"ssid": {"SiteWiFi"}, "password": {"sitepass1"},
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
// password, and nothing else.
func TestNetworkConnectSucceedsWithExactCredentials(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"SiteWiFi"}, "password": {"sitepass1"},
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
	if calls[0].SSID != "SiteWiFi" || calls[0].PSK != "sitepass1" {
		t.Fatalf("Connect called with SSID=%q PSK=%q, want the exact submitted values", calls[0].SSID, calls[0].PSK)
	}
}

// A failed Connect must show the manager's own error, not a generic one —
// nmcli's error text is the only diagnostic a headless board offers someone
// standing there with a phone.
func TestNetworkConnectFailureShowsTheManagersError(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	fake.ConnectErr = errors.New("Secrets flag is not valid")

	req := postForm("/network", url.Values{
		"ssid": {"SiteWiFi"}, "password": {"wrongpass"},
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
}

// The operator is about to lose the connection they are using; the response
// they land on after a successful submit must repeat the device's addresses
// rather than redirecting them somewhere they can no longer load.
func TestNetworkConnectSuccessShowsAddresses(t *testing.T) {
	srv, _, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/network", url.Values{
		"ssid": {"SiteWiFi"}, "password": {"sitepass1"},
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

// The site Wi-Fi password the operator types must never come back out of
// the server, in any form — this project has already had one finding about a
// query parameter echoed onto a page.
func TestNetworkConnectNeverEchoesThePassword(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.ConnectErr = errors.New("bad password")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	const secret = "SuperSecretSitePassword1"
	req := postForm("/network", url.Values{
		"ssid": {"SiteWiFi"}, "password": {secret},
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

// Joining the site's network is what drops the access point the operator
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

	form := url.Values{"ssid": {"SiteWiFi"}, "password": {"sp0lohTest42"}}
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

// The join is what takes the radio off the access point and onto the site's
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
		"ssid": {"SiteWiFi"}, "password": {"sitepass1"},
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
		"ssid": {"SiteWiFi"}, "password": {"wrong"},
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

// scanProbe scripts a Scan that always fails — with a genuine problem, or
// with the "signal: killed" shape exec.CommandContext gives a process
// killed by a parent context going away, which is what a browser leaving
// the page looks like from inside nmcli.
type scanProbe struct {
	netmgr.Fake
	err error
}

func (p *scanProbe) Scan(context.Context) ([]netmgr.Network, error) {
	return nil, p.err
}

// The same failure either way: a probe that failed the cached read and
// succeeded the forced one would let a test pass against a page that
// swallowed the error on the path it actually takes.
func (p *scanProbe) Rescan(context.Context) ([]netmgr.Network, error) {
	return nil, p.err
}

func newServerWithScanProbe(t *testing.T, probe *scanProbe) (*web.Server, *bytes.Buffer) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := web.New(web.Deps{Config: store, Network: probe, Logger: logger})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, &buf
}

// A scan cancelled because the browser left the page is an ordinary
// cancellation, not a fault: a real scan on the reference board takes 0.09s,
// so logging every cancellation at Warn would train whoever reads the
// journal to ignore the line that means an actual scan broke.
func TestNetworkScanCancelledByTheClientIsNotLoggedAsAFailure(t *testing.T) {
	srv, buf := newServerWithScanProbe(t, &scanProbe{err: errors.New("signal: killed")})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	// The browser already left the page: cancel the request's own context
	// before the handler ever runs, the same shape
	// TestNetworkConnectSurvivesTheClientDisconnecting uses for Connect.
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("an ordinary cancellation was logged as a failure: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "scan cancelled") {
		t.Errorf("the cancellation was not logged at all, which would make a real regression silent: %s", buf.String())
	}
}

// A scan that fails for a real reason — not because the request context was
// already gone — must still reach the journal at Warn: this is the case the
// distinction above must not accidentally silence.
func TestNetworkScanGenuineFailureIsStillLoggedAsAFailure(t *testing.T) {
	srv, buf := newServerWithScanProbe(t, &scanProbe{err: errors.New("nmcli: no such device")})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "scan failed") {
		t.Errorf("a genuine scan failure was not logged at Warn: %s", buf.String())
	}
}

// getNetworkPage logs an operator in and returns the body of GET /network.
func getNetworkPage(t *testing.T, srv *web.Server) string {
	t.Helper()
	cookie := login(t, srv, "correct horse battery staple")
	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// «не показується поточна мережа в налаштуваннях» — the operator's report
// that started this task. /network must say what it is joined to right now,
// above the scan list. These six cases are every state netmgr.Status can
// report from a real board: joined over Wi-Fi, joined wired (no SSID),
// hotspot up while also joined (not mutually exclusive), neither, a failed
// lookup, and no manager wired up at all.
func TestNetworkShowsCurrentSSIDWhenJoinedOverWifi(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.CurrentStatus = netmgr.Status{Connected: true, SSID: "SiteWiFi"}
	setPassword(t, srv, "correct horse battery staple")

	body := getNetworkPage(t, srv)
	if !strings.Contains(body, "SiteWiFi") {
		t.Error("the page does not show the SSID the device is joined to")
	}
	if strings.Contains(body, "Не підключено") {
		t.Error("the page claims not connected while joined over Wi-Fi")
	}
}

// Connected true with an empty SSID is a wired upstream — a board on a wall
// port in a PA closet. Telling that operator "not connected" sends them
// chasing a fault that does not exist.
func TestNetworkShowsWiredWhenConnectedWithNoSSID(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.CurrentStatus = netmgr.Status{Connected: true, SSID: ""}
	setPassword(t, srv, "correct horse battery staple")

	body := getNetworkPage(t, srv)
	if !strings.Contains(body, "Дротове підключення") {
		t.Error("the page does not report a wired connection for Connected=true, SSID=\"\"")
	}
	if strings.Contains(body, "Не підключено") {
		t.Error("the page claims not connected for a wired upstream")
	}
}

// HotspotUp is independent of Connected: a board with a wired upstream can
// report both, and the page must say both at once rather than picking one.
func TestNetworkShowsBothWiredAndHotspotWhenBothAreTrue(t *testing.T) {
	srv, store, fake := newServerWithNetwork(t)
	fake.CurrentStatus = netmgr.Status{Connected: true, SSID: "", HotspotUp: true}
	setPassword(t, srv, "correct horse battery staple")
	if err := store.Update(func(c *config.Config) error {
		c.Network.APSSID = "Spoloh-Setup"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	body := getNetworkPage(t, srv)
	if !strings.Contains(body, "Дротове підключення") {
		t.Error("the page does not report the wired upstream")
	}
	if !strings.Contains(body, "Роздає власну точку доступу") || !strings.Contains(body, "Spoloh-Setup") {
		t.Error("the page does not report the hotspot it is still serving")
	}
}

// Genuinely neither joined nor hotspotting: this is the only case that may
// say «Не підключено».
func TestNetworkShowsNotConnectedWhenNeitherJoinedNorHotspotting(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.CurrentStatus = netmgr.Status{}
	setPassword(t, srv, "correct horse battery staple")

	body := getNetworkPage(t, srv)
	if !strings.Contains(body, "Не підключено") {
		t.Error("the page does not report the device as not connected")
	}
}

// A failed Status lookup must say so in Ukrainian, and must not render the
// misleading "Не підключено" for a status that is actually unknown.
func TestNetworkShowsNoticeWhenStatusFails(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	fake.StatusErr = errors.New("nmcli: device or resource busy")
	setPassword(t, srv, "correct horse battery staple")

	body := getNetworkPage(t, srv)
	if !strings.Contains(body, "Не вдалося визначити поточне підключення") {
		t.Error("the page does not explain the failed status lookup")
	}
	if strings.Contains(body, "Не підключено") {
		t.Error("a failed status lookup was rendered as a confirmed disconnection")
	}
}

// statusDeadlineProbe records whether the context Status was called with
// carried a deadline, without actually blocking until one fires — the same
// approach TestNetworkConnectSurvivesTheClientDisconnecting takes for the
// connect timeout, and for the same reason: a test must never sleep on a
// real clock, so this proves the call is bounded rather than waiting the
// full 15s out on a wedged manager.
type statusDeadlineProbe struct {
	netmgr.Fake
	hadDeadline bool
}

func (p *statusDeadlineProbe) Status(ctx context.Context) (netmgr.Status, error) {
	_, p.hadDeadline = ctx.Deadline()
	return p.Fake.Status(ctx)
}

// A wedged NetworkManager must not turn a page load into a hang: Status is
// called with a context carrying a deadline, the same shape the scan on this
// page already uses.
func TestNetworkStatusCallIsDeadlineBounded(t *testing.T) {
	probe := &statusDeadlineProbe{}
	srv, _ := newServerWithManager(t, probe, &netmgr.JoinGate{})
	setPassword(t, srv, "correct horse battery staple")

	getNetworkPage(t, srv)
	if !probe.hadDeadline {
		t.Error("Status was called with no deadline; a wedged NetworkManager would hang the page load forever")
	}
}

// The page tolerates a missing network manager for the status section the
// same way it already does for the scan and the connect form — Deps.Network
// is nil on a fresh production board before Task 5 wires it up.
func TestNetworkStatusSectionRendersWithNilNetworkManager(t *testing.T) {
	srv, _ := newServerWithStore(t) // Deps.Network is nil
	setPassword(t, srv, "correct horse battery staple")

	body := getNetworkPage(t, srv)
	if !strings.Contains(body, "Мережевий модуль недоступний на цьому пристрої") {
		t.Error("the page does not explain the missing network manager in the status section")
	}
	if !strings.Contains(body, "<form") {
		t.Error("a nil network manager took the join form down with it")
	}
}

// The ordinary page load answers from NetworkManager's cache, and the button
// exists to ask the radio to sweep instead. A button that read the same cache
// would show the same list and teach an operator standing next to a network
// that it is not there — which is the conclusion this screen handed them
// before the button existed.
func TestNetworkRefreshAsksForAFreshSweep(t *testing.T) {
	srv, _, fake := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	get := func(path string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	get("/network")
	get("/network?rescan=1")

	var scans, rescans int
	for _, c := range fake.Calls() {
		switch c.Method {
		case "Scan":
			scans++
		case "Rescan":
			rescans++
		}
	}
	if scans != 1 {
		t.Errorf("%d cached scans, want 1: an ordinary load must not spend seconds sweeping", scans)
	}
	if rescans != 1 {
		t.Errorf("%d forced sweeps, want 1: the refresh button read the cache instead", rescans)
	}
}

// The button has to be on the page for any of that to matter, and it has to
// be a link — this screen is reached when nothing else about the device works
// and every control on it must survive JavaScript being off.
func TestNetworkOffersARefreshLink(t *testing.T) {
	srv, _, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="/network?rescan=1"`) {
		t.Errorf("no refresh link on the page; body:\n%s", body)
	}
}

// Basecoat's .btn does not clear the underline an anchor gets, so a link
// styled as a button renders as underlined text in a box — which is what the
// operator saw. Every anchor-button on this screen carries the reset.
func TestAnchorButtonsAreNotUnderlined(t *testing.T) {
	srv, _, _ := newServerWithNetwork(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/network", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	// Every <a> carrying the button class, exactly as rendered.
	for _, chunk := range strings.Split(body, `<a class="btn`)[1:] {
		attrs := chunk[:strings.Index(chunk, ">")]
		if !strings.HasPrefix(attrs, " no-underline") {
			t.Errorf("an anchor-button is missing the underline reset; attributes: %s", attrs)
		}
	}
}
