package player_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/player"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// recorder stands in for aplay. It records the argv it was given so a test
// can assert on the command, and returns canned output.
type recorder struct {
	out  []byte
	err  error
	argv [][]string
}

func (r *recorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.argv = append(r.argv, append([]string{name}, args...))
	return r.out, r.err
}

func TestDevicesParsesRealAplayOutput(t *testing.T) {
	rec := &recorder{out: fixture(t, "aplay-L.txt")}
	a := player.ALSA{Run: rec.run}

	got, err := a.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}

	// "null" is dropped: it is a working device that plays nothing, which is
	// indistinguishable from a broken one on a device whose job is to be
	// heard.
	for _, d := range got {
		if d.Name == "null" {
			t.Error("the null device was offered as an output")
		}
	}

	// The fixture's two ALSA-lib diagnostic lines sit at column 0, same as a
	// real device name, because a.run merges stderr into stdout. Neither may
	// be offered as an output: no real PCM name contains whitespace, and both
	// of these do.
	for _, d := range got {
		if strings.Contains(d.Name, "ALSA lib") {
			t.Errorf("an ALSA-lib diagnostic was offered as a device: %q", d.Name)
		}
		if strings.ContainsAny(d.Name, " \t") {
			t.Errorf("device name %q contains whitespace; no real ALSA PCM name does", d.Name)
		}
	}
	want := map[string]string{
		"default":                      "Default Audio Device",
		"sysdefault:CARD=Headphones":   "bcm2835 Headphones, bcm2835 Headphones",
		"hdmi:CARD=vc4hdmi0,DEV=0":     "vc4-hdmi-0, MAI PCM i2s-hifi-0",
		"sysdefault:CARD=Device":       "USB Audio Device, USB Audio",
		"plughw:CARD=Device,DEV=0":     "USB Audio Device, USB Audio",
		"surround71:CARD=Device,DEV=0": "USB Audio Device, USB Audio",
	}
	found := map[string]string{}
	for _, d := range got {
		found[d.Name] = d.Description
	}
	for name, desc := range want {
		if found[name] != desc {
			t.Errorf("device %q description = %q, want %q", name, found[name], desc)
		}
	}
}

// TestDevicesParsesTheRealBoardCapture parses the verbatim `aplay -L`
// capture taken over SSH from a real Raspberry Pi 4 — internal/web's /audio
// screen is what a freshly installed device needs this exact shape for: 13
// devices, of which 11 are indistinguishable from each other by description
// alone (six read "vc4-hdmi, MAI PCM i2s-hifi-0", five read "bcm2835
// Headphones, bcm2835 Headphones"), which is why that screen groups by card
// rather than listing them flat.
func TestDevicesParsesTheRealBoardCapture(t *testing.T) {
	rec := &recorder{out: fixture(t, "aplay-L-rpi4.txt")}
	a := player.ALSA{Run: rec.run}

	got, err := a.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(got) != 13 {
		t.Fatalf("got %d devices, want 13 (null dropped from the capture's 14 lines)", len(got))
	}

	want := []player.Device{
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
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("device %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestDevicesKeepsAplayOrder(t *testing.T) {
	// aplay lists the most useful first; re-sorting would put "dmix" above
	// "default" alphabetically and bury the answer most operators want.
	rec := &recorder{out: fixture(t, "aplay-L.txt")}
	a := player.ALSA{Run: rec.run}

	got, _ := a.Devices(context.Background())
	if len(got) == 0 || got[0].Name != "default" {
		t.Errorf("first device = %+v, want default", got)
	}
}

func TestDevicesToleratesMalformedOutput(t *testing.T) {
	for _, body := range []string{
		"",
		"\n\n\n",
		"   \n",                 // an indented line with no name before it
		"lonely-device-no-desc", // a name with no description at all
		strings.Repeat("x", 100000),
	} {
		rec := &recorder{out: []byte(body)}
		a := player.ALSA{Run: rec.run}
		if _, err := a.Devices(context.Background()); err != nil {
			t.Errorf("Devices(%.20q) = %v, want no error", body, err)
		}
	}
}

func TestDevicesReportsAFailedCommand(t *testing.T) {
	rec := &recorder{err: errors.New("exit status 1"), out: []byte("aplay: not found")}
	a := player.ALSA{Run: rec.run}

	_, err := a.Devices(context.Background())
	if err == nil {
		t.Fatal("a failed aplay must be an error, not an empty list")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want aplay's own message", err)
	}
}
