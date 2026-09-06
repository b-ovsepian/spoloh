package configstore_test

import (
	"os"
	"path/filepath"
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
