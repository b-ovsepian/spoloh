package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/scheduler"
)

// homeView is what home.html reads. Every time is pre-formatted in the
// device's configured location: the template escapes these into a
// JavaScript literal for Alpine's initial state, and a time.Time there would
// render as a Go struct.
type homeView struct {
	Status engine.Status
	// Addresses is both the "what do I type into a browser" answer and the
	// page's network-health signal: holding no address at all is what "not on
	// the network" looks like from inside the daemon.
	Addresses []string
	// MDNS is the name an operator can type instead of an address, empty
	// when nothing publishes one or the device holds no address for it to
	// resolve to — see MDNSName and reachableMDNSName. It sits beside
	// Addresses because it answers the same question and fails the same way.
	MDNS string
	// Level is how severe the running alert is, in the words the banner
	// shows. It is also carried in every SSE frame — see sse.go — because a
	// level rendered once would freeze at whatever it was when the page
	// loaded, which is worst during exactly the minute it changes.
	Level           alertLevelView
	SinceText       string
	LastSuccessText string
	NextEventAtText string
	// Sounds lists what an operator may choose from the manual-playback
	// form. Empty (rather than nil-and-panicking) when Deps.Library is nil
	// or its List call fails, so the rest of the page still renders.
	Sounds []library.Sound
	// Flash is a one-shot notice carried across the redirect from /play or
	// /stop, so the page an operator lands back on after pressing a button
	// visibly says what just happened instead of looking unchanged.
	Flash string
	// AlertsDisabled is true when the alert-start event is disabled, or
	// silenced the other way an operator (or a hand-edited config.json) can
	// reach: volume 0. Either leaves the device unable to announce an air
	// raid, and it is the one warning on this whole dashboard that must be
	// impossible not to notice: an operator who disables it for a ten-minute
	// PA test and forgets is the worst outcome this product has, and
	// home.html renders it above everything else, unconditionally, whenever
	// this is true.
	AlertsDisabled bool
	// AlertsSilencedByQuietHours is true when the quiet-hours window is
	// enabled and its category list covers alerts — the one configuration
	// that makes this device deliberately choose silence during a raid.
	//
	// It reads the configuration, not the clock: the warning is about how
	// the device is set up, not about whether it happens to be 03:00 right
	// now, and an operator who set this at noon must see it at noon. Both
	// this and AlertsDisabled can be true at once and both render — they
	// are different faults with different fixes.
	//
	// Shares config.QuietHours.SilencesAlerts() with the /events form, so
	// the two screens cannot drift apart on what "silenced" means.
	AlertsSilencedByQuietHours bool
	// QuietFrom and QuietTo are the window's edges, carried for the warning
	// above so the operator is told which hours rather than merely that
	// some exist.
	QuietFrom string
	QuietTo   string

	// Recent is the last few event log entries, newest first, in log.go's
	// own row shape. It is the one thing on this dashboard that answers
	// "did this device actually make a sound the last time it mattered" —
	// every other card describes how the device is configured or what it
	// intends to do next, and a device that looks healthy in all of them
	// and has not played anything successfully in a month is the failure
	// this product most needs an operator to notice. Until now finding
	// that out meant navigating away.
	Recent []logEntryView
	// RecentNotice explains an empty Recent in the operator's own terms,
	// the same three cases /log separates: no log on this device, a log
	// that would not read, and a log with nothing in it yet. All three
	// look like "no history" and mean very different things.
	RecentNotice string

	// Sources is the cascade the last poll walked, one row per configured
	// key, in the order they are tried. Empty when there is nothing to
	// report, and SourcesNotice then says why in the operator's own terms
	// rather than leaving an empty card.
	Sources []sourceHealthView
	// SourcesNotice explains an empty Sources: on a device that has not
	// polled yet there is no cascade to describe, and an empty list is not
	// the same claim as a cascade of keys that went unreached — that one
	// says a poll happened and stopped early.
	SourcesNotice string

	// Audio is the stored output as /audio's own block describes it —
	// reused wholesale rather than restated, see homeAudio.
	Audio currentAudioView
	// AudioNotice is listDevices' explanation for an output list that could
	// not be obtained, carried so the card can say why it is naming the
	// stored PCM and nothing else about it.
	AudioNotice string
	// TestSound is what the Звук card's button plays, chosen by the same
	// testSound /audio's own button uses. Empty when the library holds
	// nothing at all, and the card then says so instead of rendering a
	// button that can only fail.
	TestSound string
}

// alertLevelView is the line under ТРИВОГА that says how bad the raid is.
// Both halves are built in Go rather than in the page, so the one branch
// that must not be got wrong is written once — see alertLevel.
type alertLevelView struct {
	// Label is the level in words, empty on a calm device. Never a colour
	// on its own: this screen is read behind glass and from across a
	// corridor, and the banner is already solid red for every raid.
	Label string
	// Note is the qualifier an unknown level needs and the two named levels
	// do not, empty for them.
	Note string
	// Severe drives the banner's colour: red for a red or an unnamed level,
	// amber for a declared yellow one.
	//
	// It is a boolean decided here rather than a level string the template
	// branches on, for the same reason Label is. "Is this red" has exactly
	// one implementation — Level.TreatAsRed — and an unknown level, which
	// every Ubilling source reports, must come out of it as red. A second
	// answer to that question living in a template is how the amber banner
	// would end up on a raid nobody had graded.
	Severe bool
}

// The three things the banner can say about severity. A raid declared
// yellow and a raid declared red are different instructions to the people
// who hear the siren; an unknown level is neither a third instruction nor a
// milder one.
const (
	levelRedLabel    = "ЧЕРВОНИЙ РІВЕНЬ"
	levelYellowLabel = "ЖОВТИЙ РІВЕНЬ"
	// levelUnknownNote is what is added under the red label when upstream
	// named no level at all — which is every Ubilling key, so it is the
	// normal reading on most devices rather than an exception.
	//
	// The label stays the red one. Saying «рівень невідомий» in the badge
	// itself would read, from the other side of a corridor, as something
	// less than red, and this device announces an unknown level as red: a
	// page that implied otherwise would contradict the siren the operator
	// just heard. The note is what keeps that honest — it says plainly that
	// nobody named the level and that the device is treating it as the
	// worse of the two.
	levelUnknownNote = "джерело не вказує рівень — пристрій оголошує тривогу як червону"
)

// alertLevel turns the engine's level into what the banner shows.
//
// It branches on alertsource.Level.TreatAsRed and never on the literal
// "red". Unknown is the empty value and is handled as red everywhere in this
// repository — every Ubilling source reports it, because those providers
// have no level concept at all — so a test for "red" here would render every
// Ubilling-sourced raid as the lesser warning. That is the silent downgrade
// the whole level design exists to prevent, and this is the one line of the
// dashboard where it could be reintroduced.
//
// A calm device says nothing about severity: Status.Level is unknown when
// nothing is running, and a banner that is hidden anyway must not be carrying
// a claim about a raid that is not happening.
func alertLevel(st engine.Status) alertLevelView {
	switch {
	case !st.Alert:
		return alertLevelView{}
	case !st.Level.TreatAsRed():
		return alertLevelView{Label: levelYellowLabel, Severe: false}
	case st.Level == alertsource.LevelUnknown:
		return alertLevelView{Label: levelRedLabel, Note: levelUnknownNote, Severe: true}
	default:
		return alertLevelView{Label: levelRedLabel, Severe: true}
	}
}

// alertStartDisabled reports whether the configured alert-start event will
// not be heard: disabled, volume 0, or missing outright. events.go's
// /events forms refuse a volume of 0 for exactly this reason — disabling
// the event is meant to be the one, visible way to silence it — but nothing
// stops a volume of 0 reaching config.json by hand, and that is exactly the
// person who most needs this page to say so.
//
// It walks cfg.Events by Trigger rather than assuming a fixed index or a
// fixed ID, since nothing about the events list's shape guarantees either.
// A config missing an alert-start event entirely reports true — the second
// of two independent lines of defence against that: config.Load restores
// one from config.Default whenever it reads a config missing it, so this
// case should never actually be reached in production, but a *Config built
// some other way — a test, an import, a future migration that constructs
// one without going through Load — must not read as "fine" here just
// because the loop found nothing to object to.
func alertStartDisabled(cfg *config.Config) bool {
	for _, ev := range cfg.Events {
		if ev.Trigger == config.TriggerAlertStart {
			return !ev.Enabled || ev.Volume == 0
		}
	}
	return true
}

// status returns the engine's live status with two fields already filled in
// even when there is no engine at all: Region and the next scheduled event
// are pure functions of the configuration, not of engine state, so they are
// always shown, before the engine exists or in this package's own tests.
// NextUp is documented as safe for exactly this: a fresh, stateless Runner
// reads the same configured events the live scheduler would.
//
// Both handleHome and the SSE stream in sse.go call this one method rather
// than each computing Region and NextEvent themselves, so the page an
// operator loaded and the frames that keep it updated can never disagree
// about what the daemon has no engine for yet.
func (s *Server) status(loc *time.Location) engine.Status {
	// Engine is nil until the daemon wires the dashboard up (Task 9) and in
	// this package's own tests; a zero-value Status renders a page with
	// every engine-derived field empty rather than panicking.
	var st engine.Status
	if s.deps.Engine != nil {
		st = s.deps.Engine.Status()
	}
	st.Region = s.deps.Config.Get().Alerts.Region
	runner := scheduler.NewRunner(nil, s.deps.Config.Get, nil)
	if ev, at, ok := runner.NextUp(s.now().In(loc)); ok {
		st.NextEventName = ev.Name
		if st.NextEventName == "" {
			st.NextEventName = ev.ID
		}
		st.NextEventAt = at
	}
	return st
}

// sourceHealthView is one cascade key on the Стан джерел card.
//
// Key is the key the operator picked on /alerts — "default", "skog",
// "alertsua" — and not the name the upstream reports for itself. They are
// different strings: "default" answers as "Vadym Klymenko API", so a card
// built from self-reported names could not tell an operator which of the
// slots they configured is the one working, which is the only question this
// card exists to answer. /alerts lists the same keys in the same order, so
// the two screens can be read side by side.
//
// Failed and NotReached are separate because the three states are separate.
// A key the cascade never got to is not healthy — nothing checked it — and
// it is not broken either, and the row has to say which without the reader
// having to know that the cascade stops at the first answer.
type sourceHealthView struct {
	Key        string
	State      string
	Reason     string
	Failed     bool
	NotReached bool
}

// sourceStateText is the three outcomes in the operator's language. Each
// pairs a word with a mark, like every other state on this page: never
// colour alone, since this screen is read behind glass and from across a
// corridor.
func sourceStateText(o alertsource.Outcome) string {
	switch o {
	case alertsource.OutcomeAnswered:
		return "✓ відповів"
	case alertsource.OutcomeFailed:
		return "✕ не відповів"
	default:
		return "— не опитувався"
	}
}

// sourceReasonText turns alertsource's closed vocabulary into the sentence
// an operator can act on. It is reached only for a key that failed.
//
// The vocabulary is closed for a reason — see alertsource.Reason — and this
// is the only thing this device says about why a source did not answer.
// Nothing from upstream is echoed: engine.SourceHealth does not carry the
// error text, so there is nothing here that could. A reason with no
// classification (an unrecognised value, or a failure that carried no error
// at all) says so rather than rendering an English identifier.
func sourceReasonText(r alertsource.Reason) string {
	switch r {
	case alertsource.ReasonNoToken:
		return "немає токена — його потрібно отримати й зберегти на сторінці «Джерела»"
	case alertsource.ReasonTokenRejected:
		return "токен відхилено — можливо, він застарів або введений з помилкою"
	case alertsource.ReasonRateLimited:
		return "перевищено ліміт запитів — обмеження рахується на всю вашу мережу"
	case alertsource.ReasonUnreachable:
		return "немає зв’язку — джерело не відповіло на запит"
	case alertsource.ReasonBadResponse:
		return "відповідь не вдалося прочитати"
	default:
		return "причина невідома"
	}
}

// sourceHealth turns the engine's account of the last cascade into rows, and
// answers with a notice instead when there is no account to show.
//
// An empty list is the honest answer before the first poll of a process, and
// it is deliberately not rendered as a list of unreached keys: "not reached"
// is a fact about a poll that happened, and saying it about a device that has
// polled nothing yet would be inventing a poll. The same notice covers a
// source that cannot describe a cascade at all — no provider in this
// repository is one, both implement alertsource.Cascade, but a nil-tolerant
// page beats a page that assumes.
func sourceHealth(st engine.Status) ([]sourceHealthView, string) {
	if len(st.Sources) == 0 {
		return nil, "Джерела ще не опитувалися."
	}
	out := make([]sourceHealthView, len(st.Sources))
	for i, s := range st.Sources {
		v := sourceHealthView{
			Key:        s.Key,
			State:      sourceStateText(s.Outcome),
			Failed:     s.Outcome == alertsource.OutcomeFailed,
			NotReached: s.Outcome == alertsource.OutcomeNotReached,
		}
		if v.Failed {
			v.Reason = sourceReasonText(s.Reason)
		}
		out[i] = v
	}
	return out, ""
}

// homeRecentEntries is how many event log rows the Останні дії card shows.
//
// Five is a judgement, not a round number. One row cannot answer the
// question the card exists for: a single failure under four successes reads
// differently from five failures in a row, and it is the run that tells an
// operator whether the device is broken or merely had a bad minute. Past
// about this many the card stops being something the eye takes in at a
// glance and becomes a list to scroll past on the way to everything else,
// which is what /log — one link away, and reading recentLogEntries — is
// for.
const homeRecentEntries = 5

// recentActivity reads the last few log entries for the dashboard card.
//
// It builds log.go's logEntryView through log.go's own kindLabel, so the
// same row cannot be called one thing here and another there — an operator
// comparing this card against /log is comparing them on purpose, and two
// vocabularies for one fact would make that comparison a puzzle. What it
// deliberately does not reuse is logBaseView: that reads two hundred rows
// off an SD card and answers questions about clearing the log, and this
// page is fetched far more often than /log is, including by the stream's
// own recovery probe.
//
// Every way the log can be unavailable ends in a notice rather than an
// error, the same tolerance /log gives it: Deps.Log is nil on a device
// whose SQLite file is corrupt or whose card went read-only, and that must
// cost the operator their history, never the dashboard.
func (s *Server) recentActivity() ([]logEntryView, string) {
	if s.deps.Log == nil {
		return nil, "Журнал подій недоступний на цьому пристрої."
	}
	entries, err := s.deps.Log.Recent(homeRecentEntries)
	switch {
	case err != nil:
		s.deps.Logger.Error("event log: recent", "err", err)
		return nil, "Не вдалося завантажити журнал подій."
	case len(entries) == 0:
		return nil, "Записів поки немає."
	}

	loc := s.location()
	out := make([]logEntryView, len(entries))
	for i, e := range entries {
		out[i] = logEntryView{
			AtText:        e.At.In(loc).Format("2006-01-02 15:04:05"),
			Kind:          kindLabel(e.Kind),
			EventID:       e.EventID,
			Sound:         e.Sound,
			OK:            e.OK,
			Err:           e.Err,
			PlaybackError: e.Kind == eventlog.KindPlaybackError,
		}
	}
	return out, ""
}

// homeAudioBudget bounds everything the Звук card asks the sound system —
// listing the outputs, opening the configured one, and finding its mixer
// control — all of it together.
//
// /audio allows ten seconds for each of those three separately, which is
// right for a screen somebody opened because the sound is broken and is
// prepared to wait on. This is the dashboard. It is glanced at, it is what
// the stream's recovery probe fetches when the daemon comes back, and a
// wedged ALSA device must not be able to hold it: a card that says
// "невідомо" in two seconds is worth more than the same card thirty seconds
// later. Every call below derives its own deadline from this one, and
// context.WithTimeout takes the shorter of the two, so nothing underneath
// can outlast it.
const homeAudioBudget = 2 * time.Second

// homeAudio describes the configured output for the Звук card by calling
// /audio's own currentAudio. None of what that knows is restated here: the
// card labels, the "default resolves to card 0" heuristic, the wording for
// an output that will not open and the mixer lookup are decided in audio.go,
// and two screens on one device disagreeing about which output it uses would
// be worse than either of them being wrong.
//
// playing suppresses the probe, and that is the one rule this page adds. A
// probe is not a query — player.ProbeDevice opens the real PCM and plays a
// real WAV through it — so probing while the player holds that PCM puts a
// second opener on the sound card during the one minute this device exists
// for. Neither answer it could come back with belongs on this card either:
// both "works" and "does not work" would be facts about the siren already
// playing, not about the output. An output nobody asked about claims
// nothing, which is what applyProbes and currentAudio already do with one.
func (s *Server) homeAudio(ctx context.Context, playing bool) (currentAudioView, string) {
	ctx, cancel := context.WithTimeout(ctx, homeAudioBudget)
	defer cancel()

	devices, notice := s.listDevices(ctx)

	// One probe, of the one output this card names. /audio probes every
	// grouped row because it is a picker and those rows are candidates;
	// here there is nothing to choose between, and twelve silent playbacks
	// on a dashboard load would buy nothing at all.
	var probes map[string]probeOutcome
	if !playing {
		probes = s.probeOutputs(ctx, []string{currentDeviceName(s.deps.Config.Get().Audio.Device)})
	}
	return s.currentAudio(ctx, devices, notice, probes), notice
}

// apOnly reports whether this device is up on its own access point with no
// upstream, which is the one condition that changes what / is.
//
// It reads the feed the network supervisor publishes to, and so does
// internal/engine's poll loop: the page that says "this device stopped
// polling" and the loop that stopped polling have to be answering the same
// question from the same reading, or one of them is lying. A nil feed — this
// package's own tests, a daemon wired without a supervisor — reads as false,
// which is the ordinary dashboard.
func (s *Server) apOnly() bool { return s.deps.NetStatus.APOnly(s.now()) }

// setupNetworkView is what setup-network.html reads.
//
// It carries deliberately little. Everything homeView offers — the cascade's
// health, the sound library, the audio probe, the recent log, the next
// scheduled event — describes a device with a network, and over the access
// point every one of those is either stale by definition or beside the
// point. The person holding the phone is standing in a room with no
// internet, trying to get this device onto a network; anything else on the
// screen is something between them and that.
type setupNetworkView struct {
	// Networks and ScanNotice are /network's own scan, unchanged — see
	// scanNetworks. The join this page submits is /network's too. Two scans,
	// or two joins, would be two implementations of the operation this whole
	// screen exists to perform, and the one that broke would be the one
	// nobody was looking at.
	Networks   []netmgr.Network
	ScanNotice string
	// APSSID and APPassword are the access point the operator is reading
	// this page over. Display only, never logged — see networkView, which
	// shows them on the same terms.
	APSSID     string
	APPassword string

	// Alert is what this device still believes, and it is on this screen for
	// one reason: if it is true the siren is audible in the room. A setup
	// screen showing a calm, tidy form while the device is wailing is a
	// screen nobody trusts again.
	//
	// It is said in a sentence rather than the dashboard's banner. The
	// banner is the loudest thing on the widest page and it means "this is
	// happening now, live"; here the reading is from before the network
	// went, and the page has to say that too.
	Alert bool
	// LevelLabel is how severe the believed raid is, in the banner's own
	// words — built by alertLevel, the only place the level is branched on,
	// so an unknown level reads as red here exactly as it does there.
	LevelLabel string
	// SinceText is when the believed raid started, and LastSuccessText the
	// last moment this device heard anything from a source at all. The
	// second is the honest one: it is what makes "this reading is old" a
	// fact with a time on it rather than a hedge.
	SinceText       string
	LastSuccessText string
}

// handleNetworkSetup renders the one screen that matters over the device's
// own access point: where the device is, and a form to put it on a network.
//
// A separate template rather than a branch inside home.html, and the argument
// is maintenance rather than taste. home.html is the longest page in this
// repository and its job is to show everything; folding this in would put
// every one of its cards behind a condition, and would leave the screen whose
// whole point is that it shows one thing living inside the file that keeps
// growing. It is also more than markup: handleHome reads the event log,
// probes the ALSA output and walks the source cascade to build its view, and
// none of that should be computed for a page that will not render it — the
// audio probe in particular opens the real PCM, which is the last thing to do
// on a device that may be mid-siren. render.go gives every page its own clone
// of the layout already, so a second template costs nothing but the file.
//
// It stays on / rather than taking a route of its own. The address someone
// types is the device's, a redirect would be a second URL to get wrong next
// to the /setup that already exists for the first-run password, and the
// moment the device joins a network / has to be the dashboard again — which a
// bookmarked second URL would quietly stop being.
//
// The ordinary shell, navigation and all: the access point is how a device is
// commissioned, so the operator standing over it still has to be able to
// reach /audio and /sounds. What this screen refuses to do is put any of that
// in front of them unasked.
func (s *Server) handleNetworkSetup(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Get()
	// The refresh button matters more here than anywhere else: this is the
	// screen somebody reaches standing in a building the device has never
	// been in, looking for a network NetworkManager's cache has never seen.
	nets, notice := s.scanNetworks(r.Context(), r.URL.Query().Has("rescan"))

	loc := s.location()
	st := s.status(loc)
	view := setupNetworkView{
		Networks:   nets,
		ScanNotice: notice,
		APSSID:     cfg.Network.APSSID,
		APPassword: cfg.Network.APPassword,
		Alert:      st.Alert,
		LevelLabel: alertLevel(st).Label,
	}
	if !st.Since.IsZero() {
		view.SinceText = st.Since.In(loc).Format("2006-01-02 15:04")
	}
	if !st.LastSuccess.IsZero() {
		view.LastSuccessText = st.LastSuccess.In(loc).Format("2006-01-02 15:04")
	}
	s.render(w, http.StatusOK, "setup-network.html", view)
}

// handleHome renders the dashboard: the page an operator actually looks at.
// Every value is rendered server-side so the page is correct with
// JavaScript disabled or before Alpine has loaded — the SSE stream wired up
// in home.html only keeps it fresh afterward.
//
// Except over the device's own access point, where it renders the network
// setup screen instead. None of this dashboard is what the person holding the
// phone there needs — and the map cannot load, the cascade's health is a
// description of the last poll before the network went, and the whole page is
// between them and the one thing they came to do.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if s.apOnly() {
		s.handleNetworkSetup(w, r)
		return
	}

	loc := s.location()
	st := s.status(loc)

	addrs := LocalAddresses()
	view := homeView{
		Status:                     st,
		Addresses:                  addrs,
		MDNS:                       reachableMDNSName(MDNSName(), addrs),
		Level:                      alertLevel(st),
		AlertsDisabled:             alertStartDisabled(s.deps.Config.Get()),
		AlertsSilencedByQuietHours: s.deps.Config.Get().QuietHours.SilencesAlerts(),
		QuietFrom:                  s.deps.Config.Get().QuietHours.From,
		QuietTo:                    s.deps.Config.Get().QuietHours.To,
	}
	if !st.Since.IsZero() {
		view.SinceText = st.Since.In(loc).Format("2006-01-02 15:04")
	}
	if !st.LastSuccess.IsZero() {
		view.LastSuccessText = st.LastSuccess.In(loc).Format("15:04:05")
	}
	if !st.NextEventAt.IsZero() {
		view.NextEventAtText = st.NextEventAt.In(loc).Format("2006-01-02 15:04")
	}

	// Library is nil until Task 9 wires the dashboard up and in this
	// package's own tests that don't need it — same guard as Engine above.
	if s.deps.Library != nil {
		sounds, err := s.deps.Library.List()
		if err != nil {
			s.deps.Logger.Error("library: list", "err", err)
		} else {
			view.Sounds = sounds
		}
	}
	view.Sources, view.SourcesNotice = sourceHealth(st)
	view.Recent, view.RecentNotice = s.recentActivity()
	// Handed st.PlayingEventID rather than asking the player again: the
	// card must not probe the output the siren is coming out of, and the
	// answer to "is something playing" has to be the same one the rest of
	// this page was built from — see homeAudio.
	view.Audio, view.AudioNotice = s.homeAudio(r.Context(), st.PlayingEventID != "")
	// The same sound /audio's own button plays, chosen the same way. The
	// second result says whether there is one at all, which the template
	// reads off TestSound being empty.
	view.TestSound, _ = s.testSound()

	view.Flash = s.flashMessage(r)

	s.render(w, http.StatusOK, "home.html", view)
}

// flashMessage turns the query parameters /play and /stop redirect back with
// into the Ukrainian notice shown on the page. Query parameters rather than a
// cookie: the message is meant to be seen exactly once, by the browser that
// is about to render this response, and never needs to survive past it.
//
// Nothing here echoes what it was given. The "played" value is only ever
// repeated once Library confirms it names a real sound, and "playfailed"
// carries a code this package chose rather than a sentence. Both are
// attacker-controllable otherwise: anyone who gets an already logged-in
// operator to open a crafted "?played=..." link could plant arbitrary text
// on the device's own trusted dashboard. html/template would still escape it
// — this is not an injection fix — but an unvalidated string has no business
// looking like something the device said.
func (s *Server) flashMessage(r *http.Request) string {
	q := r.URL.Query()
	switch {
	case q.Has("stopped"):
		return "Відтворення зупинено."
	case q.Has("playfailed"):
		return playFailureMessage(q.Get("playfailed"))
	case q.Has("played"):
		sound := q.Get("played")
		// Library is nil in this package's own tests and before Task 9
		// wires the dashboard up — same guard as handleHome's Sounds
		// lookup above. With no library to confirm against, the name
		// cannot be trusted, so it falls back to the generic message too.
		if s.deps.Library != nil && s.deps.Library.Has(sound) {
			return "Відтворюється: " + sound
		}
		return "Відтворення почалося."
	default:
		return ""
	}
}

// playReturnField is the optional form field naming the page /play should
// answer on. /sounds sets it, so previewing a sound leaves the operator
// standing where they were; the home page's own form omits it and keeps the
// behaviour it has always had.
const playReturnField = "return"

// playPreviewField marks a /play submission as a preview rather than a
// manual fire. Present and non-empty means preview; absent means the home
// page's own form, which keeps the behaviour it has always had.
const playPreviewField = "preview"

// Play-failure reason codes. The redirect carries a code, never a sentence:
// whatever comes back in the query string is attacker-supplied by
// construction — see flashMessage — so the only text that may reach a page
// is text chosen here.
const (
	playFailedMissing     = "missing"
	playFailedUnavailable = "unavailable"
)

// playFailureMessage turns one of those codes into the sentence an operator
// reads. An unrecognised code — a hand-edited link, a redirect from an older
// build still sitting in a browser's history — falls back to the generic
// sentence rather than being shown.
func playFailureMessage(code string) string {
	switch code {
	case playFailedMissing:
		return "Такого звука немає в бібліотеці."
	case playFailedUnavailable:
		return "Відтворення недоступне на цьому пристрої."
	default:
		return "Не вдалося відтворити звук."
	}
}

// returnPath validates an operator-submitted return target before it is
// reflected into a Location header, and answers "/" for anything it will not
// vouch for.
//
// A redirect target taken from a form and put into Location is an open
// redirect the moment anything off this device can be named: a crafted
// preview link would bounce an operator from their own dashboard to a page
// of someone else's choosing, carrying the trust this device's address has
// earned with it. So exactly one shape is accepted — a rooted path on this
// site — and everything else falls back.
//
// Refused, and why:
//
//   - "http://evil.example/x", "https://evil.example/x" — an absolute URL
//     names a host, which is the whole thing being prevented.
//   - "//evil.example/x" — protocol-relative: no scheme, still a host.
//   - "/\evil.example" — a browser resolving a relative Location for an
//     http(s) URL folds a backslash into a slash, so this is the
//     protocol-relative form wearing a different hat. url.Parse does not see
//     that on its own, which is why the backslash is checked by hand.
//   - "sounds", "evil.example/x" — not rooted. A browser resolves these
//     against the page the form was on, so where they land is a property of
//     the referring page rather than a promise made here.
//   - "javascript:alert(1)", "mailto:x@y" — a scheme is a scheme.
//   - anything url.Parse itself refuses, which includes an embedded newline
//     or tab smuggled in to split the response header.
//
// A query or fragment on the submitted value is dropped rather than made a
// reason to refuse: the caller appends its own query, and a Location cannot
// hold two.
func returnPath(raw string) string {
	const fallback = "/"
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return fallback
	}
	if strings.Contains(raw, `\`) {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Opaque != "" || u.Host != "" || u.User != nil {
		return fallback
	}
	return u.EscapedPath()
}

// playFailed answers a refused playback.
//
// An operator who named a page to return to gets sent back to it with the
// reason in the flash, because that is the page they are looking at and a
// bare error page would lose it. Without one — the home page's own form —
// this is the plain error response /play has always given, unchanged.
func playFailed(w http.ResponseWriter, r *http.Request, back string, named bool, status int, code string) {
	if !named {
		http.Error(w, playFailureMessage(code), status)
		return
	}
	http.Redirect(w, r, back+"?"+url.Values{"playfailed": {code}}.Encode(), http.StatusSeeOther)
}

// handlePlay is spec §5.6's first-class manual path: the one that still works
// when upstream is down but the threat is real, and — through the same
// route, deliberately — the preview button on /sounds. A preview that played
// by some other path than an announcement would be a preview that can lie
// about the thing it is previewing, so there is one path and it goes through
// the engine, the player's priority queue and the configured output.
//
// Registered as POST /play so requireAuth protects it by default and a GET —
// a prefetch, a link preview, a bookmark sync — can never reach it, and
// sameOrigin closes the one gap SameSite cookies leave open for a
// state-changing POST.
//
// The sound name is handed to Engine.PlayManual exactly as the operator sent
// it: PlayManual already refuses anything that is not a real library entry,
// and building or checking a filesystem path here would only duplicate that
// check with a second chance to get it wrong.
//
// Nothing here touches priority. A preview is queued at exactly the priority
// a manual play has always had, so a real alert still preempts it.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// Where to answer. Whether the operator named a page at all is kept
	// apart from which page that turned out to be: an off-site value falls
	// back to "/" but is still a request to be answered on a page rather
	// than with an error document.
	raw := r.FormValue(playReturnField)
	back, named := returnPath(raw), raw != ""

	// Engine is nil in this package's own tests and before Task 9 wires the
	// dashboard up — same guard as handleHome and sse.go.
	if s.deps.Engine == nil {
		playFailed(w, r, back, named, http.StatusServiceUnavailable, playFailedUnavailable)
		return
	}

	sound := r.FormValue("sound")
	volume := s.deps.Config.Get().Audio.MasterVolume

	// Two different intentions arrive on this one route, and they must not
	// hold the speaker with the same authority.
	//
	// The home page's form is a manual fire: the path that still works when
	// upstream is down and the threat is real, dispatched at PriorityAlert
	// so a scheduled chime cannot cut it short. /sounds' listen button is a
	// preview — someone finding out which file is the siren — and it
	// dispatches below everything real, because Player preempts only on
	// strictly higher priority and a preview sharing PriorityAlert could
	// hold the single voice for up to five minutes with an actual air raid
	// alert queued behind it.
	//
	// The field is explicit rather than inferred from playReturnField, even
	// though only /sounds sets that today. Tying "may delay a siren" to
	// "names a page to return to" would make the next page that wants a
	// return path silently safe or silently dangerous depending on which
	// button it copied.
	play := s.deps.Engine.PlayManual
	if r.FormValue(playPreviewField) != "" {
		play = s.deps.Engine.PlayPreview
	}
	if err := play(sound, volume); err != nil {
		s.deps.Logger.Warn("manual playback rejected", "sound", sound, "err", err)
		playFailed(w, r, back, named, http.StatusBadRequest, playFailedMissing)
		return
	}

	http.Redirect(w, r, back+"?"+url.Values{"played": {sound}}.Encode(), http.StatusSeeOther)
}

// handleStop is manual playback's other half: the button an operator reaches
// for in a hurry, so it must silence whatever is on the speaker right now,
// not merely clear what was queued behind it.
//
// It goes through Engine.StopManual rather than Player.Stop for the same
// reason /play goes through PlayManual: the engine is what records the
// operator's action in the event log. Calling the player directly left the
// log showing that someone started the siren and never that they stopped it.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// Engine is nil in this package's own tests and before Task 9 wires the
	// dashboard up — same guard as handlePlay and sse.go. There is then
	// nothing to stop, which is a safe no-op rather than an error: unlike
	// /play, a stop that finds nothing playing has already achieved what the
	// operator wanted.
	if s.deps.Engine != nil {
		s.deps.Engine.StopManual()
	}
	// Same return contract as /play, and for the same reason: an operator
	// who previewed a sixty-second siren from /sounds needs to silence it
	// from /sounds. Sending them to the dashboard to do it is how a preview
	// button turns into a minute of siren nobody asked for. returnPath
	// refuses anything naming a host — see its doc comment.
	back := returnPath(r.FormValue("return"))
	http.Redirect(w, r, back+"?"+url.Values{"stopped": {"1"}}.Encode(), http.StatusSeeOther)
}
