package engine

import "time"

// Status is a point-in-time snapshot of what the dashboard shows. Every field
// is a copy; nothing in it aliases engine state.
type Status struct {
	Region      string
	Alert       bool
	Since       time.Time
	LastSuccess time.Time
	LastError   string
	SourceName  string
	Failing     bool

	ClockSynced bool

	NextEventName string
	NextEventAt   time.Time

	PlayingEventID string
	PlayingSound   string
}

// Status is safe to call from any goroutine.
func (e *Engine) Status() Status {
	cfg := e.d.Config()
	now := e.d.Clock.Now()

	e.mu.RLock()
	s := Status{
		Region:      cfg.Alerts.Region,
		Alert:       e.alert,
		Since:       e.alertSince,
		LastSuccess: e.lastSuccess,
		LastError:   e.lastError,
		SourceName:  e.sourceName,
		Failing:     e.failing,
	}
	e.mu.RUnlock()

	// Each of these owns its own synchronisation: the sync checker caches
	// behind a mutex, NextUp is a pure function of the configuration snapshot,
	// and the player guards its current request.
	if e.d.Sync != nil {
		s.ClockSynced = e.d.Sync.Synced()
	}
	if ev, at, ok := e.sched.NextUp(now); ok {
		s.NextEventName = ev.Name
		if s.NextEventName == "" {
			s.NextEventName = ev.ID
		}
		s.NextEventAt = at
	}
	if req, ok := e.d.Player.Current(); ok {
		s.PlayingEventID = req.EventID
		s.PlayingSound = req.Path
	}
	return s
}
