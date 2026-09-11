package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
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
	// Both descriptions' first occurrence is their grouped row — the
	// disclosure holding the full list renders entirely after the grouped
	// picker — so comparing the first occurrence of each is exactly this.
	headphonesAt := strings.Index(body, "bcm2835 Headphones")
	hdmiAt := strings.Index(body, "vc4-hdmi, MAI PCM i2s-hifi-0")
	if headphonesAt < 0 || hdmiAt < 0 {
		t.Fatalf("expected both card descriptions on the page; body:\n%s", body)
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
