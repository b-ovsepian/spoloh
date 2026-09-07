package netmgr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

func supervisor(t *testing.T, f *netmgr.Fake, start time.Time) (*netmgr.Supervisor, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(start)
	cfg := config.Default() // APFallbackAfter is 45s
	return netmgr.NewSupervisor(f, func() *config.Config { return cfg }, clk, nil), clk
}

// calls counts, from f's call log, how many times method was invoked.
// netmgr.Fake logs every call as a netmgr.Call{Method, SSID, PSK}; the tests
// below only ever need the count for one method at a time.
func calls(f *netmgr.Fake, method string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

var boot = time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)

func TestConnectedAtBootNeverRaisesTheHotspot(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{Connected: true, SSID: "ShkolaWiFi"}}
	s, clk := supervisor(t, f, boot)

	for i := 0; i < 300; i++ {
		s.Tick(context.Background())
		clk.Advance(time.Second)
	}
	if calls(f, "StartHotspot") != 0 {
		t.Fatalf("StartHotspot called %d times on a device that was connected throughout", calls(f, "StartHotspot"))
	}
}

func TestHotspotRisesOnlyAfterTheGracePeriod(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{Connected: false}}
	s, clk := supervisor(t, f, boot)

	// 45 seconds of nothing.
	for i := 0; i < 45; i++ {
		s.Tick(context.Background())
		if calls(f, "StartHotspot") != 0 {
			t.Fatalf("hotspot raised after %d seconds, before the 45-second grace period", i)
		}
		clk.Advance(time.Second)
	}
	s.Tick(context.Background()) // the tick at the 45-second mark
	if calls(f, "StartHotspot") != 1 {
		t.Fatalf("StartHotspot called %d times at the grace boundary, want 1", calls(f, "StartHotspot"))
	}

	// And it does not keep raising it.
	for i := 0; i < 60; i++ {
		clk.Advance(time.Second)
		s.Tick(context.Background())
	}
	if calls(f, "StartHotspot") != 1 {
		t.Fatalf("StartHotspot called %d times; the hotspot must be raised once, not every tick", calls(f, "StartHotspot"))
	}
}

func TestHotspotComesDownWhenTheDeviceJoinsANetwork(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{Connected: false}}
	s, clk := supervisor(t, f, boot)

	clk.Advance(46 * time.Second)
	s.Tick(context.Background())
	if calls(f, "StartHotspot") != 1 {
		t.Fatal("setup: the hotspot should be up")
	}

	// The operator picks a network through the setup page.
	f.CurrentStatus = netmgr.Status{Connected: true, SSID: "ShkolaWiFi", HotspotUp: true}
	clk.Advance(time.Second)
	s.Tick(context.Background())

	if calls(f, "StopHotspot") != 1 {
		t.Fatalf("StopHotspot called %d times after the device joined a network, want 1", calls(f, "StopHotspot"))
	}
}

// Someone changes the router's password. Without this the device is a brick
// that needs a monitor and a site visit.
func TestHotspotReturnsAfterProlongedLoss(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{Connected: true, SSID: "ShkolaWiFi"}}
	s, clk := supervisor(t, f, boot)

	for i := 0; i < 600; i++ { // ten happy minutes
		s.Tick(context.Background())
		clk.Advance(time.Second)
	}
	f.CurrentStatus = netmgr.Status{Connected: false}

	for i := 0; i < 45; i++ {
		s.Tick(context.Background())
		if calls(f, "StartHotspot") != 0 {
			t.Fatalf("hotspot raised %d seconds after the loss, before the grace period", i)
		}
		clk.Advance(time.Second)
	}
	s.Tick(context.Background()) // the tick at the 45-second mark
	if calls(f, "StartHotspot") != 1 {
		t.Fatal("the hotspot never returned after a prolonged loss")
	}
}

// A network that drops for a moment and comes back must not summon an access
// point and knock the operator off the dashboard they are looking at.
func TestABriefFlapRaisesNothing(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{Connected: true, SSID: "ShkolaWiFi"}}
	s, clk := supervisor(t, f, boot)

	s.Tick(context.Background())
	f.CurrentStatus = netmgr.Status{Connected: false}
	for i := 0; i < 10; i++ {
		clk.Advance(time.Second)
		s.Tick(context.Background())
	}
	f.CurrentStatus = netmgr.Status{Connected: true, SSID: "ShkolaWiFi"}
	for i := 0; i < 120; i++ {
		clk.Advance(time.Second)
		s.Tick(context.Background())
	}

	if calls(f, "StartHotspot") != 0 {
		t.Fatalf("StartHotspot called %d times for a ten-second flap", calls(f, "StartHotspot"))
	}
}

// An nmcli that fails tells us nothing about the network. Acting on that could
// tear a working device off a working network.
func TestAnUnreadableStatusChangesNothing(t *testing.T) {
	f := &netmgr.Fake{StatusErr: errors.New("nmcli: exit status 4")}
	s, clk := supervisor(t, f, boot)

	for i := 0; i < 300; i++ {
		s.Tick(context.Background())
		clk.Advance(time.Second)
	}
	if calls(f, "StartHotspot") != 0 || calls(f, "StopHotspot") != 0 {
		t.Fatalf("the supervisor acted on an unreadable status: start=%d stop=%d",
			calls(f, "StartHotspot"), calls(f, "StopHotspot"))
	}
}

// A hotspot that fails to come up must be tried again, or a device that hit a
// transient error at 45 seconds stays unreachable forever.
func TestAFailedHotspotIsRetried(t *testing.T) {
	f := &netmgr.Fake{
		CurrentStatus:   netmgr.Status{Connected: false},
		StartHotspotErr: errors.New("device busy"),
	}
	s, clk := supervisor(t, f, boot)

	clk.Advance(46 * time.Second)
	s.Tick(context.Background())
	clk.Advance(time.Second)
	s.Tick(context.Background())

	if calls(f, "StartHotspot") < 2 {
		t.Fatalf("StartHotspot called %d times; a failure must be retried", calls(f, "StartHotspot"))
	}
}
