package alertsource_test

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// A registry entry carrying a URL with no name is exactly the drift
// task-sources-brief.md asks the registry to make impossible: Lookup still
// works, but /alerts would render a link with nothing to call it. This walks
// every real key rather than asserting on one, so a future addition to the
// registry cannot skip the check by accident.
func TestEverySourceWithAURLHasAName(t *testing.T) {
	for _, key := range alertsource.Sources() {
		info, ok := alertsource.Lookup(key)
		if !ok {
			t.Fatalf("Sources() key %q has no Lookup entry", key)
		}
		if info.URL != "" && info.Name == "" {
			t.Errorf("source %q has a URL (%q) but no name", key, info.URL)
		}
	}
}

// A source needing a credential is offered only once one is stored. Offering
// it anyway would let an operator build a cascade of slots that authenticate
// against nothing — a device that hears about no air raid while its dashboard
// shows a fully configured list.
func TestSelectableSourcesHidesACredentialledSourceUntilThereIsAToken(t *testing.T) {
	credentialled := []string{alertsource.AlertsUAKey, alertsource.UkraineAlarmKey}

	without := alertsource.SelectableSources()
	with := alertsource.SelectableSources(credentialled...)

	if len(with) <= len(without) {
		t.Fatalf("setup: no source requires a token, so this test proves nothing (with=%v, without=%v)", with, without)
	}
	for _, key := range credentialled {
		if contains(without, key) {
			t.Errorf("%q is selectable with no token stored: %v", key, without)
		}
		if !contains(with, key) {
			t.Errorf("%q is not selectable even with a token stored: %v", key, with)
		}
	}

	// The keys do not share a credential, and offering one on the strength of
	// the other's would put a slot in the cascade that can only fail to
	// authenticate. Each is checked against its own.
	only := alertsource.SelectableSources(alertsource.AlertsUAKey)
	if !contains(only, alertsource.AlertsUAKey) {
		t.Errorf("%q is not offered with its own token stored: %v", alertsource.AlertsUAKey, only)
	}
	if contains(only, alertsource.UkraineAlarmKey) {
		t.Errorf("%q is offered on the strength of another provider's token: %v", alertsource.UkraineAlarmKey, only)
	}

	// Everything else needs no credential and must be offered either way —
	// a device with no account behind it stays fully configurable.
	for _, key := range alertsource.Sources() {
		info, _ := alertsource.Lookup(key)
		if info.RequiresToken {
			continue
		}
		if !contains(without, key) {
			t.Errorf("%q needs no credential but is not offered without a token", key)
		}
	}
}

// alerts.in.ua is a different provider, not a seventh value of the Ubilling
// endpoint, so its per-slot link has to go somewhere of its own. Collapsing
// every URL back into one constant would tell an operator the two are the
// same thing.
func TestTheAlertsUAEntryDoesNotShareTheUbillingDocumentationURL(t *testing.T) {
	info, ok := alertsource.Lookup(alertsource.AlertsUAKey)
	if !ok {
		t.Fatalf("%q is not in the registry", alertsource.AlertsUAKey)
	}
	if info.URL == alertsource.DocsURL {
		t.Errorf("%q links to the Ubilling wiki (%s); it is a different provider", alertsource.AlertsUAKey, info.URL)
	}
	if info.URL == "" {
		t.Error("the one source an operator has to register for links nowhere")
	}
	if !info.RequiresToken {
		t.Error("the alerts.in.ua entry does not declare that it needs a token")
	}
}

func contains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// Lookup("") is what an unused cascade slot resolves to — it must report
// false and carry no URL, or an unused slot would render a documentation
// link with nothing chosen for it to document.
func TestLookupOfEmptyKeyFindsNothing(t *testing.T) {
	info, ok := alertsource.Lookup("")
	if ok {
		t.Fatalf("Lookup(\"\") = %+v, ok=true; want ok=false", info)
	}
	if info.URL != "" {
		t.Errorf("Lookup(\"\") URL = %q, want empty", info.URL)
	}
}
