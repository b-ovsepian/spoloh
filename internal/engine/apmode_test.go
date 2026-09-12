package engine_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

// apBase is the wall time every test in this file starts from. The clock is
// fake throughout: netmgr.StatusFeed ages its readings out, so a real clock
// here would make these tests depend on how long the suite takes to run.
var apBase = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// newAPHarness builds the usual engine harness with a network feed wired in,
// and hands back both. The feed is the real netmgr type rather than a stub of
// one: the reading the engine acts on has to be the reading the supervisor
// publishes, and a stub here would be free to be more generous than the thing
// it stands in for.
func newAPHarness(t *testing.T, clk clock.Clock) (*harness, *netmgr.StatusFeed) {
	t.Helper()
	feed := &netmgr.StatusFeed{}
	h := newHarnessWith(t, alertstate.New(), clk, func(d *engine.Deps) { d.Network = feed })
	return h, feed
}

// apStatus is the reading a supervisor takes on a device that has lost its
// network, waited out the grace period and raised its own hotspot.
var apStatus = netmgr.Status{HotspotUp: true}

// joinedStatus is the reading taken the moment a join lands: joined, and the
// access point not yet lowered. Both flags true at once is not a
// contradiction — see netmgr.Status — and it is exactly what the supervisor
// sees on the tick before it stops the hotspot, so the resume has to work on
// this reading and not only on the tidy one after it.
var joinedStatus = netmgr.Status{Connected: true, SSID: "site", HotspotUp: true}

// advance moves the clock and republishes st, the way a live supervisor does
// once per tick of its own. Every test here republishes rather than letting
// the reading age out: expiry is netmgr.StatusFeed's guard against a
// supervisor that has gone quiet, and a test that leaned on it would be
// exercising the guard instead of the pause.
func advance(clk *clock.Fake, feed *netmgr.StatusFeed, d time.Duration, st netmgr.Status) {
	clk.Advance(d)
	feed.Publish(st, clk.Now())
}

// There is no internet over the device's own access point. Polling anyway
// burns the interval, fills the journal with identical failures and spends
// requests against a rate limit shared with every other device behind the
// same public address.
func TestTickDoesNotPollWhileServingItsOwnAccessPoint(t *testing.T) {
	clk := clock.NewFake(apBase)
	h, feed := newAPHarness(t, clk)
	ctx := context.Background()

	h.eng.Tick(ctx) // nextPoll starts at the zero time: always due
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after the first tick = %d, want 1", got)
	}

	// Five minutes of supervisor ticks, twenty times the default poll
	// interval.
	for i := 0; i < 60; i++ {
		advance(clk, feed, 5*time.Second, apStatus)
		h.eng.Tick(ctx)
	}
	if got := h.hits(); got != 1 {
		t.Fatalf("hits after five minutes on the access point = %d, want 1 — the poll did not stop", got)
	}
	if !h.eng.Status().PollPaused {
		t.Error("the device stopped polling and Status does not say so; a paused daemon and a wedged one must not look alike")
	}
}

// A board on a wall port with its access point still up has an upstream. Both
// flags are true at once and the poll must carry on — this is the case that
// makes "AP mode" the pair of fields rather than either one of them.
func TestTickKeepsPollingWithAnUpstreamBehindTheAccessPoint(t *testing.T) {
	clk := clock.NewFake(apBase)
	h, feed := newAPHarness(t, clk)
	ctx := context.Background()

	feed.Publish(joinedStatus, clk.Now())
	h.eng.Tick(ctx)
	advance(clk, feed, 30*time.Second, joinedStatus)
	h.eng.Tick(ctx)

	if got := h.hits(); got != 2 {
		t.Fatalf("hits on a wired board still serving a hotspot = %d, want 2 — it has an upstream", got)
	}
	if h.eng.Status().PollPaused {
		t.Error("a device with an upstream reports its poll as paused")
	}
}

// The join succeeds and the device is back on a network. It has been unable
// to ask about a raid for as long as the pause lasted, so the first thing to
// do with an upstream is use it — on the reading that says so, not on
// whatever backoff or interval was left over from before.
func TestTickResumesPollingTheMomentUpstreamReturns(t *testing.T) {
	clk := clock.NewFake(apBase)
	h, feed := newAPHarness(t, clk)
	ctx := context.Background()

	h.eng.Tick(ctx)
	advance(clk, feed, 5*time.Second, apStatus)
	h.eng.Tick(ctx)
	if got := h.hits(); got != 1 {
		t.Fatalf("hits while paused = %d, want 1", got)
	}

	// Republished at the same instant as the paused tick above: no interval
	// has elapsed, so a resume that waited for the next due poll would show
	// no hit here.
	feed.Publish(joinedStatus, clk.Now())
	h.eng.Tick(ctx)
	if got := h.hits(); got != 2 {
		t.Fatalf("hits on the tick after upstream returned = %d, want 2 — the resume waited for a timer", got)
	}
	if h.eng.Status().PollPaused {
		t.Error("the poll resumed and Status still reports it paused")
	}
}

// Paused is not calm. If a raid was running when the network dropped, this
// device still believes it, still says so, and its siren is still the right
// siren. Turning "no data" into an all-clear is the worst thing this code
// could do.
func TestAPausedPollKeepsTheRaidItIsAlreadyAnnouncing(t *testing.T) {
	clk := clock.NewFake(apBase)
	h, feed := newAPHarness(t, clk)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1) // the silent baseline every cold start begins with
	h.set(true)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q when the raid started, want siren.wav", got)
	}
	since := h.eng.Status().Since

	// Upstream goes calm in the same breath as the network going away. It
	// makes no difference: nobody is reading upstream any more.
	h.set(false)
	for i := 0; i < 60; i++ {
		advance(clk, feed, 5*time.Second, apStatus)
		h.eng.Tick(ctx)
	}

	st := h.eng.Status()
	if !st.Alert {
		t.Error("the raid was dropped while the device was on its own access point; no data is not an all-clear")
	}
	if !st.Since.Equal(since) {
		t.Errorf("the raid's start moved during the pause: %v, want %v", st.Since, since)
	}
	for _, played := range h.rec.Played() {
		if played == "all-clear.wav" {
			t.Fatal("an all-clear was announced during the pause, off no reading at all")
		}
	}
}

// The all-clear counts consecutive calm readings, and a paused poll is not a
// reading. Two calm readings, a quarter of an hour with no network and one
// more calm reading is not half a minute of watching a raid end — it is two
// readings with a hole between them, which is the bet EndConfirmations exists
// to refuse.
func TestAPausedPollDoesNotAdvanceTheAllClearRun(t *testing.T) {
	clk := clock.NewFake(apBase)
	h, feed := newAPHarness(t, clk)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q when the raid started, want siren.wav", got)
	}

	// Two of the three calm readings an all-clear needs.
	h.set(false)
	pollTimes(t, h, ctx, alertstate.EndConfirmations-1)
	if got := h.eng.Status().AllClearSeen; got != alertstate.EndConfirmations-1 {
		t.Fatalf("AllClearSeen = %d before the network went, want %d", got, alertstate.EndConfirmations-1)
	}

	advance(clk, feed, 5*time.Second, apStatus)
	h.eng.Tick(ctx)
	if got := h.eng.Status().AllClearSeen; got != 0 {
		t.Fatalf("AllClearSeen = %d once the poll paused, want 0 — the pause is the break in the run", got)
	}

	// Upstream comes back, still calm. The run starts again from nothing: one
	// reading must not complete a run that a hole ran through.
	feed.Publish(joinedStatus, clk.Now())
	h.eng.Tick(ctx)

	st := h.eng.Status()
	if !st.Alert {
		t.Error("the all-clear was announced on the first reading after the pause")
	}
	if st.AllClearSeen != 1 {
		t.Errorf("AllClearSeen = %d on the first reading after the pause, want 1", st.AllClearSeen)
	}
	for _, played := range h.rec.Played() {
		if played == "all-clear.wav" {
			t.Fatal("an all-clear was announced on a run with a pause through the middle of it")
		}
	}
}

// A device that has stopped polling and says nothing about it reads in the
// journal exactly like one that is wedged, and that is the reading that gets
// it restarted — or unplugged — by somebody trying to help. Entering and
// leaving the pause are each said once, and the recurring heartbeat carries
// it too, because whoever opens the journal is usually reading it long after
// those two lines scrolled past.
func TestTheJournalSaysThePollIsPaused(t *testing.T) {
	var buf bytes.Buffer
	clk := clock.NewFake(apBase)
	feed := &netmgr.StatusFeed{}
	h := newHarnessWith(t, alertstate.New(), clk, func(d *engine.Deps) {
		d.Network = feed
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})
	ctx := context.Background()

	h.eng.Tick(ctx)
	advance(clk, feed, 5*time.Second, apStatus)
	h.eng.Tick(ctx)

	if !strings.Contains(buf.String(), "pausing the alert poll") {
		t.Errorf("nothing in the journal says the poll stopped:\n%s", buf.String())
	}

	// Past heartbeatInterval, so the recurring liveness line is written — and
	// still on the access point, republished by a supervisor that is very
	// much alive.
	buf.Reset()
	advance(clk, feed, 20*time.Minute, apStatus)
	h.eng.Tick(ctx)
	if !strings.Contains(buf.String(), "poll_paused=true") {
		t.Errorf("the heartbeat does not carry the paused state:\n%s", buf.String())
	}

	buf.Reset()
	feed.Publish(joinedStatus, clk.Now())
	h.eng.Tick(ctx)
	if !strings.Contains(buf.String(), "resuming the alert poll") {
		t.Errorf("nothing in the journal says the poll came back:\n%s", buf.String())
	}
}
