package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// This is the screen operator feedback asked for: the oblast used to live on
// /network, findable only by someone who already knew it was there, because
// /network was built first and first-run needed the oblast somewhere. The
// oblast is an alert setting, not a network one — see isKnownRegion, moved
// here unchanged from network.go — and the source cascade answers the exact
// same question ("where do alerts come from, and for where"), so it belongs
// beside it rather than nowhere at all.

// isKnownRegion reports whether region is one of alertsource.Regions()'s 25
// keys. A crafted form must not be able to write a region key that matches
// nothing upstream ever reports, which would silence the device permanently
// while the dashboard still looked configured.
func isKnownRegion(region string) bool {
	for _, r := range alertsource.Regions() {
		if r == region {
			return true
		}
	}
	return false
}

// alertsUATokenStored reports whether the operator's alerts.in.ua token is on
// this device, which is what decides whether that source may be chosen at all
// (alertsource.SelectableSources).
//
// The one field read this has always been meant to be. It answers false on
// every device that has not been given a token, and a source whose credential
// is missing is not offered — the only honest of the two options. Offer it
// without a credential and an operator can build a cascade whose every slot
// fails to authenticate: a device that hears about no air raid at all, behind
// a dashboard showing a complete, ordered, configured-looking list. That is
// the exact failure this screen's other rules exist to prevent.
func alertsUATokenStored(cfg *config.Config) bool {
	return cfg.Alerts.AlertsUAToken != ""
}

// selectableSources is the one list this screen offers, validates against and
// sizes its pickers from. Reading it in one place is what keeps a submission
// from being checked against a different list than the one it was offered.
func selectableSources(cfg *config.Config) []string {
	return alertsource.SelectableSources(alertsUATokenStored(cfg))
}

// watch is where this device will look, exactly as the form currently reads:
// the oblast, and the location under it that was picked — zero for "the whole
// oblast", which is what a device with no location chosen has always done.
//
// The two are kept together because the honest answer to "what will this
// source announce for" needs both. They cannot disagree once stored:
// handleAlertsSave refuses a location whose alertsource.OblastOf is not the
// submitted oblast, which is what lets forSource below answer with the
// oblast field itself rather than re-deriving it.
type watch struct {
	Region      string
	LocationUID int
}

// forSource is the sentence-worth of truth the page owes the operator for one
// cascade slot: the place that source will actually report for.
//
// alerts.in.ua is the only provider that understands anything below oblast
// level, so it is the only one that can answer for the chosen location. The
// six Ubilling keys report a boolean per oblast, so a cascade that falls
// through to one of them broadens to the whole oblast — more than was asked
// for, never less, which is the safe direction but a genuine surprise if the
// first time it is noticed is during a raid two districts away. Hence saying
// it here, per source, before it happens.
//
// A UID the table no longer lists falls back to the oblast for the same
// reason the daemon does: broadening announces too much, and refusing
// announces nothing.
func (w watch) forSource(key string) string {
	if key == alertsource.AlertsUAKey {
		if l, ok := alertsource.LookupLocation(w.LocationUID); ok {
			return l.Label
		}
	}
	return w.Region
}

// sourceSlotOption is one <option> inside a single cascade-position <select>.
type sourceSlotOption struct {
	// Key is the value submitted — one of alertsource.Sources()'s six keys,
	// or "" for "not used".
	Key      string
	Selected bool
}

// sourceSlotView is one position in the cascade-order picker: a <select>
// offering every known source plus "not used", defaulting to whichever
// source (if any) currently sits at this position. Position is 1-based and
// computed here rather than in the template, the same way every other label
// on this dashboard (triggerLabel, humanSize, ...) is built in Go.
type sourceSlotView struct {
	Position int
	Options  []sourceSlotOption
	// Source is the chosen key's registry entry (alertsource.Lookup), or the
	// zero value when the slot is "not used". alerts.html shows a
	// documentation link beside the picker only when Source.URL is
	// non-empty: a chosen source with no established URL renders no link at
	// all, the same as an unused slot, rather than a broken one.
	Source alertsource.SourceInfo

	// Watches is the place this slot's source will report for — see
	// watch.forSource. Empty for an unused slot, which renders no line at
	// all rather than a claim about a source nobody chose.
	Watches string
}

// buildSourceSlots turns a stored (or just-submitted) cascade into the
// position pickers alerts.html renders. keys are the sources an operator may
// choose from (alertsource.SelectableSources); current[i] becomes slot i's
// selection, and any position past len(current) starts on "not used". This
// is what lets the page show order at all — a set of checkboxes has no way
// to say which one fires first, only which ones are in.
//
// There is one slot per selectable source, so a cascade can use every one of
// them — and never fewer slots than the stored cascade is long, so a cascade
// saved while a source was selectable is still shown in full if that source
// stops being offered. A shorter cascade is built by leaving slots on "not
// used". w is what each chosen slot's "will watch" line is built from — see
// watch.forSource.
func buildSourceSlots(current []string, keys []string, w watch) []sourceSlotView {
	count := len(keys)
	if len(current) > count {
		count = len(current)
	}
	slots := make([]sourceSlotView, count)
	for i := range slots {
		var picked string
		if i < len(current) {
			picked = current[i]
		}
		options := make([]sourceSlotOption, 0, len(keys)+1)
		options = append(options, sourceSlotOption{Key: "", Selected: picked == ""})
		for _, k := range keys {
			options = append(options, sourceSlotOption{Key: k, Selected: k == picked})
		}
		// alertsource.Lookup("") — the "not used" value — reports false and
		// the zero SourceInfo, whose URL is empty, so an unused slot needs no
		// special case here: it falls out of the same lookup every chosen
		// slot goes through.
		source, _ := alertsource.Lookup(picked)
		slots[i] = sourceSlotView{Position: i + 1, Options: options, Source: source}
		if picked != "" {
			slots[i].Watches = w.forSource(picked)
		}
	}
	return slots
}

// parseSources reads the submitted "source" values — one per position, in
// the order the positions were rendered — into a cascade. keys are the
// sources an operator may choose from, the same list the page offered. A
// blank slot ("not used") and anything that is not one of those keys are
// dropped: a crafted form must not be able to fill every slot with a key
// that matches no usable upstream, which would silence the device's alerts
// every bit as permanently as an unmatchable region does. That includes a
// real registry key whose credential is not stored — it is a source this
// device cannot authenticate to, and a slot holding one does nothing. A key
// repeated across slots is kept only at the first position it appears in —
// trying the same source twice in one cascade does nothing a single try
// does not. What survives keeps the order it was submitted in: that order
// is the cascade order, the entire reason this is asked for slot by slot
// rather than as an unordered set of checkboxes.
func parseSources(values []string, keys []string) []string {
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || !known[v] || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// The two ways parsePollInterval refuses a submission, the model for both
// being events.go's parseVolume. The floor is spelled out as a number of
// seconds because seconds are what the field asks for; the rule itself lives
// in config.MinPollInterval, and this is the operator-facing half of it.
const (
	pollIntervalInvalidMessage  = "Вкажіть інтервал опитування цілим числом секунд."
	pollIntervalTooSmallMessage = "Інтервал опитування має бути не менший за 10 секунд."
)

// parsePollInterval parses and range-checks the "poll_interval" form field,
// which carries whole seconds. An empty return message means it is valid.
//
// The floor is checked here rather than left to the config.Validate that
// config.Store.Update runs, so an operator who asks for two seconds gets a
// sentence naming the actual limit instead of the generic "не вдалося
// зберегти" every rejected Update falls back to. Validate still enforces it —
// the same belt-and-braces parseVolume keeps against config.Validate's 0..100,
// and the reason an unparseable value (an empty submission, most notably)
// is refused outright rather than being allowed to reach the store as Atoi's
// zero.
//
// A number large enough to overflow the multiplication comes out negative,
// which the same floor check refuses.
func parsePollInterval(s string) (config.Duration, string) {
	seconds, err := strconv.Atoi(s)
	if err != nil {
		return 0, pollIntervalInvalidMessage
	}
	d := time.Duration(seconds) * time.Second
	if d < config.MinPollInterval {
		return 0, pollIntervalTooSmallMessage
	}
	return config.Duration(d), ""
}

// pollIntervalSeconds renders a stored interval as the whole seconds the form
// field carries. The form and config.Default() are the only writers and both
// store whole seconds, so this truncates nothing in practice; a hand-edited
// fraction rounds down to the second below, and never below the minimum,
// which is itself a whole number of seconds.
func pollIntervalSeconds(d config.Duration) int {
	return int(d.Std() / time.Second)
}

// locationOptionView is one <option> in the location picker.
type locationOptionView struct {
	UID int
	// Label is alertsource.Location.Label, already disambiguated by the
	// generator where a bare name repeats across the country — several
	// oblasts hold a hromada of the same name, and a picker offering two
	// identical lines is a picker that gets the wrong one chosen.
	Label string
}

// locationGroupView is one <optgroup>: an oblast (or special-status city) and
// every location inside it, in alertsource.SortLocations order.
type locationGroupView struct {
	Oblast    string
	Locations []locationOptionView
}

// locationGroups is the whole picker, built once.
//
// The published table cannot change while the daemon runs, and rebuilding
// 1,622 entries on every render of a screen an operator opens twice a year is
// work no board here should do — least of all the armv6 ones, whose one CPU
// may have a siren to start. The selected entry is not baked in: alerts.html
// compares each UID against the view's own, so one shared slice serves every
// request.
//
// Only locations under an oblast the region picker actually offers are
// included. The published table carries two the 25-key Ubilling list does not
// — an operator who chose one could never satisfy the "inside the chosen
// oblast" rule, so offering it would be offering a dead end. The standing
// rule on this screen is that a picker offers only what a submission may be
// validated against; see selectableSources, which drops a source whose
// credential is missing for the same reason.
var locationGroups = sync.OnceValue(func() []locationGroupView {
	offered := alertsource.Regions()

	ls := alertsource.Locations()
	alertsource.SortLocations(ls)

	var groups []locationGroupView
	for _, l := range ls {
		oblast, ok := alertsource.OblastOf(l)
		if !ok || !slices.Contains(offered, oblast.Name) {
			continue
		}
		if len(groups) == 0 || groups[len(groups)-1].Oblast != oblast.Label {
			groups = append(groups, locationGroupView{Oblast: oblast.Label})
		}
		g := &groups[len(groups)-1]
		g.Locations = append(g.Locations, locationOptionView{UID: l.UID, Label: l.Label})
	}
	return groups
})

// The two ways a submitted location is refused. Each names the field in the
// operator's own language rather than falling back to the generic "не вдалося
// зберегти" every rejected Update ends in — the same contract
// parsePollInterval keeps.
const (
	locationInvalidMessage = "Оберіть населений пункт зі списку."
	locationOutsideMessage = "Обраний населений пункт не належить до обраної області."
)

// parseLocationUID reads the "location" form field and checks it against the
// table it was offered from, the way every operator-choosable value here is.
// An empty return message means it is valid.
//
// An empty field, and the explicit "0", are "the whole oblast" — not an
// error. That is the state every device ships in and most will stay in, and
// it is the only value a config written before the field existed can have.
//
// A UID that is real but sits under a different oblast is refused rather than
// quietly accepted. The oblast field still drives the six Ubilling sources,
// so an operator watching a hromada in one oblast while the fallbacks watch
// another would have a cascade that changes which part of the country it
// announces for depending on which source happened to answer — and would find
// that out during a raid. alertsource.OblastOf is what pairs them.
func parseLocationUID(s, region string) (int, string) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, ""
	}
	uid, err := strconv.Atoi(s)
	if err != nil {
		return 0, locationInvalidMessage
	}
	l, ok := alertsource.LookupLocation(uid)
	if !ok {
		return 0, locationInvalidMessage
	}
	oblast, ok := alertsource.OblastOf(l)
	if !ok || oblast.Name != region {
		return 0, locationOutsideMessage
	}
	return uid, ""
}

// maxAlertsUATokenLen is a sanity bound, not a claim about the format: the
// issued tokens are far shorter, and nothing here should store a megabyte
// because a field accepted whatever was posted at it.
const maxAlertsUATokenLen = 256

const (
	tokenInvalidMessage = "Токен alerts.in.ua має складатися з друкованих символів без пробілів."
	tokenTooLongMessage = "Токен alerts.in.ua задовгий."
)

// parseAlertsUAToken reads the "alerts_ua_token" field. An empty return
// message means it is valid; an empty token means "leave the stored one
// alone".
//
// That asymmetry is deliberate and is the whole reason this is not an
// ordinary text field. The token is never rendered back — leak_test.go walks
// every route to prove it — so a blank box is what an operator sees on every
// visit, and reading blank as "delete" would wipe the credential the first
// time somebody came here to change the poll interval. Removing one is its
// own checkbox, which is a thing a person has to mean.
//
// The character check is not cosmetic. This value is sent as an Authorization
// header, and a control character in a header value is header injection; a
// space or a newline is far more likely to be a paste that picked up the text
// around it than a token any API issued.
func parseAlertsUAToken(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if len(s) > maxAlertsUATokenLen {
		return "", tokenTooLongMessage
	}
	for _, r := range s {
		if r <= ' ' || r > '~' {
			return "", tokenInvalidMessage
		}
	}
	return s, ""
}

// alertsView is what alerts.html reads.
type alertsView struct {
	// Regions is the fixed 25-key list a submitted oblast is validated
	// against, and CurrentRegion is the one already configured — or, on a
	// rejected submission, the one an operator just tried.
	Regions       []string
	CurrentRegion string

	// LocationGroups is the location picker's whole content, shared between
	// every request (see locationGroups), and CurrentLocationUID is the
	// entry to mark selected — 0 for "the whole oblast".
	LocationGroups     []locationGroupView
	CurrentLocationUID int

	// SourceSlots is the cascade-order picker: alertsSlotCount positions,
	// each pre-selected from the stored (or just-submitted) cascade — see
	// buildSourceSlots.
	SourceSlots []sourceSlotView

	// PollIntervalSeconds is how often that cascade is tried, in the whole
	// seconds the form field carries. It sits on this screen because it
	// answers the same question the cascade does — how alert data is
	// fetched — and because how often to ask is exactly what went wrong:
	// polling every 5 seconds had every source rate-limit at once.
	PollIntervalSeconds int

	// TokenStored says whether an alerts.in.ua credential is on this device,
	// and it is deliberately a boolean: the token itself never enters this
	// view, because a view is what gets rendered. leak_test.go walks every
	// route on this server to hold that line.
	TokenStored bool

	// TokenDocsURL is where an operator goes to get one. It comes from the
	// registry rather than being typed into the template, for the reason
	// alertsource.SourceInfo gives: a link this device shows is a claim
	// about where its data comes from, and there is one place that owns
	// those.
	TokenDocsURL string

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once, the same contract every other
	// screen here keeps.
	Error   string
	Message string
}

// alertsBaseView builds the view a fresh GET, a success and a rejection all
// start from, the same shape every other screen here uses.
func (s *Server) alertsBaseView(cfg *config.Config) alertsView {
	stored := watch{Region: cfg.Alerts.Region, LocationUID: cfg.Alerts.LocationUID}
	return alertsView{
		Regions:             alertsource.Regions(),
		CurrentRegion:       cfg.Alerts.Region,
		LocationGroups:      locationGroups(),
		CurrentLocationUID:  cfg.Alerts.LocationUID,
		SourceSlots:         buildSourceSlots(cfg.Alerts.Sources, selectableSources(cfg), stored),
		PollIntervalSeconds: pollIntervalSeconds(cfg.Alerts.PollInterval),
		TokenStored:         alertsUATokenStored(cfg),
		TokenDocsURL:        alertsource.AlertsUADocsURL,
	}
}

// renderAlertsPage re-renders the whole screen from the store's current
// state, then lets mutate adjust the view — a rejection's Error and its own
// re-populated fields, or a success's Message. Every handler here ends
// through this rather than redirecting, the same shape events.go, sounds.go
// and audio.go use.
func (s *Server) renderAlertsPage(w http.ResponseWriter, status int, mutate func(*alertsView)) {
	view := s.alertsBaseView(s.deps.Config.Get())
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "alerts.html", view)
}

// handleAlertsPage renders the alerts screen: the oblast alerts are
// configured for, and the order the upstream cascade is tried in.
func (s *Server) handleAlertsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAlertsPage(w, http.StatusOK, nil)
}

// handleAlertsSave writes the oblast, the location under it, the source
// cascade, the poll interval and the alerts.in.ua token — everything that
// answers where alerts come from, for where, and how often they are asked
// for. Every rule this handler owns exists because getting one of them wrong
// stops the device hearing about an air raid while the dashboard still looks
// configured: an unmatchable region (isKnownRegion, moved here unchanged from
// network.go), a location under some other oblast than the fallbacks watch,
// an empty cascade (parseSources always returns a slice that can only be
// empty when nothing usable was submitted), or an interval fast enough for
// every source in that cascade to rate-limit at once, which is what
// parsePollInterval refuses.
func (s *Server) handleAlertsSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	cfg := s.deps.Config.Get()
	region := r.FormValue("region")
	locationUID, locationErr := parseLocationUID(r.FormValue("location"), region)
	// Validated against the list the page offered, which is the list the
	// stored credential allows. A token arriving in this same submission
	// cannot widen it: with no token stored the <select> has no alerts.in.ua
	// option to pick, so the operator saves the token, the page comes back
	// offering the source, and they choose it. Two steps, and the second one
	// is a choice rather than something that happened to them.
	sources := parseSources(r.Form["source"], selectableSources(cfg))
	interval, intervalErr := parsePollInterval(r.FormValue("poll_interval"))
	token, tokenErr := parseAlertsUAToken(r.FormValue("alerts_ua_token"))
	removeToken := r.FormValue("remove_token") != ""

	reject := func(status int, message string) {
		// A rejected region cannot be re-selected in the <select>, which
		// only ever offers alertsource.Regions()'s 25 keys — there is no
		// option to mark selected for anything else. An invalid submission
		// falls back to what is actually stored, the same choice
		// network.go's renderNetworkRejected made for this exact case
		// before the oblast moved here. A valid region survives a
		// rejection caused by the cascade instead, the same way
		// network.go keeps a valid SSID selected across an unrelated
		// rejection.
		selectedRegion := cfg.Alerts.Region
		if isKnownRegion(region) {
			selectedRegion = region
		}
		// Same rule for the interval, for the same reason: an interval
		// that is itself the cause of the rejection has nothing worth
		// echoing back into a number field — the message already names
		// what was wrong with it — while a perfectly good one must
		// survive a rejection the region or the cascade caused.
		selectedInterval := cfg.Alerts.PollInterval
		if intervalErr == "" {
			selectedInterval = interval
		}
		// And the same for the location, with one addition: a location is
		// only re-selectable next to the oblast it belongs to, so a
		// rejection that fell back to the stored oblast falls back to the
		// stored location with it rather than leaving a pair on screen
		// that this handler would refuse again.
		selected := watch{Region: selectedRegion, LocationUID: locationUID}
		if locationErr != "" || selectedRegion != region {
			selected.LocationUID = 0
			if selectedRegion == cfg.Alerts.Region {
				selected.LocationUID = cfg.Alerts.LocationUID
			}
		}
		s.renderAlertsPage(w, status, func(v *alertsView) {
			v.Error = message
			v.CurrentRegion = selected.Region
			v.CurrentLocationUID = selected.LocationUID
			v.SourceSlots = buildSourceSlots(sources, selectableSources(cfg), selected)
			v.PollIntervalSeconds = pollIntervalSeconds(selectedInterval)
		})
	}

	if !isKnownRegion(region) {
		reject(http.StatusBadRequest, "Оберіть область зі списку.")
		return
	}
	if locationErr != "" {
		reject(http.StatusBadRequest, locationErr)
		return
	}
	if len(sources) == 0 {
		reject(http.StatusBadRequest, "Оберіть принаймні одне джерело сповіщень.")
		return
	}
	if intervalErr != "" {
		reject(http.StatusBadRequest, intervalErr)
		return
	}
	if tokenErr != "" {
		reject(http.StatusBadRequest, tokenErr)
		return
	}
	// Deleting the credential a chosen source authenticates with would leave
	// a cascade slot that can only fail. Dropping alerts.in.ua from the
	// cascade on the operator's behalf would be quieter and worse: they
	// ordered that cascade, and a device silently reordering what it tries
	// is a device nobody can reason about during a raid. So it is refused,
	// and the sentence says which of the two things to undo.
	if removeToken && slices.Contains(sources, alertsource.AlertsUAKey) {
		reject(http.StatusBadRequest, "Спершу приберіть alerts.in.ua з черги джерел, а тоді видаляйте токен.")
		return
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.Alerts.Region = region
		c.Alerts.LocationUID = locationUID
		c.Alerts.Sources = sources
		c.Alerts.PollInterval = interval
		switch {
		case removeToken:
			c.Alerts.AlertsUAToken = ""
		case token != "":
			c.Alerts.AlertsUAToken = token
		}
		return nil
	})
	if err != nil {
		// The token is never in an error this logs, and must never become
		// so: the value is a credential and this line goes to the journal.
		s.deps.Logger.Error("alerts: save rejected", "err", err)
		reject(http.StatusBadRequest, "Не вдалося зберегти налаштування сповіщень. Перевірте введені дані.")
		return
	}

	s.renderAlertsPage(w, http.StatusOK, func(v *alertsView) {
		v.Message = "Налаштування сповіщень збережено."
	})
}
