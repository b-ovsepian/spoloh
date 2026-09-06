// Package scheduler fires configured events at their wall-clock times.
package scheduler

import (
	"time"

	"github.com/spoloh/spoloh/internal/clock"
	"github.com/spoloh/spoloh/internal/config"
)

// ISOWeekday returns Monday=1 .. Sunday=7, matching the config format.
func ISOWeekday(t time.Time) int {
	d := int(t.Weekday())
	if d == 0 {
		return 7
	}
	return d
}

// NextFire returns the next time ev is due strictly after `after`, in after's
// own location. It reports false for events that can never fire.
func NextFire(ev config.Event, after time.Time) (time.Time, bool) {
	if ev.Trigger != config.TriggerScheduled || len(ev.Weekdays) == 0 {
		return time.Time{}, false
	}
	minutes, err := config.ParseHM(ev.Time)
	if err != nil {
		return time.Time{}, false
	}

	allowed := make(map[int]bool, len(ev.Weekdays))
	for _, d := range ev.Weekdays {
		allowed[d] = true
	}

	loc := after.Location()
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
	// Eight days covers any weekday set, including one that only allows the
	// same weekday we are already past.
	for i := 0; i < 8; i++ {
		candidate := time.Date(day.Year(), day.Month(), day.Day()+i, minutes/60, minutes%60, 0, 0, loc)
		if !candidate.After(after) {
			continue
		}
		if allowed[ISOWeekday(candidate)] {
			return candidate, true
		}
	}
	return time.Time{}, false
}

// Runner fires due events on each tick. It is driven from a single goroutine.
type Runner struct {
	sync clock.SyncChecker
	cfg  func() *config.Config
	fire func(config.Event, time.Time)

	// lastFired records which wall-clock minutes each event has already fired in.
	// Key is "event-id:2006-01-02 15:04", ensuring both occurrences of a repeated
	// hour on a DST fall-back day map to the same minute and fire only once.
	lastFired map[string]bool
}

func NewRunner(sync clock.SyncChecker, cfg func() *config.Config, fire func(config.Event, time.Time)) *Runner {
	return &Runner{sync: sync, cfg: cfg, fire: fire, lastFired: map[string]bool{}}
}

// Tick fires every event due in now's minute.
func (r *Runner) Tick(now time.Time) {
	// Without a synchronised clock the device does not know what time it is,
	// and a wrong time is worse than no announcement.
	if r.sync != nil && !r.sync.Synced() {
		return
	}
	// Key on wall-clock time, not absolute instant, so that both occurrences of a
	// repeated hour on a DST fall-back day map to the same minute.
	wallClockMinute := now.Format("2006-01-02 15:04")
	cur := now.Hour()*60 + now.Minute()

	for _, ev := range r.cfg().Events {
		if ev.Trigger != config.TriggerScheduled || !ev.Enabled {
			continue
		}
		due, err := config.ParseHM(ev.Time)
		if err != nil || due != cur {
			continue
		}
		if !slicesContainsInt(ev.Weekdays, ISOWeekday(now)) {
			continue
		}
		minuteKey := ev.ID + ":" + wallClockMinute
		if r.lastFired[minuteKey] {
			continue
		}
		r.lastFired[minuteKey] = true
		r.fire(ev, now)
	}
}

// NextUp reports the soonest upcoming scheduled event, for the dashboard.
func (r *Runner) NextUp(now time.Time) (config.Event, time.Time, bool) {
	var (
		bestEv config.Event
		bestAt time.Time
		found  bool
	)
	for _, ev := range r.cfg().Events {
		if !ev.Enabled {
			continue
		}
		at, ok := NextFire(ev, now)
		if !ok {
			continue
		}
		if !found || at.Before(bestAt) {
			bestEv, bestAt, found = ev, at, true
		}
	}
	return bestEv, bestAt, found
}

func slicesContainsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
