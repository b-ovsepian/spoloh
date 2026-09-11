package alertstate_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
)

func state(alert bool, changed time.Time) alertsource.RegionState {
	return alertsource.RegionState{Region: "м. Київ", Alert: alert, Changed: changed}
}

var now = time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)

// region is the oblast state() reports for, and so the one a restored state
// has to name to be usable.
const region = "м. Київ"

// The device must not re-announce an alert that was already announced before a
// reboot. The first snapshot after start is a silent baseline, always.
func TestFirstApplyDuringActiveAlertIsSilent(t *testing.T) {
	m := alertstate.New()
	changed := now.Add(-2 * time.Hour)

	if got := m.Apply(state(true, changed), now); got != alertstate.NoChange {
		t.Fatalf("first Apply = %v, want NoChange", got)
	}
	if !m.Alert() {
		t.Fatal("machine should record the alert as active")
	}
	if !m.Since().Equal(changed) {
		t.Fatalf("Since() = %v, want %v", m.Since(), changed)
	}
	if !m.Baselined() {
		t.Fatal("machine should be baselined after the first Apply")
	}
}

func TestEndAfterSilentBaseline(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(true, now.Add(-2*time.Hour)), now)

	// The all-clear waits to be confirmed; see TestAllClearNeedsConsecutive
	// Confirmations for the reason and for what each reading in between does.
	for i := 1; i < alertstate.EndConfirmations; i++ {
		m.Apply(state(false, now), now)
	}
	if got := m.Apply(state(false, now), now); got != alertstate.Ended {
		t.Fatalf("Apply = %v, want Ended", got)
	}
	if m.Alert() {
		t.Fatal("alert should be clear")
	}
}

// The defect this covers: alertsource.Client returns the first source that
// answers and never compares sources, so when the usual one is throttled —
// all three of one operator's sources returned 429 in a single poll — the
// answer comes from a different source without anything saying so. One
// "no alert" from a fallback that lags used to end a live raid: the device
// announced the all-clear over the site's speakers, then sounded the siren
// again when the first source came back still reporting it.
func TestAllClearNeedsConsecutiveConfirmations(t *testing.T) {
	// Everything below is written in terms of the constant, so the constant
	// itself is what has to be pinned: one reading is the defect.
	if alertstate.EndConfirmations < 2 {
		t.Fatalf("EndConfirmations = %d: an all-clear announced on a single reading is exactly what ends a live raid over the site's speakers",
			alertstate.EndConfirmations)
	}

	m := alertstate.New()
	m.Apply(state(false, now), now)
	if got := m.Apply(state(true, now), now); got != alertstate.Started {
		t.Fatalf("Apply = %v, want Started", got)
	}

	for i := 1; i < alertstate.EndConfirmations; i++ {
		if got := m.Apply(state(false, now), now); got != alertstate.NoChange {
			t.Fatalf("calm reading %d of %d = %v, want NoChange: an all-clear announced before it is confirmed tells a building a live raid is over",
				i, alertstate.EndConfirmations, got)
		}
		if !m.Alert() {
			t.Fatalf("after calm reading %d the machine no longer reports the raid; it must keep reporting it until the all-clear is confirmed", i)
		}
		if m.CalmRun() != i {
			t.Fatalf("CalmRun() = %d after %d calm readings, want %d", m.CalmRun(), i, i)
		}
	}

	if got := m.Apply(state(false, now), now); got != alertstate.Ended {
		t.Fatalf("the confirming reading = %v, want Ended", got)
	}
	if m.Alert() || m.CalmRun() != 0 {
		t.Fatalf("after the all-clear: alert %v, CalmRun %d; want a clear machine with nothing pending", m.Alert(), m.CalmRun())
	}
}

// A single calm reading between two that say alert is the fallback source
// blinking, not a raid ending. It must leave nothing behind — and the run it
// interrupted must start again from zero rather than resume, or two blinks
// spread over an hour would add up to an all-clear.
func TestAStrayCalmReadingAnnouncesNothing(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now), now)
	m.Apply(state(true, now), now)

	for i := 0; i < 20; i++ {
		if got := m.Apply(state(false, now), now); got != alertstate.NoChange {
			t.Fatalf("the stray calm reading in round %d = %v, want NoChange", i, got)
		}
		if got := m.Apply(state(true, now), now); got != alertstate.NoChange {
			t.Fatalf("the alert reading in round %d = %v, want NoChange", i, got)
		}
		if m.CalmRun() != 0 {
			t.Fatalf("round %d left CalmRun at %d; a reading that says alert must discard the run entirely", i, m.CalmRun())
		}
	}
	if !m.Alert() {
		t.Fatal("the raid was cancelled by readings that never agreed twice in a row")
	}
}

// The asymmetry, stated on its own: a siren is never held back, not for one
// poll and not for one second. Announced from a pending all-clear as well as
// from a settled calm state, because the pending window is exactly when a
// second opinion would look tempting.
func TestAlertStartIsNeverHeldBack(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now.Add(-time.Hour)), now)

	changed := now.Add(time.Minute)
	if got := m.Apply(state(true, changed), now); got != alertstate.Started {
		t.Fatalf("the first reading that says alert = %v, want Started on that reading alone", got)
	}
	if !m.Since().Equal(changed) {
		t.Fatalf("Since() = %v, want %v", m.Since(), changed)
	}
}

// A poll that produced no reading at all is a hole in the run, not a calm
// reading. Two readings with ten minutes of blindness between them are not
// evidence that anything calmed down. See Machine.PollFailed.
func TestAFailedPollDiscardsThePendingAllClear(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now), now)
	m.Apply(state(true, now), now)

	for i := 1; i < alertstate.EndConfirmations; i++ {
		m.Apply(state(false, now), now)
	}
	m.PollFailed()

	if m.CalmRun() != 0 {
		t.Fatalf("CalmRun() = %d after a failed poll, want 0", m.CalmRun())
	}
	if !m.Alert() {
		t.Fatal("a failed poll changed what the machine reports; it is not evidence of anything")
	}
	if got := m.Apply(state(false, now), now); got != alertstate.NoChange {
		t.Fatalf("the first calm reading after the gap = %v, want NoChange: it is the start of a new run, not the end of the old one", got)
	}

	// A full fresh run still announces, so the reset delays the all-clear
	// rather than losing it.
	var last alertstate.Transition
	for i := 1; i < alertstate.EndConfirmations; i++ {
		last = m.Apply(state(false, now), now)
	}
	if last != alertstate.Ended {
		t.Fatalf("the completed run after the gap = %v, want Ended", last)
	}
}

// Once announced, an all-clear is announced once: the readings that keep
// arriving afterwards must not restart the count and fire again.
func TestConfirmedAllClearAnnouncesExactlyOnce(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now), now)
	m.Apply(state(true, now), now)

	fires := 0
	for i := 0; i < 50; i++ {
		if m.Apply(state(false, now), now) == alertstate.Ended {
			fires++
		}
	}
	if fires != 1 {
		t.Fatalf("a raid that ended announced %d all-clears, want exactly 1", fires)
	}
}

func TestStartAfterQuietBaseline(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now.Add(-time.Hour)), now)

	changed := now.Add(time.Minute)
	if got := m.Apply(state(true, changed), now); got != alertstate.Started {
		t.Fatalf("Apply = %v, want Started", got)
	}
	if !m.Since().Equal(changed) {
		t.Fatalf("Since() = %v, want %v", m.Since(), changed)
	}
}

func TestRepeatedPollsDoNotRetrigger(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now), now)
	if got := m.Apply(state(true, now), now); got != alertstate.Started {
		t.Fatalf("Apply = %v, want Started", got)
	}
	for i := 0; i < 20; i++ {
		if got := m.Apply(state(true, now), now); got != alertstate.NoChange {
			t.Fatalf("poll %d = %v, want NoChange", i, got)
		}
	}
}

// A long alert is announced once. Repeating the siren every few minutes reads
// as a malfunction and gets the device unplugged.
func TestLongAlertAnnouncedOnce(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now), now)
	m.Apply(state(true, now), now)

	fires := 0
	for i := 1; i <= 3600; i++ { // five hours of five-second polls
		if m.Apply(state(true, now), now.Add(time.Duration(i)*5*time.Second)) != alertstate.NoChange {
			fires++
		}
	}
	if fires != 0 {
		t.Fatalf("a sustained alert fired %d extra times, want 0", fires)
	}
}

// Upstream reports the epoch sentinel as a zero Changed. Since() must then fall
// back to the observation time rather than showing 1970.
func TestZeroChangedFallsBackToNow(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(false, now), now)

	observed := now.Add(30 * time.Second)
	m.Apply(state(true, time.Time{}), observed)
	if !m.Since().Equal(observed) {
		t.Fatalf("Since() = %v, want the observation time %v", m.Since(), observed)
	}
}

// Restore feeds the dashboard after a reboot but must not act as a baseline,
// otherwise an alert that began while the device was off would fire on boot.
func TestRestoreDoesNotBaseline(t *testing.T) {
	m := alertstate.New()
	m.Restore(alertstate.Persisted{Region: region, Alert: true, Since: now.Add(-time.Hour)}, region)

	if !m.Alert() {
		t.Fatal("restored state should show the alert")
	}
	if m.Baselined() {
		t.Fatal("restored state must not count as a baseline")
	}
	if got := m.Apply(state(true, now.Add(-time.Hour)), now); got != alertstate.NoChange {
		t.Fatalf("first Apply after Restore = %v, want NoChange", got)
	}
}

// The five to ten seconds of a systemd restart are a window in which upstream
// can flip without anyone hearing it. Every case here restores a state
// recorded for the same oblast that is configured, so this table is also what
// stops Restore's region check from being written as "ignore everything": the
// first two cases only pass if a matching state is still used as evidence. Restore's silent baseline is right for an
// alert that was already running — it was announced before the reboot — and
// wrong for one that started during the window. Upstream's own "changed"
// timestamp is what tells the two apart, and only a timestamp strictly newer
// than what the previous process knew counts as proof.
func TestRestoreWindowTransitions(t *testing.T) {
	lastKnown := now.Add(-time.Hour)

	tests := []struct {
		name          string
		restoredAlert bool
		restoredSince time.Time
		observed      bool
		changed       time.Time
		want          alertstate.Transition
	}{
		{
			name:          "alert began while the daemon was down",
			restoredAlert: false, restoredSince: lastKnown,
			observed: true, changed: now.Add(-30 * time.Second),
			want: alertstate.Started,
		},
		{
			name:          "alert ended while the daemon was down",
			restoredAlert: true, restoredSince: lastKnown,
			observed: false, changed: now.Add(-30 * time.Second),
			want: alertstate.Ended,
		},
		{
			name:          "the same alert is still running: no replay",
			restoredAlert: true, restoredSince: lastKnown,
			observed: true, changed: now.Add(-30 * time.Second),
			want: alertstate.NoChange,
		},
		{
			name:          "quiet before, quiet now",
			restoredAlert: false, restoredSince: lastKnown,
			observed: false, changed: now.Add(-30 * time.Second),
			want: alertstate.NoChange,
		},
		{
			name:          "a zero changed proves nothing, so stay silent (start)",
			restoredAlert: false, restoredSince: lastKnown,
			observed: true, changed: time.Time{},
			want: alertstate.NoChange,
		},
		{
			name:          "a zero changed proves nothing, so stay silent (end)",
			restoredAlert: true, restoredSince: lastKnown,
			observed: false, changed: time.Time{},
			want: alertstate.NoChange,
		},
		{
			name:          "a flip older than what we knew is not ours to announce",
			restoredAlert: true, restoredSince: lastKnown,
			observed: false, changed: lastKnown.Add(-time.Minute),
			want: alertstate.NoChange,
		},
		{
			name:          "no restored timestamp is no knowledge at all",
			restoredAlert: false, restoredSince: time.Time{},
			observed: true, changed: now.Add(-30 * time.Second),
			want: alertstate.NoChange,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := alertstate.New()
			m.Restore(alertstate.Persisted{Region: region, Alert: tc.restoredAlert, Since: tc.restoredSince}, region)

			if got := m.Apply(state(tc.observed, tc.changed), now); got != tc.want {
				t.Fatalf("first Apply after Restore = %v, want %v", got, tc.want)
			}
			if m.Alert() != tc.observed {
				t.Fatalf("Alert() = %v, want the observed %v", m.Alert(), tc.observed)
			}
			// Whatever the first Apply decided, the machine is baselined
			// afterwards and a second identical poll must be silent.
			if got := m.Apply(state(tc.observed, tc.changed), now); got != alertstate.NoChange {
				t.Fatalf("second Apply = %v, want NoChange", got)
			}
		})
	}
}

// /network can change the configured oblast between two runs of the daemon,
// and a power cut on these boards is routine. A state file recorded for the
// old oblast must not be used as evidence about the new one: comparing them
// is what makes the missed-transition rule announce a siren, over the site's
// public address system, for an alert that happened somewhere nobody in that
// building is.
func TestRestoreIgnoresStateFromAnotherOblast(t *testing.T) {
	m := alertstate.New()
	// Kyiv was under alert when this device was last configured for it, and
	// the timestamp is old enough to "prove" a flip if it were ever compared.
	m.Restore(alertstate.Persisted{Region: "м. Київ", Alert: true, Since: now.Add(-time.Hour)}, "Львівська область")

	if m.Alert() {
		t.Error("a state recorded for another oblast was adopted as this one's")
	}
	if m.Baselined() {
		t.Error("restore must never baseline the machine")
	}

	// Lviv is calm, and upstream says it flipped after the stored timestamp:
	// the exact shape that announces an all-clear if the stored state is
	// trusted.
	lviv := alertsource.RegionState{Region: "Львівська область", Alert: false, Changed: now.Add(-30 * time.Second)}
	if got := m.Apply(lviv, now); got != alertstate.NoChange {
		t.Fatalf("first Apply after a cross-oblast restore = %v, want NoChange", got)
	}
	if m.Region() != "Львівська область" {
		t.Errorf("Region() = %q, want the oblast actually observed", m.Region())
	}

	// Adopted, not ignored: the new oblast's own next transition announces.
	lviv.Alert = true
	if got := m.Apply(lviv, now); got != alertstate.Started {
		t.Fatalf("second Apply = %v, want Started", got)
	}
}

// A state file written before Persisted carried a region decodes with an empty
// one. It cannot be attributed to any oblast, so it is discarded the same way
// — one silent baseline, nothing announced.
func TestRestoreIgnoresStateWithNoOblast(t *testing.T) {
	m := alertstate.New()
	m.Restore(alertstate.Persisted{Alert: true, Since: now.Add(-time.Hour)}, region)

	if m.Alert() {
		t.Error("an unattributed state was adopted")
	}
	if got := m.Apply(state(false, now.Add(-30*time.Second)), now); got != alertstate.NoChange {
		t.Fatalf("first Apply after restoring an unattributed state = %v, want NoChange", got)
	}
}

// What the machine writes back names the oblast it was observed for, or the
// check above has nothing to compare against on the next boot.
func TestPersistedCarriesTheObservedOblast(t *testing.T) {
	m := alertstate.New()
	m.Apply(alertsource.RegionState{Region: "Одеська область", Alert: true, Changed: now}, now)

	if got := m.Persisted().Region; got != "Одеська область" {
		t.Fatalf("Persisted().Region = %q, want the oblast the reading came from", got)
	}
}

// Rebaseline is the runtime half of the same story: after it, the machine
// belongs to whatever oblast it next observes.
func TestRebaselineForgetsTheOblast(t *testing.T) {
	m := alertstate.New()
	m.Apply(state(true, now.Add(-time.Hour)), now)
	m.Rebaseline()

	if m.Region() != "" || m.Alert() || m.Baselined() {
		t.Fatalf("after Rebaseline: region %q, alert %v, baselined %v; want a machine as cold as a first boot",
			m.Region(), m.Alert(), m.Baselined())
	}
}

// A machine that was never restored — the true first boot, with no state file —
// keeps the unconditional silent baseline: there is nothing to compare against.
func TestFirstBootWithoutRestoreIsAlwaysSilent(t *testing.T) {
	m := alertstate.New()
	if got := m.Apply(state(true, now.Add(-30*time.Second)), now); got != alertstate.NoChange {
		t.Fatalf("first Apply with no Restore = %v, want NoChange", got)
	}
}

func TestPersistedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := alertstate.Persisted{Region: region, Alert: true, Since: now}

	if err := alertstate.SavePersisted(path, want); err != nil {
		t.Fatalf("SavePersisted: %v", err)
	}
	got, err := alertstate.LoadPersisted(path)
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if got.Region != want.Region || got.Alert != want.Alert || !got.Since.Equal(want.Since) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadPersistedMissingFileIsEmpty(t *testing.T) {
	got, err := alertstate.LoadPersisted(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing state file is normal on first boot: %v", err)
	}
	if got.Alert {
		t.Fatal("missing state should read as quiet")
	}
}

func TestLoadPersistedCorruptFileReportsErrorAndEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{{{"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := alertstate.LoadPersisted(path)
	if err == nil {
		t.Fatal("corrupt state file must report an error so it can be logged")
	}
	if got.Alert {
		t.Fatal("returned state must be the usable zero value (quiet)")
	}
}

func TestLoadPersistedUnreadableFileReportsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cannot test permission bits as root")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	got, err := alertstate.LoadPersisted(path)
	if err == nil {
		t.Fatal("unreadable state file must report an error so it can be logged")
	}
	if got.Alert {
		t.Fatal("returned state must be the usable zero value (quiet)")
	}
}
