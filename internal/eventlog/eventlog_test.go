package eventlog_test

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/eventlog"
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

// Recent must hand back the same instant that was appended, regardless of
// what location it is later rendered in — the presentation layer, not the
// storage layer, decides which timezone a timestamp is shown in.
func TestRecentPreservesTheInstant(t *testing.T) {
	lg, _ := openLog(t, 100)
	kyiv, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	at := time.Date(2026, 9, 6, 15, 41, 0, 0, kyiv)

	if err := lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindAlert}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := lg.Recent(1)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if !got[0].At.Equal(at) {
		t.Fatalf("At = %v, want the same instant as %v", got[0].At, at)
	}
	if got[0].At.In(kyiv).Format("15:04") != "15:41" {
		t.Fatalf("rendered in Kyiv = %q, want 15:41", got[0].At.In(kyiv).Format("15:04"))
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
	if err := l.Clear(time.Now()); err != nil {
		t.Fatalf("Clear on a nil log: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close on a nil log: %v", err)
	}
}

// Clearing has to actually empty the table. A clear that left rows behind
// would be worse than no clear at all: the operator would believe the record
// was gone and it would not be.
func TestClearEmptiesTheLog(t *testing.T) {
	lg, _ := openLog(t, 100)
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		if err := lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindAlert, Sound: "siren.wav", OK: true}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if err := lg.Clear(at.Add(time.Hour)); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	entries, err := lg.Recent(100)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	for _, e := range entries {
		if e.Kind == eventlog.KindAlert {
			t.Fatalf("an alert entry survived the clear: %+v", e)
		}
	}
	n, err := lg.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("Count after a clear = %d, want 1 — only the entry recording the clear", n)
	}
}

// An empty log is ambiguous: it cannot distinguish a device that has never
// done anything from one whose record somebody erased. The entry the clear
// leaves behind is the whole difference, and it has to have the same shape as
// every other entry — same table, same columns, same round-trip through
// Recent — or the one row that explains an empty log is the one row nothing
// else can read.
func TestClearRecordsThatItHappened(t *testing.T) {
	lg, _ := openLog(t, 100)
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	if err := lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindAlert, OK: true}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	clearedAt := at.Add(90 * time.Minute)
	if err := lg.Clear(clearedAt); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	entries, err := lg.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Recent returned %d entries, want exactly the one recording the clear: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.Kind != eventlog.KindLogCleared {
		t.Errorf("kind = %q, want %q", got.Kind, eventlog.KindLogCleared)
	}
	if !got.OK {
		t.Error("the entry recording the clear is marked as a failure")
	}
	if !got.At.Equal(clearedAt) {
		t.Errorf("at = %s, want %s — the entry must say when the log was cleared", got.At, clearedAt)
	}
}

// The log is appended to from the engine and from the player's OnError while
// sounds are playing. A clear that raced one of those must lose the clear's
// completeness, never the alert: an entry written after the clear has begun
// has to survive it, and nothing may corrupt or panic either way.
func TestClearDoesNotLoseAppendsThatRaceIt(t *testing.T) {
	lg, _ := openLog(t, 1000)

	const goroutines = 4
	const perGoroutine = 40

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perGoroutine; i++ {
				if err := lg.Append(eventlog.Entry{At: time.Now(), Kind: eventlog.KindAlert, Sound: "siren.wav", OK: true}); err != nil {
					t.Errorf("Append during a clear: %v", err)
				}
			}
		}()
	}

	cleared := make(chan error, 1)
	go func() {
		<-start
		cleared <- lg.Clear(time.Now())
	}()
	close(start)

	wg.Wait()
	if err := <-cleared; err != nil {
		t.Fatalf("Clear during concurrent appends: %v", err)
	}

	// The log must still be readable, and must still hold the entry recording
	// the clear — the two ways this could have gone wrong that a bare "no
	// panic" assertion would sail straight past.
	entries, err := lg.Recent(1000)
	if err != nil {
		t.Fatalf("Recent after a raced clear: %v", err)
	}
	var sawCleared bool
	for _, e := range entries {
		if e.Kind == eventlog.KindLogCleared {
			sawCleared = true
		}
	}
	if !sawCleared {
		t.Error("the entry recording the clear is missing after a raced clear")
	}
	// Ids stay monotonic across a clear — Recent orders by id and means
	// "newest" by it, and Clear itself reasons about id ordering.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].ID <= entries[i].ID {
			t.Fatalf("Recent is not newest-first by id after a clear: %d then %d", entries[i-1].ID, entries[i].ID)
		}
	}
	if _, err := lg.Count(); err != nil {
		t.Fatalf("Count after a raced clear: %v", err)
	}
}
