package alertsource_test

import (
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// state builds one upstream State node with the children given.
func state(id, name string, children ...alertsource.UkraineAlarmRegionView) alertsource.UkraineAlarmRegionView {
	return alertsource.UkraineAlarmRegionView{RegionID: id, RegionName: name, RegionType: "State", Children: children}
}

func district(id, name string, children ...alertsource.UkraineAlarmRegionView) alertsource.UkraineAlarmRegionView {
	return alertsource.UkraineAlarmRegionView{RegionID: id, RegionName: name, RegionType: "District", Children: children}
}

func community(id, name string) alertsource.UkraineAlarmRegionView {
	return alertsource.UkraineAlarmRegionView{RegionID: id, RegionName: name, RegionType: "Community"}
}

// mapped turns the result into "UID -> region id" for readable assertions.
func mapped(ms []alertsource.UkraineAlarmMapping) map[int]string {
	out := make(map[int]string, len(ms))
	for _, m := range ms {
		out[m.UID] = m.RegionID
	}
	return out
}

// The ordinary case, one whole branch of it: an oblast, a raion inside it and
// a hromada inside that, each matched to the published table by name and
// level.
func TestMatchUkraineAlarmRegionsMapsAnOblastBranch(t *testing.T) {
	ms, err := alertsource.MatchUkraineAlarmRegions([]alertsource.UkraineAlarmRegionView{
		state("1400", "Сумська область",
			district("1415", "Шосткинський район",
				community("1498", "м. Шостка та Шосткинська територіальна громада"),
			),
		),
	})
	if err != nil {
		t.Fatalf("MatchUkraineAlarmRegions: %v", err)
	}
	got := mapped(ms)
	for uid, want := range map[int]string{20: "1400", 115: "1415", 1198: "1498"} {
		if got[uid] != want {
			t.Errorf("UID %d mapped to %q, want %q (all: %v)", uid, got[uid], want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("mapped %d places, want exactly the three named: %v", len(got), got)
	}
}

// The check the generator exists for. The published table holds two hromadas
// called «Миколаївська територіальна громада» inside Сумський район alone, so
// a name that lands on both is a name that cannot be resolved — and resolving
// it by picking one is a device announcing for whichever of the two nobody
// chose. Nothing is emitted at all: a table with one wrong row in it is worse
// than no table, because the cascade knows what to do with a source that
// The published sheet lists «Миколаївська територіальна громада» twice under
// Сумський район, as UID 1180 and UID 1181 — the only pair in 1,622 rows that
// nothing distinguishes, and there is one such hromada in that raion. Two
// rows, one place.
//
// It maps rather than refusing. Nothing tells the two apart, including this
// build's own label generator, which fell back to printing the UID because
// name and raion were not enough — so whichever the operator picked, the
// device watches the right place. Refusing would leave a real hromada
// unservable by this provider over a duplicate row in somebody else's
// spreadsheet.
func TestMatchUkraineAlarmRegionsMapsAnIndistinguishableDuplicate(t *testing.T) {
	ms, err := alertsource.MatchUkraineAlarmRegions([]alertsource.UkraineAlarmRegionView{
		state("1400", "Сумська область",
			district("1414", "Сумський район",
				community("1481", "Миколаївська територіальна громада"),
			),
		),
	})
	if err != nil {
		t.Fatalf("a duplicate nothing can tell apart was refused: %v", err)
	}
	var got []int
	for _, m := range ms {
		if m.RegionID == "1481" {
			got = append(got, m.UID)
		}
	}
	if len(got) != 1 {
		t.Fatalf("region 1481 mapped to %v; want exactly one of the duplicate UIDs", got)
	}
	if got[0] != 1180 && got[0] != 1181 {
		t.Errorf("region 1481 mapped to UID %d, which is neither of the duplicate rows", got[0])
	}
}

// A place with no counterpart is not a failure: it means this source cannot
// serve that location, which the cascade already handles by broadening to one
// that can. The rest of the tree still maps.
func TestMatchUkraineAlarmRegionsEmitsNothingForAPlaceWithNoCounterpart(t *testing.T) {
	ms, err := alertsource.MatchUkraineAlarmRegions([]alertsource.UkraineAlarmRegionView{
		state("1400", "Сумська область",
			district("9999", "Район, якого немає в таблиці"),
			district("1415", "Шосткинський район"),
		),
	})
	if err != nil {
		t.Fatalf("a place with no counterpart stopped the run: %v", err)
	}
	got := mapped(ms)
	if got[20] != "1400" || got[115] != "1415" {
		t.Errorf("the places that do exist did not map: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("mapped %d places, want the two that exist: %v", len(got), got)
	}
}

// Two upstream ids for one published place is the other way the matching can
// be wrong, and there is no way to tell from here which of the two a fetch
// should use. It is refused for the same reason an ambiguous name is.
func TestMatchUkraineAlarmRegionsRefusesTwoIdsForOnePlace(t *testing.T) {
	ms, err := alertsource.MatchUkraineAlarmRegions([]alertsource.UkraineAlarmRegionView{
		state("1400", "Сумська область"),
		state("1401", "Сумська  область"),
	})
	if err == nil {
		t.Fatalf("one place was given two region ids: %v", mapped(ms))
	}
	if !strings.Contains(err.Error(), "1400") || !strings.Contains(err.Error(), "1401") {
		t.Errorf("the refusal does not name both ids: %v", err)
	}
}

// Anything but a State at the top level is a response shape this client was
// not written against, and a guess about it would be a guess about every
// region underneath it.
func TestMatchUkraineAlarmRegionsRefusesANonStateAtTheTop(t *testing.T) {
	if _, err := alertsource.MatchUkraineAlarmRegions([]alertsource.UkraineAlarmRegionView{
		district("1415", "Шосткинський район"),
	}); err == nil {
		t.Fatal("a district at the top level was accepted as an oblast")
	}
}
