package alertsource

//go:generate go run locations_gen.go

import "sort"

// Kind is how coarse a location is. The order matters: a larger Kind
// contains a smaller one, and coarsen relies on that to walk upwards.
type Kind int

const (
	// KindOblast is an oblast, and KindSpecialCity a city that announces its
	// own alerts independently of the oblast around it — Kyiv and
	// Sevastopol. They sit at the same level because neither has a parent:
	// a special-status city is not inside an oblast for alert purposes,
	// which is the whole reason it has its own UID.
	KindOblast Kind = iota
	KindSpecialCity
	KindRaion
	KindHromada
)

// Location is one entry in the published alerts.in.ua location table.
//
// Parent is the UID of the location immediately containing this one, or 0
// for an oblast or special-status city. The published sheet has no parent
// column — the hierarchy is implied by row order — so the links here were
// derived once, at generation time, by locations_gen.go, which refuses to
// emit a table whose parents do not hold together. See its comments; a
// wrong parent means a device announcing for the wrong oblast.
type Location struct {
	UID  int
	Name string
	// Label is what the picker shows. It is Name, except where Name alone
	// is ambiguous across the country — several oblasts have a hromada of
	// the same name — in which case the generator qualifies it with the
	// raion.
	Label  string
	Kind   Kind
	Parent int
}

var locationByUID = func() map[int]Location {
	m := make(map[int]Location, len(locations))
	for _, l := range locations {
		m[l.UID] = l
	}
	return m
}()

// LookupLocation returns the location with this UID.
func LookupLocation(uid int) (Location, bool) {
	l, ok := locationByUID[uid]
	return l, ok
}

// Locations returns every known location, ordered for a picker: oblasts and
// special-status cities by name, and under each, its raions and their
// hromadas.
func Locations() []Location {
	out := make([]Location, len(locations))
	copy(out, locations)
	return out
}

// KnownLocation reports whether uid names a location this build knows.
// Anything an operator can choose is validated against the table it was
// chosen from, the same way every other operator-choosable field here is.
func KnownLocation(uid int) bool {
	_, ok := locationByUID[uid]
	return ok
}

// OblastOf walks upwards until it reaches the oblast (or special-status
// city) containing l, and returns it.
//
// This is what makes a cascade of sources with different granularity work.
// alerts.in.ua understands every level; the six Ubilling keys understand
// oblasts and nothing else. An operator who picks a hromada therefore has a
// cascade whose first source can serve it and whose fallbacks cannot.
//
// They broaden to the containing oblast rather than being skipped. A cascade
// that skips its way to empty leaves the device silent, and silence is the
// one outcome this project refuses — see CLAUDE.md. Broadening announces
// MORE than the operator asked for, never less, which is the safe direction
// but is also a surprise worth stating on the page rather than leaving to be
// discovered during a raid in the next district.
//
// A location whose parent chain is broken returns itself and false. That
// cannot happen with the generated table — locations_gen.go verifies every
// chain terminates — but a caller must not silently monitor the wrong place
// if it ever does.
func OblastOf(l Location) (Location, bool) {
	seen := 0
	for l.Parent != 0 {
		parent, ok := locationByUID[l.Parent]
		if !ok {
			return l, false
		}
		l = parent
		// The table is a tree, so this cannot loop; the bound is here so a
		// future hand-edit that makes it a cycle hangs no request.
		if seen++; seen > len(locations) {
			return l, false
		}
	}
	if l.Kind != KindOblast && l.Kind != KindSpecialCity {
		return l, false
	}
	return l, true
}

// SortLocations orders locations for display: by the oblast they belong to,
// then coarsest first within it, then by label.
func SortLocations(ls []Location) {
	sort.SliceStable(ls, func(i, j int) bool {
		oi, _ := OblastOf(ls[i])
		oj, _ := OblastOf(ls[j])
		if oi.Label != oj.Label {
			return oi.Label < oj.Label
		}
		if ls[i].Kind != ls[j].Kind {
			return ls[i].Kind < ls[j].Kind
		}
		return ls[i].Label < ls[j].Label
	})
}
