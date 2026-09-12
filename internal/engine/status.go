package engine

import (
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
)

// SourceHealth is one configured cascade key as the last completed poll
// found it: the key the operator chose, what happened to it, and — when it
// failed — which of alertsource's six reasons it failed for.
//
// It deliberately drops alertsource.Attempt.Err on the way out of the
// engine. Err carries text from the far end, and one of the sources
// authenticates with a Bearer token, so an error string is one wrapped
// error away from putting a credential on a page that anyone on the LAN can
// load. A struct that never carries the text cannot render it, whatever a
// later template does; Reason is a closed vocabulary this repository owns
// and is safe by construction. The Err itself is still logged where the
// poll failed.
type SourceHealth struct {
	Key     string
	Outcome alertsource.Outcome
	Reason  alertsource.Reason
}

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

	// Sources is the cascade as the last poll walked it: one entry per
	// configured key, in the order they are tried, whether or not the poll
	// got that far. Nil before the first poll of the process, and nil for a
	// source that cannot report a cascade at all — both mean "nobody has
	// asked yet", which is not the same as a cascade of sources that were
	// not reached, and the page says so in words rather than rendering an
	// unchecked list as though it were checked.
	Sources []SourceHealth

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
	// Copied, not aliased, like every other field here: the next poll
	// replaces e.sources wholesale, and a caller still holding the old
	// slice must keep reading the poll it was handed.
	if len(e.sources) > 0 {
		s.Sources = append([]SourceHealth(nil), e.sources...)
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
