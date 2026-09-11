package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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

// mixerReporter is the other half of what player.ALSA can tell this page:
// which amixer control, if any, sets the volume on a given output. It is
// asked for by type assertion on Deps.Devices rather than by a second Deps
// field, because main.go already hands the one ALSA value to Deps.Devices
// and a field the wiring has to fill twice is a field it can fill with two
// different things. A lister that cannot answer (a test stub, a future
// implementation) simply does not satisfy it, and the page then says
// nothing about the mixer rather than guessing.
type mixerReporter interface {
	MixerControl(ctx context.Context, device string) (string, error)
}

// The type assertion above is deliberate, but it fails silently: change
// MixerControl's signature on ALSA and this page simply stops naming the
// control, with nothing to say why. This line turns that into a build
// error instead, which is the only way to catch it — a test cannot prove
// a failure whose whole nature is that nothing fails.
var _ mixerReporter = player.ALSA{}

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

	// Current is the plainly worded statement of what the device is using
	// right now, shown above the form.
	Current currentAudioView

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once, the same contract every other
	// screen here keeps.
	Error   string
	Message string
}

// currentAudioView states what the device is playing through right now, in
// words, above the picker.
//
// The operator's report was «Аудіовихід не показується поточний вихід»: a
// radio button carrying a `checked` attribute is not the same thing as
// telling someone what their device is using. It matters most in exactly
// the case a fresh install is in — the stored device is "default", and on
// the operator's board "default" is the HDMI card with nothing plugged
// into it, which is the whole reason nothing plays.
type currentAudioView struct {
	// Device is the stored PCM name, verbatim, because that is the string
	// that has to be recognised in a log line or an aplay command typed
	// over SSH.
	Device string
	// Description is aplay -L's own words for that PCM — what a person
	// actually recognises. Empty when the current list does not name it.
	Description string
	// DefaultCard describes the card "default" resolves to, and is filled
	// in only when the stored device is "default" — the trap this whole
	// block exists for. See defaultCardDescription for how firmly that is
	// known, which is "well enough to point at, not well enough to assert".
	DefaultCard string
	// Missing marks a stored device that aplay -L does not currently
	// report: a USB card unplugged since it was chosen leaves the
	// configuration pointing at a PCM that no longer exists, and until now
	// the page showed that as nothing being selected at all.
	Missing bool

	// Volume is the stored master volume.
	Volume int
	// MixerControl is the amixer control the player discovered for this
	// output, empty when it could not be asked.
	MixerControl string
	// VolumeFixed marks a card with no simple mixer controls at all, where
	// the volume box on this page genuinely does nothing — HDMI, whose
	// volume belongs to the television. Saying so is the point: an
	// operator who turns the number down and hears no change otherwise has
	// no way to tell that from a broken device.
	VolumeFixed bool
}

// defaultCardDescription names the card "default" most likely plays on.
//
// ALSA's default is card 0 unless something in /etc/asound.conf says
// otherwise, and aplay -L lists cards in card-index order, so the first PCM
// it reports that names a card is that card. This is a heuristic and the
// page words it as one — it is not worth a dependency on ALSA's own
// configuration parser to go from "almost always right" to "always right",
// and being pointed at the wrong card costs an operator one glance while
// being pointed at no card at all costs them the bug.
func defaultCardDescription(devices []player.Device) string {
	for _, d := range devices {
		if card := cardOf(d.Name); card != "" {
			if d.Description != "" {
				return d.Description
			}
			return card
		}
	}
	return ""
}

// mixerControl asks the lister which control sets the volume on device. The
// second result is false when there was nobody to ask or the asking failed,
// which is different from a card answering "none": the first means this page
// says nothing about the mixer, the second means it says the volume cannot
// be changed here at all.
func (s *Server) mixerControl(ctx context.Context, device string) (string, bool) {
	reporter, ok := s.deps.Devices.(mixerReporter)
	if !ok {
		return "", false
	}

	lookupCtx, cancel := context.WithTimeout(ctx, deviceListTimeout)
	defer cancel()

	name, err := reporter.MixerControl(lookupCtx, device)
	if err != nil {
		// Warn, not Error: a board whose amixer cannot be reached still
		// renders this page, and the volume box still saves.
		s.deps.Logger.Warn("audio: mixer control lookup failed", "device", device, "err", err)
		return "", false
	}
	return name, true
}

// currentAudio reads the live state the block above the form describes. It
// takes the already-fetched device list for the same reason renderAudioPage
// does: one aplay call per request, not two.
func (s *Server) currentAudio(ctx context.Context, devices []player.Device, notice string) currentAudioView {
	cfg := s.deps.Config.Get()
	cur := currentAudioView{Device: cfg.Audio.Device, Volume: cfg.Audio.MasterVolume}
	// An empty device means aplay runs with no -D at all and ALSA picks;
	// player.ALSA.Play already names that "default" in its errors, and this
	// page must not be the one place that calls the same thing something
	// else.
	if cur.Device == "" {
		cur.Device = "default"
	}

	for _, d := range devices {
		if d.Name == cur.Device {
			cur.Description = d.Description
			break
		}
	}
	// Only a list that was actually obtained can convict a stored device of
	// being gone. A failing or absent lister already has its own notice;
	// adding "your output no longer exists" on top of it would be a second,
	// louder, and quite possibly false alarm.
	cur.Missing = notice == "" && len(devices) > 0 && !deviceKnown(cur.Device, devices)

	if cur.Device == "default" {
		cur.DefaultCard = defaultCardDescription(devices)
	}

	if name, ok := s.mixerControl(ctx, cur.Device); ok {
		cur.MixerControl = name
		cur.VolumeFixed = name == ""
	}
	return cur
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
	//
	// Known gap, not an oversight: this is a string match on "hdmi" in the
	// card id or description, because aplay -L is the only thing this
	// package asks ALSA and it has no dependency-free way to ask whether a
	// display is actually attached. A card whose id and description do not
	// happen to say "hdmi" falls back to aplay's own card-index order —
	// which, on this exact board, is the order that put the broken output
	// first. Fixing that for real needs a signal this package does not
	// have; until then, the ordering is a heuristic that helps the common
	// case and does no harm in the uncommon one.
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
func (s *Server) renderAudioPage(ctx context.Context, w http.ResponseWriter, status int, devices []player.Device, notice, current string, mutate func(*audioView)) {
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
		Current:       s.currentAudio(ctx, devices, notice),
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
	s.renderAudioPage(r.Context(), w, http.StatusOK, devices, notice, current, s.audioFlash(r))
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
// oblast (alerts.go's isKnownRegion), arrived at from the audio side. But
// the lister is only consulted for a device that is actually changing: a
// submission whose device equals what is already stored needs no fresh
// confirmation, because it passed this exact check the moment it was first
// saved. That is what keeps a volume-only change working on a board whose
// ALSA is misbehaving right now — precisely the board whose settings most
// need changing, and the one a stricter rule would otherwise strand. Only a
// genuinely new device asks the lister to vouch for it, and if the lister
// could not be asked at all — nil, its call failed, it timed out, or it
// reported nothing — that new device is refused, using the notice from
// listDevices as the reason.
func (s *Server) handleAudioSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// The test-sound button is a second submission to this same route rather
	// than a route of its own: server.go's table is where routes are
	// registered and this task does not own that file. One endpoint with a
	// named action is the ordinary HTML way to give a form two buttons, and
	// it inherits this handler's session check and same-origin check exactly
	// as written rather than repeating them a second time somewhere else.
	if r.FormValue("action") == "test" {
		s.handleAudioTest(w, r)
		return
	}

	device := r.FormValue("device")
	volume, volumeErr := parseMasterVolume(r.FormValue("master_volume"))
	devices, notice := s.listDevices(r.Context())
	current := s.deps.Config.Get().Audio.Device

	reject := func(status int, message string) {
		s.renderAudioPage(r.Context(), w, status, devices, notice, device, func(v *audioView) {
			v.Error = message
		})
	}

	if volumeErr != "" {
		reject(http.StatusBadRequest, volumeErr)
		return
	}
	if device != current {
		if notice != "" {
			reject(http.StatusServiceUnavailable, notice)
			return
		}
		if !deviceKnown(device, devices) {
			reject(http.StatusBadRequest, fmt.Sprintf("Пристрій «%s» не знайдено серед доступних виходів.", device))
			return
		}
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

	s.renderAudioPage(r.Context(), w, http.StatusOK, devices, notice, device, func(v *audioView) {
		v.Message = "Налаштування звуку збережено."
	})
}

// The test-sound button. An operator changing the output has, until now, had
// no way to find out whether the change worked without leaving this page —
// and on a device whose entire job is to be heard, "did that work" is the
// only question this screen is really being asked.

// testSoundPreference ranks what the button plays. The all-clear first, and
// deliberately not the siren: whoever presses this is standing next to a PA
// system, and a test that sounds like a real air raid to everyone within
// earshot is a test nobody presses twice. It is also the shortest of the
// built-in sounds. A library with neither falls back to whatever it does
// have — see testSound — because a device with a custom library still needs
// its output tested.
var testSoundPreference = []string{"all-clear.wav", "metronome.wav"}

// testSound picks the sound the button plays, or false when the library has
// nothing to play at all.
func (s *Server) testSound() (string, bool) {
	// Library is nil in this package's own tests and on a daemon that could
	// not open its sound directory — same tolerance every other optional
	// dependency here gets.
	if s.deps.Library == nil {
		return "", false
	}
	for _, name := range testSoundPreference {
		if s.deps.Library.Has(name) {
			return name, true
		}
	}
	sounds, err := s.deps.Library.List()
	if err != nil {
		s.deps.Logger.Error("audio: library list for the test sound", "err", err)
		return "", false
	}
	if len(sounds) == 0 {
		return "", false
	}
	return sounds[0].Name, true
}

// testSoundMessage is what the page says after the button queued a sound. It
// stops short of claiming the sound was heard, because this handler cannot
// know that: playback is asynchronous, aplay's own failure arrives later
// through Player.OnError and lands in the event log. What the operator needs
// from this line is what to conclude from their own ears, which is why it
// says what to do if they hear nothing.
const testSoundMessage = "Тестовий звук надіслано на поточний вихід. Якщо нічого не чути — оберіть інший вихід нижче або перегляньте журнал подій."

// testSoundErrors maps the code the redirect carries to the sentence shown.
// A code rather than the sentence itself: a message read out of the query
// string is attacker-controllable, and anyone who can get a logged-in
// operator to open a crafted link must not be able to put words on the
// device's own dashboard. This is the same rule handlers.go's flashMessage
// applies to /play's "played" parameter, reached from the other direction —
// there the name is validated against the library, here nothing crosses the
// query string but a key to text that lives in this file.
var testSoundErrors = map[string]string{
	"unavailable": "Відтворення недоступне на цьому пристрої.",
	"empty":       "У бібліотеці немає жодного звуку, щоб перевірити вихід.",
	"failed":      "Не вдалося запустити тестовий звук. Перегляньте журнал подій.",
}

// audioFlash renders the one-shot notice the test-sound redirect carries
// back, or nil when the request carries none. Query parameters rather than a
// cookie, exactly as /play and /stop do it: the message is meant to be seen
// once, by the browser about to render this response, and never needs to
// survive past it.
func (s *Server) audioFlash(r *http.Request) func(*audioView) {
	q := r.URL.Query()
	switch {
	case q.Has("tested"):
		return func(v *audioView) { v.Message = testSoundMessage }
	case q.Has("test_error"):
		message, ok := testSoundErrors[q.Get("test_error")]
		if !ok {
			return nil
		}
		return func(v *audioView) { v.Error = message }
	}
	return nil
}

// handleAudioTest plays a sound through the stored output, so that changing
// the output can be checked from the page that changed it.
//
// It goes through Engine.PlayManual rather than calling the ALSA backend
// here, for two reasons. The engine is what records an operator's action in
// the event log — the same reason /play and /stop go through it. And the
// player is the device's single voice: a second aplay started behind the
// player's back would, on a card opened exclusively, make the alert that
// arrives during the test fail outright. Queued, the worst this button can
// do is make an air raid announcement wait out a ten-second all-clear.
//
// The outcome comes back as a flash on a redirect — the same mechanism
// /play already uses, not a new one. It is honest about what it can know:
// everything this handler decides (no player wired up, nothing in the
// library, a sound the engine refuses) is reported here and now, while
// aplay's own failure is asynchronous by construction and reaches the
// operator through the event log, which /log already points at this page
// for.
func (s *Server) handleAudioTest(w http.ResponseWriter, r *http.Request) {
	// Engine is nil in this package's own tests and on a daemon with no
	// playback wired up — same guard as handlePlay.
	if s.deps.Engine == nil {
		s.redirectAudio(w, r, "test_error", "unavailable")
		return
	}
	sound, ok := s.testSound()
	if !ok {
		s.redirectAudio(w, r, "test_error", "empty")
		return
	}
	if err := s.deps.Engine.PlayManual(sound, s.deps.Config.Get().Audio.MasterVolume); err != nil {
		s.deps.Logger.Error("audio: test sound rejected", "sound", sound, "err", err)
		s.redirectAudio(w, r, "test_error", "failed")
		return
	}
	s.redirectAudio(w, r, "tested", "1")
}

// redirectAudio sends the browser back to this page carrying one flash
// parameter. 303 so a reload does not re-submit the button.
func (s *Server) redirectAudio(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, "/audio?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}
