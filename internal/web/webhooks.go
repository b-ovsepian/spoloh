package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// This screen answers the operator who asked to drive their own hardware
// from this device, with the request their vendor documented:
//
//	curl -H 'Authorization: Basic ...' 'http://192.168.X.X/Sendkey?key=8'
//
// so the form is a method, an address, headers and a body, and nothing about
// it is a template of a request somebody guessed. What fires it is
// internal/webhook; what is stored is config.Webhook.
//
// The one rule this file keeps that the others do not: a header value is a
// credential. It is rendered into its own webhook's edit form and into no
// other response — no list summary, no error message, no log line. The same
// rule the administrator password hash has, kept the same way, and walked by
// webhooks_test.go the way leak_test.go walks the hash.

// headerSeparator is what a header line is split on in the textarea below.
// A colon, as in the request itself, so what the operator types is what they
// would have typed after curl's -H.
const headerSeparator = ":"

// webhookHeadersText renders stored headers as one "Name: value" per line —
// the form a textarea can hold, which is what lets this screen work with
// JavaScript off. A row-per-header editor would need scripting to add a row.
func webhookHeadersText(headers []config.WebhookHeader) string {
	var b strings.Builder
	for i, h := range headers {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(h.Name)
		b.WriteString(": ")
		b.WriteString(h.Value)
	}
	return b.String()
}

// parseWebhookHeaders reads that textarea back.
//
// A line with no colon is refused rather than dropped: the operator meant
// something by it, and a header silently missing from a request is exactly
// the failure this feature must not have — their relay would not fire and
// the screen would show no reason. Blank lines are skipped, because a
// textarea collects them by itself.
//
// Splitting on lines is also what makes header injection impossible to
// express: a CR or an LF inside a value ends the line and starts a new
// header name, which is then checked like any other. config.ValidateWebhook
// refuses control characters again on the far side.
func parseWebhookHeaders(text string) ([]config.WebhookHeader, string) {
	var out []config.WebhookHeader
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, headerSeparator)
		if !ok {
			// The line itself is never quoted back: it is the half of the
			// form that carries credentials, and this message travels into
			// a rendered page that the walk in webhooks_test.go scans.
			return nil, "Кожен заголовок пишеться як «Назва: значення», по одному в рядку."
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		out = append(out, config.WebhookHeader{Name: name, Value: value})
	}
	return out, ""
}

// webhookFieldMessage says, in the operator's language, which part of a
// submission config.ValidateWebhook refused. The checks live in one place —
// config — and this is the translation of their verdict, so the two cannot
// come to disagree about what is legal.
func webhookFieldMessage(err error) string {
	var werr *config.WebhookError
	if !errors.As(err, &werr) {
		return "Не вдалося зберегти вебхук. Перевірте введені дані."
	}
	switch werr.Field {
	case config.WebhookFieldURL:
		return "Адреса має починатися з http:// або https:// і містити хост, наприклад http://192.168.1.50/Sendkey?key=8"
	case config.WebhookFieldMethod:
		return "Оберіть метод зі списку."
	case config.WebhookFieldTrigger:
		return "Оберіть подію зі списку."
	case config.WebhookFieldHeaders:
		return "Заголовок написано неправильно: назва може містити лише літери, цифри та «-», а значення — лише звичайні символи одного рядка."
	case config.WebhookFieldBody:
		return fmt.Sprintf("Тіло запиту задовге — максимум %d символів.", config.MaxWebhookBodyLen)
	case config.WebhookFieldName:
		return fmt.Sprintf("Назва задовга — максимум %d символів.", config.MaxWebhookNameLen)
	default:
		return "Не вдалося зберегти вебхук. Перевірте введені дані."
	}
}

// webhookRowView is one webhook's card. Headers is the textarea's contents,
// which is the only place in this dashboard a header value is rendered.
type webhookRowView struct {
	ID           string
	Name         string
	Trigger      string
	TriggerLabel string
	Method       string
	URL          string
	Headers      string
	Body         string
	Enabled      bool

	// Invalid is why a stored webhook cannot be fired as it stands, empty
	// when it can. A hand-edited config.json can hold one — config.Validate
	// deliberately accepts anything here rather than refusing to start the
	// daemon — and this is where the operator is told, on the screen that
	// can fix it.
	Invalid string
}

// webhooksView is what webhooks.html reads.
type webhooksView struct {
	Rows     []webhookRowView
	Triggers []webhookTriggerOption
	Methods  []string

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once, the contract every screen here
	// keeps.
	Error   string
	Message string

	// The add form's fields, re-populated after a rejection so an operator
	// who mistyped the address does not retype the headers too.
	AddName    string
	AddTrigger string
	AddMethod  string
	AddURL     string
	AddHeaders string
	AddBody    string
}

type webhookTriggerOption struct {
	Value    string
	Label    string
	Selected bool
}

// webhookTriggers is the picker, in the order the three things happen.
func webhookTriggers(selected string) []webhookTriggerOption {
	out := make([]webhookTriggerOption, 0, 3)
	for _, t := range []string{config.TriggerAlertStart, config.TriggerAlertEnd, config.TriggerScheduled} {
		out = append(out, webhookTriggerOption{Value: t, Label: triggerLabel(t), Selected: t == selected})
	}
	return out
}

// newWebhookID names a freshly added webhook, the same way newEventID names
// an event and for the same reason: an id that collided would silently edit
// somebody else's row.
func newWebhookID(existing []config.Webhook) string {
	for i := 0; ; i++ {
		id := fmt.Sprintf("webhook-%d", time.Now().UnixNano()+int64(i))
		if indexOfWebhook(existing, id) < 0 {
			return id
		}
	}
}

func indexOfWebhook(hooks []config.Webhook, id string) int {
	for i, w := range hooks {
		if w.ID == id {
			return i
		}
	}
	return -1
}

// webhookForm is one submission, parsed. The message is the first thing
// wrong with it, in Ukrainian, empty when there is nothing.
type webhookForm struct {
	Name    string
	Trigger string
	Method  string
	URL     string
	Headers []config.WebhookHeader
	Body    string
	Enabled bool

	// HeadersText is what was typed, kept so a rejection can hand it back
	// rather than making the operator retype a header block.
	HeadersText string
}

func readWebhookForm(r *http.Request) (webhookForm, string) {
	f := webhookForm{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Trigger:     r.FormValue("trigger"),
		Method:      r.FormValue("method"),
		URL:         strings.TrimSpace(r.FormValue("url")),
		Body:        r.FormValue("body"),
		Enabled:     r.FormValue("enabled") != "",
		HeadersText: r.FormValue("headers"),
	}
	headers, message := parseWebhookHeaders(f.HeadersText)
	if message != "" {
		return f, message
	}
	f.Headers = headers
	return f, ""
}

// asWebhook builds the stored value a form describes.
func (f webhookForm) asWebhook(id string) config.Webhook {
	return config.Webhook{
		ID:      id,
		Name:    f.Name,
		Trigger: f.Trigger,
		Method:  f.Method,
		URL:     f.URL,
		Headers: f.Headers,
		Body:    f.Body,
		Enabled: f.Enabled,
	}
}

// webhooksBaseView builds the view a fresh GET, a success and a rejection all
// start from.
func webhooksBaseView(cfg *config.Config) webhooksView {
	view := webhooksView{
		Triggers:   webhookTriggers(config.TriggerAlertStart),
		Methods:    config.WebhookMethods,
		AddTrigger: config.TriggerAlertStart,
		AddMethod:  http.MethodGet,
	}
	for _, w := range cfg.Webhooks {
		row := webhookRowView{
			ID:           w.ID,
			Name:         w.Name,
			Trigger:      w.Trigger,
			TriggerLabel: triggerLabel(w.Trigger),
			Method:       w.Method,
			URL:          w.URL,
			Headers:      webhookHeadersText(w.Headers),
			Body:         w.Body,
			Enabled:      w.Enabled,
		}
		if err := config.ValidateWebhook(w); err != nil {
			row.Invalid = webhookFieldMessage(err)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

func (s *Server) renderWebhooksPage(w http.ResponseWriter, status int, mutate func(*webhooksView)) {
	view := webhooksBaseView(s.deps.Config.Get())
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "webhooks.html", view)
}

// handleWebhooksPage renders the screen: one card per webhook, plus the form
// that adds another.
func (s *Server) handleWebhooksPage(w http.ResponseWriter, r *http.Request) {
	s.renderWebhooksPage(w, http.StatusOK, nil)
}

// handleWebhooksAdd stores a new webhook.
//
// Validation happens here, before the store is touched, because
// config.Validate deliberately does not check webhooks: a stored one must
// never be able to stop the daemon starting, so this screen is where an
// impossible request is refused — the same split alerts.poll_interval has
// between its form and its loader, for the same reason.
func (s *Server) handleWebhooksAdd(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	form, message := readWebhookForm(r)
	reject := func(msg string) {
		s.renderWebhooksPage(w, http.StatusBadRequest, func(v *webhooksView) {
			v.Error = msg
			v.AddName = form.Name
			v.AddTrigger = form.Trigger
			v.AddMethod = form.Method
			v.AddURL = form.URL
			v.AddHeaders = form.HeadersText
			v.AddBody = form.Body
			v.Triggers = webhookTriggers(form.Trigger)
		})
	}
	if message != "" {
		reject(message)
		return
	}

	stored := s.deps.Config.Get().Webhooks
	if len(stored) >= config.MaxWebhooks {
		reject(fmt.Sprintf("Більше ніж %d вебхуків зберегти не можна.", config.MaxWebhooks))
		return
	}
	hook := form.asWebhook(newWebhookID(stored))
	if err := config.ValidateWebhook(hook); err != nil {
		// The error is not logged: it can quote a header name, and the
		// value beside it is what must never reach a log. The field is
		// enough to know which check refused it.
		reject(webhookFieldMessage(err))
		return
	}

	if err := s.deps.Config.Update(func(c *config.Config) error {
		c.Webhooks = append(c.Webhooks, hook)
		return nil
	}); err != nil {
		s.deps.Logger.Error("webhooks: add rejected", "id", hook.ID, "err", err)
		reject("Не вдалося додати вебхук.")
		return
	}
	s.renderWebhooksPage(w, http.StatusOK, func(v *webhooksView) { v.Message = "Вебхук додано." })
}

// handleWebhooksSave edits one stored webhook in place.
func (s *Server) handleWebhooksSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	id := r.FormValue("id")
	form, message := readWebhookForm(r)
	reject := func(msg string) {
		s.renderWebhooksPage(w, http.StatusBadRequest, func(v *webhooksView) { v.Error = msg })
	}
	if message != "" {
		reject(message)
		return
	}

	hooks := s.deps.Config.Get().Webhooks
	idx := indexOfWebhook(hooks, id)
	if idx < 0 {
		reject("Такого вебхука вже немає.")
		return
	}
	hook := form.asWebhook(id)
	if err := config.ValidateWebhook(hook); err != nil {
		reject(webhookFieldMessage(err))
		return
	}

	if err := s.deps.Config.Update(func(c *config.Config) error {
		i := indexOfWebhook(c.Webhooks, id)
		if i < 0 {
			return fmt.Errorf("webhook %q is gone", id)
		}
		c.Webhooks[i] = hook
		return nil
	}); err != nil {
		s.deps.Logger.Error("webhooks: save rejected", "id", id, "err", err)
		reject("Не вдалося зберегти вебхук.")
		return
	}
	s.renderWebhooksPage(w, http.StatusOK, func(v *webhooksView) { v.Message = "Вебхук збережено." })
}

// handleWebhooksDelete removes one. Nothing here is protected the way
// alert-start and alert-end are on /events: a device with no webhooks is the
// device every one of them ships as, and deleting one costs nothing this
// project exists to protect.
func (s *Server) handleWebhooksDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	id := r.FormValue("id")
	if err := s.deps.Config.Update(func(c *config.Config) error {
		i := indexOfWebhook(c.Webhooks, id)
		if i < 0 {
			return fmt.Errorf("webhook %q is gone", id)
		}
		c.Webhooks = append(c.Webhooks[:i], c.Webhooks[i+1:]...)
		return nil
	}); err != nil {
		s.deps.Logger.Error("webhooks: delete rejected", "id", id, "err", err)
		s.renderWebhooksPage(w, http.StatusBadRequest, func(v *webhooksView) {
			v.Error = "Не вдалося видалити вебхук."
		})
		return
	}
	s.renderWebhooksPage(w, http.StatusOK, func(v *webhooksView) { v.Message = "Вебхук видалено." })
}
