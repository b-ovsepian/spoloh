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
