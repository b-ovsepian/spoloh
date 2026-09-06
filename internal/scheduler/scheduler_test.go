package scheduler_test

import (
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/clock"
	"github.com/spoloh/spoloh/internal/config"
	"github.com/spoloh/spoloh/internal/scheduler"
)

func kyiv(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

func weekdayEvent() config.Event {
	return config.Event{
		ID: "bell", Trigger: config.TriggerScheduled, Time: "08:30",
		Weekdays: []int{1, 2, 3, 4, 5}, Sound: "bell.wav", Volume: 80, Enabled: true,
	}
}

func TestISOWeekday(t *testing.T) {
	loc := kyiv(t)
	// 2026-09-06 is a Sunday; ISO numbering makes that 7, not 0.
	sunday := time.Date(2026, 9, 6, 12, 0, 0, 0, loc)
	if got := scheduler.ISOWeekday(sunday); got != 7 {
		t.Fatalf("Sunday = %d, want 7", got)
	}
	monday := sunday.AddDate(0, 0, 1)
	if got := scheduler.ISOWeekday(monday); got != 1 {
		t.Fatalf("Monday = %d, want 1", got)
	}
}

func TestNextFireLaterToday(t *testing.T) {
	loc := kyiv(t)
	after := time.Date(2026, 9, 7, 7, 0, 0, 0, loc) // Monday 07:00

	got, ok := scheduler.NextFire(weekdayEvent(), after)
	if !ok {
		t.Fatal("expected a next fire")
	}
	want := time.Date(2026, 9, 7, 8, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextFireRollsToTomorrow(t *testing.T) {
	loc := kyiv(t)
	after := time.Date(2026, 9, 7, 9, 0, 0, 0, loc) // Monday, already past 08:30

	got, ok := scheduler.NextFire(weekdayEvent(), after)
	if !ok {
		t.Fatal("expected a next fire")
	}
	want := time.Date(2026, 9, 8, 8, 30, 0, 0, loc) // Tuesday
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextFireSkipsWeekend(t *testing.T) {
	loc := kyiv(t)
	after := time.Date(2026, 9, 5, 12, 0, 0, 0, loc) // Saturday

	got, ok := scheduler.NextFire(weekdayEvent(), after)
	if !ok {
		t.Fatal("expected a next fire")
	}
	want := time.Date(2026, 9, 7, 8, 30, 0, 0, loc) // Monday
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextFireRejectsNonScheduledEvent(t *testing.T) {
	ev := config.Event{ID: "a", Trigger: config.TriggerAlertStart, Sound: "siren.wav", Enabled: true}
	if _, ok := scheduler.NextFire(ev, time.Now()); ok {
		t.Fatal("an alert event has no scheduled fire time")
	}
}

func TestNextFireRejectsEmptyWeekdays(t *testing.T) {
	ev := weekdayEvent()
	ev.Weekdays = nil
	if _, ok := scheduler.NextFire(ev, time.Now()); ok {
		t.Fatal("an event with no weekdays never fires")
	}
}

func cfgWith(ev config.Event) func() *config.Config {
	c := config.Default()
	c.Events = []config.Event{ev}
	return func() *config.Config { return c }
}

func TestRunnerFiresOncePerMinute(t *testing.T) {
	loc := kyiv(t)
	var fired []time.Time
	r := scheduler.NewRunner(
		&clock.FakeSync{Value: true},
		cfgWith(weekdayEvent()),
		func(_ config.Event, at time.Time) { fired = append(fired, at) },
	)

	base := time.Date(2026, 9, 7, 8, 30, 0, 0, loc) // Monday 08:30
	for i := 0; i < 60; i++ {
		r.Tick(base.Add(time.Duration(i) * time.Second))
	}
	if len(fired) != 1 {
		t.Fatalf("fired %d times in one minute, want 1", len(fired))
	}
}

func TestRunnerDoesNotFireOffSchedule(t *testing.T) {
	loc := kyiv(t)
	fired := 0
	r := scheduler.NewRunner(
		&clock.FakeSync{Value: true},
		cfgWith(weekdayEvent()),
		func(config.Event, time.Time) { fired++ },
	)

	r.Tick(time.Date(2026, 9, 7, 8, 29, 59, 0, loc))
	r.Tick(time.Date(2026, 9, 7, 8, 31, 0, 0, loc))
	r.Tick(time.Date(2026, 9, 5, 8, 30, 0, 0, loc)) // Saturday
	if fired != 0 {
		t.Fatalf("fired %d times, want 0", fired)
	}
}

// ARM boards have no RTC. Firing on an unsynced clock would sound the minute of
// silence at a random hour after every power cut.
func TestRunnerHoldsWhenClockUnsynced(t *testing.T) {
	loc := kyiv(t)
	fired := 0
	sync := &clock.FakeSync{Value: false}
	r := scheduler.NewRunner(sync, cfgWith(weekdayEvent()), func(config.Event, time.Time) { fired++ })

	r.Tick(time.Date(2026, 9, 7, 8, 30, 0, 0, loc))
	if fired != 0 {
		t.Fatalf("fired %d times on an unsynced clock, want 0", fired)
	}

	// Once NTP lands, the next matching minute fires; the missed one is not
	// replayed, because a late minute of silence is worse than none.
	sync.Value = true
	r.Tick(time.Date(2026, 9, 8, 8, 30, 0, 0, loc))
	if fired != 1 {
		t.Fatalf("fired %d times after sync, want 1", fired)
	}
}

func TestRunnerSkipsDisabledEvents(t *testing.T) {
	loc := kyiv(t)
	ev := weekdayEvent()
	ev.Enabled = false
	fired := 0
	r := scheduler.NewRunner(&clock.FakeSync{Value: true}, cfgWith(ev), func(config.Event, time.Time) { fired++ })

	r.Tick(time.Date(2026, 9, 7, 8, 30, 0, 0, loc))
	if fired != 0 {
		t.Fatalf("fired %d times for a disabled event, want 0", fired)
	}
}

func TestNextUp(t *testing.T) {
	loc := kyiv(t)
	r := scheduler.NewRunner(&clock.FakeSync{Value: true}, cfgWith(weekdayEvent()), func(config.Event, time.Time) {})

	ev, at, ok := r.NextUp(time.Date(2026, 9, 7, 7, 0, 0, 0, loc))
	if !ok {
		t.Fatal("expected an upcoming event")
	}
	if ev.ID != "bell" {
		t.Fatalf("event = %q, want bell", ev.ID)
	}
	want := time.Date(2026, 9, 7, 8, 30, 0, 0, loc)
	if !at.Equal(want) {
		t.Fatalf("at = %v, want %v", at, want)
	}
}
