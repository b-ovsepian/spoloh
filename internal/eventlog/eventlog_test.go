package eventlog_test

import (
	"path/filepath"
	"sync"
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

// A device on unreliable mains power can restart more often than every
// pruneEvery inserts, so the in-process sweep counter alone can never fire.
// Open must re-establish the cap on every process start regardless of what
// happened before the last restart.
func TestReopenPrunesToCapacity(t *testing.T) {
	const capacity = 5
	lg, path := openLog(t, capacity)
	// Fewer than pruneEvery (32) inserts, so no in-process sweep fires.
	for i := 0; i < 10; i++ {
		if err := lg.Append(eventlog.Entry{At: time.Now(), Kind: eventlog.KindScheduled}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := eventlog.Open(path, capacity)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	n, err := again.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n > capacity {
		t.Fatalf("count after reopen = %d, want pruned to capacity %d", n, capacity)
	}
}

// The engine and the player call Append from different goroutines (engine
// state transitions vs. the player's OnError callback), so Append must be
// safe for concurrent use. Run with -race to catch the unsynchronized
// counter this test is guarding against.
func TestAppendIsSafeForConcurrentUse(t *testing.T) {
	lg, _ := openLog(t, 50)

	const goroutines = 8
	const perGoroutine = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := lg.Append(eventlog.Entry{At: time.Now(), Kind: eventlog.KindScheduled}); err != nil {
					t.Errorf("Append: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if _, err := lg.Count(); err != nil {
		t.Fatalf("Count: %v", err)
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

// A nil *Log stands in for an event log that could not be opened — a corrupt
// database or a read-only SD card. Every method must be a no-op, because the
// daemon carries on without one rather than refusing to announce alerts.
func TestNilLogIsSafe(t *testing.T) {
	var l *eventlog.Log

	if err := l.Append(eventlog.Entry{Kind: eventlog.KindAlert}); err != nil {
		t.Fatalf("Append on a nil log: %v", err)
	}
	entries, err := l.Recent(10)
	if err != nil || entries != nil {
		t.Fatalf("Recent on a nil log = %v, %v", entries, err)
	}
	n, err := l.Count()
	if err != nil || n != 0 {
		t.Fatalf("Count on a nil log = %d, %v", n, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close on a nil log: %v", err)
	}
}
