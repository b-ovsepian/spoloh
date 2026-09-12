package engine_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
)

// firing is one call the engine made to Deps.Webhooks.
type firing struct {
	phase   string
	trigger string
}

func (f firing) String() string { return f.phase + ":" + f.trigger }

// triggerLog records what the engine handed to Deps.Webhooks, which is the
// whole of the contract between this package and internal/webhook: a phase,
// a trigger name, a delay handed back, and nothing that can block.
type triggerLog struct {
	mu   sync.Mutex
	seen []firing

	// beforeDelay is what a config.PhaseBefore dispatch reports back, the
	// way a real dispatcher reports the longest delay among the webhooks it
	// just fired. Zero — the default — is a device with no delay
	// configured, which is every test that does not care about one.
	beforeDelay time.Duration
}

func (l *triggerLog) fire(phase, trigger string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, firing{phase: phase, trigger: trigger})
	if phase == config.PhaseBefore {
		return l.beforeDelay
	}
	return 0
}

// triggers reports the config.PhaseOnTrigger firings only, which is what
// every webhook stored before phases existed does. The tests that predate
// phases assert through this deliberately: their subject is that a device in
// the field goes on firing exactly when it always did.
func (l *triggerLog) triggers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, f := range l.seen {
		if f.phase == config.PhaseOnTrigger {
			out = append(out, f.trigger)
		}
	}
	return out
}

// firings reports every call, phase included, as "phase:trigger".
func (l *triggerLog) firings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.seen))
	for _, f := range l.seen {
		out = append(out, f.String())
	}
	return out
}

// newHarnessWithWebhooks is newHarnessWith plus somewhere for the triggers to
// land.
func newHarnessWithWebhooks(t *testing.T, clk clock.Clock) (*harness, *triggerLog) {
	t.Helper()
	log := &triggerLog{}
	h := newHarnessWith(t, alertstate.New(), clk, func(d *engine.Deps) {
		d.Webhooks = log.fire
	})
	return h, log
}

func TestAWebhookFiresOnTheAlertStartTransition(t *testing.T) {
	h, hooks := newHarnessWithWebhooks(t, clock.System{Loc: time.UTC})
	ctx := context.Background()

	pollTimes(t, h, ctx, 1) // calm: nothing has happened yet
	if got := hooks.triggers(); len(got) != 0 {
		t.Fatalf("a calm poll fired %v", got)
	}

	h.set(true)
	pollTimes(t, h, ctx, 1)

	want := []string{config.TriggerAlertStart}
	if got := hooks.triggers(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
}

// The all-clear is announced only after alertstate.EndConfirmations
// consecutive calm readings, because one source blinking is not the end of a
// raid. The webhook has to keep the same company: an operator's relay that
// opened on the first calm reading would switch their siren off in the
// middle of a raid that was still running.
func TestAnAlertEndWebhookWaitsForTheConfirmedAllClear(t *testing.T) {
	h, hooks := newHarnessWithWebhooks(t, clock.System{Loc: time.UTC})
	ctx := context.Background()

	// One calm poll first: a machine's very first reading baselines it and
	// announces nothing, because a device must never announce a transition
	// it did not witness.
	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)
	h.set(false)

	// One short of the confirmation the machine requires.
	pollTimes(t, h, ctx, alertstate.EndConfirmations-1)
	for _, got := range hooks.triggers() {
		if got == config.TriggerAlertEnd {
			t.Fatalf("the end-of-alert webhook fired after %d calm readings; %d are required",
				alertstate.EndConfirmations-1, alertstate.EndConfirmations)
		}
	}

	pollTimes(t, h, ctx, 1)

	want := []string{config.TriggerAlertStart, config.TriggerAlertEnd}
	got := hooks.triggers()
	if len(got) != len(want) {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("triggers = %v, want %v", got, want)
		}
	}
}

// Quiet hours decide what the device makes people hear at night. They are not
// an instruction to leave somebody else's hardware latched on: the relay that
// closed when the raid started has to be told the raid ended, or it stays
// closed with nothing left to open it.
//
// Both halves in one test, because the pair is the point — a webhook that
// fired on the start and not on the end would be the worst of the two
// behaviours, and either half alone would pass.
func TestQuietHoursSilenceTheSirenAndStillFireTheWebhook(t *testing.T) {
	// A fixed clock inside the window, not the wall clock and a window
	// stretched to cover the day: the first version of this test used
	// 00:00–23:59 against clock.System and passed for seven minutes, until
	// the suite happened to run during the one minute of the day that
	// window leaves out.
	clk := clock.NewFake(time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC))
	h, hooks := newHarnessWithWebhooks(t, clk)
	ctx := context.Background()
	h.cfg.QuietHours = config.QuietHours{
		Enabled: true, From: "01:00", To: "05:00",
		AppliesTo: []string{config.CategoryAlerts, config.CategoryScheduled},
	}

	pollTimes(t, h, ctx, 1) // the baselining first reading
	h.set(true)
	pollTimes(t, h, ctx, 1)
	h.set(false)
	pollTimes(t, h, ctx, alertstate.EndConfirmations)

	want := []string{config.TriggerAlertStart, config.TriggerAlertEnd}
	got := hooks.triggers()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
	// The siren itself stayed silent: that is what quiet hours were asked
	// for, and the row saying so is written elsewhere (KindAlertSuppressed).
	if played := h.rec.Played(); len(played) != 0 {
		t.Fatalf("played %v while quiet hours covered alerts", played)
	}
}

// A scheduled announcement latches nothing, so its webhook follows the sound:
// firing one for a chime the device deliberately did not make would be the
// device acting at a moment it had been told to stay out of.
func TestAScheduledWebhookDoesNotFireWhenTheChimeIsSuppressed(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC))
	h, hooks := newHarnessWithWebhooks(t, clk)
	h.cfg.QuietHours = config.QuietHours{
		Enabled: true, From: "08:00", To: "10:00",
		AppliesTo: []string{config.CategoryScheduled},
	}

	h.eng.Tick(context.Background())

	for _, got := range hooks.triggers() {
		if got == config.TriggerScheduled {
			t.Fatal("a scheduled webhook fired for an announcement quiet hours suppressed")
		}
	}
}

func TestAScheduledWebhookFiresWithItsAnnouncement(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC))
	h, hooks := newHarnessWithWebhooks(t, clk)

	h.eng.Tick(context.Background())
	waitPlayed(t, h.rec)

	want := []string{config.TriggerScheduled}
	if got := hooks.triggers(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
}

// Every other optional dependency here tolerates nil, and this one has to as
// well: a device with no webhooks configured is the device every one of them
// ships as.
func TestAnEngineWithNoWebhookDispatcherStillAnnounces(t *testing.T) {
	h := newHarness(t)                       // Deps.Webhooks left nil
	pollTimes(t, h, context.Background(), 1) // the baselining first reading
	h.set(true)
	pollTimes(t, h, context.Background(), 1)

	if got := waitPlayed(t, h.rec); got == "" {
		t.Fatal("nothing was played with no webhook dispatcher wired up")
	}
}
