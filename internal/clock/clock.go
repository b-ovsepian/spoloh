// Package clock isolates wall-clock time and NTP synchronisation state so the
// scheduler and state machine can be tested without waiting for real time.
package clock

import "time"

// Clock reports the current time. Production code uses System; tests use Fake.
type Clock interface {
	Now() time.Time
}

// System reads the host clock, rendered in Loc.
type System struct {
	Loc *time.Location
}

func (s System) Now() time.Time {
	if s.Loc == nil {
		return time.Now()
	}
	return time.Now().In(s.Loc)
}

// Fake is a manually advanced clock. It is not safe for concurrent use; tests
// drive it from a single goroutine.
type Fake struct {
	t time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{t: t} }

func (f *Fake) Now() time.Time { return f.t }

func (f *Fake) Set(t time.Time) { f.t = t }

func (f *Fake) Advance(d time.Duration) { f.t = f.t.Add(d) }
