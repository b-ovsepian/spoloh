package player

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCtlDevice(t *testing.T) {
	cases := []struct {
		pcm  string
		want string
	}{
		{"", ""},
		{"default", ""},
		{"hw:1,0", "hw:1"},
		{"plughw:1,0", "hw:1"},
		{"sysdefault:CARD=Headphones", "hw:CARD=Headphones"},
		{"plughw:CARD=Device,DEV=0", "hw:CARD=Device"},
		{"dmix:CARD=Headphones,DEV=0", "hw:CARD=Headphones"},
		{"noColonHere", ""},
		{"weird:notacard", ""},
		{"x:CARD=", ""}, // a card name is required, as a card number is
		{"x:CARD=,DEV=0", ""},
	}
	for _, c := range cases {
		if got := ctlDevice(c.pcm); got != c.want {
			t.Errorf("ctlDevice(%q) = %q, want %q", c.pcm, got, c.want)
		}
	}
}

// A failed Play must name the device that failed: on a board with several
// configured outputs, "aplay: exit status 1" alone gives whoever reads the
// log nothing to act on. See ALSA.Play's own comment on the real board this
// was observed on.
func TestPlayNamesTheDeviceOnFailure(t *testing.T) {
	failing := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("audio open error: Unknown error 524"), errors.New("exit status 1")
	}

	a := ALSA{Run: failing}
	err := a.Play(context.Background(), "plughw:CARD=vc4hdmi,DEV=0", "/tmp/siren.wav")
	if err == nil {
		t.Fatal("a failing aplay must be an error")
	}
	if !strings.Contains(err.Error(), "plughw:CARD=vc4hdmi,DEV=0") {
		t.Errorf("err = %v, want it to name the failed device", err)
	}
	if !strings.Contains(err.Error(), "audio open error: Unknown error 524") {
		t.Errorf("err = %v, want it to still carry aplay's own diagnostic", err)
	}
}

// An empty device string means aplay was run with no -D at all, letting ALSA
// pick its own default — so the error naming it has to say "default", not
// name nothing.
func TestPlayNamesTheDefaultDeviceWhenNoneWasGiven(t *testing.T) {
	failing := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	}

	a := ALSA{Run: failing}
	err := a.Play(context.Background(), "", "/tmp/siren.wav")
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Errorf("err = %v, want it to name \"default\"", err)
	}
}
