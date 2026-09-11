package timedate

import (
	"context"
	"errors"
	"sync"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// ErrNTPEnabled is what systemd answers a set-time request while network
// time synchronisation is on ("Automatic time synchronization is enabled"),
// and what Fake answers the same request. A fake that accepted it would let
// a test pass against a screen that offers the operator a button systemd
// will refuse — which is the exact failure the /time screen's guard exists
// to prevent.
var ErrNTPEnabled = errors.New("cannot set the time by hand while NTP synchronisation is on")

// Call is one recorded invocation against a Fake, kept for tests that assert
// on what was asked of the manager rather than only on its return values.
type Call struct {
	Method string
	// Arg is the single value the method carried: a zone name, "true" or
	// "false", a timestamp, an NTP server. Empty for Show and Zones.
	Arg string
}

// Fake is an in-memory Manager for tests. An HTTP handler and the test that
// drives it both hold the same instance, so every method takes mu; use Fake
// through a pointer.
type Fake struct {
	// CurrentState is what Show returns, and what the setters update on
	// success, mirroring how a real host reports the change it just made.
	CurrentState State
	// ZoneList is what Zones returns on success.
	ZoneList []string
	// Server is the NTP server ApplyNTPServer last accepted.
	Server string

	ShowErr           error
	ZonesErr          error
	SetTimezoneErr    error
	SetNTPErr         error
	SetTimeErr        error
	ApplyNTPServerErr error

	mu    sync.Mutex
	calls []Call
}

var _ Manager = (*Fake)(nil)

// Calls returns the invocations recorded so far, in call order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *Fake) Show(ctx context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "Show"})
	if f.ShowErr != nil {
		return State{}, f.ShowErr
	}
	return f.CurrentState, nil
}

func (f *Fake) Zones(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "Zones"})
	if f.ZonesErr != nil {
		return nil, f.ZonesErr
	}
	return append([]string(nil), f.ZoneList...), nil
}

func (f *Fake) SetTimezone(ctx context.Context, tz string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "SetTimezone", Arg: tz})
	if f.SetTimezoneErr != nil {
		return f.SetTimezoneErr
	}
	f.CurrentState.Timezone = tz
	return nil
}

func (f *Fake) SetNTP(ctx context.Context, on bool) error {
	arg := "false"
	if on {
		arg = "true"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "SetNTP", Arg: arg})
	if f.SetNTPErr != nil {
		return f.SetNTPErr
	}
	f.CurrentState.NTP = on
	return nil
}

// SetTime refuses while NTP is on, the way systemd does.
func (f *Fake) SetTime(ctx context.Context, stamp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "SetTime", Arg: stamp})
	if f.SetTimeErr != nil {
		return f.SetTimeErr
	}
	if f.CurrentState.NTP {
		return ErrNTPEnabled
	}
	return nil
}

// ApplyNTPServer refuses a value config.Validate would refuse, the way
// Timedatectl.ApplyNTPServer does: this is the boundary that writes a file
// root parses, and a fake that skipped the check would let a test prove a
// handler safe that is not.
func (f *Fake) ApplyNTPServer(ctx context.Context, server string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "ApplyNTPServer", Arg: server})
	if f.ApplyNTPServerErr != nil {
		return f.ApplyNTPServerErr
	}
	if err := config.ValidateNTPServer(server); err != nil {
		return err
	}
	f.Server = server
	return nil
}
