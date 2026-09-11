// Package library stores the sounds the device can play. Uploads are converted
// to WAV on arrival so that playback never has to decode anything: when an
// alert fires the device does the least work possible.
package library

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrConversionUnavailable = errors.New("ffmpeg is not installed, so only WAV uploads are accepted")
	ErrBadName               = errors.New("invalid sound name")
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// The prefixes of the two scratch files this package writes into the library
// directory. Both are dot-prefixed so an operator listing the directory over
// SSH sees them for what they are, and List skips them by these names rather
// than by "starts with a dot": an upload genuinely called ".chime.wav" is an
// odd choice but it is the operator's, and it must still show up.
const (
	uploadPrefix  = ".upload-"
	convertPrefix = ".convert-"
)

func isStagingName(name string) bool {
	return strings.HasPrefix(name, uploadPrefix) || strings.HasPrefix(name, convertPrefix)
}

type Sound struct {
	Name    string
	Size    int64
	ModTime time.Time
	// Duration is how long the sound plays. It is only meaningful when
	// DurationKnown is true: a header that will not parse, or one with a
	// byte rate of zero to divide by, leaves Duration at its zero value on
	// purpose, so that a caller who forgets to check DurationKnown renders
	// something obviously wrong (0:00) rather than something plausible and
	// false.
	Duration      time.Duration
	DurationKnown bool
}

// Converter turns an arbitrary audio file into a WAV. The contract is "write
// a WAV to dst" — an implementation must not make that conditional on how dst
// happens to be spelled, because the caller picks that name (see convert).
type Converter interface {
	Available() bool
	ToWAV(ctx context.Context, src, dst string) error
}

// RunFunc executes a command and returns its combined output. Nil means the
// real thing.
//
// It is the same seam internal/player's ALSA uses, for the same reason: the
// test suite must never run ffmpeg — it is not installed on a development
// machine, it is slow, and a test that shells out to it is testing the
// operator's board rather than this package. Capturing the argv is the only
// way anything here gets a test.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// FFmpeg is the production Converter.
type FFmpeg struct {
	Bin string // default "ffmpeg"
	// Run replaces the exec of ffmpeg. Nil means the real thing.
	Run RunFunc
}

func (f FFmpeg) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if f.Run != nil {
		return f.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (f FFmpeg) bin() string {
	if f.Bin != "" {
		return f.Bin
	}
	return "ffmpeg"
}

func (f FFmpeg) Available() bool {
	_, err := exec.LookPath(f.bin())
	return err == nil
}

// ToWAV names its output format explicitly with "-f wav". Without it ffmpeg
// chooses the muxer from the destination's file extension, and convert hands
// it a scratch name that has none, so every non-WAV upload failed on real
// hardware with:
//
//	Unable to choose an output format for '/tmp/.convert-oypAfQuy'; use a
//	standard extension for the filename or specify the format manually.
//
// The Converter contract is "write a WAV to dst" — honouring it must not
// depend on how the caller spelled dst.
func (f FFmpeg) ToWAV(ctx context.Context, src, dst string) error {
	out, err := f.run(ctx, f.bin(),
		"-y", "-i", src,
		"-ac", "2", "-ar", "44100", "-acodec", "pcm_s16le",
		"-f", "wav",
		dst,
	)
	if err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, out)
	}
	return nil
}

type Library struct {
	dir  string
	conv Converter
}

func New(dir string, conv Converter) *Library {
	return &Library{dir: dir, conv: conv}
}

// Path returns the on-disk path of a sound, or "" if the name is unsafe.
func (l *Library) Path(name string) string {
	clean, err := sanitise(name)
	if err != nil {
		return ""
	}
	return filepath.Join(l.dir, clean)
}

func (l *Library) Has(name string) bool {
	p := l.Path(name)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func (l *Library) List() ([]Sound, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Sound
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wav") {
			continue
		}
		// Skip the half-written files this package stages in the library
		// directory itself (it stages there so that the publishing rename
		// stays on one filesystem). convert's scratch file now ends in
		// ".wav", so without this an upload being transcoded would appear
		// as a playable sound on /sounds and in the /events dropdowns for
		// the several seconds ffmpeg takes, then vanish when it is renamed
		// into place.
		if isStagingName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		snd := Sound{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()}
		// A file that will not parse (mid-transcode wreckage that dodged
		// isStagingName, an operator's own recording that is not really a
		// WAV) still belongs in the list — Name/Size/ModTime came straight
		// from the directory entry and owe parseWAV nothing. It only loses
		// its duration, which renders as unknown rather than sinking the
		// whole row.
		if format, err := parseWAV(filepath.Join(l.dir, e.Name())); err == nil {
			snd.Duration, snd.DurationKnown = format.Duration()
		}
		out = append(out, snd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (l *Library) Remove(name string) error {
	p := l.Path(name)
	if p == "" {
		return ErrBadName
	}
	return os.Remove(p)
}

// Add stores an uploaded file, converting it to WAV when needed. It returns the
// stored name, which always ends in .wav.
func (l *Library) Add(ctx context.Context, filename string, r io.Reader) (string, error) {
	base := filepath.Base(filename)
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.TrimSuffix(base, filepath.Ext(base))

	target, err := sanitise(stem + ".wav")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(l.dir, uploadPrefix+"*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	dst := filepath.Join(l.dir, target)

	if ext == ".wav" {
		format, err := parseWAV(tmpName)
		if err != nil {
			return "", err
		}
		if format.conforms() {
			if err := os.Rename(tmpName, dst); err != nil {
				return "", err
			}
			return target, nil
		}
		// A WAV container that is not PCM 16-bit 44.1 kHz stereo still needs
		// the same codec normalisation a non-WAV upload would get: aplay
		// must never be the thing that discovers the format is wrong.
		if !l.conv.Available() {
			return "", fmt.Errorf("%s is %s, but the library requires PCM 16-bit 44100 Hz stereo: %w", base, format.describe(), ErrConversionUnavailable)
		}
		if err := l.convert(ctx, tmpName, dst); err != nil {
			return "", err
		}
		return target, nil
	}

	if !l.conv.Available() {
		return "", ErrConversionUnavailable
	}
	if err := l.convert(ctx, tmpName, dst); err != nil {
		return "", err
	}
	return target, nil
}

// convert runs the converter into a scratch file inside the library
// directory and only publishes the result to dst, via rename, once it has
// been validated as a complete WAV. A converter killed mid-write — context
// cancellation, a crash, a full disk — then leaves nothing at dst for
// Has/List/aplay to trip over.
func (l *Library) convert(ctx context.Context, src, dst string) error {
	// The ".wav" suffix is defence in depth, not the fix: os.CreateTemp
	// substitutes its random string at the LAST "*", so this yields
	// ".convert-abc123.wav". FFmpeg.ToWAV now states "-f wav" and no longer
	// needs the hint, but any future Converter that is less explicit would
	// need it, and the scratch file is at least honest about what is in it.
	tmp, err := os.CreateTemp(l.dir, convertPrefix+"*.wav")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	defer os.Remove(tmpName)

	if err := l.conv.ToWAV(ctx, src, tmpName); err != nil {
		return err
	}
	format, err := parseWAV(tmpName)
	if err != nil {
		return fmt.Errorf("converter produced an invalid WAV: %w", err)
	}
	// The conforming-shape invariant is a property of what lands in the
	// library, not of one Converter implementation's command-line flags: a
	// different backend, a misconfigured FFmpeg.Bin, or any other Converter
	// could emit a structurally valid but non-conforming WAV, so it is
	// checked here rather than trusted.
	if !format.conforms() {
		return fmt.Errorf("converter produced %s, but the library requires PCM 16-bit 44100 Hz stereo", format.describe())
	}
	return os.Rename(tmpName, dst)
}

// checkStoredWAV validates a file that is already sitting in the library. It
// is stricter than parseWAV alone: as well as requiring a header that holds
// together, it requires the file to be at least as long as its own RIFF header
// says it is. That second check is the one that matters here — a power cut
// partway through writing a 15 MB default leaves a file whose header parses
// perfectly well and whose audio simply stops halfway.
//
// A file LONGER than its declared size passes: trailing chunks are legal and
// common, and being wrong in that direction would delete an operator's own
// recording. Uploads deliberately do not go through this check; Add's contract
// is about the declared format, and a stricter reading of RIFF sizes there
// would reject the streamed-WAV encoders that write a placeholder length.
func checkStoredWAV(path string) error {
	if _, err := parseWAV(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var riff [8]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return fmt.Errorf("not a WAV file: %w", err)
	}
	declared := int64(binary.LittleEndian.Uint32(riff[4:8])) + 8
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < declared {
		return fmt.Errorf("truncated WAV: %d bytes on disk, the header declares %d", info.Size(), declared)
	}
	return nil
}

// wavFormat is the audio format declared by a WAV file's fmt chunk, plus
// enough of the data chunk's own shape for Duration to compute playback
// length by arithmetic rather than a decode.
type wavFormat struct {
	FormatTag     uint16
	Channels      uint16
	SampleRate    uint32
	BitsPerSample uint16
	// ByteRate is the fmt chunk's own declared average bytes per second. It
	// is read from the file rather than recomputed from the fields above so
	// that a file whose header lies about it is a duration-unknown case
	// (see Duration), not a silently different number than what the header
	// says.
	ByteRate uint32

	// dataBytes is how much audio is actually there to play: the smaller of
	// what the data chunk's header declares and what is actually left on
	// disk after it. A power cut partway through writing a file leaves a
	// header that still claims the original size — see checkStoredWAV's own
	// comment on the same failure — so trusting the declared size here would
	// report a duration longer than the audio that exists to play it.
	dataBytes int64
	// hasData reports whether a "data" chunk was found at all. Its absence
	// is as good a reason to call the duration unknown as a header that does
	// not parse — see Duration.
	hasData bool
}

// requiredFormat is the only format the playback pipeline is guaranteed to
// handle without decoding: PCM 16-bit 44.1 kHz stereo (spec §5.4). Add
// accepts a WAV upload as-is only when it already matches this exactly;
// anything else is routed through the converter like a non-WAV upload.
var requiredFormat = wavFormat{FormatTag: 1, Channels: 2, SampleRate: 44100, BitsPerSample: 16}

func (f wavFormat) conforms() bool {
	// Field by field, not "f == requiredFormat": wavFormat also carries the
	// data chunk's own shape (ByteRate, dataBytes, hasData), which varies
	// with every real file and has nothing to do with whether the codec is
	// the one the playback pipeline requires. A whole-struct comparison
	// would make every conforming upload fail this check.
	return f.FormatTag == requiredFormat.FormatTag &&
		f.Channels == requiredFormat.Channels &&
		f.SampleRate == requiredFormat.SampleRate &&
		f.BitsPerSample == requiredFormat.BitsPerSample
}

// Duration reports how long this WAV plays, and whether that could be
// determined at all. Playback never decodes anything — uploads are
// normalised to PCM on arrival — so this is arithmetic on how many bytes of
// audio there are and how fast the format plays them, using dataBytes
// exactly as parseWAV clamped it: never more than what the file on disk
// actually holds.
//
// The division is integer, deliberately: a byte rate of zero (a malformed
// or hand-crafted header) must be caught here and reported as unknown, not
// fed to a division that panics the page listing every sound in the
// library.
func (f wavFormat) Duration() (time.Duration, bool) {
	if !f.hasData {
		return 0, false
	}
	if f.ByteRate == 0 {
		return 0, false
	}
	return time.Duration(f.dataBytes) * time.Second / time.Duration(f.ByteRate), true
}

// describe renders the format for an operator-facing error message.
func (f wavFormat) describe() string {
	return fmt.Sprintf("%s, %s, %d Hz, %d-bit", formatTagName(f.FormatTag), channelWord(f.Channels), f.SampleRate, f.BitsPerSample)
}

func channelWord(channels uint16) string {
	switch channels {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	default:
		return fmt.Sprintf("%d channels", channels)
	}
}

func formatTagName(tag uint16) string {
	switch tag {
	case 1:
		return "PCM"
	case 2:
		return "ADPCM"
	case 3:
		return "IEEE float"
	case 6:
		return "A-law"
	case 7:
		return "mu-law"
	case 0xFFFE:
		return "extensible"
	default:
		return fmt.Sprintf("format tag %d", tag)
	}
}

// parseWAV reads a WAV file's RIFF/WAVE header and walks its chunks to find
// "fmt ", from which it reports the declared audio format. This is also the
// package's WAV-validity check: a file whose header or chunk structure does
// not hold together is rejected here, before it is ever trusted as playable.
//
// It keeps walking past "fmt " to look for "data" too, on the same open
// file, so that Duration has what it needs without a second pass over the
// file: this is the one place the whole package reads a WAV's header, and
// List calls it once per sound on every page load.
func parseWAV(path string) (wavFormat, error) {
	f, err := os.Open(path)
	if err != nil {
		return wavFormat{}, err
	}
	defer f.Close()

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return wavFormat{}, fmt.Errorf("not a WAV file: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return wavFormat{}, errors.New("not a WAV file: missing RIFF/WAVE header")
	}

	var format wavFormat
	haveFormat := false

	for {
		var head [8]byte
		if _, err := io.ReadFull(f, head[:]); err != nil {
			if !haveFormat {
				return wavFormat{}, errors.New("not a WAV file: missing fmt chunk")
			}
			// EOF with the format already in hand and no "data" chunk ever
			// seen: the file is a valid WAV, it just has nothing to compute
			// a duration from. hasData stays false and Duration reports
			// unknown — that is not this function's error to raise.
			return format, nil
		}
		id := string(head[0:4])
		size := binary.LittleEndian.Uint32(head[4:8])

		if id == "fmt " && !haveFormat {
			// A real fmt chunk is 16 (PCM), 18 (extensible with a zero
			// extension) or 40 bytes (WAVE_FORMAT_EXTENSIBLE). Cap well
			// above that, before allocating: a malformed or hostile upload
			// declaring a huge chunk size must not be able to force a
			// large allocation. On the 32-bit ARM boards this daemon
			// targets, that can panic (makeslice: len out of range) or
			// throw a fatal OOM that bypasses panic/recover entirely.
			const maxFmtChunkSize = 64
			if size < 16 {
				return wavFormat{}, errors.New("not a WAV file: fmt chunk too short")
			}
			if size > maxFmtChunkSize {
				return wavFormat{}, fmt.Errorf("not a WAV file: fmt chunk size %d exceeds the maximum expected size (%d bytes)", size, maxFmtChunkSize)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return wavFormat{}, fmt.Errorf("not a WAV file: truncated fmt chunk: %w", err)
			}
			format = wavFormat{
				FormatTag:     binary.LittleEndian.Uint16(body[0:2]),
				Channels:      binary.LittleEndian.Uint16(body[2:4]),
				SampleRate:    binary.LittleEndian.Uint32(body[4:8]),
				ByteRate:      binary.LittleEndian.Uint32(body[8:12]),
				BitsPerSample: binary.LittleEndian.Uint16(body[14:16]),
			}
			haveFormat = true
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return wavFormat{}, fmt.Errorf("not a WAV file: malformed %q chunk: %w", id, err)
				}
			}
			continue
		}

		if id == "data" && haveFormat {
			// The offset here is the first byte of audio: what is actually
			// left on disk from this point is info.Size()-pos, and Duration
			// must never be handed more than that, truncated-default-file
			// or not. Any error from here on leaves the format itself
			// intact and simply gives up on a duration — that is a detail
			// this function reports as "unknown", not a reason to fail
			// something that has already proven itself a valid WAV.
			pos, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return format, nil
			}
			info, err := f.Stat()
			if err != nil {
				return format, nil
			}
			available := info.Size() - pos
			if available < 0 {
				available = 0
			}
			declared := int64(size)
			if available < declared {
				format.dataBytes = available
			} else {
				format.dataBytes = declared
			}
			format.hasData = true
			return format, nil
		}

		skip := int64(size)
		if size%2 == 1 {
			skip++ // RIFF chunks are padded to an even number of bytes.
		}
		if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
			if !haveFormat {
				return wavFormat{}, fmt.Errorf("not a WAV file: malformed %q chunk: %w", id, err)
			}
			return format, nil
		}
	}
}

func sanitise(name string) (string, error) {
	clean := filepath.Base(strings.TrimSpace(name))
	if clean == "" || clean == "." || clean == ".." {
		return "", ErrBadName
	}
	if !safeName.MatchString(clean) {
		return "", ErrBadName
	}
	if !strings.HasSuffix(strings.ToLower(clean), ".wav") {
		return "", ErrBadName
	}
	return clean, nil
}
