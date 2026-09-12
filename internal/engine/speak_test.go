package engine_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/webhook"
)

// delayGate stands in for time.After. No test here may sleep on a real clock,
// and a before-phase delay is the one thing in this package that waits, so it
// is driven the way internal/player drives its playback: the wait is a
// channel a test hands out and releases by hand.
type delayGate struct {
	asked   chan time.Duration
	release chan time.Time
}

func newDelayGate() *delayGate {
	return &delayGate{asked: make(chan time.Duration, 8), release: make(chan time.Time)}
}

func (g *delayGate) after(d time.Duration) <-chan time.Time {
	g.asked <- d
	return g.release
}

// waited reports the delay the engine asked for, failing if it asked for none.
func (g *delayGate) waited(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-g.asked:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("the engine never waited out a before-phase delay")
		return 0
	}
}

// refuse fails if the engine asked for a wait it should not have.
func (g *delayGate) refuse(t *testing.T, why string) {
	t.Helper()
	select {
	case d := <-g.asked:
		t.Fatalf("the engine waited %v: %s", d, why)
	case <-time.After(150 * time.Millisecond):
	}
}

// let releases one parked wait.
func (g *delayGate) let(t *testing.T) {
	t.Helper()
	select {
	case g.release <- time.Time{}:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was waiting on the delay")
	}
}

// newGatedHarness is newHarnessWithWebhooks with the delay under the test's
// own hand.
func newGatedHarness(t *testing.T, clk clock.Clock) (*harness, *triggerLog, *delayGate) {
	t.Helper()
	hooks := &triggerLog{}
	gate := newDelayGate()
	h := newHarnessWith(t, alertstate.New(), clk, func(d *engine.Deps) {
		d.Webhooks = hooks.fire
		d.After = gate.after
	})
	return h, hooks, gate
}

// waitForFiring waits for one phase:trigger to appear, and returns everything
// seen by then. The after phase is fired from the player's goroutine, so this
// polls rather than sleeping for a guessed interval.
func waitForFiring(t *testing.T, hooks *triggerLog, want string) []string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		got := hooks.firings()
		if slices.Contains(got, want) {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("%q never fired; firings = %v", want, got)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// refuseFiring fails if a phase:trigger turns up that should not have.
func refuseFiring(t *testing.T, hooks *triggerLog, unwanted, why string) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		if got := hooks.firings(); slices.Contains(got, unwanted) {
			t.Fatalf("%q fired: %s (firings = %v)", unwanted, why, got)
		}
		select {
		case <-deadline:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func refusePlayback(t *testing.T, h *harness, why string) {
	t.Helper()
	select {
	case got := <-h.rec.ch:
		t.Fatalf("%q played: %s", got, why)
	case <-time.After(150 * time.Millisecond):
	}
}

// setRepeat gives every event on a trigger the same repeat count, and reports
// how many events that was.
func setRepeat(cfg *config.Config, trigger string, repeat int) int {
	n := 0
	for i := range cfg.Events {
		if cfg.Events[i].Trigger == trigger {
			cfg.Events[i].Repeat = repeat
			n++
		}
	}
	return n
}

// The feature, at its simplest: the request goes out, the sound waits for the
// far end, and then this device is heard. What is being tested is that the
// wait is the configured one and that it is actually in front of the sound —
// today the request and the sound leave together and the first seconds of
// every announcement are lost into hardware still catching up.
func TestABeforeWebhookHoldsTheSoundBackByItsDelay(t *testing.T) {
	h, hooks, gate := newGatedHarness(t, clock.System{Loc: time.UTC})
	hooks.beforeDelay = 3 * time.Second
	ctx := context.Background()

	pollTimes(t, h, ctx, 1) // the baselining first reading
	h.set(true)
	pollTimes(t, h, ctx, 1)

	if got := gate.waited(t); got != 3*time.Second {
		t.Fatalf("the sound waited %v, want the configured 3s", got)
	}
	waitForFiring(t, hooks, "before:"+config.TriggerAlertStart)
	refusePlayback(t, h, "the before-phase delay had not run out")

	gate.let(t)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q after the delay, want siren.wav", got)
	}
}

// A delay in front of an air raid alert is latency on a siren, so it is
// bounded by its own goroutine and never by the queue. An alert arriving
// while a scheduled announcement is still waiting out its warm-up must be
// heard now: the far end was already told, and a siren that waits for a
// chime's wiring is the failure this whole project is against.
func TestAnAlertDoesNotWaitOutAScheduledAnnouncementsDelay(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC))
	h, hooks, gate := newGatedHarness(t, clk)
	hooks.beforeDelay = 5 * time.Second
	ctx := context.Background()

	pollTimes(t, h, ctx, 1) // the baselining first reading, still calm

	h.eng.Tick(ctx) // the chime is decided, and parks on its delay
	if got := gate.waited(t); got != 5*time.Second {
		t.Fatalf("the chime waited %v, want 5s", got)
	}
	waitForFiring(t, hooks, "before:"+config.TriggerScheduled)
	refusePlayback(t, h, "the chime's before-phase delay had not run out")

	h.set(true)
	pollTimes(t, h, ctx, 1)

	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want the siren straight away", got)
	}
	gate.refuse(t, "an alert must not wait out a delay of its own while the device is already speaking")
	refuseFiring(t, hooks, "before:"+config.TriggerAlertStart,
		"the device was already speaking, so whatever the far end had to do it had already done")
}

// Event.Repeat queues each repetition as its own request. The far end is told
// once, at the start of the sequence, because the thing it switched stays
// switched for the whole of it — five requests would be five round trips and
// four chances to leave it in the wrong position.
func TestTheBeforePhaseFiresOnceForARepeatedAnnouncement(t *testing.T) {
	h, hooks, gate := newGatedHarness(t, clock.System{Loc: time.UTC})
	hooks.beforeDelay = 2 * time.Second
	ctx := context.Background()

	events := setRepeat(h.cfg, config.TriggerAlertStart, 3)
	if events != 1 {
		t.Fatalf("the default config has %d alert_start events; this test assumes exactly 1", events)
	}

	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)

	gate.waited(t)
	gate.let(t)
	for i := 0; i < 3; i++ {
		if got := waitPlayed(t, h.rec); got != "siren.wav" {
			t.Fatalf("repetition %d played %q, want siren.wav", i+1, got)
		}
	}

	gate.refuse(t, "a repetition is not a new announcement and must not wait again")
	got := hooks.firings()
	if n := countFirings(got, "before:"+config.TriggerAlertStart); n != 1 {
		t.Fatalf("the before phase fired %d times for one announcement of three repetitions: %v", n, got)
	}
}

// And the other half of the pair: the far end is told the device has finished
// once the whole sequence is over, not after the first repetition. Firing
// after the first one would put back whatever was switched while two more
// repetitions of the siren were still to come.
func TestTheAfterPhaseFiresWhenTheWholeSequenceIsOver(t *testing.T) {
	h, hooks, _ := newGatedHarness(t, clock.System{Loc: time.UTC})
	ctx := context.Background()

	setRepeat(h.cfg, config.TriggerAlertStart, 3)

	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)

	for i := 0; i < 3; i++ {
		waitPlayed(t, h.rec)
	}
	got := waitForFiring(t, hooks, "after:"+config.TriggerAlertStart)
	if n := countFirings(got, "after:"+config.TriggerAlertStart); n != 1 {
		t.Fatalf("the after phase fired %d times for one announcement: %v", n, got)
	}
	if len(h.rec.Played()) != 3 {
		t.Fatalf("played %v, want all three repetitions before the after phase", h.rec.Played())
	}
}

// The case that decides what a phase brackets. A siren cutting across a chime
// must not fire the chime's after and then the siren's before: whatever those
// two control would go back and forth in the gap between the two sounds, at
// the exact moment the room needs to hear one clear thing. The device never
// stopped speaking, so neither fires — and the pair is closed once, by the
// trigger that opened it.
func TestASirenCuttingAcrossAChimeDoesNotCloseAndReopenThePair(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC))
	h, hooks, _ := newGatedHarness(t, clk)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1) // the baselining first reading

	h.rec.block() // the chime parks inside Play, the way a real sound takes real seconds
	h.eng.Tick(ctx)
	waitPlayed(t, h.rec)
	waitForFiring(t, hooks, "before:"+config.TriggerScheduled)

	h.rec.block() // and the siren parks too, so the assertions below are not a race
	h.set(true)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want the siren to preempt the chime", got)
	}

	refuseFiring(t, hooks, "after:"+config.TriggerScheduled,
		"the chime was cut short but the siren had the voice, so the device never fell silent")
	refuseFiring(t, hooks, "before:"+config.TriggerAlertStart,
		"the device was already speaking when the siren arrived")

	// End the siren, and with it the session. It closes once, under the
	// trigger that opened it — the operator wrote the two halves as a pair
	// and a pair that comes out unmatched leaves their hardware where this
	// device put it.
	h.eng.StopManual()
	got := waitForFiring(t, hooks, "after:"+config.TriggerScheduled)
	if n := countFirings(got, "after:"+config.TriggerScheduled); n != 1 {
		t.Fatalf("the pair was closed %d times: %v", n, got)
	}
}

// A failed webhook must never silence the siren. If the far end does not
// answer, the wait ends and the sound plays: the amplifier may be asleep and
// the announcement inaudible, and that is still better than a device that
// decided not to try.
func TestAFailingBeforeWebhookStillLetsTheSoundPlay(t *testing.T) {
	// A server started and immediately closed leaves an address nothing is
	// listening on — a refused connection on loopback, which is the local
	// relay board that has been unplugged. No socket to the internet.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	gate := newDelayGate()
	h := newHarnessWith(t, alertstate.New(), clock.System{Loc: time.UTC}, func(d *engine.Deps) {
		d.Webhooks = webhook.New(d.Config, d.Log, nil).Dispatch
		d.After = gate.after
	})
	h.cfg.Webhooks = []config.Webhook{{
		ID: "webhook-relay", Trigger: config.TriggerAlertStart,
		Method: http.MethodGet, URL: deadURL, Enabled: true,
		Phase: config.PhaseBefore, Delay: config.Duration(2 * time.Second),
	}}

	ctx := context.Background()
	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)

	if got := gate.waited(t); got != 2*time.Second {
		t.Fatalf("the sound waited %v, want the configured 2s even though the target is gone", got)
	}
	gate.let(t)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want the siren to sound despite the webhook failing", got)
	}

	row := waitForLogEntry(t, h, webhook.EventKind)
	if row.OK {
		t.Fatalf("the webhook row says the request succeeded, so this test proved nothing: %+v", row)
	}
}

// There is nothing to bracket when nothing will be heard, so nothing is
// switched and nothing is left switched. An operator whose hardware has to
// act on a raid the siren was silenced for puts that webhook on
// config.PhaseOnTrigger, which fires regardless — and which is where every
// webhook written before phases existed already is.
func TestNothingIsBracketedWhenQuietHoursSilenceTheSiren(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC))
	h, hooks, gate := newGatedHarness(t, clk)
	hooks.beforeDelay = 3 * time.Second
	h.cfg.QuietHours = config.QuietHours{
		Enabled: true, From: "01:00", To: "05:00",
		AppliesTo: []string{config.CategoryAlerts, config.CategoryScheduled},
	}
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)

	// The trigger-phase webhook still fires; it is the one the relay that
	// latches depends on.
	waitForFiring(t, hooks, "on_trigger:"+config.TriggerAlertStart)
	gate.refuse(t, "no sound was going to be made, so nothing had to wait for anything")
	refuseFiring(t, hooks, "before:"+config.TriggerAlertStart, "the siren was silenced, so there was nothing to bracket")
	refuseFiring(t, hooks, "after:"+config.TriggerAlertStart, "nothing was switched, so nothing has to be switched back")
}

// Stop has to mean the device is quiet now, not that it is quiet until a wait
// somebody started five seconds ago runs out. The sound is dropped — and the
// pair is still closed, because the before phase already fired and nothing
// else was ever going to answer it.
func TestStoppingAbandonsASoundStillWaitingOutItsDelay(t *testing.T) {
	h, hooks, gate := newGatedHarness(t, clock.System{Loc: time.UTC})
	hooks.beforeDelay = 4 * time.Second
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)

	gate.waited(t)
	waitForFiring(t, hooks, "before:"+config.TriggerAlertStart)

	h.eng.StopManual()
	gate.let(t) // the parked hand-off wakes to find its session abandoned

	waitForFiring(t, hooks, "after:"+config.TriggerAlertStart)
	refusePlayback(t, h, "the operator stopped the device before the sound was ever handed over")
}

func countFirings(got []string, want string) int {
	n := 0
	for _, g := range got {
		if g == want {
			n++
		}
	}
	return n
}
