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
// The first six keys reach one endpoint between them. "?source=" selects
// which upstream aggregator Ubilling asks on this device's behalf (spec §3);
// it does not name six separate APIs with six addresses, and giving each key
// a different URL would assert exactly that falsehood. Their names below are
// the keys themselves in title case — placeholders, not provenance. What each
// one actually calls itself arrives in the API's own response at runtime and
// is what the dashboard shows as the live source name; "default", for
// instance, currently answers as "Vadym Klymenko API". So all six carry the
// documentation for the one API they speak: that wiki page is what an
// operator choosing between them, or debugging one that is failing, actually
// needs, because it is where each "?source=" value is explained.
//
// AlertsUAKey is the entry that ended that arrangement, and the reason this
// comment is worth reading twice. It is a genuinely different provider — a
// different company, a different address, its own credential — and it was the
// first one here to report a severity level or a location below oblast. Its
// URL is its own, and the per-slot link on /alerts therefore stopped being six
// copies of one address the moment it was added. Do not "tidy" the URLs back
// into a single constant.
//
// UkraineAlarmKey is the second of those, and it shares nothing with the
// first: another company, another address, another credential, and an id
// space of its own that a generated table maps onto the published one. Two
// such entries is why RequiresToken is answered per key rather than by a
// single "has a token" flag — a device may hold either key, both or neither.
type SourceInfo struct {
	Key  string
	Name string
	URL  string

	// RequiresToken marks a source an operator cannot simply pick: it
	// needs a credential they have to go and obtain first. A cascade slot
	// holding a source that cannot authenticate is a slot that does
	// nothing, so such a source is offered only once a token is stored —
	// see SelectableSources. Every Ubilling key needs nothing, which is
	// what lets a device work with no account behind it at all.
	RequiresToken bool
}

// AlertsUAKey is the cascade key for the alerts.in.ua source (AlertsUA).
const AlertsUAKey = "alertsua"

// sources is the fixed registry of upstream cascade keys — the six spec §3
// names for Ubilling's "?source=", then the two providers that need a
// credential of their own — in the order the
// dashboard's cascade picker offers them. It is the one place that names
// them, so the /alerts screen's cascade picker, its validation, and its
// provenance links all read Sources() and Lookup() rather than keeping a
// second copy that could drift.
var sources = []SourceInfo{
	{Key: "default", Name: "Ubilling", URL: DocsURL},
	{Key: "skog", Name: "Skog", URL: DocsURL},
	{Key: "klimenko", Name: "Klimenko", URL: DocsURL},
	{Key: "jaam", Name: "JAAM", URL: DocsURL},
	{Key: "aiu", Name: "AIU", URL: DocsURL},
	{Key: "ual", Name: "UAL", URL: DocsURL},
	{Key: AlertsUAKey, Name: "alerts.in.ua", URL: AlertsUADocsURL, RequiresToken: true},
	{Key: UkraineAlarmKey, Name: "ukrainealarm.com", URL: UkraineAlarmDocsURL, RequiresToken: true},
}

// Sources returns a copy of every cascade key the registry knows.
//
// This is the whole registry, including sources an operator cannot choose
// yet. Ask SelectableSources for what a picker may offer and what a
// submission may be validated against; the difference between the two is
// exactly the credential.
func Sources() []string {
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = s.Key
	}
	return out
}

// SelectableSources returns the cascade keys an operator may choose from
// right now, in registry order.
//
// withToken names the sources whose credential this device holds — by key,
// because there is more than one provider needing one now and they do not
// share a credential: a stored alerts.in.ua token says nothing about whether
// ukrainealarm.com can be reached. Everything else needs no credential at all,
// which is what lets a device work with no account behind it.
//
// The answer matters more than it looks: a cascade built entirely out of
// sources that cannot authenticate is a device that hears about no air raid,
// while its dashboard shows a full, ordered, perfectly configured-looking
// list.
func SelectableSources(withToken ...string) []string {
	stored := make(map[string]bool, len(withToken))
	for _, k := range withToken {
		stored[k] = true
	}
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		if s.RequiresToken && !stored[s.Key] {
			continue
		}
		out = append(out, s.Key)
	}
	return out
}

// Lookup returns the registry entry for key, and whether one exists. A key
// that is not one of Sources() — including "", the "not used" slot value —
// reports false and the zero SourceInfo, which has no URL.
func Lookup(key string) (SourceInfo, bool) {
	for _, s := range sources {
		if s.Key == key {
			return s, true
		}
	}
	return SourceInfo{}, false
}
