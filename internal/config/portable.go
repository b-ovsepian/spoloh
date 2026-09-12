package config

import "encoding/json"

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
