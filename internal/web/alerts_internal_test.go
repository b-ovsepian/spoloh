package web

import (
	"strconv"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// offered is the list the page builds its pickers from when no credential is
// stored — the state every device is in until an operator goes and gets a
// token.
var offered = alertsource.SelectableSources()

// sumy is the oblast the slot-builder tests watch, chosen because
// internal/alertsource pins its hromada chain: UID 1198, м. Шостка, under
// Шосткинський район, under Сумська область.
var sumy = watch{Region: "Сумська область"}

// Every entry in the real registry carries a URL, so the branch that renders
// no link for a source without one cannot be reached from the outside any
// more. It is still the behaviour a source added later without established
// provenance must get — a guessed link on a device that really dials out is
// worse than none — so it is pinned here, against the slot builder directly,
// rather than deleted along with the registry gap that used to exercise it.
func TestBuildSourceSlotsOmitsTheLinkForASourceWithNoURL(t *testing.T) {
	slots := buildSourceSlots([]string{"default"}, offered, sumy)
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

	empty := buildSourceSlots(nil, offered, sumy)
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

	got := parseSources(submitted, alertsource.SelectableSources())
	if want := "default,skog"; strings.Join(got, ",") != want {
		t.Errorf("with no token stored, parseSources(%v) = %v, want %q", submitted, got, want)
	}

	// And it is a real key, not a typo: with the credential it is accepted,
	// in the position it was submitted in.
	got = parseSources(submitted, alertsource.SelectableSources(alertsource.AlertsUAKey, alertsource.UkraineAlarmKey))
	if want := "default,alertsua,skog"; strings.Join(got, ",") != want {
		t.Errorf("with a token stored, parseSources(%v) = %v, want %q", submitted, got, want)
	}
}

// The picker is sized from the list it offers, so a source that is not
// offered does not leave an empty slot behind — and a stored cascade longer
// than that list is still shown in full rather than quietly truncated.
func TestBuildSourceSlotsOffersOnlyTheSelectableSources(t *testing.T) {
	slots := buildSourceSlots(nil, offered, sumy)
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
	if slots := buildSourceSlots(longer, offered, sumy); len(slots) != len(longer) {
		t.Errorf("slots = %d for a stored cascade of %d, want every stored position shown", len(slots), len(longer))
	}
}

// The honest half of the location picker, and the case the operator's own
// site makes concrete: choose м. Шостка and alerts.in.ua watches exactly
// that, while every Ubilling key in the same cascade broadens to Сумська
// область. Broadening announces more than was asked for, never less — which
// is the safe direction and precisely why it has to be on the screen rather
// than found out during a raid in the next district.
func TestSourceSlotsSayWhichSourceBroadensToTheOblast(t *testing.T) {
	const (
		shostkaUID   = 1198
		shostkaLabel = "м. Шостка та Шосткинська територіальна громада"
		sumyOblast   = "Сумська область"
	)
	w := watch{Region: sumyOblast, LocationUID: shostkaUID}

	if got := w.forSource(alertsource.AlertsUAKey); got != shostkaLabel {
		t.Errorf("alerts.in.ua watches %q, want the chosen hromada %q", got, shostkaLabel)
	}
	for _, key := range alertsource.SelectableSources() {
		if got := w.forSource(key); got != sumyOblast {
			t.Errorf("%q watches %q, want it broadened to %q — it reports by oblast and nothing finer", key, got, sumyOblast)
		}
	}

	// And it reaches the slots the page renders, in the position the
	// operator put the source in.
	slots := buildSourceSlots([]string{alertsource.AlertsUAKey, "default"}, alertsource.SelectableSources(alertsource.AlertsUAKey, alertsource.UkraineAlarmKey), w)
	if len(slots) < 2 {
		t.Fatalf("buildSourceSlots returned %d slots, want at least two", len(slots))
	}
	if slots[0].Watches != shostkaLabel {
		t.Errorf("slot 1 (alerts.in.ua) watches %q, want %q", slots[0].Watches, shostkaLabel)
	}
	if slots[1].Watches != sumyOblast {
		t.Errorf("slot 2 (default) watches %q, want %q", slots[1].Watches, sumyOblast)
	}
	// An unused slot claims nothing: there is no source chosen for it to be
	// a claim about.
	last := slots[len(slots)-1]
	if last.Options[0].Selected && last.Watches != "" {
		t.Errorf("an unused slot claims to watch %q", last.Watches)
	}
}

// With no location chosen — what every device ships as, and what every config
// written before the field existed carries — every source watches the oblast,
// and they all say the same thing because they all do the same thing.
func TestSourceSlotsWatchTheOblastWhenNoLocationIsChosen(t *testing.T) {
	w := watch{Region: "Сумська область"}
	for _, key := range alertsource.SelectableSources(alertsource.AlertsUAKey, alertsource.UkraineAlarmKey) {
		if got := w.forSource(key); got != "Сумська область" {
			t.Errorf("%q watches %q with no location chosen, want the oblast", key, got)
		}
	}
}

// A UID no published table lists any more must broaden to the oblast rather
// than name a place this build cannot resolve. Announcing for the whole
// oblast is too much; announcing for nowhere is the outcome this project
// refuses.
func TestAnUnknownLocationBroadensToTheOblast(t *testing.T) {
	w := watch{Region: "Сумська область", LocationUID: 999999}
	if got := w.forSource(alertsource.AlertsUAKey); got != "Сумська область" {
		t.Errorf("an unpublished UID watches %q, want the oblast", got)
	}
}

// The picker offers all four levels of the published table — oblast, special
// city, raion, hromada — because an operator who can only pick an oblast has
// no more than they had before this screen existed.
func TestLocationPickerOffersEveryLevel(t *testing.T) {
	seen := map[alertsource.Kind]int{}
	total := 0
	for _, group := range locationGroups() {
		for _, opt := range group.Locations {
			l, ok := alertsource.LookupLocation(opt.UID)
			if !ok {
				t.Fatalf("the picker offers UID %d, which is in no published table", opt.UID)
			}
			seen[l.Kind]++
			total++
		}
	}
	for _, kind := range []alertsource.Kind{alertsource.KindOblast, alertsource.KindSpecialCity, alertsource.KindRaion, alertsource.KindHromada} {
		if seen[kind] == 0 {
			t.Errorf("the picker offers nothing at level %v", kind)
		}
	}
	if total < len(alertsource.Locations())/2 {
		t.Errorf("the picker offers %d of %d published locations; something is filtering the table out", total, len(alertsource.Locations()))
	}
}

// Every entry the picker offers must be one a submission can survive: its
// oblast has to be one the region <select> offers, or choosing it is a dead
// end the operator only discovers on save.
func TestLocationPickerOffersNothingItWouldRefuse(t *testing.T) {
	offeredRegions := map[string]bool{}
	for _, r := range alertsource.Regions() {
		offeredRegions[r] = true
	}
	for _, group := range locationGroups() {
		for _, opt := range group.Locations {
			l, _ := alertsource.LookupLocation(opt.UID)
			oblast, ok := alertsource.OblastOf(l)
			if !ok || !offeredRegions[oblast.Name] {
				t.Errorf("the picker offers %q (UID %d), whose oblast %q the region picker does not", opt.Label, opt.UID, oblast.Name)
			}
			if _, msg := parseLocationUID(strconv.Itoa(opt.UID), oblast.Name); msg != "" {
				t.Errorf("the picker offers %q (UID %d) but the form refuses it: %s", opt.Label, opt.UID, msg)
			}
		}
	}
}
