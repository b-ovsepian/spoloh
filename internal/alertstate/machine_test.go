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

// leveled is state() with a severity attached, for the cases where what
// upstream said about the level is the whole point.
func leveled(alert bool, l alertsource.Level, changed time.Time) alertsource.RegionState {
	st := state(alert, changed)
	st.Level = l
	return st
}

// raise drives a machine from a cold start to a raid at the given level and
// returns it, so the tests below start where they mean to.
func raise(t *testing.T, l alertsource.Level) *alertstate.Machine {
	t.Helper()
	m := alertstate.New()
	m.Apply(state(false, now), now)
	if got := m.Apply(leveled(true, l, now), now); got != alertstate.Started {
		t.Fatalf("the reading that raised the alert = %v, want Started", got)
	}
	if m.Level() != l {
		t.Fatalf("Level() = %v after a start at %v", m.Level(), l)
	}
	return m
}

// A raid is announced at whatever severity upstream declared it, on the
// reading that declares it — including the severity a source that has no
// level concept declines to name, which is unknown and is handled as red.
func TestAStartCarriesTheLevelUpstreamReported(t *testing.T) {
	for _, l := range []alertsource.Level{alertsource.LevelYellow, alertsource.LevelRed, alertsource.LevelUnknown} {
		t.Run(l.String(), func(t *testing.T) {
			m := raise(t, l)
			if m.Level() != l {
				t.Fatalf("Level() = %v, want %v", m.Level(), l)
			}
		})
	}
}

// The transition this task exists for. «Шосткинський район» was declared red
// while a device watching a hromada inside it had already announced a lesser
// warning; the people who heard that warning are holding a picture of the
// danger that is now wrong, and a device that stays quiet leaves them with
// it. Announced on the reading that says so, like a start and unlike an
// ending — see EndConfirmations for which direction is allowed to wait.
func TestYellowRisingToRedIsAnnouncedImmediately(t *testing.T) {
	m := raise(t, alertsource.LevelYellow)

	if got := m.Apply(leveled(true, alertsource.LevelRed, now), now); got != alertstate.Escalated {
		t.Fatalf("the first reading that says red = %v, want Escalated on that reading alone", got)
	}
	if m.Level() != alertsource.LevelRed {
		t.Fatalf("Level() = %v after the escalation, want red", m.Level())
	}
	if !m.Alert() {
		t.Fatal("an escalation is a raid growing, not one ending")
	}
	// Once, not once per poll: a siren every fifteen seconds reads as a
	// malfunction and gets the device unplugged.
	for i := 0; i < 20; i++ {
		if got := m.Apply(leveled(true, alertsource.LevelRed, now), now); got != alertstate.NoChange {
			t.Fatalf("poll %d after the escalation = %v, want NoChange", i, got)
		}
	}
}

// A cascade falling from alerts.in.ua to one of the six Ubilling keys turns a
// named level into no level at all, and back again when it recovers. Nothing
// about the raid changed — the reporting did — so nothing may be announced in
// either direction.
func TestUnknownIsNotALevelChange(t *testing.T) {
	for _, l := range []alertsource.Level{alertsource.LevelRed, alertsource.LevelYellow} {
		t.Run(l.String(), func(t *testing.T) {
			m := raise(t, l)

			// The level-blind source answers for a while.
			for i := 0; i < 10; i++ {
				if got := m.Apply(leveled(true, alertsource.LevelUnknown, now), now); got != alertstate.NoChange {
					t.Fatalf("reading %d from a source with no level concept = %v, want NoChange", i, got)
				}
				if m.Level() != l {
					t.Fatalf("Level() = %v after an unknown reading, want the last level actually reported (%v)", m.Level(), l)
				}
			}
			// And the one that can name it comes back.
			if got := m.Apply(leveled(true, l, now), now); got != alertstate.NoChange {
				t.Fatalf("the reading that recovered the level = %v, want NoChange: the alert did not change, the reporting did", got)
			}
		})
	}
}

// A raid that began under a level-blind source has been announced at red's
// weight all along, because unknown is handled as red. A source that can name
// the level then saying "red" has told us nothing new, and announcing an
// escalation for it would sound a second siren for a threat that never grew.
func TestUnknownBecomingRedIsNotAnEscalation(t *testing.T) {
	m := raise(t, alertsource.LevelUnknown)

	if got := m.Apply(leveled(true, alertsource.LevelRed, now), now); got != alertstate.NoChange {
		t.Fatalf("unknown becoming red = %v, want NoChange", got)
	}
	if m.Level() != alertsource.LevelRed {
		t.Fatalf("Level() = %v, want red once a source named it", m.Level())
	}
}

// A de-escalation is never announced, and it is not taken on one reading
// either. The reading that matters is the one after it: a blink of yellow
// that dropped the stored level would make the return to red an escalation,
// and the device would sound a siren because upstream rearranged its list.
func TestADeEscalationIsSilentAndConfirmed(t *testing.T) {
	for blinks := 1; blinks < alertstate.EndConfirmations; blinks++ {
		m := raise(t, alertsource.LevelRed)
		for i := 0; i < blinks; i++ {
			if got := m.Apply(leveled(true, alertsource.LevelYellow, now), now); got != alertstate.NoChange {
				t.Fatalf("yellow reading %d = %v, want NoChange: a de-escalation is never announced", i, got)
			}
			if m.Level() != alertsource.LevelRed {
				t.Fatalf("Level() = %v after %d unconfirmed yellow readings, want red", m.Level(), i+1)
			}
		}
		if got := m.Apply(leveled(true, alertsource.LevelRed, now), now); got != alertstate.NoChange {
			t.Fatalf("red returning after %d yellow readings = %v, want NoChange: the raid never stopped being red", blinks, got)
		}
	}

	// A full run is believed, silently, and the level the dashboard shows
	// follows upstream again.
	m := raise(t, alertsource.LevelRed)
	for i := 0; i < alertstate.EndConfirmations; i++ {
		if got := m.Apply(leveled(true, alertsource.LevelYellow, now), now); got != alertstate.NoChange {
			t.Fatalf("yellow reading %d of a confirmed de-escalation = %v, want NoChange", i, got)
		}
	}
	if m.Level() != alertsource.LevelYellow {
		t.Fatalf("Level() = %v after %d consecutive yellow readings, want yellow", m.Level(), alertstate.EndConfirmations)
	}

	// And a rise out of the confirmed yellow is announced at once. The
	// confirmation delays a fall; it must never delay a rise.
	if got := m.Apply(leveled(true, alertsource.LevelRed, now), now); got != alertstate.Escalated {
		t.Fatalf("the rise back to red = %v, want Escalated with no wait", got)
	}
}

// A blind poll is a hole in the run, not a yellow reading — the same rule
// PollFailed applies to a pending all-clear, for the same reason.
func TestAFailedPollDiscardsThePendingDeEscalation(t *testing.T) {
	m := raise(t, alertsource.LevelRed)
	for i := 1; i < alertstate.EndConfirmations; i++ {
		m.Apply(leveled(true, alertsource.LevelYellow, now), now)
	}
	m.PollFailed()

	if m.Level() != alertsource.LevelRed {
		t.Fatalf("Level() = %v after a failed poll, want the red it was still reporting", m.Level())
	}
	if got := m.Apply(leveled(true, alertsource.LevelYellow, now), now); got != alertstate.NoChange {
		t.Fatalf("the first yellow after the gap = %v, want NoChange", got)
	}
	if m.Level() != alertsource.LevelRed {
		t.Fatalf("Level() = %v: the run after the gap is a new one, not the old one resumed", m.Level())
	}
}

// The all-clear's confirmation is untouched by any of this, and an announced
// all-clear leaves no level behind: with nothing running there is nothing to
// be severe about.
func TestTheAllClearStillNeedsItsConfirmationsWithALevel(t *testing.T) {
	m := raise(t, alertsource.LevelRed)

	for i := 1; i < alertstate.EndConfirmations; i++ {
		if got := m.Apply(state(false, now), now); got != alertstate.NoChange {
			t.Fatalf("calm reading %d of %d = %v, want NoChange", i, alertstate.EndConfirmations, got)
		}
	}
	if got := m.Apply(state(false, now), now); got != alertstate.Ended {
		t.Fatalf("the confirming calm reading = %v, want Ended", got)
	}
	if m.Level() != alertsource.LevelUnknown {
		t.Fatalf("Level() = %v after the all-clear, want unknown: no raid, no severity", m.Level())
	}
}

// A state.json written before levels existed has no "level" key and decodes
// with the zero value, which is unknown. It must load, and the first reading
// after it must not read as a level change in either direction.
func TestAPreLevelsStateFileRestoresWithoutAnnouncing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Written out by hand in the old shape, because that is the file this
	// test is about: a Persisted marshalled by this build would carry the
	// new key and prove nothing.
	old := `{"region":"` + region + `","alert":true,"since":"` + now.Add(-time.Hour).Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p, err := alertstate.LoadPersisted(path)
	if err != nil {
		t.Fatalf("a state file written before levels existed must load: %v", err)
	}
	if !p.Alert {
		t.Fatal("the stored alert was lost")
	}
	if p.Level != alertsource.LevelUnknown {
		t.Fatalf("Level = %q, want unknown for a file that never had the field", p.Level)
	}

	m := alertstate.New()
	m.Restore(p, region)
	// Red, the level the old file could not name. Nothing may be announced:
	// the raid is the same one, and unknown is not a level change.
	if got := m.Apply(leveled(true, alertsource.LevelRed, now.Add(-time.Hour)), now); got != alertstate.NoChange {
		t.Fatalf("first Apply after restoring a pre-levels file = %v, want NoChange", got)
	}
	if m.Level() != alertsource.LevelRed {
		t.Fatalf("Level() = %v, want the level the first reading named", m.Level())
	}
}

// The level survives a restart, so a board that loses power mid-raid comes
// back reporting the severity it was already announcing.
func TestPersistedCarriesTheLevel(t *testing.T) {
	m := raise(t, alertsource.LevelYellow)

	path := filepath.Join(t.TempDir(), "state.json")
	if err := alertstate.SavePersisted(path, m.Persisted()); err != nil {
		t.Fatalf("SavePersisted: %v", err)
	}
	p, err := alertstate.LoadPersisted(path)
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if p.Level != alertsource.LevelYellow {
		t.Fatalf("Level = %q, want yellow", p.Level)
	}

	next := alertstate.New()
	next.Restore(p, region)
	if next.Level() != alertsource.LevelYellow {
		t.Fatalf("Level() = %v after Restore, want yellow", next.Level())
	}
	// And the escalation the next process sees is still an escalation.
	if got := next.Apply(leveled(true, alertsource.LevelYellow, now), now); got != alertstate.NoChange {
		t.Fatalf("the baselining reading = %v, want NoChange", got)
	}
	if got := next.Apply(leveled(true, alertsource.LevelRed, now), now); got != alertstate.Escalated {
		t.Fatalf("the escalation after a restart = %v, want Escalated", got)
	}
}

// A transition the device did not witness is still announced when upstream
// can prove it happened while we were down — levels changed nothing about
// that, and the level the first reading names rides along with it.
func TestAMissedStartAcrossARestartStillAnnouncesWithItsLevel(t *testing.T) {
	m := alertstate.New()
	m.Restore(alertstate.Persisted{Region: region, Alert: false, Since: now.Add(-time.Hour)}, region)

	got := m.Apply(leveled(true, alertsource.LevelYellow, now.Add(-30*time.Second)), now)
	if got != alertstate.Started {
		t.Fatalf("a raid that began during the restart = %v, want Started", got)
	}
	if m.Level() != alertsource.LevelYellow {
		t.Fatalf("Level() = %v, want the level upstream named", m.Level())
	}
}
