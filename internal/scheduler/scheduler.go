// Package scheduler fires configured events at their wall-clock times.
package scheduler

import (
	"log/slog"
	"slices"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// ISOWeekday returns Monday=1 .. Sunday=7, matching the config format.
func ISOWeekday(t time.Time) int {
	d := int(t.Weekday())
	if d == 0 {
		return 7
	}
	return d
}

// occurrences reduces an event's schedule to the three numbers every caller
// here needs: the minute of the day it first fires at, the last minute it may
// fire at, and the gap between occurrences. It reports false only when Time
// is not a time, which is the one case nothing can be computed from.
//
// An event with no interval is the degenerate window rather than a separate
// branch: last is first and step is 1, so the arithmetic below produces the
// single firing at Time without either caller asking whether this event
// repeats. That is the shape of every stored event today and of most events
// there will ever be, so it is worth having exactly one code path.
//
// A window this build cannot use — an interval with an unparseable end time,
// or one that ends before it starts — collapses to that same single firing.
// config.Load repairs such a value out of a stored config before the daemon
// sees it (see config.repairRepeatWindows), and this is deliberately the
// same answer given a second time: NextFire is also called on configurations
// assembled in memory, by the dashboard and by other packages' tests, which
// never went through Load at all. Degrading to one announcement is what this
// device does everywhere else; degrading to none is what it refuses.
func occurrences(ev config.Event) (first, last, step int, ok bool) {
	first, err := config.ParseHM(ev.Time)
	if err != nil {
		return 0, 0, 0, false
	}
	last, step = first, 1
	if ev.EveryMinutes > 0 {
		if until, err := config.ParseHM(ev.Until); err == nil && until >= first {
			last, step = until, ev.EveryMinutes
		}
	}
	return first, last, step, true
}

// NextFire returns the next time ev is due strictly after `after`, in after's
// own location. It reports false for events that can never fire.
func NextFire(ev config.Event, after time.Time) (time.Time, bool) {
	if ev.Trigger != config.TriggerScheduled || len(ev.Weekdays) == 0 {
		return time.Time{}, false
	}
	first, last, step, ok := occurrences(ev)
	if !ok {
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
		for m := first; m <= last; m += step {
			candidate := time.Date(day.Year(), day.Month(), day.Day()+i, m/60, m%60, 0, 0, loc)
			if !candidate.After(after) {
				continue
			}
			if allowed[ISOWeekday(candidate)] {
				return candidate, true
			}
			// Every occurrence of one day shares that day's weekday — no
			// local time inside a day normalises onto another date — so a
			// weekday this event does not run on rules out the whole day,
			// and walking its other 1,439 minutes would only slow down the
			// once-a-second call this feeds on the dashboard.
			break
		}
	}
	return time.Time{}, false
}

// Runner fires due events on each tick. It is driven from a single goroutine.
type Runner struct {
	sync clock.SyncChecker
	cfg  func() *config.Config
	fire func(config.Event, time.Time)

	// Logger receives the hold and resume transitions. NewRunner defaults it
	// to slog.Default(); a nil value is tolerated and falls back to the same.
	Logger *slog.Logger

	// holding is true while the clock is unsynchronised, so that the hold and
	// the resume are each logged once instead of once per tick.
	holding bool

	// lastFired records which wall-clock minutes each event has already fired
	// in. Key is "event-id:2006-01-02 15:04", ensuring both occurrences of a
	// repeated hour on a DST fall-back day map to the same minute and fire
	// only once.
	//
	// This map is deliberately never pruned. It grows by exactly one entry
	// per firing, so ten scheduled events firing daily add about 3,650
	// entries a year — on the order of a megabyte after five years of
	// uninterrupted uptime on a device that reboots regularly. A previous
	// version of this code pruned entries by wall-clock age to bound that
	// growth, and that pruning is what caused the DST bug this comment now
	// warns against: on an autumn fall-back day the repeated hour (e.g.
	// 02:00-02:59) occurs twice, an hour apart in absolute time but the same
	// wall-clock minute. Pruning by wall-clock age deletes the record of the
	// first occurrence within a couple of minutes of wall-clock progress —
	// long before the clock falls back and climbs to that same minute again
	// roughly an hour of real time later — so the second occurrence finds no
	// record and fires the event a second time. Do not reintroduce pruning
	// here; the unbounded growth is the cheaper trade.
	lastFired map[string]bool
}

func NewRunner(sync clock.SyncChecker, cfg func() *config.Config, fire func(config.Event, time.Time)) *Runner {
	return &Runner{sync: sync, cfg: cfg, fire: fire, Logger: slog.Default(), lastFired: map[string]bool{}}
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

// Tick fires every event due in now's minute.
func (r *Runner) Tick(now time.Time) {
	// Without a synchronised clock the device does not know what time it is,
	// and a wrong time is worse than no announcement (spec §5.3). The hold has
	// to be visible: every cause of it — no NTP yet, no timedatectl binary, no
	// D-Bus — looks identical from here, and an invisible hold is a device
	// that reports itself healthy while the room stays silent. Logged on the
	// transition only; this runs once a second.
	if r.sync != nil && !r.sync.Synced() {
		if !r.holding {
			r.holding = true
			r.logger().Warn("scheduler holding: the clock is not NTP-synchronised, so no scheduled event will fire")
		}
		return
	}
	if r.holding {
		r.holding = false
		r.logger().Info("scheduler resumed: the clock is NTP-synchronised")
	}
	// Key on wall-clock time, not absolute instant, so that both occurrences of a
	// repeated hour on a DST fall-back day map to the same minute.
	wallClockMinute := now.Format("2006-01-02 15:04")
	cur := now.Hour()*60 + now.Minute()

	for _, ev := range r.cfg().Events {
		if ev.Trigger != config.TriggerScheduled || !ev.Enabled {
			continue
		}
		first, last, step, ok := occurrences(ev)
		if !ok {
			continue
		}
		// Inside the window and on the interval. An event with no interval
		// reduces to "cur is exactly its one minute", which is what this
		// loop tested before repeating events existed — see occurrences.
		if cur < first || cur > last || (cur-first)%step != 0 {
			continue
		}
		if !slices.Contains(ev.Weekdays, ISOWeekday(now)) {
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
//
// It reports the next occurrence, not the next start: an event repeating
// every ten minutes until 10:00, asked at 09:32, answers 09:40. Nothing
// fails when this is wrong — the «Найближча подія» card simply says
// tomorrow, plausibly, while the device is about to sound in eight minutes —
// which is why NextFire and Tick read the same occurrences() rather than
// each working the window out for themselves.
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
