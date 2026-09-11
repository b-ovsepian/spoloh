package alertsource_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// attemptFor finds one key's attempt, so a test can name the key it means
// rather than count positions.
func attemptFor(t *testing.T, attempts []alertsource.Attempt, key string) alertsource.Attempt {
	t.Helper()
	for _, a := range attempts {
		if a.Key == key {
			return a
		}
	}
	t.Fatalf("no attempt reported for %q; got %+v", key, attempts)
	return alertsource.Attempt{}
}

// The three-way answer the dashboard needs. A cascade stops at the first
// source that answers, so the keys after it were never asked — and saying so
// is the whole point: calling them healthy claims something nobody checked,
// and calling them failed sends someone after a fault that is not there.
func TestFetchCascadeTellsAnsweredFromFailedFromNotReached(t *testing.T) {
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("source") == "default" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default", "skog", "jaam"}, srv.Client())
	snap, attempts, err := c.FetchCascade(context.Background())
	if err != nil {
		t.Fatalf("FetchCascade: %v", err)
	}
	if len(snap.States) == 0 {
		t.Fatal("the cascade returned an empty snapshot")
	}

	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want one per configured key (3): %+v", len(attempts), attempts)
	}
	if got := attempts[0].Key; got != "default" {
		t.Errorf("attempts are not in cascade order: first key = %q, want \"default\"", got)
	}

	if got := attemptFor(t, attempts, "default").Outcome; got != alertsource.OutcomeFailed {
		t.Errorf("\"default\" 500ed but is reported %v", got)
	}
	if got := attemptFor(t, attempts, "skog").Outcome; got != alertsource.OutcomeAnswered {
		t.Errorf("\"skog\" produced the snapshot but is reported %v", got)
	}
	// The one that matters: "jaam" comes after the source that answered, so
	// it was never asked. It is not working and it is not broken.
	jaam := attemptFor(t, attempts, "jaam")
	if jaam.Outcome != alertsource.OutcomeNotReached {
		t.Errorf("\"jaam\" was never asked but is reported %v", jaam.Outcome)
	}
	if jaam.Err != nil {
		t.Errorf("\"jaam\" was never asked but carries an error: %v", jaam.Err)
	}
	if jaam.Reason() != alertsource.ReasonNone {
		t.Errorf("\"jaam\" was never asked but has a failure reason %q", jaam.Reason())
	}
}

// The reported key is the cascade key the operator configured, not the name
// the upstream answers with. "default" reports itself as "Vadym Klymenko
// API"; a card built from that could not tell anyone which of their six
// configured keys is the one working.
func TestFetchCascadeReportsTheCascadeKeyNotTheUpstreamsOwnName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"source":"Vadym Klymenko API","cachedat":"2026-09-06 15:39:05","states":{"м. Київ":{"alertnow":false,"changed":"2026-09-06 02:44:20"}}}`))
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default"}, srv.Client())
	snap, attempts, err := c.FetchCascade(context.Background())
	if err != nil {
		t.Fatalf("FetchCascade: %v", err)
	}
	if snap.Source != "Vadym Klymenko API" {
		t.Fatalf("setup: the snapshot does not carry a self-reported name: %q", snap.Source)
	}
	if attempts[0].Key != "default" {
		t.Errorf("the answering source is reported as %q, want the cascade key \"default\"", attempts[0].Key)
	}
}

// When every key fails, every key is reported as failed — none is left
// looking unconsulted, which would read as "we did not need it".
func TestFetchCascadeReportsEveryKeyWhenAllOfThemFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default", "skog"}, srv.Client())
	_, attempts, err := c.FetchCascade(context.Background())
	if err == nil {
		t.Fatal("a cascade where every source failed reported success")
	}
	for _, a := range attempts {
		if a.Outcome != alertsource.OutcomeFailed {
			t.Errorf("%q is reported %v after a 502", a.Key, a.Outcome)
		}
		if a.Reason() != alertsource.ReasonBadResponse {
			t.Errorf("%q reason = %q, want bad_response", a.Key, a.Reason())
		}
	}
}

// Reason is a closed vocabulary so that a card can explain a failure without
// letting upstream text — an HTML error page, a captive portal, a credential
// echoed back — reach the page. Each reason has a different next step for
// whoever installed the device, which is why they are told apart at all.
func TestAttemptReasonClassifiesEachFailure(t *testing.T) {
	const token = "token-that-must-not-escape"

	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer rateLimited.Close()

	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad token ` + token + `"}`))
	}))
	defer rejecting.Close()

	// A server that is not listening: the request never completes, which is
	// what a device with no working internet looks like on every key at once.
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := unreachable.URL
	unreachableClient := unreachable.Client()
	unreachable.Close()

	tests := []struct {
		name string
		src  alertsource.Cascade
		want alertsource.Reason
	}{
		{"no token", alertsource.NewAlertsUA("http://127.0.0.1:1/", "", nil), alertsource.ReasonNoToken},
		{"token rejected", alertsource.NewAlertsUA(rejecting.URL, token, rejecting.Client()), alertsource.ReasonTokenRejected},
		{"rate limited", alertsource.New(rateLimited.URL, []string{"default"}, rateLimited.Client()), alertsource.ReasonRateLimited},
		{"unreachable", alertsource.New(unreachableURL, []string{"default"}, unreachableClient), alertsource.ReasonUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, attempts, err := tc.src.FetchCascade(context.Background())
			if err == nil {
				t.Fatal("the fetch was expected to fail")
			}
			if len(attempts) != 1 {
				t.Fatalf("attempts = %+v, want one", attempts)
			}
			if got := attempts[0].Reason(); got != tc.want {
				t.Errorf("reason = %q, want %q (err was %v)", got, tc.want, attempts[0].Err)
			}
			// The reason is what a page renders, so it has to be one of
			// this package's own values and nothing the far end chose.
			if strings.Contains(string(attempts[0].Reason()), token) {
				t.Errorf("the reason carries the token: %q", attempts[0].Reason())
			}
		})
	}
}

// The far end's own text must not ride out on anything a page renders. This
// is the case that made Reason exist rather than a string built from the
// error: a captive portal answers 200 with its sign-in page, and the error
// that results quotes what it sent.
func TestAnUpstreamsOwnTextReachesTheErrorButNotTheReason(t *testing.T) {
	const marker = "sign-in-to-continue"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8; portal="+marker)
		w.Write([]byte("<html>" + marker + "</html>"))
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default"}, srv.Client())
	_, attempts, err := c.FetchCascade(context.Background())
	if err == nil {
		t.Fatal("an HTML body was accepted as alert state")
	}
	if !strings.Contains(attempts[0].Err.Error(), marker) {
		t.Fatalf("setup: the error does not carry the upstream's text, so this proves nothing: %v", attempts[0].Err)
	}
	if got := attempts[0].Reason(); got != alertsource.ReasonBadResponse {
		t.Errorf("reason = %q, want bad_response", got)
	}
	if strings.Contains(string(attempts[0].Reason()), marker) {
		t.Errorf("the reason carries the upstream's own text: %q", attempts[0].Reason())
	}
}

// A single alerts.in.ua source reports itself in the same shape a multi-key
// cascade does, under its own cascade key — so a caller assembling a mixed
// cascade needs no special case for the one provider that is not a
// "?source=" value.
func TestAlertsUAReportsItselfUnderItsCascadeKey(t *testing.T) {
	body := alertsUAFixture(t)
	srv, _ := serveAlertsUA(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	c := alertsource.NewAlertsUA(srv.URL, testToken, srv.Client())
	_, attempts, err := c.FetchCascade(context.Background())
	if err != nil {
		t.Fatalf("FetchCascade: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Key != alertsource.AlertsUAKey {
		t.Fatalf("attempts = %+v, want one for %q", attempts, alertsource.AlertsUAKey)
	}
	if attempts[0].Outcome != alertsource.OutcomeAnswered {
		t.Errorf("the source answered but is reported %v", attempts[0].Outcome)
	}
}

// Fetch's contract is unchanged for every caller that only wants a snapshot.
func TestFetchStillSatisfiesSourceAfterTheCascadeReport(t *testing.T) {
	var _ alertsource.Source = alertsource.New("", nil, nil)
	var _ alertsource.Cascade = alertsource.New("", nil, nil)
	var _ alertsource.Cascade = alertsource.NewAlertsUA("", "", nil)
}
