package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/web"
)

func TestHomeShowsTheRegionAndState(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The region must appear in a visible text node, not merely somewhere in
	// the response: home.html also embeds it inside the <script> block as
	// Alpine's initial state (region: "м. Київ"), and that occurrence alone
	// tells us nothing about what a browser with JavaScript disabled would
	// show. ">м. Київ<" only matches text sitting directly between two tags,
	// which is where the page's <p x-text="region"> puts it — a regression
	// that emptied that text node while leaving the script literal alone
	// would still contain "м. Київ" in the body but would fail this check.
	if !strings.Contains(body, ">м. Київ<") {
		t.Error("the page does not show the configured region in its visible markup")
	}
	if !strings.Contains(body, "/stream") {
		t.Error("the page does not subscribe to the status stream")
	}
}

// The page must be readable with JavaScript disabled or before Alpine loads:
// the server renders the real state, and the stream only keeps it fresh.
func TestHomeRendersStateServerSide(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	// Same reasoning as above: home.html also carries the event name inside
	// the <script> block (nextEvent: "Хвилина мовчання"), so a plain
	// strings.Contains would pass even if the visible
	// <span x-text="nextEvent"> were left empty. ">Хвилина мовчання<" only
	// matches the visible text node.
	if !strings.Contains(body, ">Хвилина мовчання<") {
		t.Error("the next scheduled event is not rendered in the page's visible markup")
	}
}

// The state has to read correctly to someone who cannot tell the red block
// from the calm one: each state pairs its colour with the word and with a
// shape. Both blocks are always in the markup — Alpine swaps which one is
// shown — so the check is that each carries both signals, and that the one
// the server hid is the one the engine says is not current.
func TestHomeSignalsStateWithMoreThanColour(t *testing.T) {
	srv, _ := newServerWithStore(t) // no engine, so the device is calm
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, block := range []struct{ marker, word string }{
		{`data-state="alert"`, "ТРИВОГА"},
		{`data-state="calm"`, "СПОКІЙНО"},
	} {
		start := strings.Index(body, block.marker)
		if start < 0 {
			t.Fatalf("the page has no %s block", block.marker)
		}
		end := strings.Index(body[start:], "</div>")
		if end < 0 {
			t.Fatalf("the %s block is never closed", block.marker)
		}
		markup := body[start : start+end]
		if !strings.Contains(markup, block.word) {
			t.Errorf("the %s block does not spell out %q", block.marker, block.word)
		}
		if !strings.Contains(markup, "<svg") {
			t.Errorf("the %s block carries no shape, only colour and a word", block.marker)
		}
	}

	// x-show sets exactly this inline style, so the server-rendered page
	// already shows what a browser with JavaScript disabled should see.
	if !strings.Contains(body, `data-state="alert" x-show="alert" style="display:none"`) {
		t.Error("a calm device renders the alarmed block visible")
	}
	if strings.Contains(body, `data-state="calm" x-show="!alert" style="display:none"`) {
		t.Error("a calm device hides its own calm block")
	}
}

// The stop button is pressed in a hurry, so it comes before everything an
// operator would otherwise have to scroll past — the manual play control
// included, which it must never be confused with.
func TestHomePutsStopAheadOfPlay(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	stop := strings.Index(body, `action="/stop"`)
	play := strings.Index(body, `action="/play"`)
	if stop < 0 || play < 0 {
		t.Fatalf("stop at %d, play at %d: both controls must be on the page", stop, play)
	}
	if stop > play {
		t.Error("the play control comes before the stop button, which is the one pressed in a hurry")
	}
}

// A stop button offering to stop a silent device has nothing to do — see
// engine.Status.PlayingEventID, which is what this gates on.
func TestHomeHidesStopButtonWhenNothingIsPlaying(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `x-show="playing" style="display:none"`) {
		t.Errorf("a device playing nothing does not hide the stop button; body:\n%s", rec.Body.String())
	}
}

func TestHomeShowsStopButtonWhileSomethingIsPlaying(t *testing.T) {
	srv, _, backend := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	playReq := postForm("/play", url.Values{"sound": {"siren.wav"}})
	playReq.AddCookie(cookie)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), playReq)
	waitPlayed(t, backend)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `action="/stop"`) {
		t.Fatal("the stop button is missing from the page entirely")
	}
	if strings.Contains(body, `x-show="playing" style="display:none"`) {
		t.Errorf("the stop button stayed hidden while something is playing; body:\n%s", body)
	}
}

// The stream is what keeps this screen honest, so the screen has to be able
// to say when it has lost the stream. Without the badge — and without the
// onerror that raises it — a page whose EventSource died sat frozen on its
// last frame, and an alert that began afterwards showed as СПОКІЙНО.
//
// This test renders the template through text/template and reads the
// resulting bytes; it cannot execute the <script> block's JavaScript. So it
// can only pin two kinds of thing: that the badge markup exists and starts
// hidden, and that specific snippets of the probe/back-off source are
// present and wired into the right branch (inside the CLOSED check, not
// merely somewhere in the file). It cannot prove any of that code actually
// behaves correctly at runtime — an unconditional location.reload() added as
// probe()'s first line, or a back-off that grows without bound while the
// capped formula sits unused nearby, would both leave every assertion below
// green, because both still contain the pinned substrings. The runtime
// behaviour — a closed stream defers to a HEAD probe rather than reloading
// outright, a network error retries instead of reloading, and the delay
// widens and caps instead of hammering a booting device — was verified by
// hand, tracing the fetch/promise branches against the daemon-restarted,
// device-gone, and session-expired cases; see the task report for that
// trace. Nothing here is evidence the back-off behaves; it only stops the
// wiring from silently regressing.
func TestHomeCarriesAStalenessBadgeAndAStreamErrorHandler(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	// A freshly served page is not stale, so the badge starts hidden with
	// exactly the inline style x-show itself sets — the same pairing the two
	// state blocks use, which is what makes the no-JS and Alpine renders
	// identical.
	const badge = `data-state="stale" x-show="stale" style="display:none"`
	start := strings.Index(body, badge)
	if start < 0 {
		t.Fatalf("the page has no hidden staleness badge; body: %q", body)
	}
	end := strings.Index(body[start:], "</p>")
	if end < 0 {
		t.Fatal("the staleness badge is never closed")
	}
	markup := body[start : start+end]
	if !strings.Contains(markup, "застарілі") {
		t.Error("the staleness badge does not say in words that the data is stale")
	}
	if !strings.Contains(markup, "<svg") {
		t.Error("the staleness badge carries no shape, only colour and a word")
	}

	// The badge is useless unless something raises it, and a browser that has
	// closed the stream for good never retries on its own.
	if !strings.Contains(body, "onerror") {
		t.Error("nothing handles the status stream failing")
	}
	if !strings.Contains(body, "EventSource.CLOSED") {
		t.Error("the page cannot tell a retrying stream from one the browser gave up on")
	}

	// A closed stream must not reload straight away: that would replace a
	// readable stale reading with a browser error page the moment the device
	// is merely gone, rather than restarted. It has to ask first. Matched by
	// regexp rather than a literal, indentation-sensitive string, so
	// reformatting the template does not break this test for cosmetic
	// reasons — it still requires probe() to be the statement inside the
	// CLOSED branch, not merely present somewhere in the file.
	closedCallsProbe := regexp.MustCompile(`EventSource\.CLOSED\)\s*\{\s*probe\(\);`)
	if !closedCallsProbe.MatchString(body) {
		t.Error("a closed stream reloads directly instead of probing first")
	}
	if !strings.Contains(body, `fetch('/', { method: 'HEAD', cache: 'no-store' })`) {
		t.Error("nothing probes the device with a HEAD request before reloading")
	}
	// Reloading is what the probe is for, but only once the device actually
	// answered. A restarted daemon redirects this HEAD to /login with a 303;
	// fetch's default redirect: 'follow' keeps the HEAD method and follows
	// it, so the settled response here is /login's own 200 — r.ok is what
	// reloads, not a check against 303, which a browser fetch never reports.
	if !strings.Contains(body, "if (r.ok) location.reload();") {
		t.Error("the probe does not reload once the device answers")
	}
	// A network error (the case this change exists for: the device is
	// genuinely gone) must retry, not reload — reloading there is exactly
	// the browser error page this change removes.
	if !strings.Contains(body, "else retry();") || !strings.Contains(body, ".catch(retry);") {
		t.Error("the probe reloads (or gives up) on a network error instead of retrying")
	}
	// The back-off must widen rather than hammer a device that is still
	// booting, and it must be capped rather than grow without bound.
	if !strings.Contains(body, "probeDelay = Math.min(probeDelay * 2, 30000);") {
		t.Error("the probe's retry does not back off with a cap")
	}
}

func TestHomeOmitsTheMDNSNameWhenThereIsNone(t *testing.T) {
	// The test process has no avahi socket, so the line must not render at
	// all — not as an empty element, which would leave a stray label.
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), ".local") {
		t.Error("home page printed a .local name with nothing publishing one")
	}
}

// A disabled alert-start means the device will not announce an air raid — the
// worst outcome this product has, and the only defence is that it is
// impossible not to notice. This is the default-config case: enabled, so no
// warning.
func TestHomeShowsNoWarningWhenAlertStartIsEnabled(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "Оповіщення про тривогу вимкнено") {
		t.Error("the home page warns about a disabled alert-start that is not disabled")
	}
}

func TestHomeWarnsWhenAlertStartIsDisabled(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		for i := range c.Events {
			if c.Events[i].Trigger == config.TriggerAlertStart {
				c.Events[i].Enabled = false
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Оповіщення про тривогу вимкнено") {
		t.Error("the home page does not warn that alert-start is disabled")
	}
	if !strings.Contains(body, `data-variant="destructive"`) {
		t.Error("the warning is not rendered as a destructive alert")
	}
}

// Volume 0 is a second, silent way to reach the same outcome as disabling
// alert-start: rules.ForEvent treats it as an explicit mute. The events
// screen's own forms refuse to write it (see events_test.go), but nothing
// stops it arriving via a hand-edited config.json, and that is exactly the
// operator who most needs this warning.
func TestHomeWarnsWhenAlertStartVolumeIsZero(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		for i := range c.Events {
			if c.Events[i].Trigger == config.TriggerAlertStart {
				c.Events[i].Volume = 0
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Оповіщення про тривогу вимкнено") {
		t.Error("the home page does not warn when alert-start's volume is 0, even though it is still enabled")
	}
}

// homeBody logs in and returns the rendered dashboard. Every check below
// reads the same page, and repeating the four-line request dance once per
// assertion buried what each test was actually about.
func homeBody(t *testing.T, srv *web.Server) string {
	t.Helper()
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// newServerInAlert wires a server whose engine is already in the alert state,
// with no network, no poll loop and no real clock to wait on: engine.New
// seeds its mirrored alert fields straight from the alertstate.Machine it is
// handed, which is the same path a device that lost power mid-alert comes
// back up on. It is the only way this package can render the alarmed half of
// the banner, since handleHome reads Alert from the engine and a server with
// no engine is calm by definition.
//
// The player is handed no backend and never Run: engine.Status only asks it
// what is currently playing, which on a player that was never started is
// nothing.
func newServerInAlert(t *testing.T) *web.Server {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	region := store.Get().Alerts.Region
	machine := alertstate.New()
	machine.Restore(alertstate.Persisted{
		Region: region,
		Alert:  true,
		Since:  time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
	}, region)
	eng := engine.New(engine.Deps{
		Config:  store.Get,
		Machine: machine,
		Player:  player.New(nil, "default"),
	})
	srv, err := web.New(web.Deps{Config: store, Engine: eng})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// The operator asked for this wording by name: "останній успіх" described the
// poll, not the reading, and read as though something had succeeded rather
// than as when the device last heard anything. A rename nothing pins is a
// rename the next edit silently reverts — which has already happened twice on
// this project — so both halves are checked: the new words are there and the
// old ones are gone.
//
// Capitalised because the same words now open a line of their own rather than
// sitting mid-sentence after the source name — see
// TestHomePutsTheLastUpdateOnItsOwnLine.
func TestHomeSaysWhenTheSourceLastUpdated(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	if !strings.Contains(body, "Останнє оновлення") {
		t.Error("the source line does not say «останнє оновлення»")
	}
	if strings.Contains(body, "останній успіх") {
		t.Error("the source line still carries the old «останній успіх» wording")
	}
}

// stateCard returns the markup of the Стан card alone.
//
// Every assertion about that card has to be anchored inside it. The addresses
// in particular would satisfy a whole-page check from anywhere on the page —
// including the separate card this change removed, which is the exact
// regression worth catching.
func stateCard(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, ">Стан</h2>")
	if start < 0 {
		t.Fatal("the page has no Стан card")
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatal("the Стан card is never closed")
	}
	return body[start : start+end]
}

// The dashboard used to carry a separate «Адреса пристрою» card and, in Стан,
// a «Мережа: ✓ є IP-адреса» line — the same fact told twice, the weaker half
// first. The addresses are now the network line's own content, and they keep
// both jobs they had: the size they are read at off a screen across a room,
// and their standing as the page's network-health signal.
//
// What this can check from outside the package is bounded by
// web.LocalAddresses() reading the real machine. On a host holding addresses
// it pins that each one is rendered inside the Стан card; on a host holding
// none it pins the other half, the reading an operator gets when the device
// is not on the network. Both are real assertions — which one runs is the
// host's business. TestHomeViewMDNSFieldReachesTheTemplate covers the
// hand-built view that web_test cannot construct.
func TestHomeFoldsTheDeviceAddressesIntoTheStateCard(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	if strings.Contains(body, "Адреса пристрою") {
		t.Error("the separate address card is still on the page, telling the same thing twice")
	}
	state := stateCard(t, body)

	// No "Мережа:" label is asserted any more: when the device holds an
	// address, the address is the line. The label survives only in the
	// no-address case below, where there is nothing to show and the absence
	// has to be named — a blank space would read as a rendering fault.
	addrs := web.LocalAddresses()
	if len(addrs) == 0 {
		if !strings.Contains(state, "не в мережі") {
			t.Error("holding no address at all does not read as «not on the network» in the Стан card")
		}
		return
	}
	for _, addr := range addrs {
		if !strings.Contains(state, `<p class="font-mono text-2xl">`+addr+`</p>`) {
			t.Errorf("the address %s is not in the Стан card, in the size it is read at across a room", addr)
		}
	}
}

// «Джерело: X, останнє оновлення Y» was one sentence, and the timestamp in it
// is the one thing in this card checked against a clock rather than read.
// Buried mid-sentence it was something to parse; on its own line it is
// something to glance at.
//
// The binding comes with it. The page is driven by the SSE stream and a
// binding dropped in a reflow stops updating with nothing failing — the page
// keeps showing the server's first reading for ever.
func TestHomePutsTheLastUpdateOnItsOwnLine(t *testing.T) {
	srv, _ := newServerWithStore(t)
	state := stateCard(t, homeBody(t, srv))

	if !strings.Contains(state, `<p>Останнє оновлення: <span class="font-mono" x-text="lastSuccess">`) {
		t.Error("the last-update time is not a line of its own with its live binding intact")
	}

	start := strings.Index(state, "<p>Джерело:")
	if start < 0 {
		t.Fatal("the Стан card no longer names the alert source")
	}
	end := strings.Index(state[start:], "</p>")
	if end < 0 {
		t.Fatal("the source line is never closed")
	}
	if line := state[start : start+end]; strings.Contains(line, "оновлення") {
		t.Errorf("the source line still carries the last-update time inside it: %q", line)
	}
}

// The layout renders the tab title on every page, at the moment that page was
// served. This page alone also keeps it true afterwards, off the SSE stream it
// already opens — giving the other screens the same thing would mean a second
// EventSource on this very page.
//
// The sentence is therefore spelled in two places, layout.html's alertState
// helper and this component, and both are pinned here together: a rename on
// either side has to be deliberate, and the tab that says «Спокійно» over a
// device in alert is not a failure anything else would catch.
func TestHomeKeepsTheTabTitleLive(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	if !strings.Contains(body, "this.retitle();") {
		t.Error("the stream handler no longer retitles the tab; it will show the first reading for ever")
	}
	if !strings.Contains(body, `document.title = (this.alert ? 'Тривога' : 'Спокійно') + ' | Сполох — ' + this.region;`) {
		t.Error("the live tab title is not built from the state and the region the stream carries")
	}
	if !strings.Contains(body, "<title>Спокійно | Сполох — ") {
		t.Error("the server-rendered tab title is not the sentence the stream will replace it with")
	}
}

// The dashboard is the one screen an operator scans rather than reads, so it
// widens past the shell's reading measure and lays its reference cards out
// two abreast. sm:grid-cols-2 is what does that, and it is checked as a
// literal class name because Tailwind compiles only the class names it can
// see in the template — a class assembled at render time would leave this
// assertion green and the page unstyled.
func TestHomeLaysTheReferenceCardsOutAsAGrid(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	// Read the width off the rendered <main> rather than matching
	// layout.html's whole tag: the shell is not this page's to own, and only
	// the measure it applies here is.
	start := strings.Index(body, "<main")
	if start < 0 {
		t.Fatal("the page has no <main> element")
	}
	end := strings.Index(body[start:], ">")
	mainTag := body[start : start+end]
	if !strings.Contains(mainTag, "max-w-5xl") {
		t.Errorf("the dashboard does not widen its container; <main> is %q", mainTag)
	}
	if strings.Contains(mainTag, "max-w-3xl") {
		t.Errorf("the dashboard still renders at the shell's default measure; <main> is %q", mainTag)
	}

	if !strings.Contains(body, "sm:grid-cols-2") {
		t.Error("the reference cards are not laid out in a grid that widens past one column")
	}
	// One column on a phone is the default the sm: prefix widens from, and
	// the device is routinely set up from a phone on its own access point.
	// A two-column grid with no breakpoint would squeeze that layout
	// instead.
	// Anchored on a space or the opening quote rather than a word boundary,
	// which "sm:grid-cols-2" would also satisfy — the very class this is
	// meant to let through. The column count is matched as a digit rather
	// than spelled out for two reasons: any unprefixed column count is the
	// same mistake, and Tailwind scans this file too, so a complete class
	// name written here would have put a rule no page uses into the
	// compiled stylesheet.
	if regexp.MustCompile(`class="(?:[^"]* )?grid-cols-\d`).MatchString(body) {
		t.Error("the card grid is two columns at every width, including a phone's")
	}
}

// The banner is the reason the device exists and the disabled-alerts warning
// above it is the one warning that must be impossible to miss, so neither may
// be pulled into the card grid to share a row with reference material. Both
// stay direct children of the page's own grid, which in the rendered markup
// means both come before the grid container opens.
func TestHomeKeepsTheBannerAndItsWarningOutOfTheCardGrid(t *testing.T) {
	srv, store := newServerWithStore(t)
	if err := store.Update(func(c *config.Config) error {
		for i := range c.Events {
			if c.Events[i].Trigger == config.TriggerAlertStart {
				c.Events[i].Enabled = false
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	body := homeBody(t, srv)

	warning := strings.Index(body, "Оповіщення про тривогу вимкнено")
	banner := strings.Index(body, `data-state="alert"`)
	grid := strings.Index(body, "sm:grid-cols-2")
	if warning < 0 || banner < 0 || grid < 0 {
		t.Fatalf("warning at %d, banner at %d, grid at %d: all three must be on the page", warning, banner, grid)
	}
	if warning > banner {
		t.Error("the disabled-alerts warning is rendered below the state banner instead of above it")
	}
	if banner > grid {
		t.Error("the state banner has been pulled into the card grid, where it shares a row")
	}
}

// Both halves of the banner are always in the markup and the server hides the
// one that is not current, so that a browser with no JavaScript sees the
// state the device actually holds. TestHomeSignalsStateWithMoreThanColour
// covers the calm device; this covers the other half, which the grid rebuild
// could have broken without any calm-device test noticing.
func TestHomeRendersTheAlarmedBannerOnAnAlertingDevice(t *testing.T) {
	body := homeBody(t, newServerInAlert(t))

	if strings.Contains(body, `data-state="alert" x-show="alert" style="display:none"`) {
		t.Error("an alerting device hides its own alarmed block")
	}
	if !strings.Contains(body, `data-state="calm" x-show="!alert" style="display:none"`) {
		t.Error("an alerting device renders the calm block visible")
	}
	if !strings.Contains(body, "ТРИВОГА") {
		t.Error("the alarmed block does not spell out the state")
	}
}

// Every binding on this page is driven by the SSE stream at /stream, and a
// binding dropped during a layout change stops updating silently — the page
// keeps rendering the server's first reading for ever and nothing fails.
// This is the inventory, pinned verbatim so a rename on either side (here or
// in the status() object below the markup) has to be deliberate.
func TestHomeKeepsEveryLiveBinding(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	for _, binding := range []string{
		`x-data="status()"`,
		`x-init="connect()"`,
		`x-show="alert"`,
		`x-show="!alert"`,
		`x-show="stale"`,
		`x-show="playing"`,
		`x-show="failing"`,
		`x-text="region"`,
		`x-text="since ? 'з ' + since : ''"`,
		`x-text="nextEvent"`,
		`x-text="nextEventAt"`,
		`x-text="clockSynced ? '✓ синхронізовано' : '✕ НЕ синхронізовано'"`,
		`x-text="source"`,
		`x-text="lastSuccess"`,
		`x-text="lastError ? '✕ ' + lastError : ''"`,
	} {
		if !strings.Contains(body, binding) {
			t.Errorf("the live binding %s is gone from the page; it will stop updating with nothing to say so", binding)
		}
	}
}

// The map is a third-party embed — the single approved exception to this
// project's no-CDN rule — and the two ways it can go wrong are both about
// what the operator concludes, not about whether it paints.
//
// What this test cannot do: Go renders bytes, it does not run a browser. It
// cannot load the iframe, cannot prove the map appears, cannot prove the
// aspect-ratio box is the shape it claims, and cannot prove the page still
// reads sensibly on a device serving its own access point with no internet
// at all. That last case was checked by hand. What is pinned here is only
// what is in the markup: the operator's own src and title, the frame being
// fluid rather than the 500px the snippet came with, a caption naming the
// source in words, and the map staying below the banner and shaped nothing
// like it.
func TestHomeEmbedsTheAlertMapAsALabelledFluidFrame(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	iframe := strings.Index(body, "<iframe")
	if iframe < 0 {
		t.Fatal("the page carries no map embed")
	}
	if !strings.Contains(body, `src="https://alerts.in.ua/?embed"`) {
		t.Error("the embed does not point at the source the operator asked for")
	}
	if !strings.Contains(body, `title="Мапа повітряних тривог"`) {
		t.Error("the embed carries no title, so a screen reader announces an unnamed frame")
	}
	if !strings.Contains(body, `loading="lazy"`) {
		t.Error("the embed is not lazy, so a device with no internet spends its page load on it")
	}
	// The operator's snippet fixed the frame at 500x300, which overflows a
	// phone. The size has to come from the container instead.
	if strings.Contains(body, "500px") {
		t.Error("the embed still carries the snippet's fixed 500px width")
	}

	// The banner is what drives the siren; the map is what a third party
	// believes. Reading the map as the device's state is the misreading this
	// page cannot afford, so the map comes after the banner and is styled as
	// an ordinary card — none of the banner's filled destructive colour.
	banner := strings.Index(body, `data-state="alert"`)
	if banner < 0 || banner > iframe {
		t.Errorf("banner at %d, map at %d: the map must come below the state banner", banner, iframe)
	}
	start := strings.LastIndex(body[:iframe], "<section")
	end := strings.Index(body[iframe:], "</section>")
	if start < 0 || end < 0 {
		t.Fatal("the map embed is not inside a section of its own")
	}
	card := body[start : iframe+end]
	// With the embed URL taken out, what is left is what an operator can
	// actually read. Searching the whole card would have been satisfied by
	// the src attribute alone, which names the source to nobody.
	labels := strings.Replace(card, `src="https://alerts.in.ua/?embed"`, "", 1)
	if !strings.Contains(labels, "alerts.in.ua") {
		t.Error("nothing an operator can read names alerts.in.ua as the source, so the map reads as the device's own status")
	}
	if strings.Contains(card, "destructive") {
		t.Error("the map card is styled like the alert banner")
	}
	if !strings.Contains(card, "aspect-[") {
		t.Error("the map has no aspect-ratio box, so it has no height of its own to be fluid within")
	}
}

// The dashboard must say, standing and unmissable, when the device is
// configured to choose silence during a raid. /events is where it is turned
// on; the home page is where someone who did not turn it on finds out.
func TestHomeWarnsWhenQuietHoursSilenceAlerts(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.QuietHours = config.QuietHours{
			Enabled:   true,
			From:      "22:00",
			To:        "07:00",
			AppliesTo: []string{config.CategoryScheduled, config.CategoryAlerts},
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "НЕ подаватиме сигнал повітряної тривоги") {
		t.Errorf("the dashboard does not warn that alerts are silenced; body:\n%s", body)
	}
	for _, edge := range []string{"22:00", "07:00"} {
		if !strings.Contains(body, edge) {
			t.Errorf("the warning does not name the hour %s; body:\n%s", edge, body)
		}
	}
}

// A window that does not cover alerts silences none of them, and warning
// about it would teach an operator to skip the one warning that matters.
func TestHomeDoesNotWarnWhenQuietHoursSpareAlerts(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.QuietHours = config.QuietHours{
			Enabled:   true,
			From:      "22:00",
			To:        "07:00",
			AppliesTo: []string{config.CategoryScheduled},
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "НЕ подаватиме сигнал повітряної тривоги") {
		t.Error("the dashboard warns about silenced alerts when the window does not cover them")
	}
}

// Стан before Найближча подія. What the device is doing right now outranks
// what it will do later, and the card an operator opens the page to read
// should not be the second one their eye lands on.
func TestHomePutsStateBeforeTheNextEvent(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	state := strings.Index(body, `>Стан</h2>`)
	next := strings.Index(body, `>Найближча подія</h2>`)
	if state < 0 || next < 0 {
		t.Fatalf("both cards must render; Стан at %d, Найближча подія at %d", state, next)
	}
	if state > next {
		t.Error("Найближча подія comes before Стан; current state outranks the schedule")
	}
}
