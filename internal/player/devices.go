package player

import (
	"context"
	"fmt"
	"strings"
)

// Device is one ALSA PCM an operator can send audio to.
type Device struct {
	// Name is what goes to `aplay -D` and into config.Audio.Device.
	Name string
	// Description is the first line aplay prints under the name — the card's
	// own words, which is what a person recognises. Empty when aplay printed
	// none.
	Description string
}

// Devices lists the output devices this board has, in the order aplay reports
// them.
//
// The order is aplay's, deliberately: it lists the most useful first, and
// sorting alphabetically would put "dmix" above "default" and bury the answer
// most operators want. The "null" device is dropped — it accepts audio and
// plays nothing, which on a device whose entire job is to be heard is
// indistinguishable from a fault, and offering it invites someone to pick it.
func (a ALSA) Devices(ctx context.Context) ([]Device, error) {
	out, err := a.run(ctx, a.aplay(), "-L")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return nil, fmt.Errorf("aplay -L: %w", err)
		}
		return nil, fmt.Errorf("aplay -L: %w: %s", err, msg)
	}
	return parseDevices(out), nil
}

// parseDevices reads `aplay -L`: a PCM name at column 0, then zero or more
// indented description lines belonging to it.
func parseDevices(out []byte) []Device {
	var devices []Device
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		if trimmed[0] == ' ' || trimmed[0] == '\t' {
			// A description line. It belongs to the device above it, and only
			// the first one is kept: the later lines are ALSA's own prose
			// ("Default Audio Device") rather than anything identifying.
			if n := len(devices); n > 0 && devices[n-1].Description == "" {
				devices[n-1].Description = strings.TrimSpace(trimmed)
			}
			continue
		}
		name := strings.TrimSpace(trimmed)
		if name == "null" {
			continue
		}
		if strings.ContainsAny(name, " \t") {
			// a.run merges stderr into stdout — Play and SetVolume need that,
			// since it is the only diagnostic a headless board offers when
			// aplay or amixer fails — so an ALSA-lib line like "ALSA lib
			// confmisc.c:767:(parse_card) cannot find card '0'" lands here at
			// column 0, indistinguishable from a device name by position
			// alone. No real ALSA PCM name contains whitespace ("default",
			// "sysdefault:CARD=Headphones", "surround71:CARD=Device,DEV=0"),
			// and every such diagnostic does, so that is the property this
			// rejects on. A false reject costs one device in the list; a
			// false accept puts a line of C source coordinates in front of
			// someone choosing where a siren comes out.
			continue
		}
		devices = append(devices, Device{Name: name})
	}
	return devices
}
