package web

import (
	"net/http"

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

// alertsSlotCount is how many "source N" pickers the page offers — one per
// key alertsource.Sources() names, so an operator can build a cascade using
// every one of them if they want to. A shorter cascade is built by leaving
// the remaining slots on "not used".
var alertsSlotCount = len(alertsource.Sources())

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
}

// buildSourceSlots turns a stored (or just-submitted) cascade into the fixed
// number of position pickers alerts.html renders. current[i] becomes slot
// i's selection; any position past len(current) starts on "not used". This
// is what lets the page show order at all — a set of checkboxes has no way
// to say which one fires first, only which ones are in.
func buildSourceSlots(current []string) []sourceSlotView {
	keys := alertsource.Sources()
	slots := make([]sourceSlotView, alertsSlotCount)
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
		slots[i] = sourceSlotView{Position: i + 1, Options: options}
	}
	return slots
}

// parseSources reads the submitted "source" values — one per position, in
// the order the positions were rendered — into a cascade. A blank slot
// ("not used") and anything that is not one of alertsource.Sources()'s six
// keys are dropped: a crafted form must not be able to fill every slot with
// a key that matches no real upstream, which would silence the device's
// alerts every bit as permanently as an unmatchable region does. A key
// repeated across slots is kept only at the first position it appears in —
// trying the same source twice in one cascade does nothing a single try
// does not. What survives keeps the order it was submitted in: that order
// is the cascade order, the entire reason this is asked for slot by slot
// rather than as an unordered set of checkboxes.
func parseSources(values []string) []string {
	known := make(map[string]bool, len(alertsource.Sources()))
	for _, k := range alertsource.Sources() {
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
		Regions:       alertsource.Regions(),
		CurrentRegion: cfg.Alerts.Region,
		SourceSlots:   buildSourceSlots(cfg.Alerts.Sources),
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

// handleAlertsSave writes the oblast and the source cascade — the two
// settings that answer where alerts come from and for where. Both rules this
// handler owns exist because getting either wrong silences the device
// permanently while the dashboard still looks configured: an unmatchable
// region (isKnownRegion, moved here unchanged from network.go) or an empty
// cascade (parseSources always returns a slice that can only be empty when
// nothing usable was submitted).
func (s *Server) handleAlertsSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	cfg := s.deps.Config.Get()
	region := r.FormValue("region")
	sources := parseSources(r.Form["source"])

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
		s.renderAlertsPage(w, status, func(v *alertsView) {
			v.Error = message
			v.CurrentRegion = selectedRegion
			v.SourceSlots = buildSourceSlots(sources)
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

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.Alerts.Region = region
		c.Alerts.Sources = sources
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
