package webhook_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// phased is relay() with a phase and a delay, and an id of its own so the
// event log can tell two of them apart.
func phased(id, url, phase string, delay time.Duration) config.Webhook {
	w := relay(url)
	w.ID = id
	w.Phase = phase
	w.Delay = config.Duration(delay)
	return w
}

// A phase is half of the address of a webhook: a request meant for the moment
// the device starts speaking must not also go out when it stops, or whatever
// was switched is switched twice and left where it started.
func TestAWebhookFiresOnItsPhaseAndNotOnAnother(t *testing.T) {
	before := newRecorder(t, nil)
	after := newRecorder(t, nil)
	h := newHarness(t,
		phased("w-before", before.srv.URL, config.PhaseBefore, 0),
		phased("w-after", after.srv.URL, config.PhaseAfter, 0),
	)

	h.d.Dispatch(config.PhaseBefore, config.TriggerAlertStart)
	h.d.Wait()

	if got := before.count(); got != 1 {
		t.Fatalf("the before-phase endpoint was called %d times, want 1", got)
	}
	if got := after.count(); got != 0 {
		t.Fatalf("the after-phase endpoint was called %d times on a before dispatch", got)
	}

	h.d.Dispatch(config.PhaseAfter, config.TriggerAlertStart)
	h.d.Wait()

	if got := after.count(); got != 1 {
		t.Fatalf("the after-phase endpoint was called %d times, want 1", got)
	}
	if got := before.count(); got != 1 {
		t.Fatalf("the before-phase endpoint was called %d times in total, want 1", got)
	}
}

// A webhook stored before phases existed has none, and every device in the
// field is full of them. It fires where it always did and nowhere else.
func TestAPhaselessWebhookFiresOnTheTriggerPhaseOnly(t *testing.T) {
	rec := newRecorder(t, nil)
	h := newHarness(t, relay(rec.srv.URL)) // no Phase set at all

	h.d.Dispatch(config.PhaseBefore, config.TriggerAlertStart)
	h.d.Dispatch(config.PhaseAfter, config.TriggerAlertStart)
	h.d.Wait()
	if got := rec.count(); got != 0 {
		t.Fatalf("a phase-less webhook fired %d times on the sound phases", got)
	}

	h.d.Dispatch(config.PhaseOnTrigger, config.TriggerAlertStart)
	h.d.Wait()
	if got := rec.count(); got != 1 {
		t.Fatalf("a phase-less webhook fired %d times when its trigger happened, want 1", got)
	}
}

// Two before-webhooks are two independent boxes told to do the same job at
// the same moment. The announcement is ready when the slowest of them is, so
// the longest delay wins — not the sum, which would charge the operator for
// owning a second relay, and not the first, which would leave the slower one
// still catching up when the siren started.
func TestTheLongestBeforeDelayIsTheOneReported(t *testing.T) {
	a := newRecorder(t, nil)
	b := newRecorder(t, nil)
	h := newHarness(t,
		phased("w-fast", a.srv.URL, config.PhaseBefore, 2*time.Second),
		phased("w-slow", b.srv.URL, config.PhaseBefore, 5*time.Second),
	)

	got := h.d.Dispatch(config.PhaseBefore, config.TriggerAlertStart)
	h.d.Wait()

	if got != 5*time.Second {
		t.Fatalf("delay = %v, want the longest of the two (5s)", got)
	}
}

// The delay belongs to the before phase. Waiting after a sound that is
// already over is time nobody asked for and a goroutine held to no end.
func TestNoPhaseButBeforeReportsADelay(t *testing.T) {
	rec := newRecorder(t, nil)
	for _, phase := range []string{config.PhaseAfter, config.PhaseOnTrigger} {
		h := newHarness(t, phased("w1", rec.srv.URL, phase, 5*time.Second))
		if got := h.d.Dispatch(phase, config.TriggerAlertStart); got != 0 {
			t.Fatalf("the %s phase reported a %v delay, want none", phase, got)
		}
		h.d.Wait()
	}
}

// The cap is enforced again here, at the line where a stored number becomes
// time a siren spends not sounding. config.Load already lowers it and the
// form refuses to write it, and this is still not a place to rely on two
// other files having done their job.
func TestAnOverLongStoredDelayIsStillBoundedAtDispatch(t *testing.T) {
	rec := newRecorder(t, nil)
	h := newHarness(t, phased("w1", rec.srv.URL, config.PhaseBefore, time.Hour))

	got := h.d.Dispatch(config.PhaseBefore, config.TriggerAlertStart)
	h.d.Wait()

	if got != config.MaxWebhookDelay {
		t.Fatalf("delay = %v, want it bounded to %v", got, config.MaxWebhookDelay)
	}
}

// A status code is not evidence the announcement will be heard. A controller
// can take the request, answer 200, and report in its body that it is still
// in the state that keeps this device inaudible — and the operator has no
// other way to find that out.
func TestTheAnswerFromTheFarEndIsRecorded(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, "<?xml version=\"1.0\"?><result><state>1</state></result>")
	})
	h := newHarness(t, relay(rec.srv.URL))

	h.d.Dispatch(config.PhaseOnTrigger, config.TriggerAlertStart)
	h.d.Wait()

	row := h.onlyEntry(t)
	if !row.OK {
		t.Fatalf("a 200 was recorded as a failure: %+v", row)
	}
	if !strings.Contains(row.Err, "<state>1</state>") {
		t.Fatalf("the row does not carry what the far end answered: %q", row.Err)
	}
}

// The excerpt is bytes from a box on somebody's network on their way into a
// table rendered in a building they live in. It is cut hard, and it is never
// parsed — this test asserts the bound, not a shape, because there is no
// shape: one target answers XML, the next a line of text, the next nothing.
func TestTheAnswerIsCutShortAndStrippedOfControlBytes(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\x00\x07 " + strings.Repeat("A", 4096)))
	})
	h := newHarness(t, relay(rec.srv.URL))

	h.d.Dispatch(config.PhaseOnTrigger, config.TriggerAlertStart)
	h.d.Wait()

	row := h.onlyEntry(t)
	if len(row.Err) > 300 {
		t.Fatalf("the recorded answer is %d bytes; a target answering with a web page could bury the log", len(row.Err))
	}
	if strings.ContainsAny(row.Err, "\x00\x07\n\r") {
		t.Fatalf("a control byte reached the event log: %q", row.Err)
	}
}

// A header value is a credential, and "it is never logged" is a rule this
// side of the wire keeps whatever the other side does. A far end that echoes
// the request back must not put the operator's own Authorization header into
// their event log by way of this device.
func TestAHeaderValueEchoedBackIsScrubbedFromTheAnswer(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "you sent %s", req.Header.Get("Authorization"))
	})
	h := newHarness(t, relay(rec.srv.URL))

	h.d.Dispatch(config.PhaseOnTrigger, config.TriggerAlertStart)
	h.d.Wait()

	row := h.onlyEntry(t)
	if strings.Contains(row.Err, secret) {
		t.Fatalf("the credential reached the event log through the response body: %q", row.Err)
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Fatalf("the credential reached the journal: %q", h.logs.String())
	}
}

// Dispatch has to return before a byte is sent, phase or no phase: it is
// called on the announcing path and the siren does not wait for a box that
// may be unplugged. The delay it reports is a number read out of the
// configuration, not anything learned from the far end.
func TestDispatchReturnsBeforeTheRequestIsMade(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 1)
	rec := newRecorder(t, func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
		<-release
	})
	defer close(release)

	h := newHarness(t, phased("w1", rec.srv.URL, config.PhaseBefore, 3*time.Second))
	h.d.Timeout = 2 * time.Second

	done := make(chan time.Duration, 1)
	go func() { done <- h.d.Dispatch(config.PhaseBefore, config.TriggerAlertStart) }()

	select {
	case got := <-done:
		if got != 3*time.Second {
			t.Fatalf("delay = %v, want 3s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch did not return while a request was in flight")
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("the request was never made")
	}
}
