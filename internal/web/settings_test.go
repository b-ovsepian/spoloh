package web_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// importRequest builds the multipart POST the restore form sends. Written
// with mime/multipart rather than by hand, like /sounds' own upload helper:
// a hand-rolled body is too easy to get subtly wrong, where this cannot be.
func importRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("config", "spoloh-config.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/config/import", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", "http://"+req.Host)
	return req
}

// importFile marshals a configuration the way an export would, so a test can
// say what is in the file an operator picked.
func importFile(t *testing.T, c *config.Config) []byte {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

// postImport fires one authenticated restore and returns the recorder.
func postImport(t *testing.T, srv *web.Server, cookie *http.Cookie, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := importRequest(t, body)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// A backup that cannot be put back is not a backup. The settings in the file
// have to reach the store, and the page has to say that the device needs
// restarting — the cascade, the time zone and the audio output are read once
// at startup, so until it restarts the page says one thing and the device
// does another.
func TestConfigImportAppliesTheFileAndAsksForARestart(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	incoming := config.Default()
	incoming.Device.Name = "Сполох у майстерні"
	incoming.Alerts.Region = "Львівська область"
	incoming.Audio.MasterVolume = 73

	rec := postImport(t, srv, cookie, importFile(t, incoming))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	got := store.Get()
	if got.Device.Name != "Сполох у майстерні" || got.Alerts.Region != "Львівська область" || got.Audio.MasterVolume != 73 {
		t.Errorf("the restore did not reach the store: name=%q region=%q volume=%d",
			got.Device.Name, got.Alerts.Region, got.Audio.MasterVolume)
	}
	if !strings.Contains(pageContent(t, rec.Body.String()), "перезавантажте пристрій") {
		t.Errorf("the page never says the device has to be restarted; body:\n%s", rec.Body.String())
	}
}

// The one that matters. A restore that took Auth from the file would leave
// the operator holding a cookie their own device no longer honours, locked
// out with a password they have never seen — and the file is routinely one
// they exported from a different device.
func TestConfigImportLeavesTheOperatorSignedIn(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Auth

	incoming := config.Default()
	incoming.Auth.PasswordHash = "A-HASH-FROM-A-FILE"
	incoming.Auth.SessionSecret = "A-SECRET-FROM-A-FILE"
	incoming.Alerts.Region = "Львівська область"

	rec := postImport(t, srv, cookie, importFile(t, incoming))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if store.Get().Alerts.Region != "Львівська область" {
		t.Fatal("the restore did not apply, so this test would pass against anything")
	}

	after := store.Get().Auth
	if after.PasswordHash != before.PasswordHash || after.SessionSecret != before.SessionSecret {
		t.Error("the restore replaced the running credentials with the file's")
	}
	// The cookie is verified against the stored Auth on every request, so
	// this is the operator still being signed in, not a value comparison
	// standing in for one.
	if code := getWithSession(t, srv, cookie, "/settings").Code; code != http.StatusOK {
		t.Errorf("GET /settings with the session held before the restore = %d, want 200: the restore signed the operator out", code)
	}
	// And the password they know still works.
	req := postForm("/login", url.Values{"password": {"correct horse battery staple"}})
	loginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(loginRec, req)
	if loginRec.Code != http.StatusSeeOther {
		t.Errorf("logging in again after the restore = %d, want 303: the password the operator knows no longer opens the device", loginRec.Code)
	}
}

// A configuration the daemon could not start on must not reach the file the
// daemon starts from. The device keeps announcing on what it has.
func TestConfigImportRejectsAConfigurationTheDaemonCouldNotStartOn(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get()

	broken := config.Default()
	broken.Device.Timezone = "Mars/Olympus_Mons"
	broken.Alerts.Region = "Львівська область"

	rec := postImport(t, srv, cookie, importFile(t, broken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}

	after := store.Get()
	if after.Device.Timezone != before.Device.Timezone || after.Alerts.Region != before.Alerts.Region {
		t.Errorf("a rejected restore changed the stored configuration: timezone=%q region=%q",
			after.Device.Timezone, after.Alerts.Region)
	}
	// The rejection has to be readable, not a bare 400 the operator meets
	// with no idea what to do instead — and this file's problem is a value
	// in it, which is a different answer from picking another file.
	if !strings.Contains(pageContent(t, rec.Body.String()), "пристрій не запуститься") {
		t.Errorf("the rejection never says the device would not start on this file; body:\n%s", rec.Body.String())
	}
}

// Whatever an operator picks off a phone — a photo, a half-downloaded file,
// the wrong JSON entirely — is a rejection and a sentence, never a panic
// that takes the dashboard down with it.
func TestConfigImportRejectsAFileThatIsNotAConfiguration(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Alerts.Region

	for name, body := range map[string][]byte{
		"not JSON at all":         []byte("\xff\xd8\xff\xe0 this is a photograph"),
		"JSON of the wrong shape": []byte(`{"schema": "not a number"}`),
		"an empty file":           {},
	} {
		rec := postImport(t, srv, cookie, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body:\n%s", name, rec.Code, rec.Body.String())
		}
		if got := store.Get().Alerts.Region; got != before {
			t.Fatalf("%s: the stored configuration changed to %q", name, got)
		}
		// The other rejection sentence: this file is not a configuration at
		// all, so the answer is another file, not another setting.
		if !strings.Contains(pageContent(t, rec.Body.String()), "це не збережені налаштування") {
			t.Errorf("%s: the rejection never says the file is not a configuration; body:\n%s", name, rec.Body.String())
		}
	}
}

// The form has to be a plain multipart POST: this is the screen someone
// reaches over the device's own access point, from whatever phone they have.
func TestSettingsIndexOffersARestoreThatNeedsNoScript(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	index := settingsIndex(t, srv, cookie)
	if !strings.Contains(index, `<form method="post" action="/settings/config/import" enctype="multipart/form-data"`) {
		t.Errorf("the settings page offers no plain multipart restore form; index:\n%s", index)
	}
	if !strings.Contains(index, `name="config"`) {
		t.Errorf("the restore form has no file field named config; index:\n%s", index)
	}
}
