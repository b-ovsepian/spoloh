package alertsource_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// The key a test sends. Distinctive, so that finding it somewhere it must not
// be means it was put there rather than two words lining up.
const testKey = "ukrainealarm-key-for-this-device"

// The published UIDs this file works in, off the table this repository ships:
// Сумська область, Сумський and Шосткинський районы, and the hromada holding
// Шостка. They are the same chain internal/alertsource pins elsewhere.
const (
	sumyOblastUID  = 20
	sumyRaionUID   = 114
	shostkaRaionID = 115
	shostkaUID     = 1198
)

// The ukrainealarm ids those places carry in these tests.
//
// They are stand-ins, and deliberately not the published UIDs wearing a
// different hat: the whole point of the mapping is that the two id spaces
// have nothing to do with each other, and a fixture where they matched would
// pass against a client that confused them. The real ids come from GET
// /api/v3/regions, which needs a key nobody here has — see
// ukrainealarm_table.go.
const (
	sumyOblastRegionID  = "1400"
	sumyRaionRegionID   = "1414"
	shostkaRaionRegion  = "1415"
	shostkaHromadaRegio = "1498"
)

// testRegions is the mapping those constants describe, installed by every
// test below that needs this source to be able to name a place at all.
func testRegions() map[int]string {
	return map[int]string{
		sumyOblastUID:  sumyOblastRegionID,
		sumyRaionUID:   sumyRaionRegionID,
		shostkaRaionID: shostkaRaionRegion,
		shostkaUID:     shostkaHromadaRegio,
	}
}

// ukraineAlarmFixture is one oblast's answer.
//
// Built from the API's own specification (docs/ukrainealarm-swagger.json)
// rather than captured from the live service, which needs a key this work did
// not have. Every field in it is one the spec declares, with the levels and
// the types spelled exactly as its enums spell them — Red, Yellow, AIR,
// ARTILLERY — because a fixture that invented a spelling would be a fixture
// that tests a client nobody can use.
func ukraineAlarmFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/ukrainealarm_alerts.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

// serveUkraineAlarm stands in for the API on loopback and records every
// request, so a test can assert both on the header that was sent and on which
// endpoints were asked at all — the change index turning one poll into one
// cheap request is a claim about the requests themselves.
func serveUkraineAlarm(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(context.Background()))
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// paths names the endpoints a run of requests reached, in order.
func paths(reqs []*http.Request) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.URL.Path
	}
	return out
}

// The difference between this provider and every other one here is one
// header, and getting it wrong is a 401 that reads in a log exactly like a
// key that has been revoked. The spec declares an apiKey in a header named
// Authorization — not an HTTP authentication scheme — so the key travels
// bare, with no "Bearer " and no other prefix in front of it.
func TestUkraineAlarmFetchSendsTheKeyWithNoPrefix(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	body := ukraineAlarmFixture(t)
	srv, seen := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %v, want one", paths(*seen))
	}
	req := (*seen)[0]
	if got := req.Header.Get("Authorization"); got != testKey {
		t.Errorf("Authorization = %q, want the bare key %q", got, testKey)
	}
	// The region asked for is the oblast containing the chosen hromada, not
	// the hromada: an answer for one hromada cannot carry the alert its raion
	// is under, and that is the alert this device would otherwise miss.
	if want := "/" + alertsource.UkraineAlarmAlertsPath + "/" + sumyOblastRegionID; req.URL.Path != want {
		t.Errorf("fetched %q, want %q", req.URL.Path, want)
	}
}

// The parse this provider was added for: a level and a type the six Ubilling
// keys cannot report at all, for a place finer than an oblast.
func TestUkraineAlarmParseCarriesLevelAndType(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())
	oblast, _ := alertsource.LookupLocation(sumyOblastUID)

	snap, err := alertsource.ParseUkraineAlarm(ukraineAlarmFixture(t), oblast)
	if err != nil {
		t.Fatalf("ParseUkraineAlarm: %v", err)
	}

	raion, ok := snap.Location(shostkaRaionID)
	if !ok {
		t.Fatal("Шосткинський район is missing from the snapshot")
	}
	if !raion.Alert {
		t.Error("Шосткинський район is not under alert")
	}
	if raion.Level != alertsource.LevelRed {
		t.Errorf("Шосткинський район level = %q, want red", raion.Level)
	}
	if raion.Type != "AIR" {
		t.Errorf("Шосткинський район type = %q, want the API's own AIR", raion.Type)
	}
	if want := time.Date(2026, 9, 12, 6, 10, 51, 0, time.UTC); !raion.Changed.Equal(want) {
		t.Errorf("Шосткинський район changed = %v, want the level's createdAt %v", raion.Changed, want)
	}

	// The other half of the reason this provider is here: a yellow alert is
	// genuinely yellow, and must not be announced at full weight.
	sumy, ok := snap.Location(sumyRaionUID)
	if !ok {
		t.Fatal("Сумський район is missing from the snapshot")
	}
	if sumy.Level != alertsource.LevelYellow {
		t.Errorf("Сумський район level = %q, want yellow", sumy.Level)
	}
	if sumy.Level.TreatAsRed() {
		t.Error("a level upstream reported as Yellow is being handled as red")
	}

	if snap.Source != "ukrainealarm.com" {
		t.Errorf("snapshot source = %q, want ukrainealarm.com", snap.Source)
	}
	if want := time.Date(2026, 9, 12, 6, 45, 0, 0, time.UTC); !snap.CachedAt.Equal(want) {
		t.Errorf("snapshot CachedAt = %v, want %v", snap.CachedAt, want)
	}
}

// A region the answer does not name is calm, and has to say so. Absent is
// read one level up as "this provider cannot serve that granularity", so a
// hromada that is missing rather than present-and-calm quietly turns a device
// watching one hromada into a device watching its whole oblast.
func TestUkraineAlarmParseReportsTheCalmPartsOfTheOblast(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())
	oblast, _ := alertsource.LookupLocation(sumyOblastUID)

	snap, err := alertsource.ParseUkraineAlarm([]byte(`[{"regionId":"1400","regionType":"State","regionName":"Сумська область","activeAlerts":[]}]`), oblast)
	if err != nil {
		t.Fatalf("an all-clear answer was rejected: %v", err)
	}
	for _, uid := range []int{sumyOblastUID, sumyRaionUID, shostkaRaionID, shostkaUID} {
		st, ok := snap.Location(uid)
		if !ok {
			t.Errorf("UID %d is absent from an all-clear snapshot; a consumer cannot tell calm from unknown", uid)
			continue
		}
		if st.Alert {
			t.Errorf("UID %d is under alert in an answer that names no alerts", uid)
		}
	}

	// And nothing outside the oblast that was asked about: this provider was
	// asked one question and can answer one question. Seeding the rest of the
	// country calm would be an all-clear nobody checked.
	if _, ok := snap.Region("Львівська область"); ok {
		t.Error("the snapshot claims to know an oblast this request never asked about")
	}
}

// A payload this build cannot read must be an error. The dangerous failure is
// not a crash, it is an empty snapshot: empty means "nothing is running", so
// a shape we do not understand would end a raid that is still going.
func TestUkraineAlarmParseRefusesAPayloadThatIsNotRegions(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())
	oblast, _ := alertsource.LookupLocation(sumyOblastUID)

	for name, body := range map[string]string{
		"a null body":       `null`,
		"an error envelope": `{"error":"unauthorized"}`,
		"not json at all":   `<html>502</html>`,
	} {
		if _, err := alertsource.ParseUkraineAlarm([]byte(body), oblast); err == nil {
			t.Errorf("%s parsed into a snapshot; an unreadable payload must never become an all-clear", name)
		}
	}
}

// The failure this containment exists for, in this provider's own terms: the
// alert is declared on the raion, and the device is watching a hromada inside
// it. A hromada that reported calm through a red alert over the district it
// stands in is the bug the alerts.in.ua provider already shipped once.
func TestUkraineAlarmRaionAlertRaisesTheHromadasInsideIt(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())
	oblast, _ := alertsource.LookupLocation(sumyOblastUID)

	snap, err := alertsource.ParseUkraineAlarm(ukraineAlarmFixture(t), oblast)
	if err != nil {
		t.Fatalf("ParseUkraineAlarm: %v", err)
	}

	hromada, ok := snap.Location(shostkaUID)
	if !ok {
		t.Fatal("the hromada is not in the snapshot at all")
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

	// And nothing runs the other way. An oblast raised because one raion
	// inside it is raised would have an oblast watcher hearing a siren for a
	// raid two districts away, which teaches them to ignore the siren.
	if st, ok := snap.Location(sumyOblastUID); ok && st.Alert {
		t.Error("an alert on one raion raised the whole oblast")
	}
}

// This provider reports Red or Yellow and nothing else, so it must never
// answer "unknown" for a region it did answer for. Unknown is handled as red
// everywhere here, so it is not dangerous — it is a dashboard saying it does
// not know a level the source had just told it.
func TestUkraineAlarmNeverReportsAnUnknownLevel(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())
	oblast, _ := alertsource.LookupLocation(sumyOblastUID)

	// An alert with no levels at all, and one with a level this build has
	// never seen: both are alerts, and neither may come back unknown — nor
	// may either be softened into yellow, which is the guess that leaves a
	// street asleep.
	body := []byte(`[{"regionId":"1400","regionType":"State","activeAlerts":[
		{"regionId":"1415","regionType":"District","type":"AIR","lastUpdate":"2026-09-12T06:12:04.000Z"},
		{"regionId":"1414","regionType":"District","type":"CHEMICAL","lastUpdate":"2026-09-12T06:12:04.000Z",
		 "activeAlertLevels":[{"alertLevel":"Crimson","createdAt":"2026-09-12T06:00:00.000Z"}]}
	]}]`)

	snap, err := alertsource.ParseUkraineAlarm(body, oblast)
	if err != nil {
		t.Fatalf("ParseUkraineAlarm: %v", err)
	}
	for _, uid := range []int{shostkaRaionID, sumyRaionUID} {
		st, ok := snap.Location(uid)
		if !ok || !st.Alert {
			t.Fatalf("UID %d is not raised: %+v", uid, st)
		}
		if st.Level == alertsource.LevelUnknown {
			t.Errorf("UID %d came back at an unknown level from a source that reports levels", uid)
		}
		if st.Level != alertsource.LevelRed {
			t.Errorf("UID %d level = %q, want red: the lighter level is never the one to guess", uid, st.Level)
		}
	}
}

// The change index is the whole reason this provider is affordable at a 15
// second poll: on a tick where nothing has happened anywhere, one integer is
// read and the region's state is never asked for. It must come back as the
// same picture — a short circuit that answered "no alerts" would end every
// raid at the first quiet poll after it started.
func TestUkraineAlarmChangeIndexReplacesTheStateRequest(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	body := ukraineAlarmFixture(t)
	srv, seen := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, alertsource.UkraineAlarmStatusPath) {
			w.Write([]byte(`{"lastActionIndex": 4242}`))
			return
		}
		w.Write(body)
	})

	c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
	first, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// The first poll of a process goes straight for the state: there is
	// nothing cached for an unchanged index to spare.
	if got := paths(*seen); len(got) != 1 || strings.HasSuffix(got[0], alertsource.UkraineAlarmStatusPath) {
		t.Fatalf("first poll requested %v, want the state alone", got)
	}

	second, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	third, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("third Fetch: %v", err)
	}

	// The second poll learns the index (and re-reads the state, having no
	// index to compare against yet); every poll after it costs the index
	// alone. What must never happen is the index being read on top of a
	// state request that happens anyway.
	got := paths(*seen)
	if len(got) != 4 {
		t.Fatalf("three polls made %d requests (%v); the index must replace the state request, not precede it", len(got), got)
	}
	if !strings.HasSuffix(got[3], alertsource.UkraineAlarmStatusPath) {
		t.Errorf("the third poll requested %q, want the change index alone", got[3])
	}

	raion, ok := third.Location(shostkaRaionID)
	if !ok || !raion.Alert || raion.Level != alertsource.LevelRed {
		t.Fatalf("an unchanged index lost the alert: %+v", raion)
	}
	if st, _ := first.Location(shostkaRaionID); st != raion {
		t.Errorf("an unchanged index returned a different state: %+v vs %+v", raion, st)
	}
	// Re-parsed rather than handed back, so one caller's snapshot cannot be
	// written through by another's.
	second.States["Шосткинський район"] = alertsource.RegionState{}
	if st, _ := third.Location(shostkaRaionID); !st.Alert {
		t.Error("two snapshots share one map; writing to one emptied the other")
	}
}

// An index that moved means the state has to be read again, or the device
// would announce yesterday's picture forever.
func TestUkraineAlarmFetchesAgainWhenTheIndexMoves(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	index := 1
	calm := []byte(`[{"regionId":"1400","regionType":"State","activeAlerts":[]}]`)
	srv, seen := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, alertsource.UkraineAlarmStatusPath) {
			w.Write([]byte(`{"lastActionIndex": ` + strconv.Itoa(index) + `}`))
			return
		}
		if index > 1 {
			w.Write(ukraineAlarmFixture(t))
			return
		}
		w.Write(calm)
	})

	c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
	for i := 0; i < 2; i++ {
		if _, err := c.Fetch(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i+1, err)
		}
	}
	index = 2

	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("the poll after the index moved: %v", err)
	}
	st, ok := snap.Location(shostkaRaionID)
	if !ok || !st.Alert {
		t.Fatalf("the index moved and the state was not read again: %+v (requests %v)", st, paths(*seen))
	}
}

// A failure to read the index is not "nothing changed". A cheap endpoint
// breaking must never be the reason a device stops learning about a raid.
func TestUkraineAlarmAnUnreadableIndexStillFetchesTheState(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	statusBroken := false
	srv, seen := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, alertsource.UkraineAlarmStatusPath) {
			if statusBroken {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("<html>gateway</html>"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"lastActionIndex": 7}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(ukraineAlarmFixture(t))
	})

	c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
	// Two polls to get an index stored, so that the third could short-circuit
	// if the broken index were read as "unchanged".
	for i := 0; i < 2; i++ {
		if _, err := c.Fetch(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i+1, err)
		}
	}
	statusBroken = true
	before := len(*seen)

	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("a poll with an unreadable index failed outright: %v", err)
	}
	asked := paths(*seen)[before:]
	if len(asked) != 2 || strings.HasSuffix(asked[1], alertsource.UkraineAlarmStatusPath) {
		t.Errorf("a poll with an unreadable index asked %v, want the index then the state", asked)
	}
	if st, ok := snap.Location(shostkaRaionID); !ok || !st.Alert {
		t.Errorf("the state was not read after the index could not be: %+v", st)
	}
}

// A place this build has no id for is a place this source cannot be asked
// about. Failing before any request is made is what lets the cascade move on
// to a source that can answer, and it is a different fault from every other
// one on this screen: nothing is wrong with the key or the network.
func TestUkraineAlarmWithoutAMappingMakesNoRequest(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, map[int]string{})

	srv, seen := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a source with no region id reached the network")
		w.WriteHeader(http.StatusNotFound)
	})

	c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
	_, attempts, err := c.FetchCascade(context.Background())
	if !errors.Is(err, alertsource.ErrNoRegionMapping) {
		t.Fatalf("Fetch with no mapping = %v, want ErrNoRegionMapping", err)
	}
	if len(*seen) != 0 {
		t.Errorf("requests = %v, want none", paths(*seen))
	}
	if len(attempts) != 1 || attempts[0].Reason() != alertsource.ReasonNoRegionMapping {
		t.Errorf("attempts = %+v, want one reported as %q", attempts, alertsource.ReasonNoRegionMapping)
	}
}

// Without a key there is nothing to ask with, and asking anyway would spend a
// request to be told what we already knew.
func TestUkraineAlarmFetchWithoutAKeyMakesNoRequest(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	srv, seen := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a keyless source reached the network")
		w.WriteHeader(http.StatusUnauthorized)
	})

	c := alertsource.NewUkraineAlarm(srv.URL, "", shostkaUID, "Сумська область", srv.Client())
	if c.HasToken() {
		t.Error("a source built with no key reports that it has one")
	}
	if _, err := c.Fetch(context.Background()); !errors.Is(err, alertsource.ErrNoToken) {
		t.Fatalf("Fetch with no key = %v, want ErrNoToken", err)
	}
	if len(*seen) != 0 {
		t.Errorf("requests = %v, want none", paths(*seen))
	}
}

// The key is a credential. It must not reach a log line, a wrapped error, or
// the page an operator is reading when the fetch fails — and a rejected key is
// exactly the moment somebody is tempted to print it "to check".
func TestUkraineAlarmErrorsNeverCarryTheKey(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusTooManyRequests} {
		srv, _ := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			// An upstream that echoes the key back in its own error body
			// must not get it onto our side either.
			w.Write([]byte(`{"error":"bad token ` + testKey + `"}`))
		})

		c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
		_, err := c.Fetch(context.Background())
		if err == nil {
			t.Fatalf("http %d did not fail", status)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("the error for http %d contains the key: %v", status, err)
		}
	}
}

// A rejected key and a 429 have to arrive as the sentinels the cascade's
// vocabulary is built on, or the dashboard tells an operator to fix the wrong
// thing.
func TestUkraineAlarmClassifiesItsFailures(t *testing.T) {
	alertsource.SetUkraineAlarmRegions(t, testRegions())

	for _, tc := range []struct {
		status int
		want   alertsource.Reason
	}{
		{http.StatusUnauthorized, alertsource.ReasonTokenRejected},
		{http.StatusTooManyRequests, alertsource.ReasonRateLimited},
		{http.StatusInternalServerError, alertsource.ReasonBadResponse},
	} {
		srv, _ := serveUkraineAlarm(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"error":"no"}`))
		})
		c := alertsource.NewUkraineAlarm(srv.URL, testKey, shostkaUID, "Сумська область", srv.Client())
		_, attempts, err := c.FetchCascade(context.Background())
		if err == nil {
			t.Fatalf("http %d did not fail", tc.status)
		}
		if len(attempts) != 1 {
			t.Fatalf("http %d produced %d attempts, want one", tc.status, len(attempts))
		}
		if got := attempts[0].Reason(); got != tc.want {
			t.Errorf("http %d is reported as %q, want %q", tc.status, got, tc.want)
		}
		if attempts[0].Key != alertsource.UkraineAlarmKey {
			t.Errorf("the attempt names %q, want the cascade key %q", attempts[0].Key, alertsource.UkraineAlarmKey)
		}
	}
}
