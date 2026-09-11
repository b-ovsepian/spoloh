// Package rules decides what a trigger should actually play.
package rules

import (
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
	// Quiet hours suppress exactly the categories listed in
	// cfg.QuietHours.AppliesTo, alerts included when the operator put them
	// there. There is nothing behind this check.
	//
	// There used to be. This was gated on ev.Category() itself, so that no
	// configuration — stored, hand-edited or submitted — could keep an air
	// raid alert silent; quiet hours silencing an alert was a bug in this
	// codebase, found against spec §5.1 and fixed. The operator asked for
	// the gate to come off after running the device on a wall and knowing
	// what it costs, and that is theirs to decide. Say plainly what this
	// build now does, because a stale comment promising the old guarantee
	// would be worse than none: **between the configured hours, with alerts
	// in AppliesTo, this device will not sound an air raid alert.**
	//
	// The care that went into the old guarantee did not go away with it; it
	// moved. Three things hold instead, and none of them is optional:
	//
	//   - It cannot be reached by accident. /events will not add
	//     CategoryAlerts to AppliesTo without a typed word, the same shape
	//     /log's clear button uses, for the same reason: this device is
	//     configured from a phone held next to it, and two taps in roughly
	//     the same place is what a mis-tap is.
	//   - It is visible while it is on, on the events form and as a standing
	//     warning on the home page — not a hint, a sentence.
	//   - Every alert it silences is written to the event log. Suppressions
	//     below is what the engine records that from. A device that goes
	//     quiet during a raid and cannot say afterwards that it chose to is
	//     indistinguishable from a broken one.
	//
	// config.Validate still accepts every AppliesTo value a stored file can
	// hold, and still must: refusing to start over this field would trade a
	// suppressed alert for a dead announcer, which is the worse of the two.
	// That principle is unchanged — it is the reason this check reads the
	// list rather than the reason it used to ignore it.
	if InQuietHours(cfg.QuietHours, at) && cfg.QuietHours.Covers(ev.Category()) {
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

// Suppression is one alert that quiet hours kept silent, and that the device
// therefore owes the operator an account of.
type Suppression struct {
	EventID string
	Sound   string
}

// Suppressions returns every alert of this trigger that quiet hours silenced
// at at, for the caller to record.
//
// Alerts only, deliberately. A scheduled announcement missed at night is the
// feature working as asked and would bury the rows that matter under routine
// ones; an air raid alert missed at night is the operator standing in front
// of this device the next morning asking why it said nothing at 03:40, with
// the event log as the only place an answer can come from.
//
// A disabled event is not reported. It was not silenced by quiet hours — it
// was switched off, which the home screen already warns about — and blaming
// the window for it would make every other row here less trustworthy.
//
// This recomputes instead of riding along with Resolve's return value. What
// Resolve hands back is what the caller plays; a suppressed event travelling
// in that slice would be one careless loop away from being announced, which
// is precisely the direction this package must never fail in.
func Suppressions(cfg *config.Config, trigger string, at time.Time) []Suppression {
	if !InQuietHours(cfg.QuietHours, at) || !cfg.QuietHours.Covers(config.CategoryAlerts) {
		return nil
	}
	var out []Suppression
	for _, ev := range cfg.Events {
		if ev.Trigger != trigger || !ev.Enabled || ev.Category() != config.CategoryAlerts {
			continue
		}
		out = append(out, Suppression{EventID: ev.ID, Sound: ev.Sound})
	}
	return out
}
