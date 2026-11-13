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

// Station is one entry in the list /radio offers. Group is the heading it
// is listed under there.
type Station struct{ ID, Name, URL, Group string }

// Stations are Ukrainian stations whose streams were checked with ffprobe
// from a Raspberry Pi on a Ukrainian connection: every one answered with an
// mp3 stream, at 128 kbit/s except Єдині Новини at 64.
//
// The TAVR Media stations are taken from that broadcaster's own player,
// play.tavr.media, which lists every station and channel it has and plays
// them from tavr.tvstitch.com; each has an _HD twin at 320 kbit/s, left out
// because a site's connection pays for every bit of a stream that runs all
// day. Two of its channels are not on that host — Наше Радіо УкрТоп100 and
// Єдині Новини — and are listed at the address its player uses for them.
//
// Names are the brand as its broadcaster spells it — Hit FM, Radio ROKS,
// Мелодія FM — and, after it, what the channel is, in Ukrainian and lower
// case, as the operator named the Hit FM channels: one style however the
// broadcaster's own player happens to label each one.
//
// A stream URL is the broadcaster's to move, so an entry can go stale with no
// build changing; that is why the operator can always type their own.
//
// The IDs are what config.json stores, so an ID once shipped stays, whatever
// its URL or name becomes: dropped, a device that chose it finds nothing and
// its radio goes quiet. TestEveryShippedStationIDStillResolves holds that.
var Stations = []Station{
	{ID: "hitfm", Name: "Hit FM", URL: "https://tavr.tvstitch.com/HitFM", Group: "Hit FM"},
	{ID: "hitfm-best", Name: "Hit FM Найбільші хіти", URL: "https://tavr.tvstitch.com/HitFM_Best", Group: "Hit FM"},
	{ID: "hitfm-ukr", Name: "Hit FM Українські хіти", URL: "https://tavr.tvstitch.com/HitFM_Ukr", Group: "Hit FM"},
	{ID: "hitfm-top", Name: "Hit FM Сучасні хіти", URL: "https://tavr.tvstitch.com/HitFM_Top", Group: "Hit FM"},
	{ID: "kissfm", Name: "Kiss FM", URL: "https://tavr.tvstitch.com/KissFM", Group: "Kiss FM"},
	{ID: "kissfm-ukr", Name: "Kiss FM Українською", URL: "https://tavr.tvstitch.com/KissFM_Ukr", Group: "Kiss FM"},
	{ID: "kissfm-deep", Name: "Kiss FM Діп-хаус", URL: "https://tavr.tvstitch.com/KissFM_Deep", Group: "Kiss FM"},
	{ID: "kissfm-digital", Name: "Kiss FM Електроніка", URL: "https://tavr.tvstitch.com/KissFM_Digital", Group: "Kiss FM"},
	{ID: "roks", Name: "Radio ROKS", URL: "https://tavr.tvstitch.com/RadioROKS", Group: "Radio ROKS"},
	{ID: "roks-ukr", Name: "Radio ROKS Український рок", URL: "https://tavr.tvstitch.com/RadioROKS_Ukr", Group: "Radio ROKS"},
	{ID: "roks-ballads", Name: "Radio ROKS Рок-балади", URL: "https://tavr.tvstitch.com/RadioROKS_Ballads", Group: "Radio ROKS"},
	{ID: "roks-classic", Name: "Radio ROKS Класичний рок", URL: "https://tavr.tvstitch.com/RadioROKS_ClassicRock", Group: "Radio ROKS"},
	{ID: "roks-hard", Name: "Radio ROKS Важкий рок", URL: "https://tavr.tvstitch.com/RadioROKS_HardnHeavy", Group: "Radio ROKS"},
	{ID: "roks-new", Name: "Radio ROKS Новий рок", URL: "https://tavr.tvstitch.com/RadioROKS_NewRock", Group: "Radio ROKS"},
	{ID: "relax", Name: "Radio Relax", URL: "https://tavr.tvstitch.com/RadioRelax", Group: "Radio Relax"},
	{ID: "relax-ukr", Name: "Radio Relax Українською", URL: "https://tavr.tvstitch.com/RadioRelax_Ukr", Group: "Radio Relax"},
	{ID: "relax-int", Name: "Radio Relax Світова музика", URL: "https://tavr.tvstitch.com/RadioRelax_Int", Group: "Radio Relax"},
	{ID: "relax-cafe", Name: "Radio Relax Кафе", URL: "https://tavr.tvstitch.com/RadioRelax_Cafe", Group: "Radio Relax"},
	{ID: "relax-instrumental", Name: "Radio Relax Інструментальна", URL: "https://tavr.tvstitch.com/RadioRelax_Instrumental", Group: "Radio Relax"},
	{ID: "melodia", Name: "Мелодія FM", URL: "https://tavr.tvstitch.com/MelodiaFM", Group: "Мелодія FM"},
	{ID: "melodia-int", Name: "Мелодія FM Зарубіжні хіти", URL: "https://tavr.tvstitch.com/MelodiaFM_Int", Group: "Мелодія FM"},
	{ID: "melodia-romantic", Name: "Мелодія FM Романтика", URL: "https://tavr.tvstitch.com/MelodiaFM_Romantic", Group: "Мелодія FM"},
	{ID: "melodia-disco", Name: "Мелодія FM Диско", URL: "https://tavr.tvstitch.com/MelodiaFM_Disco", Group: "Мелодія FM"},
	{ID: "jazz", Name: "Radio Jazz", URL: "https://tavr.tvstitch.com/RadioJazz", Group: "Radio Jazz"},
	{ID: "jazz-ukr", Name: "Radio Jazz Українською", URL: "https://tavr.tvstitch.com/RadioJazz_Ukr", Group: "Radio Jazz"},
	{ID: "jazz-cover", Name: "Radio Jazz Кавери", URL: "https://tavr.tvstitch.com/RadioJazz_Cover", Group: "Radio Jazz"},
	{ID: "jazz-gold", Name: "Radio Jazz Золота класика", URL: "https://tavr.tvstitch.com/RadioJazz_Gold", Group: "Radio Jazz"},
	{ID: "jazz-groove", Name: "Radio Jazz Грув", URL: "https://tavr.tvstitch.com/RadioJazz_Groove", Group: "Radio Jazz"},
	{ID: "jazz-light", Name: "Radio Jazz Легкий джаз", URL: "https://tavr.tvstitch.com/RadioJazz_Light", Group: "Radio Jazz"},
	{ID: "nashe", Name: "Наше Радіо", URL: "https://tavr.tvstitch.com/NasheRadio", Group: "Наше Радіо"},
	{ID: "nashe-ukr", Name: "Наше Радіо УкрТоп100", URL: "https://online.nasheradio.ua/NasheRadio_Ukr", Group: "Наше Радіо"},
	{ID: "bayraktar", Name: "Радіо Байрактар", URL: "https://tavr.tvstitch.com/RadioBayraktar", Group: "Інші станції TAVR Media"},
	{ID: "classic", Name: "Classic Radio", URL: "https://tavr.tvstitch.com/ClassicRadio", Group: "Інші станції TAVR Media"},
	{ID: "music", Name: "Music Radio", URL: "https://tavr.tvstitch.com/MusicRadio", Group: "Інші станції TAVR Media"},
	{ID: "gold", Name: "Radio Gold", URL: "https://tavr.tvstitch.com/RadioGold", Group: "Інші станції TAVR Media"},
	{ID: "black", Name: "Radio Black", URL: "https://tavr.tvstitch.com/RadioBlack", Group: "Інші станції TAVR Media"},
	{ID: "indie", Name: "Радіо Інді.UA", URL: "https://tavr.tvstitch.com/RadioIndieUA", Group: "Інші станції TAVR Media"},
	{ID: "flash", Name: "Flash Radio", URL: "https://tavr.tvstitch.com/FlashRadio", Group: "Інші станції TAVR Media"},
	{ID: "guliay", Name: "Гуляй Радіо", URL: "https://tavr.tvstitch.com/GuliayRadio", Group: "Інші станції TAVR Media"},
	{ID: "kpop", Name: "K-Pop Radio", URL: "https://tavr.tvstitch.com/KpopRadio", Group: "Інші станції TAVR Media"},
	{ID: "italiana", Name: "Radio Italiana", URL: "https://tavr.tvstitch.com/RadioItaliana", Group: "Інші станції TAVR Media"},
	{ID: "latino", Name: "Radio Ritmo Latino", URL: "https://tavr.tvstitch.com/RadioRitmoLatino", Group: "Інші станції TAVR Media"},
	{ID: "news", Name: "Єдині Новини", URL: "https://online-news.radioplayer.ua/RadioNews", Group: "Інші станції TAVR Media"},
	{ID: "luxfm", Name: "Lux FM", URL: "https://icecastlv.luxnet.ua/lux", Group: "Lux FM"},
	{ID: "ur1", Name: "Українське радіо", URL: "https://radio.ukr.radio/ur1-mp3", Group: "Суспільне"},
	{ID: "promin", Name: "Радіо Промінь", URL: "https://radio.ukr.radio/ur2-mp3", Group: "Суспільне"},
	{ID: "kultura", Name: "Радіо Культура", URL: "https://radio.ukr.radio/ur3-mp3", Group: "Суспільне"},
}

// Group is the stations under one heading on /radio, in catalogue order.
type Group struct {
	Name     string
	Stations []Station
}

// Groups lists Stations under their headings, in the order the headings
// first appear in the catalogue — so the catalogue's order is the page's.
func Groups() []Group {
	var out []Group
	at := map[string]int{}
	for _, s := range Stations {
		i, ok := at[s.Group]
		if !ok {
			i = len(out)
			at[s.Group] = i
			out = append(out, Group{Name: s.Group})
		}
		out[i].Stations = append(out[i].Stations, s)
	}
	return out
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
