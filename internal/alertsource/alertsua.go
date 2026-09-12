package alertsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AlertsUABaseURL is the alerts.in.ua API root.
const AlertsUABaseURL = "https://api.alerts.in.ua/v1/"

// AlertsUAActivePath is the endpoint this client reads, relative to the root.
//
// Deliberately not one of the "/iot/" endpoints. Those answer with a single
// letter per oblast — A, P or N — which is smaller, simpler, and carries no
// level at all. Carrying the level is the entire reason this provider was
// added, so the heavier endpoint is the only one worth asking.
const AlertsUAActivePath = "alerts/active.json"

// AlertsUADocsURL is where an operator reads about this provider and, more to
// the point, where they get the token it will not work without.
//
// This is the developer documentation, not the provider's front page: it is
// where the token request form and the endpoint reference live, and the
// operator arriving here has one job — get a token. It was fetched and
// confirmed to answer during this work rather than guessed, which is the bar
// for any address this repository links to; DocsURL's comment gives the rest
// of the reasoning about not sending a reader to a JSON payload.
const AlertsUADocsURL = "https://devs.alerts.in.ua/"

// alertsUASourceName is what a snapshot from here calls itself when nothing
// in the payload names the provider. The Ubilling endpoint returns a "source"
// field and the dashboard shows it; this API returns no such field, so the
// name is supplied here rather than leaving the dashboard's live source line
// blank on the one provider an operator had to go and register for.
const alertsUASourceName = "alerts.in.ua"

// ErrNoToken reports that this source was asked to fetch without a token.
//
// It is returned before any request is made. A tokenless request would come
// back 401, which looks in a log exactly like a token that has been revoked,
// and those two need different things from the operator: one is "go and get
// one", the other is "the one you have stopped working".
var ErrNoToken = errors.New("alerts.in.ua: no API token configured")

// ErrTokenRejected reports that the token was sent and refused — expired,
// revoked, or mistyped when it was entered.
//
// A sentinel rather than a bare message so that SourceAttempt.Reason can tell
// this failure apart from an unreachable host without reading the error's
// text. The operator's next step is different for each, and the text itself
// must never reach a page: see Reason.
var ErrTokenRejected = errors.New("alerts.in.ua rejected the API token")

// AlertsUA fetches active alerts from alerts.in.ua.
//
// Rate limits, from the API's own documentation: a soft limit of 8–10
// requests a minute per public IP, a hard limit of 12, and 429 above it.
// Spoloh's poll floor is 10 seconds — 6 requests a minute — and its default
// is 15 seconds, or 4. So even an operator who asks for the fastest polling
// the form allows stays inside the soft limit, which is why the floor is
// where it is and why lowering it "only a little" is not a small change.
// The limit is per IP, not per token, so several devices on one connection
// share the budget; see RateLimitError.
//
// If-Modified-Since is the other half of staying inside it. This endpoint
// answers 304 to a conditional request while nothing has changed, which for
// most of most days is every request — so the cost of asking every 15 seconds
// is a handful of bytes rather than the whole country's alert list.
//
// Zero value is not usable; call NewAlertsUA.
type AlertsUA struct {
	baseURL string
	token   string
	hc      *http.Client

	// mu guards the conditional-request cache below and serialises Fetch.
	// The poll loop is the only caller today, but a dashboard control that
	// tests a source would be a second one, and two in-flight requests
	// racing over lastModified could store a header belonging to a body
	// that was never the one cached.
	mu sync.Mutex
	// lastModified is the previous response's Last-Modified header, sent
	// back as If-Modified-Since, and cached is the body it arrived with.
	// The body is kept rather than the parsed Snapshot so that a 304
	// re-parses into a fresh map every time: handing the same map to two
	// callers would make one caller's bookkeeping visible to the other.
	lastModified string
	cached       []byte
}

// NewAlertsUA builds a source reading from baseURL with token. baseURL is a
// parameter so tests can point it at a loopback server; production passes
// AlertsUABaseURL.
func NewAlertsUA(baseURL, token string, hc *http.Client) *AlertsUA {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	if baseURL == "" {
		baseURL = AlertsUABaseURL
	}
	return &AlertsUA{baseURL: baseURL, token: token, hc: hc}
}

// HasToken reports whether this source has a credential to fetch with. It
// exists so a caller can leave the source out of a cascade it could only
// fail in, rather than finding out one poll at a time.
func (c *AlertsUA) HasToken() bool { return c.token != "" }

// Fetch returns the current country-wide alert picture.
//
// No error from here ever contains the token. It is a credential: it does not
// belong in a log, in a wrapped error, or on the page an operator is looking
// at when the fetch fails.
func (c *AlertsUA) Fetch(ctx context.Context) (Snapshot, error) {
	if c.token == "" {
		return Snapshot{}, ErrNoToken
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	url := strings.TrimSuffix(c.baseURL, "/") + "/" + AlertsUAActivePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if c.lastModified != "" {
		req.Header.Set("If-Modified-Since", c.lastModified)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if c.cached == nil {
			// A 304 is only ever an answer to a conditional request, and
			// we only make one after a body we kept — so this cannot
			// happen against a correct server. It must still be an error
			// rather than an empty snapshot: an empty snapshot means "no
			// alerts anywhere", and returning that here would end a raid
			// that is still running.
			return Snapshot{}, errors.New("alerts.in.ua: 304 with nothing cached to return")
		}
		return ParseAlertsUA(c.cached)
	case http.StatusTooManyRequests:
		return Snapshot{}, &RateLimitError{RetryAfter: retryAfter(resp)}
	case http.StatusUnauthorized, http.StatusForbidden:
		return Snapshot{}, fmt.Errorf("%w (http %d)", ErrTokenRejected, resp.StatusCode)
	case http.StatusOK:
	default:
		return Snapshot{}, fmt.Errorf("http %d", resp.StatusCode)
	}

	// A 200 carrying HTML is an upstream error or captive-portal page.
	// Parsing it as state would be worse than reporting a failure.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return Snapshot{}, fmt.Errorf("unexpected content type %q", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := ParseAlertsUA(body)
	if err != nil {
		return Snapshot{}, err
	}
	// Only a body that parsed is worth revalidating against later: caching
	// one that did not would make every following poll a 304 returning the
	// same unparseable bytes.
	c.cached = body
	c.lastModified = resp.Header.Get("Last-Modified")
	return snap, nil
}

// alertsUAAlert is one Alert object from the documented payload. threats[] is
// read by nothing and named by nothing here on purpose: it carries a
// per-threat breakdown (drones, missiles, and a level and free-text source
// message for each) that this device has no announcement to make about, and
// silently dropping a documented field is how the next reader concludes it
// does not exist.
type alertsUAAlert struct {
	ID            int    `json:"id"`
	LocationTitle string `json:"location_title"`
	LocationType  string `json:"location_type"`
	LocationUID   string `json:"location_uid"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	AlertType     string `json:"alert_type"`
	AlertLevel    string `json:"alert_level"`
}

// alertsUABody is the documented envelope. Alerts is a pointer so that "the
// key was absent" can be told apart from "the key was there and empty" — the
// difference between a payload we do not understand and a country with no
// active alerts, which is the difference between reporting a failure and
// reporting an all-clear.
type alertsUABody struct {
	Alerts *[]alertsUAAlert `json:"alerts"`
	Meta   struct {
		LastUpdatedAt string `json:"last_updated_at"`
	} `json:"meta"`
}

// ParseAlertsUA converts an active-alerts body into a Snapshot.
//
// The result is a complete picture of the country, not just of the places
// under alert: every oblast and special-status city in the published location
// table is seeded calm, and the payload's alerts are laid over it. That is
// what the endpoint actually means — it lists what is active, so a location
// it does not list is not under alert — and it is what makes a snapshot from
// here interchangeable with one from the Ubilling endpoint, which reports all
// 25 oblasts with a boolean each. A consumer must not have to know which
// provider answered.
//
// An alert for a location below oblast level is added under its own title as
// well, so a hromada-level warning survives the fetch intact. Nothing here
// resolves a hromada's alert up to its oblast or down from it; that is the
// consumer's decision and belongs where the choice of location is made.
//
// A body whose shape is not recognised is an error, never an empty snapshot.
// An empty snapshot from here means "all clear", and the one thing this parse
// must never do is turn an unreadable payload into a country-wide all-clear
// while a raid is running.
func ParseAlertsUA(data []byte) (Snapshot, error) {
	var body alertsUABody
	if err := json.Unmarshal(data, &body); err != nil {
		return Snapshot{}, fmt.Errorf("parse alerts.in.ua snapshot: %w", err)
	}
	if body.Alerts == nil {
		return Snapshot{}, errors.New("parse alerts.in.ua snapshot: payload carries no alerts array")
	}

	snap := Snapshot{
		Source: alertsUASourceName,
		States: make(map[string]RegionState, len(locations)),
	}
	// Every published location, not only the oblasts.
	//
	// Seeding oblasts alone made a calm hromada indistinguishable from one
	// this build does not know: both were absent, and engine.resolveWatched
	// reads absent as "this provider cannot serve that granularity" and
	// broadens to the oblast. So a device set to watch a hromada silently
	// watched the whole oblast for as long as the hromada was calm, and
	// narrowed only in the moments it was not — the opposite of what an
	// operator asks for by choosing one.
	for _, l := range locations {
		snap.States[l.Name] = RegionState{Region: l.Name, UID: l.UID}
	}
	if body.Meta.LastUpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, body.Meta.LastUpdatedAt); err == nil {
			snap.CachedAt = t
		}
	}

	for _, a := range *body.Alerts {
		// active.json is documented to return only alerts still running, so
		// finished_at is null here. Honour it anyway if it ever is not:
		// trusting the endpoint's name over the payload's own field would
		// announce an alert upstream has already recorded as over.
		if a.FinishedAt != "" {
			continue
		}

		uid, _ := strconv.Atoi(a.LocationUID)
		name := a.LocationTitle
		if name == "" {
			// A title is what a state is keyed by. When it is missing the
			// UID can still name the place, because the table this build
			// ships is the same published table the UIDs come from.
			if l, ok := LookupLocation(uid); ok {
				name = l.Name
			}
		}
		if name == "" {
			// Neither a title nor a UID we recognise: there is no location
			// an operator could have chosen that this could belong to, so
			// it can neither raise nor silence anything.
			continue
		}

		st := RegionState{
			Region: name,
			Alert:  true,
			Level:  ParseLevel(a.AlertLevel),
			Type:   a.AlertType,
			UID:    uid,
		}
		// Timestamps are display-only, so an unreadable one falls back to
		// the zero "never observed" sentinel rather than discarding an
		// alert that is perfectly readable otherwise — the same trade
		// Parse makes, for the same reason.
		if t, err := time.Parse(time.RFC3339, a.StartedAt); err == nil {
			st.Changed = t
		}

		// One location can be named by more than one active alert — an air
		// raid and an artillery warning over the same place. They collapse
		// into one state: the more severe level wins, the earlier start
		// wins because that is when this location's alert actually began,
		// and the first type seen wins because nothing reads it and a
		// stable answer beats a clever one.
		if prev, ok := snap.States[name]; ok && prev.Alert {
			st.Level = moreSevere(prev.Level, st.Level)
			if !prev.Changed.IsZero() && (st.Changed.IsZero() || prev.Changed.Before(st.Changed)) {
				st.Changed = prev.Changed
			}
			if prev.Type != "" {
				st.Type = prev.Type
			}
			if st.UID == 0 {
				st.UID = prev.UID
			}
		}
		snap.States[name] = st
	}

	propagateDownward(&snap)
	return snap, nil
}

// propagateDownward raises every location inside one that is already raised.
//
// Alerts are declared at whatever level the authorities announce them, and on
// a real board that is usually the raion: «Шосткинський район», red, air raid,
// with nothing on the oblast above it and nothing on the hromadas below. A
// device watching «м. Шостка та Шосткинська територіальна громада» read no
// alert for that UID, fell back to the oblast, found it calm, and reported
// calm — while the raion containing Shostka was under a red air raid alert.
// Observed on the operator's own device, against the live API.
//
// Containment is not a heuristic here. A raion under alert means every
// hromada in it is under alert; that is what the declaration says. So the
// state of a location is the most severe of its own and every ancestor's.
//
// It does not run the other way. A single hromada raised does not raise the
// oblast around it, because an oblast watcher hearing a siren for a raid two
// districts away would learn to ignore the siren — and the /iot endpoints'
// own vocabulary separates "alert across the whole location" from "alert in
// part of it" for the same reason. An operator who wants every part of their
// oblast picks the oblast, which upstream raises when it declares one.
func propagateDownward(snap *Snapshot) {
	raised := make(map[int]RegionState, len(snap.States))
	for _, st := range snap.States {
		if st.Alert && st.UID != 0 {
			raised[st.UID] = st
		}
	}
	if len(raised) == 0 {
		return
	}

	for name, st := range snap.States {
		l, ok := LookupLocation(st.UID)
		if !ok {
			continue
		}
		// Walk up to the oblast, taking the most severe thing found. The
		// bound is the table's own size: the generated parent links form a
		// tree, but a future hand-edit that made a cycle must not hang a
		// poll.
		for hops := 0; l.Parent != 0 && hops < len(locations); hops++ {
			parent, ok := LookupLocation(l.Parent)
			if !ok {
				break
			}
			l = parent
			anc, ok := raised[l.UID]
			if !ok {
				continue
			}
			if !st.Alert {
				// Inherit the ancestor's alert wholesale, including when it
				// started: this location has been under it since it was
				// declared, not since this poll noticed.
				st.Alert, st.Level, st.Type, st.Changed = true, anc.Level, anc.Type, anc.Changed
				continue
			}
			st.Level = moreSevere(st.Level, anc.Level)
			if !anc.Changed.IsZero() && (st.Changed.IsZero() || anc.Changed.Before(st.Changed)) {
				st.Changed = anc.Changed
			}
		}
		snap.States[name] = st
	}
}
