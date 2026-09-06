// Package rules decides what a trigger should actually play.
package rules

import (
	"slices"
	"time"

	"github.com/spoloh/spoloh/internal/config"
	"github.com/spoloh/spoloh/internal/player"
)

// Decision is a resolved instruction, ready to hand to the player.
type Decision struct {
	EventID  string
	Sound    string
	Volume   int
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
	// Quiet hours apply only to the categories the operator listed. The default
	// config lists scheduled events only, so alerts sound at any hour.
	if InQuietHours(cfg.QuietHours, at) && slices.Contains(cfg.QuietHours.AppliesTo, ev.Category()) {
		return Decision{}, false
	}

	priority := player.PriorityScheduled
	if ev.Category() == config.CategoryAlerts {
		priority = player.PriorityAlert
	}

	return Decision{
		EventID:  ev.ID,
		Sound:    ev.Sound,
		Volume:   ev.Volume * cfg.Audio.MasterVolume / 100,
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
