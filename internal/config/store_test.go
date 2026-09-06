package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spoloh/spoloh/internal/config"
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
