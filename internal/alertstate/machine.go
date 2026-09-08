// Package alertstate turns a stream of upstream snapshots into transitions.
package alertstate

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/atomicfile"
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

	// region is the oblast the alert below was actually observed for. It is
	// taken from the observation itself (RegionState carries its own key),
	// never from configuration, so it describes what happened rather than
	// what is currently configured. Persisted writes it out; Restore checks
	// it. Without it the stored state is a bare boolean with nothing saying
	// which oblast produced it, and a reader has no choice but to trust it.
	region string
	alert  bool
	since  time.Time

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
	// The provenance travels with the state: everything this machine believes
	// from here on was observed for this oblast, and Persisted carries that so
	// the next boot can check it instead of trusting it.
	m.region = st.Region

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

// Rebaseline discards everything the machine believes, so that the next Apply
// adopts what it sees silently — exactly as a cold start does.
//
// It exists for one caller: the engine, when the configured oblast has
// changed under it. The machine tracks a bare boolean and has no idea the
// boolean now comes from a different oblast, so without this the first
// reading of the new one is compared against the last reading of the old one
// and the difference is announced — a siren, or an all-clear, for a
// transition this device never witnessed anywhere. Spec §5.2 is exactly the
// rule that forbids that.
//
// The restored flag is cleared with the rest: the "a transition happened
// while we were down" evidence Restore left behind belongs to the oblast that
// was configured then, and it must not be used to justify announcing
// something about a different one.
func (m *Machine) Rebaseline() { *m = Machine{} }

// Region reports the oblast the machine's current state was observed for, or
// "" before it has seen anything.
func (m *Machine) Region() string { return m.region }

func (m *Machine) Alert() bool      { return m.alert }
func (m *Machine) Since() time.Time { return m.since }
func (m *Machine) Baselined() bool  { return m.baselined }
func (m *Machine) Persisted() Persisted {
	return Persisted{Region: m.region, Alert: m.alert, Since: m.since}
}

// Restore seeds the machine from disk so the dashboard can show an alert as
// ongoing straight after boot. It deliberately leaves the machine un-baselined,
// so the first poll cannot replay a siren for an alert that was already
// running — but it does record that the state came from disk, which is what
// lets the first Apply recognise a transition that happened while the device
// was down. See Apply.
//
// region is the oblast the caller is configured for. A stored state recorded
// for any other one is discarded outright, and the machine starts as cold as
// a first boot. The alternative is announcing over a school's public address
// system for something that happened in an oblast nobody in that building is
// in: /network can change the configured oblast between two runs, and the
// missed-transition rule below would then compare the new oblast's first
// reading against the old one's last and fire on the difference.
//
// A state file written before this field existed carries no region, which
// fails the comparison and takes the same path — one silent baseline and
// nothing announced, which is how an unrecognised state file has to degrade.
// That is also why this is a comparison and not a schema number like
// config.json's: state.json has never had one, the field it needs is
// load-bearing data rather than a version, and it already answers the
// question a version would have been used to ask.
func (m *Machine) Restore(p Persisted, region string) {
	m.region = region
	if p.Region != region {
		return
	}
	m.alert = p.Alert
	m.since = p.Since
	m.restored = true
}

// Persisted is the slice of state that survives a restart.
type Persisted struct {
	// Region is the oblast Alert was observed for. It is what makes this file
	// checkable at the point of use rather than something a reader has to
	// trust: see Restore. A file written before this field existed decodes
	// with an empty Region and is therefore ignored, which is the safe way
	// for it to be wrong.
	Region string    `json:"region"`
	Alert  bool      `json:"alert"`
	Since  time.Time `json:"since"`
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
