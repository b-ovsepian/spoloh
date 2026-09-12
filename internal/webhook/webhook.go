// Package webhook makes the HTTP request an operator configured when this
// device announces something, so their own hardware — a relay board, a PA
// controller, a lamp in a corridor — acts at the same moment the speaker
// does.
//
// Everything here is written around one rule: a webhook must never delay or
// break an announcement. Dispatch returns before a single byte is sent, every
// request is bounded, how many can be in flight at once is capped, and
// nothing a webhook can do — a box that is unplugged, a box that answers with
// a redirect, a stored webhook this build will not even make — produces an
// error that anything on the announcement path has to handle. The engine
// calls Dispatch and carries on to the player; that is the whole contract.
package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
)

// EventKind is the eventlog kind every webhook outcome is written under.
//
// It is a local constant rather than an eventlog.Kind* because this feature
// landed while internal/eventlog was being changed by other work; the string
// is the one that package would hold as KindWebhook, and moving it there —
// together with a case in internal/web's kindLabel, so /log reads «Вебхук»
// rather than this identifier — is a two-line follow-up.
const EventKind = "webhook"

// DefaultTimeout bounds one webhook attempt end to end: the wait for a free
// slot, the connection, the request and the response.
//
// Five seconds because the target is a box on the same LAN — an appliance
// answering a GET on a switched local network answers in milliseconds or
// not at all. The bound is not really about that box, though. It is about
// how long a goroutine holding one of the few dispatch slots can keep the
// next webhook waiting, which is why the wait for a slot is inside the same
// deadline rather than in front of it.
const DefaultTimeout = 5 * time.Second

// maxInFlight caps how many requests may be outstanding at once.
//
// This runs on a single-board computer with a few hundred megabytes of RAM,
// and the case that matters is not the happy one: it is a target that has
// gone away, where every request holds its slot for the full timeout. Four
// is comfortably more than the handful of webhooks one trigger can match and
// small enough that a dead target costs four sockets rather than twenty.
const maxInFlight = 4

// maxDrainedBody is how much of a response is read before the rest is thrown
// away. Reading a little of it lets the connection be reused instead of torn
// down, and reading none of a large one leaves the transport holding it.
const maxDrainedBody = 4 << 10

// maxLoggedBody is how much of that is kept for the event log.
//
// A status code is not evidence that the announcement will be heard. A
// controller can accept the request, answer 200, and still not be in the
// state the operator wanted — and what it reports back is the only place that
// shows. Two hundred bytes is enough for the one-line answer these boxes give
// and small enough that a target which answers with a web page cannot fill
// the log with it.
const maxLoggedBody = 200

// minRedactedHeaderValue is the shortest header value worth scrubbing out of
// a logged response. A one- or two-character value would match half the
// alphabet of an ordinary answer and redact text that is not a credential;
// anything actually secret is longer than this.
const minRedactedHeaderValue = 8

// Dispatcher fires the webhooks stored in the configuration. The zero value
// is not usable; call New.
type Dispatcher struct {
	cfg    func() *config.Config
	client *http.Client
	log    *eventlog.Log
	logger *slog.Logger
	now    func() time.Time

	// Timeout bounds one attempt. Zero means DefaultTimeout. Set it before
	// the first Dispatch; tests use a short one.
	Timeout time.Duration

	// sem caps concurrency; wg is what Wait drains.
	sem chan struct{}
	wg  sync.WaitGroup
}

// New builds a dispatcher reading its webhooks from cfg on every trigger, so
// a webhook edited on the dashboard takes effect at the next announcement
// rather than at the next restart.
//
// lg may be nil — a nil *eventlog.Log swallows every append, exactly as it
// does for the engine — and so may logger, which then falls back to the
// default. The event log is where a failure becomes visible to the operator,
// so losing it costs the history and never the request.
func New(cfg func() *config.Config, lg *eventlog.Log, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		cfg:    cfg,
		log:    lg,
		logger: logger,
		now:    time.Now,
		sem:    make(chan struct{}, maxInFlight),
		client: &http.Client{
			// Redirects are refused, not followed. A 302 turns the request
			// the operator read off their vendor's documentation into one
			// they never saw — a different host, a different path, with
			// their Authorization header attached. ErrUseLastResponse hands
			// the 3xx back as an ordinary response, which reportResult then
			// records as a failure: this device did not deliver the request
			// it was told to make, and saying so is the only honest answer.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// No Client.Timeout: each request carries its own context
			// deadline, which also covers the wait for a dispatch slot.
		},
	}
}

// Dispatch fires every enabled webhook in this phase whose trigger matches,
// and returns at once — before any connection is made.
//
// Returning immediately is the point of this function's signature having no
// error in it. It is called from the engine's announcing path, and a device
// that waited on an HTTP request to a box that may be unplugged would have
// put the operator's wiring in front of its own siren.
//
// What it does return is the delay the caller owes the far end before the
// sound starts: never anything about whether a request succeeded, which is
// not known yet and which nothing on that path is allowed to wait for. Only
// config.PhaseBefore produces one — the delay exists so that whatever was
// just switched has a moment to take effect, and there is nothing to wait for
// after a sound that is already over.
//
// The longest delay among the matching webhooks wins, not their sum and not
// the first. Two of them are two independent boxes being told to do the same
// job at the same moment, so the announcement is ready when the slowest of
// them is; adding the waits together would punish an operator for having a
// second relay.
func (d *Dispatcher) Dispatch(phase, trigger string) time.Duration {
	if d == nil {
		return 0
	}
	var wait time.Duration
	cfg := d.cfg()
	for _, w := range cfg.Webhooks {
		if !w.Enabled || w.Trigger != trigger || config.WebhookPhase(w) != phase {
			continue
		}
		if phase == config.PhaseBefore {
			wait = max(wait, boundedDelay(w.Delay.Std()))
		}
		d.wg.Add(1)
		go func(w config.Webhook) {
			defer d.wg.Done()
			d.fire(w)
		}(w)
	}
	return wait
}

// boundedDelay brings a delay inside 0..config.MaxWebhookDelay.
//
// config.Load already does this to every stored webhook and the /webhooks
// form refuses to write one outside it, so in a running daemon this changes
// nothing. It is here because this is the line where a number becomes time a
// siren spends not sounding, and that is not a place to rely on two other
// files having done their job.
func boundedDelay(d time.Duration) time.Duration {
	switch {
	case d < 0:
		return 0
	case d > config.MaxWebhookDelay:
		return config.MaxWebhookDelay
	default:
		return d
	}
}

// Wait blocks until every dispatch started so far has finished. Nothing on
// the announcement path calls it; it exists so a test can assert on what a
// dispatch did without polling, and so a caller that wants to drain before
// shutting down can.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// fire makes one request and records what came of it. It runs in its own
// goroutine and returns nothing: there is no caller left to hand an error to,
// which is precisely the property that keeps a webhook out of the
// announcement path.
func (d *Dispatcher) fire(w config.Webhook) {
	// A stored webhook this build cannot make is refused here rather than
	// at load: config.Validate deliberately ignores webhooks so that a
	// hand-edited one can never stop the daemon starting (see its comment),
	// which leaves exactly one obligation — that the refusal is loud. This
	// is where it is paid.
	if err := config.ValidateWebhook(w); err != nil {
		var werr *config.WebhookError
		field := "webhook"
		if errors.As(err, &werr) {
			field = werr.Field
		}
		d.logger.Error("webhook is not one this device can make; it will not fire",
			"webhook", w.ID, "field", field, "err", err)
		d.record(w, false, "налаштування вебхука хибне: "+fieldWord(field))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout())
	defer cancel()

	// The wait for a slot is inside the deadline, not in front of it: a
	// queue that cannot be got into within the time one request is allowed
	// is a failure the operator has to be told about, not a goroutine that
	// waits quietly for a target that is already gone.
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		d.logger.Warn("webhook was not sent: too many were already in flight", "webhook", w.ID)
		d.record(w, false, "не надіслано: забагато одночасних запитів")
		return
	}

	var body io.Reader
	if w.Body != "" {
		body = bytes.NewBufferString(w.Body)
	}
	req, err := http.NewRequestWithContext(ctx, w.Method, w.URL, body)
	if err != nil {
		// Unreachable for anything ValidateWebhook accepted, and handled
		// rather than ignored because "unreachable" is a claim about today's
		// validator and this is a device nobody can log into.
		d.logger.Error("webhook request could not be built", "webhook", w.ID, "err", safeError(err))
		d.record(w, false, "не вдалося сформувати запит")
		return
	}
	for _, h := range w.Headers {
		// Host is not an ordinary header: net/http reads it off the Request
		// and ignores whatever the header map says, so an operator whose
		// appliance is behind a name-based proxy would have set a field that
		// silently did nothing.
		if strings.EqualFold(h.Name, "Host") {
			req.Host = h.Value
			continue
		}
		req.Header.Set(h.Name, h.Value)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		reason := transportFailure(ctx, err)
		// target(), never the URL: a URL can carry credentials in its
		// userinfo and secrets in its query — the operator's own example is
		// "?key=8" — and this line goes to the journal.
		d.logger.Warn("webhook failed", "webhook", w.ID, "target", target(w.URL), "err", reason)
		d.record(w, false, reason)
		return
	}
	defer resp.Body.Close()
	answered, _ := io.ReadAll(io.LimitReader(resp.Body, maxDrainedBody))
	answer := excerpt(answered, w)

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// See CheckRedirect: the request was not delivered where the
		// operator pointed it, and it was not followed on purpose.
		d.logger.Warn("webhook answered with a redirect, which is not followed",
			"webhook", w.ID, "target", target(w.URL), "status", resp.StatusCode)
		d.record(w, false, withAnswer(fmt.Sprintf("перенаправлення %d не виконується", resp.StatusCode), answer))
	case resp.StatusCode >= 400:
		d.logger.Warn("webhook was refused", "webhook", w.ID, "target", target(w.URL), "status", resp.StatusCode)
		d.record(w, false, withAnswer(fmt.Sprintf("відповідь %d", resp.StatusCode), answer))
	default:
		d.logger.Info("webhook sent", "webhook", w.ID, "target", target(w.URL), "status", resp.StatusCode)
		d.record(w, true, withAnswer(fmt.Sprintf("відповідь %d", resp.StatusCode), answer))
	}
}

// excerpt reduces a response body to a short line for the event log.
//
// It is recorded because a status code is not evidence the announcement will
// be heard: the far end can accept the request, answer 200, and report in the
// body that it is still in the state that keeps this device inaudible. An
// operator cannot tell a working relay from a broken one without seeing what
// it actually said.
//
// It is never parsed and nothing branches on it. Every target speaks its own
// dialect — XML here, a line of plain text there, an empty body somewhere
// else — and a device that decided whether to sound a siren from a body whose
// shape it had guessed would be worse than one that always sounds it.
//
// Three things are done to it, and each is because this is untrusted input on
// its way onto a page in a building somebody lives in:
//
//   - cut to maxLoggedBody, at a rune boundary, so a target answering with a
//     web page cannot bury the log;
//   - collapsed to printable characters on one line, so control bytes and
//     binary never reach the renderer;
//   - scrubbed of this webhook's own header values, because a far end that
//     echoes the request back would otherwise put a credential into the event
//     log by way of this device. "A header value is never logged" is a rule
//     this side of the wire keeps whatever the other side does.
func excerpt(body []byte, w config.Webhook) string {
	text := string(body)
	for _, h := range w.Headers {
		if len(h.Value) >= minRedactedHeaderValue {
			text = strings.ReplaceAll(text, h.Value, "…")
		}
	}

	var b strings.Builder
	space := false
	for _, r := range text {
		if b.Len() >= maxLoggedBody {
			break
		}
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case r == utf8.RuneError || !unicode.IsPrint(r):
			// A byte that is not valid UTF-8, or a control character. Both
			// are dropped rather than replaced: a binary body would
			// otherwise render as a wall of question marks that says less
			// than nothing.
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// withAnswer joins what this device made of the request to what came back.
// Either half can be empty: a target that answers with no body at all is
// perfectly normal, and is not something to write an empty phrase about.
func withAnswer(reason, answer string) string {
	if answer == "" {
		return reason
	}
	if reason == "" {
		return "у відповідь: " + answer
	}
	return reason + " · у відповідь: " + answer
}

func (d *Dispatcher) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return DefaultTimeout
}

// record writes the row that makes a webhook's outcome visible to whoever is
// standing in front of the device.
//
// Both outcomes are written, not only failures. A webhook that silently stops
// working is worse than none — the operator believes their relay fires and it
// does not — and a log that only ever says "failed" cannot tell "it worked"
// from "it never ran at all".
//
// What is never written here is a header value. The webhook's id is the whole
// of the identification; the reason is the operator's own language, built
// from a status code or a transport failure and, where there was one, an
// excerpt of what the far end answered — which excerpt() has already scrubbed
// of anything the operator typed into a header.
func (d *Dispatcher) record(w config.Webhook, ok bool, reason string) {
	entry := eventlog.Entry{
		At: d.now(), Kind: EventKind, EventID: w.ID, OK: ok, Err: reason,
	}
	if err := d.log.Append(entry); err != nil {
		d.logger.Warn("could not write the event log", "err", err)
	}
}

// target reduces a webhook URL to the part that is safe to log: the scheme
// and the host. A path can be a command, a query can be a key — the
// operator's own example ends in "?key=8" — and userinfo is a password
// outright. The host is what somebody reading the journal needs in order to
// know which box did not answer.
func target(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(адреса)"
	}
	return u.Scheme + "://" + u.Hostname()
}

// transportFailure turns a *url.Error into one operator-facing line.
//
// The URL is dropped rather than printed: url.Error.Error() formats the full
// address, credentials and query and all, and this string is written to the
// event log and rendered on a page. What is kept is the underlying reason,
// which is where "connection refused" and "no such host" live.
func transportFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "перевищено час очікування"
	}
	return "запит не вдався: " + safeError(err)
}

// safeError strips the URL out of a transport error. Every error the http
// client returns is a *url.Error wrapping the real one.
func safeError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// fieldWord names the rejected part of a webhook in Ukrainian, for the event
// log row a stored-but-unmakeable webhook leaves behind. internal/web has its
// own, fuller wording for the form; this one only has to make the row
// readable to whoever is looking for why nothing happened.
func fieldWord(field string) string {
	switch field {
	case config.WebhookFieldURL:
		return "адреса"
	case config.WebhookFieldMethod:
		return "метод"
	case config.WebhookFieldTrigger:
		return "подія"
	case config.WebhookFieldHeaders:
		return "заголовки"
	case config.WebhookFieldBody:
		return "тіло запиту"
	case config.WebhookFieldName:
		return "назва"
	default:
		return "налаштування"
	}
}
