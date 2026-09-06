package config

// Clone returns a deep copy. Every slice is copied, so a caller can mutate the
// result without reaching into the original — which is what lets a store hand
// the same configuration to the engine's goroutine and an HTTP handler at the
// same time without either seeing the other's half-finished edit.
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
	return &out
}
