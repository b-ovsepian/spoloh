package web

import (
	"fmt"
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

// credential is one provider key this screen stores, and everything that
// differs between them.
//
// There are two now, and they have nothing in common but their shape: a
// stored alerts.in.ua token says nothing about whether ukrainealarm.com can
// be reached. Describing them as data rather than as two copies of the same
// handler is what keeps the rules identical — a blank field leaves the stored
// value alone, removing one is its own checkbox, neither is ever rendered
// back — because there is exactly one implementation of each rule below.
type credential struct {
	// Key is the cascade key this credential unlocks
	// (alertsource.SelectableSources).
	Key string
	// Name is what the operator is told they are typing a key for.
	Name string
	// Field and RemoveField are the form fields: a new value, and the
	// deliberate deletion.
	Field       string
	RemoveField string
	// Label is the field's own label; Help is why this key is worth
	// getting, shown while none is stored; DocsURL and DocsLabel are where
	// it is obtained. The URL comes from the registry rather than being
	// typed here — see alertsource.SourceInfo: a link this device shows is
	// a claim about where its data comes from, and one place owns those.
	Label     string
	Help      string
	DocsURL   string
	DocsLabel string

	get func(*config.Config) string
	set func(*config.Config, string)
}

// credentials is every provider key an operator can store here, in the order
// the registry offers the sources they unlock.
var credentials = []credential{
	{
		Key:         alertsource.AlertsUAKey,
		Name:        "alerts.in.ua",
		Field:       "alerts_ua_token",
		RemoveField: "remove_token",
		Label:       "Токен alerts.in.ua",
		Help:        "Без токена джерело alerts.in.ua недоступне — саме воно повідомляє рівень тривоги й місце точніше за область.",
		DocsURL:     alertsource.AlertsUADocsURL,
		DocsLabel:   "devs.alerts.in.ua",
		get:         func(c *config.Config) string { return c.Alerts.AlertsUAToken },
		set:         func(c *config.Config, v string) { c.Alerts.AlertsUAToken = v },
	},
	{
		Key:         alertsource.UkraineAlarmKey,
		Name:        "ukrainealarm.com",
		Field:       "ukrainealarm_token",
		RemoveField: "remove_ukrainealarm_token",
		Label:       "Ключ ukrainealarm.com",
		Help:        "Без ключа джерело ukrainealarm.com недоступне — воно повідомляє рівень тривоги, червоний або жовтий, і бачить тривоги районів та громад усередині області.",
		DocsURL:     alertsource.UkraineAlarmDocsURL,
		DocsLabel:   "api.ukrainealarm.com",
		get:         func(c *config.Config) string { return c.Alerts.UkraineAlarmToken },
		set:         func(c *config.Config, v string) { c.Alerts.UkraineAlarmToken = v },
	},
}

// storedCredentials names the sources whose credential is on this device,
// which is what decides whether each may be chosen at all
// (alertsource.SelectableSources).
//
// A source whose credential is missing is not offered — the only honest of
// the two options. Offer it anyway and an operator can build a cascade whose
// every slot fails to authenticate: a device that hears about no air raid at
// all, behind a dashboard showing a complete, ordered, configured-looking
// list. That is the exact failure this screen's other rules exist to prevent.
func storedCredentials(cfg *config.Config) []string {
	var keys []string
	for _, c := range credentials {
		if c.get(cfg) != "" {
			keys = append(keys, c.Key)
		}
	}
	return keys
}

// selectableSources is the one list this screen offers, validates against and
// sizes its pickers from. Reading it in one place is what keeps a submission
// from being checked against a different list than the one it was offered.
func selectableSources(cfg *config.Config) []string {
	return alertsource.SelectableSources(storedCredentials(cfg)...)
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
// Two providers understand something below oblast level; the six Ubilling
// keys report a boolean per oblast, so a cascade that falls through to one of
// them broadens to the whole oblast — more than was asked for, never less,
// which is the safe direction but a genuine surprise if the first time it is
// noticed is during a raid two districts away. Hence saying it here, per
// source, before it happens.
//
// ukrainealarm.com is finer-grained only where this build carries its id for
// the place. Without that row it can still watch the oblast and nothing
// smaller, and the line has to say the oblast — claiming the hromada would be
// this screen telling an operator they are covered more precisely than they
// are.
//
// A UID the table no longer lists falls back to the oblast for the same
// reason the daemon does: broadening announces too much, and refusing
// announces nothing.
func (w watch) forSource(key string) string {
	l, ok := alertsource.LookupLocation(w.LocationUID)
	if !ok {
		return w.Region
	}
	switch key {
	case alertsource.AlertsUAKey:
		return l.Label
	case alertsource.UkraineAlarmKey:
		if _, mapped := alertsource.UkraineAlarmRegionID(l.UID); mapped {
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

// maxTokenLen is a sanity bound, not a claim about any provider's format: the
// issued keys are far shorter, and nothing here should store a megabyte
// because a field accepted whatever was posted at it.
const maxTokenLen = 256

const (
	tokenInvalidMessage = "Токен %s має складатися з друкованих символів без пробілів."
	tokenTooLongMessage = "Токен %s задовгий."
)

// parseToken reads one credential field, naming provider in anything it
// refuses. An empty return message means it is valid; an empty token means
// "leave the stored one alone".
//
// That asymmetry is deliberate and is the whole reason these are not ordinary
// text fields. A stored key is never rendered back — leak_test.go walks every
// route to prove it — so a blank box is what an operator sees on every visit,
// and reading blank as "delete" would wipe the credential the first time
// somebody came here to change the poll interval. Removing one is its own
// checkbox, which is a thing a person has to mean.
//
// The character check is not cosmetic. Every one of these values is sent as
// an Authorization header, and a control character in a header value is
// header injection; a space or a newline is far more likely to be a paste
// that picked up the text around it than a key any API issued.
func parseToken(s, provider string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if len(s) > maxTokenLen {
		return "", fmt.Sprintf(tokenTooLongMessage, provider)
	}
	for _, r := range s {
		if r <= ' ' || r > '~' {
			return "", fmt.Sprintf(tokenInvalidMessage, provider)
		}
	}
	return s, ""
}

// tokenSubmission is one credential as this submission leaves it: a new
// value, a deliberate removal, or neither.
type tokenSubmission struct {
	cred   *credential
	value  string
	remove bool
	err    string
}

// parseTokens reads every credential field on the form. All of them, every
// time, so that "what will be stored after this write" can be answered before
// anything is validated against it — see handleAlertsSave.
func parseTokens(r *http.Request) []tokenSubmission {
	out := make([]tokenSubmission, len(credentials))
	for i := range credentials {
		c := &credentials[i]
		value, msg := parseToken(r.FormValue(c.Field), c.Name)
		out[i] = tokenSubmission{
			cred:   c,
			value:  value,
			remove: r.FormValue(c.RemoveField) != "",
			err:    msg,
		}
	}
	return out
}

// after is what this credential will hold once the submission is written.
func (s tokenSubmission) after(cfg *config.Config) string {
	switch {
	case s.remove:
		return ""
	case s.value != "":
		return s.value
	default:
		return s.cred.get(cfg)
	}
}

// tokenFieldView is one credential box on the form. It carries whether a key
// is stored and never the key itself — see alertsView.TokenFields.
type tokenFieldView struct {
	Label       string
	Field       string
	RemoveField string
	Help        string
	DocsURL     string
	DocsLabel   string
	Name        string
	Stored      bool
}

// tokenFields builds those boxes from the stored configuration.
func tokenFields(cfg *config.Config) []tokenFieldView {
	out := make([]tokenFieldView, len(credentials))
	for i, c := range credentials {
		out[i] = tokenFieldView{
			Label:       c.Label,
			Field:       c.Field,
			RemoveField: c.RemoveField,
			Help:        c.Help,
			DocsURL:     c.DocsURL,
			DocsLabel:   c.DocsLabel,
			Name:        c.Name,
			Stored:      c.get(cfg) != "",
		}
	}
	return out
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

	// TokenFields is one box per provider credential this screen stores.
	// Stored is deliberately a boolean: no key itself ever enters this
	// view, because a view is what gets rendered. leak_test.go walks every
	// route on this server to hold that line.
	TokenFields []tokenFieldView

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
		TokenFields:         tokenFields(cfg),
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
	interval, intervalErr := parsePollInterval(r.FormValue("poll_interval"))
	tokens := parseTokens(r)

	// Validated against what the credentials will be after this write, not
	// what they were before it.
	//
	// This used to read the stored config, on the reasoning that a token
	// arriving in the same submission should not widen the choice — the
	// operator would save the token, the page would come back offering the
	// source, and picking it would be a second, deliberate step.
	//
	// On a real device that produced the opposite of care. Submitting a
	// token and the source together silently dropped the source and
	// answered "Налаштування сповіщень збережено." The operator had chosen
	// it, the device had discarded it, and the page said yes — which is the
	// "looks configured, isn't" failure this whole screen exists to prevent.
	// Reproduced before changing anything.
	//
	// Ticking a source is already a deliberate act; it does not become more
	// deliberate for having been done twice.
	var withToken []string
	for _, t := range tokens {
		if t.after(cfg) != "" {
			withToken = append(withToken, t.cred.Key)
		}
	}
	allowed := alertsource.SelectableSources(withToken...)
	sources := parseSources(r.Form["source"], allowed)

	// Nothing this screen accepts may be quietly thrown away. A key that
	// survives the form but not the filter is either a source that needs a
	// credential the operator has not supplied, or a stale page — and both
	// deserve a sentence rather than a success message over a cascade that
	// is not the one they ordered.
	if dropped := droppedSources(r.Form["source"], sources); dropped != "" {
		reject := func(message string) {
			s.renderAlertsPage(w, http.StatusBadRequest, func(v *alertsView) { v.Error = message })
		}
		reject(fmt.Sprintf("Джерело «%s» недоступне без збереженого токена.", dropped))
		return
	}

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
	for _, t := range tokens {
		if t.err != "" {
			reject(http.StatusBadRequest, t.err)
			return
		}
	}
	// Deleting the credential a chosen source authenticates with would leave
	// a cascade slot that can only fail. Dropping that source from the
	// cascade on the operator's behalf would be quieter and worse: they
	// ordered that cascade, and a device silently reordering what it tries
	// is a device nobody can reason about during a raid. So it is refused,
	// and the sentence says which of the two things to undo.
	for _, t := range tokens {
		if t.remove && slices.Contains(sources, t.cred.Key) {
			reject(http.StatusBadRequest, fmt.Sprintf("Спершу приберіть %s з черги джерел, а тоді видаляйте токен.", t.cred.Name))
			return
		}
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.Alerts.Region = region
		c.Alerts.LocationUID = locationUID
		c.Alerts.Sources = sources
		c.Alerts.PollInterval = interval
		for _, t := range tokens {
			switch {
			case t.remove:
				t.cred.set(c, "")
			case t.value != "":
				t.cred.set(c, t.value)
			}
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

// droppedSources names the first submitted key that did not survive
// validation, or "" when every one of them did. Duplicates and blanks do not
// count: parseSources removes those on purpose, and neither is a choice the
// operator would notice losing.
func droppedSources(submitted, kept []string) string {
	keptSet := make(map[string]bool, len(kept))
	for _, k := range kept {
		keptSet[k] = true
	}
	seen := make(map[string]bool, len(submitted))
	for _, v := range submitted {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if !keptSet[v] {
			return v
		}
	}
	return ""
}
