package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
)

func TestExportWithoutSecretsCarriesNoCredential(t *testing.T) {
	c := config.Default()
	c.Alerts.AlertsUAToken = "TOKEN-SHOULD-NOT-APPEAR"
	c.Auth.PasswordHash = "HASH-SHOULD-NOT-APPEAR"
	c.Webhooks = []config.Webhook{{
		ID: "w1", Trigger: config.TriggerAlertStart, Method: "GET",
		URL: "http://192.168.0.105/setvol?mute=1", Enabled: true,
		Headers: []config.WebhookHeader{{Name: "Authorization", Value: "HEADER-SHOULD-NOT-APPEAR"}},
	}}

	data, err := config.Export(c, false)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, secret := range []string{"TOKEN-SHOULD-NOT-APPEAR", "HASH-SHOULD-NOT-APPEAR", "HEADER-SHOULD-NOT-APPEAR"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("a redacted export carries %s", secret)
		}
	}
}

func TestExportWithSecretsKeepsTheOperatorsOwnButNeverAuth(t *testing.T) {
	c := config.Default()
	c.Alerts.AlertsUAToken = "TOKEN-THE-OPERATOR-TYPED"
	c.Auth.PasswordHash = "HASH-THIS-INSTALL-DERIVED"
	c.Auth.SessionSecret = "SECRET-THIS-INSTALL-DERIVED"

	data, err := config.Export(c, true)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !strings.Contains(string(data), "TOKEN-THE-OPERATOR-TYPED") {
		t.Error("an export with credentials dropped the token the operator typed")
	}
	// Never, at any setting. See this task's note.
	for _, never := range []string{"HASH-THIS-INSTALL-DERIVED", "SECRET-THIS-INSTALL-DERIVED"} {
		if strings.Contains(string(data), never) {
			t.Errorf("the export carries %s; Auth is this installation's, not the operator's", never)
		}
	}
}

// Export hands its result to an HTTP response and nothing else, so a copy
// that reached back into the live configuration would corrupt the running
// daemon rather than the file. Clone and Redacted each promise a deep copy;
// this states that Export still has one by the time it returns.
func TestExportDoesNotDisturbTheRunningConfig(t *testing.T) {
	c := config.Default()
	c.Alerts.AlertsUAToken = "token-the-poller-needs"
	c.Auth.PasswordHash = "hash-the-login-needs"
	c.Auth.SessionSecret = "secret-the-cookies-need"
	c.Network.APPassword = "sp0lohTest42"

	for _, withSecrets := range []bool{false, true} {
		if _, err := config.Export(c, withSecrets); err != nil {
			t.Fatalf("Export(withSecrets=%v): %v", withSecrets, err)
		}
	}

	if c.Alerts.AlertsUAToken != "token-the-poller-needs" {
		t.Errorf("AlertsUAToken = %q; the poller still needs it after a download", c.Alerts.AlertsUAToken)
	}
	if c.Auth.PasswordHash != "hash-the-login-needs" || c.Auth.SessionSecret != "secret-the-cookies-need" {
		t.Errorf("Auth = %+v; the dashboard still has to sign cookies after a download", c.Auth)
	}
	if c.Network.APPassword != "sp0lohTest42" {
		t.Errorf("APPassword = %q; the access point still needs it after a download", c.Network.APPassword)
	}
}

// The access point password is the one credential Redacted deliberately
// keeps, because the network page has to show it. A file that leaves the
// device is a different audience from a screen on it, so the export without
// credentials drops it even though Redacted does not.
func TestExportWithoutSecretsDropsTheAccessPointPassword(t *testing.T) {
	c := config.Default()
	c.Network.APPassword = "AP-SHOULD-NOT-APPEAR"

	data, err := config.Export(c, false)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if strings.Contains(string(data), "AP-SHOULD-NOT-APPEAR") {
		t.Error("a redacted export carries the access point password; a shared file is not the network page")
	}

	data, err = config.Export(c, true)
	if err != nil {
		t.Fatalf("Export with secrets: %v", err)
	}
	if !strings.Contains(string(data), "AP-SHOULD-NOT-APPEAR") {
		t.Error("an export with credentials dropped the access point password, which is one of the keys it promises to carry")
	}
}

// A downloaded file is only a backup if it reads back. Nothing in this
// package parses one yet, so this states the shape the future import has to
// find: valid JSON that unmarshals into a Config with the settings intact.
func TestExportIsJSONThatReadsBackAsAConfig(t *testing.T) {
	c := config.Default()
	c.Device.Name = "Сполох у майстерні"
	c.Alerts.Region = "Львівська область"

	for _, withSecrets := range []bool{false, true} {
		data, err := config.Export(c, withSecrets)
		if err != nil {
			t.Fatalf("Export(withSecrets=%v): %v", withSecrets, err)
		}
		var back config.Config
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("Export(withSecrets=%v) is not JSON: %v\n%s", withSecrets, err, data)
		}
		if back.Device.Name != "Сполох у майстерні" || back.Alerts.Region != "Львівська область" {
			t.Errorf("Export(withSecrets=%v) lost settings: Device.Name=%q Alerts.Region=%q",
				withSecrets, back.Device.Name, back.Alerts.Region)
		}
		if back.Schema != c.Schema {
			t.Errorf("Export(withSecrets=%v) wrote schema %d, want %d; an import has to know what it is reading",
				withSecrets, back.Schema, c.Schema)
		}
	}
}
