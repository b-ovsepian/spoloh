package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/library"
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
	MDNS            string
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

// handleHome renders the dashboard: the page an operator actually looks at.
// Every value is rendered server-side so the page is correct with
// JavaScript disabled or before Alpine has loaded — the SSE stream wired up
// in home.html only keeps it fresh afterward.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	loc := s.location()
	st := s.status(loc)

	addrs := LocalAddresses()
	view := homeView{
		Status:         st,
		Addresses:      addrs,
		MDNS:           reachableMDNSName(MDNSName(), addrs),
		AlertsDisabled: alertStartDisabled(s.deps.Config.Get()),
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
	if err := s.deps.Engine.PlayManual(sound, volume); err != nil {
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
