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

	if got := m.Apply(state(false, now), now); got != alertstate.Ended {
		t.Fatalf("Apply = %v, want Ended", got)
	}
	if m.Alert() {
		t.Fatal("alert should be clear")
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
