package config

// Clone returns a deep copy. Every slice is copied, so a caller can mutate the
// result without reaching into the original — which is what lets a store hand
// the same configuration to the engine's goroutine and an HTTP handler at the
// same time without either seeing the other's half-finished edit.
//
// Only slices need the work. Everything else — the alerts.in.ua token and the
// location UID included — is copied by the struct assignment below, because a
// Go string shares its backing array but can never be written through it. A
// credential kept as a []byte would not be safe that way, and would belong in
// the list below; the webhook headers are there for exactly that reason.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c

	out.Alerts.Sources = append([]string(nil), c.Alerts.Sources...)
	out.QuietHours.AppliesTo = append([]string(nil), c.QuietHours.AppliesTo...)

	out.Events = make([]Event, len(c.Events))
	for i, ev := range c.Events {
		out.Events[i] = ev
		out.Events[i].Weekdays = append([]int(nil), ev.Weekdays...)
	}

	out.Radio.Weekdays = append([]int(nil), c.Radio.Weekdays...)

	// Nil stays nil rather than becoming an empty slice: Webhooks is
	// omitempty, and a clone that turned "no webhooks" into "webhooks: []"
	// would rewrite config.json on the next save of any unrelated setting.
	if c.Webhooks != nil {
		out.Webhooks = make([]Webhook, len(c.Webhooks))
		for i, w := range c.Webhooks {
			out.Webhooks[i] = w
			out.Webhooks[i].Headers = append([]WebhookHeader(nil), w.Headers...)
		}
	}
	return &out
}
