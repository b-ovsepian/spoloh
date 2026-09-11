package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// headerSecret is the operator's own example credential: an Authorization
// header holding a username and password in base64. It is typed into this
// screen and must be rendered back by this screen, and by nothing else.
const headerSecret = "Basic c3UzZzRnbzZzazc6amkzOTQ1NHh1L14="

// webhookForm is the add/save form, filled with the operator's own relay
// request. Individual tests change one field to see it refused.
func webhookForm() url.Values {
	return url.Values{
		"name":    {"Реле"},
		"trigger": {config.TriggerAlertStart},
		"method":  {http.MethodGet},
		"url":     {"http://192.168.1.50/Sendkey?key=8"},
		"headers": {"Authorization: " + headerSecret},
		"enabled": {"on"},
	}
}

// submit posts form to path with a live session and returns the response.
func submit(t *testing.T, srv *web.Server, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := postForm(path, form)
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// webhookPage fetches a page with a live session and returns its body.
func webhookPage(t *testing.T, srv *web.Server, cookie *http.Cookie, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, rec.Code)
	}
	return rec.Body.String()
}

// newWebhookServer returns a logged-in server, its store and the session.
func newWebhookServer(t *testing.T) (*web.Server, *configstore.Store, *http.Cookie) {
	t.Helper()
	srv, store := newServerWithStore(t)
	setPassword(t, srv, webhookPassword)
	return srv, store, login(t, srv, webhookPassword)
}

// webhookPassword is this file's administrator password; the walk below
// re-logs in for every route, the way leak_test.go does, because POST
// /logout ends whatever session it is handed.
const webhookPassword = "correct horse battery staple"

// An operator has to be able to add the request their vendor documented,
// change it, and take it away again — and what is stored has to be exactly
// what they typed, headers included. A webhook that lost its Authorization
// header on the way to disk would fail at the one moment it matters.
func TestAWebhookCanBeAddedEditedAndRemoved(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)

	if rec := submit(t, srv, cookie, "/webhooks/add", webhookForm()); rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	hooks := store.Get().Webhooks
	if len(hooks) != 1 {
		t.Fatalf("%d webhooks stored, want 1", len(hooks))
	}
	stored := hooks[0]
	if stored.URL != "http://192.168.1.50/Sendkey?key=8" || stored.Method != http.MethodGet {
		t.Errorf("stored request = %s %s, want the one that was typed", stored.Method, stored.URL)
	}
	if len(stored.Headers) != 1 || stored.Headers[0].Name != "Authorization" || stored.Headers[0].Value != headerSecret {
		t.Fatalf("stored headers = %+v, want the operator's own Authorization header", stored.Headers)
	}
	if !stored.Enabled {
		t.Error("a webhook added with «Увімкнено» ticked was stored disabled")
	}

	edit := webhookForm()
	edit.Set("id", stored.ID)
	edit.Set("method", http.MethodPost)
	edit.Set("body", `{"relay":1}`)
	edit.Del("enabled") // unticking the box is how a webhook is stopped
	if rec := submit(t, srv, cookie, "/webhooks/save", edit); rec.Code != http.StatusOK {
		t.Fatalf("save: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	saved := store.Get().Webhooks[0]
	if saved.Method != http.MethodPost || saved.Body != `{"relay":1}` {
		t.Errorf("saved request = %s with body %q, want the edited one", saved.Method, saved.Body)
	}
	if saved.Enabled {
		t.Error("unticking «Увімкнено» left the webhook enabled")
	}
	if len(saved.Headers) != 1 || saved.Headers[0].Value != headerSecret {
		t.Errorf("saving lost the header value: %+v", saved.Headers)
	}

	if rec := submit(t, srv, cookie, "/webhooks/delete", url.Values{"id": {stored.ID}}); rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200", rec.Code)
	}
	if got := store.Get().Webhooks; len(got) != 0 {
		t.Fatalf("%d webhooks left after a delete, want 0", len(got))
	}
}

// The address is the one field that can ask this device to do something that
// is not an HTTP request at all. It is refused where it is typed, with a
// sentence saying what an address looks like, and nothing is stored.
func TestAWebhookAddressThatIsNotAnHttpRequestIsRefused(t *testing.T) {
	for _, address := range []string{
		"file:///etc/spoloh/config.json",
		"gopher://192.168.1.50:25/_MAIL",
		"192.168.1.50/Sendkey", // no scheme at all
		"http:///Sendkey",      // no host
		"",
	} {
		t.Run(address, func(t *testing.T) {
			srv, store, cookie := newWebhookServer(t)
			form := webhookForm()
			form.Set("url", address)

			rec := submit(t, srv, cookie, "/webhooks/add", form)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if got := store.Get().Webhooks; len(got) != 0 {
				t.Fatalf("%d webhooks stored, want none", len(got))
			}
			if !strings.Contains(rec.Body.String(), "http://") {
				t.Error("the page does not say what an address should look like")
			}
		})
	}
}

// A header line the operator wrote but this device could not parse must be
// refused, not dropped: a request that goes out without its Authorization
// header is the failure this whole screen exists to avoid, and it would
// happen with nothing on the page to explain it.
//
// The rejection hands back what was typed — nobody should have to retype a
// credential because they missed a colon — but the message itself must not
// quote it. A message is the thing that gets logged, screenshotted and read
// out to somebody else.
func TestAHeaderLineWithNoColonIsRefused(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)
	form := webhookForm()
	form.Set("headers", "Authorization "+headerSecret)

	rec := submit(t, srv, cookie, "/webhooks/add", form)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got := store.Get().Webhooks; len(got) != 0 {
		t.Fatalf("%d webhooks stored, want none", len(got))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Кожен заголовок") {
		t.Error("the page does not say how a header line should be written")
	}
	if !strings.Contains(body, headerSecret) {
		t.Error("the rejection lost what was typed; the operator has to retype a credential")
	}
	if strings.Contains(alertText(t, body), headerSecret) {
		t.Errorf("the rejection message quotes the header value:\n%s", alertText(t, body))
	}
}

// alertText is the rendered destructive alert — the sentence the screen
// shows and the one that ends up in a screenshot or read out loud.
func alertText(t *testing.T, body string) string {
	t.Helper()
	const marker = `data-variant="destructive"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("the page carries no rejection message at all")
	}
	rest := body[i:]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		t.Fatal("the rejection message is not closed; the page cannot be read")
	}
	return rest[:j]
}

// Header injection, refused at the form: a CR-LF in a value ends the header
// and starts one the operator never wrote, on a request this device makes
// with their credentials attached.
func TestAHeaderValueCarryingANewlineCannotBeStored(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)
	form := webhookForm()
	// url.Values encodes the CR-LF, so this arrives at the handler exactly
	// as a browser would deliver a pasted value.
	form.Set("headers", "X-Key: 8\r\nX-Admin: 1")

	submit(t, srv, cookie, "/webhooks/add", form)

	for _, w := range store.Get().Webhooks {
		for _, h := range w.Headers {
			if strings.ContainsAny(h.Value, "\r\n") {
				t.Fatalf("a header value carrying a newline was stored: %q", h.Value)
			}
		}
	}
}

// config.Validate deliberately accepts any stored webhook so a hand-edited
// one can never stop the daemon starting. This screen is where the operator
// is told, on the page that can fix it.
func TestAStoredWebhookThisBuildCannotMakeIsMarkedOnThePage(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)
	if err := store.Update(func(c *config.Config) error {
		c.Webhooks = []config.Webhook{{
			ID: "webhook-broken", Name: "Реле", Trigger: config.TriggerAlertStart,
			Method: "GET", URL: "gopher://192.168.1.50/_x", Enabled: true,
		}}
		return nil
	}); err != nil {
		t.Fatalf("the store refused a webhook it must accept: %v", err)
	}

	body := webhookPage(t, srv, cookie, "/webhooks")
	if !strings.Contains(body, "не спрацює") {
		t.Errorf("the page does not say the webhook cannot fire:\n%s", body)
	}
}

// The screen has to render its own fields back, or an operator cannot edit
// what they already stored.
func TestTheWebhooksPageRendersWhatWasStored(t *testing.T) {
	srv, _, cookie := newWebhookServer(t)
	submit(t, srv, cookie, "/webhooks/add", webhookForm())

	body := webhookPage(t, srv, cookie, "/webhooks")
	for _, want := range []string{"192.168.1.50/Sendkey?key=8", "Authorization: " + headerSecret} {
		if !strings.Contains(body, want) {
			t.Errorf("the edit form does not carry %q", want)
		}
	}
}

// The rule leak_test.go keeps for the administrator's password hash, kept
// here for a header value, which is the same kind of thing: the operator's
// example is a password in base64.
//
// It may be rendered by the screen that edits it — including by the POST
// handlers, which re-render that same screen — and by nothing else. This
// walks every route the server registers, with a live session, GET and an
// empty POST, and fails if the value appears anywhere outside /webhooks.
//
// leak_test.go itself is left alone: it scans for the two Auth secrets and
// its walk is written around them. This is the same walk with a third
// secret and one deliberate exemption, which that file has no way to
// express.
func TestNoPageOutsideTheWebhookScreenRendersAHeaderValue(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)
	if rec := submit(t, srv, cookie, "/webhooks/add", webhookForm()); rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200", rec.Code)
	}
	if got := store.Get().Webhooks[0].Headers[0].Value; got != headerSecret {
		t.Fatalf("the header value was not stored, so this test would pass against anything: %q", got)
	}

	// The exemption, stated rather than implied: these four are the webhook
	// screen and the three submissions that re-render it.
	exempt := map[string]bool{
		"/webhooks":        true,
		"/webhooks/add":    true,
		"/webhooks/save":   true,
		"/webhooks/delete": true,
	}

	// Proof the walk below can see a header value at all. Without this, an
	// exemption covering every route would pass silently.
	if body := webhookPage(t, srv, cookie, "/webhooks"); !strings.Contains(body, headerSecret) {
		t.Fatal("the webhook screen does not render the header value; this walk proves nothing")
	}

	check := func(t *testing.T, method, path string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, body).WithContext(ctx)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "http://"+req.Host)
		}
		req.AddCookie(login(t, srv, webhookPassword))

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), headerSecret) {
			t.Errorf("%s %s renders a webhook header value", method, path)
		}
	}

	for _, pattern := range srv.Patterns() {
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("pattern %q carries no method", pattern)
		}
		path = strings.TrimSuffix(path, "{$}")
		if path == "" {
			path = "/"
		}
		if exempt[path] {
			continue
		}
		check(t, http.MethodGet, path)
		check(t, http.MethodPost, path)
	}
}
