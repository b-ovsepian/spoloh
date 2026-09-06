package configstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
)

func TestOpenSeedsADefaultWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	s, err := configstore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Get().Alerts.Region != config.Default().Alerts.Region {
		t.Fatal("a fresh store should hold the default configuration")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Open should have written the seeded config: %v", err)
	}
}

func TestOpenLoadsAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seed := config.Default()
	seed.Alerts.Region = "Полтавська область"
	if err := config.Save(path, seed); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s, err := configstore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := s.Get().Alerts.Region; got != "Полтавська область" {
		t.Fatalf("region = %q, want the value on disk", got)
	}
}

// A caller that mutates what Get returned must not affect the store — that is
// the whole point of handing out copies.
func TestGetReturnsAnIsolatedCopy(t *testing.T) {
	s := openTemp(t)

	got := s.Get()
	got.Alerts.Region = "Сумська область"
	got.Events[0].Enabled = false

	fresh := s.Get()
	if fresh.Alerts.Region == "Сумська область" || !fresh.Events[0].Enabled {
		t.Fatal("mutating a snapshot changed the store")
	}
}

func TestUpdatePersistsAndSwaps(t *testing.T) {
	s := openTemp(t)

	err := s.Update(func(c *config.Config) error {
		c.Alerts.Region = "Одеська область"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := s.Get().Alerts.Region; got != "Одеська область" {
		t.Fatalf("in-memory region = %q, want the updated value", got)
	}

	reloaded, err := config.Load(s.Path())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Alerts.Region != "Одеська область" {
		t.Fatal("Update did not persist to disk")
	}
}

// An update that produces an invalid config must change neither memory nor disk.
func TestUpdateRejectsAnInvalidResult(t *testing.T) {
	s := openTemp(t)
	before := s.Get().Alerts.Region

	err := s.Update(func(c *config.Config) error {
		c.Alerts.Region = ""
		return nil
	})
	if err == nil {
		t.Fatal("expected Update to reject a config that fails validation")
	}
	if s.Get().Alerts.Region != before {
		t.Fatal("a rejected update changed the in-memory config")
	}
	reloaded, err := config.Load(s.Path())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Alerts.Region != before {
		t.Fatal("a rejected update reached the disk")
	}
}

// An error from the mutator aborts the update untouched, so a handler can bail
// out half-way through editing without leaving a partial change behind.
func TestUpdatePropagatesMutatorError(t *testing.T) {
	s := openTemp(t)
	before := s.Get().Alerts.Region
	sentinel := os.ErrInvalid

	err := s.Update(func(c *config.Config) error {
		c.Alerts.Region = "Чернігівська область"
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err = %v, want the mutator's own error", err)
	}
	if s.Get().Alerts.Region != before {
		t.Fatal("an aborted update was applied anyway")
	}
}

func TestConcurrentGetAndUpdate(t *testing.T) {
	s := openTemp(t)
	regions := []string{"Вінницька область", "Волинська область", "Рівненська область"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Get().Alerts.Region
			}
		}()
	}
	for i, region := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = s.Update(func(c *config.Config) error {
					c.Alerts.Region = region
					return nil
				})
			}
		}(region)
		_ = i
	}
	wg.Wait()

	final := s.Get().Alerts.Region
	var ok bool
	for _, region := range regions {
		if final == region {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("final region %q is not one of the values written", final)
	}
}

func openTemp(t *testing.T) *configstore.Store {
	t.Helper()
	s, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// The hook is what makes eventlog.KindConfigChange reachable at all: nothing
// wrote that kind, and the very first configuration change a device sees —
// its administrator password being set — was invisible in the log. Putting it
// on the store rather than in each handler is what makes Plan 2b's editing
// screens inherit it.
func TestUpdateCallsTheChangeHookOnlyOnSuccess(t *testing.T) {
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var calls int
	store.SetOnChange(func() { calls++ })

	if err := store.Update(func(c *config.Config) error {
		c.Device.Name = "spoloh-2"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 1 {
		t.Fatalf("hook called %d times after a successful update, want 1", calls)
	}

	// A mutator that fails, and a change validation rejects, both leave the
	// configuration exactly as it was — so neither is a change to record.
	if err := store.Update(func(*config.Config) error { return errors.New("no") }); err == nil {
		t.Fatal("Update swallowed the mutator's error")
	}
	if err := store.Update(func(c *config.Config) error {
		c.Alerts.Region = ""
		return nil
	}); err == nil {
		t.Fatal("Update accepted a configuration with no region")
	}
	if calls != 1 {
		t.Fatalf("hook called %d times, want it silent for updates that changed nothing", calls)
	}
}

// The one unwrapped error path in the store, and it fires exactly when the SD
// card has been remounted read-only — the classic Pi failure — so the moment
// it matters most is the moment it said least.
func TestUpdateSaysWhichFileItCouldNotSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	store, err := configstore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// config.Save writes atomically through a temp file in the same
	// directory, so a directory it cannot write to is what a read-only card
	// looks like from here.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err = store.Update(func(c *config.Config) error {
		c.Device.Name = "spoloh-2"
		return nil
	})
	if err == nil {
		t.Fatal("Update succeeded against an unwritable directory")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file it could not save", err)
	}
}
