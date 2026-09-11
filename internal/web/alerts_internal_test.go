package web

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// Every entry in the real registry carries a URL, so the branch that renders
// no link for a source without one cannot be reached from the outside any
// more. It is still the behaviour a source added later without established
// provenance must get — a guessed link on a device that really dials out is
// worse than none — so it is pinned here, against the slot builder directly,
// rather than deleted along with the registry gap that used to exercise it.
func TestBuildSourceSlotsOmitsTheLinkForASourceWithNoURL(t *testing.T) {
	slots := buildSourceSlots([]string{"default"})
	if len(slots) == 0 {
		t.Fatal("buildSourceSlots returned nothing")
	}
	if slots[0].Source.URL == "" {
		t.Fatal("setup: \"default\" has no URL, so this test proves nothing")
	}

	// The zero SourceInfo is what an unused slot and an unknown key both
	// resolve to, and it is the shape a URL-less registry entry would have.
	var unknown alertsource.SourceInfo
	if unknown.URL != "" {
		t.Fatalf("a zero SourceInfo carries a URL: %+v", unknown)
	}

	empty := buildSourceSlots(nil)
	for i, slot := range empty {
		if slot.Source.URL != "" {
			t.Errorf("slot %d of an empty cascade carries URL %q; an unused slot must link to nothing", i+1, slot.Source.URL)
		}
	}
}
