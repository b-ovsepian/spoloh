package netmgr

import (
	"sync"
	"time"
)

// Activity records when the dashboard last served a request, so the
// supervisor can tell an operator working on the setup screen from an access
// point nobody is using.
//
// It counts requests, not associated clients, on purpose. A phone that once
// joined Spoloh-Setup and now sits in someone's pocket nearby stays
// associated for as long as it is in range, and if that held the access point
// up the device would never go back to the network it remembers — a device
// on a wall, a few metres from its own router, receiving no alerts. A pocket
// makes no requests; an operator reading the setup screen does. It also needs
// no command to read: who is associated is `iw station dump`, a binary this
// package does not otherwise depend on and that a minimal image may not have.
//
// Which network a request arrived on is not checked, because it does not need
// to be: the supervisor only consults this while the device is serving its
// access point with nothing above it, and then the access point is the only
// way to reach the dashboard at all.
//
// Like JoinGate it takes its times from its callers rather than holding a
// clock. The zero value is valid and has seen nothing, and a nil *Activity is
// never recent: "no dashboard" must mean "nobody to wait for", never "always
// busy".
type Activity struct {
	mu sync.Mutex
	// last is when the most recent request arrived, or the zero time if none
	// has. The mutex is not decoration: every request handler writes this
	// while the supervisor's goroutine reads it.
	last time.Time
}

// Touch records a request arriving at now.
func (a *Activity) Touch(now time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last = now
}

// Within reports whether a request arrived less than d before now. A request
// stamped after now counts as recent: that is a clock stepped backwards since,
// and the request it stamped was a moment ago, not hours from now. An untouched
// Activity needs no case of its own: Sub from the zero time saturates, and
// nothing is within d of the year 1.
func (a *Activity) Within(now time.Time, d time.Duration) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Sub(a.last) < d
}
