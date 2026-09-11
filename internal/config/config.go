// Package config defines Spoloh's on-disk settings and their validation rules.
package config

import (
	"encoding/json"
	"fmt"
	"strings"
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
//
// It was 3s until a device polling every 5 seconds had every source in its
// cascade answer 429 at once. A device throttled by all of its sources is one
// that may not learn about an air raid, which is the only thing it exists to
// do: polling faster made it less reliable, not more. Ten seconds is the floor
// the operator asked for.
//
// Raising this floor must not strand a device already in the field, every one
// of which stores a value this now refuses. A stored interval below the
// minimum is repaired on load, not rejected — see raisePollIntervalToMinimum
// in store.go. Validate goes on rejecting it, because Validate is also how a
// form submission is checked, and an operator typing two seconds should be
// told no rather than silently corrected.
const MinPollInterval = 10 * time.Second

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
	// NTPServer is the time server the device should synchronise against,
	// as a hostname or an IP address. Empty means "whatever the
	// distribution configures", which is the state every device ships in
	// and a perfectly good answer on a site with internet access; it is a
	// site with its own local time server, or none reachable, that needs
	// this.
	//
	// timedatectl cannot set it. It ends up in
	// /etc/systemd/timesyncd.conf.d/, written by a root oneshot unit — see
	// internal/timedate.Timedatectl.ApplyNTPServer — which is why
	// ValidateNTPServer below is strict rather than advisory: this value
	// crosses a privilege boundary into a file root parses.
	NTPServer string `json:"ntp_server,omitempty"`
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
	// Repeat is how many times the sound plays, back to back, for this one
	// trigger — an operator's answer to a siren that does not carry across a
	// corridor on a single pass. It defaults to 1 and is capped at 10 by
	// validate. The zero value is not a legal repeat count: a config.json
	// written before this field existed has no "repeat" key, and
	// json.Unmarshal would otherwise leave this at 0, silently turning
	// "play once" into "play never" on a device where a missing setting must
	// never mean silence. store.go's Load repairs that case the same way it
	// already repairs a missing alert event, before validate ever sees it.
	Repeat  int  `json:"repeat"`
	Enabled bool `json:"enabled"`
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
			Region:  "м. Київ",
			Sources: []string{"default", "skog", "aiu"},
			// 15s rather than the 10s floor: the 429 that forced the floor
			// up asked for 15 seconds by name in its Retry-After, so take
			// the upstream at its word rather than sitting exactly on the
			// fastest rate it tolerates.
			PollInterval: Duration(15 * time.Second),
		},
		Audio: Audio{Device: "default", MasterVolume: 80},
		QuietHours: QuietHours{
			Enabled:   false,
			From:      "22:00",
			To:        "07:00",
			AppliesTo: []string{CategoryScheduled},
		},
		Events: []Event{
			{ID: "alert-start", Trigger: TriggerAlertStart, Sound: "siren.wav", Volume: 100, Repeat: 1, Enabled: true},
			{ID: "alert-end", Trigger: TriggerAlertEnd, Sound: "all-clear.wav", Volume: 90, Repeat: 1, Enabled: true},
			{
				ID: "silence", Trigger: TriggerScheduled, Name: "Хвилина мовчання",
				Time: "09:00", Weekdays: []int{1, 2, 3, 4, 5, 6, 7},
				Sound: "metronome.wav", Volume: 85, Repeat: 1, Enabled: true,
			},
		},
		Network: Network{
			APSSID:          "Spoloh-Setup",
			APFallbackAfter: Duration(45 * time.Second),
		},
	}
}

// maxNTPServerLen is the longest a DNS name may be. An IPv6 literal is far
// shorter, so one bound covers both.
const maxNTPServerLen = 253

// ValidateNTPServer accepts an empty value, a hostname or an IP address, and
// rejects everything else.
//
// This is an allow-list of bytes rather than a search for dangerous ones,
// and that is the whole point. The value is written to a file that a root
// unit reads and turns into an NTP= line in
// /etc/systemd/timesyncd.conf.d/spoloh.conf: a newline in it appends a
// second directive of the operator's choosing to a root-owned config file,
// and a quote or an "=" lets the line be re-read as something other than the
// key it was meant to be. Listing what may appear — letters, digits, dots,
// hyphens and the colons of an IPv6 literal — means a character nobody
// thought about is refused by default instead of admitted by default.
//
// One server, not a list: systemd-timesyncd's NTP= accepts several
// space-separated, but a space is exactly the character that would let one
// field become two, and a single server is what the dashboard asks for. A
// site that needs a pool can still point this at a hostname that resolves to
// several addresses.
//
// The leading and trailing checks are ordinary hostname hygiene rather than
// safety: ".example.com" and "-example.com" are not names anything will
// resolve, and rejecting them here beats a device that looks configured and
// never synchronises.
func ValidateNTPServer(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > maxNTPServerLen {
		return fmt.Errorf("%q is %d characters, the maximum is %d", s, len(s), maxNTPServerLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == ':':
		default:
			return fmt.Errorf("%q contains %q; a time server is a hostname or an IP address", s, string(c))
		}
	}
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "-") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, "-") {
		return fmt.Errorf("%q is not a hostname or an IP address", s)
	}
	return nil
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
	if err := ValidateNTPServer(c.Device.NTPServer); err != nil {
		return fmt.Errorf("device.ntp_server: %w", err)
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
	// Empty is the "not generated yet" state: config.Default() ships it
	// empty, and netmgr.EnsureAPPassword fills it in on first run. Only a
	// non-empty value gets checked against WPA2's floor, so a hand-edited
	// config cannot produce a hotspot that refuses every connection.
	if pw := c.Network.APPassword; pw != "" && len(pw) < 8 {
		return fmt.Errorf("network.ap_password is %d characters, WPA2 requires at least 8", len(pw))
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
	// 1..10, not 0..10: unlike volume, repeat has no zero state to protect —
	// see Event.Repeat's doc comment for why 0 must never reach here. The
	// upper bound is the typo guard the brief asks for: a mistyped repeat
	// count must not occupy the single playback queue for an hour.
	if e.Repeat < 1 || e.Repeat > 10 {
		return fmt.Errorf("event %q: repeat %d is outside 1..10", e.ID, e.Repeat)
	}
	return nil
}
