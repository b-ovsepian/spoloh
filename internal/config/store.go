package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/b-ovsepian/spoloh/internal/atomicfile"
)

// Load reads and validates a config file. A missing file returns an error for
// which os.IsNotExist reports true, so callers can seed a default.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Runs here, not in Validate: Validate must stay free of side effects, a
	// mutation hiding inside a validator is a trap for every later caller.
	// It runs on every path that turns stored bytes into a *Config, rather
	// than only the one caller happens to use today, because that is
	// exactly the guarantee a config import (or any future loader) needs
	// without having to remember to ask for it separately.
	restoreMissingAlertEvents(&c)
	restoreMissingRepeat(&c)
	raisePollIntervalToMinimum(&c)
	repairWebhookPhases(&c)
	lowerWebhookDelaysToMaximum(&c)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &c, nil
}

// restoreMissingAlertEvents appends alert_start and/or alert_end from
// Default() when either trigger has no event in c.Events, and logs at Error
// which one it restored.
//
// A device missing one has nothing to say and no way to say it:
// config.Validate accepts an empty, or alert-less, Events list outright, and
// rules.Resolve silently produces zero decisions for a trigger with no
// matching event — the daemon starts, runs, and never announces an air
// raid, with nothing on the home page to say why. Requiring the event in
// Validate was considered and rejected: it would refuse to start the daemon
// entirely over a config a dashboard could otherwise still fix. Restoring it
// here is the same principle the rest of this daemon already follows —
// degrade to working, never to silent, and say so loudly — and it leaves
// the supported way to stop alert announcements exactly where it was:
// enabled: false, which this leaves untouched and which the dashboard's
// home page already warns about (see web.alertStartDisabled).
//
// Only ever adds. An event of either trigger already present — disabled or
// not, whatever its sound, volume or ID — is left exactly as loaded; this
// repairs "missing", never "different from the default".
func restoreMissingAlertEvents(c *Config) {
	for _, trigger := range []string{TriggerAlertStart, TriggerAlertEnd} {
		if hasEventTrigger(c.Events, trigger) {
			continue
		}
		restored := defaultEvent(trigger)
		c.Events = append(c.Events, restored)
		slog.Default().Error(
			"restored a missing alert event: the device could not have announced without it",
			"trigger", trigger, "id", restored.ID,
		)
	}
}

// restoreMissingRepeat sets Repeat to 1 wherever it loaded as the Go zero
// value. It is the same repair restoreMissingAlertEvents makes for a missing
// alert event, run for the same reason and at the same point in Load, just
// before Validate: a config.json written before Event.Repeat existed has no
// "repeat" key at all, so json.Unmarshal leaves it at 0, and Event.Repeat's
// doc comment is explicit that 0 must never be allowed to mean "never play
// this" on a device where a missing setting must never mean silence.
//
// Only ever raises a 0 to 1. An event whose repeat was actually saved as
// something else is left exactly as loaded — this repairs "missing", not
// "different from the default", exactly like its precedent.
func restoreMissingRepeat(c *Config) {
	for i := range c.Events {
		if c.Events[i].Repeat == 0 {
			c.Events[i].Repeat = 1
		}
	}
}

// raisePollIntervalToMinimum raises an alerts.poll_interval below
// MinPollInterval to exactly that minimum, and logs at Error that it did. It
// is the same repair the two above make, run at the same point in Load and
// for the same reason.
//
// MinPollInterval has already been raised once — 3s to 10s, after every
// source in the cascade rate-limited a device polling every 5 seconds — so
// every config.json written before that holds a value Validate now refuses.
// Rejecting them means the daemon will not start, which is to say it would
// silence precisely the devices that were already announcing, over a number
// the dashboard could have corrected in one field. Refusing to start is the
// worse outcome: degrade to working, never to silent, and say so loudly.
//
// It clamps to the minimum, not to Default()'s interval. The minimum is the
// smallest change that makes the stored config legal, and an operator who
// deliberately chose a fast poll is left with the fastest one still allowed
// rather than a number nobody asked for. A poll_interval missing from the
// file entirely unmarshals as 0 and lands on the same floor.
//
// Only ever raises. An interval at or above the minimum is left exactly as
// loaded — this repairs "no longer allowed", never "different from the
// default", exactly like its precedents.
func raisePollIntervalToMinimum(c *Config) {
	stored := c.Alerts.PollInterval.Std()
	if stored >= MinPollInterval {
		return
	}
	c.Alerts.PollInterval = Duration(MinPollInterval)
	slog.Default().Error(
		"raised alerts.poll_interval to the minimum: the stored value would rate-limit every alert source",
		"stored", stored, "using", MinPollInterval,
	)
}

// repairWebhookPhases rewrites a stored webhook phase this build does not
// recognise to PhaseOnTrigger, and logs at Error that it did. It is the same
// repair its three neighbours above make, run at the same point in Load and
// for the same reason.
//
// The reason is narrower here than for an alert event, and worth saying
// plainly. ValidateWebhook refuses an unknown phase, and internal/webhook
// refuses to fire any webhook ValidateWebhook refuses — so without this, one
// mistyped word in a hand-edited config.json would cost the operator that
// webhook entirely, silently, at the moment it mattered. PhaseOnTrigger is
// the phase every webhook had before phases existed, and the one that fires
// unconditionally; a phase nobody can place is better answered by firing at
// the old moment than by not firing at all.
//
// Only ever rewrites what it cannot place. A phase this build understands is
// left exactly as loaded — this repairs "not a phase", never "not the phase
// I would have chosen", exactly like its precedents.
func repairWebhookPhases(c *Config) {
	for i := range c.Webhooks {
		switch c.Webhooks[i].Phase {
		case "", PhaseOnTrigger, PhaseBefore, PhaseAfter:
			continue
		}
		slog.Default().Error(
			"a webhook names a phase this build does not know; it will fire when the trigger happens",
			"webhook", c.Webhooks[i].ID, "stored", c.Webhooks[i].Phase, "using", PhaseOnTrigger,
		)
		c.Webhooks[i].Phase = PhaseOnTrigger
	}
}

// lowerWebhookDelaysToMaximum brings a stored webhook delay outside
// 0..MaxWebhookDelay back inside it, and logs at Error that it did.
//
// It is raisePollIntervalToMinimum's shape, pointed the other way, and for
// the same reason: the value is refused by Validate, Validate is what every
// load runs through, and a daemon that will not start is the one outcome
// worse than a corrected number. The direction is the difference. A poll
// interval is raised because too small a one rate-limits the source; a delay
// is lowered because too large a one is a siren held back, and the cap is
// the most this device is willing to hold it (see MaxWebhookDelay). A
// negative delay — which only a hand-edited file can hold — lands on zero
// and the sound starts at once.
//
// Only ever clamps. A delay already inside the range is left exactly as
// loaded, exactly like its precedents.
func lowerWebhookDelaysToMaximum(c *Config) {
	for i := range c.Webhooks {
		stored := c.Webhooks[i].Delay.Std()
		switch {
		case stored < 0:
			c.Webhooks[i].Delay = 0
		case stored > MaxWebhookDelay:
			c.Webhooks[i].Delay = Duration(MaxWebhookDelay)
		default:
			continue
		}
		slog.Default().Error(
			"brought a webhook delay back inside the allowed range: the stored value would have held a sound back too long",
			"webhook", c.Webhooks[i].ID, "stored", stored, "using", c.Webhooks[i].Delay.Std(),
		)
	}
}

func hasEventTrigger(events []Event, trigger string) bool {
	for _, e := range events {
		if e.Trigger == trigger {
			return true
		}
	}
	return false
}

// defaultEvent returns Default()'s event for the given trigger. It panics if
// trigger names one Default() does not define — a bug in this package, not a
// runtime condition restoreMissingAlertEvents' one caller needs to handle.
func defaultEvent(trigger string) Event {
	for _, e := range Default().Events {
		if e.Trigger == trigger {
			return e
		}
	}
	panic("config: Default() defines no event for trigger " + trigger)
}

// Save validates then writes the config atomically. A power cut mid-write
// leaves the previous file intact, which matters on an appliance that loses
// mains power without warning; so does an invalid config, which is rejected
// before anything is written at all.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to save an invalid config: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	return atomicfile.Write(path, data, 0o600)
}
