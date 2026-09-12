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
	// Escalated is a raid that was announced as yellow and is now red. It
	// is a rise, not a start and not an ending, and it is announced with a
	// start's urgency: see Machine.yellowRun and observeLevelWhileRaised.
	Escalated
)

func (t Transition) String() string {
	switch t {
	case Started:
		return "started"
	case Ended:
		return "ended"
	case Escalated:
		return "escalated"
	default:
		return "no_change"
	}
}

// EndConfirmations is how many consecutive calm readings an all-clear needs
// before Apply reports Ended. A start needs one, and always will.
//
// The asymmetry is the whole point, and it is not a tuning knob.
// alertsource.Client returns the first source that answers without an error
// and never compares sources, so which source a reading came from changes
// silently the moment the usual one is throttled — on one operator's board
// all three configured sources returned 429 in the same poll. A fallback that
// lags, or simply disagrees, then says "no alert" while a raid is running,
// and one such reading used to be enough to announce over the site's speakers
// that it was over.
//
// Three readings hold an all-clear for about thirty seconds at the default
// 15s interval and twenty at the 10s floor (config.MinPollInterval), which
// rides out a fallback one or two polls behind without this package having to
// know which source answered. Two would ride out a single blink and nothing
// more; four buys another poll's cover for another quarter-minute of waiting.
//
// The two ways of being wrong are not comparable, and that is what fixes the
// direction to err in: too high means people hear the all-clear half a minute
// late, too low means the device tells a building the raid is over while it
// is still on. Only one of those is something this project can survive
// saying.
//
// It also bounds the only other run of readings this machine keeps — the
// pending de-escalation in yellowRun — because that run exists to ride out
// the same thing: a source that lags, disagrees, or rearranges its list
// between two polls. One constant, one reason, and nothing that has to be
// kept in step with it by hand.
const EndConfirmations = 3

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

	// calmRun counts consecutive calm readings seen while the machine still
	// reports an alert: the pending half of an all-clear that has been seen
	// upstream and is deliberately not being announced yet. Nonzero is a
	// state an operator debugging a late all-clear has to be able to see,
	// which is what CalmRun is for. Any reading that says alert clears it,
	// and so does PollFailed.
	calmRun int

	// level is how severe the alert above is, as last positively reported.
	//
	// An unknown reading never overwrites a known level. A cascade that
	// falls from alerts.in.ua to one of the six Ubilling keys stops
	// reporting severity at all — every state those produce is
	// LevelUnknown — and letting that erase a red would make the first
	// reading after the recovery look like a level change the alert never
	// made. What changed was who answered. Holding the last positive
	// report also means an escalation that really did happen while the
	// cascade was on a level-blind source is announced the moment a source
	// that can see it answers again.
	//
	// Cleared when the alert ends: with no raid running there is no
	// severity to report, and the next raid's level is the next raid's.
	level alertsource.Level

	// yellowRun counts consecutive yellow readings seen while the machine
	// still reports something more severe — the pending half of a
	// de-escalation, which is confirmed on the same terms as an all-clear.
	//
	// A de-escalation is never announced, so on its own it could be taken
	// on the first reading. The run is not there to delay it; it is there
	// to stop a wobble becoming a siren. alertsource.propagateDownward
	// collapses a raion's red and a hromada's yellow into one state, so a
	// single poll in which upstream omits the raion's alert reads as
	// red → yellow → red, and the rise back is an escalation this machine
	// would announce. Riding out the blink is the difference between a
	// device that speaks when the threat grows and one that speaks
	// whenever upstream's list is rearranged — and people taught to ignore
	// the second stop hearing the first.
	//
	// Broken by any reading that is not yellow, and by PollFailed, for the
	// reason calmRun is.
	yellowRun int
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
		// The level is adopted, never announced — including one that
		// provably moved while the device was down, because no such proof
		// exists to be had. The evidence the missed-transition rule runs on
		// is upstream's own "changed", and that timestamp marks when the
		// alert began rather than when its level last moved: a raid raised
		// an hour ago and escalated during a thirty-second restart carries
		// the same timestamp on both sides of the restart. So the level is
		// taken silently, which is what §5.2 requires of anything this
		// device cannot prove it witnessed.
		m.yellowRun = 0
		if st.Alert {
			m.level = observeLevel(m.level, st.Level)
		} else {
			m.level = alertsource.LevelUnknown
		}

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
		// A reading that agrees with what we already report ends any run of
		// calm readings building towards an all-clear. The run has to be
		// consecutive, or it confirms nothing: one calm reading now and
		// another two raids later are not two readings of the same calm.
		m.calmRun = 0
		if !st.Alert {
			m.yellowRun = 0
			return NoChange
		}
		// The raid goes on, but its severity may not have. That is the one
		// thing left that a reading agreeing on the boolean can still say.
		return m.observeLevelWhileRaised(st.Level)
	}

	if st.Alert {
		// A siren is announced on the reading that asks for it, always. No
		// poll of latency, no second opinion: see EndConfirmations for why
		// only one of the two directions is allowed to wait.
		m.calmRun = 0
		m.yellowRun = 0
		m.alert = true
		m.since = since
		// This reading's level, not whatever the last raid left behind: a
		// new raid's severity is its own, and an unknown here is unknown —
		// which alertsource.Level has handled as red since it was written.
		m.level = st.Level
		return Started
	}

	// Calm, while this machine still reports a raid. Count it and say
	// nothing until the run is long enough to be believed.
	m.yellowRun = 0
	m.calmRun++
	if m.calmRun < EndConfirmations {
		return NoChange
	}
	m.calmRun = 0
	m.alert = false
	m.since = since
	m.level = alertsource.LevelUnknown
	return Ended
}

// observeLevelWhileRaised folds one reading's level into a raid this machine
// is already reporting, and says whether the threat grew.
func (m *Machine) observeLevelWhileRaised(seen alertsource.Level) Transition {
	if seen == alertsource.LevelUnknown || seen == m.level {
		// Nothing was said, or the same thing was said again. Either way
		// any pending de-escalation is discarded: that run has to be
		// unbroken to confirm anything, exactly as the all-clear's does.
		m.yellowRun = 0
		return NoChange
	}

	if seen.TreatAsRed() {
		m.yellowRun = 0
		rose := m.level == alertsource.LevelYellow
		m.level = seen
		if rose {
			// People heard a lesser warning and the threat has grown. A
			// device that says nothing now leaves them holding a picture
			// of the danger that is wrong, which is the silence this
			// project exists to refuse — so this is announced on the
			// reading that says so, with no confirmation, exactly as a
			// start is.
			return Escalated
		}
		// Unknown becoming red is not a rise. Unknown is handled as red
		// already, so the raid has been announced at red's weight all
		// along; the only thing that changed is that a source able to name
		// the level answered this time.
		return NoChange
	}

	// Yellow, while this machine reports something more severe. Counted and
	// never announced: see Machine.yellowRun for both halves of that.
	m.yellowRun++
	if m.yellowRun < EndConfirmations {
		return NoChange
	}
	m.yellowRun = 0
	m.level = alertsource.LevelYellow
	return NoChange
}

// observeLevel folds an observed level into a raid that is already raised: a
// level upstream named replaces what was there, and one it did not name
// leaves it alone. See Machine.level.
func observeLevel(have, seen alertsource.Level) alertsource.Level {
	if seen == alertsource.LevelUnknown {
		return have
	}
	return seen
}

// PollFailed tells the machine that a poll produced no usable reading — the
// whole source cascade failed, or the snapshot did not carry the configured
// oblast — and discards any run of calm readings building towards an
// all-clear.
//
// The run must be unbroken, not merely N readings that eventually happened. A
// device that saw one calm reading, went blind for ten minutes, then saw one
// more has not watched anything calm down: it has two readings with a hole
// between them, and announcing off that is the same bet as announcing off a
// single reading, which is the bet this whole mechanism exists to refuse.
// Being strict costs an all-clear that waits for polling to steady — late,
// not wrong.
//
// The pending de-escalation goes the same way, for the same reason: a run of
// yellow readings with a blind poll in the middle is two readings and a hole,
// not evidence that a raid has calmed down a step.
//
// Nothing else is touched. A failed poll is not evidence of anything, least
// of all that a raid ended, so what the machine reports does not move.
func (m *Machine) PollFailed() { m.calmRun, m.yellowRun = 0, 0 }

// CalmRun reports how many consecutive calm readings have been seen towards
// an all-clear that has not been announced yet; zero when nothing is being
// held back. The dashboard shows it so a late all-clear reads as one being
// confirmed rather than one that was lost.
func (m *Machine) CalmRun() int { return m.calmRun }

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
// something about a different one. So is any pending run of calm readings,
// for the same reason — those readings were about the old oblast.
func (m *Machine) Rebaseline() { *m = Machine{} }

// Region reports the oblast the machine's current state was observed for, or
// "" before it has seen anything.
func (m *Machine) Region() string { return m.region }

func (m *Machine) Alert() bool      { return m.alert }
func (m *Machine) Since() time.Time { return m.since }
func (m *Machine) Baselined() bool  { return m.baselined }

// Level reports how severe the raid the machine is reporting is, as last
// positively named upstream, and alertsource.LevelUnknown when nothing is
// running or when no source has ever named one. Unknown is handled as red —
// read alertsource.Level before branching on this.
func (m *Machine) Level() alertsource.Level { return m.level }

func (m *Machine) Persisted() Persisted {
	return Persisted{Region: m.region, Alert: m.alert, Since: m.since, Level: m.level}
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
// a first boot. The alternative is announcing over the site's public address
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
	m.level = p.Level
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

	// Level is how severe Alert was, so a board that loses power mid-raid
	// comes back reporting the severity it was already announcing rather
	// than waiting for a source that can name one to answer again.
	//
	// A file written before this field existed decodes with the empty
	// string, which is alertsource.LevelUnknown — "nobody said" — and
	// unknown is never a level change. Such a file therefore restores and
	// the first reading after it announces nothing about the level: the
	// zero value doing the right thing, not a case anyone has to remember
	// to write.
	Level alertsource.Level `json:"level,omitempty"`
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
