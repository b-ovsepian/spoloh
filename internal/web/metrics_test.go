package web_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/web"
)

const metricsToken = "zabbix-test-token-0123456789abcdefghij"

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type metricsRig struct {
	srv   *web.Server
	store *configstore.Store
	log   *eventlog.Log
	pl    *player.Player
	feed  *netmgr.StatusFeed
	act   *netmgr.Activity
}

func newMetricsRig(t *testing.T, withToken bool) *metricsRig {
	t.Helper()
	dir := t.TempDir()
	store, err := configstore.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if withToken {
		if err := store.Update(func(c *config.Config) error { c.Monitoring.TokenHash = tokenHash(metricsToken); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	lg, err := eventlog.Open(filepath.Join(dir, "spoloh.db"), 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	pl := player.New(newFakePlaybackBackend(), "default")
	feed := &netmgr.StatusFeed{}
	act := &netmgr.Activity{}
	eng := engine.New(engine.Deps{
		Config:  store.Get,
		Machine: alertstate.New(),
		Player:  pl,
		Log:     lg,
		Clock:   clock.System{Loc: time.UTC},
		Sync:    &clock.FakeSync{Value: true},
	})
	srv, err := web.New(web.Deps{
		Config: store, Engine: eng, Log: lg, Player: pl,
		NetStatus: feed, Activity: act, Version: "v9.9.9-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &metricsRig{srv: srv, store: store, log: lg, pl: pl, feed: feed, act: act}
}

func (r *metricsRig) get(token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// No token generated is no endpoint: a device nobody asked to be monitored
// does not answer questions about itself.
func TestMetricsIsOffUntilATokenIsMade(t *testing.T) {
	r := newMetricsRig(t, false)
	if rec := r.get(metricsToken); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d with no token configured, want 404", rec.Code)
	}
}

// A monitoring system holds no session, so /metrics is outside the login —
// and behind its own token instead.
func TestMetricsRefusesAMissingOrWrongToken(t *testing.T) {
	r := newMetricsRig(t, true)
	for name, token := range map[string]string{"none": "", "wrong": "not-the-token"} {
		rec := r.get(token)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: no Bearer challenge", name)
		}
		if strings.Contains(rec.Body.String(), "spoloh_") {
			t.Errorf("%s: the refusal carries metrics", name)
		}
	}
}

// Every line is either a comment or a sample in the Prometheus text format:
// one malformed line and a scraper drops the whole page, which is a device
// that looks down to its monitoring when it is not.
var sampleLine = regexp.MustCompile(`^[a-z_]+(\{[a-z_]+="([^"\\\n]|\\.)*"(,[a-z_]+="([^"\\\n]|\\.)*")*\})? -?[0-9]+(\.[0-9]+)?$`)

func TestMetricsIsWellFormedAndComplete(t *testing.T) {
	r := newMetricsRig(t, true)
	rec := r.get(metricsToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; body:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	// Each metric declared once, before its samples: a scraper that meets a
	// sample with no TYPE, or a TYPE twice, may drop the page.
	typed := map[string]bool{}
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name := strings.Fields(rest)[0]
			if typed[name] {
				t.Errorf("line %d declares %s a second time", i+1, name)
			}
			typed[name] = true
			continue
		}
		if !sampleLine.MatchString(line) {
			t.Errorf("line %d is not a sample: %q", i+1, line)
			continue
		}
		if name := line[:strings.IndexAny(line, "{ ")]; !typed[name] {
			t.Errorf("line %d is a sample of %s before its TYPE", i+1, name)
		}
	}
	for _, name := range []string{
		`spoloh_build_info{version="v9.9.9-test"} 1`,
		"spoloh_start_time_seconds ",
		`spoloh_alert_active{region="м. Київ"} 0`,
		"spoloh_alert_red 0",
		"spoloh_alert_since_timestamp_seconds 0",
		"spoloh_poll_last_success_timestamp_seconds 0",
		"spoloh_poll_failing 0",
		"spoloh_poll_paused 0",
		"spoloh_clock_synced 1",
		"spoloh_playing 0",
		"spoloh_alert_start_disabled 0",
		"spoloh_quiet_hours_silence_alerts 0",
		"spoloh_radio_enabled 0",
		"spoloh_radio_on 0",
		"spoloh_radio_held 0",
		"spoloh_radio_last_error_timestamp_seconds 0",
		"spoloh_eventlog_write_errors_total 0",
		"# TYPE spoloh_events_total counter",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("missing %q", name)
		}
	}
	// No reading from the supervisor yet: the network is not reported at
	// all rather than reported as down.
	if strings.Contains(body, "spoloh_network_") {
		t.Errorf("network metrics with no reading to report:\n%s", body)
	}
}

// The numbers follow the device: what the operator configured, what the log
// counted, what the radio and the network are doing.
func TestMetricsFollowTheDevice(t *testing.T) {
	r := newMetricsRig(t, true)
	if err := r.store.Update(func(c *config.Config) error {
		for i := range c.Events {
			if c.Events[i].Trigger == config.TriggerAlertStart {
				c.Events[i].Enabled = false
			}
		}
		c.QuietHours = config.QuietHours{Enabled: true, From: "22:00", To: "07:00", AppliesTo: []string{config.CategoryAlerts}}
		c.Radio.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = r.log.Append(eventlog.Entry{Kind: eventlog.KindPlaybackError, OK: false})
	r.pl.SetBackground(player.Background{URL: "https://online.hitfm.ua/HitFM", Volume: 80, Gain: 50})
	r.pl.Stop()
	r.feed.Publish(netmgr.Status{HotspotUp: true}, time.Now())

	body := r.get(metricsToken).Body.String()
	for _, want := range []string{
		"spoloh_alert_start_disabled 1",
		"spoloh_quiet_hours_silence_alerts 1",
		`spoloh_events_total{kind="playback_error",ok="false"} 1`,
		"spoloh_radio_enabled 1",
		"spoloh_radio_on 1",
		"spoloh_radio_held 1",
		"spoloh_network_connected 0",
		"spoloh_network_hotspot_up 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// A region is the operator's text, and a quote or a backslash in it must not
// break the line it sits in.
func TestMetricsEscapesLabelValues(t *testing.T) {
	r := newMetricsRig(t, true)
	if err := r.store.Update(func(c *config.Config) error { c.Alerts.Region = `a "b" \c`; return nil }); err != nil {
		t.Fatal(err)
	}
	body := r.get(metricsToken).Body.String()
	if !strings.Contains(body, `spoloh_alert_active{region="a \"b\" \\c"} 0`) {
		t.Fatalf("region not escaped:\n%s", body)
	}
}

// A scrape is a machine, not someone using the access point: counting it as
// dashboard activity would let a monitoring system hold the device on its
// access point and off the network it remembers.
func TestAScrapeIsNotDashboardActivity(t *testing.T) {
	r := newMetricsRig(t, true)
	r.get(metricsToken)
	if r.act.Within(time.Now(), time.Hour) {
		t.Fatal("a scrape of /metrics was recorded as someone using the dashboard")
	}
}

// monitoringPost submits the form as a browser showing the current page
// would, revision included.
func monitoringPost(t *testing.T, h http.Handler, cookie *http.Cookie, action, origin string) *httptest.ResponseRecorder {
	t.Helper()
	page := httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/monitoring", nil)
	get.AddCookie(cookie)
	h.ServeHTTP(page, get)
	rev := ""
	if m := regexp.MustCompile(`name="rev" value="([^"]*)"`).FindStringSubmatch(page.Body.String()); m != nil {
		rev = m[1]
	}
	form := url.Values{"action": {action}, "rev": {rev}}
	req := httptest.NewRequest(http.MethodPost, "/monitoring", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var shownToken = regexp.MustCompile(`data-token="([A-Za-z0-9_-]{43})"`)

// The token is shown once, as it is made, and only its hash is kept: the
// next look at the page, a backup or a leak of config.json gives nobody a
// token to scrape with.
func TestATokenIsShownOnceAndOnlyItsHashIsKept(t *testing.T) {
	r := newMetricsRig(t, false)
	setPassword(t, r.srv, "correct horse battery staple")
	cookie := login(t, r.srv, "correct horse battery staple")
	h := r.srv.Handler()

	rec := monitoringPost(t, h, cookie, "generate", "http://example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: status %d", rec.Code)
	}
	m := shownToken.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("the new token is not on the page:\n%s", rec.Body.String())
	}
	token := m[1]
	if got := r.store.Get().Monitoring.TokenHash; got != tokenHash(token) {
		t.Fatalf("stored %q, want the SHA-256 of the token shown", got)
	}
	if r.get(token).Code != http.StatusOK {
		t.Fatal("the token the page showed does not open /metrics")
	}

	req := httptest.NewRequest(http.MethodGet, "/monitoring", nil)
	req.AddCookie(cookie)
	again := httptest.NewRecorder()
	h.ServeHTTP(again, req)
	if strings.Contains(again.Body.String(), token) {
		t.Fatal("the token is shown again on a later visit")
	}
	if !strings.Contains(again.Body.String(), "увімкнено") {
		t.Errorf("the page does not say monitoring is on:\n%s", again.Body.String())
	}

	if rec := monitoringPost(t, h, cookie, "disable", "http://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("disable: status %d", rec.Code)
	}
	if r.get(token).Code != http.StatusNotFound {
		t.Fatal("/metrics still answers after monitoring was switched off")
	}
}

// An empty form, or a form from elsewhere, changes nothing.
func TestMonitoringChangesOnlyOnAnExplicitSameOriginAction(t *testing.T) {
	r := newMetricsRig(t, false)
	setPassword(t, r.srv, "correct horse battery staple")
	cookie := login(t, r.srv, "correct horse battery staple")
	h := r.srv.Handler()

	if rec := monitoringPost(t, h, cookie, "generate", "http://elsewhere.example"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin generate: status %d, want 403", rec.Code)
	}
	monitoringPost(t, h, cookie, "", "http://example.com")
	if got := r.store.Get().Monitoring.TokenHash; got != "" {
		t.Fatalf("a token was made without being asked for: %q", got)
	}
}

// Never answered since the daemon started is silent for as long as it has
// been running — not -1, which a monitoring system would raise as "silent" on
// the first scrape after every reboot, before any poll had a chance to run.
func TestPollAgeBeforeAnyAnswerCountsFromTheStart(t *testing.T) {
	r := newMetricsRig(t, true)
	r.srv.SetNow(func() time.Time { return time.Now().Add(90 * time.Second) })
	body := r.get(metricsToken).Body.String()
	m := regexp.MustCompile(`(?m)^spoloh_poll_last_success_age_seconds (\d+)$`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no poll age:\n%s", body)
	}
	if m[1] < "89" || m[1] > "91" || len(m[1]) != 2 {
		t.Fatalf("poll age = %s with nothing answered 90s after start, want 90", m[1])
	}
}

// The scheme name is case-insensitive, and a pasted token often carries a
// trailing space: neither is a reason to refuse a monitoring system.
func TestMetricsAcceptsTheSchemeInAnyCaseAndAStraySpace(t *testing.T) {
	r := newMetricsRig(t, true)
	for _, h := range []string{"bearer " + metricsToken, "BEARER " + metricsToken + " "} {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Authorization %q: status %d, want 200", h, rec.Code)
		}
	}
}

// The page that shows a token must not be kept by the browser, and sending
// it again — F5, back and forward — must not quietly replace the token the
// operator just copied into Zabbix, which would go dark five minutes later.
func TestResendingTheTokenPageDoesNotReplaceTheToken(t *testing.T) {
	r := newMetricsRig(t, false)
	setPassword(t, r.srv, "correct horse battery staple")
	cookie := login(t, r.srv, "correct horse battery staple")
	h := r.srv.Handler()

	page := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/monitoring", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(page, req)
	rev := regexp.MustCompile(`name="rev" value="([^"]*)"`).FindStringSubmatch(page.Body.String())
	if rev == nil {
		t.Fatalf("the form carries no revision:\n%s", page.Body.String())
	}

	post := func() *httptest.ResponseRecorder {
		form := url.Values{"action": {"generate"}, "rev": {rev[1]}}
		req := httptest.NewRequest(http.MethodPost, "/monitoring", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.com")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := post()
	if cc := first.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("the page showing a token has Cache-Control %q, want no-store", cc)
	}
	made := r.store.Get().Monitoring.TokenHash
	if made == "" {
		t.Fatal("the first generate made no token")
	}
	again := post()
	if got := r.store.Get().Monitoring.TokenHash; got != made {
		t.Fatal("sending the same form again replaced the token")
	}
	if shownToken.MatchString(again.Body.String()) {
		t.Error("a resent form showed a token")
	}
}
