package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
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

// The two downloads are two links, not one link and a remembered choice.
// Whichever way the operator gets a file with credentials in it, they have
// to have clicked the one that says so.
func TestSettingsIndexOffersBothDownloadsSeparately(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	index := settingsIndex(t, srv, cookie)

	for _, href := range []string{"/settings/config/export", "/settings/config/export?secrets=1"} {
		if !strings.Contains(index, `href="`+href+`"`) {
			t.Errorf("the settings index does not offer %s", href)
		}
	}
}

// The default download is the one an operator sends to somebody. Every
// credential this device holds has to be gone from it — the alerts.in.ua
// token, the webhook header value that is routinely an Authorization
// credential, and the access point password the network page shows on
// purpose but a file leaving the device must not.
func TestConfigExportWithoutSecretsCarriesNoCredential(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.Alerts.AlertsUAToken = "alerts-ua-token-0123456789abcdef"
		c.Network.APPassword = "sp0lohTest42"
		c.Webhooks = []config.Webhook{{
			ID: "w1", Trigger: config.TriggerAlertStart, Method: "GET",
			URL: "http://192.168.0.105/setvol?mute=1", Enabled: true,
			Headers: []config.WebhookHeader{{Name: "Authorization", Value: "Basic bXVzaWM6cGxheWVy"}},
		}}
		return nil
	}); err != nil {
		t.Fatalf("store the credentials this download must drop: %v", err)
	}

	rec := getWithSession(t, srv, cookie, "/settings/config/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	cfg := store.Get()
	body := rec.Body.String()
	for name, secret := range map[string]string{
		"alerts.in.ua token":    cfg.Alerts.AlertsUAToken,
		"webhook header value":  "Basic bXVzaWM6cGxheWVy",
		"access point password": cfg.Network.APPassword,
		"password hash":         cfg.Auth.PasswordHash,
		"session secret":        cfg.Auth.SessionSecret,
	} {
		if secret == "" {
			t.Fatalf("the %s is empty, so this test would pass against anything", name)
		}
		if strings.Contains(body, secret) {
			t.Errorf("the download without credentials carries the %s", name)
		}
	}
}

// The other download is a backup for the same device, so the keys the
// operator typed travel with it. Auth does not, at either setting: nobody
// typed it, import never reads it back, and a file carrying it would let
// whoever held it forge a live session.
func TestConfigExportWithSecretsCarriesTheOperatorsKeysButNeverAuth(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.Alerts.AlertsUAToken = "alerts-ua-token-0123456789abcdef"
		c.Network.APPassword = "sp0lohTest42"
		return nil
	}); err != nil {
		t.Fatalf("store the keys this download must carry: %v", err)
	}

	rec := getWithSession(t, srv, cookie, "/settings/config/export?secrets=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	cfg := store.Get()
	body := rec.Body.String()
	for name, key := range map[string]string{
		"alerts.in.ua token":    cfg.Alerts.AlertsUAToken,
		"access point password": cfg.Network.APPassword,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("the backup dropped the %s; it is one of the keys this download promises", name)
		}
	}
	for name, never := range map[string]string{
		"password hash":  cfg.Auth.PasswordHash,
		"session secret": cfg.Auth.SessionSecret,
	} {
		if never == "" {
			t.Fatalf("the %s is empty, so this test would pass against anything", name)
		}
		if strings.Contains(body, never) {
			t.Errorf("the backup carries the %s; Auth is this installation's, not the operator's", name)
		}
	}
}

// The file has to arrive as a file, and the two downloads must not arrive
// under the same name: an operator who has both in a downloads folder can
// otherwise not tell which one is safe to send on.
func TestConfigExportArrivesAsANamedDownload(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for path, wantName := range map[string]string{
		"/settings/config/export":           "spoloh-config.json",
		"/settings/config/export?secrets=1": "spoloh-config-with-secrets.json",
	} {
		rec := getWithSession(t, srv, cookie, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="`+wantName+`"`) {
			t.Errorf("GET %s Content-Disposition = %q, want an attachment named %s", path, got, wantName)
		}
		var back config.Config
		if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
			t.Errorf("GET %s did not send JSON: %v\n%s", path, err, rec.Body.String())
		}
	}
}

// Anything other than the exact opt-in is the safe download. A stale link, a
// copied URL with "secrets=true" in it, a query somebody typed from memory —
// none of them may be the one that writes a token to disk.
func TestConfigExportOnlyTheExactOptInCarriesKeys(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.Alerts.AlertsUAToken = "alerts-ua-token-0123456789abcdef"
		return nil
	}); err != nil {
		t.Fatalf("store the token: %v", err)
	}
	token := store.Get().Alerts.AlertsUAToken

	for _, path := range []string{
		"/settings/config/export?secrets=true",
		"/settings/config/export?secrets=0",
		"/settings/config/export?secrets=",
		"/settings/config/export?SECRETS=1",
	} {
		rec := getWithSession(t, srv, cookie, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), token) {
			t.Errorf("GET %s carries the alerts.in.ua token; only the exact opt-in may", path)
		}
	}
}

// getWithSession fires one authenticated GET and returns the recorder.
func getWithSession(t *testing.T, srv *web.Server, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
