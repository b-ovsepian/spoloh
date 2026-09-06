package alertstate_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/alertsource"
	"github.com/spoloh/spoloh/internal/alertstate"
)

func state(alert bool, changed time.Time) alertsource.RegionState {
	return alertsource.RegionState{Region: "м. Київ", Alert: alert, Changed: changed}
}

var now = time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)

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
	m.Restore(alertstate.Persisted{Alert: true, Since: now.Add(-time.Hour)})

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

func TestPersistedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := alertstate.Persisted{Alert: true, Since: now}

	if err := alertstate.SavePersisted(path, want); err != nil {
		t.Fatalf("SavePersisted: %v", err)
	}
	got, err := alertstate.LoadPersisted(path)
	if err != nil {
		t.Fatalf("LoadPersisted: %v", err)
	}
	if got.Alert != want.Alert || !got.Since.Equal(want.Since) {
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
