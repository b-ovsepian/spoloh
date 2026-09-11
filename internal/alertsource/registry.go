package alertsource

// SourceInfo is the registry entry for one "?source=" cascade key: the key
// itself, a human name, and — where this repository can establish one with
// confidence — the URL of that source's API or documentation. Name and URL
// live on the same entry as Key so a new source cannot be added with a key
// and no provenance; a registry where the two can drift apart is a registry
// that will drift.
//
// This device fetches alerts from these URLs over the real internet, so URL
// is a claim about where alert data comes from, not decoration. It is left
// empty rather than guessed for every key besides "default":
// docs/superpowers/specs/2026-09-06-spoloh-design.md (spec §3) documents only
// the Ubilling base endpoint with confidence — the other five keys are
// upstream's own "?source=" values, and nothing on disk here says which
// distinct provider each one names. A wrong URL on a safety device's control
// surface is worse than no URL.
type SourceInfo struct {
	Key  string
	Name string
	URL  string
}

// sources is the fixed registry of the six upstream cascade keys spec §3
// names for "?source=", in the order the dashboard's cascade picker offers
// them. It is the one place that names them, so the /alerts screen's cascade
// picker, its validation, and its provenance links all read Sources() and
// Lookup() rather than keeping a second copy that could drift.
var sources = []SourceInfo{
	{Key: "default", Name: "Ubilling", URL: DefaultBaseURL},
	{Key: "skog", Name: "Skog"},
	{Key: "klimenko", Name: "Klimenko"},
	{Key: "jaam", Name: "JAAM"},
	{Key: "aiu", Name: "AIU"},
	{Key: "ual", Name: "UAL"},
}

// Sources returns a copy of the six keys "?source=" accepts.
func Sources() []string {
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = s.Key
	}
	return out
}

// Lookup returns the registry entry for key, and whether one exists. A key
// that is not one of Sources()'s six — including "", the "not used" slot
// value — reports false and the zero SourceInfo, which has no URL.
func Lookup(key string) (SourceInfo, bool) {
	for _, s := range sources {
		if s.Key == key {
			return s, true
		}
	}
	return SourceInfo{}, false
}
