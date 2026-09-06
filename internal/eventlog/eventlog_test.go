package eventlog_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/eventlog"
)

func openLog(t *testing.T, capacity int) (*eventlog.Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spoloh.db")
	lg, err := eventlog.Open(path, capacity)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { lg.Close() })
	return lg, path
}

func TestAppendAndRecent(t *testing.T) {
	lg, _ := openLog(t, 100)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		err := lg.Append(eventlog.Entry{
			At:      at.Add(time.Duration(i) * time.Minute),
			Kind:    eventlog.KindAlert,
			EventID: "alert-start",
			Sound:   "siren.wav",
			OK:      true,
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := lg.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	// Newest first, so the dashboard can render without reversing.
	if !got[0].At.Equal(at.Add(2 * time.Minute)) {
		t.Fatalf("first entry = %v, want the newest", got[0].At)
	}
	if got[0].Kind != eventlog.KindAlert || got[0].Sound != "siren.wav" || !got[0].OK {
		t.Fatalf("entry not round-tripped: %+v", got[0])
	}
}

func TestRecentHonoursLimit(t *testing.T) {
	lg, _ := openLog(t, 100)
	for i := 0; i < 10; i++ {
		if err := lg.Append(eventlog.Entry{At: time.Now(), Kind: eventlog.KindScheduled}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := lg.Recent(4)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("entries = %d, want 4", len(got))
	}
}

// The log lives on an SD card. Unbounded growth would eventually fill it and
// wear it out, so old rows must be dropped.
func TestCapacityIsEnforced(t *testing.T) {
	lg, _ := openLog(t, 20)
	for i := 0; i < 500; i++ {
		if err := lg.Append(eventlog.Entry{At: time.Now(), Kind: eventlog.KindScheduled}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	n, err := lg.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n > 40 {
		t.Fatalf("count = %d, want the log pruned near its capacity of 20", n)
	}
}

func TestErrorEntriesRoundTrip(t *testing.T) {
	lg, _ := openLog(t, 100)
	want := "source default: http 500"
	if err := lg.Append(eventlog.Entry{
		At: time.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: want,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := lg.Recent(1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if got[0].OK || got[0].Err != want {
		t.Fatalf("entry = %+v, want a failure carrying %q", got[0], want)
	}
}

func TestReopenPersists(t *testing.T) {
	lg, path := openLog(t, 100)
	if err := lg.Append(eventlog.Entry{At: time.Now(), Kind: eventlog.KindManual, Sound: "siren.wav"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := eventlog.Open(path, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	got, err := again.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].Sound != "siren.wav" {
		t.Fatalf("entries = %+v, want the persisted manual play", got)
	}
}
