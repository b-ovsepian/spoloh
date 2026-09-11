package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// This is the screen operator feedback asked for: the oblast used to live on
// /network (network_test.go carried these cases before this task moved
// them), findable only by someone who already knew it was there. These tests
// prove the move kept every rule /network used to enforce, and cover the two
// new ones the source cascade needs.

// sourceSlotHTML returns the rendered markup for one cascade-position
// <select> — from its opening tag to its closing "</select>" — so a test can
// assert which option is selected at that specific position rather than
// merely that the option's markup appears somewhere on a page that renders
// the same six options six times over.
func sourceSlotHTML(t *testing.T, body string, position int) string {
	t.Helper()
	marker := fmt.Sprintf(`id="source-%d"`, position)
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("no source slot rendered for position %d", position)
	}
	end := strings.Index(body[start:], "</select>")
	if end < 0 {
		t.Fatalf("source slot %d has no closing </select>", position)
	}
	return body[start : start+end]
}

// selectedOption is the exact markup an alerts.html <option> renders with
// when it is the selected one — asserting this rather than a looser
// substring proves the option is actually marked selected, not merely
// present in the list.
func selectedOption(value string) string {
	return fmt.Sprintf(`<option value="%s" selected>`, value)
}

// sourceSlotFieldHTML returns one cascade slot's whole "field" block — the
// <select> and, when the chosen key has a documentation link, the <p> that
// follows it. Unlike sourceSlotHTML, which stops at the select's own closing
// tag, this can see whether a link was rendered after it.
func sourceSlotFieldHTML(t *testing.T, body string, position int) string {
	t.Helper()
	marker := fmt.Sprintf(`id="source-%d"`, position)
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("no source slot rendered for position %d", position)
	}
	end := strings.Index(body[start:], "</div>")
	if end < 0 {
		t.Fatalf("source slot %d has no closing </div>", position)
	}
	return body[start : start+end]
}

func TestAlertsRequiresSession(t *testing.T) {
	srv := newServer(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/alerts", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /alerts with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

// A fresh device (config.Default()) already has a region and a cascade; the
// page must show exactly those as selected, in the order they are stored —
// not the canonical alphabetical-by-declaration order alertsource.Sources()
// happens to return.
func TestAlertsPageShowsTheStoredRegionAndCascadeOrder(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	cfg := store.Get()
	if len(cfg.Alerts.Sources) < 2 {
		t.Fatalf("setup: config.Default() sources = %v, want at least two to prove order", cfg.Alerts.Sources)
	}

	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, fmt.Sprintf(`<option value="%s" selected>%s</option>`, cfg.Alerts.Region, cfg.Alerts.Region)) {
		t.Errorf("the configured region %q is not rendered as selected", cfg.Alerts.Region)
	}
	for i, want := range cfg.Alerts.Sources {
		slot := sourceSlotHTML(t, body, i+1)
		if !strings.Contains(slot, selectedOption(want)) {
			t.Errorf("position %d: want %q selected, slot markup = %s", i+1, want, slot)
		}
	}
	// The position right after the stored cascade must fall back to "not
	// used" — otherwise a shorter-than-full cascade would look padded with
	// a source that was never actually configured there.
	next := sourceSlotHTML(t, body, len(cfg.Alerts.Sources)+1)
	if !strings.Contains(next, selectedOption("")) {
		t.Errorf("position %d (past the stored cascade) is not left on \"not used\": %s", len(cfg.Alerts.Sources)+1, next)
	}
}

func TestAlertsSaveRejectsCrossOrigin(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts

	req := postForm("/alerts", url.Values{
		"region": {"Львівська область"}, "source": {"default", "skog"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("a cross-site /alerts submission was accepted")
	}
	if got := store.Get().Alerts; got.Region != before.Region || strings.Join(got.Sources, ",") != strings.Join(before.Sources, ",") {
		t.Fatalf("stored alerts config changed on a cross-site submission: got %+v, want unchanged %+v", got, before)
	}
}

// A crafted form could submit a region key that matches no known oblast,
// which would silence the device permanently while the dashboard still
// looked configured. This is the same rule /network used to enforce before
// the oblast moved here.
func TestAlertsSaveRejectsAnUnknownRegion(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Region

	req := postForm("/alerts", url.Values{
		"region": {"Марсіанська область"}, "source": {"default", "skog"},
		"poll_interval": {"20"},
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
}

// An empty cascade is a device that never learns anything and never says
// why, so it must be refused rather than silently stored.
func TestAlertsSaveRejectsAnEmptyCascade(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Sources

	req := postForm("/alerts", url.Values{
		"region": {"Львівська область"}, "source": {"", "", "", "", "", ""},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection for an empty cascade", rec.Code)
	}
	if got := strings.Join(store.Get().Alerts.Sources, ","); got != strings.Join(before, ",") {
		t.Fatalf("stored sources = %q, want unchanged %q", got, strings.Join(before, ","))
	}
}

// A cascade filled entirely with keys that match nothing in
// alertsource.Sources() is, for the device, indistinguishable from an empty
// one: every entry fails upstream and the device never learns anything. A
// crafted form must not be able to reach that state by filling every slot
// with garbage the checkboxes on the real page could never produce.
func TestAlertsSaveRejectsACascadeOfOnlyUnknownSources(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.Sources

	req := postForm("/alerts", url.Values{
		"region": {"Львівська область"}, "source": {"not-a-real-source", "another-fake-one"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection for a cascade of only unknown sources", rec.Code)
	}
	if got := strings.Join(store.Get().Alerts.Sources, ","); got != strings.Join(before, ",") {
		t.Fatalf("stored sources = %q, want unchanged %q", got, strings.Join(before, ","))
	}
}

// The whole point of asking for the cascade slot by slot is that the
// submitted order is the order it is tried in — not sorted, and not
// reordered to match alertsource.Sources()'s own declaration order.
func TestAlertsSavePreservesTheSubmittedCascadeOrder(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	// alertsource.Sources() declares "default" before "jaam" before "skog";
	// this submits them in the opposite order to prove nothing re-sorts them.
	req := postForm("/alerts", url.Values{
		"region": {"Одеська область"}, "source": {"skog", "jaam", "default"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := []string{"skog", "jaam", "default"}
	got := store.Get().Alerts.Sources
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stored sources = %v, want %v in the exact submitted order", got, want)
	}
}

// A source repeated across two positions is kept only at the first one it
// appears in: trying the same upstream twice in one cascade does nothing a
// single try does not, and letting it occupy two slots would only crowd out
// a source that could have used the second one.
func TestAlertsSaveDropsARepeatedSourceKeepingItsFirstPosition(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/alerts", url.Values{
		"region": {"Одеська область"}, "source": {"skog", "default", "skog"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := []string{"skog", "default"}
	got := store.Get().Alerts.Sources
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stored sources = %v, want %v (the repeat dropped, not the original position)", got, want)
	}
}

// A "not used" position in the middle of the cascade is skipped, not treated
// as the end of the list: a source picked at a later position must not be
// silently dropped just because an earlier one was left blank.
func TestAlertsSaveSkipsAGapInTheMiddleOfTheCascade(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/alerts", url.Values{
		"region": {"Одеська область"}, "source": {"default", "", "skog"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := []string{"default", "skog"}
	got := store.Get().Alerts.Sources
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stored sources = %v, want %v (the gap skipped, not treated as the end of the cascade)", got, want)
	}
}

// The happy path: a valid region and a valid cascade are both stored
// exactly as submitted, and the page confirms it rather than redirecting.
func TestAlertsSaveSucceedsAndStoresRegionAndSources(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/alerts", url.Values{
		"region": {"Харківська область"}, "source": {"default", "aiu", "ual"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "збережено") {
		t.Error("the success page does not confirm the save")
	}

	cfg := store.Get()
	if cfg.Alerts.Region != "Харківська область" {
		t.Errorf("stored region = %q, want %q", cfg.Alerts.Region, "Харківська область")
	}
	want := []string{"default", "aiu", "ual"}
	if strings.Join(cfg.Alerts.Sources, ",") != strings.Join(want, ",") {
		t.Errorf("stored sources = %v, want %v", cfg.Alerts.Sources, want)
	}
}

// How often the cascade is polled belongs on this screen for the same reason
// the cascade itself does: both answer how alert data is fetched. The page
// must show what is actually stored rather than a placeholder — an operator
// who has just been rate-limited comes here to read the current rate.
func TestAlertsPageShowsTheStoredPollInterval(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	want := int(store.Get().Alerts.PollInterval.Std() / time.Second)
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`name="poll_interval" value="%d"`, want)) {
		t.Errorf("the page does not show the stored poll interval of %ds; body:\n%s", want, rec.Body.String())
	}
}

// An operator asking for a rate the sources will throttle is told no, in a
// sentence that names the limit. This is the human-facing half of
// config.MinPollInterval and the deliberate opposite of the repair
// config.Load makes to a file an older build wrote: a person asking for
// something impossible gets an error, a stored file gets repaired.
func TestAlertsSaveRejectsAPollIntervalBelowTheMinimum(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts.PollInterval

	req := postForm("/alerts", url.Values{
		"region": {"Львівська область"}, "source": {"default", "skog"},
		"poll_interval": {"5"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection for a 5s poll interval", rec.Code)
	}
	if got := store.Get().Alerts.PollInterval; got != before {
		t.Fatalf("stored poll interval = %v, want unchanged %v", got.Std(), before.Std())
	}
	// The whole sentence, not just the number: the field's own help text
	// names 10 seconds too, so a looser assertion would pass against a page
	// showing the generic "could not save" message instead of this rule.
	if body := rec.Body.String(); !strings.Contains(body, "Інтервал опитування має бути не менший за 10 секунд.") {
		t.Errorf("the rejection does not tell the operator what the limit is; body:\n%s", body)
	}
}

// A submission carrying no interval at all must not reach the store: strconv
// would otherwise hand it a zero, and an empty POST mutating stored config is
// exactly what leak_test.go's walk of every route assumes cannot happen. The
// same rule events.go's parseVolume keeps for a blank volume.
func TestAlertsSaveRejectsAMissingPollInterval(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	before := store.Get().Alerts

	req := postForm("/alerts", url.Values{
		"region": {"Львівська область"}, "source": {"default", "skog"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection for a submission with no interval", rec.Code)
	}
	if got := store.Get().Alerts; got.PollInterval != before.PollInterval || got.Region != before.Region {
		t.Fatalf("stored alerts config changed on a submission with no interval: got %+v, want unchanged %+v", got, before)
	}
}

// The happy path for the field: seconds in, seconds stored, and the page
// comes back showing what was just saved.
func TestAlertsSaveStoresTheSubmittedPollInterval(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/alerts", url.Values{
		"region": {"Одеська область"}, "source": {"default"},
		"poll_interval": {"30"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Alerts.PollInterval.Std(); got != 30*time.Second {
		t.Fatalf("stored poll interval = %v, want 30s", got)
	}
	if !strings.Contains(rec.Body.String(), `name="poll_interval" value="30"`) {
		t.Error("the page does not come back showing the interval that was just saved")
	}
}

// A rejection caused by the region must not throw away an interval the
// operator already typed correctly — the same "don't lose what they typed"
// contract the cascade keeps across an unrelated rejection.
func TestAlertsSaveRejectionOnBadRegionKeepsTheSubmittedPollInterval(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/alerts", url.Values{
		"region": {"Марсіанська область"}, "source": {"default"},
		"poll_interval": {"42"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `name="poll_interval" value="42"`) {
		t.Errorf("the validly submitted interval was not kept across a region rejection; body:\n%s", rec.Body.String())
	}
}

// alertsource.Regions() and alertsource.Sources() are what this page's
// validation and picker both read; this pins their sizes so a change to
// either package is felt here rather than only discovered by a page that
// silently offers fewer choices than it used to.
func TestAlertsPageOffersEveryKnownRegionAndSource(t *testing.T) {
	srv := newServer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, region := range alertsource.Regions() {
		if !strings.Contains(body, fmt.Sprintf(`<option value="%s"`, region)) {
			t.Errorf("region %q is not offered on the page", region)
		}
	}
	slot := sourceSlotHTML(t, body, 1)
	// The selectable set, not the whole registry: a source whose credential
	// this device does not hold must not appear in the picker at all.
	for _, source := range alertsource.SelectableSources(false) {
		if !strings.Contains(slot, fmt.Sprintf(`<option value="%s"`, source)) {
			t.Errorf("source %q is not offered in the cascade picker", source)
		}
	}
	if strings.Contains(slot, fmt.Sprintf(`<option value="%s"`, alertsource.AlertsUAKey)) {
		t.Errorf("%q is offered with no token stored; the cascade slot would authenticate against nothing", alertsource.AlertsUAKey)
	}
}

// A rejection caused by the region must not throw away a cascade the
// operator already picked correctly — the same "don't lose what they typed"
// contract network.go's renderNetworkRejected keeps for the selected SSID.
// The region itself falls back to what is actually stored: a <select> that
// only ever offers alertsource.Regions()'s 25 keys has no option to mark
// selected for "Марсіанська область", so there is nothing to echo back.
func TestAlertsSaveRejectionOnBadRegionKeepsTheSubmittedCascade(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	storedRegion := store.Get().Alerts.Region

	req := postForm("/alerts", url.Values{
		"region": {"Марсіанська область"}, "source": {"jaam", "ual"},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, fmt.Sprintf(`<option value="%s" selected>%s</option>`, storedRegion, storedRegion)) {
		t.Errorf("the page does not fall back to the stored region %q", storedRegion)
	}
	if slot := sourceSlotHTML(t, body, 1); !strings.Contains(slot, selectedOption("jaam")) {
		t.Errorf("position 1 does not keep the submitted source \"jaam\": %s", slot)
	}
	if slot := sourceSlotHTML(t, body, 2); !strings.Contains(slot, selectedOption("ual")) {
		t.Errorf("position 2 does not keep the submitted source \"ual\": %s", slot)
	}
}

// A rejection caused by the cascade must not throw away a region the
// operator already picked correctly, mirroring the case above.
func TestAlertsSaveRejectionOnEmptyCascadeKeepsTheSubmittedRegion(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/alerts", url.Values{
		"region": {"Тернопільська область"}, "source": {""},
		"poll_interval": {"20"},
	})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<option value="Тернопільська область" selected>Тернопільська область</option>`) {
		t.Error("the validly submitted region was not kept across a cascade rejection")
	}
}

// isKnownRegion moved here unchanged from network.go's own unit test intent;
// config.Default()'s own region must, at minimum, be one alertsource.Regions()
// actually reports, or every device would ship pre-broken.
func TestDefaultRegionIsAKnownRegion(t *testing.T) {
	def := config.Default()
	for _, r := range alertsource.Regions() {
		if r == def.Alerts.Region {
			return
		}
	}
	t.Fatalf("config.Default().Alerts.Region = %q, want one of alertsource.Regions()", def.Alerts.Region)
}

// The label is pinned in a test because a rename that nothing tests can be
// reverted by the next edit without anything noticing — the exact failure
// Task 5's «Аудіовихід» rename ran into. "Джерела" says what the screen is
// for: choosing where alert data comes from. "Тривоги" read as though the
// screen showed the alerts themselves, which is the home page's job.
func TestAlertsPageHeadingAndNavLabelReadDzherela(t *testing.T) {
	srv := newServer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<h1 class="text-lg font-semibold">Джерела</h1>`) {
		t.Errorf("the page heading is not \"Джерела\"; body:\n%s", body)
	}
	if !strings.Contains(body, `'/alerts' ? 'page' : null">Джерела</a>`) {
		t.Errorf("the nav link to /alerts is not labelled \"Джерела\"; body:\n%s", body)
	}
}

// An operator who does not recognise a cascade key, or is debugging one that
// keeps failing, needs a way to go and look at it — task-sources-brief.md's
// whole point. config.Default() already puts "default" first, and
// alertsource.Lookup("default") is the one key this repository can back with
// a real URL (see internal/alertsource/registry.go), so a fresh device's own
// /alerts page is enough to prove the link renders, opens safely, and does
// not read as a live status.
func TestAlertsPageShowsDocumentationLinkForAKnownSource(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	cfg := store.Get()
	if len(cfg.Alerts.Sources) == 0 || cfg.Alerts.Sources[0] != "default" {
		t.Fatalf("setup: want \"default\" first in the stored cascade, got %v", cfg.Alerts.Sources)
	}
	info, ok := alertsource.Lookup("default")
	if !ok || info.URL == "" {
		t.Fatalf("setup: alertsource.Lookup(\"default\") has no URL to assert against")
	}

	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	field := sourceSlotFieldHTML(t, rec.Body.String(), 1)
	if !strings.Contains(field, fmt.Sprintf(`href="%s"`, info.URL)) {
		t.Errorf("position 1 (%q) does not link to %q; field markup: %s", "default", info.URL, field)
	}
	if !strings.Contains(field, `target="_blank"`) {
		t.Errorf("the source link does not open in a new tab; field markup: %s", field)
	}
	if !strings.Contains(field, `rel="noopener noreferrer"`) {
		t.Errorf("the source link does not carry rel=\"noopener noreferrer\"; field markup: %s", field)
	}
	// The operator asked for a compact control beside the <select> rather
	// than a sentence on its own line. "API" is the whole of the visible
	// text; the sentence moved into the accessible name.
	if !strings.Contains(field, ">API<") {
		t.Errorf("the source link is not the compact \"API\" control; field markup: %s", field)
	}
	if strings.Contains(field, "Документація джерела «Ubilling»<") {
		t.Errorf("the source link still renders the old full sentence as its visible text; field markup: %s", field)
	}
}

// "не використовується" must never grow a link: there is nothing chosen for
// it to document, and rendering one anyway would look like every other slot
// that does have a source.
func TestAlertsPageShowsNoDocumentationLinkForAnUnusedSlot(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	unusedPosition := len(store.Get().Alerts.Sources) + 1

	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	field := sourceSlotFieldHTML(t, rec.Body.String(), unusedPosition)
	if strings.Contains(field, "<a ") {
		t.Errorf("an unused slot rendered a documentation link; field markup: %s", field)
	}
}

// Every registered source now carries the documentation URL for the one API
// they are all values of, so the "no link" branch can no longer be reached
// through the real registry. It is still the right behaviour for a source
// added later without a URL, and buildSourceSlots is tested against a
// URL-less entry directly in alerts_internal_test.go. What this asserts is
// the claim the page makes today: choose a source, get a way to read about
// it.
func TestAlertsPageLinksEveryChosenSource(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, key := range alertsource.SelectableSources(false) {
		info, ok := alertsource.Lookup(key)
		if !ok || info.URL == "" {
			t.Fatalf("setup: source %q carries no URL: %+v", key, info)
		}

		req := postForm("/alerts", url.Values{
			"region": {"Львівська область"}, "source": {key},
			"poll_interval": {"20"},
		})
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body:\n%s", key, rec.Code, rec.Body.String())
		}
		field := sourceSlotFieldHTML(t, rec.Body.String(), 1)
		if !strings.Contains(field, info.URL) {
			t.Errorf("%s: no link to %s; field markup: %s", key, info.URL, field)
		}
		// Compact, and named. The visible word is the same on every slot,
		// so the accessible name is the only thing telling a screen reader
		// user which source this one documents — and now that the links no
		// longer all lead to the same page, that is a real difference.
		if !strings.Contains(field, fmt.Sprintf(`aria-label="Документація джерела «%s» — відкриється в новій вкладці"`, info.Name)) {
			t.Errorf("%s: the link has no accessible name saying which source it documents; field markup: %s", key, field)
		}
		// "<p class=" rather than "<p": the arrow is an inline <svg> and
		// its <path> elements would match the looser prefix.
		if strings.Contains(field, "<p class=") {
			t.Errorf("%s: the link is still a paragraph of its own rather than a control beside the picker; field markup: %s", key, field)
		}
	}
}
