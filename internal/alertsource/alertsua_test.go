package alertsource_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

const testToken = "s3cret-operator-token"

func alertsUAFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/alertsua_active.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

// serveAlertsUA stands in for the API on loopback. It records every request
// so a test can assert on the headers that were actually sent — the
// Authorization header most of all, which is the whole difference between
// this provider and the six that need no credential.
func serveAlertsUA(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(context.Background()))
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// The parse that the whole provider was added for: an alert carries a level
// and a type, and both survive into the snapshot.
func TestAlertsUAParseCarriesLevelAndType(t *testing.T) {
	snap, err := alertsource.ParseAlertsUA(alertsUAFixture(t))
	if err != nil {
		t.Fatalf("ParseAlertsUA: %v", err)
	}

	sumy, ok := snap.Region("Сумська область")
	if !ok {
		t.Fatal("Сумська область is missing from the snapshot")
	}
	if !sumy.Alert {
		t.Error("Сумська область is not under alert")
	}
	if sumy.Level != alertsource.LevelRed {
		t.Errorf("Сумська область level = %q, want red", sumy.Level)
	}
	if sumy.Type != "air_raid" {
		t.Errorf("Сумська область alert type = %q, want air_raid", sumy.Type)
	}
	if sumy.UID != 20 {
		t.Errorf("Сумська область UID = %d, want 20", sumy.UID)
	}
	if want := time.Date(2026, 9, 12, 3, 12, 4, 0, time.UTC); !sumy.Changed.Equal(want) {
		t.Errorf("Сумська область changed = %v, want %v", sumy.Changed, want)
	}

	luhansk, ok := snap.Region("Луганська область")
	if !ok {
		t.Fatal("Луганська область is missing from the snapshot")
	}
	if luhansk.Level != alertsource.LevelYellow {
		t.Errorf("Луганська область level = %q, want yellow", luhansk.Level)
	}
	if luhansk.Level.TreatAsRed() {
		t.Error("a level upstream reported as yellow is being handled as red")
	}

	if snap.Source != "alerts.in.ua" {
		t.Errorf("snapshot source = %q, want alerts.in.ua", snap.Source)
	}
	if want := time.Date(2026, 9, 12, 3, 45, 0, 0, time.UTC); !snap.CachedAt.Equal(want) {
		t.Errorf("snapshot CachedAt = %v, want %v", snap.CachedAt, want)
	}
}

// An alert below oblast level has to survive the fetch as itself. Flattening
// every warning up to its oblast here would throw away the precision that is
// the reason for choosing this provider at all.
func TestAlertsUAParseKeepsAHromadaLevelAlert(t *testing.T) {
	snap, err := alertsource.ParseAlertsUA(alertsUAFixture(t))
	if err != nil {
		t.Fatalf("ParseAlertsUA: %v", err)
	}

	st, ok := snap.Region("м. Шостка та Шосткинська територіальна громада")
	if !ok {
		t.Fatal("the hromada-level alert did not survive the parse")
	}
	if !st.Alert || st.UID != 1198 {
		t.Errorf("hromada state = %+v, want an alert with UID 1198", st)
	}
	// Two alerts name this hromada at once: a red air raid, then a yellow
	// artillery warning that started earlier. The payload deliberately puts
	// the red one first, so a merge that simply kept whichever it saw last
	// would hand a running air raid the lighter announcement — and would
	// pass a fixture ordered the other way round without anyone noticing.
	if st.Level != alertsource.LevelRed {
		t.Errorf("overlapping alerts collapsed to level %q, want red", st.Level)
	}
	// The earlier of the two starts is when this location's alert actually
	// began; the later one is only when a second warning joined it.
	if want := time.Date(2026, 9, 12, 3, 10, 51, 0, time.UTC); !st.Changed.Equal(want) {
		t.Errorf("overlapping alerts collapsed to start %v, want the earlier %v", st.Changed, want)
	}
	if st.Type != "air_raid" {
		t.Errorf("overlapping alerts collapsed to type %q, want the first seen, air_raid", st.Type)
	}
}

// The endpoint lists what is active, so a location it does not list is calm —
// and the snapshot has to say so rather than leave it out. A consumer asking
// for an oblast that is not under alert must get "no alert", not "I have
// never heard of it", which is how the poll loop tells a working snapshot
// from a broken one.
func TestAlertsUAParseSeedsEveryOblastCalm(t *testing.T) {
	snap, err := alertsource.ParseAlertsUA(alertsUAFixture(t))
	if err != nil {
		t.Fatalf("ParseAlertsUA: %v", err)
	}

	for _, region := range alertsource.Regions() {
		st, ok := snap.Region(region)
		if !ok {
			t.Errorf("%s is absent from the snapshot; a consumer cannot tell calm from unknown", region)
			continue
		}
		if region == "Сумська область" || region == "Луганська область" {
			continue
		}
		if st.Alert {
			t.Errorf("%s is reported under alert but the payload never names it", region)
		}
	}
}

// An empty alerts array is a real answer — nowhere in the country is under
// alert — and must parse, not fail. Refusing it would make the cascade fall
// through to the next source on every quiet poll, which is most of them.
func TestAlertsUAParseAcceptsAnEmptyAlertList(t *testing.T) {
	snap, err := alertsource.ParseAlertsUA([]byte(`{"alerts":[],"meta":{"last_updated_at":"2026-09-12T03:45:00Z"}}`))
	if err != nil {
		t.Fatalf("an all-clear payload was rejected: %v", err)
	}
	if len(snap.States) == 0 {
		t.Fatal("an all-clear snapshot carries no states at all")
	}
	for name, st := range snap.States {
		if st.Alert {
			t.Errorf("%s is under alert in an empty payload", name)
		}
	}
}

// A payload this build cannot read must be an error. The dangerous failure
// here is not a crash, it is an empty snapshot: empty means "all clear", so a
// shape we do not understand would end a raid that is still running.
func TestAlertsUAParseRefusesAPayloadWithNoAlertsArray(t *testing.T) {
	for name, body := range map[string]string{
		"a bare object":      `{"meta":{"last_updated_at":"2026-09-12T03:45:00Z"}}`,
		"a bare array":       `[]`,
		"an error envelope":  `{"error":"unauthorized"}`,
		"not json at all":    `<html>502</html>`,
		"alerts of a string": `{"alerts":"none"}`,
	} {
		if _, err := alertsource.ParseAlertsUA([]byte(body)); err == nil {
			t.Errorf("%s parsed into a snapshot; an unreadable payload must never become an all-clear", name)
		}
	}
}

// The token is what separates this provider from the six that need none. A
// request that forgets it is a request that will be refused, and the failure
// would look in a log like a revoked token rather than a missing one.
func TestAlertsUAFetchSendsABearerToken(t *testing.T) {
	body := alertsUAFixture(t)
	srv, seen := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	c := alertsource.NewAlertsUA(srv.URL, testToken, srv.Client())
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	req := (*seen)[0]
	if got, want := req.Header.Get("Authorization"), "Bearer "+testToken; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if !strings.HasSuffix(req.URL.Path, "/"+alertsource.AlertsUAActivePath) {
		t.Errorf("fetched %q, want the %q endpoint", req.URL.Path, alertsource.AlertsUAActivePath)
	}
}

// Without a token there is nothing to ask with, and asking anyway would burn
// a request against the rate limit to be told what we already knew.
func TestAlertsUAFetchWithoutATokenMakesNoRequest(t *testing.T) {
	srv, seen := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a tokenless source reached the network")
		w.WriteHeader(http.StatusUnauthorized)
	})

	c := alertsource.NewAlertsUA(srv.URL, "", srv.Client())
	if c.HasToken() {
		t.Error("a source built with no token reports that it has one")
	}
	_, err := c.Fetch(context.Background())
	if !errors.Is(err, alertsource.ErrNoToken) {
		t.Fatalf("Fetch with no token = %v, want ErrNoToken", err)
	}
	if len(*seen) != 0 {
		t.Errorf("requests = %d, want none", len(*seen))
	}
}

// If-Modified-Since is what keeps a heavier endpoint affordable at a 15
// second poll. The 304 it earns has to come back as the same picture the 200
// did — a 304 read as "no alerts" would end every raid at the first quiet
// poll after it started.
func TestAlertsUAFetchRevalidatesAndReusesA304(t *testing.T) {
	const lastModified = "Sat, 12 Sep 2026 03:45:00 GMT"
	body := alertsUAFixture(t)
	srv, seen := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lastModified)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	c := alertsource.NewAlertsUA(srv.URL, testToken, srv.Client())
	first, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	second, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch (the 304): %v", err)
	}

	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
	if got := (*seen)[0].Header.Get("If-Modified-Since"); got != "" {
		t.Errorf("the first request sent If-Modified-Since: %q", got)
	}
	if got := (*seen)[1].Header.Get("If-Modified-Since"); got != lastModified {
		t.Errorf("the second request sent If-Modified-Since %q, want %q", got, lastModified)
	}

	sumyFirst, _ := first.Region("Сумська область")
	sumySecond, ok := second.Region("Сумська область")
	if !ok || !sumySecond.Alert || sumySecond.Level != alertsource.LevelRed {
		t.Fatalf("the 304 lost the alert: %+v", sumySecond)
	}
	if sumyFirst != sumySecond {
		t.Errorf("the 304 returned a different state: %+v vs %+v", sumySecond, sumyFirst)
	}
	// Re-parsed rather than handed back, so one caller's snapshot cannot be
	// written through by another's.
	if len(first.States) == 0 {
		t.Fatal("the first snapshot is empty")
	}
	first.States["Сумська область"] = alertsource.RegionState{}
	if st, _ := second.Region("Сумська область"); !st.Alert {
		t.Error("the two snapshots share one map; writing to the first emptied the second")
	}
}

// The rate limit is per public IP, not per token, so a household with two
// devices shares it. A 429 must come back as a RateLimitError the poll loop
// can back off on, not as a generic failure it retries straight into.
func TestAlertsUAFetchRateLimited(t *testing.T) {
	srv, _ := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"too many requests"}`))
	})

	c := alertsource.NewAlertsUA(srv.URL, testToken, srv.Client())
	_, err := c.Fetch(context.Background())

	var rl *alertsource.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("Fetch on a 429 = %v, want RateLimitError", err)
	}
	if rl.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", rl.RetryAfter)
	}
}

// The token is a credential. It must not reach a log line, a wrapped error,
// or the page an operator is reading when the fetch fails — and a rejected
// token is exactly the moment someone is tempted to print it "to check".
func TestAlertsUAErrorsNeverCarryTheToken(t *testing.T) {
	statuses := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusTooManyRequests}
	for _, status := range statuses {
		srv, _ := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			// An upstream that echoes the token back in its own error
			// body must not get it onto our side either.
			w.Write([]byte(`{"error":"bad token ` + testToken + `"}`))
		})

		c := alertsource.NewAlertsUA(srv.URL, testToken, srv.Client())
		_, err := c.Fetch(context.Background())
		if err == nil {
			t.Fatalf("http %d did not fail", status)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("the error for http %d contains the token: %v", status, err)
		}
	}
}

// A 200 carrying HTML is a captive portal or an upstream error page. Reading
// it as state would be worse than reporting a failure the cascade can move
// past.
func TestAlertsUAFetchRejectsHTMLWith200(t *testing.T) {
	srv, _ := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>sign in to the wifi</html>"))
	})

	c := alertsource.NewAlertsUA(srv.URL, testToken, srv.Client())
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("an HTML body was accepted as alert state")
	}
}

// AlertsUA has to be usable everywhere a Ubilling source is, or a cascade
// could not mix them.
func TestAlertsUASatisfiesSource(t *testing.T) {
	var _ alertsource.Source = alertsource.NewAlertsUA("", "", nil)
}

// The live failure this exists for, with the real UIDs off the operator's
// own device: alerts.in.ua declared «Шосткинський район» (115) under a red
// air raid, with nothing on Сумська область (20) above it and nothing on
// «м. Шостка та Шосткинська територіальна громада» (1198) below. The device
// was watching 1198, found no alert for it, broadened to the oblast, found
// that calm, and announced nothing — through a red alert over the district
// it stands in.
func TestARaionAlertRaisesTheHromadasInsideIt(t *testing.T) {
	body := []byte(`{"alerts":[{
		"id": 1,
		"location_uid": "115",
		"location_title": "Шосткинський район",
		"location_type": "raion",
		"location_oblast": "Сумська область",
		"location_oblast_uid": "20",
		"started_at": "2026-09-12T08:00:00.000Z",
		"finished_at": null,
		"alert_type": "air_raid",
		"alert_level": "red"
	}]}`)

	snap, err := alertsource.ParseAlertsUA(body)
	if err != nil {
		t.Fatalf("ParseAlertsUA: %v", err)
	}

	hromada, ok := snap.Location(1198)
	if !ok {
		t.Fatal("the hromada is not in the snapshot at all, so a calm one cannot be told from an unsupported one")
	}
	if !hromada.Alert {
		t.Error("the raion containing Шостка is under a red alert and the hromada reports calm")
	}
	if hromada.Level != alertsource.LevelRed {
		t.Errorf("hromada level = %v, want the raion's red", hromada.Level)
	}
	if hromada.Changed.IsZero() {
		t.Error("the inherited alert carries no start time: it began when the raion's did, not when this poll noticed")
	}
}

// A hromada raised on its own must not raise the oblast around it. An oblast
// watcher hearing a siren for a raid two districts away learns to ignore the
// siren, and that is a worse device than one that stays quiet.
func TestAHromadaAlertDoesNotRaiseTheOblastAroundIt(t *testing.T) {
	body := []byte(`{"alerts":[{
		"id": 2,
		"location_uid": "1198",
		"location_title": "м. Шостка та Шосткинська територіальна громада",
		"location_type": "hromada",
		"started_at": "2026-09-12T08:00:00.000Z",
		"finished_at": null,
		"alert_type": "air_raid",
		"alert_level": "yellow"
	}]}`)

	snap, err := alertsource.ParseAlertsUA(body)
	if err != nil {
		t.Fatalf("ParseAlertsUA: %v", err)
	}

	if st, ok := snap.Location(1198); !ok || !st.Alert {
		t.Fatal("the hromada named by the alert is not raised")
	}
	if st, ok := snap.Location(20); ok && st.Alert {
		t.Error("one raised hromada raised the whole oblast")
	}
	if st, ok := snap.Location(115); ok && st.Alert {
		t.Error("one raised hromada raised the raion above it")
	}
}

// A calm hromada must be present and calm, not absent. Absent is read one
// level up as "this provider cannot serve that granularity" and broadens to
// the oblast, which is how a device set to one hromada quietly watched a
// whole oblast for as long as nothing was happening.
func TestACalmHromadaIsPresentAndCalm(t *testing.T) {
	snap, err := alertsource.ParseAlertsUA([]byte(`{"alerts":[]}`))
	if err != nil {
		t.Fatalf("ParseAlertsUA: %v", err)
	}
	st, ok := snap.Location(1198)
	if !ok {
		t.Fatal("a calm hromada is missing from the snapshot")
	}
	if st.Alert {
		t.Error("a hromada with no alert anywhere reports one")
	}
}
