package config_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// storeRawWebhook writes a config.json whose single webhook is exactly the
// JSON given, so a test can store a phase or a delay that Save would refuse
// to write. A hand-edited file on an SD card is the case every repair in
// store.go exists for, and it is the only way to produce one.
func storeRawWebhook(t *testing.T, path, webhookJSON string) {
	t.Helper()
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	var hook map[string]any
	if err := json.Unmarshal([]byte(webhookJSON), &hook); err != nil {
		t.Fatalf("Unmarshal webhook: %v", err)
	}
	generic["webhooks"] = []any{hook}
	out, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })
	return &logs
}

// A delay above the cap is latency on a siren, and the form refuses to write
// one. A file that already holds one is a different question: refusing to
// load it would stop the daemon starting, which trades an over-long delay for
// no announcer at all.
func TestLoadLowersAnOverLongWebhookDelayToTheMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeRawWebhook(t, path, `{"id":"w1","trigger":"alert_start","method":"GET",
		"url":"http://192.168.1.50/on","enabled":true,"phase":"before","delay":"45s"}`)
	logs := captureLog(t)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load refused a stored 45s webhook delay instead of repairing it: %v", err)
	}
	if len(loaded.Webhooks) != 1 {
		t.Fatalf("webhooks = %d, want 1", len(loaded.Webhooks))
	}
	if got := loaded.Webhooks[0].Delay.Std(); got != config.MaxWebhookDelay {
		t.Fatalf("delay = %v, want it lowered to the %v maximum exactly", got, config.MaxWebhookDelay)
	}
	if !strings.Contains(logs.String(), "stored=45s") {
		t.Errorf("the log line does not name the value that was repaired; log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("the repair was not logged at Error; log = %q", logs.String())
	}
}

// Only ever clamps. A delay the operator chose and this build allows must
// come back exactly as saved.
func TestLoadPreservesAWebhookDelayInsideTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Webhooks = []config.Webhook{{
		ID: "w1", Trigger: config.TriggerAlertStart, Method: "GET",
		URL: "http://192.168.1.50/on", Enabled: true,
		Phase: config.PhaseBefore, Delay: config.Duration(4 * time.Second),
	}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Webhooks[0].Delay.Std(); got != 4*time.Second {
		t.Fatalf("delay = %v, want the stored 4s left alone", got)
	}
}

// A mistyped phase in a hand-edited file must not cost the operator the
// webhook. ValidateWebhook refuses an unknown phase and internal/webhook
// refuses to fire anything ValidateWebhook refuses, so without the repair one
// wrong word would silently stop the request at the moment it mattered.
func TestLoadRepairsAWebhookPhaseThisBuildDoesNotKnow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeRawWebhook(t, path, `{"id":"w1","trigger":"alert_start","method":"GET",
		"url":"http://192.168.1.50/on","enabled":true,"phase":"aftr"}`)
	logs := captureLog(t)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load refused an unknown webhook phase instead of repairing it: %v", err)
	}
	if got := loaded.Webhooks[0].Phase; got != config.PhaseOnTrigger {
		t.Fatalf("phase = %q, want it repaired to %q", got, config.PhaseOnTrigger)
	}
	if err := config.ValidateWebhook(loaded.Webhooks[0]); err != nil {
		t.Fatalf("the repaired webhook still will not fire: %v", err)
	}
	if !strings.Contains(logs.String(), "stored=aftr") {
		t.Errorf("the log line does not name the phase that was repaired; log = %q", logs.String())
	}
}

// Every webhook on every device in the field has no phase at all, because the
// field did not exist when they were written. They must go on firing exactly
// where they always did.
func TestAStoredWebhookWithNoPhaseFiresWhenTheTriggerHappens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeRawWebhook(t, path, `{"id":"w1","trigger":"alert_start","method":"GET",
		"url":"http://192.168.1.50/on","enabled":true}`)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Webhooks[0].Phase != "" {
		t.Fatalf("phase = %q, want an absent phase left absent", loaded.Webhooks[0].Phase)
	}
	if got := config.WebhookPhase(loaded.Webhooks[0]); got != config.PhaseOnTrigger {
		t.Fatalf("WebhookPhase of a phase-less webhook = %q, want %q", got, config.PhaseOnTrigger)
	}
}

// The other shape the delay has, and the one the operator meets: a number
// typed into the form is refused rather than silently corrected, exactly as
// an impossible poll interval is.
func TestValidateWebhookRefusesADelayAboveTheCap(t *testing.T) {
	w := config.Webhook{
		ID: "w1", Trigger: config.TriggerAlertStart, Method: "GET",
		URL: "http://192.168.1.50/on", Phase: config.PhaseBefore,
		Delay: config.Duration(config.MaxWebhookDelay + time.Second),
	}
	err := config.ValidateWebhook(w)
	if err == nil {
		t.Fatal("a delay above the cap was accepted")
	}
	var werr *config.WebhookError
	if !errors.As(err, &werr) || werr.Field != config.WebhookFieldDelay {
		t.Fatalf("error = %v, want one naming the %q field", err, config.WebhookFieldDelay)
	}

	w.Delay = config.Duration(config.MaxWebhookDelay)
	if err := config.ValidateWebhook(w); err != nil {
		t.Fatalf("a delay exactly at the cap was refused: %v", err)
	}
}

func TestValidateWebhookRefusesAPhaseThisBuildDoesNotKnow(t *testing.T) {
	w := config.Webhook{
		ID: "w1", Trigger: config.TriggerAlertStart, Method: "GET",
		URL: "http://192.168.1.50/on", Phase: "aftr",
	}
	err := config.ValidateWebhook(w)
	if err == nil {
		t.Fatal("an unknown phase was accepted")
	}
	var werr *config.WebhookError
	if !errors.As(err, &werr) || werr.Field != config.WebhookFieldPhase {
		t.Fatalf("error = %v, want one naming the %q field", err, config.WebhookFieldPhase)
	}
}
