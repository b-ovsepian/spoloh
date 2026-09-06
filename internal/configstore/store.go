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

	// onChange is called after every successful Update. It exists so the
	// daemon can record a KindConfigChange entry in one place instead of
	// every editing screen remembering to: setting the administrator
	// password is the first configuration change any device sees, and it was
	// invisible in the log. Plan 2b's screens inherit this by going through
	// Update, which they already have to.
	//
	// Set it with SetOnChange before the store is shared with other
	// goroutines; it is not guarded, because a hook that can be swapped
	// mid-flight is a hook nobody can reason about.
	onChange func()

	mu  sync.RWMutex
	cfg *config.Config
}

// SetOnChange installs the hook called after each successful Update. Call it
// during startup, before the store is handed to anything that writes.
func (s *Store) SetOnChange(fn func()) { s.onChange = fn }

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
//
// fn must not retain the *config.Config it is given beyond the call: on
// success that same value becomes the store's live configuration, so a later
// write to it would reach live state outside the lock.
func (s *Store) Update(fn func(*config.Config) error) error {
	if err := s.update(fn); err != nil {
		return err
	}
	// Outside the lock: the hook writes to the event log, and anything that
	// reaches back into the store from in here would deadlock.
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}

func (s *Store) update(fn func(*config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cfg.Clone()
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("rejected configuration change: %w", err)
	}
	// Wrapped, unlike the bare return it used to be: this is the path that
	// fires when the SD card has been remounted read-only — the classic Pi
	// failure — and "permission denied" with no file name in front of it is
	// the least useful thing a log can say at that moment.
	if err := config.Save(s.path, next); err != nil {
		return fmt.Errorf("save %s: %w", s.path, err)
	}
	s.cfg = next
	return nil
}
