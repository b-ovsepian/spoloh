package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// storeConfigWithPollInterval writes a config.json whose alerts.poll_interval
// is exactly the given duration string. It edits the JSON rather than going
// through config.Save because Save validates, and the whole point of the
// cases below is a stored value Validate refuses — which is the only shape a
// config written by a build with the older 3s floor can have.
func storeConfigWithPollInterval(t *testing.T, path, interval string) {
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
	alerts, ok := generic["alerts"].(map[string]any)
	if !ok {
		t.Fatalf("alerts is not an object: %v", generic["alerts"])
	}
	alerts["poll_interval"] = interval
	edited, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// config.MinPollInterval was raised from 3s to 10s after every source in the
// cascade rate-limited a device polling every 5 seconds. Every config.json
// already in the field holds a value Validate now refuses, so rejecting it
// would stop the daemon starting on exactly the devices that were already
// announcing. Load raises it to the minimum and says so loudly instead — the
// same repair it makes for a missing alert event, for the same reason.
func TestLoadRaisesABelowMinimumPollIntervalToTheMinimum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeConfigWithPollInterval(t, path, "5s")

	var logs bytes.Buffer
	restoreLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(restoreLogger)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load refused a stored 5s poll interval instead of repairing it: %v", err)
	}
	if got := loaded.Alerts.PollInterval.Std(); got != config.MinPollInterval {
		t.Fatalf("poll interval = %v, want it raised to the %v minimum exactly", got, config.MinPollInterval)
	}
	if !strings.Contains(logs.String(), "poll_interval") {
		t.Errorf("Load did not log the repair; log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), "stored=5s") {
		t.Errorf("the log line does not name the value that was repaired; log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("the repair was not logged at Error; log = %q", logs.String())
	}
}

// A config.json written before alerts.poll_interval existed has no such key,
// so json.Unmarshal leaves it at Go's zero value — which is below the floor
// and would otherwise refuse to load rather than polling at all.
func TestLoadRepairsAnAbsentPollInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
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
	alerts, ok := generic["alerts"].(map[string]any)
	if !ok {
		t.Fatalf("alerts is not an object: %v", generic["alerts"])
	}
	delete(alerts, "poll_interval")
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
	if got := loaded.Alerts.PollInterval.Std(); got != config.MinPollInterval {
		t.Fatalf("poll interval = %v, want the %v minimum for a config that never carried one", got, config.MinPollInterval)
	}
}

// The repair only ever raises an interval that is actually below the floor.
// One stored above it is the operator's own choice and must come back exactly
// as saved, not reset to the minimum or to Default().
func TestLoadPreservesAPollIntervalAboveTheMinimum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Alerts.PollInterval = config.Duration(45 * time.Second)
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.Alerts.PollInterval.Std(); got != 45*time.Second {
		t.Fatalf("poll interval = %v, want the saved 45s to survive Load unchanged", got)
	}
}

// storeConfigWithRepeatWindow writes a config.json whose one scheduled event
// carries exactly the given repeat window. It edits the JSON rather than
// going through config.Save because Save validates, and every case below is
// a stored window Validate refuses — which is the only shape a hand-edited
// file, or a config restored from a build that allowed more than this one
// does, can have.
func storeConfigWithRepeatWindow(t *testing.T, path string, every any, until string) {
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
	events, ok := generic["events"].([]any)
	if !ok {
		t.Fatalf("events is not a list: %v", generic["events"])
	}
	edited := false
	for _, e := range events {
		ev, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("event is not an object: %T", e)
		}
		if ev["trigger"] != "scheduled" {
			continue
		}
		ev["every_minutes"] = every
		ev["until"] = until
		edited = true
	}
	if !edited {
		t.Fatal("no scheduled event in the default config to give a window to")
	}
	out, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// loadWithRepairLog loads path with slog captured, so a test can assert both
// on what was repaired and on the device having said so.
func loadWithRepairLog(t *testing.T, path string) (*config.Config, string) {
	t.Helper()
	var logs bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(restore)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load refused a repairable config instead of repairing it: %v", err)
	}
	return cfg, logs.String()
}

// scheduledOf returns the config's one scheduled event.
func scheduledOf(t *testing.T, c *config.Config) config.Event {
	t.Helper()
	for _, ev := range c.Events {
		if ev.Trigger == config.TriggerScheduled {
			return ev
		}
	}
	t.Fatal("the loaded config has no scheduled event")
	return config.Event{}
}

// A window that ends before it starts describes a series with no occurrences
// in it. Validate refuses it, so loading it unrepaired would stop the daemon
// starting — and that costs the operator not this event but every event on
// the device, alert_start included. Load drops the window and leaves the
// single firing at Time, which is the part of the event nobody can doubt was
// wanted.
func TestLoadDropsARepeatWindowThatEndsBeforeItStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeConfigWithRepeatWindow(t, path, 10, "02:00") // Default()'s scheduled event is at 09:00

	loaded, logs := loadWithRepairLog(t, path)

	ev := scheduledOf(t, loaded)
	if ev.EveryMinutes != 0 || ev.Until != "" {
		t.Fatalf("window = every %d min until %q, want it dropped", ev.EveryMinutes, ev.Until)
	}
	if ev.Time != "09:00" || !ev.Enabled {
		t.Errorf("the repair took the event itself away: %+v", ev)
	}
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("the repair was not logged at Error; log = %q", logs)
	}
	if !strings.Contains(logs, "02:00") {
		t.Errorf("the log line does not name the window that was dropped; log = %q", logs)
	}
}

// An interval with no end time is what a config written by a build that
// stored one field and not the other would leave behind. It is the same
// unusable series and gets the same answer.
func TestLoadDropsAnIntervalWithNoEndTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeConfigWithRepeatWindow(t, path, 10, "")

	loaded, _ := loadWithRepairLog(t, path)

	if ev := scheduledOf(t, loaded); ev.EveryMinutes != 0 {
		t.Fatalf("every_minutes = %d, want the unusable interval dropped", ev.EveryMinutes)
	}
}

// Validate refuses a negative interval outright, so only the repair stands
// between a hand-edited minus sign and a device that will not boot.
func TestLoadDropsANegativeInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	storeConfigWithRepeatWindow(t, path, -10, "10:00")

	loaded, _ := loadWithRepairLog(t, path)

	if ev := scheduledOf(t, loaded); ev.EveryMinutes != 0 || ev.Until != "" {
		t.Fatalf("window = every %d min until %q, want it dropped", ev.EveryMinutes, ev.Until)
	}
}

// The repair only ever clears a window it cannot use. One an operator
// actually configured must come back exactly as saved — including an
// interval longer than its own window, which is legal and fires once.
func TestLoadPreservesAUsableRepeatWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	for i := range cfg.Events {
		if cfg.Events[i].Trigger == config.TriggerScheduled {
			cfg.Events[i].EveryMinutes, cfg.Events[i].Until = 90, "10:00"
		}
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, logs := loadWithRepairLog(t, path)

	ev := scheduledOf(t, loaded)
	if ev.EveryMinutes != 90 || ev.Until != "10:00" {
		t.Fatalf("window = every %d min until %q, want the saved 90 and 10:00 untouched", ev.EveryMinutes, ev.Until)
	}
	if strings.Contains(logs, "repeat window") {
		t.Errorf("Load reported repairing a window it should not have touched; log = %q", logs)
	}
}
