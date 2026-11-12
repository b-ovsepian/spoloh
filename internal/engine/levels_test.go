package engine_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
)

// levelSource answers with a severity, which none of the six Ubilling keys
// can: it stands in for alerts.in.ua, the only provider here that reports one.
//
// Driven in process rather than over HTTP, unlike the default harness's
// source, because what these tests are about is what the engine does with a
// level once it has one — alertsua_test.go already covers turning a response
// into one.
type levelSource struct {
	mu    sync.Mutex
	state alertsource.RegionState
}

func newLevelSource(region string) *levelSource {
	return &levelSource{state: alertsource.RegionState{Region: region}}
}

func (s *levelSource) set(alert bool, l alertsource.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Alert = alert
	s.state.Level = l
}

func (s *levelSource) Fetch(context.Context) (alertsource.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return alertsource.Snapshot{
		Source: "alerts.in.ua",
		States: map[string]alertsource.RegionState{s.state.Region: s.state},
	}, nil
}

// newLevelHarness is the standard harness with a level-reporting source and
// somewhere for the webhook triggers to land.
func newLevelHarness(t *testing.T) (*harness, *levelSource, *triggerLog) {
	t.Helper()
	src := newLevelSource("м. Київ")
	hooks := &triggerLog{}
	h := newHarnessWith(t, alertstate.New(), clock.System{Loc: time.UTC}, func(d *engine.Deps) {
		d.Source = src
		d.Webhooks = hooks.fire
	})
	return h, src, hooks
}

// Every configuration on a device today has one alert_start event and no
// escalation event, because the trigger did not exist when they were written.
// Such a device must still be heard when a yellow warning is declared red:
// gaining a level and losing the siren is the worst trade this project can
// make.
func TestAnEscalationSoundsTheSirenWithNothingConfiguredForIt(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	ctx := context.Background()

	for _, ev := range h.cfg.Events {
		if ev.Trigger == config.TriggerAlertEscalated {
			t.Fatalf("this test is about a config with no escalation event; the default has %q", ev.ID)
		}
	}

	pollTimes(t, h, ctx, 1) // the quiet baseline
	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("the yellow alert played %q, want siren.wav", got)
	}

	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("the escalation played %q, want the start sound standing in for it", got)
	}
}

// With no sound of its own, an escalation is announced with the start's
// sound, and as the start's session: the before-webhook that wakes the
// amplifier for that sound is the start's, and it has to fire for the
// escalation too, or the siren standing in for it plays into a sleeping
// amplifier.
func TestAnEscalationWithNothingConfiguredIsBracketedAsAStart(t *testing.T) {
	h, src, hooks := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)
	waitForFiring(t, hooks, config.PhaseAfter+":"+config.TriggerAlertStart)
	before := len(hooks.firings())

	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)
	waitForFiring(t, hooks, config.PhaseAfter+":"+config.TriggerAlertStart)
	got := hooks.firings()[before:]
	if !slices.Contains(got, config.PhaseBefore+":"+config.TriggerAlertStart) {
		t.Fatalf("the escalation fired %v, want the start's before phase around the start's sound", got)
	}
}

// An operator who does configure a sound for the escalation hears that one
// instead — one announcement, not the escalation's and the start's together.
func TestAnEscalationPlaysItsOwnSoundWhenOneIsConfigured(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	ctx := context.Background()

	h.cfg.Events = append(h.cfg.Events, config.Event{
		ID: "alert-escalated", Trigger: config.TriggerAlertEscalated,
		Sound: "metronome.wav", Volume: 100, Repeat: 1, Enabled: true,
	})

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)

	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "metronome.wav" {
		t.Fatalf("the escalation played %q, want the sound configured for it", got)
	}

	// And the raid then ends normally, which is what proves nothing was
	// queued behind the escalation: the next sound to reach the speaker is
	// the all-clear and not a second siren.
	src.set(false, alertsource.LevelUnknown)
	pollTimes(t, h, ctx, alertstate.EndConfirmations)
	if got := waitPlayed(t, h.rec); got != "all-clear.wav" {
		t.Fatalf("played %q after the escalation, want all-clear.wav", got)
	}
}

// The escalation has its own webhook name whether or not it has its own
// sound. An operator whose relay board wants to know about red specifically
// must not be handed the start's trigger instead.
func TestAnEscalationFiresItsOwnWebhookTrigger(t *testing.T) {
	h, src, hooks := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)

	want := []string{config.TriggerAlertStart, config.TriggerAlertEscalated}
	got := hooks.triggers()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
}

// withEscalationSound gives the harness an operator who chose a sound for a
// raid declared red, which is what makes a red start distinguishable from a
// yellow one at the speaker at all.
func withEscalationSound(h *harness) {
	h.cfg.Events = append(h.cfg.Events, config.Event{
		ID: "alert-escalated", Trigger: config.TriggerAlertEscalated,
		Sound: "metronome.wav", Volume: 100, Repeat: 1, Enabled: true,
	})
}

// Quiet, then red at once: no yellow came first, so no escalation follows.
// A device whose alert-start sound is the yellow warning played that warning
// for a full air raid — seen on a real board, where the operator had chosen
// the start's sound expecting red to arrive as an escalation. The red sound
// the operator configured is the one a red raid is announced with, whichever
// way it got to red.
func TestARaidThatStartsRedPlaysTheRedSound(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	withEscalationSound(h)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "metronome.wav" {
		t.Fatalf("a raid that started red played %q, want metronome.wav, the sound configured for red", got)
	}

	// One announcement, not the red one and the start's together: the next
	// sound to reach the speaker is the all-clear.
	src.set(false, alertsource.LevelUnknown)
	pollTimes(t, h, ctx, alertstate.EndConfirmations)
	if got := waitPlayed(t, h.rec); got != "all-clear.wav" {
		t.Fatalf("played %q after the red start, want all-clear.wav", got)
	}
}

// Unknown is handled as red, and a start is where that rule matters most: a
// source that cannot name the level must not get the lighter announcement.
func TestARaidThatStartsAtAnUnknownLevelPlaysTheRedSound(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	withEscalationSound(h)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelUnknown)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "metronome.wav" {
		t.Fatalf("a raid that started at an unknown level played %q, want metronome.wav: unknown is red", got)
	}
}

// A yellow start is still the start: the red sound is for red.
func TestARaidThatStartsYellowPlaysTheStartSound(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	withEscalationSound(h)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("a raid that started yellow played %q, want siren.wav", got)
	}
}

// Both things are true of a raid that starts red — it began, and it is red —
// so both webhooks fire: the relay that marks a raid closes, and so does the
// one an operator wired to red specifically. Firing only the start's would
// leave the red relay open through a red raid.
func TestARaidThatStartsRedFiresBothWebhooks(t *testing.T) {
	h, src, hooks := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)

	want := []string{config.TriggerAlertStart, config.TriggerAlertEscalated}
	got := hooks.triggers()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("triggers = %v, want %v", got, want)
	}
}

// A red start is still a start at the speaker's end. An operator whose
// before-webhook on alert_start wakes an amplifier or mutes a music feed
// needs it to fire for this raid too, or the red sound plays into a sleeping
// amplifier: the session is the start's, only the sound is red's.
func TestARaidThatStartsRedIsBracketedAsAStart(t *testing.T) {
	h, src, hooks := newLevelHarness(t)
	withEscalationSound(h)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)

	got := waitForFiring(t, hooks, config.PhaseBefore+":"+config.TriggerAlertStart)
	if slices.Contains(got, config.PhaseBefore+":"+config.TriggerAlertEscalated) {
		t.Fatalf("firings = %v: a red start opened an escalation's session as well as or instead of the start's", got)
	}
}

// The red sound configured but gone from the library — an SD card that lost a
// file, a hand-deleted sound. Before red starts used it, that could cost at
// most an escalation's announcement mid-raid; now it would cost the start of
// the raid itself, so the start's sound stands in.
func TestARaidThatStartsRedWithItsRedSoundMissingPlaysTheStartSound(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	h.cfg.Events = append(h.cfg.Events, config.Event{
		ID: "alert-escalated", Trigger: config.TriggerAlertEscalated,
		Sound: "gone-from-the-card.wav", Volume: 100, Repeat: 1, Enabled: true,
	})
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("a red start whose red sound is missing played %q, want siren.wav", got)
	}
}

// With nothing configured for red, a red start sounds exactly as it always
// has: the start's sound, never silence.
func TestARaidThatStartsRedWithNothingConfiguredForRedPlaysTheStartSound(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("a red start with no red sound configured played %q, want siren.wav", got)
	}
}

// The cascade falling off alerts.in.ua onto a level-blind source, and back.
// The raid did not change; the reporting did. Proven by what reaches the
// speaker over the whole raid: the siren that raised it and the all-clear
// that ended it, with nothing in between.
func TestACascadeFallingToALevelBlindSourceAnnouncesNothing(t *testing.T) {
	h, src, hooks := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("the red alert played %q, want siren.wav", got)
	}

	// Every Ubilling key reports a bare boolean, so this is what the whole
	// fallback looks like from here — and then the recovery.
	src.set(true, alertsource.LevelUnknown)
	pollTimes(t, h, ctx, 5)
	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 2)

	src.set(false, alertsource.LevelUnknown)
	pollTimes(t, h, ctx, alertstate.EndConfirmations)
	if got := waitPlayed(t, h.rec); got != "all-clear.wav" {
		t.Fatalf("played %q, want all-clear.wav", got)
	}

	want := []string{"siren.wav", "all-clear.wav"}
	got := h.rec.Played()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the raid played %v, want %v: a source that cannot name the level said nothing about it", got, want)
	}
	// The red start fires the escalation's webhook too (see
	// TestARaidThatStartsRedFiresBothWebhooks); after it, nothing until the end.
	wantHooks := []string{config.TriggerAlertStart, config.TriggerAlertEscalated, config.TriggerAlertEnd}
	if gotHooks := hooks.triggers(); len(gotHooks) != 3 || gotHooks[0] != wantHooks[0] || gotHooks[1] != wantHooks[1] || gotHooks[2] != wantHooks[2] {
		t.Fatalf("triggers = %v, want %v", hooks.triggers(), wantHooks)
	}
}

// The level reaches the dashboard, which is the operator's whole diagnostic
// surface on a board with no monitor.
func TestStatusReportsTheLevel(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	if got := h.eng.Status().Level; got != alertsource.LevelUnknown {
		t.Fatalf("Level = %q with nothing running, want unknown", got)
	}

	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)
	if got := h.eng.Status().Level; got != alertsource.LevelYellow {
		t.Fatalf("Level = %q, want yellow", got)
	}

	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)
	if got := h.eng.Status().Level; got != alertsource.LevelRed {
		t.Fatalf("Level = %q after the escalation, want red", got)
	}
}

// The level is written to state.json on the transition that changed it, so a
// board that loses power mid-raid comes back reporting the severity it was
// already announcing rather than an empty one.
func TestTheEscalatedLevelIsPersisted(t *testing.T) {
	h, src, _ := newLevelHarness(t)
	ctx := context.Background()

	pollTimes(t, h, ctx, 1)
	src.set(true, alertsource.LevelYellow)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)

	src.set(true, alertsource.LevelRed)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)

	p, err := alertstate.LoadPersisted(h.statePath)
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if p.Level != alertsource.LevelRed {
		t.Fatalf("the stored level is %q, want red", p.Level)
	}
}
