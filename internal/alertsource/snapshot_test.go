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
