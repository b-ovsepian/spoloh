package player

import (
	"context"
	"fmt"
	"os/exec"
)

// ALSA shells out to alsa-utils. Exec is deliberate: it keeps the binary free
// of cgo, which is what lets us cross-compile for every board from a laptop.
type ALSA struct {
	AplayBin  string // default "aplay"
	AmixerBin string // default "amixer"
	Mixer     string // default "Master"
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

func (a ALSA) SetVolume(ctx context.Context, device string, pct int) error {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	args := []string{"-q"}
	if device != "" && device != "default" {
		args = append(args, "-D", device)
	}
	args = append(args, "sset", a.mixer(), fmt.Sprintf("%d%%", pct))

	out, err := exec.CommandContext(ctx, a.amixer(), args...).CombinedOutput()
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
	args = append(args, path)

	out, err := exec.CommandContext(ctx, a.aplay(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("aplay: %w: %s", err, out)
	}
	return nil
}
