package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/player"
)

// This is the screen a freshly installed device needs before any other one
// on this dashboard: on the real board `default` resolves to card 0, which
// is HDMI, and with nothing plugged into HDMI playback fails outright — the
// very first thing a new device does is fail to make a sound. /audio is
// what moves it to an output that actually works, and — see
// handleAudioSave — the only thing standing between a crafted form and a
// PCM that does not exist, which would silence the device while the
// dashboard still looked configured.

// DeviceLister is what player.ALSA.Devices satisfies. A narrow interface
// rather than *player.ALSA itself, so a test can script the list — a slow
// board, a failing aplay, an empty one — without ever running aplay.
type DeviceLister interface {
	Devices(ctx context.Context) ([]player.Device, error)
}

// deviceListTimeout bounds the aplay call behind this page. A person is
// watching a phone; a page that lists no devices with a line saying why is
// better than one that never loads.
const deviceListTimeout = 10 * time.Second

// deviceRowView is one row of the picker — either the grouped list (one per
// card, plus "default") or the full disclosure list (every PCM aplay -L
// reported). Both share the same shape: a value to submit, a label to
// read, and whether it is the one currently configured (or, on a rejected
// submission, the one an operator just tried — see renderAudioPage).
type deviceRowView struct {
	Value    string
	Label    string
	Selected bool
}

// audioView is what audio.html reads.
type audioView struct {
	// Groups is the grouped picker: "default" first (only when the lister
	// actually reported one — never synthesised), then one row per
	// physical card. Eleven of the thirteen PCMs the real board reports are
	// indistinguishable from each other by description alone — see
	// groupByCard — so this, not Options, is what an operator actually
	// chooses from.
	Groups []deviceRowView
	// Options is every PCM aplay -L reported, ungrouped, in its own order —
	// reachable behind a disclosure for the case a USB card needs a
	// specific one the grouped picker does not expose. Never filtered away.
	Options []deviceRowView
	// OptionsOpen marks the disclosure open by default when the configured
	// device (or the one an operator just tried) is only reachable through
	// Options — a selection hidden inside a closed <details> would look, to
	// the operator, like nothing at all is selected.
	OptionsOpen bool
	// DevicesNotice explains an empty Groups/Options: no lister wired up,
	// its call failing or timing out, or an empty report — so a board with
	// broken ALSA still shows the rest of the page rather than erroring
	// outright.
	DevicesNotice string

	MasterVolume int

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once, the same contract every other
	// screen here keeps.
	Error   string
	Message string
}

// masterVolumeMessage is parseMasterVolume's one rejection sentence. Unlike
// events.go's parseVolume, master volume has no "disable the event
// instead" alternative to single out 0 for, so every failure — blank,
// unparseable, zero, out of range — collapses to the one message.
const masterVolumeMessage = "Вкажіть гучність числом від 1 до 100."

// parseMasterVolume parses and range-checks the "master_volume" form field.
// An empty returned string means volume is valid.
func parseMasterVolume(s string) (int, string) {
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 100 {
		return 0, masterVolumeMessage
	}
	return v, ""
}

// cardOf extracts the ALSA CARD= value from a PCM name — "Headphones" from
// "plughw:CARD=Headphones,DEV=0" — or "" when the name carries none, which
// is true of exactly "default" and "sysdefault".
func cardOf(name string) string {
	idx := strings.Index(name, "CARD=")
	if idx < 0 {
		return ""
	}
	rest := name[idx+len("CARD="):]
	if c := strings.IndexByte(rest, ','); c >= 0 {
		rest = rest[:c]
	}
	return rest
}

// cardGroup is one physical output: every PCM aplay -L reported for one
// card, collapsed to the single PCM this picker submits.
type cardGroup struct {
	Card        string
	Description string
	// Device is the PCM name a selection of this row writes to
	// config.Audio.Device: the card's plughw variant when it offers one —
	// on the real board, the one that plays where the raw hw device and the
	// system default both fail, because plughw applies ALSA's own format
	// and rate conversion where hw demands an exact match the source file
	// may not have. Falls back to the first PCM this card reported when it
	// offers no plughw variant at all.
	Device string
	// HDMI marks a card whose id or description names it as an HDMI
	// output. It exists to order the picker, not to hide anything: HDMI
	// audio needs a synced display, which a headless board like this one
	// routinely does not have, so a card in this category is sorted after
	// every other kind of output rather than left in aplay's own
	// card-index order — which, on the real board, puts the one output
	// that does not work first.
	HDMI bool
}

// groupByCard collapses devices into one row per physical card, in the
// order each card was first seen, with any HDMI-labelled card sorted after
// every other kind. Devices reporting no card ("default", "sysdefault") are
// not included here: "default" is offered separately (see buildDeviceRows),
// and "sysdefault" names no particular output either, so it is left to the
// full list behind the disclosure.
func groupByCard(devices []player.Device) []cardGroup {
	var order []string
	byCard := map[string]*cardGroup{}
	for _, d := range devices {
		card := cardOf(d.Name)
		if card == "" {
			continue
		}
		g, ok := byCard[card]
		if !ok {
			g = &cardGroup{Card: card}
			byCard[card] = g
			order = append(order, card)
		}
		if g.Description == "" {
			g.Description = d.Description
		}
		if g.Device == "" || strings.HasPrefix(d.Name, "plughw:") {
			g.Device = d.Name
		}
		if strings.Contains(strings.ToLower(card+" "+d.Description), "hdmi") {
			g.HDMI = true
		}
	}
	groups := make([]cardGroup, 0, len(order))
	for _, c := range order {
		groups = append(groups, *byCard[c])
	}
	sort.SliceStable(groups, func(i, j int) bool { return !groups[i].HDMI && groups[j].HDMI })
	return groups
}

// buildDeviceRows turns the raw device list into the two views audio.html
// offers: the grouped picker and the full disclosure list. current is what
// config.Audio.Device holds, or what an operator just tried on a rejected
// submission — see renderAudioPage — and marks the matching row selected in
// both.
func buildDeviceRows(devices []player.Device, current string) (groups []deviceRowView, options []deviceRowView) {
	for _, d := range devices {
		if d.Name != "default" {
			continue
		}
		label := "За замовчуванням"
		if d.Description != "" {
			label = "За замовчуванням (" + d.Description + ")"
		}
		groups = append(groups, deviceRowView{Value: "default", Label: label, Selected: current == "default"})
		break
	}

	for _, g := range groupByCard(devices) {
		label := g.Description
		if label == "" {
			label = g.Card
		}
		groups = append(groups, deviceRowView{Value: g.Device, Label: label, Selected: g.Device == current})
	}

	options = make([]deviceRowView, len(devices))
	for i, d := range devices {
		label := d.Name
		if d.Description != "" {
			label = d.Name + " — " + d.Description
		}
		options[i] = deviceRowView{Value: d.Name, Label: label, Selected: d.Name == current}
	}
	return groups, options
}

// listDevices runs the lister behind deviceListTimeout and turns every way
// it can come up empty — no lister wired up, the call failing, the bound
// expiring, or an empty report — into a Ukrainian notice rather than an
// error page, the same shape network.go's scanNetworks gives a broken
// netmgr.Manager.
func (s *Server) listDevices(ctx context.Context) ([]player.Device, string) {
	if s.deps.Devices == nil {
		return nil, "Список аудіопристроїв недоступний на цьому пристрої."
	}

	listCtx, cancel := context.WithTimeout(ctx, deviceListTimeout)
	defer cancel()

	devices, err := s.deps.Devices.Devices(listCtx)
	if err != nil {
		s.deps.Logger.Warn("audio: device list failed", "err", err)
		return nil, "Не вдалося отримати список аудіопристроїв."
	}
	if len(devices) == 0 {
		return nil, "Пристрій не повідомив жодного аудіовиходу."
	}
	return devices, ""
}

// renderAudioPage re-renders the whole screen from an already-fetched
// device list, then lets mutate adjust the view — a rejection's Error or a
// success's Message. devices/notice are passed in rather than re-fetched
// here: handleAudioSave already needs one aplay call to validate the
// submission, and a second one just to re-render would double the wait an
// operator on a phone is watching.
func (s *Server) renderAudioPage(w http.ResponseWriter, status int, devices []player.Device, notice, current string, mutate func(*audioView)) {
	cfg := s.deps.Config.Get()
	groups, options := buildDeviceRows(devices, current)

	groupSelected, optionsSelected := false, false
	for _, g := range groups {
		if g.Selected {
			groupSelected = true
			break
		}
	}
	for _, o := range options {
		if o.Selected {
			optionsSelected = true
			break
		}
	}

	view := audioView{
		Groups:        groups,
		Options:       options,
		OptionsOpen:   optionsSelected && !groupSelected,
		DevicesNotice: notice,
		MasterVolume:  cfg.Audio.MasterVolume,
	}
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "audio.html", view)
}

// handleAudioPage renders the audio screen: where the sound comes out, and
// how loud.
func (s *Server) handleAudioPage(w http.ResponseWriter, r *http.Request) {
	devices, notice := s.listDevices(r.Context())
	current := s.deps.Config.Get().Audio.Device
	s.renderAudioPage(w, http.StatusOK, devices, notice, current, nil)
}

// deviceKnown reports whether name is one Devices actually reported. This
// is the one rule this screen owns — see handleAudioSave.
func deviceKnown(name string, devices []player.Device) bool {
	for _, d := range devices {
		if d.Name == name {
			return true
		}
	}
	return false
}

// handleAudioSave writes the chosen output device and master volume.
//
// A PCM this board does not have silences the device while the dashboard
// shows a configured output — the same shape of failure as an unmatchable
// oblast (network.go's isKnownRegion), arrived at from the audio side. When
// the lister itself could not be asked at all — nil, its call failed, it
// timed out, or it reported nothing — nothing can be confirmed as real, so
// nothing is accepted either: the notice from listDevices doubles as the
// rejection reason, and this is checked before the volume so a broken
// lister is what the operator is told about, not an unrelated typo.
func (s *Server) handleAudioSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	device := r.FormValue("device")
	volume, volumeErr := parseMasterVolume(r.FormValue("master_volume"))
	devices, notice := s.listDevices(r.Context())

	reject := func(status int, message string) {
		s.renderAudioPage(w, status, devices, notice, device, func(v *audioView) {
			v.Error = message
		})
	}

	if notice != "" {
		reject(http.StatusServiceUnavailable, notice)
		return
	}
	if volumeErr != "" {
		reject(http.StatusBadRequest, volumeErr)
		return
	}
	if !deviceKnown(device, devices) {
		reject(http.StatusBadRequest, fmt.Sprintf("Пристрій «%s» не знайдено серед доступних виходів.", device))
		return
	}

	err := s.deps.Config.Update(func(c *config.Config) error {
		c.Audio.Device = device
		c.Audio.MasterVolume = volume
		return nil
	})
	if err != nil {
		s.deps.Logger.Error("audio: save rejected", "err", err)
		reject(http.StatusBadRequest, "Не вдалося зберегти налаштування звуку. Перевірте введені дані.")
		return
	}

	s.renderAudioPage(w, http.StatusOK, devices, notice, device, func(v *audioView) {
		v.Message = "Налаштування звуку збережено."
	})
}
