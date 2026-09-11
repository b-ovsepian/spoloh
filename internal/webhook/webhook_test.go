package webhook_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/webhook"
)

// secret is the operator's own example credential, base64 of a username and
// password. Several tests below assert it never appears anywhere but in the
// request itself.
const secret = "Basic c3UzZzRnbzZzazc6amkzOTQ1NHh1L14="

// recorder is a test double for the relay board: an httptest server on
// loopback, which is not the internet and is what the constraints call for.
// It remembers every request that reached it.
type recorder struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
}

// newRecorder starts a server answering handle, or 200 when handle is nil.
func newRecorder(t *testing.T, handle http.HandlerFunc) *recorder {
	t.Helper()
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, req.Clone(context.Background()))
		r.bodies = append(r.bodies, body.String())
		r.mu.Unlock()
		if handle != nil {
			handle(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) last(t *testing.T) *http.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("nothing reached the endpoint")
	}
	return r.requests[len(r.requests)-1]
}

// harness is a dispatcher with somewhere to write and somewhere to log, so a
// test can assert on both. The log buffer is what proves no header value
// reaches the journal.
type harness struct {
	d    *webhook.Dispatcher
	log  *eventlog.Log
	logs *bytes.Buffer
}

func newHarness(t *testing.T, hooks ...config.Webhook) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Webhooks = hooks

	lg, err := eventlog.Open(filepath.Join(t.TempDir(), "spoloh.db"), 100)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { lg.Close() })

	logs := new(bytes.Buffer)
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := webhook.New(func() *config.Config { return cfg }, lg, logger)
	return &harness{d: d, log: lg, logs: logs}
}

// entries returns the event log, newest first.
func (h *harness) entries(t *testing.T) []eventlog.Entry {
	t.Helper()
	got, err := h.log.Recent(50)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	return got
}

// onlyEntry returns the single webhook row, failing when there is not
// exactly one: "fired twice" and "never fired" are both bugs this catches.
func (h *harness) onlyEntry(t *testing.T) eventlog.Entry {
	t.Helper()
	var rows []eventlog.Entry
	for _, e := range h.entries(t) {
		if e.Kind == webhook.EventKind {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("the event log holds %d webhook rows, want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// relay is the operator's own webhook, pointed at url.
func relay(url string) config.Webhook {
	return config.Webhook{
		ID:      "webhook-relay",
		Name:    "Реле",
		Trigger: config.TriggerAlertStart,
		Method:  http.MethodGet,
		URL:     url,
		Headers: []config.WebhookHeader{{Name: "Authorization", Value: secret}},
		Enabled: true,
	}
}

// A webhook exists to act on one thing happening. Firing on a trigger it was
// not configured for would close somebody's relay at the wrong moment — at
// the all-clear, say, with the raid over and the siren wired to the contact.
func TestAWebhookFiresOnItsTriggerAndNotOnAnother(t *testing.T) {
	rec := newRecorder(t, nil)
	h := newHarness(t, relay(rec.srv.URL+"/Sendkey?key=8"))

	h.d.Dispatch(config.TriggerAlertEnd)
	h.d.Dispatch(config.TriggerScheduled)
	h.d.Wait()
	if n := rec.count(); n != 0 {
		t.Fatalf("%d requests were made for triggers this webhook does not carry", n)
	}

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()
	if n := rec.count(); n != 1 {
		t.Fatalf("the webhook's own trigger produced %d requests, want 1", n)
	}
	if got := h.onlyEntry(t); !got.OK {
		t.Errorf("a delivered webhook was recorded as a failure: %+v", got)
	}
}

// Disabling is how an operator stops a webhook without losing the address,
// the headers and the body they typed. A disabled one that still fired would
// make that switch a lie — and the thing it drives is their own hardware.
func TestADisabledWebhookNeverFires(t *testing.T) {
	rec := newRecorder(t, nil)
	hook := relay(rec.srv.URL + "/Sendkey?key=8")
	hook.Enabled = false
	h := newHarness(t, hook)

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	if n := rec.count(); n != 0 {
		t.Fatalf("a disabled webhook made %d requests", n)
	}
	for _, e := range h.entries(t) {
		if e.Kind == webhook.EventKind {
			t.Errorf("a disabled webhook left a row behind: %+v", e)
		}
	}
}

// The operator's example is a request that does nothing without its header:
// the relay board answers 401 and the siren contact never closes. Method,
// path, query, headers and body all have to arrive exactly as typed.
func TestTheRequestIsTheOneTheOperatorWrote(t *testing.T) {
	rec := newRecorder(t, nil)
	hook := relay(rec.srv.URL + "/Sendkey?key=8")
	hook.Method = http.MethodPost
	hook.Body = `{"relay":1}`
	hook.Headers = append(hook.Headers, config.WebhookHeader{Name: "X-Relay", Value: "8"})
	h := newHarness(t, hook)

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	req := rec.last(t)
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL.Path != "/Sendkey" || req.URL.RawQuery != "key=8" {
		t.Errorf("target = %q?%q, want /Sendkey?key=8", req.URL.Path, req.URL.RawQuery)
	}
	if got := req.Header.Get("Authorization"); got != secret {
		t.Errorf("Authorization = %q, want the operator's own credential", got)
	}
	if got := req.Header.Get("X-Relay"); got != "8" {
		t.Errorf("X-Relay = %q, want %q", got, "8")
	}
	rec.mu.Lock()
	body := rec.bodies[len(rec.bodies)-1]
	rec.mu.Unlock()
	if body != `{"relay":1}` {
		t.Errorf("body = %q, want the one that was configured", body)
	}
}

// A redirect turns the request the operator read off their vendor's
// documentation into one they never saw — another host, with their
// Authorization header attached. It is not followed, and the attempt is
// recorded as undelivered rather than quietly counted as a success.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := newRecorder(t, nil)
	redirector := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/Sendkey?key=8", http.StatusFound)
	})
	h := newHarness(t, relay(redirector.srv.URL+"/Sendkey?key=8"))

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	if n := elsewhere.count(); n != 0 {
		t.Fatalf("the redirect was followed: %d requests reached the second host", n)
	}
	if n := redirector.count(); n != 1 {
		t.Fatalf("the first host saw %d requests, want 1", n)
	}
	got := h.onlyEntry(t)
	if got.OK {
		t.Errorf("a redirect that was not followed was recorded as a delivered webhook: %+v", got)
	}
}

// A webhook that silently stops working is worse than none: the operator
// believes their relay fires and it does not. A refused request has to leave
// a row saying so, and that row must not read as a success.
func TestARefusedRequestIsRecordedAsAFailure(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	h := newHarness(t, relay(rec.srv.URL+"/Sendkey?key=8"))

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	got := h.onlyEntry(t)
	if got.OK {
		t.Fatalf("a 401 was recorded as a delivered webhook: %+v", got)
	}
	if !strings.Contains(got.Err, "401") {
		t.Errorf("the row does not say what happened: %q", got.Err)
	}
}

// A box that is unplugged must cost nothing but a row. The dispatcher
// returns no error by design — there is no caller left to hand one to —
// so the evidence that it failed is the log, and it has to be there.
func TestAnUnreachableTargetIsRecordedAndNotFatal(t *testing.T) {
	rec := newRecorder(t, nil)
	url := rec.srv.URL + "/Sendkey?key=8"
	rec.srv.Close() // nothing is listening on that port any more

	h := newHarness(t, relay(url))
	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	got := h.onlyEntry(t)
	if got.OK {
		t.Fatalf("a request to a closed port was recorded as delivered: %+v", got)
	}
	if got.EventID != "webhook-relay" {
		t.Errorf("the row names %q, so the operator cannot tell which webhook failed", got.EventID)
	}
}

// The announcement must never wait for a webhook. Dispatch has to return
// while the target is still holding the connection open — a box that answers
// slowly, or not at all, is the ordinary case on a network with a device
// someone unplugged.
func TestDispatchReturnsWhileTheRequestIsStillOutstanding(t *testing.T) {
	release := make(chan struct{})
	answered := make(chan struct{})
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		close(answered)
	})
	h := newHarness(t, relay(rec.srv.URL+"/Sendkey?key=8"))

	h.d.Dispatch(config.TriggerAlertStart)
	// Dispatch has returned. The endpoint has not answered and cannot have:
	// nothing has closed release yet. If Dispatch had waited on the request,
	// this line would never be reached.
	select {
	case <-answered:
		t.Fatal("the endpoint answered before Dispatch returned, so Dispatch waited for it")
	default:
	}

	close(release)
	h.d.Wait()
	if n := rec.count(); n != 1 {
		t.Fatalf("%d requests reached the endpoint, want 1", n)
	}
}

// Bounded, not merely asynchronous. A target that accepts the connection and
// then says nothing would otherwise hold a dispatch slot for as long as it
// liked, and four of those are every slot this device has.
func TestAnAttemptIsBoundedByTheTimeout(t *testing.T) {
	release := make(chan struct{})
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	// After newRecorder's own cleanup, so it runs before it: t.Cleanup is
	// LIFO, and httptest.Server.Close waits for the handler above to return.
	t.Cleanup(func() { close(release) })

	h := newHarness(t, relay(rec.srv.URL+"/Sendkey?key=8"))
	h.d.Timeout = 50 * time.Millisecond

	done := make(chan struct{})
	go func() {
		h.d.Dispatch(config.TriggerAlertStart)
		h.d.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a webhook to a target that never answers was not bounded")
	}

	got := h.onlyEntry(t)
	if got.OK {
		t.Fatalf("an abandoned request was recorded as delivered: %+v", got)
	}
	if !strings.Contains(got.Err, "час") {
		t.Errorf("the row does not say the attempt ran out of time: %q", got.Err)
	}
}

// A header value is a credential — the operator's own example is a password
// in base64. It travels in the request and nowhere else: not into the
// journal, not into the event log, not into an error. This is the same rule
// internal/netmgr keeps for a Wi-Fi PSK and internal/web keeps for the admin
// password hash.
//
// Every outcome is exercised in one test, because the leak this guards
// against is a line somebody adds on the failure path, where the temptation
// to print "the whole request" is strongest.
func TestNoHeaderValueReachesALogOrTheEventLog(t *testing.T) {
	refuser := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	redirector := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.168.1.99/elsewhere", http.StatusFound)
	})
	ok := newRecorder(t, nil)

	dead := newRecorder(t, nil)
	deadURL := dead.srv.URL + "/Sendkey"
	dead.srv.Close()

	hooks := []config.Webhook{
		relay(ok.srv.URL + "/Sendkey?key=8"),
		relay(refuser.srv.URL + "/Sendkey?key=8"),
		relay(redirector.srv.URL + "/Sendkey?key=8"),
		relay(deadURL),
		// One this build will not even make, so the refusal path is walked
		// too: it is the newest place a "here is the whole webhook" log line
		// could be written.
		func() config.Webhook {
			w := relay("gopher://192.168.1.50/_x")
			w.ID = "webhook-broken"
			return w
		}(),
	}
	for i := range hooks {
		hooks[i].ID = hooks[i].ID + "-" + string(rune('a'+i))
	}
	h := newHarness(t, hooks...)

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	if strings.Contains(h.logs.String(), secret) {
		t.Errorf("a log line carries the header value:\n%s", h.logs.String())
	}
	// The credential's own alphabet soup is the obvious thing to search for;
	// the header name is not a secret and may well be logged. Check the
	// value's distinctive tail too, in case a line quoted a truncated one.
	if tail := secret[len(secret)-12:]; strings.Contains(h.logs.String(), tail) {
		t.Errorf("a log line carries part of the header value:\n%s", h.logs.String())
	}
	for _, e := range h.entries(t) {
		if strings.Contains(e.Err, secret) || strings.Contains(e.EventID, secret) || strings.Contains(e.Sound, secret) {
			t.Errorf("an event log row carries the header value: %+v", e)
		}
	}
}

// A URL can carry secrets too: the operator's own example ends in "?key=8",
// and userinfo in an address is a password outright. The journal gets the
// host, which is what somebody debugging needs, and not the rest.
func TestNoLogLineCarriesTheUrlsPathOrQuery(t *testing.T) {
	rec := newRecorder(t, nil)
	h := newHarness(t, relay(rec.srv.URL+"/Sendkey?key=8"))

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	logs := h.logs.String()
	if strings.Contains(logs, "key=8") {
		t.Errorf("a log line carries the URL's query:\n%s", logs)
	}
	if strings.Contains(logs, "/Sendkey") {
		t.Errorf("a log line carries the URL's path:\n%s", logs)
	}
	if !strings.Contains(logs, "127.0.0.1") {
		t.Errorf("no log line names the host, which is what makes a failure diagnosable:\n%s", logs)
	}
}

// config.Validate deliberately accepts any stored webhook, so that a
// hand-edited one can never stop the daemon starting. That leaves exactly
// one obligation, and this is it: the webhook that will not be fired says so
// where the operator looks, instead of being dropped in silence.
func TestAStoredWebhookThisBuildCannotMakeIsRefusedLoudly(t *testing.T) {
	hook := relay("gopher://192.168.1.50/_x")
	h := newHarness(t, hook)

	h.d.Dispatch(config.TriggerAlertStart)
	h.d.Wait()

	got := h.onlyEntry(t)
	if got.OK {
		t.Fatalf("a webhook this build cannot make was recorded as delivered: %+v", got)
	}
	if got.EventID != hook.ID {
		t.Errorf("the row names %q, not the webhook: %+v", got.EventID, got)
	}
	if !strings.Contains(h.logs.String(), hook.ID) {
		t.Errorf("nothing in the journal names the webhook that will not fire:\n%s", h.logs.String())
	}
}

// A nil event log is what a device with a corrupt SQLite file or a
// read-only SD card runs with. It costs the operator their history and must
// never cost them the request.
func TestANilEventLogDoesNotStopAWebhook(t *testing.T) {
	rec := newRecorder(t, nil)
	cfg := config.Default()
	cfg.Webhooks = []config.Webhook{relay(rec.srv.URL + "/Sendkey?key=8")}

	d := webhook.New(func() *config.Config { return cfg }, nil, slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)))
	d.Dispatch(config.TriggerAlertStart)
	d.Wait()

	if n := rec.count(); n != 1 {
		t.Fatalf("%d requests were made with no event log to write to, want 1", n)
	}
}
