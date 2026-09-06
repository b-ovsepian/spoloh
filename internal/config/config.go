// Package config defines Spoloh's on-disk settings and their validation rules.
package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Schema is the config format version this build understands.
const Schema = 1

// Event triggers.
const (
	TriggerAlertStart = "alert_start"
	TriggerAlertEnd   = "alert_end"
	TriggerScheduled  = "scheduled"
)

// Quiet-hour categories. Quiet hours default to suppressing CategoryScheduled
// only: an announcer that goes silent at night is silent when it matters most.
const (
	CategoryAlerts    = "alerts"
	CategoryScheduled = "scheduled"
)

// MinPollInterval keeps us clear of the upstream rate limit, which returns 429
// for two requests roughly a second apart.
const MinPollInterval = 3 * time.Second

// Duration serialises as a Go duration string such as "5s".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Schema     int        `json:"schema"`
	Device     Device     `json:"device"`
	Alerts     Alerts     `json:"alerts"`
	Audio      Audio      `json:"audio"`
	QuietHours QuietHours `json:"quiet_hours"`
	Events     []Event    `json:"events"`
	Auth       Auth       `json:"auth"`
	Network    Network    `json:"network"`
}

type Device struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

type Alerts struct {
	Region       string   `json:"region"`
	Sources      []string `json:"sources"`
	PollInterval Duration `json:"poll_interval"`
}

type Audio struct {
	Device       string `json:"device"`
	MasterVolume int    `json:"master_volume"`
}

type QuietHours struct {
	Enabled   bool     `json:"enabled"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	AppliesTo []string `json:"applies_to"`
}

// Event covers both alert announcements and scheduled announcements. Time and
// Weekdays are meaningful only when Trigger is TriggerScheduled.
type Event struct {
	ID       string `json:"id"`
	Trigger  string `json:"trigger"`
	Name     string `json:"name,omitempty"`
	Time     string `json:"time,omitempty"`
	Weekdays []int  `json:"weekdays,omitempty"`
	Sound    string `json:"sound"`
	Volume   int    `json:"volume"`
	Enabled  bool   `json:"enabled"`
}

// Category maps a trigger onto the quiet-hours vocabulary.
func (e Event) Category() string {
	if e.Trigger == TriggerScheduled {
		return CategoryScheduled
	}
	return CategoryAlerts
}

type Auth struct {
	PasswordHash string `json:"password_hash"`

	// SessionSecret signs the dashboard's session cookies. It is generated
	// once, when the administrator password is first set, and lives here
	// rather than in memory so that sessions survive a restart of the
	// daemon — systemd restarts this unit routinely (Restart=always,
	// WatchdogSec, self-update), and an operator logged out by every one of
	// them would leave a wall-mounted dashboard sitting on a login page.
	//
	// It is a credential, not a setting: no editing screen may render it,
	// and a device rebuilt from an exported configuration inherits the
	// sessions of the device it was exported from.
	SessionSecret string `json:"session_secret,omitempty"`
}

type Network struct {
	APSSID          string   `json:"ap_ssid"`
	APPassword      string   `json:"ap_password"`
	APFallbackAfter Duration `json:"ap_fallback_after"`
}

// Default returns a config that is valid and immediately useful: alerts for
// Kyiv city and a minute of silence at 09:00 every day.
func Default() *Config {
	return &Config{
		Schema: Schema,
		Device: Device{Name: "spoloh", Timezone: "Europe/Kyiv"},
		Alerts: Alerts{
			Region:       "м. Київ",
			Sources:      []string{"default", "skog", "aiu"},
			PollInterval: Duration(5 * time.Second),
		},
		Audio: Audio{Device: "default", MasterVolume: 80},
		QuietHours: QuietHours{
			Enabled:   false,
			From:      "22:00",
			To:        "07:00",
			AppliesTo: []string{CategoryScheduled},
		},
		Events: []Event{
			{ID: "alert-start", Trigger: TriggerAlertStart, Sound: "siren.wav", Volume: 100, Enabled: true},
			{ID: "alert-end", Trigger: TriggerAlertEnd, Sound: "all-clear.wav", Volume: 90, Enabled: true},
			{
				ID: "silence", Trigger: TriggerScheduled, Name: "Хвилина мовчання",
				Time: "09:00", Weekdays: []int{1, 2, 3, 4, 5, 6, 7},
				Sound: "metronome.wav", Volume: 85, Enabled: true,
			},
		},
		Network: Network{
			APSSID:          "Spoloh-Setup",
			APFallbackAfter: Duration(45 * time.Second),
		},
	}
}

// ParseHM parses an "HH:MM" wall-clock string into minutes since midnight.
func ParseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (c *Config) Validate() error {
	if c.Schema != Schema {
		return fmt.Errorf("config schema %d, this build understands %d", c.Schema, Schema)
	}
	if c.Device.Timezone == "" {
		return fmt.Errorf("device.timezone is required")
	}
	if _, err := time.LoadLocation(c.Device.Timezone); err != nil {
		return fmt.Errorf("device.timezone %q: %w", c.Device.Timezone, err)
	}
	if c.Alerts.Region == "" {
		return fmt.Errorf("alerts.region is required")
	}
	if len(c.Alerts.Sources) == 0 {
		return fmt.Errorf("alerts.sources must list at least one source")
	}
	if c.Alerts.PollInterval.Std() < MinPollInterval {
		return fmt.Errorf("alerts.poll_interval %v is below the %v minimum", c.Alerts.PollInterval.Std(), MinPollInterval)
	}
	if c.Audio.MasterVolume < 0 || c.Audio.MasterVolume > 100 {
		return fmt.Errorf("audio.master_volume %d is outside 0..100", c.Audio.MasterVolume)
	}
	if c.QuietHours.Enabled {
		if _, err := ParseHM(c.QuietHours.From); err != nil {
			return fmt.Errorf("quiet_hours.from: %w", err)
		}
		if _, err := ParseHM(c.QuietHours.To); err != nil {
			return fmt.Errorf("quiet_hours.to: %w", err)
		}
	}
	seen := make(map[string]bool, len(c.Events))
	for _, e := range c.Events {
		if err := e.validate(); err != nil {
			return err
		}
		if seen[e.ID] {
			return fmt.Errorf("duplicate event id %q", e.ID)
		}
		seen[e.ID] = true
	}
	return nil
}

func (e Event) validate() error {
	if e.ID == "" {
		return fmt.Errorf("event id is required")
	}
	switch e.Trigger {
	case TriggerAlertStart, TriggerAlertEnd:
	case TriggerScheduled:
		if _, err := ParseHM(e.Time); err != nil {
			return fmt.Errorf("event %q: %w", e.ID, err)
		}
		if len(e.Weekdays) == 0 {
			return fmt.Errorf("event %q: weekdays must not be empty", e.ID)
		}
		for _, d := range e.Weekdays {
			if d < 1 || d > 7 {
				return fmt.Errorf("event %q: weekday %d is outside 1..7", e.ID, d)
			}
		}
	default:
		return fmt.Errorf("event %q: unknown trigger %q", e.ID, e.Trigger)
	}
	if e.Sound == "" {
		return fmt.Errorf("event %q: sound is required", e.ID)
	}
	if e.Volume < 0 || e.Volume > 100 {
		return fmt.Errorf("event %q: volume %d is outside 0..100", e.ID, e.Volume)
	}
	return nil
}
