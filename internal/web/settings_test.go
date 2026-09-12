package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/web"
)

// Six of the nine top-level sections were configuration, and the nav wrapped
// to two rows on a phone because of it. They now live behind one entry — so
// that entry has to reach every one of them. A screen nothing links to is a
// screen nobody finds.
func TestSettingsIndexLinksEveryConfigurationScreen(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	// The index itself, not the whole page: the shell's nav sits above it and
	// carries Мережа on every screen, so a check against the full body could
	// never tell an index that lists Мережа from one that does not.
	index := pageContent(t, rec.Body.String())

	for _, href := range []string{"/alerts", "/events", "/sounds", "/audio", "/webhooks", "/time"} {
		if !strings.Contains(index, `href="`+href+`"`) {
			t.Errorf("the settings index does not link %s; a screen nothing links to is a screen nobody finds", href)
		}
	}
	// Мережа is deliberately not here: it stays in the top-level nav,
	// because it is the screen someone reaches when nothing else works.
	if strings.Contains(index, `href="/network"`) {
		t.Error("the settings index links /network; it belongs in the top nav, one tap away")
	}
}

// pageContent returns what the page itself rendered, with the shell's header
// and footer cut away. Every authenticated page carries the nav, so "is this
// link on the page" is only ever a question about the content.
func pageContent(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<main")
	end := strings.Index(body, "</main>")
	if start < 0 || end < start {
		t.Fatalf("the page has no <main> to read; body:\n%s", body)
	}
	return body[start:end]
}

// settingsIndex renders /settings and returns the index itself. The screens
// that moved behind it pin their own labels here now, from their own tests.
func settingsIndex(t *testing.T, srv *web.Server, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	return pageContent(t, rec.Body.String())
}

// settingsRow returns one row of the index: the whole <a>, from its href to
// the closing tag. A label checked against the whole index would pass on a
// label that had drifted onto somebody else's row.
func settingsRow(t *testing.T, index, href string) string {
	t.Helper()
	start := strings.Index(index, `href="`+href+`"`)
	if start < 0 {
		t.Fatalf("the settings index has no row for %s; index:\n%s", href, index)
	}
	end := strings.Index(index[start:], "</a>")
	if end < 0 {
		t.Fatalf("the settings index row for %s never closes; index:\n%s", href, index)
	}
	return index[start : start+end]
}
