package config_test

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
)

func TestCloneIsDeep(t *testing.T) {
	original := config.Default()
	clone := original.Clone()

	clone.Alerts.Region = "Львівська область"
	clone.Alerts.Sources[0] = "changed"
	clone.Events[0].Sound = "changed.wav"
	clone.Events[2].Weekdays[0] = 7
	clone.QuietHours.AppliesTo[0] = "changed"

	if original.Alerts.Region == "Львівська область" {
		t.Error("Region leaked through the clone")
	}
	if original.Alerts.Sources[0] == "changed" {
		t.Error("Sources slice is shared with the clone")
	}
	if original.Events[0].Sound == "changed.wav" {
		t.Error("Events slice is shared with the clone")
	}
	if original.Events[2].Weekdays[0] == 7 {
		t.Error("Weekdays slice is shared with the clone")
	}
	if original.QuietHours.AppliesTo[0] == "changed" {
		t.Error("AppliesTo slice is shared with the clone")
	}
}

func TestCloneRoundTripsEveryField(t *testing.T) {
	original := config.Default()
	original.Auth.PasswordHash = "hash"
	original.Network.APPassword = "secret12"

	clone := original.Clone()

	if err := clone.Validate(); err != nil {
		t.Fatalf("a clone of a valid config must validate: %v", err)
	}
	if clone.Auth.PasswordHash != "hash" || clone.Network.APPassword != "secret12" {
		t.Fatal("clone dropped a field")
	}
	if clone.Alerts.PollInterval != original.Alerts.PollInterval {
		t.Fatal("clone dropped the poll interval")
	}
}
