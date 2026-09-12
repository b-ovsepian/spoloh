package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/timedate"
	"github.com/b-ovsepian/spoloh/internal/web"
)

const timePassword = "correct horse battery staple"

// newServerWithTimedate wires a *timedate.Fake in as Deps.Timedate, so the
// /time screen is exercised against a scripted host rather than a real
// timedatectl — which no test here may run.
func newServerWithTimedate(t *testing.T, state timedate.State) (*web.Server, *configstore.Store, *timedate.Fake) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	fake := &timedate.Fake{
		CurrentState: state,
		ZoneList:     []string{"Africa/Abidjan", "Europe/Kyiv", "Europe/Warsaw", "UTC"},
	}
	srv, err := web.New(web.Deps{Config: store, Timedate: fake})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store, fake
}

func timeGet(t *testing.T, srv *web.Server, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/time", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func timePost(t *testing.T, srv *web.Server, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func timeSession(t *testing.T, srv *web.Server) *http.Cookie {
	t.Helper()
	setPassword(t, srv, timePassword)
	return login(t, srv, timePassword)
}

func TestTimePageRequiresSession(t *testing.T) {
	srv, _, _ := newServerWithTimedate(t, timedate.State{})
	setPassword(t, srv, timePassword)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/time", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /time with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

// The whole reason this screen exists is that an operator could not see, let
// alone change, what the device thinks the time is.
func TestTimePageShowsTheClockStateAndTheZonePicker(t *testing.T) {
	srv, _, _ := newServerWithTimedate(t, timedate.State{
		Timezone: "Europe/Kyiv", CanNTP: true, NTP: true, NTPSynchronized: true,
	})
	cookie := timeSession(t, srv)

	body := timeGet(t, srv, cookie).Body.String()
	for _, want := range []string{
		"Зараз на пристрої",
		"Europe/Kyiv",
		"Синхронізація часу",
		`action="/time/timezone"`,
		`action="/time/ntp"`,
		`action="/time/server"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not carry %q; body:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `<option value="Africa/Abidjan"`) {
		t.Errorf("the picker does not offer the host's own zone list; body:\n%s", body)
	}
}

// Ukraine is the overwhelmingly common answer, so Europe/Kyiv leads the
// picker rather than sitting alphabetically between Europe/Kirov and
// Europe/Lisbon in a list of six hundred. Asserted on a device configured
// for some other zone, because that is the only case the rule is about: on a
// device already set to Europe/Kyiv the configured zone leads the list
// anyway and the rule would be invisible.
func TestTheZonePickerOffersKyivFirstEvenWhenItIsNotConfigured(t *testing.T) {
	srv, store, _ := newServerWithTimedate(t, timedate.State{Timezone: "UTC", CanNTP: true})
	if err := store.Update(func(c *config.Config) error {
		c.Device.Timezone = "UTC"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	cookie := timeSession(t, srv)

	body := timeGet(t, srv, cookie).Body.String()
	kyiv := strings.Index(body, `<option value="Europe/Kyiv"`)
	abidjan := strings.Index(body, `<option value="Africa/Abidjan"`)
	if kyiv < 0 || abidjan < 0 {
		t.Fatalf("the picker is missing one of the two zones compared; body:\n%s", body)
	}
	if kyiv > abidjan {
		t.Errorf("Europe/Kyiv is offered after Africa/Abidjan; it must lead the picker; body:\n%s", body)
	}
}

// The settings index is how the screen is reached at all now that it is no
// longer in the nav; a page nothing links to is a page an operator finds only
// by guessing the URL.
func TestTimePageIsReachableFromTheSettingsIndex(t *testing.T) {
	srv, _, _ := newServerWithTimedate(t, timedate.State{})
	cookie := timeSession(t, srv)

	if row := settingsRow(t, settingsIndex(t, srv, cookie), "/time"); !strings.Contains(row, "Час") {
		t.Errorf("the settings index has no entry for /time; row:\n%s", row)
	}
}

// The two halves of a time zone are the system's own setting and
// config.Device.Timezone, which is what the dashboard and the scheduler
// format with. A change that lands in one and not the other is the drift
// this screen exists to end.
func TestSavingATimezoneWritesBothTheSystemAndTheConfig(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "UTC", CanNTP: true})
	cookie := timeSession(t, srv)

	rec := timePost(t, srv, cookie, "/time/timezone", url.Values{"timezone": {"Europe/Warsaw"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Device.Timezone; got != "Europe/Warsaw" {
		t.Errorf("config timezone = %q, want Europe/Warsaw", got)
	}
	var applied string
	for _, c := range fake.Calls() {
		if c.Method == "SetTimezone" {
			applied = c.Arg
		}
	}
	if applied != "Europe/Warsaw" {
		t.Errorf("the system timezone was set to %q, want Europe/Warsaw", applied)
	}
}

// If either half fails, neither may be left applied. The half that will
// actually fail on a real board is the privileged one — it goes through
// polkit, and that rule is unverified on hardware — so the configuration
// has to be put back rather than left describing a zone the system is not
// in.
func TestAFailedSystemTimezoneLeavesTheConfigUnchanged(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	fake.SetTimezoneErr = errors.New("Failed to set time zone: Access denied")
	cookie := timeSession(t, srv)

	before := store.Get().Device.Timezone

	rec := timePost(t, srv, cookie, "/time/timezone", url.Values{"timezone": {"Europe/Warsaw"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Device.Timezone; got != before {
		t.Errorf("config timezone = %q after the system half failed, want it rolled back to %q", got, before)
	}
	if !strings.Contains(rec.Body.String(), "Нічого не змінено") {
		t.Errorf("the failure does not tell the operator that nothing was applied; body:\n%s", rec.Body.String())
	}
}

// The other half failing is the mirror case: config.Update validates the
// zone through time.LoadLocation, so a zone this build cannot resolve must
// be refused before anything privileged is attempted at all.
func TestAnUnknownTimezoneNeverReachesTheSystem(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	cookie := timeSession(t, srv)

	rec := timePost(t, srv, cookie, "/time/timezone", url.Values{"timezone": {"Mars/Olympus"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Device.Timezone; got != "Europe/Kyiv" {
		t.Errorf("config timezone = %q, want it untouched", got)
	}
	for _, c := range fake.Calls() {
		if c.Method == "SetTimezone" {
			t.Errorf("an unresolvable zone %q was handed to the system anyway", c.Arg)
		}
	}
}

func TestSwitchingNTPOffAndOn(t *testing.T) {
	srv, _, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true, NTP: true})
	cookie := timeSession(t, srv)

	if rec := timePost(t, srv, cookie, "/time/ntp", url.Values{"ntp": {"off"}}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if fake.CurrentState.NTP {
		t.Error("NTP is still on after being switched off")
	}
	if rec := timePost(t, srv, cookie, "/time/ntp", url.Values{"ntp": {"on"}}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !fake.CurrentState.NTP {
		t.Error("NTP is still off after being switched on")
	}
}

// A form that does not say which way to go must not switch the setting that
// decides whether the board keeps its own idea of the time.
func TestAnEmptyNTPFormChangesNothing(t *testing.T) {
	srv, _, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true, NTP: true})
	cookie := timeSession(t, srv)

	rec := timePost(t, srv, cookie, "/time/ntp", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	for _, c := range fake.Calls() {
		if c.Method == "SetNTP" {
			t.Error("an empty form switched NTP anyway")
		}
	}
}

// systemd refuses set-time while NTP is on. The screen must say that itself
// rather than offering a field whose submission the system throws back in
// English.
func TestTheManualClockIsRefusedAndHiddenWhileNTPIsOn(t *testing.T) {
	srv, _, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true, NTP: true})
	cookie := timeSession(t, srv)

	body := timeGet(t, srv, cookie).Body.String()
	if strings.Contains(body, `name="manual_time"`) {
		t.Errorf("the by-hand clock field is offered while NTP is on; body:\n%s", body)
	}
	if !strings.Contains(body, "Доступно лише тоді, коли синхронізацію часу вимкнено") {
		t.Errorf("the page does not say why the clock cannot be set by hand; body:\n%s", body)
	}

	rec := timePost(t, srv, cookie, "/time/manual", url.Values{"manual_time": {"2026-09-12T00:14:22"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "вимкніть синхронізацію") {
		t.Errorf("the refusal does not tell the operator what to do; body:\n%s", rec.Body.String())
	}
	for _, c := range fake.Calls() {
		if c.Method == "SetTime" {
			t.Error("the clock was set by hand despite NTP being on")
		}
	}
}

// With NTP off the field is offered, and what the browser submits is
// normalised into the form timedatectl accepts before it is run.
func TestTheManualClockIsAcceptedWhileNTPIsOff(t *testing.T) {
	srv, _, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	cookie := timeSession(t, srv)

	if body := timeGet(t, srv, cookie).Body.String(); !strings.Contains(body, `name="manual_time"`) {
		t.Errorf("the by-hand clock field is missing while NTP is off; body:\n%s", body)
	}

	rec := timePost(t, srv, cookie, "/time/manual", url.Values{"manual_time": {"2026-09-12T00:14:22"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	var stamp string
	for _, c := range fake.Calls() {
		if c.Method == "SetTime" {
			stamp = c.Arg
		}
	}
	if stamp != "2026-09-12 00:14:22" {
		t.Errorf("the clock was set to %q, want the timedatectl form 2026-09-12 00:14:22", stamp)
	}
}

func TestAnUnparseableManualClockIsRefused(t *testing.T) {
	srv, _, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	cookie := timeSession(t, srv)

	rec := timePost(t, srv, cookie, "/time/manual", url.Values{"manual_time": {"учора ввечері"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	for _, c := range fake.Calls() {
		if c.Method == "SetTime" {
			t.Errorf("%q was handed to the system as a timestamp", c.Arg)
		}
	}
}

// The NTP server ends up in a file a root unit parses. A value carrying a
// newline would append a directive of the submitter's choosing to it, so the
// form must refuse it and nothing may be stored or applied.
func TestAnInjectedNTPServerIsRefusedAndNothingIsApplied(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	cookie := timeSession(t, srv)

	for _, bad := range []string{
		"ntp.example.org\nNTP=evil.example.org",
		"ntp.example.org evil.example.org",
		`ntp.example.org"`,
		"NTP=evil.example.org",
	} {
		rec := timePost(t, srv, cookie, "/time/server", url.Values{"ntp_server": {bad}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /time/server %q = %d, want 400", bad, rec.Code)
		}
		if got := store.Get().Device.NTPServer; got != "" {
			t.Fatalf("%q was stored as the NTP server", got)
		}
		for _, c := range fake.Calls() {
			if c.Method == "ApplyNTPServer" {
				t.Fatalf("%q reached the system anyway", c.Arg)
			}
		}
	}
}

func TestSavingAnNTPServerStoresItAndAsksTheSystemToAdoptIt(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	cookie := timeSession(t, srv)

	rec := timePost(t, srv, cookie, "/time/server", url.Values{"ntp_server": {"ua.pool.ntp.org"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Device.NTPServer; got != "ua.pool.ntp.org" {
		t.Errorf("config ntp_server = %q, want ua.pool.ntp.org", got)
	}
	if fake.Server != "ua.pool.ntp.org" {
		t.Errorf("the system was asked to adopt %q, want ua.pool.ntp.org", fake.Server)
	}
}

// The same rule as the time zone: a privileged half that fails leaves the
// configuration describing a server the system is not using.
func TestAFailedNTPServerApplyLeavesTheConfigUnchanged(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	fake.ApplyNTPServerErr = errors.New("Failed to start spoloh-timesync.service: Access denied")
	cookie := timeSession(t, srv)

	rec := timePost(t, srv, cookie, "/time/server", url.Values{"ntp_server": {"ua.pool.ntp.org"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Device.NTPServer; got != "" {
		t.Errorf("config ntp_server = %q after the apply failed, want it rolled back to empty", got)
	}
}

// A board with no manager wired up — or one whose timedatectl cannot be
// reached — still has to render the rest of the dashboard and say why it
// knows nothing, rather than erroring outright.
func TestTimePageSaysSoWhenTheHostCannotBeAsked(t *testing.T) {
	srv, _ := newServerWithStore(t)
	cookie := timeSession(t, srv)

	rec := timeGet(t, srv, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "недоступне на цьому пристрої") {
		t.Errorf("the page does not say the clock could not be read; body:\n%s", rec.Body.String())
	}

	post := timePost(t, srv, cookie, "/time/timezone", url.Values{"timezone": {"Europe/Warsaw"}})
	if post.Code != http.StatusServiceUnavailable {
		t.Errorf("POST with no manager = %d, want 503", post.Code)
	}
}

// The configured zone must stay in the picker even when the host cannot be
// asked for its list, or an operator loses sight of their own setting on
// exactly the board whose zone needs changing.
func TestTheZonePickerFallsBackWithoutLosingTheConfiguredZone(t *testing.T) {
	srv, store, fake := newServerWithTimedate(t, timedate.State{Timezone: "Europe/Kyiv", CanNTP: true})
	fake.ZonesErr = errors.New("timedatectl: command not found")
	if err := store.Update(func(c *config.Config) error {
		c.Device.Timezone = "Europe/Warsaw"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	cookie := timeSession(t, srv)

	body := timeGet(t, srv, cookie).Body.String()
	if !strings.Contains(body, `<option value="Europe/Warsaw" selected>`) {
		t.Errorf("the configured zone is not offered or not selected; body:\n%s", body)
	}
	if !strings.Contains(body, `<option value="Europe/Kyiv"`) {
		t.Errorf("Europe/Kyiv fell out of the fallback picker; body:\n%s", body)
	}
}
