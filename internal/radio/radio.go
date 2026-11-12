// Package radio decides what internet stream, if any, the device plays while
// it has nothing to announce. It plays nothing itself: the stream is handed
// to the player as its background, below every sound that means something —
// see player.SetBackground.
package radio

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/rules"
	"github.com/b-ovsepian/spoloh/internal/scheduler"
)

// Custom is the Station value meaning "the operator's own URL".
const Custom = "custom"

// Station is one entry in the list /radio offers.
type Station struct{ ID, Name, URL string }

// Stations are Ukrainian stations whose streams were checked by playing them
// with ffmpeg from a Raspberry Pi on a Ukrainian home connection. A stream
// URL is the broadcaster's to move, so an entry here can go stale with no
// build changing; that is why the operator can always type their own.
// Several that looked obvious did not answer at all — Classic Radio, Jam FM,
// DJ FM, Radio NV, Країна FM — and are left out rather than listed broken.
var Stations = []Station{
	{ID: "hitfm", Name: "Hit FM", URL: "https://online.hitfm.ua/HitFM"},
	{ID: "kissfm", Name: "Kiss FM", URL: "https://online.kissfm.ua/KissFM"},
	{ID: "roks", Name: "Radio ROKS", URL: "https://online.radioroks.ua/RadioROKS"},
	{ID: "roks-ukr", Name: "Radio ROKS Українською", URL: "https://online.radioroks.ua/RadioROKS_Ukr"},
	{ID: "relax", Name: "Radio Relax", URL: "https://online.radiorelax.ua/RadioRelax"},
	{ID: "melodia", Name: "Melodia FM", URL: "https://online.melodiafm.ua/MelodiaFM"},
	{ID: "bayraktar", Name: "Radio Bayraktar", URL: "https://online.radiobayraktar.ua/RadioBayraktar"},
	{ID: "nashe", Name: "Наше Радіо", URL: "https://online.nasheradio.ua/NasheRadio"},
	{ID: "jazz", Name: "Radio Jazz", URL: "https://online.radiojazz.ua/RadioJazz"},
	{ID: "luxfm", Name: "Lux FM", URL: "https://icecastlv.luxnet.ua/lux"},
	{ID: "ur1", Name: "Українське радіо", URL: "https://radio.ukr.radio/ur1-mp3"},
	{ID: "promin", Name: "Радіо Промінь", URL: "https://radio.ukr.radio/ur2-mp3"},
	{ID: "kultura", Name: "Радіо Культура", URL: "https://radio.ukr.radio/ur3-mp3"},
}

// Lookup finds a catalogue station by ID.
func Lookup(id string) (Station, bool) {
	for _, s := range Stations {
		if s.ID == id {
			return s, true
		}
	}
	return Station{}, false
}

// ValidateURL accepts an http or https URL with a host, and nothing else.
// ffmpeg reads a great many schemes, file: among them, and an operator's URL
// is handed to it: anything wider than this lets a form field read files off
// the board. The player holds ffmpeg to the same two protocols as well; this
// is the half that tells the operator why.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q: only http and https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("no host")
	}
	return nil
}

// Field names what a FieldError is about, so /radio can say it in Ukrainian.
const (
	FieldVolume   = "volume"
	FieldWeekdays = "weekdays"
	FieldTime     = "time"
	FieldWindow   = "window"
	FieldStation  = "station"
	FieldURL      = "url"
)

// FieldError is a Validate refusal and the field it is about.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func fieldErr(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Err: fmt.Errorf(format, args...)}
}

// Validate is what /radio refuses, as a *FieldError naming the field.
func Validate(r config.Radio) error {
	if r.Volume < 0 || r.Volume > 100 {
		return fieldErr(FieldVolume, "%d is outside 0..100", r.Volume)
	}
	if len(r.Weekdays) == 0 {
		return fieldErr(FieldWeekdays, "none chosen")
	}
	for _, d := range r.Weekdays {
		if d < 1 || d > 7 {
			return fieldErr(FieldWeekdays, "%d is outside 1..7", d)
		}
	}
	from, to, err := window(r)
	if err != nil {
		return &FieldError{Field: FieldTime, Err: err}
	}
	if to <= from {
		return fieldErr(FieldWindow, "ends at or before it starts")
	}
	if r.Station == Custom {
		if err := ValidateURL(r.URL); err != nil {
			return &FieldError{Field: FieldURL, Err: err}
		}
		return nil
	}
	if _, ok := Lookup(r.Station); !ok {
		return fieldErr(FieldStation, "unknown station %q", r.Station)
	}
	return nil
}

// State is what Due needs to know that is not in the configuration.
type State struct {
	// Synced is whether the clock is NTP-synchronised. Without it the window
	// is a guess, and the scheduler holds scheduled events on the same
	// ground: music at 03:00 because the clock thinks it is noon is the
	// device doing something nobody asked for.
	Synced bool
	// Alert is whether a raid is running.
	Alert bool
}

// Due reports the stream that should be playing at now. False means nothing
// should be. How loud is the caller's to say — see player.Background for why
// the radio's level and the card's are kept apart.
//
// It never errors. A stored setting it cannot make sense of — an unknown
// station, a window that does not parse, a URL /radio would refuse — is a
// radio that does not play, the same reading the config package gives the
// radio as a whole.
func Due(cfg *config.Config, now time.Time, st State) (string, bool) {
	r := cfg.Radio
	if !r.Enabled || !st.Synced {
		return "", false
	}
	if st.Alert && r.PauseDuringAlert {
		return "", false
	}
	// Whatever the window covers. Its categories name announcements, and
	// someone who asked for quiet at night did not mean "quiet, except for
	// music".
	quiet := cfg.QuietHours
	if rules.InQuietHours(quiet, now) {
		return "", false
	}
	if !slices.Contains(r.Weekdays, scheduler.ISOWeekday(now)) {
		return "", false
	}
	from, to, err := window(r)
	if err != nil {
		return "", false
	}
	cur := now.Hour()*60 + now.Minute()
	if cur < from || cur >= to {
		return "", false
	}
	return streamURL(r)
}

// Redact is a stream URL fit for a log line: scheme, host and path, without
// the user, password or query a stream of the operator's own may carry.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

func window(r config.Radio) (from, to int, err error) {
	if from, err = config.ParseHM(r.From); err != nil {
		return 0, 0, fmt.Errorf("from: %w", err)
	}
	if to, err = config.ParseHM(r.To); err != nil {
		return 0, 0, fmt.Errorf("to: %w", err)
	}
	return from, to, nil
}

func streamURL(r config.Radio) (string, bool) {
	if r.Station == Custom {
		return r.URL, ValidateURL(r.URL) == nil
	}
	s, ok := Lookup(r.Station)
	return s.URL, ok
}
