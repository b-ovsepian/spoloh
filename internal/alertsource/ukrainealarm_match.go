package alertsource

import (
	"fmt"
	"strings"
)

// The v2RegionType values this matching understands, from the API's own spec
// (docs/ukrainealarm-swagger.json). "Null" is in that enum too and names
// nothing, so it is not matched against anything.
const (
	ukraineAlarmTypeState         = "State"
	ukraineAlarmTypeDistrict      = "District"
	ukraineAlarmTypeCommunity     = "Community"
	ukraineAlarmTypeCityOrVillage = "CityOrVillage"
	ukraineAlarmTypeCityDistrict  = "CityDistrict"
)

// UkraineAlarmRegionView is one node of GET /api/v3/regions — a tree, not a
// flat list, which is the shape the id mapping has to be derived from.
type UkraineAlarmRegionView struct {
	RegionID   string                   `json:"regionId"`
	RegionName string                   `json:"regionName"`
	RegionType string                   `json:"regionType"`
	Children   []UkraineAlarmRegionView `json:"regionChildIds"`
}

// UkraineAlarmMapping is one row of the generated table: a published
// alerts.in.ua location and the ukrainealarm id for the same place.
type UkraineAlarmMapping struct {
	UID      int
	RegionID string
	// Name is the published location's name, carried so a refusal and the
	// generated file can both name the place a person would recognise.
	Name string
}

// MatchUkraineAlarmRegions pairs the upstream region tree with the published
// location table, and refuses on any ambiguity.
//
// It lives in the package rather than inside ukrainealarm_gen.go, which is
// the file that calls it, for one reason: a generator behind a //go:build
// ignore tag cannot be tested, and the rule this implements — refuse rather
// than guess — is the one rule here whose failure is a device announcing for
// somewhere else. A rule like that is not left to a file nothing can exercise.
//
// Scope is what makes the matching safe. A State is matched against the
// table's top-level entries; that State's districts only against the raions
// inside the location it matched; a district's communities only against that
// raion's hromadas. Hromada names repeat all over the country — 73 of them,
// seven inside a single oblast — so a country-wide match on a name would be a
// coin toss dressed as a lookup.
//
// A place with no counterpart gets no row, and that is not a failure: an
// unmapped location is one this source cannot serve, which the cascade
// already handles by broadening to a source that can. Two candidates, on the
// other hand, stops everything with both of them named, because there is no
// way to tell from here which of the two a siren would be announcing for.
func MatchUkraineAlarmRegions(states []UkraineAlarmRegionView) ([]UkraineAlarmMapping, error) {
	children := map[int][]Location{}
	for _, l := range locations {
		children[l.Parent] = append(children[l.Parent], l)
	}

	var out []UkraineAlarmMapping
	// A State's counterpart is an oblast or a special-status city; both are
	// top-level in the published table, which is what "no parent" means there.
	tops := byName(children[0])
	for _, state := range states {
		if state.RegionType != ukraineAlarmTypeState {
			// The spec's type list is wider than what "states" holds in
			// practice. Anything else at the top is a shape this was not
			// written against, and a guess about it would be a guess about
			// every region underneath it.
			return nil, fmt.Errorf("top-level region %q (%s) is a %q, not a %q",
				state.RegionName, state.RegionID, state.RegionType, ukraineAlarmTypeState)
		}
		oblast, err := matchOne(tops, state)
		if err != nil {
			return nil, err
		}
		if oblast == nil {
			// Crimea, and anything else upstream names that the published
			// table does not carry. No row and no error: a place no operator
			// can choose here needs no id.
			continue
		}
		out = append(out, UkraineAlarmMapping{UID: oblast.UID, RegionID: state.RegionID, Name: oblast.Name})

		raions := byName(children[oblast.UID])
		for _, district := range state.Children {
			if district.RegionType != ukraineAlarmTypeDistrict {
				continue
			}
			raion, err := matchOne(raions, district)
			if err != nil {
				return nil, err
			}
			if raion == nil {
				continue
			}
			out = append(out, UkraineAlarmMapping{UID: raion.UID, RegionID: district.RegionID, Name: raion.Name})

			hromadas := byName(children[raion.UID])
			for _, community := range district.Children {
				switch community.RegionType {
				case ukraineAlarmTypeCommunity, ukraineAlarmTypeCityOrVillage, ukraineAlarmTypeCityDistrict:
				default:
					continue
				}
				hromada, err := matchOne(hromadas, community)
				if err != nil {
					return nil, err
				}
				if hromada == nil {
					continue
				}
				out = append(out, UkraineAlarmMapping{UID: hromada.UID, RegionID: community.RegionID, Name: hromada.Name})
			}
		}
	}
	if err := verifyUkraineAlarmMappings(out); err != nil {
		return nil, err
	}
	return out, nil
}

// matchOne finds the one location in scope whose name is this region's, or
// nil when there is none. Two candidates that are telling apart is a refusal,
// not a choice.
//
// The exception is candidates that cannot be told apart at all: identical
// name, identical kind, the same parent. The published sheet has exactly one
// such pair in 1,622 rows — «Миколаївська територіальна громада» under
// Сумський район, listed twice as UID 1180 and UID 1181 — and there is one
// such hromada in that raion. Two rows, one place.
//
// They all map to the region, and that is not a guess: nothing distinguishes
// them, including this build's own label generator, which fell back to
// printing the UID because name and raion were not enough. Whichever of the
// two an operator picked from the list, the device watches the right place.
// Refusing instead would leave a real hromada unservable by this provider
// over a duplicate row in somebody else's spreadsheet.
//
// The refusal keeps its teeth where they matter. Candidates that differ in
// name, kind or parent are different places, one region cannot be both, and
// that is still a refusal — which is the case that would otherwise have a
// device announcing for somewhere else.
func matchOne(scope map[string][]Location, r UkraineAlarmRegionView) (*Location, error) {
	candidates := scope[normaliseName(r.RegionName)]
	switch len(candidates) {
	case 0:
		return nil, nil
	case 1:
		l := candidates[0]
		return &l, nil
	default:
		if l, ok := onlyIndistinguishable(candidates); ok {
			return &l, nil
		}
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			names = append(names, fmt.Sprintf("%q (UID %d)", c.Label, c.UID))
		}
		return nil, fmt.Errorf("region %q (%s, %s) matches %d published locations — %s; "+
			"no table is emitted, because a mis-mapped region is a device announcing for somewhere else",
			r.RegionName, r.RegionID, r.RegionType, len(candidates), strings.Join(names, ", "))
	}
}

// byName groups one level's locations by normalised name, so a name that
// repeats inside a single parent stays visible as two candidates rather than
// quietly becoming one.
func byName(ls []Location) map[string][]Location {
	out := make(map[string][]Location, len(ls))
	for _, l := range ls {
		out[normaliseName(l.Name)] = append(out[normaliseName(l.Name)], l)
	}
	return out
}

// normaliseName is how two spellings of one place are compared: case folded,
// apostrophes made one character, non-breaking spaces made ordinary, runs of
// whitespace collapsed. Nothing more — a normaliser that started dropping
// words would start finding matches, which is the opposite of the point.
func normaliseName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("’", "'", "`", "'", "ʼ", "'", " ", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// verifyUkraineAlarmMappings refuses a table that is not one-to-one.
//
// Two locations sharing one upstream id, or one location claimed by two ids,
// means the matching found something it did not understand. At least one of
// the two rows is wrong and there is no way to tell which from here.
func verifyUkraineAlarmMappings(ms []UkraineAlarmMapping) error {
	byUID := make(map[int]UkraineAlarmMapping, len(ms))
	byRegion := make(map[string]UkraineAlarmMapping, len(ms))
	oblasts := 0
	for _, m := range ms {
		if m.RegionID == "" {
			return fmt.Errorf("%q (UID %d) matched a region with no id", m.Name, m.UID)
		}
		if prev, dup := byUID[m.UID]; dup {
			return fmt.Errorf("UID %d (%q) is claimed by region ids %s and %s",
				m.UID, m.Name, prev.RegionID, m.RegionID)
		}
		if prev, dup := byRegion[m.RegionID]; dup {
			return fmt.Errorf("region id %s is mapped to both %q (UID %d) and %q (UID %d)",
				m.RegionID, prev.Name, prev.UID, m.Name, m.UID)
		}
		byUID[m.UID] = m
		byRegion[m.RegionID] = m

		l, ok := LookupLocation(m.UID)
		if !ok {
			return fmt.Errorf("UID %d is not in the published table", m.UID)
		}
		if l.Kind == KindOblast || l.Kind == KindSpecialCity {
			oblasts++
		}
	}
	// Every fetch from this provider is made for an oblast, so a run that
	// mapped only places below one would emit a table that serves nobody.
	if len(ms) > 0 && oblasts == 0 {
		return fmt.Errorf("no oblast matched at all; this source is asked by oblast, so the table would serve nobody")
	}
	return nil
}

// onlyIndistinguishable reports whether every candidate is the same place
// wearing different UIDs — identical name, kind and parent — and returns the
// one to map to. See matchOne for why that is a mapping rather than a
// refusal.
//
// The returned candidate is the lowest UID, chosen only so the generator is
// deterministic: the whole premise here is that the choice does not matter,
// and a table that changed between runs would be a diff nobody could review.
func onlyIndistinguishable(candidates []Location) (Location, bool) {
	first := candidates[0]
	pick := first
	for _, c := range candidates[1:] {
		if c.Name != first.Name || c.Kind != first.Kind || c.Parent != first.Parent {
			return Location{}, false
		}
		if c.UID < pick.UID {
			pick = c
		}
	}
	return pick, true
}
