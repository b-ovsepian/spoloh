package config

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Export renders a configuration for an operator to keep.
//
// withSecrets is the operator's choice and covers the credentials they
// supplied: the alerts.in.ua token, webhook header values, the access point
// password. A backup meant for the same device wants them; a configuration
// handed to somebody else does not.
//
// Auth is outside that choice and is never written, at either setting. It is
// not a key anybody typed — it is a password hash and a session secret this
// installation derived, and either one is enough to forge a live session.
// Import never reads them back for the same reason, so exporting them would
// be a hazard that buys nothing.
func Export(c *Config, withSecrets bool) ([]byte, error) {
	out := c.Clone()
	if !withSecrets {
		// Redacted clones again; taking its result rather than layering on
		// the clone above keeps one source of truth for what a credential
		// is, which is the whole point of that function existing.
		out = c.Redacted()
	}
	out.Auth = Auth{}
	// Monitoring follows Auth for the same reason: it belongs to this
	// device, and Import never reads it back.
	out.Monitoring = Monitoring{}
	if !withSecrets {
		// Redacted keeps the access point password on purpose — the network
		// page shows it, because an operator who lost the installer's output
		// has nowhere else to read it. A file that leaves this device is a
		// different audience from a screen on it.
		out.Network.APPassword = ""
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// The two ways an import fails, told apart so a caller can say which one
// happened in words an operator can act on: the wrong file entirely is a
// different problem from the right file carrying a value this build refuses,
// and they have different answers — pick another file, or fix the setting on
// the screen that owns it.
var (
	ErrImportUnreadable = errors.New("the file is not a stored configuration")
	ErrImportUnusable   = errors.New("the configuration would not load")
)

// Import folds an exported configuration into the running one and returns
// the result. It writes nothing; the caller stores it.
//
// Auth is taken from current and never from data. An import that carried a
// password hash would lock the operator out of their own device with a
// password they have never seen, and a session secret from a file would
// revive whatever sessions the device it came from had open.
//
// repairStored runs first, exactly as it does in Load, because a backup is
// only a backup if a file written by an older build reads back: the repairs
// are what turn a poll interval from before the minimum was raised, or an
// event saved before Repeat existed, into something Validate accepts
// instead of something a restore is refused over.
//
// Validate then runs before anything is returned, because the caller is
// about to write this to the file the daemon starts from. A configuration
// that fails to load is a device that announces nothing.
func Import(current *Config, data []byte) (*Config, error) {
	var in Config
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImportUnreadable, err)
	}
	in.Auth = current.Auth
	// The monitoring token too: the monitoring system is configured against
	// this board, so a restore keeps it — whether the backup carried no keys
	// or came from another board.
	in.Monitoring = current.Monitoring
	repairStored(&in)
	if err := in.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImportUnusable, err)
	}
	return &in, nil
}
