package player

import "testing"

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
