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

// deviceProber is the third thing player.ALSA can tell this page, and the
// only one that is not a description: whether an output can actually be
// opened. Reached by type assertion on Deps.Devices for exactly the reason
// mixerReporter is — main.go hands the one ALSA value to Deps.Devices, and
// a second field the wiring must fill is a field it can fill with something
// else. A lister that cannot answer simply does not satisfy it, and the
// page then says nothing about whether an output works rather than
// guessing, which is the state this screen was in before.
type deviceProber interface {
	ProbeDevice(ctx context.Context, device string) (player.ProbeResult, error)
}

// The type assertions above are deliberate, but they fail silently: change
// MixerControl's or ProbeDevice's signature on ALSA and this page simply
// stops naming the control, or stops saying which outputs work, with
// nothing to say why. These lines turn that into a build error instead,
// which is the only way to catch it — a test cannot prove a failure whose
// whole nature is that nothing fails.
var (
	_ mixerReporter = player.ALSA{}
	_ deviceProber  = player.ALSA{}
)

// deviceListTimeout bounds the aplay call behind this page. A person is
// watching a phone; a page that lists no devices with a line saying why is
// better than one that never loads.
const deviceListTimeout = 10 * time.Second

// probeBudget bounds every probe this page runs put together. Each single
// probe carries its own deadline inside player.ProbeDevice; this is the
// second bound, over the whole set, so that a board where several cards are
// wedged at once cannot multiply those deadlines into a page load nobody
// waits out. A row left unprobed when the budget runs out says nothing
// about its output, which is the honest thing for it to say.
const probeBudget = 10 * time.Second

// deviceRowView is one row of the picker — either the grouped list (one per
// card, plus "default") or the full disclosure list (every PCM aplay -L
// reported). Both share the same shape: a value to submit, a label to
// read, and whether it is the one currently configured (or, on a rejected
// submission, the one an operator just tried — see renderAudioPage).
type deviceRowView struct {
	Value string
	// Label is what the thing is, in words an operator recognises — "HDMI",
	// "Роз'єм для навушників (3,5 мм)" — for the grouped rows, and the raw
	// PCM name for the full list, which is what that list is for.
	Label string
	// Detail is the grouped rows' smaller second line: the PCM name that
	// goes to aplay and appears in the log, plus the card's own words. The
	// friendly label alone would make the grouped picker and the full list
	// impossible to match up against each other, and impossible to match
	// against an error line in the event log.
	Detail   string
	Selected bool
	// HDMI marks a row whose card names itself as HDMI. It decides which
	// sentence an unusable row gets, not whether the row is shown.
	HDMI bool
	// Probed marks a row whose output was actually opened — or refused to
	// open — during this request. False means nothing was asked (no prober
	// wired up, or the budget ran out), and the row must then claim
	// nothing either way.
	Probed bool
	// Unusable marks a probed row whose output could not be opened right
	// now. The row stays selectable regardless: a board whose HDMI sink is
	// plugged in an hour from now must not have been permanently talked
	// out of it.
	Unusable bool
	// Note is the sentence under an unusable row, in terms an operator can
	// act on — a cable, a screen, an amplifier — never aplay's own
	// "Unknown error 524". Computed for every unusable row, but audio.html
	// renders it only for the one that is Selected: on the operator's board
	// two of three rows are unusable for the same reason (HDMI, and the
	// "default" that resolves to it), and repeating this paragraph on both
	// was the "не дублювати" half of their report. Every unusable row still
	// gets the bare "Не працює зараз." so a candidate that cannot play is
	// not hidden — only the explanation is reserved for the row that
	// matters.
	Note string
}

// probeOutcome is what one probe said, reduced to what this page renders. A
// device absent from a probe map was not asked about; a device present with
// Playable false was asked and could not be opened.
type probeOutcome struct {
	Playable bool
	TimedOut bool
}

// probeOutputs opens each of names for real and reports what happened, one
// probe per distinct name per request.
//
// Nothing is remembered between requests, on purpose: player.ProbeDevice
// explains why at length, and the short of it is that whether an output
// opens is a fact about cables and amplifiers, not about the driver. What
// is shared is this map, within the one request — the grouped rows, the
// live-state block and a save's confirmation are all answering the same
// question at the same instant, and asking ALSA three times would be three
// different answers to it as well as three needless playbacks.
func (s *Server) probeOutputs(ctx context.Context, names []string) map[string]probeOutcome {
	prober, ok := s.deps.Devices.(deviceProber)
	if !ok {
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()

	out := map[string]probeOutcome{}
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, done := out[name]; done {
			continue
		}
		if probeCtx.Err() != nil {
			break
		}

		res, err := prober.ProbeDevice(probeCtx, name)
		if probeCtx.Err() != nil {
			// The budget for the whole page ran out, not this device's own
			// deadline. That is a fact about how many outputs this board
			// has, and says nothing about this one.
			break
		}
		if err != nil {
			// The probe could not be carried out at all — no temp space to
			// write the silence to. Warn and leave the row unprobed; a row
			// that claims an output is broken on the strength of a full
			// disk is worse than one that claims nothing.
			s.deps.Logger.Warn("audio: output probe failed", "device", name, "err", err)
			continue
		}
		out[name] = probeOutcome{Playable: res.Playable, TimedOut: res.TimedOut}
		if !res.Playable {
			// aplay's own diagnostic reaches the operator here and not on
			// the page: "Unknown error 524" is a sentence about a kernel
			// constant, and /log is where those belong.
			s.deps.Logger.Warn("audio: output could not be opened", "device", name, "detail", res.Detail)
		}
	}
	return out
}

// unusableNote words why an output could not be opened, in something an
// operator standing at the board can do something about.
func unusableNote(hdmi, timedOut bool) string {
	switch {
	case timedOut:
		return "Цей вихід не відповів вчасно. Можливо, його зайняла інша програма або карта не відповідає."
	case hdmi:
		return "HDMI передає звук лише тоді, коли під'єднано екран або підсилювач, який цей звук приймає. Якщо колонки в роз'ємі для навушників, оберіть його."
	default:
		return "Не вдалося відкрити цей вихід зараз. Перевірте, чи під'єднано та увімкнено пристрій."
	}
}

// applyProbes writes what the probes found onto the rows they describe.
func applyProbes(rows []deviceRowView, probes map[string]probeOutcome) {
	for i := range rows {
		outcome, asked := probes[rows[i].Value]
		if !asked {
			continue
		}
		rows[i].Probed = true
		rows[i].Unusable = !outcome.Playable
		if rows[i].Unusable {
			rows[i].Note = unusableNote(rows[i].HDMI, outcome.TimedOut)
		}
	}
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
	// Warning is the third thing a save can have to say, and the reason
	// this screen needs one where the others do not: a chosen output that
	// could not be opened is stored all the same — the operator may be
	// wiring the speakers next — so the submission was neither rejected
	// nor plainly fine. Saying only "збережено" there is the whole bug
	// this task exists for, in one word. It pairs with Message; it never
	// pairs with Error, which means nothing was stored at all.
	Warning string
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

	// Probed, Unusable and Note say whether this very output opened when
	// the page asked it to, a moment ago. Being listed and being openable
	// are different facts and this screen used to show only the first: the
	// stored output on a fresh install is "default", which this board
	// reports happily and cannot play through at all.
	Probed   bool
	Unusable bool
	Note     string

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
func (s *Server) currentAudio(ctx context.Context, devices []player.Device, notice string, probes map[string]probeOutcome) currentAudioView {
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

	if outcome, asked := probes[cur.Device]; asked {
		cur.Probed = true
		cur.Unusable = !outcome.Playable
		if cur.Unusable {
			// "default" carries no card of its own, so what decides the
			// wording is the card it resolves to — which on the operator's
			// board is the HDMI one, and is the whole trap.
			hdmi := isHDMIText(cur.Description)
			if cur.Device == "default" {
				hdmi = isHDMIText(cur.DefaultCard)
			}
			cur.Note = unusableNote(hdmi, outcome.TimedOut)
		}
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

func isHDMIText(s string) bool { return strings.Contains(strings.ToLower(s), "hdmi") }

// outputKinds turns the card's own words into what the thing physically is.
//
// The grouped picker used to read "bcm2835 Headphones, bcm2835 Headphones"
// and "vc4-hdmi, MAI PCM i2s-hifi-0" — ALSA driver strings, which are the
// right answer to "which PCM" and no answer at all to "where do I plug the
// speakers in". An operator who has just put a jack plug into the 3.5 mm
// socket has to already know that the first of those is that socket.
//
// Matched against the words aplay -L reports for the card, deliberately,
// and not against a list of board models: this project runs on Orange Pi
// and on whatever USB card someone has in a drawer, and a table of
// Raspberry Pi revisions would be wrong on every one of them. A card whose
// words match nothing here keeps its own description, which is no worse
// than what this screen showed before. Ordered because the words overlap —
// an HDMI card that calls itself "hdmi-audio" must not be read as a
// loudspeaker.
var outputKinds = []struct {
	// match is lowercase and is looked for inside the card id and the
	// card's description together.
	match string
	label string
}{
	{"hdmi", "HDMI"},
	{"headphone", "Роз'єм для навушників (3,5 мм)"},
	{"headset", "Гарнітура"},
	{"bluetooth", "Bluetooth"},
	{"usb", "USB-аудіопристрій"},
	{"speaker", "Вбудований динамік"},
	{"analog", "Аналоговий вихід"},
}

// outputLabel names a card in words an operator recognises, falling back to
// the card's own description and then to its ALSA id.
func outputLabel(card, description string) string {
	haystack := strings.ToLower(card + " " + description)
	for _, kind := range outputKinds {
		if strings.Contains(haystack, kind.match) {
			return kind.label
		}
	}
	if description != "" {
		return description
	}
	return card
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
		if isHDMIText(card + " " + d.Description) {
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
		groups = append(groups, deviceRowView{
			Value:    "default",
			Label:    "За замовчуванням",
			Detail:   rowDetail(d.Name, d.Description),
			Selected: current == "default",
			// "default" names no card of its own — ALSA resolves it, and
			// on the operator's board it resolves to the HDMI one. Which
			// card that is decides what an unusable "default" row has to
			// say, so this row borrows the same heuristic the live-state
			// block above the form already uses.
			HDMI: isHDMIText(defaultCardDescription(devices)),
		})
		break
	}

	for _, g := range groupByCard(devices) {
		groups = append(groups, deviceRowView{
			Value: g.Device,
			// What the thing is, above; which PCM it is, in smaller text
			// below. Dropping the second would make this picker and the
			// full list behind the disclosure impossible to match against
			// each other, and impossible to match against a log line.
			Label:    outputLabel(g.Card, g.Description),
			Detail:   rowDetail(g.Device, g.Description),
			Selected: g.Device == current,
			HDMI:     g.HDMI,
		})
	}

	options = make([]deviceRowView, len(devices))
	for i, d := range devices {
		options[i] = deviceRowView{Value: d.Name, Label: rowDetail(d.Name, d.Description), Selected: d.Name == current}
	}
	return groups, options
}

// rowDetail is the "PCM — card's own words" line, the one string both lists
// print so that a row in one can be found in the other.
func rowDetail(name, description string) string {
	if description == "" {
		return name
	}
	return name + " — " + description
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
func (s *Server) renderAudioPage(ctx context.Context, w http.ResponseWriter, status int, devices []player.Device, notice, current string, mutate func(*audioView, map[string]probeOutcome)) {
	cfg := s.deps.Config.Get()
	groups, options := buildDeviceRows(devices, current)

	// The grouped rows and the stored output, and deliberately not the full
	// disclosure list: the real board reports thirteen PCMs, and thirteen
	// silent playbacks on every page load buys nothing — eleven of them are
	// the same two cards answering again. A PCM only the full list offers
	// gets probed when it is the one actually chosen, which is what puts
	// the stored device in here beside the groups.
	probeNames := make([]string, 0, len(groups)+1)
	for _, g := range groups {
		probeNames = append(probeNames, g.Value)
	}
	probeNames = append(probeNames, currentDeviceName(cfg.Audio.Device))
	probes := s.probeOutputs(ctx, probeNames)
	applyProbes(groups, probes)

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
		Current:       s.currentAudio(ctx, devices, notice, probes),
	}
	if mutate != nil {
		mutate(&view, probes)
	}
	s.render(w, status, "audio.html", view)
}

// currentDeviceName is the one place that spells out what an empty stored
// device means: aplay with no -D at all, which player.ALSA.Play already
// calls "default" in its errors and currentAudio already shows as
// "default". Probing it under any other name would ask about a different
// thing than the page is displaying.
func currentDeviceName(stored string) string {
	if stored == "" {
		return "default"
	}
	return stored
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
		s.renderAudioPage(r.Context(), w, status, devices, notice, device, func(v *audioView, _ map[string]probeOutcome) {
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

	// The running player took its device at construction, so without this
	// the config said one output and the next announcement went to the old
	// one until the daemon restarted. Observed on the board: the output was
	// switched to the headphone jack and the test sound still went to HDMI.
	// After Update, never before — a player pointed at a device the stored
	// config does not name is the one state worse than the stale one.
	if s.deps.Player != nil {
		s.deps.Player.SetDevice(device)
	}

	// The saved output is opened for real before this page claims anything
	// about it. renderAudioPage probes the stored device as part of every
	// render, and the store has just been written, so the answer below is
	// about the output that was actually saved.
	//
	// A probe that fails does not undo the save. The operator may be about
	// to wire the speakers, may be saving HDMI before the amplifier is
	// switched on, and a settings screen that refuses to remember a choice
	// until the hardware agrees with it is a screen that cannot be used to
	// set the hardware up. What it must not do is stay quiet: silent
	// success on an output that cannot play is this whole defect.
	s.renderAudioPage(r.Context(), w, http.StatusOK, devices, notice, device, func(v *audioView, probes map[string]probeOutcome) {
		v.Message = savedMessage
		outcome, asked := probes[currentDeviceName(device)]
		switch {
		case !asked:
			// Nothing could be asked — no prober wired up, or the budget
			// ran out. The old, quiet behaviour, and the only case that
			// still deserves it.
		case outcome.Playable:
			v.Message = savedAndPlayableMessage
		default:
			v.Warning = fmt.Sprintf("%s %s", saveProbeFailedPrefix, v.Current.Note)
		}
	})
}

// What a save says about the output it just stored. Three sentences and not
// one, because there are three different things to have found out: nothing
// was asked, the output opened, the output would not open.
const (
	savedMessage            = "Налаштування звуку збережено."
	savedAndPlayableMessage = "Налаштування звуку збережено. Вихід перевірено — його вдалося відкрити."
	saveProbeFailedPrefix   = "Вихід збережено, але зараз його не вдалося відкрити — звуку не буде, доки це не зміниться."
)

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
func (s *Server) audioFlash(r *http.Request) func(*audioView, map[string]probeOutcome) {
	q := r.URL.Query()
	switch {
	case q.Has("tested"):
		return func(v *audioView, _ map[string]probeOutcome) { v.Message = testSoundMessage }
	case q.Has("test_error"):
		message, ok := testSoundErrors[q.Get("test_error")]
		if !ok {
			return nil
		}
		return func(v *audioView, _ map[string]probeOutcome) { v.Error = message }
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
	if err := s.deps.Engine.PlayPreview(sound, s.deps.Config.Get().Audio.MasterVolume); err != nil {
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
