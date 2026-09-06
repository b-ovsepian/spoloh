package library_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spoloh/spoloh/internal/library"
)

// fakeConv simulates ffmpeg: whatever it is given, it always emits a
// conforming WAV (PCM 16-bit 44100 Hz stereo), exactly as the real FFmpeg
// converter does via its "-ac 2 -ar 44100 -acodec pcm_s16le" flags.
type fakeConv struct {
	available bool
	ran       int
}

func (f *fakeConv) Available() bool { return f.available }

func (f *fakeConv) ToWAV(_ context.Context, src, dst string) error {
	f.ran++
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, makeWAV(1, 2, 44100, 16, data), 0o644)
}

// crashingConv simulates a converter that is killed mid-write: it leaves
// some bytes at its destination and then reports failure. This is the
// regression scenario for finding 1: a non-atomic Add would leave that
// partial file sitting at the served path.
type crashingConv struct{}

func (crashingConv) Available() bool { return true }

func (crashingConv) ToWAV(_ context.Context, _, dst string) error {
	if err := os.WriteFile(dst, []byte("RIFF\x00\x00\x00\x00WAVEtruncated"), 0o644); err != nil {
		return err
	}
	return errors.New("simulated crash mid-conversion")
}

// makeWAV builds a minimal, structurally valid WAV file with the given
// format, so tests can construct both conforming and non-conforming uploads.
func makeWAV(formatTag, channels uint16, sampleRate uint32, bitsPerSample uint16, data []byte) []byte {
	blockAlign := channels * (bitsPerSample / 8)
	byteRate := sampleRate * uint32(blockAlign)

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, formatTag)
	binary.Write(&buf, binary.LittleEndian, channels)
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, bitsPerSample)

	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)

	return buf.Bytes()
}

// conformingWAV is PCM 16-bit 44100 Hz stereo: the only format the library
// accepts without running it through the converter.
func conformingWAV() []byte {
	return makeWAV(1, 2, 44100, 16, []byte("payload"))
}

func TestAddConvertsNonWAV(t *testing.T) {
	dir := t.TempDir()
	conv := &fakeConv{available: true}
	lib := library.New(dir, conv)

	name, err := lib.Add(context.Background(), "chime.mp3", bytes.NewReader([]byte("fake mp3")))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if name != "chime.wav" {
		t.Fatalf("name = %q, want chime.wav", name)
	}
	if conv.ran != 1 {
		t.Fatalf("converter ran %d times, want 1", conv.ran)
	}
	if !lib.Has("chime.wav") {
		t.Fatal("converted file should be in the library")
	}
}

// Converting at upload time means no decoding when an alert fires. On a Pi 2
// that difference is measurable, so a conforming WAV upload must also land
// as-is, without going through the converter.
func TestAddAcceptsWAVWithoutConverter(t *testing.T) {
	dir := t.TempDir()
	conv := &fakeConv{available: false}
	lib := library.New(dir, conv)

	name, err := lib.Add(context.Background(), "bell.wav", bytes.NewReader(conformingWAV()))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if name != "bell.wav" {
		t.Fatalf("name = %q, want bell.wav", name)
	}
	if conv.ran != 0 {
		t.Fatal("a conforming WAV upload must not be converted")
	}
}

func TestAddWithoutConverterRejectsMP3(t *testing.T) {
	lib := library.New(t.TempDir(), &fakeConv{available: false})

	_, err := lib.Add(context.Background(), "chime.mp3", bytes.NewReader([]byte("fake mp3")))
	if !errors.Is(err, library.ErrConversionUnavailable) {
		t.Fatalf("err = %v, want ErrConversionUnavailable", err)
	}
}

func TestAddRejectsNonRIFFClaimingToBeWAV(t *testing.T) {
	lib := library.New(t.TempDir(), &fakeConv{available: false})

	_, err := lib.Add(context.Background(), "lie.wav", bytes.NewReader([]byte("this is not audio at all")))
	if err == nil {
		t.Fatal("expected a rejection for a file that is not RIFF/WAVE")
	}
}

// A WAV container is not enough: the invariant the playback pipeline relies
// on is PCM 16-bit 44100 Hz stereo specifically. A WAV holding a different
// sample rate or channel count must go through the same converter path a
// non-WAV upload would.
func TestAddConvertsNonConformingWAV(t *testing.T) {
	dir := t.TempDir()
	conv := &fakeConv{available: true}
	lib := library.New(dir, conv)

	nonConforming := makeWAV(1, 1, 22050, 16, []byte("payload")) // mono, 22050 Hz
	name, err := lib.Add(context.Background(), "mono.wav", bytes.NewReader(nonConforming))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if name != "mono.wav" {
		t.Fatalf("name = %q, want mono.wav", name)
	}
	if conv.ran != 1 {
		t.Fatalf("converter ran %d times, want 1", conv.ran)
	}
}

// Without a converter, a non-conforming WAV cannot be normalised, so it must
// be rejected with an error that names what the file actually is.
func TestAddNonConformingWAVWithoutConverterNamesFormat(t *testing.T) {
	lib := library.New(t.TempDir(), &fakeConv{available: false})

	nonConforming := makeWAV(1, 1, 22050, 16, []byte("payload")) // mono, 22050 Hz
	_, err := lib.Add(context.Background(), "mono.wav", bytes.NewReader(nonConforming))
	if !errors.Is(err, library.ErrConversionUnavailable) {
		t.Fatalf("err = %v, want wrapping ErrConversionUnavailable", err)
	}
	if !strings.Contains(err.Error(), "22050") || !strings.Contains(err.Error(), "mono") {
		t.Fatalf("error should name the actual format, got: %v", err)
	}
}

// A WAV container holding a non-PCM codec (e.g. A-law) is routed to the
// converter exactly like a non-conforming PCM WAV: the format tag matters,
// not just the container.
func TestAddNonPCMWAVConvertsWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	conv := &fakeConv{available: true}
	lib := library.New(dir, conv)

	alaw := makeWAV(6, 2, 44100, 16, []byte("payload")) // A-law, format tag 6
	name, err := lib.Add(context.Background(), "alaw.wav", bytes.NewReader(alaw))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if name != "alaw.wav" {
		t.Fatalf("name = %q, want alaw.wav", name)
	}
	if conv.ran != 1 {
		t.Fatalf("converter ran %d times, want 1", conv.ran)
	}
}

func TestAddNonPCMWAVWithoutConverterNamesFormat(t *testing.T) {
	lib := library.New(t.TempDir(), &fakeConv{available: false})

	alaw := makeWAV(6, 2, 44100, 16, []byte("payload"))
	_, err := lib.Add(context.Background(), "alaw.wav", bytes.NewReader(alaw))
	if !errors.Is(err, library.ErrConversionUnavailable) {
		t.Fatalf("err = %v, want wrapping ErrConversionUnavailable", err)
	}
	if !strings.Contains(err.Error(), "A-law") {
		t.Fatalf("error should name the actual format, got: %v", err)
	}
}

// Regression test for finding 1: converting into a scratch file and only
// renaming it into place once validated means a converter that is killed
// mid-write cannot leave a broken file where Has/List/aplay would treat it
// as ready to play.
func TestAddConversionFailureLeavesNoDestinationFile(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, crashingConv{})

	_, err := lib.Add(context.Background(), "chime.mp3", bytes.NewReader([]byte("fake mp3")))
	if err == nil {
		t.Fatal("expected an error from a converter that crashes mid-write")
	}
	if lib.Has("chime.wav") {
		t.Fatal("a failed conversion must not leave a file at the destination")
	}
}

func TestAddSanitisesNames(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: true})

	name, err := lib.Add(context.Background(), "../../etc/passwd", bytes.NewReader([]byte("x")))
	if err != nil && !errors.Is(err, library.ErrBadName) {
		t.Fatalf("unexpected error: %v", err)
	}
	if err == nil {
		if strings.Contains(name, "/") || strings.Contains(name, "..") {
			t.Fatalf("name %q escaped the library directory", name)
		}
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
			t.Fatalf("sanitised file not written: %v", statErr)
		}
	}
}

func TestListAndRemove(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: false})

	if _, err := lib.Add(context.Background(), "a.wav", bytes.NewReader(conformingWAV())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := lib.Add(context.Background(), "b.wav", bytes.NewReader(conformingWAV())); err != nil {
		t.Fatalf("Add: %v", err)
	}

	sounds, err := lib.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sounds) != 2 || sounds[0].Name != "a.wav" || sounds[1].Name != "b.wav" {
		t.Fatalf("List = %+v, want a.wav then b.wav", sounds)
	}

	if err := lib.Remove("a.wav"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if lib.Has("a.wav") {
		t.Fatal("a.wav should be gone")
	}
}

func TestRemoveRejectsEscape(t *testing.T) {
	lib := library.New(t.TempDir(), &fakeConv{available: false})
	if err := lib.Remove("../config.json"); err == nil {
		t.Fatal("Remove must refuse a path outside the library")
	}
}

// The device must be able to sound an alert before anyone uploads anything.
func TestEnsureDefaults(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: false})

	if err := lib.EnsureDefaults(); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}
	for _, want := range []string{"siren.wav", "all-clear.wav", "metronome.wav"} {
		if !lib.Has(want) {
			t.Fatalf("%s missing after EnsureDefaults", want)
		}
	}
}

// A default the operator has replaced must not be overwritten on restart.
func TestEnsureDefaultsDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: false})
	if err := lib.EnsureDefaults(); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}

	custom := []byte("operator recording, definitely not the default siren")
	if err := os.WriteFile(filepath.Join(dir, "siren.wav"), custom, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := lib.EnsureDefaults(); err != nil {
		t.Fatalf("second EnsureDefaults: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "siren.wav"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, custom) {
		t.Fatal("EnsureDefaults overwrote an operator's own siren")
	}
}
