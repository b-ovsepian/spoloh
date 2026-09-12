package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// TestNoPageRendersASecret walks every route srv.Patterns() registers
// and requests each one twice with a live session — once with GET, once with
// POST and an empty form — failing if either response body contains the
// password hash or the session secret. The POST leg matters as much as the
// GET one: every screen this plan adds re-renders its own view on a rejected
// submission (an unknown sound, a volume out of range, a device ALSA never
// reported, ...), and that render is reachable only through POST. A GET-only
// walk would never see it.
//
// An empty form is deliberately the payload, not an incidental choice: every
// POST route in this codebase rejects one today — no SSID, no password, no
// sound — without writing anything, so the body scanned back is exactly the
// rejection render and nothing has mutated. That was checked by hand for
// each POST route as of this test (see task-1-report.md); a route that
// starts accepting an empty form and changing state would silently drop out
// of that guarantee; the fresh session below is what keeps the walk correct
// regardless of what it changed.
//
// A fresh cookie is fetched for every route rather than one reused for the
// whole walk: POST /logout ends whatever session it is given, and a stale,
// revoked cookie would make every route requested after it look
// unauthenticated — redirected to /login, not actually rendered — rather
// than genuinely scanned.
//
// One route is held to a different rule than the rest, and it is the reason
// the secret list is split in two below. GET /settings/config/export?secrets=1
// is the backup an operator takes for the same device, so it carries the keys
// they typed on purpose — the alerts.in.ua token among them. It is walked with
// the query string rather than left at its default, because the safe download
// is the one the bare path returns and covering only that would be covering
// the route in name alone. What that leg enforces is the absolute half: Auth
// is never in the file, at any setting, because nobody typed it and import
// never reads it back.
//
// What this still does not reach: path resolution below only understands
// "{$}", the one exact-match marker in use today, so a future route
// registered with a wildcard segment (e.g. "{id}") would be requested
// literally, with the braces in the path, and 404 rather than render
// anything. And this only ever sends GET and an empty POST — a rejection
// or success render reachable solely through a specific field value (rather
// than an empty one) is not exercised here.
func TestNoPageRendersASecret(t *testing.T) {
	const password = "correct horse battery staple"
	// A token distinctive enough that finding it in a body means it was
	// rendered, not that two ordinary words happened to line up.
	const alertsUAToken = "alerts-ua-token-0123456789abcdef"

	srv, store := newServerWithStore(t)
	setPassword(t, srv, password)
	// The alerts.in.ua token is a credential the operator hands this device,
	// and /alerts is a form that writes it — so it is exactly the kind of
	// value a re-render echoes back into an <input> without anyone meaning
	// to. It is stored here so the walk below has something to find: the
	// admin hash has never leaked because this test existed before it did,
	// and this token must not become the first exception.
	if err := store.Update(func(c *config.Config) error {
		c.Alerts.AlertsUAToken = alertsUAToken
		return nil
	}); err != nil {
		t.Fatalf("store the alerts.in.ua token: %v", err)
	}

	cfg := store.Get()
	// Auth is split out from the rest because one route may deliberately
	// carry the operator's own credentials and none may ever carry these.
	// See the export leg at the bottom of the walk.
	authSecrets := map[string]string{
		"password hash":  cfg.Auth.PasswordHash,
		"session secret": cfg.Auth.SessionSecret,
	}
	secrets := map[string]string{
		"alerts.in.ua token": cfg.Alerts.AlertsUAToken,
	}
	for name, v := range authSecrets {
		secrets[name] = v
	}
	for name, v := range secrets {
		if v == "" {
			t.Fatalf("%s is empty, so this test would pass against anything", name)
		}
	}

	// checkNoSecrets fires one request at the given method and path, bounded
	// so /events (which streams over SSE until its context ends) returns
	// instead of hanging the walk, and fails the test if the body contains
	// either secret.
	checkNoSecrets := func(t *testing.T, cookie *http.Cookie, method, path string, forbidden map[string]string) {
		t.Helper()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		var reqBody io.Reader
		if method == http.MethodPost {
			reqBody = strings.NewReader("") // an empty form, url-encoded
		}
		req := httptest.NewRequest(method, path, reqBody).WithContext(ctx)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			// sameOrigin rejects with 403 before any handler runs unless a
			// browser-style same-origin signal is present.
			req.Header.Set("Origin", "http://"+req.Host)
		}
		req.AddCookie(cookie)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		got := rec.Body.String()
		for name, v := range forbidden {
			if strings.Contains(got, v) {
				t.Errorf("%s %s renders the %s", method, path, name)
			}
		}
	}

	// The download that deliberately carries the operator's credentials. It
	// is named rather than spotted by shape, and the flag below fails the
	// test if the route is ever renamed out from under this walk.
	const exportPath = "/settings/config/export"
	exportWalked := false

	for _, pattern := range srv.Patterns() {
		// Patterns are "METHOD /path" (see routes_test.go); strip the method
		// to get an actual request target.
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("pattern %q carries no method; expected %q form", pattern, "METHOD /path")
		}
		path = strings.TrimSuffix(path, "{$}")
		if path == "" {
			path = "/"
		}

		cookie := login(t, srv, password)
		checkNoSecrets(t, cookie, http.MethodGet, path, secrets)
		checkNoSecrets(t, cookie, http.MethodPost, path, secrets)

		// The one response on this device that is meant to carry a
		// credential. Without the query string the walk requests the safe
		// download and learns nothing about the other one, so the route
		// would be covered in name only. What it is held to here is
		// narrower and absolute: the operator's own keys are the point of
		// the file, Auth is never in it at any setting.
		if path == exportPath {
			exportWalked = true
			checkNoSecrets(t, cookie, http.MethodGet, path+"?secrets=1", authSecrets)
		}
	}
	if !exportWalked {
		t.Errorf("no route registers %s any more; the leg of this walk that checks the download with credentials is checking nothing", exportPath)
	}
}
