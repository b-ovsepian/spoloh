package config

// Redacted returns a deep copy with the Auth block zeroed — the only
// sanctioned way to hand a configuration to anything that renders it, writes
// it out, or sends it anywhere.
//
// Auth is redacted as a block rather than field by field. Session cookies are
// signed with HMAC(SessionSecret, PasswordHash): either value alone is a
// credential, both together reproduce every live session, and a field added
// to Auth later will be a credential too. Zeroing the struct means the next
// person does not have to remember that.
//
// Nothing else is removed. The access point password looks like a secret and
// is deliberately kept: the network page shows it because an operator who
// lost the installer's output has nowhere else to read it, and it protects a
// setup network rather than this device's administration.
func (c *Config) Redacted() *Config {
	out := c.Clone()
	out.Auth = Auth{}
	return out
}
