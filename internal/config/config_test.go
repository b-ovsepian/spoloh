package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

func TestDurationRoundTrip(t *testing.T) {
	var d config.Duration
	if err := json.Unmarshal([]byte(`"5s"`), &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Std() != 5*time.Second {
		t.Fatalf("got %v, want 5s", d.Std())
	}

	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != `"5s"` {
		t.Fatalf("got %s, want \"5s\"", out)
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var d config.Duration
	if err := json.Unmarshal([]byte(`"soon"`), &d); err == nil {
		t.Fatal("expected an error for an unparseable duration")
	}
}

func TestDefaultIsValid(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"empty region", func(c *config.Config) { c.Alerts.Region = "" }},
		{"no sources", func(c *config.Config) { c.Alerts.Sources = nil }},
		{"poll interval too small", func(c *config.Config) {
			c.Alerts.PollInterval = config.Duration(time.Second)
		}},
		{"unknown trigger", func(c *config.Config) { c.Events[0].Trigger = "explode" }},
		{"duplicate event id", func(c *config.Config) {
			c.Events = append(c.Events, c.Events[0])
		}},
		{"bad schedule time", func(c *config.Config) {
			for i := range c.Events {
				if c.Events[i].Trigger == config.TriggerScheduled {
					c.Events[i].Time = "9am"
				}
			}
		}},
		{"weekday out of range", func(c *config.Config) {
			for i := range c.Events {
				if c.Events[i].Trigger == config.TriggerScheduled {
					c.Events[i].Weekdays = []int{0}
				}
			}
		}},
		{"volume out of range", func(c *config.Config) { c.Events[0].Volume = 300 }},
		{"empty sound", func(c *config.Config) { c.Events[0].Sound = "" }},
		{"repeat zero", func(c *config.Config) { c.Events[0].Repeat = 0 }},
		{"repeat above ten", func(c *config.Config) { c.Events[0].Repeat = 11 }},
		{"ap password shorter than WPA2's minimum", func(c *config.Config) {
			c.Network.APPassword = "short1"
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Default()
			tc.mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
		})
	}
}

// An empty ap_password is the "not generated yet" state, not an error:
// config.Default() ships it empty, and it must stay valid so a device with no
// installer yet (Plan 3b) can still boot and generate one for itself.
func TestValidateAllowsEmptyAPPassword(t *testing.T) {
	c := config.Default()
	c.Network.APPassword = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected an empty ap_password: %v", err)
	}
}

func TestValidateAllowsAnAPPasswordAtTheWPA2Minimum(t *testing.T) {
	c := config.Default()
	c.Network.APPassword = "eightchr"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected an 8-character ap_password: %v", err)
	}
}

// A shipped device must announce each alert on the first pass, not silence
// itself because a corridor swallowed a single siren: config.Default()'s
// events must all carry the field's default explicitly rather than relying
// on Go's zero value to happen to be right.
func TestDefaultEventsRepeatOnce(t *testing.T) {
	for _, ev := range config.Default().Events {
		if ev.Repeat != 1 {
			t.Errorf("event %q: repeat = %d, want 1", ev.ID, ev.Repeat)
		}
	}
}

func TestValidateAcceptsRepeatAtBothBoundaries(t *testing.T) {
	for _, n := range []int{1, 10} {
		c := config.Default()
		c.Events[0].Repeat = n
		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected repeat %d: %v", n, err)
		}
	}
}

// The operator's instruction after every source in the cascade rate-limited
// at once was "no more often than every 10 seconds", and the 429 that caused
// it asked for 15. Default() therefore sits above the floor rather than
// exactly on it, so a fresh device does not poll at the fastest rate the
// upstream tolerates.
func TestPollIntervalFloorAndDefault(t *testing.T) {
	if config.MinPollInterval != 10*time.Second {
		t.Errorf("MinPollInterval = %v, want 10s", config.MinPollInterval)
	}
	got := config.Default().Alerts.PollInterval.Std()
	if got != 15*time.Second {
		t.Errorf("Default().Alerts.PollInterval = %v, want 15s", got)
	}
	if got <= config.MinPollInterval {
		t.Errorf("Default().Alerts.PollInterval = %v, want it above the %v floor", got, config.MinPollInterval)
	}
}

// Validate is the path a form submission is checked on, so an operator asking
// for an interval the upstream will rate-limit must be told no. That is not
// in conflict with Load repairing a file written by an older build: a human
// asking for something impossible gets an error, a stored config gets
// repaired (see store.go's raisePollIntervalToMinimum).
func TestValidateRejectsAPollIntervalBelowTheMinimum(t *testing.T) {
	c := config.Default()
	c.Alerts.PollInterval = config.Duration(2 * time.Second)
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate accepted a 2s poll interval, below the %v minimum", config.MinPollInterval)
	}
}

func TestValidateAcceptsAPollIntervalAtTheMinimum(t *testing.T) {
	c := config.Default()
	c.Alerts.PollInterval = config.Duration(config.MinPollInterval)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected the %v minimum itself: %v", config.MinPollInterval, err)
	}
}

// Validate must not change the config it is handed. It runs from Save, from
// every dashboard form handler and from Load, so a mutation hiding inside it
// would reach all three and be invisible at each — which is exactly why
// Load's repairs (restoreMissingAlertEvents, restoreMissingRepeat,
// raisePollIntervalToMinimum) live in Load and not here. The config below
// carries one of each repair's input so that moving any of them into Validate
// is caught.
func TestValidateDoesNotMutateTheConfig(t *testing.T) {
	c := config.Default()
	c.Alerts.PollInterval = config.Duration(2 * time.Second)
	c.Events[0].Repeat = 0
	c.Events = c.Events[:1]

	before, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_ = c.Validate()
	after, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("Validate mutated the config it was given:\nbefore %s\nafter  %s", before, after)
	}
}

func TestEventCategory(t *testing.T) {
	alert := config.Event{Trigger: config.TriggerAlertStart}
	if got := alert.Category(); got != config.CategoryAlerts {
		t.Fatalf("got %q, want %q", got, config.CategoryAlerts)
	}
	sched := config.Event{Trigger: config.TriggerScheduled}
	if got := sched.Category(); got != config.CategoryScheduled {
		t.Fatalf("got %q, want %q", got, config.CategoryScheduled)
	}
}

// Every applies_to a stored config.json can hold must keep loading, whatever
// it says — the empty list, both categories, a category this build has never
// heard of. A device that refuses to start over this field trades a
// suppressed alert for a dead announcer, which is the worse of the two, and
// that has been true since before alerts were selectable at all.
func TestValidateAcceptsEveryQuietHoursCategoryList(t *testing.T) {
	lists := [][]string{
		nil,
		{},
		{config.CategoryScheduled},
		{config.CategoryAlerts},
		{config.CategoryAlerts, config.CategoryScheduled},
		{"alarms"},                 // an older or newer spelling
		{"scheduled", "scheduled"}, // a duplicated checkbox submission
		{"", "alerts", "🙂"},        // junk a hand edit can leave behind
	}
	for _, list := range lists {
		c := config.Default()
		c.QuietHours.Enabled = true
		c.QuietHours.AppliesTo = list
		if err := c.Validate(); err != nil {
			t.Errorf("Validate with applies_to %q = %v, want nil — this field must never stop a device starting", list, err)
		}
	}
}

func TestQuietHoursCoversAndSilencesAlerts(t *testing.T) {
	tests := []struct {
		name         string
		qh           config.QuietHours
		covers       string
		wantCovers   bool
		wantSilences bool
	}{
		{
			name:         "the shipped default covers scheduled only",
			qh:           config.Default().QuietHours,
			covers:       config.CategoryScheduled,
			wantCovers:   true,
			wantSilences: false,
		},
		{
			name:         "alerts listed on an enabled window silences them",
			qh:           config.QuietHours{Enabled: true, AppliesTo: []string{config.CategoryAlerts}},
			covers:       config.CategoryAlerts,
			wantCovers:   true,
			wantSilences: true,
		},
		{
			// The whole point of SilencesAlerts folding in Enabled: a list
			// naming alerts on a window nobody switched on silences nothing,
			// and a warning shown for it would be a warning about nothing.
			name:         "alerts listed on a disabled window silences nothing",
			qh:           config.QuietHours{Enabled: false, AppliesTo: []string{config.CategoryAlerts}},
			covers:       config.CategoryAlerts,
			wantCovers:   true,
			wantSilences: false,
		},
		{
			name:         "a category nobody knows matches nothing",
			qh:           config.QuietHours{Enabled: true, AppliesTo: []string{"alarms"}},
			covers:       config.CategoryAlerts,
			wantCovers:   false,
			wantSilences: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.qh.Covers(tc.covers); got != tc.wantCovers {
				t.Errorf("Covers(%q) = %v, want %v", tc.covers, got, tc.wantCovers)
			}
			if got := tc.qh.SilencesAlerts(); got != tc.wantSilences {
				t.Errorf("SilencesAlerts() = %v, want %v", got, tc.wantSilences)
			}
		})
	}
}
