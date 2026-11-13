package netmgr_test

import (
	"context"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

var feedStart = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

// The three fields are independent, so the four combinations have to be
// stated rather than derived from one of them. Only one is "serving its own
// access point with nothing above it"; a board on a wall port with its
// hotspot still up has an upstream and must keep polling, which is the case
// this reading exists to get right.
func TestAPOnlyIsHotspotWithoutUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   netmgr.Status
		want bool
	}{
		{"on its own access point and nothing else", netmgr.Status{HotspotUp: true}, true},
		{"joined over wifi", netmgr.Status{Connected: true, SSID: "site"}, false},
		{"wall port with the access point still up", netmgr.Status{Connected: true, HotspotUp: true}, false},
		{"neither, mid-join or mid-grace", netmgr.Status{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &netmgr.StatusFeed{}
			f.Publish(tc.st, feedStart)
			if got := f.APOnly(feedStart); got != tc.want {
				t.Errorf("APOnly = %t, want %t for %+v", got, tc.want, tc.st)
			}
		})
	}
}

// Nobody has read the network yet. That is not the same claim as "no network"
// and must never be answered as though it were: the only thing this answer is
// used for is stopping the alert poll.
func TestAPOnlyIsFalseBeforeAnythingIsPublished(t *testing.T) {
	var f netmgr.StatusFeed
	if f.APOnly(feedStart) {
		t.Error("a feed nothing has published to claims the device is on its own access point")
	}
	var nilFeed *netmgr.StatusFeed
	if nilFeed.APOnly(feedStart) {
		t.Error("a nil feed claims the device is on its own access point")
	}
}

// The supervisor stops publishing whenever its status read fails, whenever a
// join holds the gate, and if its goroutine goes away. None of those is
// evidence about the network, so the last reading has to expire rather than
// pause the poll forever.
func TestAPOnlyExpiresAReadingNothingHasRefreshed(t *testing.T) {
	f := &netmgr.StatusFeed{}
	f.Publish(netmgr.Status{HotspotUp: true}, feedStart)

	if !f.APOnly(feedStart.Add(30 * time.Second)) {
		t.Error("a reading half a minute old was discarded; one slow nmcli call must not flap the poll")
	}
	if f.APOnly(feedStart.Add(2 * time.Minute)) {
		t.Error("a two-minute-old reading still stops the poll; nothing has confirmed it since")
	}

	// These boards have no RTC and NTP corrects the clock shortly after boot,
	// so a reading timestamped in the future is a clock that moved, not a
	// reading to trust.
	if f.APOnly(feedStart.Add(-time.Hour)) {
		t.Error("a reading from the future still stops the poll")
	}
}

// The whole design rests on there being one reader of the radio. If the
// supervisor's own tick does not publish what it read, everything downstream
// is back to asking nmcli separately — or, worse, never finding out at all.
func TestSupervisorPublishesWhatItRead(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{HotspotUp: true}}
	feed := &netmgr.StatusFeed{}
	clk := clock.NewFake(feedStart)
	cfg := config.Default()
	cfg.Network.APPassword = testAPPassword
	s := netmgr.NewSupervisor(f, func() *config.Config { return cfg }, clk, nil, nil, feed, nil)

	s.Tick(context.Background())
	if !feed.APOnly(clk.Now()) {
		t.Fatal("a tick that read a hotspot with no upstream published nothing")
	}

	// A device that joins a network has to hand polling straight back, on the
	// next reading and not on a timer.
	f.CurrentStatus = netmgr.Status{Connected: true, SSID: "site", HotspotUp: true}
	clk.Advance(5 * time.Second)
	s.Tick(context.Background())
	if feed.APOnly(clk.Now()) {
		t.Error("the device has an upstream again and the feed still says it is on its own access point")
	}
}

// An unreadable status is not a reading of a device with nothing attached. A
// board whose OS has no NetworkManager fails this read on every tick forever,
// and publishing the zero value would tell the poll loop it is on a hotspot.
func TestSupervisorPublishesNothingForAFailedRead(t *testing.T) {
	f := &netmgr.Fake{CurrentStatus: netmgr.Status{HotspotUp: true}}
	feed := &netmgr.StatusFeed{}
	clk := clock.NewFake(feedStart)
	cfg := config.Default()
	cfg.Network.APPassword = testAPPassword
	s := netmgr.NewSupervisor(f, func() *config.Config { return cfg }, clk, nil, nil, feed, nil)

	s.Tick(context.Background())
	if !feed.APOnly(clk.Now()) {
		t.Fatal("the first tick published nothing")
	}

	f.StatusErr = context.DeadlineExceeded
	clk.Advance(5 * time.Second)
	s.Tick(context.Background())
	// The old reading is still fresh, so it still stands — that is the point
	// of the staleness bound rather than a flag. What must not happen is a
	// new publication built out of the failure.
	if !feed.APOnly(clk.Now()) {
		t.Error("a failed status read overwrote the last real reading")
	}
	clk.Advance(2 * time.Minute)
	if feed.APOnly(clk.Now()) {
		t.Error("a status that has not been readable for two minutes still stops the poll")
	}
}

// /metrics reports the network as the supervisor last read it, and only while
// that reading is fresh: a supervisor that stopped ticking must not leave a
// monitoring system believing a connection from an hour ago.
func TestLatestIsTheFreshReadingOrNothing(t *testing.T) {
	var nilFeed *netmgr.StatusFeed
	if _, ok := nilFeed.Latest(feedStart); ok {
		t.Fatal("a nil feed reported a reading")
	}
	f := &netmgr.StatusFeed{}
	if _, ok := f.Latest(feedStart); ok {
		t.Fatal("an empty feed reported a reading")
	}
	f.Publish(netmgr.Status{Connected: true, SSID: "site"}, feedStart)
	if st, ok := f.Latest(feedStart.Add(30 * time.Second)); !ok || !st.Connected {
		t.Fatalf("Latest = %+v, %t for a 30s-old reading", st, ok)
	}
	if _, ok := f.Latest(feedStart.Add(2 * time.Minute)); ok {
		t.Fatal("a 2-minute-old reading was reported as current")
	}
	// A reading stamped after now is a clock that stepped back since, not
	// a reading from the future: not reported, as APOnly does not act on it.
	f.Publish(netmgr.Status{Connected: true}, feedStart.Add(time.Minute))
	if _, ok := f.Latest(feedStart); ok {
		t.Fatal("a reading from a minute in the future was reported as current")
	}
}
