package netmgr

import (
	"sync"
	"time"
)

// staleAfter is how long a published status is still worth acting on.
//
// The supervisor is the only publisher and it reads once per tickInterval, so
// a fresh value is never more than a few seconds old. It stops publishing for
// three reasons — a status read that failed (a board installed on an OS with
// no NetworkManager fails every one of them, forever), a join holding the
// gate, or the supervisor's goroutine being gone — and not one of those is
// evidence about the network. A reader must not act on a value from such a
// run indefinitely, because the thing this answer is used for is stopping the
// alert poll, and a device that has stopped polling on a stale belief is a
// device that does not hear the next raid.
//
// The bound has to clear one legitimately slow tick: tickInterval plus
// tickDeadline is 35 seconds, so anything at or below that would flap on a
// single nmcli call that took its whole allowance. A minute clears that with
// room for the retry behind it, and still hands polling back within a minute
// of the supervisor going quiet. Erring long costs a minute of requests
// nobody needed; erring short costs only churn — which is the direction to
// err in, and why this is a bound at all rather than "the last value,
// forever".
const staleAfter = time.Minute

// StatusFeed carries the network status the supervisor last read, so the rest
// of the daemon can act on the device's networking without running an nmcli
// of its own.
//
// One publisher is the whole point. internal/engine and internal/web both
// need to know whether this device is up on its own access point with nothing
// above it; asking NetworkManager separately — once a second from the poll
// loop, once per page load from the dashboard — would be two more answers
// that can disagree with the supervisor's, and the supervisor's is the one
// that decides whether the access point stays up at all. It would also put
// an nmcli invocation on the tick of a daemon that runs on a Pi, which is
// exactly what tickInterval exists to avoid.
//
// It is the same shape as JoinGate and for the same reasons: a small value
// shared between the supervisor's goroutine and somebody else's, carrying its
// own mutex, and tolerating a nil receiver so that a wiring with no
// supervisor behind it — this package's own tests, internal/web's, a daemon
// started without one — reads as "nobody has said", never as a confident
// answer.
type StatusFeed struct {
	mu sync.Mutex
	st Status
	// at is when st was read, or the zero time when nothing has ever been
	// published. Zero is not "a long time ago": it means no read has ever
	// succeeded, which is a different claim from a reading that has aged
	// out, even though both have to be answered the same way.
	at time.Time
}

// Publish records one successfully read status, observed at now. Callers
// publish only what a manager actually returned: a failed read is not a
// status, and writing a zero value for one would say "no network, no
// hotspot" — a confident answer built out of not knowing.
func (f *StatusFeed) Publish(st Status, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st, f.at = st, now
}

// APOnly reports whether the device is serving its own access point and has
// no upstream: the state in which there is nothing upstream to poll, and
// nobody can reach the device except over that access point.
//
// It is deliberately neither "not Connected" nor "HotspotUp" on its own. The
// two are independent — see Status — and a board on a wall port in a closet
// with its access point still up has an upstream and must keep polling.
//
// Unknown reads as false, both before the first publish and after the last
// one has aged out, and so does a clock that has stepped backwards past the
// reading (these boards have no RTC, so NTP corrects the clock shortly after
// boot and clock.System.Now strips the monotonic reading, making that a pure
// backwards jump in the values compared here). Every one of those is "nobody
// can currently say", and the only safe way to answer that question is the
// one that leaves the device polling.
func (f *StatusFeed) APOnly(now time.Time) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at.IsZero() {
		return false
	}
	if age := now.Sub(f.at); age < 0 || age > staleAfter {
		return false
	}
	return f.st.HotspotUp && !f.st.Connected
}
