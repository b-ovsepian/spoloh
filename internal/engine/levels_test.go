package engine_test

import (
	"context"
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
	wantHooks := []string{config.TriggerAlertStart, config.TriggerAlertEnd}
	if gotHooks := hooks.triggers(); len(gotHooks) != 2 || gotHooks[0] != wantHooks[0] || gotHooks[1] != wantHooks[1] {
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
