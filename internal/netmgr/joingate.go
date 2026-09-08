package netmgr

import (
	"sync"
	"time"
)

// JoinGate lets the dashboard tell the supervisor that a join is in flight.
//
// Without it the two fight over the one radio, and the fight breaks the
// operation this whole package exists to perform. The /network handler gives
// a join up to networkConnectTimeout; the supervisor ticks every
// tickInterval. In between leaving the hotspot and associating with the
// school network the device reports neither Connected nor HotspotUp, and
// downSince has been set since boot — so past the fallback grace period that
// mid-join moment is exactly the state that makes the supervisor raise an
// access point on top of the join it should be waiting for. Association plus
// a DHCP lease takes tens of seconds, so essentially every real join spends
// several ticks there.
//
// The hold is a deadline rather than a flag, deliberately. If the goroutine
// that took the gate never reaches its release — the handler's detached join
// panicking, or some shape of failure nobody thought of — a flag would wedge
// the supervisor forever and the device could never raise an access point
// again: a broken join traded for a permanently unreachable board, which is
// the worse of the two. A deadline expires on its own, so the worst case is
// that the supervisor sits out one join's worth of ticks and then resumes.
//
// It takes its times from its callers rather than holding a clock, the way
// Supervisor takes one from its constructor: the handler passes time.Now()
// and a test passes fake-clock times.
//
// The zero value is a valid, unheld gate, and a nil *JoinGate is never held.
// Not every construction has one — this package's own tests, and any wiring
// that has no dashboard behind it — and "no gate" has to mean "no join in
// flight", never "held forever".
type JoinGate struct {
	mu sync.Mutex
	// until is when the hold expires, or the zero time when nothing is
	// holding the gate. The mutex is not decoration: the dashboard's handler
	// goroutine writes this while the supervisor's own goroutine reads it.
	until time.Time
}

// HoldUntil holds the gate until t and returns that deadline as a token for
// Release. The most recent call wins, whether it extends or shortens the
// hold: the caller that just set it is the one that knows what it is about to
// do.
func (g *JoinGate) HoldUntil(t time.Time) time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = t
	return t
}

// Release drops the hold that HoldUntil returned, and only that one. Callers
// release on every path out of a join, success and failure alike — the
// deadline is the backstop for the paths nobody thought of, not the ordinary
// way a hold ends.
//
// It is scoped to its own token because two joins can overlap: an operator
// double-submitting, or two people on the access point at once. An
// unconditional release would let the first join to finish drop the hold
// protecting the second, handing the supervisor back exactly the mid-join
// moment this gate exists to cover. A release whose token is no longer the
// live deadline has been superseded and does nothing; the newer hold expires
// on its own.
func (g *JoinGate) Release(token time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.Equal(token) {
		g.until = time.Time{}
	}
}

// Held reports whether a join is in flight as of now.
func (g *JoinGate) Held(now time.Time) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.until.IsZero() && now.Before(g.until)
}
