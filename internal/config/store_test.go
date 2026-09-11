package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := config.Default()
	want.Alerts.Region = "Львівська область"

	if err := config.Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Alerts.Region != want.Alerts.Region {
		t.Fatalf("region = %q, want %q", got.Alerts.Region, want.Alerts.Region)
	}
	if len(got.Events) != len(want.Events) {
		t.Fatalf("events = %d, want %d", len(got.Events), len(want.Events))
	}
	if got.Alerts.PollInterval != want.Alerts.PollInterval {
		t.Fatalf("poll interval = %v, want %v", got.Alerts.PollInterval, want.Alerts.PollInterval)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("directory should hold only config.json, got %v", entries)
	}
}

func TestSaveRejectsInvalidAndKeepsPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	good := config.Default()
	good.Alerts.Region = "Полтавська область"
	if err := config.Save(path, good); err != nil {
		t.Fatalf("Save: %v", err)
	}

	bad := config.Default()
	bad.Alerts.Region = ""
	if err := config.Save(path, bad); err == nil {
		t.Fatal("expected Save to reject an invalid config")
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Alerts.Region != "Полтавська область" {
		t.Fatalf("previous config was clobbered: region = %q", got.Alerts.Region)
	}
}

func TestLoadRejectsTruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"device":{`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected Load to reject a truncated file")
	}
}

func TestLoadRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := config.Default()
	if err := config.Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	bumped := []byte(strings.Replace(string(raw), `"schema": 1`, `"schema": 99`, 1))
	if err := os.WriteFile(path, bumped, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected Load to reject a newer schema version")
	}
}

func TestLoadMissingFileReportsNotExist(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "absent.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("want os.IsNotExist, got %v", err)
	}
}

// A device with no alert_start event has nothing to announce an air raid
// with and no way on the dashboard to add one back (the events screen only
// creates TriggerScheduled rows) — so Load restores it from Default rather
// than merely rejecting or silently accepting the gap. This is the guarantee
// that must hold for a hand-edited config.json, a config migrated from
// before this rule existed, or any other bytes on disk, not only the ones
// this daemon itself ever wrote.
func TestLoadRestoresAMissingAlertStartEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := config.Default()
	var withoutAlertStart []config.Event
	for _, ev := range cfg.Events {
		if ev.Trigger != config.TriggerAlertStart {
			withoutAlertStart = append(withoutAlertStart, ev)
		}
	}
	cfg.Events = withoutAlertStart
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var logs bytes.Buffer
	restoreLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(restoreLogger)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var restored *config.Event
	for i := range loaded.Events {
		if loaded.Events[i].Trigger == config.TriggerAlertStart {
			restored = &loaded.Events[i]
		}
	}
	if restored == nil {
		t.Fatal("Load did not restore the missing alert-start event")
	}
	want := config.Default().Events[0] // alert-start, per config.Default's own ordering
	if want.Trigger != config.TriggerAlertStart {
		t.Fatalf("test assumption broken: config.Default().Events[0] is trigger %q, not alert_start", want.Trigger)
	}
	if restored.ID != want.ID || restored.Sound != want.Sound || restored.Volume != want.Volume || !restored.Enabled {
		t.Fatalf("restored event = %+v, want the Default() alert-start event %+v", *restored, want)
	}

	if !strings.Contains(logs.String(), "restored a missing alert event") {
		t.Errorf("Load did not log the restoration; log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), config.TriggerAlertStart) {
		t.Errorf("the log line does not name which trigger was restored; log = %q", logs.String())
	}
	if lvl := "level=ERROR"; !strings.Contains(logs.String(), lvl) {
		t.Errorf("the restoration was not logged at Error; log = %q", logs.String())
	}
}

// Restoring is only for an event that is absent — one that is present, even
// disabled, must come back exactly as loaded. enabled: false is the
// supported way to stop alert announcements, and Load must not paper over
// it as if it were the same problem as a missing event.
func TestLoadDoesNotTouchAPresentAlertStartEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := config.Default()
	for i := range cfg.Events {
		if cfg.Events[i].Trigger == config.TriggerAlertStart {
			cfg.Events[i].Enabled = false
			cfg.Events[i].Sound = "custom-siren.wav"
			cfg.Events[i].Volume = 37
		}
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, ev := range loaded.Events {
		if ev.Trigger != config.TriggerAlertStart {
			continue
		}
		if ev.Enabled || ev.Sound != "custom-siren.wav" || ev.Volume != 37 {
			t.Fatalf("a present alert-start event was altered by Load: %+v", ev)
		}
		return
	}
	t.Fatal("alert-start event disappeared entirely")
}

// A config.json written before Event.Repeat existed has no "repeat" key at
// all, so json.Unmarshal leaves every event at Go's zero value for it. Load
// must repair that to 1, the same way it repairs a missing alert event,
// because 0 would mean the device stops announcing that event entirely — on
// an upgrade an operator never asked for and has no way to notice happened.
func TestLoadDefaultsAMissingRepeatToOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Strip "repeat" from every event, simulating bytes written by a build
	// that predates the field — config.Save itself always writes it, so this
	// is the one way to produce that shape from inside the test.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	events, ok := generic["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("events is not a non-empty list: %v", generic["events"])
	}
	for _, e := range events {
		ev, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("event is not an object: %T", e)
		}
		delete(ev, "repeat")
	}
	stripped, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, stripped, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Events) == 0 {
		t.Fatal("test assumption broken: Load returned no events")
	}
	for _, ev := range loaded.Events {
		if ev.Repeat != 1 {
			t.Errorf("event %q: repeat = %d after loading a pre-repeat config, want 1", ev.ID, ev.Repeat)
		}
	}
}

// Load's repair only ever raises a 0; an event that was actually saved with
// a repeat above 1 must come back exactly as stored, not reset.
func TestLoadPreservesAnExplicitRepeatAboveOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Events[0].Repeat = 5
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Events[0].Repeat != 5 {
		t.Fatalf("repeat = %d, want the saved value of 5 to survive Load unchanged", loaded.Events[0].Repeat)
	}
}
