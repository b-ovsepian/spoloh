package scheduler_test

import (
	"bytes"
	"log/slog"
	"strings"
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

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

// On a DST fall-back day, the local time 02:00..02:59 occurs twice (an hour apart
// in absolute time). An event scheduled during this window must fire exactly once,
// not twice.
func TestRunnerDoesNotDoubleFireOnDSTFallback(t *testing.T) {
	loc := berlin(t)
	// Europe/Berlin falls back on 2025-10-26 at 03:00 CEST → 02:00 CET.
	// The wall-clock hour 02:00..02:59 happens twice: once in CEST (UTC+2) and once in CET (UTC+1).

	ev := config.Event{
		ID: "fallback-test", Trigger: config.TriggerScheduled, Time: "02:15",
		Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Sound: "test.wav", Volume: 80, Enabled: true,
	}

	var fired []time.Time
	r := scheduler.NewRunner(
		&clock.FakeSync{Value: true},
		cfgWith(ev),
		func(_ config.Event, at time.Time) { fired = append(fired, at) },
	)

	// Create two absolute times that both display as 02:15 local time:
	// - first: 00:15 UTC = 02:15 CEST (first occurrence, before transition)
	// - second: 01:15 UTC = 02:15 CET (second occurrence, after transition)
	// They are exactly one hour apart in absolute time.
	first := time.Date(2025, 10, 26, 0, 15, 30, 0, time.UTC).In(loc)
	second := time.Date(2025, 10, 26, 1, 15, 30, 0, time.UTC).In(loc)

	// Tick both occurrences; the event should fire exactly once.
	r.Tick(first)
	r.Tick(second)

	if len(fired) != 1 {
		t.Fatalf("fired %d times across DST fall-back, want 1; fired at %v and %v", len(fired), first, second)
	}
}

// NextFire must compute times correctly across DST transitions, particularly
// for transitions that occur during the event's scheduled time window.
func TestNextFireComputesCorrectlyAcrossDSTTransition(t *testing.T) {
	loc := berlin(t)

	ev := config.Event{
		ID: "transition-test", Trigger: config.TriggerScheduled, Time: "02:30",
		Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Sound: "test.wav", Volume: 80, Enabled: true,
	}

	// Query from just before the DST fall-back transition on 2025-10-26.
	// Europe/Berlin falls back at 03:00 CEST → 02:00 CET.
	// We ask: what is the next 02:30?
	after := time.Date(2025, 10, 26, 1, 0, 0, 0, loc) // 01:00 on the transition day

	got, ok := scheduler.NextFire(ev, after)
	if !ok {
		t.Fatal("expected a next fire")
	}

	// The next 02:30 should be on the same day, after the transition.
	// In the wall-clock sequence on 2025-10-26, after 03:00 becomes 02:00,
	// so 02:30 is still in the future.
	want := time.Date(2025, 10, 26, 2, 30, 0, 0, loc)

	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Verify the returned time is strictly after `after`.
	if !got.After(after) {
		t.Fatalf("returned time %v is not after %v", got, after)
	}
}

// An invisible hold is the failure the spec calls the worst one: the device
// reports itself healthy while the room stays silent. Once when it starts
// holding, once when it resumes — and not once per tick, because Tick runs
// every second and would otherwise write 86,400 lines a day to an SD card.
func TestRunnerLogsHoldAndResumeExactlyOnce(t *testing.T) {
	loc := kyiv(t)
	var buf bytes.Buffer
	sync := &clock.FakeSync{Value: false}
	r := scheduler.NewRunner(sync, cfgWith(weekdayEvent()), func(config.Event, time.Time) {})
	r.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	base := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	for i := 0; i < 300; i++ {
		r.Tick(base.Add(time.Duration(i) * time.Second))
	}
	if got := strings.Count(buf.String(), "scheduler holding"); got != 1 {
		t.Fatalf("logged the hold %d times over 300 ticks, want 1:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "scheduler resumed"); got != 0 {
		t.Fatalf("logged a resume %d times while still holding, want 0:\n%s", got, buf.String())
	}

	sync.Value = true
	for i := 300; i < 600; i++ {
		r.Tick(base.Add(time.Duration(i) * time.Second))
	}
	if got := strings.Count(buf.String(), "scheduler resumed"); got != 1 {
		t.Fatalf("logged the resume %d times over 300 ticks, want 1:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "scheduler holding"); got != 1 {
		t.Fatalf("logged the hold %d times in total, want 1:\n%s", got, buf.String())
	}
}

// A clock that is synchronised from the first tick is the normal case and must
// say nothing at all: there was no hold to resume from.
func TestRunnerSaysNothingWhenNeverHolding(t *testing.T) {
	loc := kyiv(t)
	var buf bytes.Buffer
	r := scheduler.NewRunner(&clock.FakeSync{Value: true}, cfgWith(weekdayEvent()), func(config.Event, time.Time) {})
	r.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	base := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	for i := 0; i < 120; i++ {
		r.Tick(base.Add(time.Duration(i) * time.Second))
	}
	if buf.Len() != 0 {
		t.Fatalf("a synchronised clock logged:\n%s", buf.String())
	}
}

// A nil Logger must not panic: the field is optional.
func TestRunnerNilLoggerIsSafe(t *testing.T) {
	loc := kyiv(t)
	r := scheduler.NewRunner(&clock.FakeSync{Value: false}, cfgWith(weekdayEvent()), func(config.Event, time.Time) {})
	r.Logger = nil
	r.Tick(time.Date(2026, 9, 7, 8, 30, 0, 0, loc))
}
