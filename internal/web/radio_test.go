package web_test

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/radio"
	"github.com/b-ovsepian/spoloh/internal/web"
)

func radioServer(t *testing.T) (http.Handler, *http.Cookie, func() config.Radio) {
	t.Helper()
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")
	return srv.Handler(), cookie, func() config.Radio { return store.Get().Radio }
}

func getRadio(t *testing.T, h http.Handler, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/radio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /radio: status %d; body:\n%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func postRadio(h http.Handler, cookie *http.Cookie, form url.Values, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/radio", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validRadioForm() url.Values {
	return url.Values{
		"enabled":  {"on"},
		"station":  {"promin"},
		"volume":   {"35"},
		"weekdays": {"1", "3", "5"},
		"from":     {"09:00"},
		"to":       {"17:30"},
	}
}

// Every station in the catalogue is offered, by name, as a plain <select>:
// the page has to work with scripts off, like every control on this device.
func TestTheRadioPageOffersEveryStation(t *testing.T) {
	h, cookie, _ := radioServer(t)
	body := getRadio(t, h, cookie)
	for _, s := range radio.Stations {
		// As html/template writes it: "Hard'n'Heavy" reaches the page as
		// Hard&#39;n&#39;Heavy, which a browser shows as typed.
		if !strings.Contains(body, `value="`+s.ID+`"`) || !strings.Contains(body, template.HTMLEscapeString(s.Name)) {
			t.Errorf("station %q (%s) is not offered", s.ID, s.Name)
		}
	}
	// Forty-odd stations in one flat list is a list nobody finds anything
	// in: they come under their brand's heading, still a plain <select>.
	if !strings.Contains(body, `<optgroup label="Hit FM">`) || !strings.Contains(body, `<optgroup label="Суспільне">`) {
		t.Error("the stations are not grouped under their brands")
	}
	if !strings.Contains(body, `value="`+radio.Custom+`"`) {
		t.Error("there is no way to choose a stream of the operator's own")
	}
	if !strings.Contains(body, `<form method="post" action="/radio"`) {
		t.Error("the radio settings are not a plain form post")
	}
}

// What an operator is told before they switch it on: a sound or an alert
// always comes first, quiet hours silence it, and a raid does not unless they
// ask. A setting whose consequences are found out on the day is a trap.
func TestTheRadioPageSaysWhatComesFirst(t *testing.T) {
	h, cookie, _ := radioServer(t)
	body := getRadio(t, h, cookie)
	for _, want := range []string{"тихі години", "замовкає", "відбою"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
}

func TestTheRadioSettingsAreSaved(t *testing.T) {
	h, cookie, stored := radioServer(t)
	form := validRadioForm()
	form.Set("pause_during_alert", "on")
	if rec := postRadio(h, cookie, form, "http://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("status %d; body:\n%s", rec.Code, rec.Body.String())
	}
	got := stored()
	want := config.Radio{
		Enabled: true, Station: "promin", Volume: 35,
		Weekdays: []int{1, 3, 5}, From: "09:00", To: "17:30", PauseDuringAlert: true,
	}
	if got.Enabled != want.Enabled || got.Station != want.Station || got.Volume != want.Volume ||
		!slices.Equal(got.Weekdays, want.Weekdays) || got.From != want.From || got.To != want.To ||
		got.PauseDuringAlert != want.PauseDuringAlert || got.URL != "" {
		t.Fatalf("stored %+v, want %+v", got, want)
	}

	// An unticked box is absent from the form, and absent means off.
	form.Del("enabled")
	form.Del("pause_during_alert")
	if rec := postRadio(h, cookie, form, "http://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := stored(); got.Enabled || got.PauseDuringAlert {
		t.Fatalf("unticked boxes stored as %+v", got)
	}
}

func TestAStreamOfTheOperatorsOwnIsSaved(t *testing.T) {
	h, cookie, stored := radioServer(t)
	form := validRadioForm()
	form.Set("station", radio.Custom)
	form.Set("url", "https://stream.example/live")
	if rec := postRadio(h, cookie, form, "http://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("status %d; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := stored(); got.Station != radio.Custom || got.URL != "https://stream.example/live" {
		t.Fatalf("stored %+v", got)
	}
}

// Refused with the reason in Ukrainian, and nothing written.
func TestBadRadioSettingsAreRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		field, value, says string
	}{
		"window backwards": {"to", "08:00", "Час"},
		"no weekdays":      {"weekdays", "", "день"},
		"loud":             {"volume", "150", "Гучність"},
		"file url":         {"url", "file://localhost/etc/spoloh/config.json", "http"},
		"unknown station":  {"station", "nope", "станцію"},
	} {
		t.Run(name, func(t *testing.T) {
			h, cookie, stored := radioServer(t)
			before := stored()
			form := validRadioForm()
			if tc.field == "url" {
				form.Set("station", radio.Custom)
			}
			if tc.value == "" {
				form.Del(tc.field)
			} else {
				form.Set(tc.field, tc.value)
			}
			rec := postRadio(h, cookie, form, "http://example.com")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.says) {
				t.Errorf("the refusal does not say %q:\n%s", tc.says, rec.Body.String())
			}
			if got := stored(); got.Enabled != before.Enabled || got.Station != before.Station || got.Volume != before.Volume {
				t.Errorf("a refused form changed the stored radio: %+v", got)
			}
		})
	}
}

func TestTheRadioRefusesACrossOriginPost(t *testing.T) {
	h, cookie, stored := radioServer(t)
	rec := postRadio(h, cookie, validRadioForm(), "http://elsewhere.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if stored().Enabled {
		t.Fatal("a cross-origin post switched the radio on")
	}
}

// failingStreamer streams nothing and says why, the way ffmpeg does for a
// station that has moved.
type failingStreamer struct{ *fakePlaybackBackend }

func (failingStreamer) Stream(context.Context, string, string, int) error {
	return errors.New("Server returned 404 Not Found")
}

func radioServerWithPlayer(t *testing.T, b player.Backend) (http.Handler, *http.Cookie, *player.Player) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	pl := player.New(b, "default")
	srv, err := web.New(web.Deps{Config: store, Player: pl})
	if err != nil {
		t.Fatal(err)
	}
	setPassword(t, srv, "correct horse battery staple")
	return srv.Handler(), login(t, srv, "correct horse battery staple"), pl
}

// The stop button holds the radio off, and the radio page is where it is
// brought back: it says it is held, and offers to resume it.
func TestTheRadioPageResumesARadioTheStopButtonHeld(t *testing.T) {
	h, cookie, pl := radioServerWithPlayer(t, newFakePlaybackBackend())
	pl.SetBackground(player.Background{URL: "https://online.hitfm.ua/HitFM", Volume: 80, Gain: 50})
	pl.Stop()

	body := getRadio(t, h, cookie)
	if !strings.Contains(body, "призупинено") || !strings.Contains(body, `value="resume"`) {
		t.Fatalf("a held radio is not shown as held with a way to resume it:\n%s", body)
	}
	if rec := postRadio(h, cookie, url.Values{"action": {"resume"}}, "http://example.com"); rec.Code != http.StatusOK {
		t.Fatalf("resume: status %d", rec.Code)
	}
	if pl.BackgroundHeld() {
		t.Fatal("the radio is still held after resuming it from the page")
	}
}

// A station that fails is shown as failing, with ffmpeg's reason, rather
// than as playing.
func TestTheRadioPageShowsAFailingStation(t *testing.T) {
	h, cookie, pl := radioServerWithPlayer(t, failingStreamer{newFakePlaybackBackend()})
	pl.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pl.Run(ctx)
	pl.SetBackground(player.Background{URL: "https://online.hitfm.ua/HitFM", Volume: 80, Gain: 50})

	deadline := time.After(2 * time.Second)
	for {
		if err, _ := pl.BackgroundError(); err != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the stream never failed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	body := getRadio(t, h, cookie)
	if !strings.Contains(body, "не вдалося") || !strings.Contains(body, "404 Not Found") {
		t.Fatalf("a failing station is not shown as failing:\n%s", body)
	}
}
