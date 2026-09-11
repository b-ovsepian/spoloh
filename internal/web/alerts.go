package web

import (
	"net/http"
	"strconv"
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
// It is false today, and that is not a placeholder for something forgotten:
// config.Alerts has no field to keep a token in yet. Until it does, the
// source is not offered — which is the only honest of the two options. Offer
// it without a credential and an operator can build a cascade whose every
// slot fails to authenticate: a device that hears about no air raid at all,
// behind a dashboard showing a complete, ordered, configured-looking list.
// That is the exact failure this screen's other rules exist to prevent.
//
// When the field lands this becomes one field read, and every caller, view
// and test on this screen goes on working unchanged.
func alertsUATokenStored(cfg *config.Config) bool {
	_ = cfg
	return false
}

// selectableSources is the one list this screen offers, validates against and
// sizes its pickers from. Reading it in one place is what keeps a submission
// from being checked against a different list than the one it was offered.
func selectableSources(cfg *config.Config) []string {
	return alertsource.SelectableSources(alertsUATokenStored(cfg))
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
// used".
func buildSourceSlots(current []string, keys []string) []sourceSlotView {
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

// alertsView is what alerts.html reads.
type alertsView struct {
	// Regions is the fixed 25-key list a submitted oblast is validated
	// against, and CurrentRegion is the one already configured — or, on a
	// rejected submission, the one an operator just tried.
	Regions       []string
	CurrentRegion string

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

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once, the same contract every other
	// screen here keeps.
	Error   string
	Message string
}

// alertsBaseView builds the view a fresh GET, a success and a rejection all
// start from, the same shape every other screen here uses.
func (s *Server) alertsBaseView(cfg *config.Config) alertsView {
	return alertsView{
		Regions:             alertsource.Regions(),
		CurrentRegion:       cfg.Alerts.Region,
		SourceSlots:         buildSourceSlots(cfg.Alerts.Sources, selectableSources(cfg)),
		PollIntervalSeconds: pollIntervalSeconds(cfg.Alerts.PollInterval),
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

// handleAlertsSave writes the oblast, the source cascade and the poll
// interval — the three settings that answer where alerts come from, for
// where, and how often they are asked for. Every rule this handler owns
// exists because getting one of them wrong stops the device hearing about an
// air raid while the dashboard still looks configured: an unmatchable region
// (isKnownRegion, moved here unchanged from network.go), an empty cascade
// (parseSources always returns a slice that can only be empty when nothing
// usable was submitted), or an interval fast enough for every source in that
// cascade to rate-limit at once, which is what parsePollInterval refuses.
func (s *Server) handleAlertsSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	cfg := s.deps.Config.Get()
	region := r.FormValue("region")
	sources := parseSources(r.Form["source"], selectableSources(cfg))
	interval, intervalErr := parsePollInterval(r.FormValue("poll_interval"))

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
		s.renderAlertsPage(w, status, func(v *alertsView) {
			v.Error = message
			v.CurrentRegion = selectedRegion
			v.SourceSlots = buildSourceSlots(sources, selectableSources(cfg))
			v.PollIntervalSeconds = pollIntervalSeconds(selectedInterval)
		})
	}

	if !isKnownRegion(region) {
		reject(http.StatusBadRequest, "Оберіть область зі списку.")
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

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.Alerts.Region = region
		c.Alerts.Sources = sources
		c.Alerts.PollInterval = interval
		return nil
	})
	if err != nil {
		s.deps.Logger.Error("alerts: save rejected", "err", err)
		reject(http.StatusBadRequest, "Не вдалося зберегти налаштування сповіщень. Перевірте введені дані.")
		return
	}

	s.renderAlertsPage(w, http.StatusOK, func(v *alertsView) {
		v.Message = "Налаштування сповіщень збережено."
	})
}
