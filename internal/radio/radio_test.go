package radio_test

import (
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/radio"
)

// Every station in the catalogue is reachable by its ID and plays over
// https: ffmpeg is held to http and https, and a catalogue entry it would
// refuse is a station the dashboard offers and the speaker never plays.
func TestEveryStationIsUsable(t *testing.T) {
	if len(radio.Stations) == 0 {
		t.Fatal("the catalogue is empty")
	}
	seen := map[string]bool{}
	for _, s := range radio.Stations {
		if s.ID == "" || s.Name == "" {
			t.Errorf("station %+v has no ID or name", s)
		}
		if seen[s.ID] {
			t.Errorf("station ID %q is used twice", s.ID)
		}
		seen[s.ID] = true
		if s.ID == radio.Custom {
			t.Errorf("station ID %q collides with the operator's own stream", s.ID)
		}
		if err := radio.ValidateURL(s.URL); err != nil {
			t.Errorf("station %q: %v", s.ID, err)
		}
		if got, ok := radio.Lookup(s.ID); !ok || got != s {
			t.Errorf("Lookup(%q) = %+v, %t", s.ID, got, ok)
		}
	}
}

func plain() config.Radio {
	return config.Radio{Enabled: true, Station: "hitfm", Volume: 50, Weekdays: []int{1, 5}, From: "08:00", To: "18:00"}
}

// What the /radio form refuses. A stored config is never checked against
// this — a device must never refuse to start over its radio — only what an
// operator submits.
func TestValidate(t *testing.T) {
	if err := radio.Validate(plain()); err != nil {
		t.Fatalf("a plain radio setting was refused: %v", err)
	}
	custom := plain()
	custom.Station, custom.URL = radio.Custom, "https://stream.example/live"
	if err := radio.Validate(custom); err != nil {
		t.Fatalf("an https stream of the operator's own was refused: %v", err)
	}
	for name, mutate := range map[string]func(*config.Radio){
		"volume above 100":        func(r *config.Radio) { r.Volume = 101 },
		"volume below 0":          func(r *config.Radio) { r.Volume = -1 },
		"no weekdays":             func(r *config.Radio) { r.Weekdays = nil },
		"weekday 8":               func(r *config.Radio) { r.Weekdays = []int{8} },
		"from not a time":         func(r *config.Radio) { r.From = "8am" },
		"to before from":          func(r *config.Radio) { r.From, r.To = "18:00", "08:00" },
		"to equal to from":        func(r *config.Radio) { r.To = r.From },
		"unknown station":         func(r *config.Radio) { r.Station = "no-such-station" },
		"custom with no url":      func(r *config.Radio) { r.Station, r.URL = radio.Custom, "" },
		"custom file url":         func(r *config.Radio) { r.Station, r.URL = radio.Custom, "file://localhost/etc/spoloh/config.json" },
		"custom ftp url":          func(r *config.Radio) { r.Station, r.URL = radio.Custom, "ftp://stream.example/live" },
		"custom url with no host": func(r *config.Radio) { r.Station, r.URL = radio.Custom, "https:///live" },
	} {
		r := plain()
		mutate(&r)
		if err := radio.Validate(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Wednesday 2026-11-11, inside a weekday 08:00–18:00 window.
var wednesdayNoon = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func onCfg() *config.Config {
	cfg := config.Default()
	cfg.Audio.MasterVolume = 80
	cfg.Radio = config.Radio{Enabled: true, Station: "hitfm", Volume: 50, Weekdays: []int{1, 2, 3, 4, 5}, From: "08:00", To: "18:00"}
	return cfg
}

func TestStreamIsDueInsideItsWindow(t *testing.T) {
	url, ok := radio.Due(onCfg(), wednesdayNoon, radio.State{Synced: true})
	if !ok {
		t.Fatal("the radio is not due on a weekday at noon inside 08:00–18:00")
	}
	if want, _ := radio.Lookup("hitfm"); url != want.URL {
		t.Errorf("url = %q, want %q", url, want.URL)
	}
}

func TestStreamIsNotDue(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*config.Config)
		at     time.Time
		st     radio.State
	}{
		"switched off":         {func(c *config.Config) { c.Radio.Enabled = false }, wednesdayNoon, radio.State{Synced: true}},
		"before the window":    {nil, wednesdayNoon.Add(-4*time.Hour - time.Minute), radio.State{Synced: true}},
		"at the window's end":  {nil, wednesdayNoon.Add(6 * time.Hour), radio.State{Synced: true}},
		"on a day not chosen":  {nil, wednesdayNoon.AddDate(0, 0, 3), radio.State{Synced: true}},
		"unknown station":      {func(c *config.Config) { c.Radio.Station = "gone" }, wednesdayNoon, radio.State{Synced: true}},
		"custom file url":      {func(c *config.Config) { c.Radio.Station, c.Radio.URL = radio.Custom, "file://localhost/etc/passwd" }, wednesdayNoon, radio.State{Synced: true}},
		"unparseable window":   {func(c *config.Config) { c.Radio.From = "" }, wednesdayNoon, radio.State{Synced: true}},
		"clock not yet synced": {nil, wednesdayNoon, radio.State{Synced: false}},
		"inside quiet hours": {func(c *config.Config) {
			c.QuietHours = config.QuietHours{Enabled: true, From: "11:00", To: "13:00", AppliesTo: []string{config.CategoryScheduled}}
		}, wednesdayNoon, radio.State{Synced: true}},
		"a raid, paused for one": {func(c *config.Config) { c.Radio.PauseDuringAlert = true }, wednesdayNoon, radio.State{Synced: true, Alert: true}},
	} {
		cfg := onCfg()
		if tc.mutate != nil {
			tc.mutate(cfg)
		}
		if _, ok := radio.Due(cfg, tc.at, tc.st); ok {
			t.Errorf("%s: the radio is due", name)
		}
	}
}

// The operator's choice, and the default: between announcements of a raid the
// radio plays on, unless they ask for it to pause until the all-clear.
func TestARaidDoesNotStopTheRadioUnlessAskedTo(t *testing.T) {
	if _, ok := radio.Due(onCfg(), wednesdayNoon, radio.State{Synced: true, Alert: true}); !ok {
		t.Fatal("a raid stopped the radio with PauseDuringAlert off")
	}
}

// Quiet hours silence the radio whatever they are set to cover: the categories
// are about announcements, and an operator who asked for quiet at night did
// not mean "quiet, except for music".
func TestQuietHoursCoveringOnlyAlertsStillSilenceTheRadio(t *testing.T) {
	cfg := onCfg()
	cfg.QuietHours = config.QuietHours{Enabled: true, From: "11:00", To: "13:00", AppliesTo: nil}
	if _, ok := radio.Due(cfg, wednesdayNoon, radio.State{Synced: true}); ok {
		t.Fatal("quiet hours with no categories let the radio play")
	}
}

// What reaches the journal. A stream of the operator's own can carry a
// password or a token, and a log line is not where either belongs.
func TestRedactKeepsTheStationAndDropsTheSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"https://online.hitfm.ua/HitFM":                         "https://online.hitfm.ua/HitFM",
		"https://user:secret@stream.example/live?token=abc&x=1": "https://stream.example/live",
		"not a url %": "",
	} {
		if got := radio.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
