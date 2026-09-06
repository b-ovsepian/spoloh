package alertsource_test

import (
	"os"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/alertsource"
)

func kyiv(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/ubilling.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

func TestParseFixture(t *testing.T) {
	snap, err := alertsource.Parse(fixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if snap.Source != "Mørk Skogen API (default)" {
		t.Fatalf("source = %q", snap.Source)
	}
	if len(snap.States) != 5 {
		t.Fatalf("states = %d, want 5", len(snap.States))
	}

	kh, ok := snap.Region("Харківська область")
	if !ok {
		t.Fatal("Харківська область missing")
	}
	if !kh.Alert {
		t.Fatal("Харківська область should be alerting")
	}
	want := time.Date(2026, 9, 6, 7, 32, 8, 0, kyiv(t))
	if !kh.Changed.Equal(want) {
		t.Fatalf("changed = %v, want %v", kh.Changed, want)
	}
}

func TestParseEpochSentinelBecomesZeroTime(t *testing.T) {
	snap, err := alertsource.Parse(fixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	lv, ok := snap.Region("Львівська область")
	if !ok {
		t.Fatal("Львівська область missing")
	}
	if !lv.Changed.IsZero() {
		t.Fatalf("epoch sentinel must become the zero time, got %v", lv.Changed)
	}
}

func TestParseUnknownRegion(t *testing.T) {
	snap, err := alertsource.Parse(fixture(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := snap.Region("Атлантида"); ok {
		t.Fatal("unknown region should not resolve")
	}
}

func TestParseRejectsNonJSON(t *testing.T) {
	if _, err := alertsource.Parse([]byte("<html>429 Too Many Requests</html>")); err == nil {
		t.Fatal("expected an error for an HTML body")
	}
}

func TestParseRejectsEmptyStates(t *testing.T) {
	if _, err := alertsource.Parse([]byte(`{"source":"x","cachedat":"2026-09-06 15:39:05","states":{}}`)); err == nil {
		t.Fatal("an empty states object means a broken upstream, not a quiet country")
	}
}

func TestRegionsFallbackList(t *testing.T) {
	regions := alertsource.Regions()
	if len(regions) != 25 {
		t.Fatalf("regions = %d, want 25", len(regions))
	}
	found := false
	for _, r := range regions {
		if r == "м. Київ" {
			found = true
		}
	}
	if !found {
		t.Fatal("м. Київ must be in the fallback list")
	}
}

func TestParseMalformedChangedFallsBackToZeroTime(t *testing.T) {
	// Regression test: malformed timestamp in changed must not discard the snapshot.
	// The Alert boolean must still be usable even if Changed cannot be parsed.
	data := []byte(`{
		"source": "test",
		"cachedat": "2026-09-06 15:39:05",
		"states": {
			"Київська область": { "alertnow": true, "changed": "not-a-timestamp" },
			"Львівська область": { "alertnow": false, "changed": "also-invalid" }
		}
	}`)
	snap, err := alertsource.Parse(data)
	if err != nil {
		t.Fatalf("Parse should succeed with malformed timestamps: %v", err)
	}
	if len(snap.States) != 2 {
		t.Fatalf("states = %d, want 2", len(snap.States))
	}

	kv, ok := snap.Region("Київська область")
	if !ok {
		t.Fatal("Київська область missing")
	}
	if !kv.Alert {
		t.Fatal("Alert boolean must be preserved even with malformed changed")
	}
	if !kv.Changed.IsZero() {
		t.Fatalf("malformed changed must become zero time, got %v", kv.Changed)
	}

	lv, ok := snap.Region("Львівська область")
	if !ok {
		t.Fatal("Львівська область missing")
	}
	if lv.Alert {
		t.Fatal("Alert boolean must be false")
	}
	if !lv.Changed.IsZero() {
		t.Fatalf("malformed changed must become zero time, got %v", lv.Changed)
	}
}

func TestParseMalformedCachedAtFallsBackToZeroTime(t *testing.T) {
	// Regression test: malformed cachedat must not discard the snapshot or its states.
	data := []byte(`{
		"source": "test",
		"cachedat": "not-a-valid-timestamp",
		"states": {
			"Харківська область": { "alertnow": true, "changed": "2026-09-06 07:32:08" },
			"м. Київ": { "alertnow": false, "changed": "2026-09-06 02:44:20" }
		}
	}`)
	snap, err := alertsource.Parse(data)
	if err != nil {
		t.Fatalf("Parse should succeed with malformed cachedat: %v", err)
	}
	if !snap.CachedAt.IsZero() {
		t.Fatalf("malformed cachedat must become zero time, got %v", snap.CachedAt)
	}
	if len(snap.States) != 2 {
		t.Fatalf("states = %d, want 2", len(snap.States))
	}

	kh, ok := snap.Region("Харківська область")
	if !ok {
		t.Fatal("Харківська область missing")
	}
	if !kh.Alert {
		t.Fatal("Харківська область should be alerting")
	}
	want := time.Date(2026, 9, 6, 7, 32, 8, 0, kyiv(t))
	if !kh.Changed.Equal(want) {
		t.Fatalf("valid changed must be parsed correctly, got %v want %v", kh.Changed, want)
	}
}

// Europe/Kyiv is EET (+2) in winter and EEST (+3) in summer. Resolving the
// location in a package-level init() lost a race with time/tzdata's own
// initialisation, so on an image with no /usr/share/zoneinfo the fixed +2
// fallback won silently and every summer timestamp was an hour out.
func TestKyivIsTheRealZoneNotAFixedOffset(t *testing.T) {
	if got := alertsource.Kyiv().String(); got != "Europe/Kyiv" {
		t.Fatalf("Kyiv() = %q, want Europe/Kyiv", got)
	}

	body := []byte(`{"source":"t","cachedat":"2026-07-01 12:00:00",
		"states":{"м. Київ":{"alertnow":true,"changed":"2026-07-01 12:00:00"}}}`)
	snap, err := alertsource.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	st, _ := snap.Region("м. Київ")
	if want := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC); !st.Changed.Equal(want) {
		t.Fatalf("a summer timestamp parsed as %v, want %v (Kyiv is +3 in July, not +2)", st.Changed.UTC(), want)
	}
}
