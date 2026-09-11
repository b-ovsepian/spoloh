package engine

import (
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
)

// Status is a point-in-time snapshot of what the dashboard shows. Every field
// is a copy; nothing in it aliases engine state.
//
// Region, NextEventName and NextEventAt are the presentation layer's to fill,
// not the engine's — see Status() for why. They live on this struct because
// it is also the view model the dashboard's templates read.
type Status struct {
	Region      string
	Alert       bool
	Since       time.Time
	LastSuccess time.Time
	LastError   string
	SourceName  string
	Failing     bool

	// AllClearSeen and AllClearNeeded expose an all-clear that upstream has
	// reported and this device is not announcing yet: how many consecutive
	// calm readings have been seen, and how many it takes. Seen is zero
	// unless something is actually being held back, so a template gates on
	// it; Needed is here because a page cannot read a Go constant.
	//
	// It belongs on the dashboard rather than only in the journal because
	// the screen is the operator's whole diagnostic surface on a board with
	// no monitor, and this is the one state in which the device and upstream
	// visibly disagree. Without it, "alerts.in.ua says it is over and my
	// device still says тривога" looks like a device that has stopped
	// working, which is the reading that gets it unplugged — during a raid,
	// by someone who has concluded it is broken.
	AllClearSeen   int
	AllClearNeeded int

	ClockSynced bool

	NextEventName string
	NextEventAt   time.Time

	PlayingEventID string
	PlayingSound   string
}

// Status is safe to call from any goroutine.
//
// It reports only what the engine alone knows. Region and the next scheduled
// event are pure functions of the configuration, and web.status() already
// computes both — correctly, in the configured location, which is the
// constraint that governs how times render. Computing them here too gave two
// implementations of one value reading two different clocks (the engine's
// e.d.Clock.Now() and the web layer's s.now().In(loc)), and had publishLoop
// allocating a scheduler.Runner and rescanning the event list every second
// for an answer it then threw away.
func (e *Engine) Status() Status {
	e.mu.RLock()
	s := Status{
		Alert:       e.alert,
		Since:       e.alertSince,
		LastSuccess: e.lastSuccess,
		LastError:   e.lastError,
		SourceName:  e.sourceName,
		Failing:     e.failing,

		AllClearSeen:   e.allClearSeen,
		AllClearNeeded: alertstate.EndConfirmations,
	}
	e.mu.RUnlock()

	// Each of these owns its own synchronisation: the sync checker caches
	// behind a mutex, and the player guards its current request.
	if e.d.Sync != nil {
		s.ClockSynced = e.d.Sync.Synced()
	}
	if req, ok := e.d.Player.Current(); ok {
		s.PlayingEventID = req.EventID
		s.PlayingSound = req.Path
	}
	return s
}
