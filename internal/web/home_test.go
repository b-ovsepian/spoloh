package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
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
	// LevelUnknown, which is the level of every raid this package rendered
	// before levels existed and of every raid a Ubilling source reports.
	return newServerInAlertAtLevel(t, alertsource.LevelUnknown)
}

// newServerInAlertAtLevel is newServerInAlert with the severity upstream
// named, restored the same way the rest of the alert is: alertstate.Persisted
// carries the level precisely so a board that lost power mid-raid comes back
// announcing the severity it was already announcing.
func newServerInAlertAtLevel(t *testing.T, level alertsource.Level) *web.Server {
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
		Level:  level,
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
// their standing as the page's network-health signal.
//
// They are one labelled «IP адреса:» line now, not a stack of oversized
// paragraphs: at text-2xl they shouted between four ordinary lines and broke
// the card's rhythm, which is what the operator reported. What is pinned is
// the label and the address inside the card — not the size, which was the
// thing that was wrong.
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

	addrs := web.LocalAddresses()
	// The «Мережа:» label survives only in the no-address case, where there
	// is nothing to show and the absence has to be named — a blank space
	// would read as a rendering fault.
	if len(addrs) == 0 {
		if !strings.Contains(state, "не в мережі") {
			t.Error("holding no address at all does not read as «not on the network» in the Стан card")
		}
		return
	}
	if !strings.Contains(state, "IP адреса:") {
		t.Error("the address line carries no label, so it does not read like the rest of the card")
	}
	for _, addr := range addrs {
		if !strings.Contains(state, addr) {
			t.Errorf("the address %s is not in the Стан card at all", addr)
		}
	}
	// The size was the complaint. text-2xl anywhere in this card means the
	// oversized paragraphs came back, in this line or in another one copied
	// from it.
	if strings.Contains(state, "text-2xl") {
		t.Error("the Стан card is shouting a line again; every line here is the card's ordinary size")
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
		`x-text="levelLabel"`,
		`x-text="levelNote"`,
		`x-show="levelNote"`,
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

	// The map now sits above the banner, on the operator's instruction: it
	// is the country at a glance and it was at the bottom of a page nobody
	// scrolled. What still has to hold is the thing the old ordering was
	// protecting — the map is what a third party believes, the banner is
	// what drives the siren, and the map must never be readable as the
	// device's own state. So the banner must still render, and the map must
	// still be an ordinary card with none of the banner's filled
	// destructive colour (checked below).
	banner := strings.Index(body, `data-state="alert"`)
	if banner < 0 {
		t.Error("the state banner is not on the page at all")
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

// cardNamed returns the markup of one reference card, found by its heading.
//
// Every assertion about a card has to be anchored inside it. The dashboard
// now carries six, several of which talk about the same things — a sound
// name appears in Ручне відтворення, in Останні дії and in the Звук card's
// test button — so a whole-page check is satisfied from anywhere and proves
// nothing about where an operator will actually read it.
func cardNamed(t *testing.T, body, heading string) string {
	t.Helper()
	start := strings.Index(body, ">"+heading+"</h2>")
	if start < 0 {
		t.Fatalf("the page has no %s card", heading)
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatalf("the %s card is never closed", heading)
	}
	return body[start : start+end]
}

// The Останні дії card is what answers the question no other card on this
// page does: did this device actually make a sound the last time it
// mattered. A dashboard where every other card reads healthy over a device
// that has played nothing successfully in a month is the failure this
// product most needs noticed, and until now finding that out meant
// navigating away to /log.
//
// Newest first, and bounded — the card is a summary, and the rest of the
// history is one link away.
func TestHomeShowsTheLastFewActionsAndLinksToTheRest(t *testing.T) {
	srv, _, lg := newServerWithLog(t)

	// Seven, so the bound itself is exercised: the two oldest must not be
	// on the card. Appended oldest first, which is the order the daemon
	// writes them in.
	for _, name := range []string{
		"oldest-one.wav", "oldest-two.wav", "third.wav",
		"fourth.wav", "fifth.wav", "sixth.wav", "newest.wav",
	} {
		appendAlert(t, lg, name)
	}

	card := cardNamed(t, homeBody(t, srv), "Останні дії")

	if !strings.Contains(card, "newest.wav") {
		t.Error("the newest entry is not on the card; the card is not showing recent activity at all")
	}
	for _, old := range []string{"oldest-one.wav", "oldest-two.wav"} {
		if strings.Contains(card, old) {
			t.Errorf("%s is on the card: it has stopped being a summary and become a list to scroll past", old)
		}
	}
	// /log's own vocabulary for the same rows, built by its kindLabel — an
	// operator comparing this card against /log is doing it on purpose, and
	// two names for one fact would make that a puzzle.
	if !strings.Contains(card, "Тривога") {
		t.Error("the card does not label the entry's kind the way /log does")
	}
	if !strings.Contains(card, `href="/log"`) {
		t.Error("the card does not link to the rest of the history, so the five rows read as everything that happened")
	}
}

// A failed entry has to be told from a successful one at a glance, without
// reading either. Colour alone will not do it — the same rule the state
// banner keeps — so the failed row carries a thick bar down its leading edge
// and a tinted background as well as the ✕ and the word.
//
// The assertion is on the markup that differs between the two rows. Checking
// only for the word «Помилка» would pass against a card that renders both
// rows identically apart from that word, which is exactly the regression
// this is here to catch.
func TestHomeMarksAFailedActionApartFromASuccessfulOne(t *testing.T) {
	srv, _, lg := newServerWithLog(t)

	appendAlert(t, lg, "worked.wav")
	if err := lg.Append(eventlog.Entry{
		Kind:  eventlog.KindPlaybackError,
		Sound: "broke.wav",
		OK:    false,
		Err:   "audio open error: Unknown error 524",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	card := cardNamed(t, homeBody(t, srv), "Останні дії")

	if got := strings.Count(card, "border-l-destructive"); got != 1 {
		t.Errorf("%d rows carry the failure bar, want exactly 1: the failed entry is not marked apart from the one that worked", got)
	}
	if got := strings.Count(card, "bg-destructive/10"); got != 1 {
		t.Errorf("%d rows are tinted as failures, want exactly 1", got)
	}
	if !strings.Contains(card, "✕ Помилка") {
		t.Error("the failed row is marked by colour and shape alone; it must carry the word too")
	}
	if !strings.Contains(card, "✓ ОК") {
		t.Error("the successful row carries no outcome at all, so there is nothing for the failure to stand against")
	}
	// The raw error is what an operator takes to /audio or reads over the
	// phone; a row that says only "Помилка" sends them to /log for it.
	if !strings.Contains(card, "Unknown error 524") {
		t.Error("the failed row does not carry the error itself")
	}
}

// Three ways there is nothing to show, and they mean very different things:
// no log on this device at all (a corrupt database, a read-only card), and a
// log that is simply empty. Both must read as themselves rather than as a
// blank card — the same three-way separation /log already makes.
func TestHomeSaysWhyThereIsNoRecentActivity(t *testing.T) {
	t.Run("no log on the device", func(t *testing.T) {
		srv, _ := newServerWithStore(t) // Deps.Log is nil
		card := cardNamed(t, homeBody(t, srv), "Останні дії")
		if !strings.Contains(card, "Журнал подій недоступний на цьому пристрої.") {
			t.Errorf("a device with no event log does not say so; card:\n%s", card)
		}
	})

	t.Run("nothing logged yet", func(t *testing.T) {
		srv, _, _ := newServerWithLog(t)
		card := cardNamed(t, homeBody(t, srv), "Останні дії")
		if !strings.Contains(card, "Записів поки немає.") {
			t.Errorf("an empty log reads the same as a missing one; card:\n%s", card)
		}
		if strings.Contains(card, "недоступний") {
			t.Error("an empty log is reported as an unavailable one; they need different fixes")
		}
	})
}

// The Звук card states what the device will play through, in the words
// /audio already chose for it — the stored PCM verbatim, because that is the
// string that has to be recognised in a log line or an aplay command typed
// over SSH, plus the volume and the mixer control that sets it.
func TestHomeShowsTheConfiguredOutputAndItsVolume(t *testing.T) {
	srv, _, _ := newServerWithTestSound(t)
	card := cardNamed(t, homeBody(t, srv), "Звук")

	if !strings.Contains(card, "default") {
		t.Errorf("the card does not name the stored output; card:\n%s", card)
	}
	if !strings.Contains(card, "80 %") {
		t.Errorf("the card does not carry the master volume; card:\n%s", card)
	}
	if !strings.Contains(card, `href="/audio"`) {
		t.Error("the card does not link to the screen that changes any of this")
	}
}

// The test button is a preview, and it has to say so. /play serves both the
// home page's manual fire — deliberately PriorityAlert, so a scheduled chime
// cannot cut it short — and this button; without the marker, somebody
// finding out whether the speaker works would hold the single voice at alert
// priority with a real air raid alert queued behind them.
//
// Pinned on the markup, the same way TestSoundsPreviewFormsDeclareThemselves
// Previews is, because the priority itself is chosen inside the engine and
// this field is the only signal that reaches it. The submission below then
// proves the form is wired to something rather than merely well-spelled.
func TestHomeTestSoundPlaysAtPreviewPriority(t *testing.T) {
	srv, _, backend := newServerWithTestSound(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	card := cardNamed(t, rec.Body.String(), "Звук")

	if !strings.Contains(card, `action="/play"`) {
		t.Fatalf("the test button does not post to /play; card:\n%s", card)
	}
	if !strings.Contains(card, `<input type="hidden" name="preview" value="1">`) {
		t.Error("the test button is not marked a preview: it plays at alert priority and can hold the speaker while a siren waits")
	}
	if !strings.Contains(card, `<input type="hidden" name="return" value="/">`) {
		t.Error("the test button does not name this page as its return, so pressing it walks the operator off the dashboard")
	}

	// And it reaches the player. testSound prefers all-clear.wav, and
	// deliberately not the siren: whoever presses this is standing next to
	// a PA system.
	form := url.Values{"sound": {"all-clear.wav"}, "preview": {"1"}, "return": {"/"}}
	play := httptest.NewRequest(http.MethodPost, "/play", strings.NewReader(form.Encode()))
	play.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	play.Header.Set("Origin", "http://"+play.Host)
	play.AddCookie(cookie)
	playRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(playRec, play)

	if playRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /play = %d, want 303", playRec.Code)
	}
	if got := waitPlayed(t, backend); !strings.HasSuffix(got, "all-clear.wav") {
		t.Errorf("the player was handed %q, want the test sound", got)
	}
}

// Three states, not two: the output opened, the output would not open, and
// nobody asked. The third is as real as either — it is what this card shows
// while a siren is playing, because it must not put a second opener on the
// sound card during the one minute this device exists for, and what it shows
// when the two-second budget runs out.
//
// Claiming the output works when nothing opened it would be the dashboard
// telling an operator their speakers are fine on the strength of no evidence
// at all, which is the one thing a card about audibility must never do.
func TestHomeClaimsNothingAboutAnOutputItDidNotProbe(t *testing.T) {
	// deviceListerFunc lists outputs but cannot open one — it does not
	// satisfy the prober assertion — so this is the "nobody could be
	// asked" case exactly.
	srv, _, _ := newServerWithTestSound(t)
	card := cardNamed(t, homeBody(t, srv), "Звук")

	if !strings.Contains(card, "невідомий") {
		t.Errorf("the card does not say the output's state is unknown; card:\n%s", card)
	}
	if strings.Contains(card, "Перевірено щойно") {
		t.Error("the card says the output was checked when nothing opened it")
	}
	if strings.Contains(card, "не відкривається") {
		t.Error("the card calls an output broken on the strength of a probe that never ran")
	}
}

// The operator's own board: the stored output is "default", "default"
// resolves to the HDMI card, and with nothing plugged into HDMI it will not
// open at all. That is the state a fresh install ships in, and the dashboard
// is where it has to be visible.
func TestHomeSaysWhenTheConfiguredOutputWillNotOpen(t *testing.T) {
	srv, _ := newServerWithProber(t, realBoardProber())
	card := cardNamed(t, homeBody(t, srv), "Звук")

	if !strings.Contains(card, "звуку не буде") {
		t.Errorf("the card does not say that an output which will not open means no sound; card:\n%s", card)
	}
	if strings.Contains(card, "Перевірено щойно") {
		t.Error("the card reports a refused output as a working one")
	}
}

// Every dependency this page reads is allowed to be nil — a daemon whose
// sound directory would not open, a board whose ALSA is broken — and the
// dashboard has to render regardless. A dashboard that 500s because aplay is
// missing is a dashboard nobody can use to find out why.
func TestHomeRendersWithNoAudioDependenciesAtAll(t *testing.T) {
	srv, _ := newServerWithStore(t) // no Devices, no Library, no Engine
	card := cardNamed(t, homeBody(t, srv), "Звук")

	if !strings.Contains(card, "Список аудіопристроїв недоступний на цьому пристрої.") {
		t.Errorf("the card does not say why it knows nothing about the outputs; card:\n%s", card)
	}
	// A button whose only possible outcome is a failure message teaches an
	// operator to distrust the page.
	if strings.Contains(card, `action="/play"`) {
		t.Error("the card renders a test button with an empty library behind it")
	}
	if !strings.Contains(card, "немає жодного звуку") {
		t.Error("the card does not say why there is no test button")
	}
}

// Seven cards in a two-column grid leaves a hole unless three of them are
// told to span it. Останні дії is one — it is a list of rows and reads better
// across the full measure, the same argument /log makes for its own table —
// the map is the second, which it already was, and Стан джерел is the third:
// a cascade can hold seven keys, and setting them in a narrow strip beside a
// mostly empty neighbour would waste the width they need.
//
// The order is the claim: what the device is doing now, then what it will do
// next, then where that information came from, then the two controls, then
// the evidence that any of it works, then the third party's map last. The
// banner and both standing warnings stay outside the grid entirely, which
// TestHomeKeepsTheBannerAndItsWarningOutOfTheCardGrid pins.
func TestHomeLaysTheCardsOutWithoutARaggedHole(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	// The map is no longer in this list: it was lifted out of the grid to
	// sit under the header, so the grid holds six cards and the map's own
	// placement is pinned by TestHomeEmbedsTheAlertsMap.
	order := []string{"Стан", "Найближча подія", "Стан джерел", "Ручне відтворення", "Звук", "Останні дії"}
	at := make([]int, len(order))
	for i, heading := range order {
		at[i] = strings.Index(body, ">"+heading+"</h2>")
		if at[i] < 0 {
			t.Fatalf("the %s card is not on the page", heading)
		}
		if i > 0 && at[i-1] > at[i] {
			t.Errorf("%s comes before %s", order[i], order[i-1])
		}
	}

	// Two full-width cards among six is what makes the rows come out even.
	// Counted as a literal class name because Tailwind compiles only the
	// names it can see in the template.
	if got := strings.Count(body, "sm:col-span-2"); got != 2 {
		t.Errorf("%d cards span both columns, want 2: six cards in a two-column grid leave a ragged hole otherwise", got)
	}
	// The span lives on the <section> tag, which opens before the heading
	// cardNamed anchors on — so this reads the tag itself.
	for _, heading := range []string{"Останні дії", "Стан джерел"} {
		at := strings.Index(body, ">"+heading+"</h2>")
		open := strings.LastIndex(body[:at], "<section")
		if open < 0 {
			t.Fatalf("the %s heading is not inside a section", heading)
		}
		if !strings.Contains(body[open:at], "sm:col-span-2") {
			t.Errorf("%s does not span the grid, so the row it shares ends short", heading)
		}
	}
}

// cascadeRegion is the oblast the Стан джерел tests monitor. It is not the
// default one: the card has to render the location the device is actually
// watching, and a test against the value that would appear anyway if the
// field were dropped proves nothing.
const cascadeRegion = "Львівська область"

// newServerAfterPoll wires a dashboard over an engine that has polled once,
// through a real alertsource.Client, against a fake upstream that answers
// each cascade key however the test says.
//
// The client is the real one on purpose. What these tests are about is the
// account a cascade gives of itself — which key answered, which failed and
// why, which were never asked — and a hand-written source reporting attempts
// of its own invention could describe a cascade the real code never produces.
// The upstream is a local httptest server, so nothing here dials the
// internet.
//
// reply is called with the "?source=" key of each request, in cascade order.
func newServerAfterPoll(t *testing.T, keys []string, reply func(key string, w http.ResponseWriter)) *web.Server {
	t.Helper()
	srv, eng := dashboardOverUpstream(t, keys, reply)
	// One poll, synchronously: the card describes the last cascade, and a
	// dashboard rendered before any poll has a different thing to say.
	// Whether it succeeded is the test's business, not this helper's.
	_ = eng.PollOnce(context.Background())
	return srv
}

// dashboardOverUpstream builds the store, the engine and the server, and
// polls nothing. Tests that need a particular engine state — a cascade
// walked once, an all-clear part way through its run — drive the polls
// themselves.
func dashboardOverUpstream(t *testing.T, keys []string, reply func(key string, w http.ResponseWriter)) (*web.Server, *engine.Engine) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(r.URL.Query().Get("source"), w)
	}))
	t.Cleanup(upstream.Close)

	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Alerts.Region = cascadeRegion
		c.Alerts.Sources = keys
		return nil
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	eng := engine.New(engine.Deps{
		Config:  store.Get,
		Source:  alertsource.New(upstream.URL, keys, upstream.Client()),
		Machine: alertstate.New(),
		Player:  player.New(nil, "default"),
		Clock:   clock.System{Loc: time.UTC},
	})
	srv, err := web.New(web.Deps{Config: store, Engine: eng})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, eng
}

// answerState replies with a usable snapshot for the monitored oblast.
func answerState(w http.ResponseWriter, alert bool) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"source":"test","cachedat":"2026-09-12 10:00:00","states":{%q:{"alertnow":%t,"changed":"2026-09-12 09:00:00"}}}`, cascadeRegion, alert)
}

func answerCalm(w http.ResponseWriter) { answerState(w, false) }

// newServerHoldingAnAllClear wires a dashboard over a device that upstream
// says is calm and that is still announcing a raid: the window between the
// first calm reading and the third, which is the state this counter exists
// to explain.
//
// Two real polls rather than a hand-set field. The first adopts the raid
// silently, exactly as a cold start does; the second is the calm reading
// that starts the run. Anything faked here would be free to produce a run
// alertstate.Machine never actually holds.
func newServerHoldingAnAllClear(t *testing.T) *web.Server {
	t.Helper()

	var mu sync.Mutex
	alert := true
	srv, eng := dashboardOverUpstream(t, []string{"default"}, func(_ string, w http.ResponseWriter) {
		mu.Lock()
		a := alert
		mu.Unlock()
		answerState(w, a)
	})

	ctx := context.Background()
	if err := eng.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce adopting the raid: %v", err)
	}
	mu.Lock()
	alert = false
	mu.Unlock()
	if err := eng.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce on the first calm reading: %v", err)
	}

	if got := eng.Status().AllClearSeen; got != 1 {
		t.Fatalf("the engine holds %d calm readings, want 1: this harness is not in the state it claims", got)
	}
	return srv
}

// The cascade stops at the first key that answers, so the keys behind it were
// never consulted. Showing them as working is a promise nothing checked —
// and it is the promise that has an operator walk away believing they have
// two fallbacks standing by.
func TestHomeSaysWhichSourcesTheCascadeNeverReached(t *testing.T) {
	srv := newServerAfterPoll(t, []string{"default", "skog", "aiu"},
		func(_ string, w http.ResponseWriter) { answerCalm(w) })
	card := cardNamed(t, homeBody(t, srv), "Стан джерел")

	for _, key := range []string{"default", "skog", "aiu"} {
		if !strings.Contains(card, key) {
			t.Errorf("the card does not name the configured key %q; card:\n%s", key, card)
		}
	}
	if got := strings.Count(card, "відповів"); got != 1 {
		t.Errorf("%d keys are reported as having answered, want exactly 1: the cascade stops at the first", got)
	}
	if got := strings.Count(card, "не опитувався"); got != 2 {
		t.Errorf("%d keys are reported as not consulted, want 2; card:\n%s", got, card)
	}
}

// A cascade that had to fall through twice: the two that failed say so and
// say why, and the third is the one actually answering for this device.
func TestHomeSaysWhyEachFailedSourceFailed(t *testing.T) {
	srv := newServerAfterPoll(t, []string{"default", "skog", "aiu"}, func(key string, w http.ResponseWriter) {
		switch key {
		case "default":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "skog":
			http.Error(w, "upstream is having a bad day", http.StatusInternalServerError)
		default:
			answerCalm(w)
		}
	})
	card := cardNamed(t, homeBody(t, srv), "Стан джерел")

	if !strings.Contains(card, "ліміт запитів") {
		t.Errorf("the throttled key does not say it was rate limited; card:\n%s", card)
	}
	if !strings.Contains(card, "відповідь не вдалося прочитати") {
		t.Errorf("the refusing key does not say its reply was unusable; card:\n%s", card)
	}
	if got := strings.Count(card, "не відповів"); got != 2 {
		t.Errorf("%d keys are reported as failed, want 2; card:\n%s", got, card)
	}
	if got := strings.Count(card, "не опитувався"); got != 0 {
		t.Errorf("%d keys are reported as not consulted, want 0 — the cascade walked all three", got)
	}
}

// Every source down is the poll this card exists for, and it is the one the
// reporting is easiest to lose: it travels out of PollOnce on the error path.
func TestHomeReportsACascadeWhereEverySourceFailed(t *testing.T) {
	srv := newServerAfterPoll(t, []string{"default", "skog"}, func(_ string, w http.ResponseWriter) {
		http.Error(w, "upstream is having a bad day", http.StatusInternalServerError)
	})
	card := cardNamed(t, homeBody(t, srv), "Стан джерел")

	if got := strings.Count(card, "не відповів"); got != 2 {
		t.Errorf("%d keys are reported as failed, want both; card:\n%s", got, card)
	}
	if strings.Contains(card, "✓ відповів") {
		t.Errorf("a source is reported as having answered when none did; card:\n%s", card)
	}
}

// The card has to say which place these sources are being asked about. A
// cascade reported without it is a list of verdicts about nothing in
// particular — and the oblast is changed on /alerts, by the same person
// reading this card.
func TestHomeSourcesCardNamesTheMonitoredLocation(t *testing.T) {
	srv := newServerAfterPoll(t, []string{"default"},
		func(_ string, w http.ResponseWriter) { answerCalm(w) })
	card := cardNamed(t, homeBody(t, srv), "Стан джерел")

	if !strings.Contains(card, cascadeRegion) {
		t.Errorf("the card does not name the monitored location %q; card:\n%s", cascadeRegion, card)
	}
}

// Upstream text must not reach this card. alerts.in.ua authenticates with a
// Bearer token, an error carries whatever the far end sent, and a wrapped
// error is all it would take to print a credential on a page anyone on the
// LAN can load. So the card renders alertsource's closed vocabulary and
// nothing else.
//
// The echoed text here is real, not contrived: alertsource quotes the
// upstream's own Content-Type in the error for a reply it cannot use, which
// is exactly the sort of far-end string that ends up inside an error value.
func TestHomeSourcesCardNeverRendersUpstreamErrorText(t *testing.T) {
	const fromUpstream = "text/html; charset=utf-8-this-came-from-upstream"
	srv := newServerAfterPoll(t, []string{"default"}, func(_ string, w http.ResponseWriter) {
		w.Header().Set("Content-Type", fromUpstream)
		_, _ = w.Write([]byte("<html>sign in to continue</html>"))
	})
	card := cardNamed(t, homeBody(t, srv), "Стан джерел")

	if strings.Contains(card, "utf-8-this-came-from-upstream") {
		t.Errorf("the card printed text the upstream chose; card:\n%s", card)
	}
	if !strings.Contains(card, "не відповів") {
		t.Errorf("the card does not report the source as having failed at all; card:\n%s", card)
	}
}

// Before the first poll nothing has asked anything, and that is not the same
// fact as a cascade of keys that were not reached — that one says a poll
// happened and stopped early. Rendering the configured list as unreached
// would be inventing a poll that has not happened.
func TestHomeSaysSoWhenNothingHasBeenPolledYet(t *testing.T) {
	srv, _ := newServerWithStore(t) // no engine at all
	card := cardNamed(t, homeBody(t, srv), "Стан джерел")

	if !strings.Contains(card, "ще не опитувалися") {
		t.Errorf("the card does not say that no poll has happened yet; card:\n%s", card)
	}
	if strings.Contains(card, "не опитувався") {
		t.Errorf("the card reports keys as unreached before any poll; card:\n%s", card)
	}
}

// allClearLine returns the opening tag of the held-back-all-clear line, so a
// test can tell a line that is on the page from one that is on the page and
// hidden. Both are rendered — x-show's own inline display:none is what a
// browser with scripts off obeys — so asserting the words alone would pass
// against a page that never shows them.
func allClearLine(t *testing.T, body string) string {
	t.Helper()
	card := cardNamed(t, body, "Стан")
	const open = `<p x-show="allClearSeen > 0"`
	at := strings.Index(card, open)
	if at < 0 {
		t.Fatalf("the Стан card has no held-back-all-clear line at all; card:\n%s", card)
	}
	// From the end of the x-show attribute, not from the start of the tag:
	// the condition itself contains a ">".
	end := strings.Index(card[at+len(open):], ">")
	if end < 0 {
		t.Fatalf("the held-back-all-clear line's tag is never closed; card:\n%s", card)
	}
	return card[at : at+len(open)+end+1]
}

// A device upstream says is calm and that is still announcing a raid looks,
// from the other side of a room, exactly like a device that has stopped
// working — and that is the reading that gets it unplugged, during a raid,
// by somebody who has concluded it is broken. The screen has to say that the
// wait is deliberate, and how far along it is.
func TestHomeSaysAnAllClearIsWaitingForConfirmation(t *testing.T) {
	body := homeBody(t, newServerHoldingAnAllClear(t))
	card := cardNamed(t, body, "Стан")

	if !strings.Contains(card, "чекає на підтвердження") {
		t.Errorf("the card does not say the all-clear is being confirmed; card:\n%s", card)
	}
	if strings.Contains(allClearLine(t, body), "display:none") {
		t.Errorf("the line is rendered hidden while an all-clear is actually being held back; card:\n%s", card)
	}
	// How far through the run, and how long the run is. The second number
	// is alertstate.EndConfirmations, which a page cannot read for itself.
	if !strings.Contains(card, ">1</span> з <span class=\"font-mono\" x-text=\"allClearNeeded\">3<") {
		t.Errorf("the card does not say how many confirmations have been seen out of how many; card:\n%s", card)
	}
}

// And nothing to say when nothing is being held back. A counter that is
// always on screen is a counter nobody reads on the one day it matters.
func TestHomeHidesTheAllClearLineWhenNothingIsHeldBack(t *testing.T) {
	srv, _ := newServerWithStore(t)
	if line := allClearLine(t, homeBody(t, srv)); !strings.Contains(line, "display:none") {
		t.Errorf("the all-clear line is shown on a device holding nothing back: %s", line)
	}
}

// The operator asked for the map under the header. What must survive that
// move is the reason it used to be last: it shows what a third party
// believes, the banner shows what drives the siren, and the two can
// disagree. So the map must still be an ordinary card — never the banner's
// filled destructive styling — and the banner must still be the widest,
// loudest thing on the page.
func TestHomePutsTheMapUnderTheHeaderWithoutDressingItAsState(t *testing.T) {
	srv, _ := newServerWithStore(t)
	body := homeBody(t, srv)

	iframe := strings.Index(body, `src="https://alerts.in.ua/?embed"`)
	grid := strings.Index(body, `class="grid gap-4 sm:grid-cols-2"`)
	if iframe < 0 || grid < 0 {
		t.Fatalf("map at %d, card grid at %d: both must render", iframe, grid)
	}
	if iframe > grid {
		t.Error("the map is still inside or below the card grid; it belongs under the header")
	}

	start := strings.LastIndex(body[:iframe], "<section")
	end := iframe + strings.Index(body[iframe:], "</section>")
	card := body[start:end]
	if strings.Contains(card, "destructive") || strings.Contains(card, "border-4") {
		t.Errorf("the map card wears the state banner's styling; card markup:\n%s", card)
	}
	if !strings.Contains(card, "alerts.in.ua") {
		t.Error("the map card does not name its source, so it can be read as the device's own state")
	}
}

// alertBanner returns the markup of the alarmed half of the state block
// alone. Every assertion about the level has to be anchored inside it: the
// words also travel in the Alpine initial state below the markup, and a
// whole-page check would be satisfied by that alone while the banner itself
// said nothing.
func alertBanner(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `data-state="alert"`)
	if start < 0 {
		t.Fatal("the page carries no alarmed banner at all")
	}
	start = strings.LastIndex(body[:start], "<div")
	end := strings.Index(body[start:], "</div>")
	if start < 0 || end < 0 {
		t.Fatal("the alarmed banner is not a block of its own")
	}
	return body[start : start+end]
}

// The same raid can be declared yellow or red, and the two are different
// instructions to the people who hear the siren. ТРИВОГА on its own does not
// say which, and this page is what an operator checks before deciding what
// to do.
func TestHomeNamesTheLevelOnTheAlertBanner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		level   alertsource.Level
		want    string
		wantNot string
	}{
		{"red", alertsource.LevelRed, "ЧЕРВОНИЙ", "ЖОВТИЙ"},
		{"yellow", alertsource.LevelYellow, "ЖОВТИЙ", "ЧЕРВОНИЙ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			banner := alertBanner(t, homeBody(t, newServerInAlertAtLevel(t, tc.level)))
			if !strings.Contains(banner, tc.want) {
				t.Errorf("the banner of a raid declared %s does not say so; banner:\n%s", tc.name, banner)
			}
			if strings.Contains(banner, tc.wantNot) {
				t.Errorf("the banner of a raid declared %s names the other level; banner:\n%s", tc.name, banner)
			}
		})
	}
}

// An unknown level is not a mild one. Every Ubilling source reports unknown —
// those providers have no level concept at all — and this device announces
// such a raid as red, so a banner that showed it as the lesser warning would
// contradict the siren the operator just heard. It reads as red, with a line
// saying plainly that nobody named the level.
func TestHomeShowsAnUnnamedLevelAsRed(t *testing.T) {
	banner := alertBanner(t, homeBody(t, newServerInAlertAtLevel(t, alertsource.LevelUnknown)))

	if !strings.Contains(banner, "ЧЕРВОНИЙ") {
		t.Errorf("a raid upstream named no level for does not read as red; banner:\n%s", banner)
	}
	if strings.Contains(banner, "ЖОВТИЙ") {
		t.Errorf("a raid upstream named no level for reads as the lesser warning; banner:\n%s", banner)
	}
	if !strings.Contains(banner, "джерело не вказує рівень") {
		t.Errorf("the banner does not say that upstream named no level at all; banner:\n%s", banner)
	}
}

// A calm device claims nothing about severity. The banner is hidden anyway,
// but a hidden block carrying a level is one an Alpine binding can reveal
// with the wrong words already in it.
func TestHomeNamesNoLevelOnACalmDevice(t *testing.T) {
	banner := alertBanner(t, homeBody(t, newServer(t)))

	for _, word := range []string{"ЧЕРВОНИЙ", "ЖОВТИЙ"} {
		if strings.Contains(banner, word) {
			t.Errorf("a calm device's banner already names a level; banner:\n%s", banner)
		}
	}
}
