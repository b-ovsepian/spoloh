package alertsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// UkraineAlarmKey is the cascade key for this source.
//
// It lives here rather than in registry.go, where AlertsUAKey does, because
// everything else about this provider is in this file: a client that names
// itself in an Attempt needs the key before the registry has an opinion about
// it, and the registry entry a few lines of code away reads this constant.
const UkraineAlarmKey = "ukrainealarm"

// UkraineAlarmBaseURL is the ukrainealarm.com API root, version 3.
const UkraineAlarmBaseURL = "https://api.ukrainealarm.com/api/v3/"

// UkraineAlarmAlertsPath is the per-region state endpoint, relative to the
// root; the region id is appended to it.
//
// This device asks about one region — the oblast (or special-status city)
// containing the place the operator chose — and not about the country. The
// spec also offers a bare "alerts" that answers for everywhere at once, and
// that is the wrong trade here: a board on a wall cares about one oblast, and
// the whole-country body is the one thing on this API that grows without
// bound as a bad night goes on.
//
// Asking for the oblast rather than for the hromada is deliberate and is the
// safety half of the choice. An alert is declared at whatever level the
// authorities announce it — usually the raion — and an answer for one hromada
// cannot carry its raion's alert.
//
// So this asks for the country, not for a region. The spec's own summaries
// settle it: /alerts is «Області, регіони та громади з тривогами» — every
// level that currently has one — while /alerts/{regionId} is «Статус
// області/регіону/громади», that region's own status and nothing about what
// is declared inside it. Asking an oblast and hoping its answer nests its
// raions is an assumption the spec does not make, and the cost of being
// wrong is a hromada watcher silent through a raion alert.
//
// The country is also what alerts.in.ua's client already reads, for the same
// reason, and propagateDownward then takes containment the rest of the way
// down using this build's own table rather than the provider's nesting.
const UkraineAlarmAlertsPath = "alerts"

// UkraineAlarmStatusPath is the change index — a single integer that moves
// whenever anything anywhere in the country changes. See Fetch: it replaces
// the state request on a tick where nothing has happened, which is most ticks.
const UkraineAlarmStatusPath = "alerts/status"

// UkraineAlarmDocsURL is where an operator reads about this provider and
// where the key is asked for.
//
// It is the API's own published specification, the same document
// docs/ukrainealarm-swagger.json was taken from: it states the security
// scheme, and its description is where the token's origin ("the one mailed to
// you after registering") is written down. There is no separate developer
// portal to send somebody to, and a guessed address on a device that really
// dials out is worse than the one address that can be established.
const UkraineAlarmDocsURL = "https://api.ukrainealarm.com/swagger/index.html"

// ukraineAlarmSourceName is what a snapshot from here calls itself. This API
// names no source in its payload, so the dashboard's live source line would
// otherwise be blank for it — see alertsUASourceName, same reasoning.
const ukraineAlarmSourceName = "ukrainealarm.com"

// ErrNoRegionMapping reports that this build has no ukrainealarm region id
// for the place this device watches.
//
// It is returned before any request is made, and it is its own sentinel
// rather than a generic failure because the operator's next step is unlike
// every other one on this screen: nothing is wrong with the network, the key
// or the far end — the mapping table in this build simply does not name their
// oblast, and regenerating it (see ukrainealarm_gen.go) is what fixes it.
var ErrNoRegionMapping = errors.New("ukrainealarm: no region id for the configured location")

// UkraineAlarm fetches alerts for one oblast from ukrainealarm.com.
//
// Two things separate it from the other providers here.
//
// The credential travels in a bare Authorization header — not "Bearer ", not
// "Token ", the key and nothing else. The spec declares an apiKey scheme in
// the header rather than an HTTP auth scheme, and a prefix added out of habit
// comes back 401, which reads in a log exactly like a key that has been
// revoked.
//
// And no rate limit is documented anywhere in that spec, which is not the
// same as there being none. So this polls at the interval the operator set,
// like every other source, and uses the change index to make most of those
// ticks cost a single integer rather than a region's whole state.
//
// Zero value is not usable; call NewUkraineAlarm.
type UkraineAlarm struct {
	baseURL string
	token   string

	// scope is the oblast this device asks about, and regionID is its id at
	// the far end. Both are fixed at construction: the operator's chosen
	// place cannot change without the configuration changing, and the
	// daemon rebuilds its cascade when that happens.
	scope    Location
	regionID string

	hc *http.Client

	// mu guards the change-index cache below and serialises Fetch, for the
	// reason AlertsUA gives: two in-flight polls racing could store an
	// index belonging to a body that was never the one cached.
	mu sync.Mutex
	// index is the lastActionIndex the cached body was fetched at, and
	// haveIndex says whether it was ever read successfully — an index of 0
	// is a real value this API could return. cached is the body itself,
	// kept rather than the parsed Snapshot so that an unchanged tick
	// re-parses into a fresh map: handing the same map to two callers would
	// make one caller's bookkeeping visible to the other.
	index     int64
	haveIndex bool
	cached    []byte
}

// NewUkraineAlarm builds a source for the oblast containing the watched
// location, which is how this provider is asked anything at all: locationUID
// is the operator's chosen place (0 for "the whole oblast") and region is the
// oblast name the rest of the device already stores.
//
// baseURL is a parameter so tests can point it at a loopback server;
// production passes UkraineAlarmBaseURL.
//
// A location or a region this build cannot resolve is not an error here. The
// source is built, and every Fetch fails with ErrNoRegionMapping — which is
// what the cascade is for, and is far better than a daemon that will not
// start because a table it ships lacks a row.
func NewUkraineAlarm(baseURL, token string, locationUID int, region string, hc *http.Client) *UkraineAlarm {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if baseURL == "" {
		baseURL = UkraineAlarmBaseURL
	}
	c := &UkraineAlarm{baseURL: baseURL, token: token}
	c.hc = hc
	if oblast, ok := watchedOblast(locationUID, region); ok {
		c.scope = oblast
		c.regionID, _ = UkraineAlarmRegionID(oblast.UID)
	}
	return c
}

// The dashboard's per-source card reads Attempts through Cascade, and the
// engine asks for them only when its source satisfies that interface. A
// provider that implemented Source alone would compile, poll correctly, and
// leave the card silent about it forever — a feature dead with nothing
// failing, which no test sees. So it is asserted here, the way
// internal/web/audio.go and cmd/spoloh's sourceChain assert theirs.
var _ Cascade = (*UkraineAlarm)(nil)

// HasToken reports whether this source has a credential to fetch with, the
// same question AlertsUA answers: a cascade slot that cannot authenticate is
// a slot that does nothing.
func (c *UkraineAlarm) HasToken() bool { return c.token != "" }

// Fetch returns the current picture for the watched oblast and everything
// inside it.
//
// The change index is what makes this affordable. /alerts/status answers with
// one integer that moves when anything in the country changes, so on a tick
// where it has not moved the cached body is re-parsed and the region's state
// is never asked for. Two things it deliberately is not:
//
// It does not precede every request. The index is read only when there is a
// cached body it could spare us fetching again; the first poll of a process
// goes straight for the state.
//
// And a failure to read it is not "nothing changed". An unreadable index
// falls through to the full request, because the alternative is a device that
// goes quiet because a cheap endpoint broke.
//
// No error from here ever contains the key. It is a credential: it does not
// belong in a log, in a wrapped error, or on the page an operator is looking
// at when the fetch fails.
func (c *UkraineAlarm) Fetch(ctx context.Context) (Snapshot, error) {
	if c.token == "" {
		return Snapshot{}, ErrNoToken
	}
	if c.regionID == "" {
		return Snapshot{}, ErrNoRegionMapping
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		index    int64
		haveIdx  bool
		unchange bool
	)
	if c.cached != nil {
		if v, err := c.status(ctx); err == nil {
			index, haveIdx = v, true
			unchange = c.haveIndex && v == c.index
		}
	}
	if unchange {
		return ParseUkraineAlarm(c.cached, c.scope)
	}

	body, err := c.state(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := ParseUkraineAlarm(body, c.scope)
	if err != nil {
		return Snapshot{}, err
	}
	// Only a body that parsed is worth short-circuiting to later: caching one
	// that did not would hand the same unreadable bytes back on every quiet
	// tick after it.
	c.cached = body
	c.index, c.haveIndex = index, haveIdx
	return snap, nil
}

// FetchCascade lets this single source report itself in the shape a multi-key
// cascade does, so a caller assembling a mixed cascade needs no special case
// for it. Same contract as AlertsUA's.
func (c *UkraineAlarm) FetchCascade(ctx context.Context) (Snapshot, []Attempt, error) {
	snap, err := c.Fetch(ctx)
	attempt := Attempt{Key: UkraineAlarmKey, Outcome: OutcomeAnswered}
	if err != nil {
		attempt.Outcome = OutcomeFailed
		attempt.Err = err
	}
	return snap, []Attempt{attempt}, err
}

// get performs one authenticated request and returns the body.
func (c *UkraineAlarm) get(ctx context.Context, path string) ([]byte, error) {
	url := strings.TrimSuffix(c.baseURL, "/") + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Bare. The spec's security scheme is an apiKey in a header that happens
	// to be called Authorization, not an HTTP authentication scheme, so there
	// is no prefix to put in front of it.
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return nil, &RateLimitError{RetryAfter: retryAfter(resp)}
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w (http %d)", ErrTokenRejected, resp.StatusCode)
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	// A 200 carrying HTML is an upstream error page or a captive portal.
	// Parsing it as state would be worse than reporting a failure.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return nil, fmt.Errorf("unexpected content type %q", ct)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// status reads the change index. Its error is dropped by Fetch on purpose —
// see there — so nothing here needs to classify why it failed.
func (c *UkraineAlarm) status(ctx context.Context) (int64, error) {
	body, err := c.get(ctx, UkraineAlarmStatusPath)
	if err != nil {
		return 0, err
	}
	// A pointer so that an envelope with no lastActionIndex in it — an error
	// page that happened to be JSON, a shape this build does not know — is
	// not read as index 0, which would look like a perfectly good answer and
	// freeze this source on its cached body.
	var got struct {
		LastActionIndex *int64 `json:"lastActionIndex"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return 0, fmt.Errorf("parse ukrainealarm status: %w", err)
	}
	if got.LastActionIndex == nil {
		return 0, errors.New("parse ukrainealarm status: payload carries no lastActionIndex")
	}
	return *got.LastActionIndex, nil
}

// state reads every alert in the country.
//
// Not the watched region's own endpoint: see UkraineAlarmAlertsPath. What
// comes back is every oblast, raion and hromada that currently has an alert,
// each naming its own region, which is what lets a raion's declaration reach
// the hromadas inside it through this build's table rather than through a
// nesting the provider never promised.
func (c *UkraineAlarm) state(ctx context.Context) ([]byte, error) {
	return c.get(ctx, UkraineAlarmAlertsPath)
}

// ukraineAlarmLevelWithReason is one entry of activeAlertLevels.
type ukraineAlarmLevelWithReason struct {
	AlertLevel string `json:"alertLevel"`
	Reason     string `json:"reason"`
	CreatedAt  string `json:"createdAt"`
}

// ukraineAlarmAlert is one Alert object. Its regionId is the place the alert
// was declared for, which is not necessarily the region that was asked about:
// an oblast's answer carries the raions and hromadas inside it, each naming
// itself, and that is the whole reason this provider is asked for the oblast.
type ukraineAlarmAlert struct {
	RegionID          string                        `json:"regionId"`
	RegionType        string                        `json:"regionType"`
	Type              string                        `json:"type"`
	LastUpdate        string                        `json:"lastUpdate"`
	ActiveAlertLevels []ukraineAlarmLevelWithReason `json:"activeAlertLevels"`
}

// ukraineAlarmRegion is one AlertRegionModel. ActiveAlerts is a pointer for
// the reason alertsUABody.Alerts is: "the key was absent" and "the key was
// there and empty" are the difference between a payload we do not understand
// and a region with nothing running, which is the difference between
// reporting a failure and reporting an all-clear.
type ukraineAlarmRegion struct {
	RegionID     string               `json:"regionId"`
	RegionType   string               `json:"regionType"`
	RegionName   string               `json:"regionName"`
	LastUpdate   string               `json:"lastUpdate"`
	ActiveAlerts *[]ukraineAlarmAlert `json:"activeAlerts"`
}

// ParseUkraineAlarm converts one region's answer into a Snapshot, for the
// oblast (or special-status city) scope.
//
// The result is a complete picture of that oblast and nothing else. Every
// location the published table puts inside scope is seeded calm and the
// payload's alerts are laid over it, which is what the endpoint means — it
// answers with what is active, so a place it does not name is not under
// alert. Nothing outside scope is invented: this provider was asked about one
// oblast and can speak for one oblast, and seeding the other 24 calm would be
// claiming an all-clear nobody checked.
//
// Each alert is placed at the region it names, never at the region that was
// asked about. Raising the oblast because something inside it is raised would
// be upward propagation, which this repository refuses on purpose — an oblast
// watcher hearing a siren for a raid two districts away learns to ignore the
// siren. propagateDownward then does the direction that is true by
// definition: everything inside a raised place is raised.
//
// A body whose shape is not recognised is an error, never an empty snapshot.
// Empty here means "nothing is running", and the one thing this must never do
// is turn an unreadable payload into an all-clear during a raid.
func ParseUkraineAlarm(data []byte, scope Location) (Snapshot, error) {
	var regions *[]ukraineAlarmRegion
	if err := json.Unmarshal(data, &regions); err != nil {
		return Snapshot{}, fmt.Errorf("parse ukrainealarm snapshot: %w", err)
	}
	if regions == nil {
		return Snapshot{}, errors.New("parse ukrainealarm snapshot: payload is not a list of regions")
	}
	if scope.UID == 0 {
		return Snapshot{}, ErrNoRegionMapping
	}

	subtree := oblastSubtree(scope.UID)
	snap := Snapshot{
		Source: ukraineAlarmSourceName,
		States: make(map[string]RegionState, len(subtree)),
	}
	// Every location inside the watched oblast, not only the oblast itself.
	// A calm hromada that is absent rather than present-and-calm is read one
	// level up as "this provider cannot serve that granularity", and the
	// device quietly broadens to the whole oblast for as long as nothing is
	// happening — the opposite of what choosing a hromada asks for. See
	// ParseAlertsUA, where that was a real bug.
	for _, l := range subtree {
		snap.States[l.Name] = RegionState{Region: l.Name, UID: l.UID}
	}

	for _, region := range *regions {
		if t, ok := ukraineAlarmTime(region.LastUpdate); ok && t.After(snap.CachedAt) {
			snap.CachedAt = t
		}
		if region.ActiveAlerts == nil {
			continue
		}
		for _, a := range *region.ActiveAlerts {
			// An alert that names no region of its own belongs to the region
			// whose answer carried it.
			id := a.RegionID
			if id == "" {
				id = region.RegionID
			}
			uid, ok := UkraineAlarmUID(id)
			if !ok {
				// A region this build has no row for. It cannot raise
				// anything, because there is no location an operator could
				// have chosen that it names — and guessing it belongs to the
				// oblast that answered would announce for a whole oblast on
				// the strength of one unrecognised id.
				continue
			}
			l, ok := LookupLocation(uid)
			if !ok {
				continue
			}

			st := RegionState{
				Region: l.Name,
				Alert:  true,
				Level:  ukraineAlarmLevel(a.ActiveAlertLevels),
				// Upstream's own AlertType — "AIR", "ARTILLERY", and the
				// rest of the enum — carried verbatim, the way RegionState.Type
				// is documented: it arrives for free with the parse, nothing
				// branches on it, and translating it into another provider's
				// vocabulary would invent a mapping nobody needs yet.
				Type: a.Type,
				UID:  uid,
			}
			if t, ok := ukraineAlarmAlertStart(a); ok {
				st.Changed = t
			}

			// One place can be named by more than one alert at once — an air
			// raid and an artillery warning over the same raion. They
			// collapse the way ParseAlertsUA collapses them: the more severe
			// level, the earlier start, the first type seen.
			if prev, ok := snap.States[l.Name]; ok && prev.Alert {
				st.Level = moreSevere(prev.Level, st.Level)
				if !prev.Changed.IsZero() && (st.Changed.IsZero() || prev.Changed.Before(st.Changed)) {
					st.Changed = prev.Changed
				}
				if prev.Type != "" {
					st.Type = prev.Type
				}
			}
			snap.States[l.Name] = st
		}
	}

	propagateDownward(&snap)
	return snap, nil
}

// ukraineAlarmLevel reads the severity off an alert's activeAlertLevels.
//
// The API's enum is exactly Red or Yellow, so this provider is the first here
// that genuinely reports a level — and it must never answer LevelUnknown for
// a region it did answer for. Unknown is handled as red everywhere (see
// Level), so answering unknown would not endanger anybody; it would show an
// operator "рівень невідомий" for a source that had just told us the level,
// which is a dashboard that lies about what it knows.
//
// Anything that is not the literal "Yellow" — an absent list, an empty one, a
// value added to the enum after this release — is red. The lighter of two
// levels is never the one to guess.
func ukraineAlarmLevel(levels []ukraineAlarmLevelWithReason) Level {
	if len(levels) == 0 {
		return LevelRed
	}
	worst := LevelYellow
	for _, l := range levels {
		if !strings.EqualFold(l.AlertLevel, string(LevelYellow)) {
			worst = LevelRed
		}
	}
	return worst
}

// ukraineAlarmAlertStart is when this alert began: the earliest createdAt of
// its levels, or the alert's own lastUpdate when none of them parses.
//
// Timestamps are display-only, so an unreadable one falls back to the zero
// "never observed" sentinel rather than discarding an alert that is perfectly
// readable otherwise — the trade Parse and ParseAlertsUA both make.
func ukraineAlarmAlertStart(a ukraineAlarmAlert) (time.Time, bool) {
	var earliest time.Time
	for _, l := range a.ActiveAlertLevels {
		t, ok := ukraineAlarmTime(l.CreatedAt)
		if !ok {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if !earliest.IsZero() {
		return earliest, true
	}
	return ukraineAlarmTime(a.LastUpdate)
}

// ukraineAlarmTime reads one timestamp.
//
// Two layouts, because this API is a .NET service and its date-time fields
// arrive both ways: offset-qualified, and bare local time with no zone at
// all. A bare one is read as Kyiv wall-clock, which is what it is — reading
// it as UTC would put every alert three hours in the past on a screen.
func ukraineAlarmTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, Kyiv()); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// watchedOblast is the oblast (or special-status city) this device asks
// about: the one containing the chosen location, or the one named by the
// stored region when no location is chosen — which is what every device ships
// as and what every configuration written before locations existed carries.
//
// A UID the published table no longer lists falls back to the region name for
// the same reason the rest of the device does: broadening announces more than
// was asked for, and refusing announces nothing.
func watchedOblast(locationUID int, region string) (Location, bool) {
	if locationUID != 0 {
		if l, ok := LookupLocation(locationUID); ok {
			if oblast, ok := OblastOf(l); ok {
				return oblast, true
			}
		}
	}
	for _, l := range locations {
		if l.Name != region {
			continue
		}
		if l.Kind == KindOblast || l.Kind == KindSpecialCity {
			return l, true
		}
	}
	return Location{}, false
}

// subtrees indexes the published table by the oblast each location sits in,
// built once. It is read on every poll, and walking 1,622 parent chains every
// fifteen seconds is work the smallest boards here should not repeat when the
// table cannot change while the daemon runs.
var subtrees = sync.OnceValue(func() map[int][]Location {
	out := make(map[int][]Location, 27)
	for _, l := range locations {
		oblast, ok := OblastOf(l)
		if !ok {
			continue
		}
		out[oblast.UID] = append(out[oblast.UID], l)
	}
	return out
})

// oblastSubtree is every location inside one oblast, the oblast itself
// included.
func oblastSubtree(uid int) []Location { return subtrees()[uid] }

// UkraineAlarmRegionID is the ukrainealarm region id for one published
// location UID, and whether this build has one.
//
// Not every location has one, and that is a supported state rather than a
// fault: an unmapped location is one this source cannot serve, which the
// cascade already handles by broadening to a source that can. See
// ukrainealarm_gen.go for what produces the table and why it refuses to guess.
func UkraineAlarmRegionID(uid int) (string, bool) {
	id, ok := ukraineAlarmRegions[uid]
	return id, ok
}

// ukraineAlarmUIDs is the table read backwards, which is the direction a
// parse needs: a payload names region ids and this device thinks in published
// UIDs. Built at package initialisation from the generated table, and kept
// beside it rather than derived on every lookup — a poll resolves one id per
// active alert and a scan of the whole table each time would be work the
// smallest boards here do for nothing.
var ukraineAlarmUIDs = reverseUkraineAlarmRegions(ukraineAlarmRegions)

func reverseUkraineAlarmRegions(m map[int]string) map[string]int {
	out := make(map[string]int, len(m))
	for uid, id := range m {
		out[id] = uid
	}
	return out
}

// UkraineAlarmUID returns the published location UID for a region id the API
// named, and whether this build knows it.
func UkraineAlarmUID(regionID string) (int, bool) {
	uid, ok := ukraineAlarmUIDs[regionID]
	return uid, ok
}
