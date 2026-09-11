package web_test

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// realBoardDevices mirrors internal/player/testdata/aplay-L-rpi4.txt — the
// verbatim `aplay -L` capture taken over SSH from a real Raspberry Pi 4 —
// so this package's tests can exercise the grouped picker against the exact
// shape that board reports without reading across a package boundary into
// internal/player's own testdata. 13 devices, of which 11 are
// indistinguishable by description alone: six read "vc4-hdmi, MAI PCM
// i2s-hifi-0" and five read "bcm2835 Headphones, bcm2835 Headphones" — the
// reason /audio groups by card rather than listing them flat.
var realBoardDevices = []player.Device{
	{Name: "default", Description: "Default Audio Device"},
	{Name: "sysdefault", Description: "Default Audio Device"},
	{Name: "hw:CARD=vc4hdmi,DEV=0", Description: "vc4-hdmi, MAI PCM i2s-hifi-0"},
	{Name: "plughw:CARD=vc4hdmi,DEV=0", Description: "vc4-hdmi, MAI PCM i2s-hifi-0"},
	{Name: "default:CARD=vc4hdmi", Description: "vc4-hdmi, MAI PCM i2s-hifi-0"},
	{Name: "sysdefault:CARD=vc4hdmi", Description: "vc4-hdmi, MAI PCM i2s-hifi-0"},
	{Name: "hdmi:CARD=vc4hdmi,DEV=0", Description: "vc4-hdmi, MAI PCM i2s-hifi-0"},
	{Name: "dmix:CARD=vc4hdmi,DEV=0", Description: "vc4-hdmi, MAI PCM i2s-hifi-0"},
	{Name: "hw:CARD=Headphones,DEV=0", Description: "bcm2835 Headphones, bcm2835 Headphones"},
	{Name: "plughw:CARD=Headphones,DEV=0", Description: "bcm2835 Headphones, bcm2835 Headphones"},
	{Name: "default:CARD=Headphones", Description: "bcm2835 Headphones, bcm2835 Headphones"},
	{Name: "sysdefault:CARD=Headphones", Description: "bcm2835 Headphones, bcm2835 Headphones"},
	{Name: "dmix:CARD=Headphones,DEV=0", Description: "bcm2835 Headphones, bcm2835 Headphones"},
}

func audioPostForm(t *testing.T, srv *web.Server, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/audio", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAudioPageRequiresSession(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /audio with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

// "Аудіовихід" sits one letter from "Звуки" (the sound library) in the nav —
// two near-identical Ukrainian words leading to unrelated screens is exactly
// the mix-up this label was chosen to avoid, on the one device whose first
// act, out of the box, is to fail to make a sound. Pinned here so a future
// edit that quietly drifts the heading or the nav link back towards "Звук"
// fails a test instead of only a reviewer's eye.
func TestAudioPageHeadingAndNavLabelReadAudiovyhid(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/audio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<h1 class="text-lg font-semibold">Аудіовихід</h1>`) {
		t.Errorf("the page heading is not \"Аудіовихід\"; body:\n%s", body)
	}
	if !strings.Contains(body, `'/audio' ? 'page' : null">Аудіовихід</a>`) {
		t.Errorf("the nav link to /audio is not labelled \"Аудіовихід\"; body:\n%s", body)
	}
}

// The configured device ("default", config.Default's own initial value) must
// come back marked selected, proving the picker reads live state rather than
// always defaulting to its first row.
func TestAudioListsDevicesAndMarksConfiguredOneSelected(t *testing.T) {
	srv, _ := newServerWithDevices(t, []player.Device{
		{Name: "default", Description: "PLUG plugin"},
		{Name: "plughw:CARD=Headphones,DEV=0", Description: "bcm2835 Headphones"},
	})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/audio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="default" checked`) {
		t.Errorf("the configured device is not marked selected; body:\n%s", body)
	}
	if strings.Contains(body, `value="plughw:CARD=Headphones,DEV=0" checked`) {
		t.Errorf("a device that is not configured was marked selected; body:\n%s", body)
	}
}

func TestAudioSaveRejectsCrossOrigin(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodPost, "/audio", strings.NewReader(url.Values{
		"device": {"default"}, "master_volume": {"80"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAudioSaveWritesConfiguredDevice(t *testing.T) {
	srv, store := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := audioPostForm(t, srv, cookie, url.Values{
		"device": {"plughw:CARD=Headphones,DEV=0"}, "master_volume": {"65"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	cfg := store.Get()
	if cfg.Audio.Device != "plughw:CARD=Headphones,DEV=0" {
		t.Errorf("Audio.Device = %q, want the submitted device", cfg.Audio.Device)
	}
	if cfg.Audio.MasterVolume != 65 {
		t.Errorf("Audio.MasterVolume = %d, want 65", cfg.Audio.MasterVolume)
	}
}

// A PCM this board does not have silences the device while the dashboard
// shows a configured output — the same shape of failure as an unmatchable
// oblast, arrived at from the audio side.
func TestAudioRefusesADeviceAlsaDidNotReport(t *testing.T) {
	srv, store := newServerWithDevices(t, []player.Device{{Name: "default", Description: "PLUG plugin"}})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Audio.Device

	form := url.Values{"device": {"plughw:99,0"}, "master_volume": {"80"}}
	req := httptest.NewRequest(http.MethodPost, "/audio", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if got := store.Get().Audio.Device; got != before {
		t.Errorf("Audio.Device = %q, want it unchanged at %q", got, before)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "plughw:99,0") {
		t.Errorf("the rejection does not name the rejected device; body:\n%s", rec.Body.String())
	}
}

func TestAudioSaveRejectsMasterVolumeOutsideRange(t *testing.T) {
	for _, volume := range []string{"0", "101", "-5", "abc", ""} {
		t.Run(volume, func(t *testing.T) {
			srv, store := newServerWithDevices(t, realBoardDevices)
			setPassword(t, srv, "correct horse battery staple")
			cookie := login(t, srv, "correct horse battery staple")
			before := store.Get().Audio.MasterVolume

			rec := audioPostForm(t, srv, cookie, url.Values{"device": {"default"}, "master_volume": {volume}})

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
			}
			if got := store.Get().Audio.MasterVolume; got != before {
				t.Errorf("Audio.MasterVolume = %d, want it unchanged at %d", got, before)
			}
		})
	}
}

// A broken lister must not strand a volume-only change: the stored device
// already passed the "ALSA reported it" check the moment it was first
// saved, and refusing to re-confirm it now would bite hardest on exactly
// the board whose ALSA is misbehaving — the board whose settings most need
// changing.
func TestAudioSaveWithBrokenListerSavesAVolumeOnlyChange(t *testing.T) {
	store, err := configstore.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	lister := deviceListerFunc(func(context.Context) ([]player.Device, error) {
		return nil, errors.New("aplay -L: exit status 127: aplay: not found")
	})
	srv, err := web.New(web.Deps{Config: store, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	current := store.Get().Audio.Device // config.Default's "default"

	rec := audioPostForm(t, srv, cookie, url.Values{"device": {current}, "master_volume": {"42"}})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	cfg := store.Get()
	if cfg.Audio.Device != current {
		t.Errorf("Audio.Device = %q, want it unchanged at %q", cfg.Audio.Device, current)
	}
	if cfg.Audio.MasterVolume != 42 {
		t.Errorf("Audio.MasterVolume = %d, want 42", cfg.Audio.MasterVolume)
	}
}

// The narrower rule still refuses what it exists to refuse: a *new* device
// cannot be confirmed real when the lister that would confirm it is broken,
// so it must not be written to config.
func TestAudioSaveWithBrokenListerStillRefusesANewDevice(t *testing.T) {
	store, err := configstore.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	lister := deviceListerFunc(func(context.Context) ([]player.Device, error) {
		return nil, errors.New("aplay -L: exit status 127: aplay: not found")
	})
	srv, err := web.New(web.Deps{Config: store, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Audio.Device

	rec := audioPostForm(t, srv, cookie, url.Values{
		"device": {"plughw:CARD=Headphones,DEV=0"}, "master_volume": {"50"},
	})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body:\n%s", rec.Code, rec.Body.String())
	}
	cfg := store.Get()
	if cfg.Audio.Device != before {
		t.Errorf("Audio.Device = %q, want it unchanged at %q", cfg.Audio.Device, before)
	}
	if cfg.Audio.MasterVolume == 50 {
		t.Error("the volume was saved alongside a device change that should have been refused")
	}
}

// A board whose ALSA is broken must still show the rest of the page, not
// error outright — an operator on a device with no lister wired up at all
// still needs to see this screen exists.
func TestAudioPageRendersWithNoticeWhenDevicesIsNil(t *testing.T) {
	store, err := configstore.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/audio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "недоступний") {
		t.Errorf("the page does not explain why no devices are listed; body:\n%s", rec.Body.String())
	}
}

// A failing aplay must not turn this page into a 500, and its raw error text
// must never reach the browser — the same policy every other screen here
// applies to a backend failure.
func TestAudioPageRendersWithNoticeWhenTheListerFails(t *testing.T) {
	store, err := configstore.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	lister := deviceListerFunc(func(context.Context) ([]player.Device, error) {
		return nil, errors.New("aplay -L: exit status 127: aplay: not found")
	})
	srv, err := web.New(web.Deps{Config: store, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/audio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Не вдалося отримати список") {
		t.Errorf("the page does not explain the failure; body:\n%s", body)
	}
	if strings.Contains(body, "exit status") || strings.Contains(body, "not found") {
		t.Errorf("the raw aplay error leaked onto the page; body:\n%s", body)
	}
}

// The picker cannot be a flat list: 11 of the real board's 13 devices are
// indistinguishable by description, so this proves the picker actually
// groups by card rather than offering all 13 as separate rows, and orders
// the working output (the headphone jack) ahead of the one that does not
// work without a monitor attached (HDMI).
func TestAudioGroupsTheRealBoardCaptureByCard(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/audio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The grouped picker offers "default" plus one row per card (vc4hdmi,
	// Headphones): each card's representative PCM — the plughw variant —
	// therefore appears as a radio *value* exactly twice in the whole page:
	// once as that one grouped row, once again inside the full list behind
	// the disclosure. A flat, ungrouped picker would instead show every one
	// of the six vc4hdmi (or five Headphones) PCMs as its own row, and this
	// count would be higher. Counted by the value="..." attribute rather
	// than the bare name: the full list's own label text ("name —
	// description") repeats the name a second time within that one row, so
	// counting the bare name would overcount by one for a reason that has
	// nothing to do with grouping.
	for _, pcm := range []string{"plughw:CARD=vc4hdmi,DEV=0", "plughw:CARD=Headphones,DEV=0"} {
		attr := `value="` + pcm + `"`
		if got := strings.Count(body, attr); got != 2 {
			t.Errorf("%s appears %d times, want 2 (one grouped row, one full-list row)", attr, got)
		}
	}

	// Every one of the 13 raw PCMs is still reachable, behind the
	// disclosure — the rule is "group", not "filter away".
	for _, d := range realBoardDevices {
		if !strings.Contains(body, d.Name) {
			t.Errorf("the full list does not offer %q", d.Name)
		}
	}

	// The working output (Headphones) is easier to find than the one that
	// fails without a monitor attached (HDMI): its grouped row comes first.
	// Both descriptions' first occurrence *within the picker* is their
	// grouped row — the disclosure holding the full list renders entirely
	// after the grouped picker — so comparing the first occurrence of each
	// there is exactly this. Measured from the fieldset rather than from
	// the top of the page because the live-state block above the form names
	// the card "default" resolves to, which on this board is the HDMI one:
	// that sentence is the whole point of that block and says nothing about
	// the order of the rows below it.
	picker := body[strings.Index(body, "<fieldset"):]
	headphonesAt := strings.Index(picker, "bcm2835 Headphones")
	hdmiAt := strings.Index(picker, "vc4-hdmi, MAI PCM i2s-hifi-0")
	if headphonesAt < 0 || hdmiAt < 0 {
		t.Fatalf("expected both card descriptions in the picker; body:\n%s", body)
	}
	if headphonesAt > hdmiAt {
		t.Errorf("HDMI's row (at %d) comes before Headphones' (at %d); want the working output first", hdmiAt, headphonesAt)
	}

	// "default" itself must still be offered — it is config.Default's own
	// initial value, and the realistic case an untouched device starts in.
	if !strings.Contains(body, `value="default"`) {
		t.Error(`the "default" row is missing`)
	}
}

// A device saved by hand (or by a USB card only reachable through the full
// list) must not look unselected just because it lives behind a closed
// <details> — the disclosure has to open by itself so the operator can see
// what is actually configured.
func TestAudioDisclosureOpensWhenTheConfiguredDeviceIsOnlyInTheFullList(t *testing.T) {
	srv, store := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.Audio.Device = "hw:CARD=Headphones,DEV=0"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/audio", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "<details") {
		t.Fatalf("the page does not render the disclosure at all; body:\n%s", body)
	}
	if !strings.Contains(body, `<details class="mt-2" open>`) {
		t.Errorf("the disclosure did not open for a device only reachable inside it; body:\n%s", body)
	}
}

// mixerLister is the pair of questions a real board answers: what outputs
// are there (player.ALSA.Devices) and what control sets the volume on one of
// them (player.ALSA.MixerControl). /audio asks the second by type assertion,
// so a stub that answers both is what exercises that path.
type mixerLister struct {
	devices []player.Device
	control string
	err     error
}

func (m mixerLister) Devices(context.Context) ([]player.Device, error) { return m.devices, nil }

func (m mixerLister) MixerControl(context.Context, string) (string, error) {
	return m.control, m.err
}

func newServerWithMixer(t *testing.T, lister web.DeviceLister) (*web.Server, *configstore.Store) {
	t.Helper()
	store, err := configstore.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store
}

// getAudio fetches /audio with a session and returns the rendered body.
func getAudio(t *testing.T, srv *web.Server, cookie *http.Cookie, query string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/audio"+query, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /audio%s = %d, want 200; body:\n%s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// The operator's report was «Аудіовихід не показується поточний вихід». A
// radio button carrying `checked` is not the same thing as telling someone
// what their device is using, and the case it matters in is the one a fresh
// install is in: the stored device is "default", and on this board
// "default" is the HDMI card nothing is plugged into — which is the whole
// reason it plays nothing.
func TestAudioPageStatesTheStoredOutputAndWhereDefaultLeads(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "Поточний вихід") {
		t.Errorf("the page never states the current output; body:\n%s", body)
	}
	// The trap, named: "default" on this board is card 0, which is HDMI.
	if !strings.Contains(body, "веде на першу звукову карту пристрою — vc4-hdmi, MAI PCM i2s-hifi-0") {
		t.Errorf("the page does not say which card \"default\" actually resolves to; body:\n%s", body)
	}
}

// The volume box has no effect on HDMI, because HDMI's volume belongs to the
// television. An operator who turns the number down and hears no change has
// otherwise no way to tell that from a broken device.
func TestAudioPageSaysWhenTheOutputHasNoVolumeControl(t *testing.T) {
	srv, _ := newServerWithMixer(t, mixerLister{devices: realBoardDevices, control: ""})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "регулювання гучності недоступне для цього виходу") {
		t.Errorf("the page does not explain why the volume box does nothing here; body:\n%s", body)
	}
}

// And when the card does have one, the page names it: "PCM" is what the
// Pi's headphone card calls the control the daemon spent this whole bug
// asking for as "Master".
func TestAudioPageNamesTheDiscoveredMixerControl(t *testing.T) {
	srv, _ := newServerWithMixer(t, mixerLister{devices: realBoardDevices, control: "PCM"})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "регулятор «PCM»") {
		t.Errorf("the page does not name the control that sets the volume; body:\n%s", body)
	}
	if strings.Contains(body, "регулювання гучності недоступне") {
		t.Errorf("a card with a PCM control was described as having no volume control; body:\n%s", body)
	}
}

// A USB card unplugged since it was chosen leaves the configuration pointing
// at a PCM that no longer exists. The page used to show that as nothing
// being selected at all, which looks like a page that failed to load rather
// than a device that cannot play.
func TestAudioPageWarnsWhenTheStoredOutputIsNoLongerReported(t *testing.T) {
	srv, store := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if err := store.Update(func(c *config.Config) error {
		c.Audio.Device = "plughw:CARD=USB,DEV=0"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "не повідомляється системою") {
		t.Errorf("the page does not warn that the stored output is gone; body:\n%s", body)
	}
	if !strings.Contains(body, "plughw:CARD=USB,DEV=0") {
		t.Errorf("the warning does not name the output that is gone; body:\n%s", body)
	}
}

// A stored device the lister does report must not be accused of being gone.
func TestAudioPageDoesNotWarnAboutAnOutputThatIsThere(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	if body := getAudio(t, srv, cookie, ""); strings.Contains(body, "не повідомляється системою") {
		t.Errorf("the stored device \"default\" is in the list but was reported missing; body:\n%s", body)
	}
}

// A lister that failed cannot convict the stored device of anything: the
// page already says the list is unavailable, and "your output no longer
// exists" on top of that is a second, louder and quite possibly false alarm.
func TestAudioPageDoesNotWarnWhenTheListerItselfFailed(t *testing.T) {
	store, err := configstore.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	lister := deviceListerFunc(func(context.Context) ([]player.Device, error) {
		return nil, errors.New("aplay -L: exit status 127: aplay: not found")
	})
	srv, err := web.New(web.Deps{Config: store, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if strings.Contains(body, "не повідомляється системою") {
		t.Errorf("a broken lister was treated as proof the output is gone; body:\n%s", body)
	}
	// The state it does know — what is stored — is still stated.
	if !strings.Contains(body, "Поточний вихід") {
		t.Errorf("the page stopped stating the stored output when the lister failed; body:\n%s", body)
	}
}

// newServerWithTestSound wires a real engine, player and library — the same
// shape newServerWithPlayer uses — plus the device lister /audio needs, so
// the test-sound button exercises the actual manual-playback path rather
// than a mock standing in for it. The library holds the all-clear and the
// siren, so which of the two the button reaches for is a fact about the
// handler and not about what happened to be on disk.
func newServerWithTestSound(t *testing.T) (*web.Server, *configstore.Store, *fakePlaybackBackend) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}

	soundsDir := t.TempDir()
	riff := make([]byte, 64)
	copy(riff, "RIFF")
	copy(riff[8:], "WAVE")
	for _, name := range []string{"siren.wav", "all-clear.wav"} {
		if err := os.WriteFile(filepath.Join(soundsDir, name), riff, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	lib := library.New(soundsDir, library.FFmpeg{})

	backend := newFakePlaybackBackend()
	pl := player.New(backend, "default")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pl.Run(ctx)

	eng := engine.New(engine.Deps{
		Config:  store.Get,
		Player:  pl,
		Library: lib,
		Clock:   clock.System{Loc: time.UTC},
	})

	lister := deviceListerFunc(func(context.Context) ([]player.Device, error) { return realBoardDevices, nil })
	srv, err := web.New(web.Deps{Config: store, Engine: eng, Player: pl, Library: lib, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store, backend
}

func TestAudioPageOffersATestSoundButton(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, `name="action" value="test"`) {
		t.Errorf("the page offers no way to test the output; body:\n%s", body)
	}
	if !strings.Contains(body, "Перевірити звук") {
		t.Errorf("the test button is not labelled; body:\n%s", body)
	}
}

// The button plays the all-clear and deliberately not the siren: whoever
// presses it is standing next to a PA system, and a test that sounds like a
// real air raid to everyone within earshot is a test nobody presses twice.
func TestAudioTestSoundPlaysTheAllClearNotTheSiren(t *testing.T) {
	srv, _, backend := newServerWithTestSound(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := audioPostForm(t, srv, cookie, url.Values{"action": {"test"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/audio?tested=1" {
		t.Errorf("redirected to %q, want /audio?tested=1", loc)
	}
	if got := waitPlayed(t, backend); got != "all-clear.wav" {
		t.Errorf("the test played %q, want all-clear.wav", got)
	}
}

// Pressing the button must not save anything: it tests what is stored, not
// what the radio buttons happen to be showing.
func TestAudioTestSoundDoesNotSaveSettings(t *testing.T) {
	srv, store, backend := newServerWithTestSound(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	before := store.Get().Audio

	audioPostForm(t, srv, cookie, url.Values{
		"action": {"test"}, "device": {"plughw:CARD=Headphones,DEV=0"}, "master_volume": {"11"},
	})
	waitPlayed(t, backend)

	if got := store.Get().Audio; got != before {
		t.Errorf("Audio = %+v, want it unchanged at %+v: the test button saved settings", got, before)
	}
}

// The page the operator lands back on has to say what happened, which is
// the whole point of testing from here rather than from the dashboard.
func TestAudioTestSoundReportsSuccessOnThePage(t *testing.T) {
	srv, _, backend := newServerWithTestSound(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	audioPostForm(t, srv, cookie, url.Values{"action": {"test"}})
	waitPlayed(t, backend)

	body := getAudio(t, srv, cookie, "?tested=1")

	if !strings.Contains(body, "Тестовий звук надіслано на поточний вихід") {
		t.Errorf("the page does not report that the test sound was sent; body:\n%s", body)
	}
}

// A device with no playback wired up at all must say so, and must not
// report a success it did not have. Silence that the page calls success is
// worse than silence it calls a failure: it sends the operator looking at
// the speaker wiring instead of at the daemon.
func TestAudioTestSoundReportsFailureWhenPlaybackIsUnavailable(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices) // no Engine, no Library
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := audioPostForm(t, srv, cookie, url.Values{"action": {"test"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/audio?test_error=unavailable" {
		t.Fatalf("redirected to %q, want /audio?test_error=unavailable", loc)
	}

	body := getAudio(t, srv, cookie, "?test_error=unavailable")
	if !strings.Contains(body, "Відтворення недоступне на цьому пристрої.") {
		t.Errorf("the page does not report the failure; body:\n%s", body)
	}
	if strings.Contains(body, "Тестовий звук надіслано") {
		t.Errorf("the page reported a success that never happened; body:\n%s", body)
	}
}

// The flash travels as a code, not as a sentence: anyone who can get a
// logged-in operator to open a crafted link must not be able to put words on
// the device's own dashboard. Same rule handlers.go's flashMessage applies
// to /play's "played" parameter.
func TestAudioFlashNeverEchoesTheQueryString(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "?test_error="+url.QueryEscape("Тривога! Це не тест"))

	if strings.Contains(body, "Тривога! Це не тест") {
		t.Errorf("an unvalidated flash code was echoed onto the page; body:\n%s", body)
	}
}

// Saving an output must reach the running player, not only the config file.
// Observed on the board before this was fixed: the output was switched to
// the headphone jack, the config said so, and the very next test sound
// still went to "default" — HDMI, with nothing plugged into it — because
// the player had taken its device at construction and never looked again.
// On this product that is the difference between announcing and staying
// silent, and the only cure was a restart.
func TestAudioSaveRetargetsTheRunningPlayer(t *testing.T) {
	srv, _, backend := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	form := url.Values{
		"device":        {"plughw:CARD=Headphones,DEV=0"},
		"master_volume": {"80"},
	}
	req := httptest.NewRequest(http.MethodPost, "/audio", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.test")
	req.Host = "example.test"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	play := url.Values{"sound": {"siren.wav"}}
	preq := httptest.NewRequest(http.MethodPost, "/play", strings.NewReader(play.Encode()))
	preq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	preq.Header.Set("Origin", "http://example.test")
	preq.Host = "example.test"
	preq.AddCookie(cookie)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), preq)

	waitPlayed(t, backend)
	got := backend.Devices()
	if len(got) == 0 {
		t.Fatal("Play was never called")
	}
	if got[len(got)-1] != "plughw:CARD=Headphones,DEV=0" {
		t.Errorf("played to %q, want the output just saved; all devices: %v", got[len(got)-1], got)
	}
}

// audioText is the rendered page with HTML escaping undone.
//
// The Ukrainian on this screen is full of apostrophes — «роз'єм»,
// «під'єднано» — and html/template turns every one of them into &#39; on the
// way out, correctly. A test asserting on a sentence that reached the page
// through a template variable therefore has to compare against the words, not
// against the entity soup, or it pins the escaping rather than the wording.
func audioText(body string) string { return html.UnescapeString(body) }

// probingLister answers both questions a real board answers about an
// output: what outputs there are (player.ALSA.Devices) and whether one of
// them can actually be opened (player.ALSA.ProbeDevice). /audio reaches the
// second by type assertion, so a lister that does not implement it — the
// plain deviceListerFunc the other tests here use — is also how "nobody
// could be asked" gets exercised.
type probingLister struct {
	devices []player.Device
	// refuse maps a PCM name to what opening it says. A name absent from
	// it opens fine, which is the shape a real board has: most outputs
	// work and one does not.
	refuse map[string]player.ProbeResult
	// err makes the probe itself impossible to carry out — no temp space
	// to write the silence to — which says nothing about any device.
	err error
	// probed records every output this lister was asked to open, in order,
	// so a test can state which ones the page probed and, more to the
	// point, which ones it did not.
	probed []string
}

func (p *probingLister) Devices(context.Context) ([]player.Device, error) { return p.devices, nil }

func (p *probingLister) ProbeDevice(_ context.Context, device string) (player.ProbeResult, error) {
	p.probed = append(p.probed, device)
	if p.err != nil {
		return player.ProbeResult{}, p.err
	}
	if res, ok := p.refuse[device]; ok {
		return res, nil
	}
	return player.ProbeResult{Playable: true}, nil
}

// realBoardProber is the operator's Raspberry Pi 4 as it actually behaved:
// the HDMI card, and the "default" that resolves to it, refuse to open with
// nothing plugged into HDMI, while the headphone jack plays immediately.
func realBoardProber() *probingLister {
	refused := player.ProbeResult{Detail: "aplay -D default: exit status 1: aplay: main:850: audio open error: Unknown error 524"}
	return &probingLister{
		devices: realBoardDevices,
		refuse: map[string]player.ProbeResult{
			"default":                   refused,
			"plughw:CARD=vc4hdmi,DEV=0": refused,
		},
	}
}

func newServerWithProber(t *testing.T, lister web.DeviceLister) (*web.Server, *configstore.Store) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store, Devices: lister})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store
}

// The defect, on the page: the board offers three outputs, the one listed
// first and named "default" is the only one that cannot play, and the screen
// gave an operator no way to find that out short of an air raid. Now it
// opens each of them and says.
func TestAudioMarksAnOutputThatCannotBeOpened(t *testing.T) {
	srv, _ := newServerWithProber(t, realBoardProber())
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "Не працює зараз") {
		t.Errorf("no output was marked unusable, though HDMI and \"default\" both refused to open; body:\n%s", body)
	}
	// The sentence has to be one an operator can act on — a cable and a
	// screen, not a kernel constant.
	if !strings.Contains(audioText(body), "HDMI передає звук лише тоді, коли під'єднано екран або підсилювач") {
		t.Errorf("the page does not say why HDMI cannot play; body:\n%s", body)
	}
	// And aplay's own diagnostic stays in the log where it belongs.
	if strings.Contains(body, "Unknown error 524") || strings.Contains(body, "exit status") {
		t.Errorf("the raw aplay diagnostic reached the page; body:\n%s", body)
	}
	// The working output is marked as working. "It did not say it was
	// broken" is not the same thing as "it said it works", and an operator
	// picking between three rows needs the second.
	if !strings.Contains(body, "Перевірено: вихід вдається відкрити.") {
		t.Errorf("the working output is not marked as working; body:\n%s", body)
	}
}

// A board whose HDMI amplifier is switched on an hour from now must not have
// been permanently talked out of HDMI by one probe. Marked, never disabled.
func TestAudioKeepsAnUnusableOutputSelectable(t *testing.T) {
	srv, _ := newServerWithProber(t, realBoardProber())
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, `value="plughw:CARD=vc4hdmi,DEV=0"`) {
		t.Fatalf("the unusable output is not offered at all; body:\n%s", body)
	}
	if strings.Contains(body, "disabled") {
		t.Errorf("an output was disabled; it must stay selectable for a sink plugged in later; body:\n%s", body)
	}
}

// Thirteen probes on a page load is thirteen silent playbacks to learn what
// three of them already said — eleven of the board's PCMs are the same two
// cards answering again. The full list is probed only when one of its PCMs
// is the output actually in use.
func TestAudioDoesNotProbeTheFullDisclosureList(t *testing.T) {
	lister := realBoardProber()
	srv, _ := newServerWithProber(t, lister)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	getAudio(t, srv, cookie, "")

	// The three grouped rows, and the stored output — which on an untouched
	// device is "default", already one of the three.
	if got := len(lister.probed); got > 4 {
		t.Errorf("the page ran %d probes (%v), want at most the 3 grouped rows plus the stored output", got, lister.probed)
	}
	for _, pcm := range []string{"hw:CARD=Headphones,DEV=0", "dmix:CARD=vc4hdmi,DEV=0", "sysdefault:CARD=Headphones"} {
		for _, p := range lister.probed {
			if p == pcm {
				t.Errorf("the page probed %q, which only the full disclosure list offers", pcm)
			}
		}
	}
}

// A wedged card — one another process holds open — is a different thing to
// tell an operator than a card with nothing plugged into it. The first sends
// them to the daemon, the second to the cable.
func TestAudioWordsAWedgedOutputDifferentlyFromARefusedOne(t *testing.T) {
	lister := &probingLister{
		devices: realBoardDevices,
		refuse: map[string]player.ProbeResult{
			"plughw:CARD=Headphones,DEV=0": {TimedOut: true},
		},
	}
	srv, _ := newServerWithProber(t, lister)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "не відповів вчасно") {
		t.Errorf("a wedged output was not described as one; body:\n%s", body)
	}
}

// A lister that cannot be asked whether an output opens — the plain
// deviceListerFunc, a future implementation, a board with nothing wired up —
// must leave the page saying nothing about it rather than guessing. A page
// that claims an output works because nobody checked is the defect again.
func TestAudioSaysNothingAboutOutputsWhenThereIsNoProber(t *testing.T) {
	srv, _ := newServerWithDevices(t, realBoardDevices) // deviceListerFunc: Devices only
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if strings.Contains(body, "Перевірено") || strings.Contains(body, "Не працює зараз") {
		t.Errorf("the page claimed something about outputs nobody probed; body:\n%s", body)
	}
}

// A probe that could not be carried out at all — no temp space for the
// silence — is not evidence against the device, and must not be rendered as
// if it were.
func TestAudioSaysNothingWhenTheProbeItselfCouldNotRun(t *testing.T) {
	lister := &probingLister{devices: realBoardDevices, err: errors.New("probe file: no space left on device")}
	srv, _ := newServerWithProber(t, lister)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if strings.Contains(body, "Не працює зараз") {
		t.Errorf("a probe that never ran was rendered as a broken output; body:\n%s", body)
	}
	if strings.Contains(body, "no space left") {
		t.Errorf("the raw probe error reached the page; body:\n%s", body)
	}
}

// The grouped list used to read "bcm2835 Headphones, bcm2835 Headphones" and
// "vc4-hdmi, MAI PCM i2s-hifi-0" — driver strings, which are the right
// answer to "which PCM" and no answer at all to "where do I plug the
// speakers in". The PCM name stays, in smaller text: it is what the full
// list and the event log use, and hiding it would make the two impossible to
// match up.
func TestAudioGroupedRowsAreLabelledForAnOperator(t *testing.T) {
	srv, _ := newServerWithProber(t, realBoardProber())
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	for _, want := range []string{"Роз'єм для навушників (3,5 мм)", "HDMI"} {
		if !strings.Contains(audioText(body), want) {
			t.Errorf("the grouped picker never says %q; body:\n%s", want, body)
		}
	}
	for _, pcm := range []string{"plughw:CARD=Headphones,DEV=0", "plughw:CARD=vc4hdmi,DEV=0"} {
		if !strings.Contains(body, pcm) {
			t.Errorf("the raw PCM name %q is not shown anywhere; the two lists cannot be matched up", pcm)
		}
	}
}

// The labels come from the words aplay -L gives for the card, not from a
// list of board models: this project runs on Orange Pi and on whatever USB
// card is in someone's drawer, and a table of Raspberry Pi revisions would
// be wrong on every one of them.
func TestAudioLabelsAUsbCardItHasNeverMet(t *testing.T) {
	srv, _ := newServerWithProber(t, &probingLister{devices: []player.Device{
		{Name: "default", Description: "Default Audio Device"},
		{Name: "plughw:CARD=Device,DEV=0", Description: "USB Audio Device, USB Audio"},
	}})
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	body := getAudio(t, srv, cookie, "")

	if !strings.Contains(body, "USB-аудіопристрій") {
		t.Errorf("a USB card was not named as one; body:\n%s", body)
	}
}

// The whole of the save half of this defect: a chosen output that cannot be
// opened is stored anyway — the operator may be wiring the speakers next —
// but the page must say plainly that it could not be opened. Silent success
// on a device that cannot play is the bug.
func TestAudioSaveWithAFailingProbeStoresTheDeviceAndSaysSo(t *testing.T) {
	srv, store := newServerWithProber(t, realBoardProber())
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := audioPostForm(t, srv, cookie, url.Values{
		"device": {"plughw:CARD=vc4hdmi,DEV=0"}, "master_volume": {"70"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a failed probe must not reject the save; body:\n%s", rec.Code, rec.Body.String())
	}
	cfg := store.Get()
	if cfg.Audio.Device != "plughw:CARD=vc4hdmi,DEV=0" {
		t.Errorf("Audio.Device = %q, want the chosen output stored even though it could not be opened", cfg.Audio.Device)
	}
	if cfg.Audio.MasterVolume != 70 {
		t.Errorf("Audio.MasterVolume = %d, want 70", cfg.Audio.MasterVolume)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "не вдалося відкрити") {
		t.Errorf("the save reported nothing about the output refusing to open; body:\n%s", body)
	}
	if strings.Contains(body, "Вихід перевірено — його вдалося відкрити.") {
		t.Errorf("the save claimed the output opened when it did not; body:\n%s", body)
	}
}

// And the other side of it: a save whose probe succeeded says so, because
// "збережено" alone is what left the operator with nothing to conclude.
func TestAudioSaveConfirmsAnOutputThatOpened(t *testing.T) {
	srv, store := newServerWithProber(t, realBoardProber())
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := audioPostForm(t, srv, cookie, url.Values{
		"device": {"plughw:CARD=Headphones,DEV=0"}, "master_volume": {"70"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := store.Get().Audio.Device; got != "plughw:CARD=Headphones,DEV=0" {
		t.Errorf("Audio.Device = %q, want the chosen output", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Вихід перевірено — його вдалося відкрити.") {
		t.Errorf("the save does not confirm the output opened; body:\n%s", body)
	}
	if strings.Contains(body, "не вдалося відкрити") {
		t.Errorf("a working output was reported as unopenable; body:\n%s", body)
	}
}

// The save probes what was saved, not what was stored a moment before it.
// Getting this backwards would report the old output's health against the
// new output's name — the same class of mistake as the player that kept
// playing through the device it was built with.
func TestAudioSaveProbesTheOutputItJustStored(t *testing.T) {
	lister := realBoardProber()
	srv, _ := newServerWithProber(t, lister)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	lister.probed = nil
	audioPostForm(t, srv, cookie, url.Values{
		"device": {"plughw:CARD=Headphones,DEV=0"}, "master_volume": {"70"},
	})

	found := false
	for _, p := range lister.probed {
		if p == "plughw:CARD=Headphones,DEV=0" {
			found = true
		}
	}
	if !found {
		t.Errorf("the saved output was never probed; probed %v", lister.probed)
	}
}
