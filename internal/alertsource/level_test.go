package alertsource_test

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
)

// The rule the whole type exists for. A source that does not report a level
// is not reporting a mild alert, it is declining to say — and a consumer that
// read that silence as yellow would announce an air raid with the lighter
// sound. Unknown is the answer every Ubilling key gives, so this is not a
// corner case: it is the common one.
func TestAnUnknownLevelIsHandledAsRed(t *testing.T) {
	if !alertsource.LevelUnknown.TreatAsRed() {
		t.Error("an unknown level is not handled as red; a level-less source's alert would be downgraded")
	}
	if !alertsource.LevelRed.TreatAsRed() {
		t.Error("a red level is not handled as red")
	}
	if alertsource.LevelYellow.TreatAsRed() {
		t.Error("a yellow level is handled as red; only a level upstream positively reported as yellow may be yellow")
	}
}

// The zero value carries the rule: anything built without thinking about
// levels — Parse, a fixture, a struct literal written next year — has to come
// out severe, not mild.
func TestTheZeroLevelIsUnknownAndThereforeRed(t *testing.T) {
	var zero alertsource.Level
	if zero != alertsource.LevelUnknown {
		t.Fatalf("the zero Level is %q, want LevelUnknown", zero)
	}
	if !zero.TreatAsRed() {
		t.Error("the zero Level is not handled as red")
	}

	// A RegionState nobody set a level on is the shape this actually
	// matters in.
	var st alertsource.RegionState
	if !st.Level.TreatAsRed() {
		t.Error("a RegionState with no level set is not handled as red")
	}
}

// A level name this build has never seen must fall to unknown — and so be
// handled as red — rather than to the lighter of the two it does know.
func TestParseLevelSendsAnythingUnrecognisedToUnknown(t *testing.T) {
	for _, in := range []string{"", "orange", "RED", "Yellow", "green", "  red"} {
		if got := alertsource.ParseLevel(in); got != alertsource.LevelUnknown {
			t.Errorf("ParseLevel(%q) = %q, want unknown", in, got)
		}
	}
	if got := alertsource.ParseLevel("red"); got != alertsource.LevelRed {
		t.Errorf("ParseLevel(\"red\") = %q", got)
	}
	if got := alertsource.ParseLevel("yellow"); got != alertsource.LevelYellow {
		t.Errorf("ParseLevel(\"yellow\") = %q", got)
	}
}

// The empty string is the right wire value for "unset", but it is the wrong
// thing to put in a log line a person has to read at three in the morning.
func TestAnUnknownLevelNamesItselfInALogLine(t *testing.T) {
	if got := alertsource.LevelUnknown.String(); got != "unknown" {
		t.Errorf("LevelUnknown.String() = %q, want \"unknown\"", got)
	}
	if got := alertsource.LevelRed.String(); got != "red" {
		t.Errorf("LevelRed.String() = %q", got)
	}
}
