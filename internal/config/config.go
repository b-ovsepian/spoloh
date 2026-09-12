// Package config defines Spoloh's on-disk settings and their validation rules.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Schema is the config format version this build understands.
const Schema = 1

// Event triggers.
const (
	TriggerAlertStart = "alert_start"
	TriggerAlertEnd   = "alert_end"
	TriggerScheduled  = "scheduled"
)

// Quiet-hour categories. Quiet hours default to suppressing CategoryScheduled
// only: an announcer that goes silent at night is silent when it matters most.
// CategoryAlerts is a choice the operator has to make deliberately — see
// QuietHours.AppliesTo and internal/web's /events form.
const (
	CategoryAlerts    = "alerts"
	CategoryScheduled = "scheduled"
)

// MinPollInterval keeps us clear of the upstream rate limit, which returns 429
// for two requests roughly a second apart.
//
// It was 3s until a device polling every 5 seconds had every source in its
// cascade answer 429 at once. A device throttled by all of its sources is one
// that may not learn about an air raid, which is the only thing it exists to
// do: polling faster made it less reliable, not more. Ten seconds is the floor
// the operator asked for.
//
// Raising this floor must not strand a device already in the field, every one
// of which stores a value this now refuses. A stored interval below the
// minimum is repaired on load, not rejected — see raisePollIntervalToMinimum
// in store.go. Validate goes on rejecting it, because Validate is also how a
// form submission is checked, and an operator typing two seconds should be
// told no rather than silently corrected.
const MinPollInterval = 10 * time.Second

// Duration serialises as a Go duration string such as "5s".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Schema     int        `json:"schema"`
	Device     Device     `json:"device"`
	Alerts     Alerts     `json:"alerts"`
	Audio      Audio      `json:"audio"`
	QuietHours QuietHours `json:"quiet_hours"`
	Events     []Event    `json:"events"`
	Auth       Auth       `json:"auth"`
	Network    Network    `json:"network"`
	// Webhooks are HTTP requests this device makes when it announces
	// something, so the operator's own hardware — a relay board, a PA
	// controller, a light — acts at the same moment the speaker does. See
	// Webhook and ValidateWebhook.
	Webhooks []Webhook `json:"webhooks,omitempty"`
}

type Device struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
	// NTPServer is the time server the device should synchronise against,
	// as a hostname or an IP address. Empty means "whatever the
	// distribution configures", which is the state every device ships in
	// and a perfectly good answer on a site with internet access; it is a
	// site with its own local time server, or none reachable, that needs
	// this.
	//
	// timedatectl cannot set it. It ends up in
	// /etc/systemd/timesyncd.conf.d/, written by a root oneshot unit — see
	// internal/timedate.Timedatectl.ApplyNTPServer — which is why
	// ValidateNTPServer below is strict rather than advisory: this value
	// crosses a privilege boundary into a file root parses.
	NTPServer string `json:"ntp_server,omitempty"`
}

type Alerts struct {
	Region       string   `json:"region"`
	Sources      []string `json:"sources"`
	PollInterval Duration `json:"poll_interval"`

	// LocationUID is the published alerts.in.ua location this device
	// watches, finer than an oblast. Zero — what every device ships with,
	// and what every config written before this field existed carries —
	// means the whole oblast named by Region, which is exactly what the
	// device did when there was nowhere to store a location. No stored file
	// needs migrating and no device changes what it announces because it
	// was upgraded.
	//
	// Only alerts.in.ua understands anything below oblast level. The six
	// Ubilling keys answer with a boolean per oblast and nothing else, so a
	// cascade that falls back to one of them broadens to the oblast holding
	// this location rather than going quiet — see alertsource.OblastOf.
	// That announces more than was asked for, never less, and /alerts says
	// it per source before it can happen.
	//
	// Checked against alertsource.KnownLocation where it is chosen
	// (internal/web/alerts.go), deliberately not in Validate. Validate is
	// also what every load runs through, and a daemon that refuses to start
	// because a UID left the published table is a device that announces
	// nothing at all. An unrecognised UID falls back to the oblast instead.
	LocationUID int `json:"location_uid,omitempty"`

	// AlertsUAToken is the operator's own alerts.in.ua API token, without
	// which that source cannot be used at all — it is the credential
	// alertsource.SelectableSources gates the source on.
	//
	// It is a secret, and the three rules that follow from that are each
	// enforced somewhere that fails loudly rather than left to care:
	// Redacted() strips it (config_test.go walks Config by reflection for
	// anything secret-shaped it forgets), no page renders it
	// (internal/web/leak_test.go walks every route), and no error or log
	// line carries it (internal/alertsource keeps it out of every wrapped
	// error; see AlertsUA.Fetch and alertsource.Reason).
	AlertsUAToken string `json:"alerts_ua_token,omitempty"`
}

type Audio struct {
	Device       string `json:"device"`
	MasterVolume int    `json:"master_volume"`
}

type QuietHours struct {
	Enabled bool   `json:"enabled"`
	From    string `json:"from"`
	To      string `json:"to"`

	// AppliesTo lists the categories the window actually silences, and it
	// is the whole of the rule: rules.ForEvent suppresses an event when the
	// clock is inside the window and the event's category is in here, with
	// nothing else behind it.
	//
	// It used to have something else behind it. CategoryAlerts was accepted
	// into this list and then ignored, so that no configuration could keep
	// an air raid alert silent. The operator asked for that gate to come
	// off after running the device on a wall, knowing what it means, and
	// this list is now taken at its word. What replaces the old guarantee
	// is not another guarantee but three obligations, each of them real
	// code: the dashboard refuses to add CategoryAlerts here without a
	// typed confirmation, the state is stated plainly on the screens while
	// it is on, and every alert it silences leaves an
	// eventlog.KindAlertSuppressed row behind.
	//
	// Validate does not check this field at all, and must not start. Every
	// value a stored config.json can hold has to keep loading: a device
	// that refuses to start over this field trades a suppressed alert for a
	// dead announcer, which is the worse of the two.
	AppliesTo []string `json:"applies_to"`
}

// Covers reports whether category is one this window silences. An
// unrecognised category matches nothing, which is why Validate can afford to
// accept whatever a hand-edited file put here: a value this build does not
// know cannot silence anything.
func (q QuietHours) Covers(category string) bool {
	return slices.Contains(q.AppliesTo, category)
}

// SilencesAlerts reports whether this device is configured to keep an air
// raid alert silent inside its quiet-hours window. It is the condition the
// dashboard's standing warning and the /events form's warning both test, so
// the two can never disagree about whether the state is on.
//
// Unlike Covers it folds in Enabled, because that is the question an
// operator is really asking: a category list naming alerts on a window that
// is switched off silences nothing.
func (q QuietHours) SilencesAlerts() bool {
	return q.Enabled && q.Covers(CategoryAlerts)
}

// Event covers both alert announcements and scheduled announcements. Time and
// Weekdays are meaningful only when Trigger is TriggerScheduled.
type Event struct {
	ID       string `json:"id"`
	Trigger  string `json:"trigger"`
	Name     string `json:"name,omitempty"`
	Time     string `json:"time,omitempty"`
	Weekdays []int  `json:"weekdays,omitempty"`
	Sound    string `json:"sound"`
	Volume   int    `json:"volume"`
	// Repeat is how many times the sound plays, back to back, for this one
	// trigger — an operator's answer to a siren that does not carry across a
	// corridor on a single pass. It defaults to 1 and is capped at 10 by
	// validate. The zero value is not a legal repeat count: a config.json
	// written before this field existed has no "repeat" key, and
	// json.Unmarshal would otherwise leave this at 0, silently turning
	// "play once" into "play never" on a device where a missing setting must
	// never mean silence. store.go's Load repairs that case the same way it
	// already repairs a missing alert event, before validate ever sees it.
	Repeat  int  `json:"repeat"`
	Enabled bool `json:"enabled"`
}

// Category maps a trigger onto the quiet-hours vocabulary.
func (e Event) Category() string {
	if e.Trigger == TriggerScheduled {
		return CategoryScheduled
	}
	return CategoryAlerts
}

type Auth struct {
	PasswordHash string `json:"password_hash"`

	// SessionSecret signs the dashboard's session cookies. It is generated
	// once, when the administrator password is first set, and lives here
	// rather than in memory so that sessions survive a restart of the
	// daemon — systemd restarts this unit routinely (Restart=always,
	// WatchdogSec, self-update), and an operator logged out by every one of
	// them would leave a wall-mounted dashboard sitting on a login page.
	//
	// It is a credential, not a setting: no editing screen may render it,
	// and a device rebuilt from an exported configuration inherits the
	// sessions of the device it was exported from.
	SessionSecret string `json:"session_secret,omitempty"`
}

type Network struct {
	APSSID          string   `json:"ap_ssid"`
	APPassword      string   `json:"ap_password"`
	APFallbackAfter Duration `json:"ap_fallback_after"`
}

// Webhook is one HTTP request the device makes when Trigger fires. The
// operator who asked for this drives a relay board on the local network with
// a request their own vendor documented, credentials and all:
//
//	curl -H 'Authorization: Basic ...' 'http://192.168.X.X/Sendkey?key=8'
//
// so every part of it — the method, the URL, the headers, the body — is
// theirs to write, and none of it is guessed from a template.
//
// Headers are an ordered slice rather than a map for two reasons: a map
// renders in a different order on every load, which makes an edit form
// shuffle itself under the operator's hands, and http.Header's canonical
// casing would quietly rewrite a name some appliance firmware compares
// literally.
//
// A header value is a credential — the example above is a password in
// base64. It is shown on this webhook's own edit form and nowhere else: not
// in a log line, not in an error, not in the event log. See
// internal/webhook for the firing side of that rule and
// internal/web/webhooks_test.go for the walk that enforces it.
type Webhook struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Trigger is one of TriggerAlertStart, TriggerAlertEnd or
	// TriggerScheduled — the same vocabulary Event uses, so "when this
	// fires" means the same thing on both screens.
	Trigger string          `json:"trigger"`
	Method  string          `json:"method"`
	URL     string          `json:"url"`
	Headers []WebhookHeader `json:"headers,omitempty"`
	Body    string          `json:"body,omitempty"`
	Enabled bool            `json:"enabled"`
}

// WebhookHeader is one request header, exactly as the operator typed it.
type WebhookHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// The bounds a webhook is held to. They are typo guards, not policy: this
// configuration lives on an SD card and is typed on a phone, and a pasted
// page of text in a URL field should be refused where it is typed rather
// than discovered later by a device that cannot make the request.
const (
	MaxWebhooks        = 20
	MaxWebhookHeaders  = 20
	MaxWebhookURLLen   = 2000
	MaxWebhookBodyLen  = 4096
	MaxWebhookNameLen  = 100
	MaxHeaderNameLen   = 100
	MaxHeaderValueLen  = 1000
	MaxWebhookIDLength = 64
)

// WebhookMethods is what a webhook may be. The list is an allow-list of the
// standard methods rather than a check for dangerous ones: this string is
// written verbatim into a request line, and a method nobody thought about —
// one containing a space, say — is a second request line.
var WebhookMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}

// WebhookSchemes is every scheme a webhook may name.
//
// Only these two, and not because of what is on the other end: this device
// sits on a private network and 192.168.x.x is the intended target, so there
// is deliberately no address blocklist here — one would forbid precisely the
// relay board this feature exists for. What is refused is what is not an
// HTTP request at all. "file:" would have the daemon read its own disk and
// call it a webhook; "gopher:" is the classic way to turn a fetcher into a
// writer of arbitrary bytes on an arbitrary port. Neither is something an
// operator asked this device to do.
var WebhookSchemes = []string{"http", "https"}

// Webhook field names, as WebhookError.Field reports them. They exist so a
// dashboard can say which field is wrong in the operator's language without
// re-implementing any of the checks below.
const (
	WebhookFieldID      = "id"
	WebhookFieldName    = "name"
	WebhookFieldTrigger = "trigger"
	WebhookFieldMethod  = "method"
	WebhookFieldURL     = "url"
	WebhookFieldHeaders = "headers"
	WebhookFieldBody    = "body"
	WebhookFieldCount   = "count"
)

// WebhookError names the part of a webhook that was refused. The message is
// English like every other error in this package; Field is what lets
// internal/web answer in Ukrainian, and internal/webhook log which part of a
// hand-edited configuration it will not fire.
type WebhookError struct {
	Field string
	Err   error
}

func (e *WebhookError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *WebhookError) Unwrap() error { return e.Err }

func webhookErr(field, format string, args ...any) *WebhookError {
	return &WebhookError{Field: field, Err: fmt.Errorf(format, args...)}
}

// ValidateWebhook reports whether w is one this device can actually make.
//
// It is exported, and Config.Validate deliberately does not call it — see
// the comment on Webhooks in Validate. The two callers are the /webhooks
// form, which refuses a submission and tells the operator why, and
// internal/webhook, which refuses to fire a stored webhook that would not
// pass and writes that refusal to the event log. Between them, a webhook
// this build cannot make is always either rejected as it is typed or
// reported as it fails to fire — never silently dropped, and never a reason
// the daemon will not start.
func ValidateWebhook(w Webhook) error {
	if w.ID == "" {
		return webhookErr(WebhookFieldID, "id is required")
	}
	if len(w.ID) > MaxWebhookIDLength {
		return webhookErr(WebhookFieldID, "id is %d characters, the maximum is %d", len(w.ID), MaxWebhookIDLength)
	}
	if len(w.Name) > MaxWebhookNameLen {
		return webhookErr(WebhookFieldName, "name is %d characters, the maximum is %d", len(w.Name), MaxWebhookNameLen)
	}
	switch w.Trigger {
	case TriggerAlertStart, TriggerAlertEnd, TriggerScheduled:
	default:
		return webhookErr(WebhookFieldTrigger, "unknown trigger %q", w.Trigger)
	}
	if !slices.Contains(WebhookMethods, w.Method) {
		return webhookErr(WebhookFieldMethod, "method %q is not one of %v", w.Method, WebhookMethods)
	}
	if err := validateWebhookURL(w.URL); err != nil {
		return err
	}
	if len(w.Headers) > MaxWebhookHeaders {
		return webhookErr(WebhookFieldHeaders, "%d headers, the maximum is %d", len(w.Headers), MaxWebhookHeaders)
	}
	for _, h := range w.Headers {
		if err := validateHeader(h); err != nil {
			return err
		}
	}
	if len(w.Body) > MaxWebhookBodyLen {
		return webhookErr(WebhookFieldBody, "body is %d bytes, the maximum is %d", len(w.Body), MaxWebhookBodyLen)
	}
	return nil
}

func validateWebhookURL(raw string) error {
	if raw == "" {
		return webhookErr(WebhookFieldURL, "url is required")
	}
	if len(raw) > MaxWebhookURLLen {
		return webhookErr(WebhookFieldURL, "url is %d characters, the maximum is %d", len(raw), MaxWebhookURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return webhookErr(WebhookFieldURL, "url is not an address")
	}
	if !slices.Contains(WebhookSchemes, u.Scheme) {
		return webhookErr(WebhookFieldURL, "scheme %q is not one of %v", u.Scheme, WebhookSchemes)
	}
	if u.Host == "" {
		return webhookErr(WebhookFieldURL, "url names no host")
	}
	return nil
}

// validateHeader refuses a name that is not an HTTP token and a value
// carrying a control character.
//
// The value check is the one that matters: a CR or an LF in a value ends the
// header and starts whatever the operator typed next, which is header
// injection into a request this device signs with its own credentials. Go's
// own net/http would refuse to send it, but by then the refusal is an error
// on a device with no screen rather than a sentence under the field that is
// wrong.
func validateHeader(h WebhookHeader) error {
	if h.Name == "" {
		return webhookErr(WebhookFieldHeaders, "a header with no name")
	}
	if len(h.Name) > MaxHeaderNameLen {
		return webhookErr(WebhookFieldHeaders, "a header name is %d characters, the maximum is %d", len(h.Name), MaxHeaderNameLen)
	}
	if len(h.Value) > MaxHeaderValueLen {
		// The value itself is never quoted into an error: it is a
		// credential, and an error string travels into logs and onto pages.
		// The name is not, and is what the operator needs to find the field.
		return webhookErr(WebhookFieldHeaders, "the value of header %q is %d characters, the maximum is %d",
			h.Name, len(h.Value), MaxHeaderValueLen)
	}
	for i := 0; i < len(h.Name); i++ {
		if !isTokenByte(h.Name[i]) {
			return webhookErr(WebhookFieldHeaders, "header name %q contains %q", h.Name, string(h.Name[i]))
		}
	}
	for i := 0; i < len(h.Value); i++ {
		if c := h.Value[i]; c < 0x20 || c == 0x7f {
			return webhookErr(WebhookFieldHeaders, "the value of header %q contains a control character", h.Name)
		}
	}
	return nil
}

// isTokenByte reports whether c may appear in an HTTP field name (RFC 9110's
// "token"). Written out rather than pulled from a dependency: the module
// graph does not grow, and the set is fifteen punctuation marks long.
func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// Default returns a config that is valid and immediately useful: alerts for
// Kyiv city and a minute of silence at 09:00 every day.
func Default() *Config {
	return &Config{
		Schema: Schema,
		Device: Device{Name: "spoloh", Timezone: "Europe/Kyiv"},
		Alerts: Alerts{
			Region:  "м. Київ",
			Sources: []string{"default", "skog", "aiu"},
			// 15s rather than the 10s floor: the 429 that forced the floor
			// up asked for 15 seconds by name in its Retry-After, so take
			// the upstream at its word rather than sitting exactly on the
			// fastest rate it tolerates.
			PollInterval: Duration(15 * time.Second),
		},
		Audio: Audio{Device: "default", MasterVolume: 80},
		QuietHours: QuietHours{
			Enabled:   false,
			From:      "22:00",
			To:        "07:00",
			AppliesTo: []string{CategoryScheduled},
		},
		Events: []Event{
			{ID: "alert-start", Trigger: TriggerAlertStart, Sound: "siren.wav", Volume: 100, Repeat: 1, Enabled: true},
			{ID: "alert-end", Trigger: TriggerAlertEnd, Sound: "all-clear.wav", Volume: 90, Repeat: 1, Enabled: true},
			{
				ID: "silence", Trigger: TriggerScheduled, Name: "Хвилина мовчання",
				Time: "09:00", Weekdays: []int{1, 2, 3, 4, 5, 6, 7},
				Sound: "metronome.wav", Volume: 85, Repeat: 1, Enabled: true,
			},
		},
		Network: Network{
			APSSID:          "Spoloh-Setup",
			APFallbackAfter: Duration(45 * time.Second),
		},
	}
}

// maxNTPServerLen is the longest a DNS name may be. An IPv6 literal is far
// shorter, so one bound covers both.
const maxNTPServerLen = 253

// ValidateNTPServer accepts an empty value, a hostname or an IP address, and
// rejects everything else.
//
// This is an allow-list of bytes rather than a search for dangerous ones,
// and that is the whole point. The value is written to a file that a root
// unit reads and turns into an NTP= line in
// /etc/systemd/timesyncd.conf.d/spoloh.conf: a newline in it appends a
// second directive of the operator's choosing to a root-owned config file,
// and a quote or an "=" lets the line be re-read as something other than the
// key it was meant to be. Listing what may appear — letters, digits, dots,
// hyphens and the colons of an IPv6 literal — means a character nobody
// thought about is refused by default instead of admitted by default.
//
// One server, not a list: systemd-timesyncd's NTP= accepts several
// space-separated, but a space is exactly the character that would let one
// field become two, and a single server is what the dashboard asks for. A
// site that needs a pool can still point this at a hostname that resolves to
// several addresses.
//
// The leading and trailing checks are ordinary hostname hygiene rather than
// safety: ".example.com" and "-example.com" are not names anything will
// resolve, and rejecting them here beats a device that looks configured and
// never synchronises.
func ValidateNTPServer(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > maxNTPServerLen {
		return fmt.Errorf("%q is %d characters, the maximum is %d", s, len(s), maxNTPServerLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == ':':
		default:
			return fmt.Errorf("%q contains %q; a time server is a hostname or an IP address", s, string(c))
		}
	}
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "-") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, "-") {
		return fmt.Errorf("%q is not a hostname or an IP address", s)
	}
	return nil
}

// ParseHM parses an "HH:MM" wall-clock string into minutes since midnight.
func ParseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (c *Config) Validate() error {
	if c.Schema != Schema {
		return fmt.Errorf("config schema %d, this build understands %d", c.Schema, Schema)
	}
	if c.Device.Timezone == "" {
		return fmt.Errorf("device.timezone is required")
	}
	if _, err := time.LoadLocation(c.Device.Timezone); err != nil {
		return fmt.Errorf("device.timezone %q: %w", c.Device.Timezone, err)
	}
	if err := ValidateNTPServer(c.Device.NTPServer); err != nil {
		return fmt.Errorf("device.ntp_server: %w", err)
	}
	if c.Alerts.Region == "" {
		return fmt.Errorf("alerts.region is required")
	}
	if len(c.Alerts.Sources) == 0 {
		return fmt.Errorf("alerts.sources must list at least one source")
	}
	if c.Alerts.PollInterval.Std() < MinPollInterval {
		return fmt.Errorf("alerts.poll_interval %v is below the %v minimum", c.Alerts.PollInterval.Std(), MinPollInterval)
	}
	// alerts.location_uid and alerts.alerts_ua_token are deliberately not
	// checked here, for any value, and that must not start — the same ruling
	// quiet_hours.applies_to and webhooks get below, reached the same way.
	// Both are operator-choosable, so both are checked where they are
	// chosen: /alerts refuses a UID that is not in the published table and a
	// token that could not be an Authorization header, each with the field
	// named in Ukrainian. Checking them here as well would mean a UID
	// dropped from a later build's location table, or a token from a device
	// restored out of a backup, stops the daemon starting — and a device
	// that will not start announces nothing at all. Neither value can
	// silence anything on its own: an unrecognised UID broadens to the
	// oblast, and a token nothing accepts leaves a cascade slot that fails
	// and is logged as failing.
	if c.Audio.MasterVolume < 0 || c.Audio.MasterVolume > 100 {
		return fmt.Errorf("audio.master_volume %d is outside 0..100", c.Audio.MasterVolume)
	}
	if c.QuietHours.Enabled {
		if _, err := ParseHM(c.QuietHours.From); err != nil {
			return fmt.Errorf("quiet_hours.from: %w", err)
		}
		if _, err := ParseHM(c.QuietHours.To); err != nil {
			return fmt.Errorf("quiet_hours.to: %w", err)
		}
	}
	// quiet_hours.applies_to is deliberately not checked, for any value,
	// including one this build does not recognise — see the field's comment.
	// A category nobody knows silences nothing, and refusing to start over a
	// list is how a device ends up not announcing anything at all.
	seen := make(map[string]bool, len(c.Events))
	for _, e := range c.Events {
		if err := e.validate(); err != nil {
			return err
		}
		if seen[e.ID] {
			return fmt.Errorf("duplicate event id %q", e.ID)
		}
		seen[e.ID] = true
	}
	// webhooks are deliberately not checked here, for any value, and this
	// must not start — the same ruling quiet_hours.applies_to has above,
	// reached the same way. A webhook is the operator's own hardware, and
	// the device announcing air raid alerts must never refuse to start
	// because a hand-edited request to a relay board names a scheme this
	// build does not make. What replaces the check is not a weaker check
	// but two loud ones: /webhooks refuses the submission that would
	// create such a webhook (ValidateWebhook, with the field named in
	// Ukrainian), and internal/webhook refuses to fire one that is already
	// stored and writes that refusal to the event log, where an operator
	// looking for "why did my relay not click" will find it.

	// Empty is the "not generated yet" state: config.Default() ships it
	// empty, and netmgr.EnsureAPPassword fills it in on first run. Only a
	// non-empty value gets checked against WPA2's floor, so a hand-edited
	// config cannot produce a hotspot that refuses every connection.
	if pw := c.Network.APPassword; pw != "" && len(pw) < 8 {
		return fmt.Errorf("network.ap_password is %d characters, WPA2 requires at least 8", len(pw))
	}
	return nil
}

func (e Event) validate() error {
	if e.ID == "" {
		return fmt.Errorf("event id is required")
	}
	switch e.Trigger {
	case TriggerAlertStart, TriggerAlertEnd:
	case TriggerScheduled:
		if _, err := ParseHM(e.Time); err != nil {
			return fmt.Errorf("event %q: %w", e.ID, err)
		}
		if len(e.Weekdays) == 0 {
			return fmt.Errorf("event %q: weekdays must not be empty", e.ID)
		}
		for _, d := range e.Weekdays {
			if d < 1 || d > 7 {
				return fmt.Errorf("event %q: weekday %d is outside 1..7", e.ID, d)
			}
		}
	default:
		return fmt.Errorf("event %q: unknown trigger %q", e.ID, e.Trigger)
	}
	if e.Sound == "" {
		return fmt.Errorf("event %q: sound is required", e.ID)
	}
	if e.Volume < 0 || e.Volume > 100 {
		return fmt.Errorf("event %q: volume %d is outside 0..100", e.ID, e.Volume)
	}
	// 1..10, not 0..10: unlike volume, repeat has no zero state to protect —
	// see Event.Repeat's doc comment for why 0 must never reach here. The
	// upper bound is the typo guard the brief asks for: a mistyped repeat
	// count must not occupy the single playback queue for an hour.
	if e.Repeat < 1 || e.Repeat > 10 {
		return fmt.Errorf("event %q: repeat %d is outside 1..10", e.ID, e.Repeat)
	}
	return nil
}
