package eventlog

import (
	"path/filepath"
	"testing"
	"time"
)

// White-box: this test needs the unexported pruneEvery constant to state the
// real invariant, so it lives in package eventlog rather than eventlog_test,
// mirroring internal/player/alsa_test.go.

// The log lives on an SD card. Unbounded growth would eventually fill it and
// wear it out, so old rows must be dropped. Rows accumulate between sweeps
// (the sweep only runs every pruneEvery inserts), so the table can
// transiently hold up to one sweep interval more than capacity, but never
// more than that.
func TestCapacityIsEnforced(t *testing.T) {
	const capacity = 20
	path := filepath.Join(t.TempDir(), "spoloh.db")
	lg, err := Open(path, capacity)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { lg.Close() })

	for i := 0; i < 500; i++ {
		if err := lg.Append(Entry{At: time.Now(), Kind: KindScheduled}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	n, err := lg.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}

	maxRows := capacity + pruneEvery - 1
	if n > maxRows {
		t.Fatalf("count = %d, want at most %d (capacity %d plus one sweep interval)", n, maxRows, capacity)
	}
}
