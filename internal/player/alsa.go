package player

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
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
	// Mixer names the amixer simple control to set the volume on. Empty —
	// the normal case — means discover it from the card itself; see
	// MixerControl. An explicit value always wins over discovery, which is
	// the escape hatch for a card whose control this package's priority
	// list ranks wrongly.
	Mixer string
	// Run replaces the exec of aplay/amixer. Nil means the real thing.
	Run RunFunc
	// mixers holds the discovered control per card. Nil — which is what
	// every production construction leaves it, since the field is
	// unexported — means the package-wide cache below. The cache cannot
	// live in this struct directly: main.go builds one ALSA value and
	// hands copies of it to both player.New and the dashboard's device
	// lister, so a per-value cache would be filled once per copy, and a
	// mutex inside a struct copied like that is a vet error besides.
	// Tests set it to keep one test's discovery out of the next one's.
	mixers *mixerCache
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

// mixerControlPriority ranks the simple controls a card may report, best
// first. It is a priority list and not a single name because the name that
// works is a property of the card, not of ALSA: a PC sound card calls its
// output "Master", the Raspberry Pi's headphone jack calls the very same
// thing "PCM", and the daemon asking for "Master" on a Pi produced
//
//	amixer: Unable to find simple control 'Master',0
//
// on every single announcement — volume never once applied, and an ERROR in
// the log that reads, to whoever is diagnosing a silent device, exactly like
// the sound itself having failed.
//
// A card reporting none of these still gets its first control used rather
// than nothing (see pickMixerControl): an unranked name is a card this list
// has not met yet, which is a reason to try it, not to give up on volume.
var mixerControlPriority = []string{"Master", "PCM", "Headphone", "Speaker", "Digital", "Playback", "Analogue"}

// mixerCache remembers the control discovered for one card, so that
// announcing an alert does not fork amixer twice — once to ask what the
// control is called, once to set it. The shape is internal/clock's
// Timedatectl: the mutex is held across the exec as well as the bookkeeping,
// so two announcements arriving together cannot both go and ask.
//
// A cached "" is a real answer — "this card has no simple controls" — which
// is why presence in the map, not the emptiness of the value, is what says
// whether the question has been asked.
type mixerCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newMixerCache() *mixerCache { return &mixerCache{m: map[string]string{}} }

// sharedMixerCache is what every ALSA value built outside a test uses. It is
// package-wide because ALSA is passed around by value — see the mixers field
// — and a device has exactly one set of sound cards, so one cache for the
// process is the same thing as one cache per board.
var sharedMixerCache = newMixerCache()

func (a ALSA) mixerCache() *mixerCache {
	if a.mixers != nil {
		return a.mixers
	}
	return sharedMixerCache
}

// MixerControl reports the amixer simple control that sets the volume for
// device, discovering it from the card when ALSA.Mixer does not say.
//
// An empty name is not a failure: it means the card reports no simple
// controls at all, which is the normal and permanent state of HDMI output —
// its volume is the sink's, not ALSA's. Callers must treat that as "there is
// nothing to set here", never as an error. See SetVolume.
func (a ALSA) MixerControl(ctx context.Context, device string) (string, error) {
	if a.Mixer != "" {
		return a.Mixer, nil
	}

	ctl := ctlDevice(device)
	// Keyed by the amixer binary as well as the card: a test (or a second
	// ALSA value with its own AmixerBin) asks a different program the same
	// question and can get a different answer.
	key := a.amixer() + "\x00" + ctl

	cache := a.mixerCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if name, ok := cache.m[key]; ok {
		return name, nil
	}

	args := []string{}
	if ctl != "" {
		args = append(args, "-D", ctl)
	}
	args = append(args, "scontrols")

	out, err := a.run(ctx, a.amixer(), args...)
	if err != nil {
		// Nothing is cached on a failure: amixer missing on a board someone
		// is still setting up, or a card busy at this instant, must not
		// leave "this device has no volume" behind for the life of the
		// daemon.
		return "", fmt.Errorf("amixer scontrols: %w: %s", err, strings.TrimSpace(string(out)))
	}

	name := pickMixerControl(parseMixerControls(out))
	cache.m[key] = name
	return name, nil
}

// parseMixerControls reads `amixer scontrols`, whose every line is
//
//	Simple mixer control 'PCM',0
//
// and returns the names in the order the card reported them. The quotes and
// the trailing index are amixer's own punctuation: "'PCM',0" handed back to
// amixer as a control name finds nothing.
func parseMixerControls(out []byte) []string {
	const prefix = "Simple mixer control "
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(line[len(prefix):])
		// The index after the last comma identifies which of several
		// same-named controls this is; it is not part of the name.
		if c := strings.LastIndexByte(rest, ','); c >= 0 {
			rest = rest[:c]
		}
		name := strings.Trim(strings.TrimSpace(rest), "'")
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// pickMixerControl chooses which of a card's controls to set, by
// mixerControlPriority and then by the card's own order. "" out means ""
// in: a card with no controls.
func pickMixerControl(names []string) string {
	for _, want := range mixerControlPriority {
		for _, have := range names {
			if have == want {
				return have
			}
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// SetVolume sets the master volume on the card behind device.
//
// A card with no simple mixer control is not an error and must never be
// reported as one. The Raspberry Pi's HDMI card is exactly this: its volume
// lives in the television or the amplifier, ALSA has no say in it, and there
// is nothing here to fail. Returning an error for it put an ERROR line in
// the log on every single announcement of every air raid — the noise this
// method exists to have stopped producing — and Player.playOne, which
// deliberately plays on after a volume failure, would have gone on reporting
// a fault that was never one.
func (a ALSA) SetVolume(ctx context.Context, device string, pct int) error {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	control, err := a.MixerControl(ctx, device)
	if err != nil {
		return err
	}
	if control == "" {
		return nil
	}

	args := []string{"-q"}
	if ctl := ctlDevice(device); ctl != "" {
		args = append(args, "-D", ctl)
	}
	args = append(args, "sset", control, fmt.Sprintf("%d%%", pct))

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
		// The device is named in the error, not just aplay's own exit status:
		// on a freshly installed board `default` resolves to the HDMI card,
		// and with nothing plugged into it every single playback fails the
		// same way ("audio open error: Unknown error 524", observed on a
		// real Raspberry Pi 4). Without the device name here, that error
		// reaches whoever reads the log as an unqualified "aplay: exit
		// status 1" — no way to tell which of possibly several configured
		// outputs is the broken one. web.handleLog points a playback error
		// at /audio for the same reason; this is what lets that message say
		// which device to change there.
		name := device
		if name == "" {
			name = "default"
		}
		return fmt.Errorf("aplay -D %s: %w: %s", name, err, out)
	}
	return nil
}
