package engine_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/player"
)

func TestStatusBeforeAnyPoll(t *testing.T) {
	h := newHarness(t)

	got := h.eng.Status()
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

// Region and the next scheduled event are the presentation layer's to fill:
// internal/web computes both from the configuration in the configured
// location, and internal/web's own tests are what cover them. The engine
// deliberately leaves them zero rather than offering a second answer read
// from a different clock.
func TestStatusLeavesThePresentationFieldsToTheWebLayer(t *testing.T) {
	h := newHarness(t)

	got := h.eng.Status()
	if got.Region != "" {
		t.Errorf("Region = %q, want the engine to leave it to the presentation layer", got.Region)
	}
	if got.NextEventName != "" || !got.NextEventAt.IsZero() {
		t.Errorf("NextEvent = %q at %v, want the engine to leave both to the presentation layer", got.NextEventName, got.NextEventAt)
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
	m.Restore(alertstate.Persisted{Region: "м. Київ", Alert: true, Since: since}, "м. Київ")

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

// cascadeHarness builds an engine that polls several cascade keys against one
// fake upstream, with a real alertsource.Client walking them.
//
// It does not reuse newHarnessWith, which fixes the cascade at a single key
// before it has a URL to hand anywhere: everything below the source is what
// PollOnce needs and nothing more. The client is real on purpose — the whole
// point of these tests is what an actual cascade reports about itself, and a
// hand-written stand-in would be free to report a shape the real one never
// produces.
func cascadeHarness(t *testing.T, keys []string, handler http.HandlerFunc) *engine.Engine {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Alerts.Region = "м. Київ"
	cfg.Alerts.Sources = keys

	return engine.New(engine.Deps{
		Config:  func() *config.Config { return cfg },
		Source:  alertsource.New(srv.URL, keys, srv.Client()),
		Machine: alertstate.New(),
		// Never Run: Status only asks the player what is playing, which on
		// a player that was never started is nothing.
		Player: player.New(nil, "default"),
		Clock:  clock.System{Loc: time.UTC},
	})
}

// calmSnapshot answers every request with the same two-oblast snapshot the
// rest of this package's fakes serve.
func calmSnapshot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, snapshotBody(false, false))
}

// The cascade stops at the first key that answers, so the keys behind it were
// never asked. Reporting them as healthy would be a claim nothing checked,
// and it is the claim that sends an operator into a raid believing they have
// two working fallbacks.
func TestStatusReportsTheKeysBehindTheAnsweringOneAsNotReached(t *testing.T) {
	eng := cascadeHarness(t, []string{"default", "skog", "aiu"}, calmSnapshot)

	if err := eng.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	got := eng.Status().Sources
	want := []engine.SourceHealth{
		{Key: "default", Outcome: alertsource.OutcomeAnswered},
		{Key: "skog", Outcome: alertsource.OutcomeNotReached},
		{Key: "aiu", Outcome: alertsource.OutcomeNotReached},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status().Sources = %+v, want %+v", got, want)
	}
}

// A cascade where everything failed is the poll this is most needed for, and
// it is the one the reporting is easiest to drop on: it travels back out of
// PollOnce on the error path.
func TestStatusReportsEveryKeyWhenTheWholeCascadeFails(t *testing.T) {
	eng := cascadeHarness(t, []string{"default", "skog"}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is down", http.StatusInternalServerError)
	})

	if err := eng.PollOnce(context.Background()); err == nil {
		t.Fatal("expected the poll to fail with every source refusing")
	}

	got := eng.Status().Sources
	want := []engine.SourceHealth{
		{Key: "default", Outcome: alertsource.OutcomeFailed, Reason: alertsource.ReasonBadResponse},
		{Key: "skog", Outcome: alertsource.OutcomeFailed, Reason: alertsource.ReasonBadResponse},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status().Sources = %+v, want %+v", got, want)
	}
}

// Before the first poll nothing has been asked anything, which is not the
// same fact as a cascade of keys that were not reached — that one says a poll
// happened and stopped early. The dashboard tells them apart, so Status has
// to.
func TestStatusReportsNoCascadeBeforeTheFirstPoll(t *testing.T) {
	eng := cascadeHarness(t, []string{"default", "skog"}, calmSnapshot)

	if got := eng.Status().Sources; got != nil {
		t.Fatalf("Status().Sources = %+v before any poll, want nil", got)
	}
}
