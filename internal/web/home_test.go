package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
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
