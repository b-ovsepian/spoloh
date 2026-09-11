package alertsource

// Level is how severe upstream says an alert is.
//
// Three values, and the third is the whole point of the type: red, yellow,
// and unknown — the answer given by every source that has no level concept
// at all. All six "?source=" keys are such sources: the Ubilling endpoint
// reports a bare boolean per oblast and says nothing about severity, so every
// state it produces is unknown. alerts.in.ua is the first source here that
// reports a level.
//
// The rule for unknown is the one a consumer must not get backwards:
// **unknown is handled as red, never as yellow.** A source that does not
// report a level is not reporting a mild alert; it is declining to say. Treat
// that silence as yellow and a full air raid gets the lighter announcement —
// on a device whose entire job is to be heard during exactly that alert.
// Treat it as red and, at worst, an alert that was really yellow is announced
// more loudly than it needed to be. One of those mistakes wakes a street that
// could have slept; the other leaves a street asleep. Only a level upstream
// positively reported as yellow is ever treated as yellow, and TreatAsRed
// below is the only sanctioned way to ask.
//
// The zero value is LevelUnknown deliberately. A RegionState built by code
// that knows nothing about levels — Parse, every test fixture, anything
// written after this — comes out unknown, and therefore severe, without
// anyone having to remember to set the field.
type Level string

const (
	// LevelUnknown is "upstream did not say", which is handled as red.
	LevelUnknown Level = ""
	// LevelYellow and LevelRed are alerts.in.ua's own two values.
	LevelYellow Level = "yellow"
	LevelRed    Level = "red"
)

// ParseLevel maps an upstream "alert_level" string onto a Level. Anything
// this build does not recognise — a value the API grows after this release,
// an empty string, a typo upstream — becomes LevelUnknown and is therefore
// handled as red. A level name we have never seen must never be read as the
// lighter of the two we have.
func ParseLevel(s string) Level {
	switch Level(s) {
	case LevelRed:
		return LevelRed
	case LevelYellow:
		return LevelYellow
	default:
		return LevelUnknown
	}
}

// TreatAsRed reports whether this level must be handled as a red alert.
//
// It is written as "anything that is not yellow" rather than "red or
// unknown" on purpose: a value added to Level later is then severe by
// default, instead of being severe only if whoever added it remembered to
// extend a list here.
func (l Level) TreatAsRed() bool { return l != LevelYellow }

// String names the level for a log line. LevelUnknown is the empty string on
// the wire — useful, because an unset field is unknown — but "unknown" is
// what a person reading a log needs to see.
func (l Level) String() string {
	if l == LevelUnknown {
		return "unknown"
	}
	return string(l)
}

// moreSevere returns whichever of two levels must win when one location is
// named by more than one active alert at the same time — an air raid and an
// artillery warning over the same hromada, say. Unknown wins over both named
// levels for the reason in Level's own comment: it is not a mild answer, it
// is no answer, and it is handled as red.
func moreSevere(a, b Level) Level {
	switch {
	case a == b:
		return a
	case a == LevelUnknown || b == LevelUnknown:
		return LevelUnknown
	case a == LevelRed || b == LevelRed:
		return LevelRed
	default:
		return LevelYellow
	}
}
