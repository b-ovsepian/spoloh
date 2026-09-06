package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if !strings.Contains(body, "/events") {
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
