package web

import (
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// offered is the list the page builds its pickers from when no credential is
// stored — the state every device is in until an operator goes and gets a
// token.
var offered = alertsource.SelectableSources(false)

// Every entry in the real registry carries a URL, so the branch that renders
// no link for a source without one cannot be reached from the outside any
// more. It is still the behaviour a source added later without established
// provenance must get — a guessed link on a device that really dials out is
// worse than none — so it is pinned here, against the slot builder directly,
// rather than deleted along with the registry gap that used to exercise it.
func TestBuildSourceSlotsOmitsTheLinkForASourceWithNoURL(t *testing.T) {
	slots := buildSourceSlots([]string{"default"}, offered)
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

	empty := buildSourceSlots(nil, offered)
	for i, slot := range empty {
		if slot.Source.URL != "" {
			t.Errorf("slot %d of an empty cascade carries URL %q; an unused slot must link to nothing", i+1, slot.Source.URL)
		}
	}
}

// A source whose credential this device does not hold is not a source it can
// use. Letting one through the form would let an operator build a cascade of
// slots that all fail to authenticate — a device that hears about no air raid
// while the dashboard shows a complete, ordered list.
func TestParseSourcesDropsASourceWhoseCredentialIsNotStored(t *testing.T) {
	submitted := []string{"default", alertsource.AlertsUAKey, "skog"}

	got := parseSources(submitted, alertsource.SelectableSources(false))
	if want := "default,skog"; strings.Join(got, ",") != want {
		t.Errorf("with no token stored, parseSources(%v) = %v, want %q", submitted, got, want)
	}

	// And it is a real key, not a typo: with the credential it is accepted,
	// in the position it was submitted in.
	got = parseSources(submitted, alertsource.SelectableSources(true))
	if want := "default,alertsua,skog"; strings.Join(got, ",") != want {
		t.Errorf("with a token stored, parseSources(%v) = %v, want %q", submitted, got, want)
	}
}

// The picker is sized from the list it offers, so a source that is not
// offered does not leave an empty slot behind — and a stored cascade longer
// than that list is still shown in full rather than quietly truncated.
func TestBuildSourceSlotsOffersOnlyTheSelectableSources(t *testing.T) {
	slots := buildSourceSlots(nil, offered)
	if len(slots) != len(offered) {
		t.Errorf("slots = %d, want one per selectable source (%d)", len(slots), len(offered))
	}
	for _, slot := range slots {
		for _, opt := range slot.Options {
			if opt.Key == alertsource.AlertsUAKey {
				t.Fatalf("slot %d offers %q with no token stored", slot.Position, opt.Key)
			}
		}
	}

	longer := make([]string, len(offered)+2)
	copy(longer, offered)
	if slots := buildSourceSlots(longer, offered); len(slots) != len(longer) {
		t.Errorf("slots = %d for a stored cascade of %d, want every stored position shown", len(slots), len(longer))
	}
}
