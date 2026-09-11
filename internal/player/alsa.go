package player

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RunFunc executes a command and returns its combined output. Nil means the
// real thing.
//
// It exists because ALSA is the one part of this daemon that cannot be
// exercised on a development machine: aplay and amixer are not installed, and
// the build is CGO_ENABLED=0 precisely so that no ALSA library is linked. A
// seam is the only way anything here gets a test.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// ALSA shells out to alsa-utils. Exec is deliberate: it keeps the binary free
// of cgo, which is what lets us cross-compile for every board from a laptop.
type ALSA struct {
	AplayBin  string // default "aplay"
	AmixerBin string // default "amixer"
	Mixer     string // default "Master"
	// Run replaces the exec of aplay/amixer. Nil means the real thing.
	Run RunFunc
}

func (a ALSA) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if a.Run != nil {
		return a.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (a ALSA) aplay() string {
	if a.AplayBin != "" {
		return a.AplayBin
	}
	return "aplay"
}

func (a ALSA) amixer() string {
	if a.AmixerBin != "" {
		return a.AmixerBin
	}
	return "amixer"
}

func (a ALSA) mixer() string {
	if a.Mixer != "" {
		return a.Mixer
	}
	return "Master"
}

// ctlDevice derives the amixer control name from an aplay PCM device name.
// aplay -D takes a PCM ("plughw:1,0", "sysdefault:CARD=Headphones"); amixer
// -D takes a control ("hw:1", "hw:CARD=Headphones"). Handing the PCM name to
// amixer fails on any non-default card, so volume is silently never applied.
// An empty result means "pass no -D at all and let amixer use the default
// card" — used both for "" / "default" and for anything unrecognised.
func ctlDevice(pcm string) string {
	if pcm == "" || pcm == "default" {
		return ""
	}
	idx := strings.IndexByte(pcm, ':')
	if idx < 0 {
		return ""
	}
	seg := pcm[idx+1:]
	if c := strings.IndexByte(seg, ','); c >= 0 {
		seg = seg[:c]
	}
	// The trailing name has to be there: "x:CARD=" would otherwise yield the
	// meaningless control "hw:CARD=", the same trap the digit branch below
	// guards against.
	if strings.HasPrefix(seg, "CARD=") && len(seg) > len("CARD=") {
		return "hw:" + seg
	}
	if seg != "" && isDigits(seg) {
		return "hw:" + seg
	}
	return ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a ALSA) SetVolume(ctx context.Context, device string, pct int) error {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	args := []string{"-q"}
	if ctl := ctlDevice(device); ctl != "" {
		args = append(args, "-D", ctl)
	}
	args = append(args, "sset", a.mixer(), fmt.Sprintf("%d%%", pct))

	out, err := a.run(ctx, a.amixer(), args...)
	if err != nil {
		return fmt.Errorf("amixer: %w: %s", err, out)
	}
	return nil
}

func (a ALSA) Play(ctx context.Context, device, path string) error {
	args := []string{"-q"}
	if device != "" {
		args = append(args, "-D", device)
	}
	// "--" so that a file whose name begins with a dash is read as a path and
	// not as a flag.
	args = append(args, "--", path)

	out, err := a.run(ctx, a.aplay(), args...)
	if err != nil {
		return fmt.Errorf("aplay: %w: %s", err, out)
	}
	return nil
}
