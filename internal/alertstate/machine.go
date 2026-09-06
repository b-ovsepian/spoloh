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

	// restored is true between Restore and the first Apply, so that the first
	// observation can be compared against what the previous process knew.
	restored bool
}

func New() *Machine { return &Machine{} }

// Apply folds one observation into the machine and reports what changed.
//
// The first observation after start is normally a silent baseline: if an alert
// was already running before a reboot it was announced then, and repeating the
// siren only confuses people (spec §5.2).
//
// The exception is a transition that provably happened while the device was
// down — the five to ten seconds of a systemd restart, or however long a power
// cut lasted. Restore hands us what the previous process knew and when it knew
// it; upstream hands us when the state last flipped. When the observed state
// disagrees with the restored one AND upstream says the flip came after our
// last knowledge, that is a real transition nobody heard, and it is announced.
// Anything less than proof — no restored timestamp, or a zero Changed, which
// means upstream has never observed a change or sent something unparseable —
// falls back to the silent baseline. The device never announces on a guess.
func (m *Machine) Apply(st alertsource.RegionState, now time.Time) Transition {
	since := st.Changed
	if since.IsZero() {
		since = now
	}

	if !m.baselined {
		restored, restoredAlert, restoredSince := m.restored, m.alert, m.since
		m.baselined = true
		m.restored = false
		m.alert = st.Alert
		m.since = since

		missed := restored &&
			st.Alert != restoredAlert &&
			!restoredSince.IsZero() &&
			!st.Changed.IsZero() &&
			st.Changed.After(restoredSince)
		if missed {
			if st.Alert {
				return Started
			}
			return Ended
		}
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
// ongoing straight after boot. It deliberately leaves the machine un-baselined,
// so the first poll cannot replay a siren for an alert that was already
// running — but it does record that the state came from disk, which is what
// lets the first Apply recognise a transition that happened while the device
// was down. See Apply.
func (m *Machine) Restore(p Persisted) {
	m.alert = p.Alert
	m.since = p.Since
	m.restored = true
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
