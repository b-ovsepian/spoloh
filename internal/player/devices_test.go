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
	want := map[string]string{
		"default":                    "Playback/recording through the PLUG plugin",
		"sysdefault:CARD=Headphones": "bcm2835 Headphones, bcm2835 Headphones",
		"hdmi:CARD=vc4hdmi0,DEV=0":   "vc4-hdmi-0, MAI PCM i2s-hifi-0",
		"sysdefault:CARD=Device":     "USB Audio Device, USB Audio",
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
