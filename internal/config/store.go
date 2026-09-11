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
