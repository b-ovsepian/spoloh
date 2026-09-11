package rules_test

import (
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/rules"
)

func at(hh, mm int) time.Time {
	return time.Date(2026, 9, 6, hh, mm, 0, 0, time.UTC)
}

func TestInQuietHoursCrossingMidnight(t *testing.T) {
	qh := config.QuietHours{Enabled: true, From: "22:00", To: "07:00", AppliesTo: []string{config.CategoryScheduled}}

	tests := []struct {
		h, m int
		want bool
	}{
		{21, 59, false},
		{22, 0, true},
		{23, 30, true},
		{0, 15, true},
		{6, 59, true},
		{7, 0, false},
		{13, 0, false},
	}
	for _, tc := range tests {
		if got := rules.InQuietHours(qh, at(tc.h, tc.m)); got != tc.want {
			t.Errorf("InQuietHours(%02d:%02d) = %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
}

func TestInQuietHoursSameDayWindow(t *testing.T) {
	qh := config.QuietHours{Enabled: true, From: "13:00", To: "14:00"}
	if rules.InQuietHours(qh, at(12, 59)) {
		t.Error("12:59 is outside 13:00-14:00")
	}
	if !rules.InQuietHours(qh, at(13, 0)) {
		t.Error("13:00 is inside 13:00-14:00")
	}
	if rules.InQuietHours(qh, at(14, 0)) {
		t.Error("14:00 is outside 13:00-14:00")
	}
}

func TestInQuietHoursDisabled(t *testing.T) {
	qh := config.QuietHours{Enabled: false, From: "22:00", To: "07:00"}
	if rules.InQuietHours(qh, at(23, 0)) {
		t.Error("disabled quiet hours must never match")
	}
}

// Alerts are not covered by default, and the shipped default is what almost
// every device in the field runs: a window that lists only scheduled events
// must leave a siren at 02:00 completely alone.
func TestQuietHoursLeaveAnAlertAloneWhenAlertsAreNotCovered(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryScheduled}

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(2, 0))
	if len(got) != 1 {
		t.Fatalf("alert decisions at 02:00 = %d, want 1 \u2014 alerts are not in applies_to, nothing may silence them", len(got))
	}
	if got[0].Priority != player.PriorityAlert {
		t.Fatalf("priority = %v, want PriorityAlert", got[0].Priority)
	}
	if len(rules.Suppressions(cfg, config.TriggerAlertStart, at(2, 0))) != 0 {
		t.Error("an alert that played was reported as suppressed")
	}
}

func TestQuietHoursSuppressScheduled(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryScheduled}

	if got := rules.Resolve(cfg, config.TriggerScheduled, at(2, 0)); len(got) != 0 {
		t.Fatalf("scheduled decisions at 02:00 = %d, want 0", len(got))
	}
	if got := rules.Resolve(cfg, config.TriggerScheduled, at(9, 0)); len(got) != 1 {
		t.Fatalf("scheduled decisions at 09:00 = %d, want 1", len(got))
	}
}

// A scheduled event left out of applies_to plays through the window, exactly
// as it did before alerts became selectable. The list is read the same way
// for both categories now, so this is the half of that symmetry that must not
// have regressed while the other half changed.
func TestQuietHoursLeaveScheduledAloneWhenScheduledIsNotCovered(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "08:00"
	cfg.QuietHours.To = "10:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryAlerts}

	if got := rules.Resolve(cfg, config.TriggerScheduled, at(9, 0)); len(got) != 1 {
		t.Fatalf("scheduled decisions at 09:00 = %d, want 1 \u2014 scheduled is not in applies_to", len(got))
	}
}

// The reversal, stated as a test so nobody has to infer it: an operator who
// lists the alerts category gets a device that does not sound an air raid
// alert inside the window. This was forbidden until the operator asked for
// it; it is deliberate, and the three tests below it are the conditions it
// was granted under.
func TestQuietHoursSilenceAnAlertWhenTheOperatorCoversAlerts(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryScheduled, config.CategoryAlerts}

	if got := rules.Resolve(cfg, config.TriggerAlertStart, at(2, 0)); len(got) != 0 {
		t.Fatalf("alert decisions at 02:00 = %d, want 0 \u2014 applies_to lists alerts", len(got))
	}
	// Outside the window the same configuration is an ordinary announcer.
	if got := rules.Resolve(cfg, config.TriggerAlertStart, at(13, 0)); len(got) != 1 {
		t.Fatalf("alert decisions at 13:00 = %d, want 1 \u2014 13:00 is outside 22:00-07:00", len(got))
	}
}

// Silencing an alert has to leave a trace, and this is where the trace comes
// from: the engine writes one eventlog row per Suppression. An alert this
// device chose not to sound, with nothing in the log about it, is
// indistinguishable from an alert it failed to sound.
func TestSuppressionsReportEverySilencedAlert(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryAlerts}

	got := rules.Suppressions(cfg, config.TriggerAlertStart, at(2, 0))
	if len(got) != 1 {
		t.Fatalf("suppressions = %+v, want exactly one \u2014 a silenced alert that is not reported is never recorded", got)
	}
	if got[0].EventID != "alert-start" || got[0].Sound != "siren.wav" {
		t.Fatalf("suppression = %+v, want the alert-start event and its sound", got[0])
	}
	// The all-clear is an alert too, and is silenced by the same setting.
	if end := rules.Suppressions(cfg, config.TriggerAlertEnd, at(2, 0)); len(end) != 1 || end[0].EventID != "alert-end" {
		t.Fatalf("alert-end suppressions = %+v, want the alert-end event", end)
	}
}

// Everything the log must not be filled with. A routine scheduled
// announcement, an event the operator switched off, a window that does not
// cover alerts and a time outside the window are four different reasons
// nothing sounded, and none of them is "this device silenced a raid".
func TestSuppressionsReportNothingElse(t *testing.T) {
	base := func() *config.Config {
		cfg := config.Default()
		cfg.QuietHours.Enabled = true
		cfg.QuietHours.From = "22:00"
		cfg.QuietHours.To = "07:00"
		cfg.QuietHours.AppliesTo = []string{config.CategoryScheduled, config.CategoryAlerts}
		return cfg
	}

	if got := rules.Suppressions(base(), config.TriggerScheduled, at(2, 0)); len(got) != 0 {
		t.Errorf("scheduled suppressions = %+v, want none \u2014 a quiet night is the setting working", got)
	}
	if got := rules.Suppressions(base(), config.TriggerAlertStart, at(13, 0)); len(got) != 0 {
		t.Errorf("suppressions outside the window = %+v, want none", got)
	}

	uncovered := base()
	uncovered.QuietHours.AppliesTo = []string{config.CategoryScheduled}
	if got := rules.Suppressions(uncovered, config.TriggerAlertStart, at(2, 0)); len(got) != 0 {
		t.Errorf("suppressions with alerts uncovered = %+v, want none", got)
	}

	disabled := base()
	for i := range disabled.Events {
		disabled.Events[i].Enabled = false
	}
	if got := rules.Suppressions(disabled, config.TriggerAlertStart, at(2, 0)); len(got) != 0 {
		t.Errorf("suppressions for a disabled event = %+v, want none \u2014 it was switched off, not silenced", got)
	}
}

func TestVolumeCombinesMaster(t *testing.T) {
	cfg := config.Default()
	cfg.Audio.MasterVolume = 50 // event volume is 100 for alert-start

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	if got[0].Volume != 50 {
		t.Fatalf("volume = %d, want 50 (100%% event of 50%% master)", got[0].Volume)
	}
}

func TestDisabledEventsAreSkipped(t *testing.T) {
	cfg := config.Default()
	for i := range cfg.Events {
		cfg.Events[i].Enabled = false
	}
	if got := rules.Resolve(cfg, config.TriggerAlertStart, at(12, 0)); len(got) != 0 {
		t.Fatalf("decisions = %d, want 0", len(got))
	}
}

func TestResolveIgnoresOtherTriggers(t *testing.T) {
	cfg := config.Default()
	got := rules.Resolve(cfg, config.TriggerAlertEnd, at(12, 0))
	if len(got) != 1 || got[0].EventID != "alert-end" {
		t.Fatalf("decisions = %+v, want only alert-end", got)
	}
}

func TestVolumeRoundsInsteadOfTruncating(t *testing.T) {
	cfg := config.Default()
	// alert-end has default volume 90; master volume 1 would truncate to 0.
	// Rounding must produce a non-zero result instead.
	cfg.Audio.MasterVolume = 1

	got := rules.Resolve(cfg, config.TriggerAlertEnd, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	// 90 * 1 / 100: truncates to 0, but (90*1+50)/100 = 140/100 = 1
	if got[0].Volume != 1 {
		t.Fatalf("volume = %d, want 1 (rounded from 0.9)", got[0].Volume)
	}
}

func TestVolumeClampedToOneWhenBothNonZero(t *testing.T) {
	cfg := config.Default()
	// Craft an event with volume 1, master volume 1.
	cfg.Events = []config.Event{
		{ID: "test", Trigger: config.TriggerScheduled, Sound: "test.wav", Volume: 1, Enabled: true},
	}
	cfg.Audio.MasterVolume = 1

	got := rules.Resolve(cfg, config.TriggerScheduled, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	// 1 * 1 / 100: truncates to 0, but (1*1+50)/100 = 51/100 = 0, then clamped to 1
	if got[0].Volume != 1 {
		t.Fatalf("volume = %d, want 1 (clamped from rounding)", got[0].Volume)
	}
}

func TestVolumeZeroWhenMasterZero(t *testing.T) {
	cfg := config.Default()
	cfg.Audio.MasterVolume = 0

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	if got[0].Volume != 0 {
		t.Fatalf("volume = %d, want 0 (explicit mute)", got[0].Volume)
	}
}

func TestVolumeZeroWhenEventZero(t *testing.T) {
	cfg := config.Default()
	// Set alert-start volume to 0 (explicit mute).
	cfg.Events[0].Volume = 0
	cfg.Audio.MasterVolume = 100

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	if got[0].Volume != 0 {
		t.Fatalf("volume = %d, want 0 (explicit event mute)", got[0].Volume)
	}
}

func TestVolumeExactCalculationPreserved(t *testing.T) {
	cfg := config.Default()
	cfg.Audio.MasterVolume = 50 // alert-start volume is 100

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	if got[0].Volume != 50 {
		t.Fatalf("volume = %d, want 50 (100*50/100)", got[0].Volume)
	}
}

// The shipped default configuration protects alerts from quiet hours. This test
// pins that protection to the actual default values, so changes to config.Default()
// that remove alerts from the safety guarantee will fail here.
func TestShippedDefaultDoesNotSuppressAlertsInQuietHours(t *testing.T) {
	cfg := config.Default()
	// Only change: enable quiet hours that contain the test time.
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	// Do not touch AppliesTo — test the shipped default.

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(2, 0))
	if len(got) != 1 {
		t.Fatalf("shipped default: alert decisions at 02:00 = %d, want 1", len(got))
	}
	if got[0].Priority != player.PriorityAlert {
		t.Fatalf("priority = %v, want PriorityAlert", got[0].Priority)
	}
}

// A window whose start equals its end resolves to "not quiet", not to "quiet
// all day". Pinned deliberately: this is the safety-critical direction of an
// ambiguous configuration — a 24-hour quiet window would silence every
// scheduled event on the device, and the operator who typed 22:00 twice by
// accident would have no idea why. The choice must not be flipped silently.
func TestInQuietHoursEqualFromAndToIsNotQuiet(t *testing.T) {
	qh := config.QuietHours{Enabled: true, From: "22:00", To: "22:00", AppliesTo: []string{config.CategoryScheduled}}

	for _, hm := range []string{"00:00", "07:30", "21:59", "22:00", "22:01", "23:59"} {
		at, err := time.Parse("15:04", hm)
		if err != nil {
			t.Fatalf("Parse %s: %v", hm, err)
		}
		if rules.InQuietHours(qh, at) {
			t.Fatalf("InQuietHours at %s = true, want false for a zero-length window", hm)
		}
	}
}

// A Decision carries the event's own repeat count through unchanged, so the
// engine knows how many times to queue the sound.
func TestDecisionCarriesEventRepeat(t *testing.T) {
	cfg := config.Default()
	cfg.Events = []config.Event{
		{ID: "test", Trigger: config.TriggerAlertStart, Sound: "test.wav", Volume: 90, Repeat: 4, Enabled: true},
	}

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(12, 0))
	if len(got) != 1 {
		t.Fatalf("decisions = %d, want 1", len(got))
	}
	if got[0].Repeat != 4 {
		t.Fatalf("repeat = %d, want 4", got[0].Repeat)
	}
}

// ForEvent must never let a zero repeat through, whatever produced the
// config.Config it was handed. config.Load and config.Validate keep a stored
// event's Repeat at 1 or above (see store.go's restoreMissingRepeat and
// Event.validate), but this Event is built directly, bypassing both — the
// same way a config assembled by future code, not only a test, could reach
// ForEvent without ever having gone through either guard. Silence here would
// be indistinguishable from the event never firing at all.
func TestForEventFloorsAZeroRepeatToOne(t *testing.T) {
	cfg := config.Default()
	cfg.Events = []config.Event{
		{ID: "test", Trigger: config.TriggerAlertStart, Sound: "test.wav", Volume: 90, Enabled: true}, // Repeat left at the zero value
	}

	got, ok := rules.ForEvent(cfg, cfg.Events[0], at(12, 0))
	if !ok {
		t.Fatal("ForEvent refused an enabled event")
	}
	if got.Repeat != 1 {
		t.Fatalf("repeat = %d, want 1 (a zero repeat must never reach the player)", got.Repeat)
	}
}
