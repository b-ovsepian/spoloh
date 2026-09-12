package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// chainKeys flattens the cascade keys a built source stands for, in the order
// it will try them — which is the only thing about a cascade that matters.
func chainKeys(t *testing.T, src alertsource.Cascade) []string {
	t.Helper()
	chain, ok := src.(sourceChain)
	if !ok {
		t.Fatalf("built source is %T, want a sourceChain", src)
	}
	var keys []string
	for _, link := range chain {
		keys = append(keys, link.keys...)
	}
	return keys
}

// A cascade that mixes the two providers must be tried in the order the
// operator put it in on /alerts, not re-grouped so each provider's keys sit
// together. The cascade stops at the first source that answers, so re-
// grouping would change which source that is — and the operator chose it.
func TestBuildAlertSourceKeepsTheOperatorsOrderAcrossProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Alerts.Sources = []string{"default", alertsource.AlertsUAKey, "skog", "aiu"}
	cfg.Alerts.AlertsUAToken = "a-token-this-device-holds"

	src := buildAlertSource(cfg)

	want := strings.Join(cfg.Alerts.Sources, ",")
	if got := strings.Join(chainKeys(t, src), ","); got != want {
		t.Fatalf("cascade order = %q, want %q", got, want)
	}

	// And each key reached the provider that can actually serve it. Handing
	// "alertsua" to the Ubilling client would send it as a "?source=" value,
	// which fails without ever saying why.
	chain := src.(sourceChain)
	if len(chain) != 3 {
		t.Fatalf("chain has %d links, want three: the Ubilling run, alerts.in.ua, then the rest", len(chain))
	}
	if _, ok := chain[0].src.(*alertsource.Client); !ok {
		t.Errorf("link 1 (%v) is %T, want the Ubilling client", chain[0].keys, chain[0].src)
	}
	ua, ok := chain[1].src.(*alertsource.AlertsUA)
	if !ok {
		t.Fatalf("link 2 (%v) is %T, want the alerts.in.ua client", chain[1].keys, chain[1].src)
	}
	if !ua.HasToken() {
		t.Error("the alerts.in.ua client was built without the stored token; every poll would fail before it was sent")
	}
	if _, ok := chain[2].src.(*alertsource.Client); !ok {
		t.Errorf("link 3 (%v) is %T, want the Ubilling client", chain[2].keys, chain[2].src)
	}
}

// A source whose credential this device does not hold is left out rather than
// dialled. A slot that cannot authenticate does nothing but burn a poll, and
// the cascade behind it is what actually has to answer.
func TestBuildAlertSourceLeavesOutASourceWithNoToken(t *testing.T) {
	cfg := config.Default()
	cfg.Alerts.Sources = []string{alertsource.AlertsUAKey, "default", "skog"}
	cfg.Alerts.AlertsUAToken = ""

	src := buildAlertSource(cfg)

	// One provider left, so no chain wrapper at all — the shape every
	// unregistered device in the field runs.
	if _, ok := src.(*alertsource.Client); !ok {
		t.Fatalf("built source is %T, want the bare Ubilling client — alerts.in.ua must not be in a cascade it cannot authenticate to", src)
	}
}

// The one outcome this project refuses is a device with nothing to ask. A
// cascade of nothing but a source that cannot authenticate must still leave
// something that polls.
func TestBuildAlertSourceNeverLeavesTheDeviceWithNoSource(t *testing.T) {
	cfg := config.Default()
	cfg.Alerts.Sources = []string{alertsource.AlertsUAKey}
	cfg.Alerts.AlertsUAToken = ""

	if src := buildAlertSource(cfg); src == nil {
		t.Fatal("a cascade whose only source has no credential built no source at all")
	}

	cfg.Alerts.Sources = nil
	if src := buildAlertSource(cfg); src == nil {
		t.Fatal("an empty cascade built no source at all")
	}
}

// fakeCascade stands in for a provider. It opens no socket: every test here
// runs on a board that must never depend on the internet to prove itself.
type fakeCascade struct {
	key string
	err error
}

func (f fakeCascade) Fetch(context.Context) (alertsource.Snapshot, error) {
	return alertsource.Snapshot{Source: f.key}, f.err
}

func (f fakeCascade) FetchCascade(context.Context) (alertsource.Snapshot, []alertsource.Attempt, error) {
	a := alertsource.Attempt{Key: f.key, Outcome: alertsource.OutcomeAnswered}
	if f.err != nil {
		a.Outcome = alertsource.OutcomeFailed
		a.Err = f.err
	}
	snap, err := f.Fetch(context.Background())
	return snap, []alertsource.Attempt{a}, err
}

// A mixed cascade has to report itself the way a single-provider one does:
// one attempt per configured key, in cascade order, including the keys it
// never got to. Reporting an unconsulted source as working is a claim nothing
// verified; reporting it as failed sends whoever installed the device chasing
// a fault that does not exist.
func TestSourceChainReportsEveryKeyIncludingTheOnesItNeverReached(t *testing.T) {
	chain := sourceChain{
		{keys: []string{"default"}, src: fakeCascade{key: "default", err: errors.New("upstream is down")}},
		{keys: []string{alertsource.AlertsUAKey}, src: fakeCascade{key: alertsource.AlertsUAKey}},
		{keys: []string{"skog", "aiu"}, src: fakeCascade{key: "skog"}},
	}

	snap, attempts, err := chain.FetchCascade(context.Background())
	if err != nil {
		t.Fatalf("FetchCascade: %v", err)
	}
	if snap.Source != alertsource.AlertsUAKey {
		t.Errorf("snapshot came from %q, want the first source that answered", snap.Source)
	}

	want := []struct {
		key     string
		outcome alertsource.Outcome
	}{
		{"default", alertsource.OutcomeFailed},
		{alertsource.AlertsUAKey, alertsource.OutcomeAnswered},
		{"skog", alertsource.OutcomeNotReached},
		{"aiu", alertsource.OutcomeNotReached},
	}
	if len(attempts) != len(want) {
		t.Fatalf("%d attempts, want one per configured key (%d): %+v", len(attempts), len(want), attempts)
	}
	for i, w := range want {
		if attempts[i].Key != w.key || attempts[i].Outcome != w.outcome {
			t.Errorf("attempt %d = %q %v, want %q %v", i, attempts[i].Key, attempts[i].Outcome, w.key, w.outcome)
		}
	}
}

// Every link failing is one error, not a snapshot: an empty snapshot means
// "no alerts anywhere", and returning that when nothing answered would end a
// raid that is still running.
func TestSourceChainFailsWhenNothingAnswers(t *testing.T) {
	chain := sourceChain{
		{keys: []string{"default"}, src: fakeCascade{key: "default", err: errors.New("upstream is down")}},
		{keys: []string{alertsource.AlertsUAKey}, src: fakeCascade{key: alertsource.AlertsUAKey, err: alertsource.ErrTokenRejected}},
	}

	if _, _, err := chain.FetchCascade(context.Background()); err == nil {
		t.Fatal("a cascade in which nothing answered reported success")
	}
	if _, err := chain.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch reported success where FetchCascade reported failure")
	}
}
