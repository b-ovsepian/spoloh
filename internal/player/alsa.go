package player

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
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
	FfmpegBin string // default "ffmpeg"; used only by Stream
	// Mixer names the amixer simple control to set the volume on. Empty —
	// the normal case — means discover it from the card itself; see
	// MixerControl. An explicit value always wins over discovery, which is
	// the escape hatch for a card whose control this package's priority
	// list ranks wrongly.
	Mixer string
	// Run replaces the exec of aplay/amixer. Nil means the real thing.
	Run RunFunc
	// ProbeTimeout bounds one ProbeDevice call. Zero means
	// defaultProbeTimeout. It is a field rather than a constant so that a
	// test can prove the bound exists without waiting out the real one —
	// and so a board with a famously slow card can be given more room
	// without a rebuild.
	ProbeTimeout time.Duration
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

// The probe: answering "can this output actually be opened?" by opening it.
//
// Every other thing this package knows about an output is a name — a PCM
// string from `aplay -L` and a card description beside it. None of that says
// whether a sound comes out. On the operator's Raspberry Pi 4 the output
// named "default" is the one that cannot play at all: ALSA resolves it to
// card 0, which is HDMI, and HDMI with nothing attached that accepts audio
// returns ENOTSUPP the moment it is opened —
//
//	aplay -q -D default /tmp/sil.wav
//	aplay: main:850: audio open error: Unknown error 524
//
// while the very same command aimed at plughw:CARD=Headphones,DEV=0 exits 0
// immediately. Reasoning about the names cannot tell those two apart. Opening
// them can, and that is the whole of this.

// probe audio format. PCM 16-bit 44100 Hz stereo is what every ALSA card this
// project has met accepts, and plughw converts for the ones that would not.
const (
	probeSampleRate    = 44100
	probeChannels      = 2
	probeBitsPerSample = 16
	// probeSilence is how much silence the probe plays. Long enough that
	// aplay opens the device and hands it real frames — the ENOTSUPP this
	// exists to catch arrives at open, not at the end — and short enough
	// that three of these in a row on a page load are not a wait.
	probeSilence = 100 * time.Millisecond
	// wavHeaderLen is the canonical 44-byte RIFF/WAVE/fmt /data header.
	wavHeaderLen = 44
)

// defaultProbeTimeout bounds one probe when ALSA.ProbeTimeout says nothing.
// A wedged ALSA device — a card another process holds open, a driver that
// never answers — must not hang the page that is trying to diagnose it.
const defaultProbeTimeout = 3 * time.Second

// ProbeResult is what one attempt to open an output for real said.
//
// It is a value and not an error because "this output cannot be opened" is
// an answer, not a failure of the asking: it is precisely the answer the
// dashboard exists to obtain. ProbeDevice's error return is reserved for the
// probe itself not having been carried out.
type ProbeResult struct {
	// Playable is true only when aplay opened the device and played the
	// silence through it without complaint.
	Playable bool
	// TimedOut marks a device that never answered inside the deadline,
	// which is a different thing to say to an operator than a device that
	// refused: the first reads as "something is holding it", the second as
	// "nothing is attached to it". A caller who cancels the request its
	// context came from lands here too — the page it was for is gone by
	// then, so nothing is claimed about the device either way.
	TimedOut bool
	// Detail is aplay's own diagnostic, for the log. It is deliberately
	// not what the page shows: "Unknown error 524" is a sentence about a
	// kernel constant, and an operator standing at the board needs a
	// sentence about a cable.
	Detail string
}

// ProbeDevice reports whether device can be opened for playback, by opening
// it: a short silent WAV is written to a temp file and played through
// `aplay -q -D device`, which is the one recipe verified on the operator's
// own board.
//
// The silence is generated here rather than shipped as an asset or produced
// by ffmpeg. ffmpeg is optional on these devices, and a probe that needed it
// would fail on exactly the boards that most need probing — the freshly
// installed ones.
//
// `aplay --dump-hw-params /dev/null` is the obvious cheaper alternative and
// does not work: tried on the board, it exits 1 on a *working* device too,
// because aplay fails reading the WAV header before it ever reaches the
// card. It cannot tell the two apart, which is the only thing being asked.
//
// Nothing here is cached, and that is deliberate — see MixerControl, which
// does cache. The two questions differ in kind. Which simple control sets a
// card's volume is a property of the driver: it is the same answer for the
// life of the daemon, and asking twice per announcement was pure waste.
// Whether an output opens is a property of the world outside the box — a
// speaker plugged into the jack, an amplifier switched on at the far end of
// an HDMI cable, another process holding the card. A cached "this output is
// broken" would outlive the moment someone plugs the speaker in, and the
// page would go on telling them their working output does not work, which
// is the same class of bug as the one this probe was written to fix. The
// cost of not caching is one 100 ms silent playback per grouped row per page
// load, and a failing device answers instantly.
func (a ALSA) ProbeDevice(ctx context.Context, device string) (ProbeResult, error) {
	path, remove, err := writeSilentWAV(probeSilence)
	if err != nil {
		// The probe could not be carried out at all — a read-only or full
		// temp directory. This says nothing about the device, and callers
		// must not render it as if it did.
		return ProbeResult{}, fmt.Errorf("probe file: %w", err)
	}
	defer remove()

	probeCtx, cancel := context.WithTimeout(ctx, a.probeTimeout())
	defer cancel()

	playErr := a.Play(probeCtx, device, path)
	// Checked before playErr, and not merely as a classification of it: a
	// Run that returns success after its context expired would otherwise be
	// reported as a working device on the strength of an answer that came
	// too late to mean anything.
	if probeCtx.Err() != nil {
		return ProbeResult{TimedOut: true, Detail: probeCtx.Err().Error()}, nil
	}
	if playErr != nil {
		return ProbeResult{Detail: strings.TrimSpace(playErr.Error())}, nil
	}
	return ProbeResult{Playable: true}, nil
}

func (a ALSA) probeTimeout() time.Duration {
	if a.ProbeTimeout > 0 {
		return a.ProbeTimeout
	}
	return defaultProbeTimeout
}

// writeSilentWAV puts d worth of silence in a temp file and returns its path
// together with the function that removes it. The file is the probe's whole
// payload and has no business outliving the probe, so every return path of
// ProbeDevice runs remove.
func writeSilentWAV(d time.Duration) (path string, remove func(), err error) {
	f, err := os.CreateTemp("", "spoloh-probe-*.wav")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	cleanup := func() { os.Remove(name) }

	if _, err := f.Write(silentWAV(d)); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	// Closed before aplay is told about it, not deferred: a file still open
	// for writing is a file aplay may read short.
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}

// silentWAV builds a complete PCM WAV file of d worth of silence: the
// canonical 44-byte header followed by zero samples, which is what silence
// is in signed 16-bit PCM.
//
// Silence and not a tone, because this runs unannounced on a page load. A
// device whose entire job is to be heard must not make a noise nobody asked
// for; the operator has a button for that, and it plays the all-clear.
func silentWAV(d time.Duration) []byte {
	// Integer arithmetic throughout: d * rate / second is exact where
	// d.Seconds() * rate rounds, and a frame count short by one is a WAV
	// whose header disagrees with its body.
	frames := int(time.Duration(probeSampleRate) * d / time.Second)
	if frames < 1 {
		frames = 1
	}
	const blockAlign = probeChannels * probeBitsPerSample / 8
	dataLen := frames * blockAlign

	buf := make([]byte, wavHeaderLen+dataLen)
	copy(buf[0:], "RIFF")
	// Everything after this field: the 36 remaining header bytes plus the
	// samples.
	binary.LittleEndian.PutUint32(buf[4:], uint32(wavHeaderLen-8+dataLen))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16) // PCM fmt chunk length
	binary.LittleEndian.PutUint16(buf[20:], 1)  // format 1 = uncompressed PCM
	binary.LittleEndian.PutUint16(buf[22:], probeChannels)
	binary.LittleEndian.PutUint32(buf[24:], probeSampleRate)
	binary.LittleEndian.PutUint32(buf[28:], probeSampleRate*blockAlign)
	binary.LittleEndian.PutUint16(buf[32:], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:], probeBitsPerSample)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataLen))
	// The samples themselves are the zero value of the slice: silence.
	return buf
}

// Stream plays an internet stream into device until ctx is cancelled or the
// stream ends, which is what makes ALSA a Streamer. ffmpeg decodes and writes
// to the ALSA PCM itself, through the same names aplay takes, so the radio
// reaches whichever output /audio chose and nothing else.
//
// -protocol_whitelist is the safety catch: ffmpeg reads file:, pipe: and a
// dozen more, and a URL an operator typed must not reach the board's files.
// radio.ValidateURL refuses those on the form; this refuses them whatever
// got into config.json. The reconnect flags ride out a dropped connection
// inside one run, so the player's own backoff is for a station that is
// really gone; -rw_timeout ends one that stalls without dropping, which no
// reconnect notices and which would otherwise hold ffmpeg forever. The
// volume filter is the radio's level within the card's — see
// player.Background. -nostdin because a daemon's stdin is not ffmpeg's to
// read.
//
// Cancelling ctx kills ffmpeg, and Stream returns only once it has exited,
// so the PCM is released before the player hands the card to aplay for
// whatever cut the radio off.
func (a ALSA) Stream(ctx context.Context, device, url string, gain int) error {
	if device == "" {
		device = "default"
	}
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "http,https,tls,tcp,crypto",
		"-rw_timeout", "15000000",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "30",
		"-i", url,
		"-af", fmt.Sprintf("volume=%.2f", float64(max(0, min(gain, 100)))/100),
		"-f", "alsa", device,
	}
	var out []byte
	var err error
	if a.Run != nil {
		out, err = a.Run(ctx, a.ffmpeg(), args...)
	} else {
		// Not run(): CombinedOutput keeps everything a process prints, and
		// this one runs for hours. Only the end is kept, which is where the
		// reason it stopped is.
		var tail tailBuffer
		cmd := exec.CommandContext(ctx, a.ffmpeg(), args...)
		cmd.Stdout, cmd.Stderr = &tail, &tail
		// After a cancel kills ffmpeg, Wait also waits for the output to
		// drain; this bounds that, so a stuck pipe can never keep the
		// player — and whatever cut the radio off — waiting.
		cmd.WaitDelay = 2 * time.Second
		err = cmd.Run()
		out = []byte(tail.String())
	}
	if err != nil {
		return fmt.Errorf("ffmpeg -f alsa %s: %w: %s", device, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tailBuffer keeps the last limit bytes written to it, or the last
// tailBufferLimit when limit is zero.
type tailBuffer struct {
	limit int
	buf   []byte
}

const tailBufferLimit = 2048

func (t *tailBuffer) Write(b []byte) (int, error) {
	limit := t.limit
	if limit <= 0 {
		limit = tailBufferLimit
	}
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

func (a ALSA) ffmpeg() string {
	if a.FfmpegBin != "" {
		return a.FfmpegBin
	}
	return "ffmpeg"
}
