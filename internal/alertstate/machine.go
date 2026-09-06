// Package alertstate turns a stream of upstream snapshots into transitions.
package alertstate

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spoloh/spoloh/internal/alertsource"
	"github.com/spoloh/spoloh/internal/atomicfile"
)

type Transition int

const (
	NoChange Transition = iota
	Started
	Ended
)

func (t Transition) String() string {
	switch t {
	case Started:
		return "started"
	case Ended:
		return "ended"
	default:
		return "no_change"
	}
}

// Machine tracks one oblast. It is driven from the poll loop's single
// goroutine and is not safe for concurrent Apply calls.
type Machine struct {
	baselined bool
	alert     bool
	since     time.Time
}

func New() *Machine { return &Machine{} }

// Apply folds one observation into the machine and reports what changed.
//
// The first observation after start is always a silent baseline: if an alert
// was already running before a reboot it was announced then, and repeating the
// siren only confuses people.
func (m *Machine) Apply(st alertsource.RegionState, now time.Time) Transition {
	since := st.Changed
	if since.IsZero() {
		since = now
	}

	if !m.baselined {
		m.baselined = true
		m.alert = st.Alert
		m.since = since
		return NoChange
	}
	if st.Alert == m.alert {
		return NoChange
	}

	m.alert = st.Alert
	m.since = since
	if st.Alert {
		return Started
	}
	return Ended
}

func (m *Machine) Alert() bool          { return m.alert }
func (m *Machine) Since() time.Time     { return m.since }
func (m *Machine) Baselined() bool      { return m.baselined }
func (m *Machine) Persisted() Persisted { return Persisted{Alert: m.alert, Since: m.since} }

// Restore seeds the machine from disk so the dashboard can show an alert as
// ongoing straight after boot. It deliberately leaves the machine un-baselined.
func (m *Machine) Restore(p Persisted) {
	m.alert = p.Alert
	m.since = p.Since
}

// Persisted is the slice of state that survives a restart.
type Persisted struct {
	Alert bool      `json:"alert"`
	Since time.Time `json:"since"`
}

// LoadPersisted reads the state file. The returned Persisted is always safe to
// use even when the error is non-nil. Callers must log the error and carry on
// rather than treating it as fatal: the device has to boot.
//
// A missing file returns Persisted{}, nil: first boot is normal and unremarkable.
// A file that exists but cannot be read returns Persisted{}, err.
// A file that reads but fails to unmarshal returns Persisted{}, err.
func LoadPersisted(path string) (Persisted, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Persisted{}, nil
		}
		return Persisted{}, err
	}
	var p Persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return Persisted{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, nil
}

// SavePersisted writes the state file atomically. Callers must invoke it only
// on a transition, never on every poll: this file lives on an SD card.
func SavePersisted(path string, p Persisted) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, 0o600)
}
