// Package configstore owns the running configuration. It exists because the
// engine reads the configuration from its own goroutine on every tick while
// HTTP handlers write it, and a shared *config.Config would be a data race on
// a device that has to be trusted.
package configstore

import (
	"fmt"
	"os"
	"sync"

	"github.com/b-ovsepian/spoloh/internal/config"
)

type Store struct {
	path string

	mu  sync.RWMutex
	cfg *config.Config
}

// Open loads the configuration, seeding the file with the defaults when it does
// not exist yet. A file that exists but is invalid is an error: silently
// replacing an operator's settings would be worse than refusing to start.
func Open(path string) (*Store, error) {
	cfg, err := config.Load(path)
	switch {
	case os.IsNotExist(err):
		cfg = config.Default()
		if err := config.Save(path, cfg); err != nil {
			return nil, fmt.Errorf("seed %s: %w", path, err)
		}
	case err != nil:
		return nil, err
	}
	return &Store{path: path, cfg: cfg}, nil
}

// Path reports where the configuration is persisted.
func (s *Store) Path() string { return s.path }

// Get returns a private copy. Callers may mutate it freely; nothing they do
// reaches the store or any other reader.
func (s *Store) Get() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Update applies fn to a copy of the current configuration, validates the
// result, persists it atomically, and only then makes it visible to readers.
// Any failure — the mutator's own error, validation, or the write — leaves both
// memory and disk exactly as they were.
func (s *Store) Update(fn func(*config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("rejected configuration change: %w", err)
	}
	if err := config.Save(s.path, next); err != nil {
		return err
	}
	s.cfg = next
	return nil
}
