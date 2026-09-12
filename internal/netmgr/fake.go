package netmgr

import (
	"context"
	"sync"
)

// Call is one recorded invocation against a Fake, kept for tests that assert
// on what was asked of the manager rather than only on its return values.
type Call struct {
	Method string
	SSID   string
	PSK    string
}

// Fake is an in-memory Manager for tests. A supervisor and an HTTP handler
// are both expected to hold the same instance, so every method takes mu; use
// Fake through a pointer.
type Fake struct {
	// Networks is what Scan returns on success.
	Networks []Network
	// CurrentStatus is what Status returns, and what Connect and the
	// hotspot methods update on success, mirroring how the real device
	// reports the network it just joined.
	CurrentStatus Status

	StatusErr       error
	ScanErr         error
	ConnectErr      error
	StartHotspotErr error
	StopHotspotErr  error

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

func (f *Fake) Status(ctx context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "Status"})
	if f.StatusErr != nil {
		return Status{}, f.StatusErr
	}
	return f.CurrentStatus, nil
}

func (f *Fake) Scan(ctx context.Context) ([]Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "Scan"})
	if f.ScanErr != nil {
		return nil, f.ScanErr
	}
	out := make([]Network, len(f.Networks))
	copy(out, f.Networks)
	return out, nil
}

// Rescan records itself under its own method name so a test can tell which
// of the two a caller asked for — the distinction is the entire point of
// having both, and a fake that blurred it would let the refresh button go
// back to reading a cache with nothing failing.
func (f *Fake) Rescan(ctx context.Context) ([]Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "Rescan"})
	if f.ScanErr != nil {
		return nil, f.ScanErr
	}
	out := make([]Network, len(f.Networks))
	copy(out, f.Networks)
	return out, nil
}

// Connect records the attempt and, on success, updates CurrentStatus to
// reflect a device now joined to ssid — the behaviour a real one has.
func (f *Fake) Connect(ctx context.Context, ssid, psk string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "Connect", SSID: ssid, PSK: psk})
	if f.ConnectErr != nil {
		return f.ConnectErr
	}
	f.CurrentStatus.Connected = true
	f.CurrentStatus.SSID = ssid
	return nil
}

func (f *Fake) StartHotspot(ctx context.Context, ssid, psk string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "StartHotspot", SSID: ssid, PSK: psk})
	if f.StartHotspotErr != nil {
		return f.StartHotspotErr
	}
	// NMCLI refuses an empty password rather than raising an open access
	// point; a fake that accepted one would let a test pass against a device
	// configuration that cannot work.
	if psk == "" {
		return errEmptyAPPassword
	}
	f.CurrentStatus.HotspotUp = true
	return nil
}

func (f *Fake) StopHotspot(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "StopHotspot"})
	if f.StopHotspotErr != nil {
		return f.StopHotspotErr
	}
	f.CurrentStatus.HotspotUp = false
	return nil
}
