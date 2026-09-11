package web_test

import (
	"net/http"
	"net/http/httptest"
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

// The stream is what keeps this screen honest, so the screen has to be able
// to say when it has lost the stream. Without the badge — and without the
// onerror that raises it — a page whose EventSource died sat frozen on its
// last frame, and an alert that began afterwards showed as СПОКІЙНО.
//
// The Go side can only assert the markup and the wiring; the browser
// behaviour itself is verified by hand (see the fix report).
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
	if !strings.Contains(body, "location.reload()") {
		t.Error("a closed stream never recovers: nothing reloads the page")
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
