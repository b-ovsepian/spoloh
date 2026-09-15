package config

// Redacted returns a deep copy with every credential this process holds
// removed — the only sanctioned way to hand a configuration to anything that
// renders it, writes it out, or sends it anywhere.
//
// Auth is redacted as a block rather than field by field. Session cookies are
// signed with HMAC(SessionSecret, PasswordHash): either value alone is a
// credential, both together reproduce every live session, and a field added
// to Auth later will be a credential too. Zeroing the struct means the next
// person does not have to remember that.
//
// Alerts cannot be treated the same way, and the difference is worth stating
// rather than leaving as an inconsistency. Auth is nothing but secrets, so
// zeroing it costs nothing; Alerts is the oblast, the location, the cascade
// and the poll interval, which are exactly what a redacted configuration
// exists to show. So each provider key is named here, by hand — and what
// replaces the block trick is not care but a test:
// TestRedactedRemovesEveryFieldThatLooksLikeACredential walks Config by
// reflection and fails, naming the field, on any secret-shaped value this
// function leaves behind. Add the next credential anywhere and forget this
// line, and it is a red suite rather than a leak.
//
// Webhook header values are removed, and they are the reason name-shaped
// detection is not enough on its own. The field is called Value, which looks
// like nothing, and it routinely holds an Authorization header — the
// operator's example for this feature was a Basic credential against a box
// on their own network. Nothing about the name says so; only what operators
// put there does.
//
// The name is emptied with it. "Authorization" alone leaks nothing, but a
// header whose value is gone and whose name survives reads, on the screen
// this exists to feed, as a header that was configured with no value — and
// somebody will helpfully fill it in.
//
// Nothing else is removed. The access point password looks like a secret and
// is deliberately kept: the network page shows it because an operator who
// lost the installer's output has nowhere else to read it, and it protects a
// setup network rather than this device's administration.
func (c *Config) Redacted() *Config {
	out := c.Clone()
	out.Auth = Auth{}
	out.Alerts.AlertsUAToken = ""
	out.Alerts.UkraineAlarmToken = ""
	for i := range out.Webhooks {
		for j := range out.Webhooks[i].Headers {
			out.Webhooks[i].Headers[j] = WebhookHeader{}
		}
	}
	return out
}
