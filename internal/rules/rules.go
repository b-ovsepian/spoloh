// Package rules decides what a trigger should actually play.
package rules

import (
	"slices"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/player"
)

// Decision is a resolved instruction, ready to hand to the player.
type Decision struct {
	EventID  string
	Sound    string
	Volume   int
	Repeat   int
	Priority player.Priority
}

// InQuietHours reports whether t falls inside the configured window. A window
// whose end is before its start wraps around midnight.
func InQuietHours(qh config.QuietHours, t time.Time) bool {
	if !qh.Enabled {
		return false
	}
	from, err := config.ParseHM(qh.From)
	if err != nil {
		return false
	}
	to, err := config.ParseHM(qh.To)
	if err != nil {
		return false
	}
	if from == to {
		return false
	}
	cur := t.Hour()*60 + t.Minute()
	if from < to {
		return cur >= from && cur < to
	}
	return cur >= from || cur < to
}

// ForEvent resolves one event, reporting false when it must not play.
func ForEvent(cfg *config.Config, ev config.Event, at time.Time) (Decision, bool) {
	if !ev.Enabled {
		return Decision{}, false
	}
	// Quiet hours can only ever suppress a scheduled event, never an alert —
	// spec §5.1, "quiet hours never silence an air raid alert". This is
	// gated on ev.Category() itself, not on membership in AppliesTo: that
	// list is operator-editable config, and a device upgrading from before
	// this rule existed (or one hand-edited directly) could carry
	// CategoryAlerts in it. config.Validate deliberately does not reject
	// that value — refusing to start over it would trade a suppressed alert
	// for a dead announcer, the worse of the two — so this is the one place
	// the guarantee actually has to hold. main.go logs once at startup if
	// it finds CategoryAlerts there, so a config carrying it is visible
	// without being fatal.
	if ev.Category() == config.CategoryScheduled &&
		InQuietHours(cfg.QuietHours, at) &&
		slices.Contains(cfg.QuietHours.AppliesTo, config.CategoryScheduled) {
		return Decision{}, false
	}

	priority := player.PriorityScheduled
	if ev.Category() == config.CategoryAlerts {
		priority = player.PriorityAlert
	}

	// Calculate volume: round to nearest, and clamp non-zero inputs to minimum 1.
	// Zero volume (explicit mute) must stay zero, but any non-zero request must not
	// become silent through truncation (e.g., 90*1/100 would truncate to 0).
	volume := 0
	if ev.Volume != 0 && cfg.Audio.MasterVolume != 0 {
		volume = (ev.Volume*cfg.Audio.MasterVolume + 50) / 100
		if volume == 0 {
			volume = 1
		}
	}

	// config.Load (via restoreMissingRepeat) and config.Validate both keep a
	// stored Event's Repeat at 1 or above, but ForEvent takes whatever
	// *config.Config a caller hands it — a config built directly by a test,
	// or by code that has not gone through either of those — and a stray
	// zero reaching engine.play would mean the event never plays at all
	// rather than playing once. The floor belongs here too, not only on
	// disk, for the same reason the brief gives it: a missing repeat count
	// must never mean silence.
	repeat := ev.Repeat
	if repeat < 1 {
		repeat = 1
	}

	return Decision{
		EventID:  ev.ID,
		Sound:    ev.Sound,
		Volume:   volume,
		Repeat:   repeat,
		Priority: priority,
	}, true
}

// Resolve returns every decision for a trigger, in config order.
func Resolve(cfg *config.Config, trigger string, at time.Time) []Decision {
	var out []Decision
	for _, ev := range cfg.Events {
		if ev.Trigger != trigger {
			continue
		}
		if d, ok := ForEvent(cfg, ev, at); ok {
			out = append(out, d)
		}
	}
	return out
}
