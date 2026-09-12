package web_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// The two halves of the operator's pair, typed into the form and read back
// off the disk. A phase that did not survive the round trip would leave the
// request firing at a moment nobody chose.
func TestAPhaseAndADelaySurviveTheForm(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)

	form := webhookForm()
	form.Set("phase", config.PhaseBefore)
	form.Set("delay", "4")
	if rec := submit(t, srv, cookie, "/webhooks/add", form); rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	hooks := store.Get().Webhooks
	if len(hooks) != 1 {
		t.Fatalf("%d webhooks stored, want 1", len(hooks))
	}
	if hooks[0].Phase != config.PhaseBefore {
		t.Fatalf("phase = %q, want %q", hooks[0].Phase, config.PhaseBefore)
	}
	if got := hooks[0].Delay.Std(); got != 4*time.Second {
		t.Fatalf("delay = %v, want 4s", got)
	}

	body := webhookPage(t, srv, cookie, "/webhooks")
	if !strings.Contains(body, `value="4"`) {
		t.Fatal("the stored delay is not rendered back into the edit form")
	}
}

// The delay belongs to the before phase alone. Storing a number that nothing
// will ever read would put a setting on the screen that does not do what it
// says, which on a device with no other screen is worse than not offering it.
func TestADelayIsDroppedForAPhaseThatCannotUseIt(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)

	form := webhookForm()
	form.Set("phase", config.PhaseAfter)
	form.Set("delay", "6")
	if rec := submit(t, srv, cookie, "/webhooks/add", form); rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	stored := store.Get().Webhooks[0]
	if stored.Phase != config.PhaseAfter {
		t.Fatalf("phase = %q, want %q", stored.Phase, config.PhaseAfter)
	}
	if got := stored.Delay.Std(); got != 0 {
		t.Fatalf("delay = %v, want none: only the before phase has one", got)
	}
}

// A stored delay over the cap is lowered on load, because refusing it would
// stop the daemon starting. The same number typed here is refused outright:
// somebody asking for something this device will not do should be told so,
// not silently given something else.
func TestADelayAboveTheCapIsRefusedByTheForm(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)

	form := webhookForm()
	form.Set("phase", config.PhaseBefore)
	form.Set("delay", "45")
	rec := submit(t, srv, cookie, "/webhooks/add", form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a delay above the %v cap", rec.Code, config.MaxWebhookDelay)
	}
	if got := len(store.Get().Webhooks); got != 0 {
		t.Fatalf("%d webhooks stored, want the submission refused before anything was written", got)
	}
	if !strings.Contains(rec.Body.String(), "Затримка") {
		t.Fatalf("the rejection does not say which field is wrong; body:\n%s", rec.Body.String())
	}
}

// A webhook stored before phases existed has none, and the card must show the
// moment it actually fires at rather than an empty picker. Whoever opens this
// screen after an upgrade has to be able to see what their device is doing
// now without changing it.
func TestAPhaselessStoredWebhookShowsTheMomentItActuallyFires(t *testing.T) {
	srv, store, cookie := newWebhookServer(t)

	if err := store.Update(func(c *config.Config) error {
		c.Webhooks = []config.Webhook{{
			ID: "webhook-legacy", Name: "Реле", Trigger: config.TriggerAlertStart,
			Method: http.MethodGet, URL: "http://192.168.1.50/Sendkey?key=8", Enabled: true,
		}}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// This webhook's own picker, not the add form's — that one always opens
	// on the trigger phase, and asserting against the whole page would pass
	// on its markup while the card beside it showed nothing chosen at all.
	picker := selectMarkup(t, webhookPage(t, srv, cookie, "/webhooks"), "phase-webhook-legacy")
	if !strings.Contains(picker, `<option value="on_trigger" selected>`) {
		t.Fatalf("a phase-less webhook does not show as firing when its trigger happens; picker:\n%s", picker)
	}
}

// selectMarkup returns the <select> with the given id, so an assertion can be
// about one card rather than about anything on the page that happens to match.
func selectMarkup(t *testing.T, body, id string) string {
	t.Helper()
	i := strings.Index(body, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element with id %q on the page:\n%s", id, body)
	}
	rest := body[i:]
	end := strings.Index(rest, "</select>")
	if end < 0 {
		t.Fatalf("the element with id %q is not a closed <select>", id)
	}
	return rest[:end]
}

// This screen works with scripts off like every other control here, so it
// cannot swap the explanation when the picker changes. All three moments are
// spelled out on the add form, because an operator choosing between them
// needs to read what they mean before choosing.
func TestTheAddFormExplainsEveryPhaseWithoutScripting(t *testing.T) {
	srv, _, cookie := newWebhookServer(t)
	body := webhookPage(t, srv, cookie, "/webhooks")

	// The sentences, not the picker labels: an <option> that says "Після
	// звуку" and nothing else leaves an operator guessing what it does.
	for moment, explains := range map[string]string{
		"Коли подія стається": "навіть якщо звук не пролунає",
		"Перед звуком":        "починає говорити",
		"Після звуку":         "замовкає",
	} {
		if !strings.Contains(body, moment) {
			t.Fatalf("the page does not name the %q moment; body:\n%s", moment, body)
		}
		if !strings.Contains(body, explains) {
			t.Fatalf("the page names %q without saying what it does; body:\n%s", moment, body)
		}
	}
	if !strings.Contains(body, "повертає") {
		t.Fatal("the page does not say that the after request is what puts the operator's hardware back")
	}
}
