package eventlog_test

import (
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/eventlog"
)

// What /metrics reports as spoloh_events_total: every entry since the daemon
// started, by kind and outcome. Not the table, which is capped and pruned —
// a counter that fell when old rows were swept would read to a monitoring
// system as a restart.
func TestCountsAreByKindAndOutcomeAndSurvivePruning(t *testing.T) {
	lg, _ := openLog(t, 5)
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		_ = lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindScheduled, OK: true})
	}
	_ = lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindPlaybackError, OK: false})
	_ = lg.Append(eventlog.Entry{At: at, Kind: eventlog.KindPlaybackError, OK: false})

	got := map[eventlog.CountKey]uint64{}
	for _, c := range lg.Counts() {
		got[c.CountKey] = c.N
	}
	if n := got[eventlog.CountKey{Kind: eventlog.KindScheduled, OK: true}]; n != 40 {
		t.Errorf("scheduled ok = %d, want 40 though the table holds at most 5", n)
	}
	if n := got[eventlog.CountKey{Kind: eventlog.KindPlaybackError, OK: false}]; n != 2 {
		t.Errorf("playback_error failed = %d, want 2", n)
	}
	if lg.WriteErrors() != 0 {
		t.Errorf("WriteErrors = %d with every write succeeding", lg.WriteErrors())
	}
}

// An SD card remounted read-only is the classic way a Pi loses its log. The
// event still happened, and is counted; the write that failed is counted
// too, which is the number a monitoring system can raise an alarm on.
func TestAFailedWriteIsCountedAndTheEventStillIs(t *testing.T) {
	lg, _ := openLog(t, 100)
	lg.Close()
	if err := lg.Append(eventlog.Entry{Kind: eventlog.KindAlert, OK: true}); err == nil {
		t.Fatal("Append on a closed log succeeded")
	}
	if lg.WriteErrors() != 1 {
		t.Errorf("WriteErrors = %d, want 1", lg.WriteErrors())
	}
	counts := lg.Counts()
	if len(counts) != 1 || counts[0].Kind != eventlog.KindAlert || counts[0].N != 1 {
		t.Errorf("Counts = %+v, want the alert counted once", counts)
	}
}

func TestANilLogCountsNothing(t *testing.T) {
	var lg *eventlog.Log
	if lg.Counts() != nil || lg.WriteErrors() != 0 {
		t.Fatal("a nil log reported counts")
	}
}
