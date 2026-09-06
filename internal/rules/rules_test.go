package rules_test

import (
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/config"
	"github.com/spoloh/spoloh/internal/player"
	"github.com/spoloh/spoloh/internal/rules"
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

// The single most important rule in the system: night must never silence a
// siren. A device that goes quiet at 02:00 is silent exactly when it matters.
func TestQuietHoursNeverSuppressAlerts(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryScheduled}

	got := rules.Resolve(cfg, config.TriggerAlertStart, at(2, 0))
	if len(got) != 1 {
		t.Fatalf("alert decisions at 02:00 = %d, want 1", len(got))
	}
	if got[0].Priority != player.PriorityAlert {
		t.Fatalf("priority = %v, want PriorityAlert", got[0].Priority)
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

// An operator who explicitly lists alerts in applies_to gets what they asked
// for; the protection is in the default, not in a veto.
func TestQuietHoursCanSuppressAlertsWhenExplicitlyConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.QuietHours.Enabled = true
	cfg.QuietHours.From = "22:00"
	cfg.QuietHours.To = "07:00"
	cfg.QuietHours.AppliesTo = []string{config.CategoryScheduled, config.CategoryAlerts}

	if got := rules.Resolve(cfg, config.TriggerAlertStart, at(2, 0)); len(got) != 0 {
		t.Fatalf("decisions = %d, want 0 when alerts are explicitly listed", len(got))
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
