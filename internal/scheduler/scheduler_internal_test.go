package scheduler

import (
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/clock"
	"github.com/spoloh/spoloh/internal/config"
)

// lastFired grows by one entry per firing, so a daily event on a device that
// runs for a year would otherwise accumulate 365 entries — on a board with
// 512 MB of RAM and no reason ever to be restarted.
func TestLastFiredIsBounded(t *testing.T) {
	loc := time.UTC
	ev := config.Event{
		ID: "bell", Trigger: config.TriggerScheduled, Time: "08:30",
		Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Sound: "bell.wav", Volume: 80, Enabled: true,
	}
	cfg := config.Default()
	cfg.Events = []config.Event{ev}

	fired := 0
	r := NewRunner(&clock.FakeSync{Value: true}, func() *config.Config { return cfg }, func(config.Event, time.Time) { fired++ })

	// A hundred days, two ticks each: the firing minute and one a minute later.
	for day := 0; day < 100; day++ {
		at := time.Date(2026, 1, 1, 8, 30, 0, 0, loc).AddDate(0, 0, day)
		r.Tick(at)
		r.Tick(at.Add(time.Minute))
	}
	if fired != 100 {
		t.Fatalf("fired %d times over 100 days, want 100", fired)
	}
	if len(r.lastFired) > 2 {
		t.Fatalf("lastFired holds %d entries after 100 days, want it bounded by the prune window", len(r.lastFired))
	}
}

// Pruning must be by wall-clock age, not elapsed absolute time: the two
// occurrences of a repeated hour on a DST fall-back day are an hour apart in
// absolute time and are the same wall-clock minute. Pruning between them would
// resurrect exactly the double fire the wall-clock key exists to prevent.
func TestPruneKeepsTheCurrentWallClockMinuteAcrossADSTFallback(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	r := NewRunner(&clock.FakeSync{Value: true}, config.Default, func(config.Event, time.Time) {})

	first := time.Date(2025, 10, 26, 0, 15, 30, 0, time.UTC).In(loc)  // 02:15 CEST
	second := time.Date(2025, 10, 26, 1, 15, 30, 0, time.UTC).In(loc) // 02:15 CET

	r.lastFired["bell:"+first.Format("2006-01-02 15:04")] = wallMinute(first)
	r.prune(wallMinute(second))

	if len(r.lastFired) != 1 {
		t.Fatal("the second occurrence of a repeated hour pruned the first's record")
	}
}

// A backwards NTP step — the failure mode on a board with no RTC — must not
// discard a record of a minute that has already fired.
func TestPruneKeepsRecordsDatedInTheFuture(t *testing.T) {
	r := NewRunner(&clock.FakeSync{Value: true}, config.Default, func(config.Event, time.Time) {})
	at := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)

	r.lastFired["bell:2026-09-07 09:00"] = wallMinute(at)
	r.prune(wallMinute(at.Add(-time.Hour)))

	if len(r.lastFired) != 1 {
		t.Fatal("a backwards clock step pruned a record of a minute that had already fired")
	}
}
