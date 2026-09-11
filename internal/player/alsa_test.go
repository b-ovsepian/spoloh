package player

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// readFixture reads one of this package's verbatim captures. devices_test.go
// has its own copy of this helper because it lives in package player_test;
// these tests are inside the package, where the mixer cache is reachable.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// amixerScript stands in for amixer. It answers `scontrols` with a canned
// capture and every other invocation with success, recording the argv so a
// test can state which commands the code actually ran — including the ones
// it must not have run at all.
type amixerScript struct {
	controls    []byte
	controlsErr error
	argv        [][]string
}

func (s *amixerScript) run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.argv = append(s.argv, append([]string{name}, args...))
	for _, a := range args {
		if a == "scontrols" {
			return s.controls, s.controlsErr
		}
	}
	return nil, nil
}

// ran reports whether any recorded invocation contained sub as an argument.
func (s *amixerScript) ran(sub string) bool {
	for _, argv := range s.argv {
		for _, a := range argv {
			if a == sub {
				return true
			}
		}
	}
	return false
}

func (s *amixerScript) count(sub string) int {
	n := 0
	for _, argv := range s.argv {
		for _, a := range argv {
			if a == sub {
				n++
				break
			}
		}
	}
	return n
}

// The quotes and the trailing index are amixer's own punctuation: handing
// "'PCM',0" back to amixer as a control name finds nothing. The capture is
// verbatim from the operator's Raspberry Pi 4 (card 1, the headphone jack).
func TestParseMixerControlsStripsAmixersPunctuation(t *testing.T) {
	got := parseMixerControls(readFixture(t, "amixer-scontrols-rpi4-card1.txt"))

	want := []string{"PCM"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("parseMixerControls = %#v, want %#v", got, want)
	}
}

// Card 0 on that board is vc4hdmi, and `amixer scontrols` for it prints
// nothing at all: HDMI volume is the television's, not ALSA's. The capture
// is an empty file because that is what the board printed.
func TestParseMixerControlsReadsACardWithNoControls(t *testing.T) {
	if got := parseMixerControls(readFixture(t, "amixer-scontrols-rpi4-card0.txt")); len(got) != 0 {
		t.Fatalf("parseMixerControls = %#v, want none", got)
	}
}

func TestPickMixerControl(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  string
	}{
		// The Pi's headphone card: "Master" is not there, and assuming it
		// was is the bug this whole path exists to fix.
		{"the pi headphone card", []string{"PCM"}, "PCM"},
		{"master outranks the rest", []string{"PCM", "Master", "Speaker"}, "Master"},
		{"pcm outranks headphone", []string{"Headphone", "PCM"}, "PCM"},
		// A card this list has not met is a reason to try its control, not
		// to give up on volume entirely.
		{"an unranked card falls back to its first control", []string{"Wave", "Mic"}, "Wave"},
		{"a card with no controls", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickMixerControl(c.names); got != c.want {
				t.Errorf("pickMixerControl(%#v) = %q, want %q", c.names, got, c.want)
			}
		})
	}
}

// The whole of bug 1 in one test: on the real board the control is called
// "PCM", the daemon asked for "Master", and volume was therefore never once
// applied on this hardware.
func TestSetVolumeSetsTheControlTheCardActuallyReports(t *testing.T) {
	script := &amixerScript{controls: readFixture(t, "amixer-scontrols-rpi4-card1.txt")}
	a := ALSA{Run: script.run, mixers: newMixerCache()}

	if err := a.SetVolume(context.Background(), "plughw:CARD=Headphones,DEV=0", 65); err != nil {
		t.Fatalf("SetVolume: %v", err)
	}

	var sset []string
	for _, argv := range script.argv {
		for _, arg := range argv {
			if arg == "sset" {
				sset = argv
			}
		}
	}
	if sset == nil {
		t.Fatalf("amixer was never asked to set anything: %v", script.argv)
	}
	joined := strings.Join(sset, " ")
	if !strings.Contains(joined, "sset PCM 65%") {
		t.Errorf("amixer ran %q, want it to set the card's own control PCM", joined)
	}
	if strings.Contains(joined, "Master") {
		t.Errorf("amixer ran %q, want it not to assume a Master control", joined)
	}
	if !strings.Contains(joined, "-D hw:CARD=Headphones") {
		t.Errorf("amixer ran %q, want it aimed at the card the PCM names", joined)
	}
}

// HDMI has no simple controls and never will: its volume is the television's.
// That is normal, so SetVolume must do nothing and say nothing — no amixer
// call, no error. An error here is what put
//
//	level=ERROR msg="playback failed" stage=volume
//
// in the log on every single announcement on the operator's board, which
// reads, to whoever is diagnosing a silent device, like the sound failed.
func TestSetVolumeIsASilentNoOpOnACardWithNoControls(t *testing.T) {
	script := &amixerScript{controls: readFixture(t, "amixer-scontrols-rpi4-card0.txt")}
	a := ALSA{Run: script.run, mixers: newMixerCache()}

	if err := a.SetVolume(context.Background(), "plughw:CARD=vc4hdmi,DEV=0", 80); err != nil {
		t.Fatalf("SetVolume on a card with no controls = %v, want nil: this is normal hardware, not a fault", err)
	}
	if script.ran("sset") {
		t.Errorf("amixer was asked to set a control on a card that has none: %v", script.argv)
	}
}

// An explicitly configured control is the escape hatch for a card whose
// control this package's priority list ranks wrongly, so it must not even
// ask the card.
func TestSetVolumeUsesAConfiguredMixerWithoutDiscovering(t *testing.T) {
	script := &amixerScript{controls: []byte("Simple mixer control 'PCM',0\n")}
	a := ALSA{Run: script.run, Mixer: "Headphone", mixers: newMixerCache()}

	if err := a.SetVolume(context.Background(), "plughw:CARD=Headphones,DEV=0", 40); err != nil {
		t.Fatalf("SetVolume: %v", err)
	}
	if script.ran("scontrols") {
		t.Errorf("the card was interrogated even though a mixer was configured: %v", script.argv)
	}
	if !strings.Contains(strings.Join(script.argv[0], " "), "sset Headphone 40%") {
		t.Errorf("amixer ran %v, want it to set the configured control", script.argv)
	}
}

// An alert announcement sets the volume before every sound it plays. Asking
// the card what its control is called each time would fork amixer twice per
// sound, on a board where the sound is the whole point.
func TestMixerControlIsDiscoveredOncePerCard(t *testing.T) {
	script := &amixerScript{controls: readFixture(t, "amixer-scontrols-rpi4-card1.txt")}
	a := ALSA{Run: script.run, mixers: newMixerCache()}

	for i := 0; i < 3; i++ {
		if err := a.SetVolume(context.Background(), "plughw:CARD=Headphones,DEV=0", 50); err != nil {
			t.Fatalf("SetVolume: %v", err)
		}
	}

	if got := script.count("scontrols"); got != 1 {
		t.Errorf("the card was interrogated %d times, want 1: %v", got, script.argv)
	}
	if got := script.count("sset"); got != 3 {
		t.Errorf("the volume was set %d times, want 3", got)
	}
}

// amixer missing altogether is a real fault and stays an error — but it must
// not be remembered as "this card has no volume", or a board someone is
// still installing alsa-utils on would never apply volume again for the life
// of the daemon.
func TestMixerControlDoesNotCacheAFailedDiscovery(t *testing.T) {
	script := &amixerScript{controlsErr: errors.New("exit status 127")}
	a := ALSA{Run: script.run, mixers: newMixerCache()}

	if err := a.SetVolume(context.Background(), "plughw:CARD=Headphones,DEV=0", 50); err == nil {
		t.Fatal("a failing amixer must be reported")
	}

	script.controlsErr = nil
	script.controls = readFixture(t, "amixer-scontrols-rpi4-card1.txt")
	if err := a.SetVolume(context.Background(), "plughw:CARD=Headphones,DEV=0", 50); err != nil {
		t.Fatalf("SetVolume after amixer came back: %v", err)
	}
	if !script.ran("sset") {
		t.Errorf("the failure was cached: the volume was never set afterwards: %v", script.argv)
	}
}
