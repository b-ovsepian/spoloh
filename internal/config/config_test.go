package config_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
)

func TestDurationRoundTrip(t *testing.T) {
	var d config.Duration
	if err := json.Unmarshal([]byte(`"5s"`), &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Std() != 5*time.Second {
		t.Fatalf("got %v, want 5s", d.Std())
	}

	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(out) != `"5s"` {
		t.Fatalf("got %s, want \"5s\"", out)
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var d config.Duration
	if err := json.Unmarshal([]byte(`"soon"`), &d); err == nil {
		t.Fatal("expected an error for an unparseable duration")
	}
}

func TestDefaultIsValid(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"empty region", func(c *config.Config) { c.Alerts.Region = "" }},
		{"no sources", func(c *config.Config) { c.Alerts.Sources = nil }},
		{"poll interval too small", func(c *config.Config) {
			c.Alerts.PollInterval = config.Duration(time.Second)
		}},
		{"unknown trigger", func(c *config.Config) { c.Events[0].Trigger = "explode" }},
		{"duplicate event id", func(c *config.Config) {
			c.Events = append(c.Events, c.Events[0])
		}},
		{"bad schedule time", func(c *config.Config) {
			for i := range c.Events {
				if c.Events[i].Trigger == config.TriggerScheduled {
					c.Events[i].Time = "9am"
				}
			}
		}},
		{"weekday out of range", func(c *config.Config) {
			for i := range c.Events {
				if c.Events[i].Trigger == config.TriggerScheduled {
					c.Events[i].Weekdays = []int{0}
				}
			}
		}},
		{"volume out of range", func(c *config.Config) { c.Events[0].Volume = 300 }},
		{"empty sound", func(c *config.Config) { c.Events[0].Sound = "" }},
		{"repeat zero", func(c *config.Config) { c.Events[0].Repeat = 0 }},
		{"repeat above ten", func(c *config.Config) { c.Events[0].Repeat = 11 }},
		{"ap password shorter than WPA2's minimum", func(c *config.Config) {
			c.Network.APPassword = "short1"
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Default()
			tc.mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatalf("expected Validate to reject %s", tc.name)
			}
		})
	}
}

// An empty ap_password is the "not generated yet" state, not an error:
// config.Default() ships it empty, and it must stay valid so a device with no
// installer yet (Plan 3b) can still boot and generate one for itself.
func TestValidateAllowsEmptyAPPassword(t *testing.T) {
	c := config.Default()
	c.Network.APPassword = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected an empty ap_password: %v", err)
	}
}

func TestValidateAllowsAnAPPasswordAtTheWPA2Minimum(t *testing.T) {
	c := config.Default()
	c.Network.APPassword = "eightchr"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected an 8-character ap_password: %v", err)
	}
}

// A shipped device must announce each alert on the first pass, not silence
// itself because a corridor swallowed a single siren: config.Default()'s
// events must all carry the field's default explicitly rather than relying
// on Go's zero value to happen to be right.
func TestDefaultEventsRepeatOnce(t *testing.T) {
	for _, ev := range config.Default().Events {
		if ev.Repeat != 1 {
			t.Errorf("event %q: repeat = %d, want 1", ev.ID, ev.Repeat)
		}
	}
}

func TestValidateAcceptsRepeatAtBothBoundaries(t *testing.T) {
	for _, n := range []int{1, 10} {
		c := config.Default()
		c.Events[0].Repeat = n
		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected repeat %d: %v", n, err)
		}
	}
}

// The operator's instruction after every source in the cascade rate-limited
// at once was "no more often than every 10 seconds", and the 429 that caused
// it asked for 15. Default() therefore sits above the floor rather than
// exactly on it, so a fresh device does not poll at the fastest rate the
// upstream tolerates.
func TestPollIntervalFloorAndDefault(t *testing.T) {
	if config.MinPollInterval != 10*time.Second {
		t.Errorf("MinPollInterval = %v, want 10s", config.MinPollInterval)
	}
	got := config.Default().Alerts.PollInterval.Std()
	if got != 15*time.Second {
		t.Errorf("Default().Alerts.PollInterval = %v, want 15s", got)
	}
	if got <= config.MinPollInterval {
		t.Errorf("Default().Alerts.PollInterval = %v, want it above the %v floor", got, config.MinPollInterval)
	}
}

// Validate is the path a form submission is checked on, so an operator asking
// for an interval the upstream will rate-limit must be told no. That is not
// in conflict with Load repairing a file written by an older build: a human
// asking for something impossible gets an error, a stored config gets
// repaired (see store.go's raisePollIntervalToMinimum).
func TestValidateRejectsAPollIntervalBelowTheMinimum(t *testing.T) {
	c := config.Default()
	c.Alerts.PollInterval = config.Duration(2 * time.Second)
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate accepted a 2s poll interval, below the %v minimum", config.MinPollInterval)
	}
}

func TestValidateAcceptsAPollIntervalAtTheMinimum(t *testing.T) {
	c := config.Default()
	c.Alerts.PollInterval = config.Duration(config.MinPollInterval)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected the %v minimum itself: %v", config.MinPollInterval, err)
	}
}

// Validate must not change the config it is handed. It runs from Save, from
// every dashboard form handler and from Load, so a mutation hiding inside it
// would reach all three and be invisible at each — which is exactly why
// Load's repairs (restoreMissingAlertEvents, restoreMissingRepeat,
// raisePollIntervalToMinimum) live in Load and not here. The config below
// carries one of each repair's input so that moving any of them into Validate
// is caught.
func TestValidateDoesNotMutateTheConfig(t *testing.T) {
	c := config.Default()
	c.Alerts.PollInterval = config.Duration(2 * time.Second)
	c.Events[0].Repeat = 0
	c.Events = c.Events[:1]

	before, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	_ = c.Validate()
	after, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("Validate mutated the config it was given:\nbefore %s\nafter  %s", before, after)
	}
}

func TestEventCategory(t *testing.T) {
	alert := config.Event{Trigger: config.TriggerAlertStart}
	if got := alert.Category(); got != config.CategoryAlerts {
		t.Fatalf("got %q, want %q", got, config.CategoryAlerts)
	}
	sched := config.Event{Trigger: config.TriggerScheduled}
	if got := sched.Category(); got != config.CategoryScheduled {
		t.Fatalf("got %q, want %q", got, config.CategoryScheduled)
	}
}

// Every applies_to a stored config.json can hold must keep loading, whatever
// it says — the empty list, both categories, a category this build has never
// heard of. A device that refuses to start over this field trades a
// suppressed alert for a dead announcer, which is the worse of the two, and
// that has been true since before alerts were selectable at all.
func TestValidateAcceptsEveryQuietHoursCategoryList(t *testing.T) {
	lists := [][]string{
		nil,
		{},
		{config.CategoryScheduled},
		{config.CategoryAlerts},
		{config.CategoryAlerts, config.CategoryScheduled},
		{"alarms"},                 // an older or newer spelling
		{"scheduled", "scheduled"}, // a duplicated checkbox submission
		{"", "alerts", "🙂"},        // junk a hand edit can leave behind
	}
	for _, list := range lists {
		c := config.Default()
		c.QuietHours.Enabled = true
		c.QuietHours.AppliesTo = list
		if err := c.Validate(); err != nil {
			t.Errorf("Validate with applies_to %q = %v, want nil — this field must never stop a device starting", list, err)
		}
	}
}

func TestQuietHoursCoversAndSilencesAlerts(t *testing.T) {
	tests := []struct {
		name         string
		qh           config.QuietHours
		covers       string
		wantCovers   bool
		wantSilences bool
	}{
		{
			name:         "the shipped default covers scheduled only",
			qh:           config.Default().QuietHours,
			covers:       config.CategoryScheduled,
			wantCovers:   true,
			wantSilences: false,
		},
		{
			name:         "alerts listed on an enabled window silences them",
			qh:           config.QuietHours{Enabled: true, AppliesTo: []string{config.CategoryAlerts}},
			covers:       config.CategoryAlerts,
			wantCovers:   true,
			wantSilences: true,
		},
		{
			// The whole point of SilencesAlerts folding in Enabled: a list
			// naming alerts on a window nobody switched on silences nothing,
			// and a warning shown for it would be a warning about nothing.
			name:         "alerts listed on a disabled window silences nothing",
			qh:           config.QuietHours{Enabled: false, AppliesTo: []string{config.CategoryAlerts}},
			covers:       config.CategoryAlerts,
			wantCovers:   true,
			wantSilences: false,
		},
		{
			name:         "a category nobody knows matches nothing",
			qh:           config.QuietHours{Enabled: true, AppliesTo: []string{"alarms"}},
			covers:       config.CategoryAlerts,
			wantCovers:   false,
			wantSilences: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.qh.Covers(tc.covers); got != tc.wantCovers {
				t.Errorf("Covers(%q) = %v, want %v", tc.covers, got, tc.wantCovers)
			}
			if got := tc.qh.SilencesAlerts(); got != tc.wantSilences {
				t.Errorf("SilencesAlerts() = %v, want %v", got, tc.wantSilences)
			}
		})
	}
}

// A webhook carries an operator's credentials to hardware on their own
// network, so what is refused and what is accepted both matter. The address
// in every legal case below is a private one on purpose: 192.168.x.x is the
// intended target of this feature, not something to be defended against.
func TestValidateWebhook(t *testing.T) {
	// base is the operator's own example, translated into stored form.
	base := func() config.Webhook {
		return config.Webhook{
			ID:      "webhook-1",
			Name:    "Реле",
			Trigger: config.TriggerAlertStart,
			Method:  "GET",
			URL:     "http://192.168.1.50/Sendkey?key=8",
			Headers: []config.WebhookHeader{{Name: "Authorization", Value: "Basic c3UzZzRnbzZzazc6amkzOTQ1NHh1L14="}},
			Enabled: true,
		}
	}

	tests := []struct {
		name      string
		mutate    func(*config.Webhook)
		wantField string // "" means the webhook must be accepted
	}{
		{name: "the operator's own relay request"},
		{
			name:   "https to a name",
			mutate: func(w *config.Webhook) { w.URL = "https://relay.example/on" },
		},
		{
			name:   "a POST with a body",
			mutate: func(w *config.Webhook) { w.Method = "POST"; w.Body = `{"relay":1}` },
		},
		{
			name:      "no address at all",
			mutate:    func(w *config.Webhook) { w.URL = "" },
			wantField: config.WebhookFieldURL,
		},
		{
			// Not a request this device makes; it would read its own disk
			// and call the result a webhook.
			name:      "a file url",
			mutate:    func(w *config.Webhook) { w.URL = "file:///etc/spoloh/config.json" },
			wantField: config.WebhookFieldURL,
		},
		{
			name:      "a gopher url",
			mutate:    func(w *config.Webhook) { w.URL = "gopher://192.168.1.50:25/_MAIL" },
			wantField: config.WebhookFieldURL,
		},
		{
			name:      "an address naming no host",
			mutate:    func(w *config.Webhook) { w.URL = "http:///Sendkey" },
			wantField: config.WebhookFieldURL,
		},
		{
			name:      "not an address at all",
			mutate:    func(w *config.Webhook) { w.URL = "192.168.1.50/Sendkey" },
			wantField: config.WebhookFieldURL,
		},
		{
			name:      "a method nobody defined",
			mutate:    func(w *config.Webhook) { w.Method = "FETCH" },
			wantField: config.WebhookFieldMethod,
		},
		{
			// A method with a space in it is a second request line.
			name:      "a method carrying a space",
			mutate:    func(w *config.Webhook) { w.Method = "GET /x HTTP/1.1" },
			wantField: config.WebhookFieldMethod,
		},
		{
			name:      "an unknown trigger",
			mutate:    func(w *config.Webhook) { w.Trigger = "sunrise" },
			wantField: config.WebhookFieldTrigger,
		},
		{
			// Header injection: the value ends the header and starts one the
			// operator never audited, on a request this device signs.
			name: "a newline inside a header value",
			mutate: func(w *config.Webhook) {
				w.Headers = []config.WebhookHeader{{Name: "X-Key", Value: "8\r\nX-Admin: 1"}}
			},
			wantField: config.WebhookFieldHeaders,
		},
		{
			name: "a header name that is not a token",
			mutate: func(w *config.Webhook) {
				w.Headers = []config.WebhookHeader{{Name: "X Key", Value: "8"}}
			},
			wantField: config.WebhookFieldHeaders,
		},
		{
			name: "a header with no name",
			mutate: func(w *config.Webhook) {
				w.Headers = []config.WebhookHeader{{Name: "", Value: "8"}}
			},
			wantField: config.WebhookFieldHeaders,
		},
		{
			name:      "no id",
			mutate:    func(w *config.Webhook) { w.ID = "" },
			wantField: config.WebhookFieldID,
		},
		{
			name:      "a body longer than the cap",
			mutate:    func(w *config.Webhook) { w.Body = strings.Repeat("x", config.MaxWebhookBodyLen+1) },
			wantField: config.WebhookFieldBody,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := base()
			if tc.mutate != nil {
				tc.mutate(&w)
			}
			err := config.ValidateWebhook(w)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("ValidateWebhook rejected a webhook this device can make: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateWebhook accepted a webhook this device must not make")
			}
			var werr *config.WebhookError
			if !errors.As(err, &werr) {
				t.Fatalf("error %v is not a *config.WebhookError; the dashboard could not name the field", err)
			}
			if werr.Field != tc.wantField {
				t.Errorf("refused field %q, want %q", werr.Field, tc.wantField)
			}
		})
	}
}

// A header value is a credential. It must never be quoted into an error,
// because an error travels into the journal and onto a page.
func TestARejectedHeaderDoesNotQuoteItsValue(t *testing.T) {
	const secret = "Basic c3UzZzRnbzZzazc6amkzOTQ1NHh1L14="
	w := config.Webhook{
		ID: "webhook-1", Trigger: config.TriggerAlertStart, Method: "GET",
		URL:     "http://192.168.1.50/Sendkey?key=8",
		Headers: []config.WebhookHeader{{Name: "Authorization", Value: secret + strings.Repeat("x", config.MaxHeaderValueLen)}},
	}
	err := config.ValidateWebhook(w)
	if err == nil {
		t.Fatal("an over-long header value was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the rejection quotes the header value: %v", err)
	}
}

// config.Validate must not judge webhooks, for any value a stored file can
// hold. A device that refuses to start because a hand-edited request to a
// relay board names a scheme this build does not make is a device that
// announces no air raid alerts — which is the worse of the two outcomes by
// the only measure this project has. The refusal happens where it costs
// nothing: on the form, and at the moment the webhook would have fired.
func TestValidateAcceptsAStoredWebhookItWouldNeverFire(t *testing.T) {
	c := config.Default()
	c.Webhooks = []config.Webhook{{
		ID:      "",
		Trigger: "sunrise",
		Method:  "FETCH",
		URL:     "gopher://192.168.1.50/_x",
		Headers: []config.WebhookHeader{{Name: "X Key", Value: "8\r\nX-Admin: 1"}},
		Enabled: true,
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("a stored webhook stopped the configuration loading: %v", err)
	}
	if err := config.ValidateWebhook(c.Webhooks[0]); err == nil {
		t.Fatal("ValidateWebhook accepted it too, so this test proves nothing")
	}
}

// Clone has to copy the header slice, not share it: the store hands one
// configuration to the engine's goroutine and another to an HTTP handler,
// and a shared backing array is a write from the dashboard landing in the
// engine's copy mid-announcement.
func TestCloneCopiesWebhookHeaders(t *testing.T) {
	c := config.Default()
	c.Webhooks = []config.Webhook{{
		ID: "webhook-1", Trigger: config.TriggerAlertStart, Method: "GET",
		URL:     "http://192.168.1.50/Sendkey?key=8",
		Headers: []config.WebhookHeader{{Name: "Authorization", Value: "Basic one"}},
	}}

	clone := c.Clone()
	clone.Webhooks[0].Headers[0].Value = "Basic two"

	if got := c.Webhooks[0].Headers[0].Value; got != "Basic one" {
		t.Errorf("the original's header value is now %q; the clone shares its headers", got)
	}
}

// A configuration with no webhooks must round-trip with no webhooks key at
// all, so saving an unrelated setting on a device that has none does not
// start writing "webhooks": [] into every config.json in the field.
func TestACloneOfAConfigWithNoWebhooksStaysEmpty(t *testing.T) {
	clone := config.Default().Clone()
	if clone.Webhooks != nil {
		t.Fatalf("Webhooks = %v, want nil", clone.Webhooks)
	}
	out, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "webhooks") {
		t.Errorf("a device with no webhooks writes a webhooks key: %s", out)
	}
}

// A fresh device stores no location and no token: zero means "the whole
// oblast named by Region", which is what every device did before there was
// anywhere to put a location. If Default() ever started shipping a UID, every
// new install would announce for somewhere nobody chose.
func TestDefaultShipsNoLocationAndNoToken(t *testing.T) {
	c := config.Default()
	if c.Alerts.LocationUID != 0 {
		t.Errorf("Default().Alerts.LocationUID = %d, want 0 — the whole oblast", c.Alerts.LocationUID)
	}
	if c.Alerts.AlertsUAToken != "" {
		t.Error("Default() ships an alerts.in.ua token")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
}

// A config.json written before these fields existed has neither key, and must
// load as "the whole oblast, no credential" rather than as anything that
// stops the daemon. Every device in the field is holding one of these files.
func TestAConfigWrittenBeforeTheLocationFieldsLoadsUnchanged(t *testing.T) {
	c := config.Default()
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// omitempty on both keys is what keeps an unrelated save from writing
	// "location_uid": 0 and an empty token into every config.json in the
	// field.
	for _, key := range []string{"location_uid", "alerts_ua_token"} {
		if strings.Contains(string(out), key) {
			t.Errorf("a device with no location written writes a %q key: %s", key, out)
		}
	}

	var back config.Config
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Alerts.LocationUID != 0 || back.Alerts.AlertsUAToken != "" {
		t.Errorf("Alerts = %+v, want the location and the token unset", back.Alerts)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("a config with no location must still validate: %v", err)
	}
}

// Validate is what every load runs through, so it must not be the thing that
// stops a daemon starting over a location a later build's table no longer
// lists, or a token some upstream has since revoked. Neither can silence
// anything on its own — see the comment in Validate — and a device that
// refuses to start announces nothing at all.
func TestValidateAcceptsALocationAndATokenItCannotVouchFor(t *testing.T) {
	c := config.Default()
	c.Alerts.LocationUID = 999999 // in no published table, now or ever
	c.Alerts.AlertsUAToken = "a-token-nothing-will-accept"

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused a repairable alerts config, which would stop the daemon: %v", err)
	}
}

// Both new fields survive a Clone. The token is a string, so the struct copy
// already carries it; this is here so that a future change to a []byte (or to
// a list of tokens) is caught rather than shipping a clone that shares — or
// silently drops — a credential.
func TestCloneCarriesTheLocationAndTheToken(t *testing.T) {
	original := config.Default()
	original.Alerts.LocationUID = 1198
	original.Alerts.AlertsUAToken = "clone-me"

	clone := original.Clone()

	if clone.Alerts.LocationUID != 1198 {
		t.Errorf("clone LocationUID = %d, want 1198", clone.Alerts.LocationUID)
	}
	if clone.Alerts.AlertsUAToken != "clone-me" {
		t.Errorf("clone AlertsUAToken = %q, want it carried over", clone.Alerts.AlertsUAToken)
	}
}

// The token is a credential and Redacted is the only sanctioned way to hand a
// config to anything that renders or writes it. The rest of Alerts is not a
// credential and must survive: a redacted config still has to be able to show
// which oblast and which location this device watches.
func TestRedactedRemovesTheAlertsUATokenAndKeepsTheRestOfAlerts(t *testing.T) {
	c := config.Default()
	c.Alerts.LocationUID = 1198
	c.Alerts.AlertsUAToken = "token-that-must-not-escape"

	got := c.Redacted()

	if got.Alerts.AlertsUAToken != "" {
		t.Errorf("AlertsUAToken = %q, want it stripped", got.Alerts.AlertsUAToken)
	}
	if got.Alerts.LocationUID != 1198 || got.Alerts.Region != c.Alerts.Region {
		t.Errorf("Alerts = %+v, want everything but the token preserved", got.Alerts)
	}
	if c.Alerts.AlertsUAToken == "" {
		t.Error("Redacted emptied the caller's own token; the daemon still has to fetch with it")
	}
}

// secretish matches a field name that reads like a credential. Deliberately
// broad: the point is to catch a field nobody thought about, so a false
// positive (answered by keptOnPurpose below, with a reason) is the cheap
// direction and a miss is the expensive one.
var secretish = regexp.MustCompile(`(?i)token|secret|password|passphrase|hash|credential`)

// This is what replaces, for every block that cannot be zeroed whole, the
// property Redacted gets for free on Auth: add a credential to Config and
// forget to strip it, and a named test fails rather than a page leaking it.
//
// It fills every string in a Config with a marker first, so it cannot pass
// vacuously against a config that happened to leave the field empty — the
// trap that made a green suite hide a total failure once already.
func TestRedactedRemovesEveryFieldThatLooksLikeACredential(t *testing.T) {
	keptOnPurpose := map[string]bool{
		// Shown on the network page on purpose: an operator who lost the
		// installer's output has nowhere else to read it, and it protects
		// a setup network rather than this device's administration. See
		// Redacted.
		"Network.APPassword": true,
	}

	c := config.Default()
	// A webhook, before the walk fills anything. Default() ships an empty
	// slice, so without this the walk steps over Webhooks entirely and every
	// field inside it — including the header value, which is where an
	// Authorization credential lives — is never examined at all. The test
	// passed for exactly that reason while Redacted() left header values
	// untouched.
	c.Webhooks = []config.Webhook{{
		Headers: []config.WebhookHeader{{}, {}},
	}}
	fillStrings(reflect.ValueOf(c).Elem())
	got := c.Redacted()

	// A header value is a credential whatever it is called, and it is called
	// "Value". Name-shaped detection cannot reach it, so it is named here —
	// the one place where what operators put in a field outranks what the
	// field is called.
	alsoSecret := regexp.MustCompile(`^Webhooks\[\d+\]\.Headers\[\d+\]\.`)

	var found int
	walkStrings(t, "", reflect.ValueOf(got).Elem(), func(path, value string) {
		if !secretish.MatchString(path[strings.LastIndexByte(path, '.')+1:]) && !alsoSecret.MatchString(path) {
			return
		}
		found++
		if keptOnPurpose[path] {
			return
		}
		if value != "" {
			t.Errorf("Redacted() leaves %s = %q; it reads like a credential and nothing strips it", path, value)
		}
	})
	if found == 0 {
		t.Fatal("this walk found no secret-shaped field at all, so it proves nothing")
	}
}

// fillStrings writes a marker into every string in v, so that a field
// Redacted forgets is non-empty and therefore visible.
func fillStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("filled-by-the-redaction-walk")
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillStrings(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			fillStrings(v.Index(i))
		}
	}
}

// walkStrings calls fn for every string in v, with a dotted path naming it.
func walkStrings(t *testing.T, path string, v reflect.Value, fn func(path, value string)) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		fn(path, v.String())
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if path != "" {
				name = path + "." + name
			}
			walkStrings(t, name, v.Field(i), fn)
		}
	case reflect.Slice:
		for i := range v.Len() {
			walkStrings(t, fmt.Sprintf("%s[%d]", path, i), v.Index(i), fn)
		}
	}
}

// An escalation event and an escalation webhook have to survive a round trip
// through the daemon's own loader. A stored config this build writes but
// refuses to read would not be a validation bug; it would be a device that
// will not start, which is the failure this whole file is written to avoid.
func TestTheEscalationTriggerIsAcceptedWhereverATriggerIs(t *testing.T) {
	c := config.Default()
	c.Events = append(c.Events, config.Event{
		ID: "alert-escalated", Trigger: config.TriggerAlertEscalated,
		Sound: "siren.wav", Volume: 100, Repeat: 1, Enabled: true,
	})
	c.Webhooks = append(c.Webhooks, config.Webhook{
		ID: "relay-red", Trigger: config.TriggerAlertEscalated,
		Method: "GET", URL: "http://192.168.1.50/Sendkey?key=9", Enabled: true,
	})

	if err := c.Validate(); err != nil {
		t.Fatalf("a config carrying an escalation would stop the daemon starting: %v", err)
	}
	if err := config.ValidateWebhook(c.Webhooks[len(c.Webhooks)-1]); err != nil {
		t.Fatalf("/webhooks would refuse the escalation an operator typed: %v", err)
	}
	// Quiet hours reason in categories, not triggers: an escalation is an
	// alert, so a window covering alerts covers it and one covering only
	// scheduled announcements leaves it alone.
	if got := c.Events[len(c.Events)-1].Category(); got != config.CategoryAlerts {
		t.Fatalf("Category() = %q, want %q", got, config.CategoryAlerts)
	}
}
