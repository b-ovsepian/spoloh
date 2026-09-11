package config_test

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
)

func TestRedactedRemovesEveryAuthField(t *testing.T) {
	c := config.Default()
	c.Auth.PasswordHash = "$2a$10$abcdefghijklmnopqrstuv"
	c.Auth.SessionSecret = "s3cr3t-session-key"

	got := c.Redacted()

	if got.Auth.PasswordHash != "" || got.Auth.SessionSecret != "" {
		t.Errorf("Auth = %+v, want every field empty", got.Auth)
	}
	// The whole block, not the two fields someone remembered: the signing key
	// is derived from both, so a field added later is a credential too.
	if (config.Auth{}) != got.Auth {
		t.Errorf("Auth = %+v, want the zero value", got.Auth)
	}
}

func TestRedactedDoesNotDisturbTheOriginal(t *testing.T) {
	c := config.Default()
	c.Auth.PasswordHash = "hash"
	c.Auth.SessionSecret = "secret"

	_ = c.Redacted()

	if c.Auth.PasswordHash != "hash" || c.Auth.SessionSecret != "secret" {
		t.Error("Redacted mutated the config it was given; the caller still needs it to sign cookies")
	}
}

func TestRedactedIsADeepCopy(t *testing.T) {
	// Events and Weekdays are slices. A shallow copy would let a caller that
	// renders a redacted config mutate the live one.
	c := config.Default()
	got := c.Redacted()
	if len(got.Events) == 0 {
		t.Fatal("Default has no events to check")
	}
	got.Events[0].Sound = "changed.wav"
	if c.Events[0].Sound == "changed.wav" {
		t.Error("Redacted shares its Events backing array with the original")
	}
}

func TestRedactedKeepsEverythingElse(t *testing.T) {
	c := config.Default()
	c.Alerts.Region = "Львівська область"
	c.Network.APPassword = "sp0lohTest42"

	got := c.Redacted()

	if got.Alerts.Region != "Львівська область" {
		t.Errorf("Region = %q, want it preserved", got.Alerts.Region)
	}
	// The access point password stays: the network page shows it on purpose,
	// because an operator who lost the installer's output has nowhere else to
	// find it. Only Auth is a secret this process must never hand out.
	if got.Network.APPassword != "sp0lohTest42" {
		t.Errorf("APPassword = %q, want it preserved", got.Network.APPassword)
	}
}
