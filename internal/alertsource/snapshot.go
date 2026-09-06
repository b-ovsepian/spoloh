// Package alertsource fetches and parses air raid alert snapshots from
// upstream providers. The Ubilling endpoint needs no key and no registration,
// which is what lets a Spoloh device work with no account behind it.
package alertsource

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// tsLayout is the wall-clock format used by every upstream timestamp.
const tsLayout = "2006-01-02 15:04:05"

// Kyiv returns the location every upstream timestamp is expressed in.
//
// It is resolved on first use rather than in an init(). cmd/spoloh imports
// time/tzdata to embed the zone database for the minimal OS images that have
// no /usr/share/zoneinfo, but package initialisation order puts an init() here
// before tzdata has registered it — so the lookup would fail on exactly the
// images that import exists for, and the fixed +2 fallback would silently win.
// That is an hour wrong from late March to late October. By first use, every
// package's initialisation is long finished.
//
// The fallback remains for a genuinely missing database: +2 is right for
// winter and an hour out for summer, which still beats refusing to read a
// timestamp at all.
var Kyiv = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Kyiv"); err == nil {
		return loc
	}
	return time.FixedZone("EET", 2*3600)
})

// RegionState is one oblast's alert state at a point in time.
type RegionState struct {
	Region  string
	Alert   bool
	Changed time.Time // zero when upstream has never observed a change
}

// Snapshot is one complete upstream response.
type Snapshot struct {
	Source   string
	CachedAt time.Time
	States   map[string]RegionState
}

// Region looks up one oblast by its upstream key.
func (s Snapshot) Region(name string) (RegionState, bool) {
	st, ok := s.States[name]
	return st, ok
}

type wireSnapshot struct {
	Source   string `json:"source"`
	CachedAt string `json:"cachedat"`
	States   map[string]struct {
		AlertNow bool   `json:"alertnow"`
		Changed  string `json:"changed"`
	} `json:"states"`
}

// Parse converts an upstream body into a Snapshot. Structural decoding and the
// presence of states are strict: invalid JSON or an empty states object is an
// error. Timestamps, however, are parsed leniently and fall back to zero time
// on error, because they are display-only and must never cause a snapshot with
// valid alert booleans to be discarded. Consequently, a malformed timestamp
// is indistinguishable from the "never observed" sentinel; this trade-off
// preserves the most critical data (alertnow) for a safety-critical device.
func Parse(data []byte) (Snapshot, error) {
	var w wireSnapshot
	if err := json.Unmarshal(data, &w); err != nil {
		return Snapshot{}, fmt.Errorf("parse snapshot: %w", err)
	}
	if len(w.States) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot carries no states")
	}

	snap := Snapshot{
		Source: w.Source,
		States: make(map[string]RegionState, len(w.States)),
	}
	if w.CachedAt != "" {
		if t, err := time.ParseInLocation(tsLayout, w.CachedAt, Kyiv()); err == nil {
			snap.CachedAt = t
		}
	}
	for name, st := range w.States {
		rs := RegionState{Region: name, Alert: st.AlertNow}
		// Upstream emits "1970-01-01 03:00:00" — the Unix epoch in Kyiv local
		// time — to mean "never observed". Testing the year rather than a
		// precomputed instant keeps this correct whichever historical offset
		// the tzdata carries for Kyiv in 1970.
		if t, err := time.ParseInLocation(tsLayout, st.Changed, Kyiv()); err == nil && t.Year() > 1970 {
			rs.Changed = t
		}
		snap.States[name] = rs
	}
	return snap, nil
}

// regions is the fallback oblast list, captured from the live endpoint on
// 2026-09-06. It seeds the dashboard before the first successful poll; the live
// snapshot's keys take over as soon as one arrives, so an upstream rename does
// not strand the operator.
var regions = []string{
	"Вінницька область", "Волинська область", "Дніпропетровська область",
	"Донецька область", "Житомирська область", "Закарпатська область",
	"Запорізька область", "Івано-Франківська область", "Київська область",
	"Кіровоградська область", "Луганська область", "Львівська область",
	"Миколаївська область", "Одеська область", "Полтавська область",
	"Рівненська область", "Сумська область", "Тернопільська область",
	"Харківська область", "Херсонська область", "Хмельницька область",
	"Черкаська область", "Чернівецька область", "Чернігівська область",
	"м. Київ",
}

// Regions returns a copy of the fallback oblast list.
func Regions() []string {
	out := make([]string, len(regions))
	copy(out, regions)
	return out
}
