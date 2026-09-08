package netmgr_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

// testAPPassword stands in for the password EnsureAPPassword writes at
// startup. config.Default() ships the field empty — the "not generated yet"
// state — and StartHotspot refuses that rather than raising an open access
// point, so a supervisor test running on the bare default would be exercising
// a configuration no real device ever reaches.
const testAPPassword = "sp0lohTest42"

func supervisor(t *testing.T, f *netmgr.Fake, start time.Time) (*netmgr.Supervisor, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(start)
	cfg := config.Default() // APFallbackAfter is 45s
	cfg.Network.APPassword = testAPPassword
	return netmgr.NewSupervisor(f, func() *config.Config { return cfg }, clk, nil, nil), clk
}

// supervisorWithGate is supervisor plus the join gate the dashboard holds
// while it is joining the device to a network.
func supervisorWithGate(t *testing.T, f *netmgr.Fake, start time.Time, g *netmgr.JoinGate) (*netmgr.Supervisor, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(start)
	cfg := config.Default()
	cfg.Network.APPassword = testAPPassword
	return netmgr.NewSupervisor(f, func() *config.Config { return cfg }, clk, nil, g), clk
}

// supervisorWithLog is supervisor plus a logger writing into a buffer the
// test can inspect, for the failure-log-throttling tests below.
func supervisorWithLog(t *testing.T, f *netmgr.Fake, start time.Time) (*netmgr.Supervisor, *clock.Fake, *bytes.Buffer) {
	t.Helper()
	clk := clock.NewFake(start)
	cfg := config.Default()
	cfg.Network.APPassword = testAPPassword
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	return netmgr.NewSupervisor(f, func() *config.Config { return cfg }, clk, logger, nil), clk, &buf
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

	// The operator picks a network through the setup page. Connected and
	// HotspotUp both true is not a contrived state: parseStatus aggregates
	// over every device, so a board with an ethernet upstream and wlan0
	// still serving the access point reports both at once. On a
	// single-radio board wlan0 cannot do both jobs, which is exactly why
	// this transitional state is unreachable there and this rule exists.
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

// A StartHotspot that keeps failing must keep being retried every tick — the
// alternative is a device that never becomes reachable — but at tickInterval
// an unthrottled Error line would write one every 5 seconds indefinitely, and
// these devices run off SD cards that wear out (spec §5.7). The retry and the
// logging are two different questions; this only tests the second.
func TestFailedStartHotspotLoggingIsThrottled(t *testing.T) {
	f := &netmgr.Fake{
		CurrentStatus:   netmgr.Status{Connected: false},
		StartHotspotErr: errors.New("device busy"),
	}
	s, clk, buf := supervisorWithLog(t, f, boot)

	clk.Advance(46 * time.Second) // past the grace boundary before the first tick
	s.Tick(context.Background())

	const moreTicks = 700 // several hundred, comfortably past multiple throttle windows
	for i := 0; i < moreTicks; i++ {
		clk.Advance(time.Second)
		s.Tick(context.Background())
	}

	wantAttempts := moreTicks + 1
	if got := calls(f, "StartHotspot"); got != wantAttempts {
		t.Fatalf("StartHotspot attempted %d times, want %d; the retry must not be weakened by the log throttle", got, wantAttempts)
	}

	lines := strings.Count(buf.String(), "could not raise the access point")
	if lines == 0 {
		t.Fatal("the first failure was never logged")
	}
	if lines >= wantAttempts {
		t.Fatalf("%d error lines for %d attempts; the failure log must be throttled, not one per tick", lines, wantAttempts)
	}
	if lines > 10 {
		t.Fatalf("%d error lines logged for a single unbroken run of failures; want a small, bounded number", lines)
	}
}

// StopHotspot's failure path re-enters the same branch every tick too:
// downSince is cleared before the attempt (the device really is connected),
// so nothing else stops Tick from retrying — and nothing but the throttle
// stops it from logging every time.
func TestFailedStopHotspotLoggingIsThrottled(t *testing.T) {
	f := &netmgr.Fake{
		CurrentStatus:  netmgr.Status{Connected: true, SSID: "ShkolaWiFi", HotspotUp: true},
		StopHotspotErr: errors.New("device busy"),
	}
	s, clk, buf := supervisorWithLog(t, f, boot)

	const ticks = 700
	for i := 0; i < ticks; i++ {
		s.Tick(context.Background())
		clk.Advance(time.Second)
	}

	if got := calls(f, "StopHotspot"); got != ticks {
		t.Fatalf("StopHotspot attempted %d times, want %d; a failing StopHotspot must still be retried every tick", got, ticks)
	}

	lines := strings.Count(buf.String(), "could not lower the access point")
	if lines == 0 {
		t.Fatal("the first failure was never logged")
	}
	if lines >= ticks {
		t.Fatalf("%d error lines for %d attempts; the failure log must be throttled, not one per tick", lines, ticks)
	}
	if lines > 10 {
		t.Fatalf("%d error lines logged for a single unbroken run of failures; want a small, bounded number", lines)
	}
}

// A fault that clears must be reported again, not stay silenced by a stale
// throttle timestamp from a run of failures that already ended.
func TestFailureLoggingResumesAfterASuccess(t *testing.T) {
	f := &netmgr.Fake{
		CurrentStatus:   netmgr.Status{Connected: false},
		StartHotspotErr: errors.New("device busy"),
	}
	s, clk, buf := supervisorWithLog(t, f, boot)

	clk.Advance(46 * time.Second)
	s.Tick(context.Background()) // logs immediately: first failure of the run

	f.StartHotspotErr = nil
	clk.Advance(time.Second)
	s.Tick(context.Background()) // succeeds; HotspotUp is now true

	f.CurrentStatus = netmgr.Status{Connected: false} // router changes password later
	f.StartHotspotErr = errors.New("device busy again")
	clk.Advance(46 * time.Second)
	s.Tick(context.Background()) // a new run of failures

	lines := strings.Count(buf.String(), "could not raise the access point")
	if lines != 2 {
		t.Fatalf("%d error lines logged; want 2 (one per run of failures), the success in between should have reset the throttle", lines)
	}
}

// Run is the loop the daemon spends its life in while networking is healthy;
// it must return promptly when asked to stop and not before, exactly as
// internal/engine.Engine.Run does.
func TestRunReturnsOnContextCancellation(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{Connected: true, SSID: "ShkolaWiFi"}}
	s, _ := supervisor(t, f, boot)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Run returned before the context was cancelled")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// deadlineProbe records whether the context a Supervisor hands to the manager
// carries a deadline. It reports a disconnected device so Tick takes its
// longest path, through StartHotspot.
type deadlineProbe struct {
	statusHadDeadline  bool
	hotspotHadDeadline bool
}

func (p *deadlineProbe) Status(ctx context.Context) (netmgr.Status, error) {
	_, p.statusHadDeadline = ctx.Deadline()
	return netmgr.Status{Connected: false}, nil
}

func (p *deadlineProbe) StartHotspot(ctx context.Context, ssid, psk string) error {
	_, p.hotspotHadDeadline = ctx.Deadline()
	return nil
}

func (p *deadlineProbe) Scan(context.Context) ([]netmgr.Network, error) { return nil, nil }
func (p *deadlineProbe) Connect(context.Context, string, string) error  { return nil }
func (p *deadlineProbe) StopHotspot(context.Context) error              { return nil }

func TestTickBoundsEveryCommand(t *testing.T) {
	// A wedged NetworkManager must not stop the supervisor for good. Tick runs
	// in Run's only goroutine, so an unbounded nmcli call ends the supervision
	// of a device whose access point is its last way back.
	p := &deadlineProbe{}
	clk := clock.NewFake(boot)
	cfg := config.Default()
	cfg.Network.APPassword = testAPPassword
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	s := netmgr.NewSupervisor(p, func() *config.Config { return cfg }, clk, logger, nil)

	clk.Advance(46 * time.Second)
	s.Tick(context.Background())

	if !p.statusHadDeadline {
		t.Error("Status ran on a context with no deadline; a hung nmcli would stall the supervisor forever")
	}
	if !p.hotspotHadDeadline {
		t.Error("StartHotspot ran on a context with no deadline")
	}
}

// The commonest cause of a failing status read is not a fault: a board on an
// OS without NetworkManager fails it on every tick, forever. Unthrottled that
// is a line every five seconds onto an SD card the spec says wears out.
func TestFailedStatusLoggingIsThrottled(t *testing.T) {
	f := &netmgr.Fake{StatusErr: errors.New("exec: \"nmcli\": executable file not found in $PATH")}
	s, clk, buf := supervisorWithLog(t, f, boot)

	for i := 0; i < 700; i++ {
		s.Tick(context.Background())
		clk.Advance(time.Second)
	}

	if got := calls(f, "Status"); got != 700 {
		t.Errorf("Status attempted %d times over 700 ticks; throttling the log must not throttle the read", got)
	}
	lines := strings.Count(buf.String(), "could not read the network status")
	if lines == 0 {
		t.Fatal("a failing status read must be reported at least once")
	}
	// 700 seconds at a five-minute interval: the first line plus two more.
	if lines > 3 {
		t.Errorf("%d log lines over 700 ticks; the warning must be throttled, not written every tick", lines)
	}
}

// A read that starts working again must be reported when it next fails,
// rather than staying silent because a fault hours ago used up the interval.
func TestStatusFailureLoggingResumesAfterASuccess(t *testing.T) {
	f := &netmgr.Fake{StatusErr: errors.New("nmcli is missing")}
	s, clk, buf := supervisorWithLog(t, f, boot)

	s.Tick(context.Background())
	first := strings.Count(buf.String(), "could not read the network status")
	if first != 1 {
		t.Fatalf("first failure logged %d times, want 1", first)
	}

	f.StatusErr = nil
	clk.Advance(time.Second)
	s.Tick(context.Background())

	f.StatusErr = errors.New("nmcli is missing again")
	clk.Advance(time.Second)
	s.Tick(context.Background())

	if got := strings.Count(buf.String(), "could not read the network status"); got != 2 {
		t.Errorf("logged %d times, want 2: a fault that returns after a success must be reported, not suppressed", got)
	}
}

// midJoin is what the radio reports while a join started from the dashboard
// is in flight: it has left the access point and has not yet associated with
// the school network, so it is neither joined to anything nor serving
// anything. It is built here rather than produced by Fake.Connect on purpose
// — Fake.Connect models a board that ends up connected, which is correct for
// what it models, and a board with an Ethernet upstream legitimately reports
// Connected and HotspotUp together. This transient state is neither of those,
// so the test states it directly.
var midJoin = netmgr.Status{Connected: false, HotspotUp: false}

// While the dashboard is joining the device to a network, the supervisor must
// do nothing at all. Mid-join the radio looks exactly like a device with no
// network — which, past the grace period, is the state that makes the
// supervisor raise an access point, on top of the join it should be waiting
// for. Association plus a DHCP lease takes tens of seconds and the supervisor
// ticks every five, so without the gate essentially every real join is fought.
func TestAHeldJoinGateStopsTheSupervisorRaisingAnAccessPoint(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: midJoin}
	gate := &netmgr.JoinGate{}
	s, clk := supervisorWithGate(t, f, boot, gate)

	// Well past the 45-second grace period: downSince has been set since
	// boot, so this is the state the supervisor would otherwise act on.
	clk.Advance(5 * time.Minute)
	gate.HoldUntil(clk.Now().Add(time.Minute))

	for i := 0; i < 12; i++ { // a minute of ticks at the real cadence
		s.Tick(context.Background())
		clk.Advance(5 * time.Second)
	}

	if got := calls(f, "StartHotspot"); got != 0 {
		t.Fatalf("StartHotspot called %d times while a join was in flight, want 0", got)
	}
	if got := calls(f, "Status"); got != 0 {
		t.Errorf("Status read %d times while a join was in flight; a held gate must stop the tick before it touches the manager", got)
	}
}

// The gate holds a deadline, not a flag, so a handler that dies without
// releasing it cannot leave the device unable to raise an access point ever
// again. Once the hold has expired the same state raises one as usual.
func TestAnExpiredJoinGateLetsTheSupervisorRaiseAnAccessPoint(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: midJoin}
	gate := &netmgr.JoinGate{}
	s, clk := supervisorWithGate(t, f, boot, gate)

	clk.Advance(5 * time.Minute)
	gate.HoldUntil(clk.Now().Add(time.Minute))
	clk.Advance(2 * time.Minute) // the holder never released it

	s.Tick(context.Background())

	if got := calls(f, "StartHotspot"); got != 1 {
		t.Fatalf("StartHotspot called %d times after the hold expired, want 1", got)
	}
}

// Release is what ends a hold in the ordinary case; the deadline is only the
// backstop.
func TestAReleasedJoinGateLetsTheSupervisorRaiseAnAccessPoint(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: midJoin}
	gate := &netmgr.JoinGate{}
	s, clk := supervisorWithGate(t, f, boot, gate)

	clk.Advance(5 * time.Minute)
	gate.HoldUntil(clk.Now().Add(time.Minute))
	gate.Release()

	s.Tick(context.Background())

	if got := calls(f, "StartHotspot"); got != 1 {
		t.Fatalf("StartHotspot called %d times after the gate was released, want 1", got)
	}
}

// A nil gate has to mean "no join is in flight", never "held forever": not
// every construction has a dashboard behind it.
func TestANilJoinGateIsNeverHeld(t *testing.T) {
	var g *netmgr.JoinGate
	if g.Held(boot) {
		t.Error("a nil gate reported itself held")
	}
	g.HoldUntil(boot.Add(time.Minute)) // must not panic
	g.Release()                        // must not panic

	var zero netmgr.JoinGate
	if zero.Held(boot) {
		t.Error("the zero gate reported itself held")
	}
	zero.HoldUntil(boot.Add(time.Minute))
	if !zero.Held(boot) {
		t.Error("a gate held for another minute reported itself free")
	}
	if zero.Held(boot.Add(2 * time.Minute)) {
		t.Error("a hold that expired an hour of ticks ago is still reported held")
	}
}
