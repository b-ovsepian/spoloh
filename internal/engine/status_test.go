package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
)

func TestStatusBeforeAnyPoll(t *testing.T) {
	h := newHarness(t)

	got := h.eng.Status()
	if got.Region == "" {
		t.Error("Region should come from the configuration even before the first poll")
	}
	if got.Alert {
		t.Error("no poll has happened, so nothing should be reported as alerting")
	}
	if !got.LastSuccess.IsZero() {
		t.Error("LastSuccess should be the zero time before the first successful poll")
	}
	if !got.ClockSynced {
		t.Error("the harness uses a synced fake clock")
	}
}

func TestStatusAfterAnAlertStarts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx) // silent baseline
	h.set(true)
	if err := h.eng.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	got := h.eng.Status()
	if !got.Alert {
		t.Fatal("Status should report the alert")
	}
	if got.Since.IsZero() {
		t.Fatal("Status should report when the alert began")
	}
	if got.LastSuccess.IsZero() {
		t.Fatal("Status should report the time of the last successful poll")
	}
	if got.LastError != "" {
		t.Fatalf("LastError = %q, want empty after a success", got.LastError)
	}
}

func TestStatusReportsTheLastFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx)
	h.fail(true)
	if err := h.eng.PollOnce(ctx); err == nil {
		t.Fatal("expected the poll to fail")
	}

	got := h.eng.Status()
	if got.LastError == "" {
		t.Fatal("Status should carry the reason the last poll failed")
	}
	if !got.Failing {
		t.Fatal("Status should report that polling is currently failing")
	}
}

// A recovery must clear the failure, otherwise the dashboard shows a stale
// error forever and an operator cannot tell a current outage from an old one.
func TestStatusClearsTheFailureOnRecovery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.eng.PollOnce(ctx)
	h.fail(true)
	h.eng.PollOnce(ctx)
	h.fail(false)
	if err := h.eng.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce after recovery: %v", err)
	}

	got := h.eng.Status()
	if got.Failing || got.LastError != "" {
		t.Fatalf("Status still reports a failure after recovery: %+v", got)
	}
}

func TestStatusIsSafeForConcurrentUse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = h.eng.Status()
		}
	}()
	for i := 0; i < 20; i++ {
		h.set(i%2 == 0)
		h.eng.PollOnce(ctx)
	}
	<-done
}

func TestStatusReportsTheNextScheduledEvent(t *testing.T) {
	h := newHarness(t)

	got := h.eng.Status()
	if got.NextEventName == "" {
		t.Fatal("the default configuration has a daily event, so one should be upcoming")
	}
	if got.NextEventAt.Before(time.Now().Add(-24 * time.Hour)) {
		t.Fatalf("NextEventAt = %v, which is not a plausible upcoming time", got.NextEventAt)
	}
}

// A device that loses power in the middle of an alert must come back up
// showing that alert as ongoing, before it has managed a single poll — and
// stay showing it if upstream is unreachable, which is exactly when the
// operator needs the screen (spec §5.8). Restore is what cmd/spoloh calls on
// boot; Status has to read what it left behind rather than only what a
// successful poll wrote.
func TestStatusReportsARestoredAlertBeforeAnyPoll(t *testing.T) {
	since := time.Date(2026, 9, 6, 2, 41, 57, 0, time.UTC)
	m := alertstate.New()
	m.Restore(alertstate.Persisted{Alert: true, Since: since})

	h := newHarnessWithMachine(t, m)

	got := h.eng.Status()
	if !got.Alert {
		t.Fatal("a restored alert is not reported as ongoing before the first poll")
	}
	if !got.Since.Equal(since) {
		t.Fatalf("Since = %v, want the restored start time %v", got.Since, since)
	}
	if !got.LastSuccess.IsZero() {
		t.Error("no poll has succeeded yet, so LastSuccess must still be the zero time")
	}
}
