package alertsource

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// Outcome is what happened to one cascade key on one poll.
//
// Three values, and the reason there are three rather than two is the whole
// point of this file. The cascade stops at the first source that answers, so
// the keys after it were never asked. They are not healthy — nobody checked —
// and they are not broken either. Reporting an unconsulted source as working
// is a claim nothing verified; reporting it as failed sends whoever installed
// the device chasing a fault that does not exist. It has to be its own
// answer.
//
// OutcomeNotReached is the zero value on purpose: an attempt nobody filled in
// is one nobody made.
type Outcome int

const (
	// OutcomeNotReached means the cascade never got this far, because an
	// earlier key answered or the poll was cancelled.
	OutcomeNotReached Outcome = iota
	// OutcomeAnswered means this key produced the snapshot that was
	// returned. Exactly one key in a successful cascade has it.
	OutcomeAnswered
	// OutcomeFailed means this key was asked and did not answer.
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAnswered:
		return "answered"
	case OutcomeFailed:
		return "failed"
	default:
		return "not reached"
	}
}

// Reason classifies a failure into a fixed vocabulary this package owns.
//
// It exists so that a card on the dashboard can say why a source did not
// answer without any upstream text reaching the page. An error from a fetch
// can carry whatever the far end sent — an HTML error page, a captive
// portal's sign-in copy, a content type it chose — and a request that
// authenticates with a Bearer token is one wrapped error away from printing
// the credential beside it. A closed set of values cannot carry any of that.
type Reason string

const (
	// ReasonNone is a key that did not fail: it answered, or was never
	// reached.
	ReasonNone Reason = ""
	// ReasonNoToken is a source that needs a credential this device does
	// not have. Nothing was sent; the operator has to go and get one.
	ReasonNoToken Reason = "no_token"
	// ReasonTokenRejected is a credential that was sent and refused —
	// expired, revoked, or mistyped. The operator has one; it needs
	// replacing.
	ReasonTokenRejected Reason = "token_rejected"
	// ReasonRateLimited is an upstream 429. The limit is per public IP, so
	// this can be another device on the same connection rather than
	// anything wrong here.
	ReasonRateLimited Reason = "rate_limited"
	// ReasonUnreachable is a request that never completed: no route, no
	// DNS, a refused connection, a timeout. This is what a device with no
	// working internet looks like, and it looks the same on every source
	// at once — which is itself the diagnosis.
	ReasonUnreachable Reason = "unreachable"
	// ReasonBadResponse is a reply that arrived and could not be used: a
	// status we do not handle, a body that is not the JSON it claims to
	// be, a snapshot with nothing in it.
	ReasonBadResponse Reason = "bad_response"
)

// Attempt is one cascade key's place in one poll.
type Attempt struct {
	// Key is the cascade key — "default", "skog", AlertsUAKey — and not
	// the name the upstream reports for itself. Those are different
	// strings and the difference matters: "default" answers as "Vadym
	// Klymenko API", so a card built from self-reported names could not
	// tell an operator which of the keys they configured is the one
	// working.
	Key     string
	Outcome Outcome

	// Err is why this key failed, for a log line, and nil otherwise.
	//
	// It must not be rendered. It carries text from the far end, and the
	// far end is not trusted: see Reason, which is what a page shows.
	Err error
}

// Reason classifies this attempt's failure. It is ReasonNone for a key that
// answered or was never reached.
func (a Attempt) Reason() Reason {
	if a.Outcome != OutcomeFailed || a.Err == nil {
		return ReasonNone
	}
	switch {
	case errors.Is(a.Err, ErrNoToken):
		return ReasonNoToken
	case errors.Is(a.Err, ErrTokenRejected):
		return ReasonTokenRejected
	}
	var rl *RateLimitError
	if errors.As(a.Err, &rl) {
		return ReasonRateLimited
	}
	// A *url.Error is what net/http returns when the request itself did
	// not complete — no route, refused, timed out. Anything else got a
	// reply we could not use.
	var ue *url.Error
	if errors.As(a.Err, &ue) {
		return ReasonUnreachable
	}
	return ReasonBadResponse
}

// Cascade is a Source that can also say what happened to every key it holds.
//
// Fetch's contract is unchanged for callers that only want the snapshot;
// FetchCascade is for the one caller that has to explain the cascade to a
// person. Reporting only — it does not change which source is used, and
// nothing here compares sources or prefers one answer to another. The
// cascade still stops at the first key that answers.
type Cascade interface {
	Source
	FetchCascade(ctx context.Context) (Snapshot, []Attempt, error)
}

// FetchCascade tries each configured key in order and returns the first
// success, along with one Attempt per configured key, in cascade order.
//
// Every key gets an entry whether or not it was tried, so a caller never has
// to reconstruct "not reached" from a list's length — which is how an
// unconsulted source ends up displayed as a working one.
func (c *Client) FetchCascade(ctx context.Context) (Snapshot, []Attempt, error) {
	attempts := make([]Attempt, len(c.sources))
	for i, src := range c.sources {
		attempts[i] = Attempt{Key: src}
	}

	var errs []error
	for i, src := range c.sources {
		snap, err := c.fetchOne(ctx, src)
		if err == nil {
			attempts[i].Outcome = OutcomeAnswered
			return snap, attempts, nil
		}
		attempts[i].Outcome = OutcomeFailed
		attempts[i].Err = err
		errs = append(errs, fmt.Errorf("source %s: %w", src, err))
		// A cancelled poll leaves the remaining keys not reached, which is
		// exactly what happened to them.
		if ctx.Err() != nil {
			break
		}
	}
	return Snapshot{}, attempts, errors.Join(errs...)
}

// FetchCascade lets a single alerts.in.ua source report itself in the same
// shape a multi-key cascade does, so a caller assembling a mixed cascade does
// not need a special case for the one provider that is not a "?source=" key.
func (c *AlertsUA) FetchCascade(ctx context.Context) (Snapshot, []Attempt, error) {
	snap, err := c.Fetch(ctx)
	attempt := Attempt{Key: AlertsUAKey, Outcome: OutcomeAnswered}
	if err != nil {
		attempt.Outcome = OutcomeFailed
		attempt.Err = err
	}
	return snap, []Attempt{attempt}, err
}
