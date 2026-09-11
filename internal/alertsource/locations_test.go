package alertsource_test

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// The operator's own location, and the reason the hromada level exists at
// all. If this chain is wrong the device announces for somewhere else.
func TestShostkaResolvesToSumyOblast(t *testing.T) {
	shostka, ok := alertsource.LookupLocation(1198)
	if !ok {
		t.Fatal("UID 1198 is not in the table")
	}
	if shostka.Kind != alertsource.KindHromada {
		t.Errorf("UID 1198 kind = %v, want a hromada", shostka.Kind)
	}
	if shostka.Name != "м. Шостка та Шосткинська територіальна громада" {
		t.Errorf("UID 1198 name = %q", shostka.Name)
	}

	oblast, ok := alertsource.OblastOf(shostka)
	if !ok {
		t.Fatal("Шостка does not resolve to an oblast")
	}
	if oblast.Name != "Сумська область" {
		t.Errorf("Шостка resolves to %q, want Сумська область", oblast.Name)
	}
}

// Every location must reach an oblast. A broken chain is how a device ends
// up monitoring somewhere nobody chose, and the table is generated rather
// than hand-written precisely because that cannot be spot-checked by eye.
func TestEveryLocationReachesAnOblast(t *testing.T) {
	for _, l := range alertsource.Locations() {
		if _, ok := alertsource.OblastOf(l); !ok {
			t.Errorf("%s (UID %d) does not resolve to an oblast", l.Name, l.UID)
		}
	}
}

// An oblast and a special-status city are their own oblast: Kyiv announces
// independently of Kyiv oblast, which is the reason it holds its own UID.
func TestAnOblastAndASpecialCityAreTheirOwnOblast(t *testing.T) {
	for _, uid := range []int{31, 20} {
		l, ok := alertsource.LookupLocation(uid)
		if !ok {
			t.Fatalf("UID %d is not in the table", uid)
		}
		got, ok := alertsource.OblastOf(l)
		if !ok || got.UID != l.UID {
			t.Errorf("OblastOf(%s) = %v (ok=%v), want itself", l.Name, got.Name, ok)
		}
	}
}

// A UID nobody published must not be selectable. Everything an operator can
// choose is validated against the table it was offered from.
func TestKnownLocationRejectsAnUnpublishedUID(t *testing.T) {
	if alertsource.KnownLocation(999999) {
		t.Error("an unpublished UID is treated as a known location")
	}
	if !alertsource.KnownLocation(1198) {
		t.Error("Шостка is not treated as a known location")
	}
}
