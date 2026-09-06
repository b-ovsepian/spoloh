package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/config"
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
