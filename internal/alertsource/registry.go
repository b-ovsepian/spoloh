package alertsource

// SourceInfo is the registry entry for one "?source=" cascade key: the key
// itself, a human name, and — where this repository can establish one with
// confidence — the URL of that source's API or documentation. Name and URL
// live on the same entry as Key so a new source cannot be added with a key
// and no provenance; a registry where the two can drift apart is a registry
// that will drift.
//
// URL is a claim about where alert data comes from, not decoration, so it
// points at what can actually be established.
//
// All six keys reach the same endpoint. "?source=" selects which upstream
// aggregator Ubilling asks on this device's behalf (spec §3); it does not
// name six separate APIs with six addresses, and giving each key a different
// URL would assert exactly that falsehood. The names below are the keys
// themselves in title case — placeholders, not provenance. What each source
// actually calls itself arrives in the API's own response at runtime and is
// what the dashboard shows as the live source name; "default", for
// instance, currently answers as "Vadym Klymenko API".
//
// So every entry carries the documentation for the one API all of them
// speak. That is the page an operator choosing between these keys, or
// debugging one that is failing, actually needs — the wiki documents what
// each "?source=" value is.
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
	{Key: "default", Name: "Ubilling", URL: DocsURL},
	{Key: "skog", Name: "Skog", URL: DocsURL},
	{Key: "klimenko", Name: "Klimenko", URL: DocsURL},
	{Key: "jaam", Name: "JAAM", URL: DocsURL},
	{Key: "aiu", Name: "AIU", URL: DocsURL},
	{Key: "ual", Name: "UAL", URL: DocsURL},
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
