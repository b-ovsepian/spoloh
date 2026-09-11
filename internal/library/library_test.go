package library_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/library"
)

// fakeConv simulates ffmpeg: whatever it is given, it emits a conforming WAV
// (PCM 16-bit 44100 Hz stereo), exactly as the real FFmpeg converter does via
// its "-ac 2 -ar 44100 -acodec pcm_s16le" flags — but only if the destination
// it is handed is named like a WAV.
//
// That last clause is the whole lesson of the extensionless-scratch-file bug.
// Without it this fake was MORE PERMISSIVE THAN THE REAL THING: ffmpeg picks
// its output muxer from the destination's extension and refuses to run at all
// without one ("Unable to choose an output format for '/tmp/.convert-oypAfQuy'"),
// while this fake happily wrote a WAV to any name at all. Every non-WAV upload
// had therefore failed on every real device since the feature shipped — a
// total, 100%-reproducible failure — and the suite stayed green the entire
// time, because the one property of ffmpeg that broke was the one property the
// fake did not model. A fake that accepts more than the real thing does not
// test the code; it tests the fake.
type fakeConv struct {
	available bool
	ran       int
}

func (f *fakeConv) Available() bool { return f.available }

func (f *fakeConv) ToWAV(_ context.Context, src, dst string) error {
	f.ran++
	if !strings.HasSuffix(dst, ".wav") {
		return fmt.Errorf("unable to choose an output format for %q: use a standard extension for the filename or specify the format manually", dst)
	}
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

	// A complete WAV, because that is what an operator's own recording is.
	// EnsureDefaults now validates before it skips, so the thing being pinned
	// here is "a usable file is left alone", not "any bytes are left alone" —
	// see TestEnsureDefaultsReplacesATruncatedDefault for the other half.
	custom := makeWAV(1, 2, 44100, 16, []byte("operator recording, not the default siren"))
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

// The defaults are about 15 MB written on first boot, so a power cut leaves a
// truncated one behind — the exact scenario the spec's success criteria name.
// Checking only for existence meant it was skipped forever, Has() called it
// usable, and every alert handed the wreckage to aplay.
func TestEnsureDefaultsReplacesATruncatedDefault(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: false})
	if err := lib.EnsureDefaults(); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}
	path := filepath.Join(dir, "siren.wav")
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Cut it off mid-audio: the RIFF and fmt headers survive intact, which is
	// what a power cut during a sequential write actually leaves.
	truncated := full[:len(full)/2]
	if err := os.WriteFile(path, truncated, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := lib.EnsureDefaults(); err != nil {
		t.Fatalf("second EnsureDefaults: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("a truncated default was not healed: %d bytes on disk, want the full %d", len(got), len(full))
	}
}

// A default that is not a WAV at all — a zero-length file from a failed write,
// say — is wreckage too, and must be replaced rather than served.
func TestEnsureDefaultsReplacesAFileThatIsNotAWAV(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: false})
	path := filepath.Join(dir, "siren.wav")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := lib.EnsureDefaults(); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("a zero-length siren.wav survived EnsureDefaults")
	}
}

// A malformed upload must be rejected by a cheap, explicit size check before
// its declared fmt-chunk length is ever used for an allocation. On a 32-bit
// ARM board, a few-hundred-megabyte (let alone multi-gigabyte) allocation
// forced by a hostile or corrupt file can panic or OOM the daemon outright,
// bypassing panic/recover entirely.
func TestAddRejectsOversizedFmtChunk(t *testing.T) {
	lib := library.New(t.TempDir(), &fakeConv{available: false})

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(999999)) // arbitrary; not validated
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	// Declare a huge fmt chunk with no such data actually following. Chosen
	// large enough to be an obvious attack (a real fmt chunk is 16, 18 or 40
	// bytes) while staying cheap enough to allocate that the RED run of this
	// test against the pre-fix code does not itself stress the test runner.
	binary.Write(&buf, binary.LittleEndian, uint32(100_000_000))

	_, err := lib.Add(context.Background(), "huge.wav", bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected rejection of a WAV declaring an oversized fmt chunk")
	}
	// "exceeds" only appears in the capped-size rejection message; the old,
	// uncapped code fails later with a different ("truncated fmt chunk")
	// message after already having sized the allocation off the file.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error should reject the chunk on its declared size, got: %v", err)
	}
}

// nonConformingConv always succeeds but emits a WAV in the wrong format,
// modeling a Converter backend that does not guarantee the required shape
// the way FFmpeg's hard-coded flags do (a different backend, a misconfigured
// FFmpeg{Bin: ...} wrapper, or any other Converter implementation).
type nonConformingConv struct{}

func (nonConformingConv) Available() bool { return true }

func (nonConformingConv) ToWAV(_ context.Context, _, dst string) error {
	return os.WriteFile(dst, makeWAV(1, 1, 22050, 16, []byte("payload")), 0o644) // mono, 22050 Hz
}

// The conforming-shape invariant is a property of what ends up on disk, not
// of one Converter implementation's command line: convert must re-check its
// own output and refuse to publish a structurally valid but non-conforming
// WAV, whatever produced it.
func TestAddRejectsNonConformingConverterOutput(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, nonConformingConv{})

	_, err := lib.Add(context.Background(), "chime.mp3", bytes.NewReader([]byte("fake mp3")))
	if err == nil {
		t.Fatal("expected rejection of non-conforming converter output")
	}
	if !strings.Contains(err.Error(), "22050") || !strings.Contains(err.Error(), "mono") {
		t.Fatalf("error should name the format the converter actually produced, got: %v", err)
	}
	if lib.Has("chime.wav") {
		t.Fatal("non-conforming converter output must not be published to the library")
	}
}

// ToWAV must state its output format on the command line rather than leaving
// ffmpeg to infer one from the destination's file extension: convert names
// that file, and the contract ToWAV implements is "write a WAV to dst",
// whatever dst is called. ffmpeg itself is never run here — the RunFunc seam
// captures the argv instead, which is the only thing this can assert without
// the binary the suite is forbidden to invoke.
func TestFFmpegToWAVNamesTheOutputFormatExplicitly(t *testing.T) {
	var got []string
	conv := library.FFmpeg{
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		},
	}

	// Deliberately extensionless, exactly as the scratch file used to be.
	const dst = "/var/lib/spoloh/sounds/.convert-oypAfQuy"
	if err := conv.ToWAV(context.Background(), "/tmp/upload.mp3", dst); err != nil {
		t.Fatalf("ToWAV: %v", err)
	}

	format := indexOf(got, "-f")
	if format < 0 || format+1 >= len(got) || got[format+1] != "wav" {
		t.Fatalf(`argv does not pass "-f wav": %q`, got)
	}
	target := indexOf(got, dst)
	if target < 0 {
		t.Fatalf("argv does not name the destination: %q", got)
	}
	// ffmpeg reads "-f" as applying to the file that follows it, so the flag
	// after the destination would configure nothing.
	if format > target {
		t.Fatalf(`"-f wav" must come before the destination: %q`, got)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// With no Run injected the converter must still shell out for real — the seam
// is for tests, not a behaviour switch, and a nil-Run FFmpeg is what the
// daemon actually constructs. Asserting on the error rather than a success is
// what keeps this from running ffmpeg: the binary named here cannot exist.
func TestFFmpegWithoutARunFuncExecsTheBinary(t *testing.T) {
	conv := library.FFmpeg{Bin: filepath.Join(t.TempDir(), "no-such-ffmpeg")}
	err := conv.ToWAV(context.Background(), "/tmp/upload.mp3", "/tmp/out.wav")
	if err == nil {
		t.Fatal("expected an exec error from a binary that does not exist")
	}
	if !strings.Contains(err.Error(), "ffmpeg:") {
		t.Fatalf("error should come from ToWAV's own wrapping, got: %v", err)
	}
}

// convert stages its output in the library directory, and that scratch file
// now ends in ".wav" so ffmpeg can never be left guessing a muxer. List must
// therefore skip it by name: otherwise an mp3 being transcoded shows up as a
// playable sound on /sounds and in the /events dropdowns for the seconds the
// transcode takes, then disappears when it is renamed into place.
func TestListSkipsAConversionInProgress(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, &fakeConv{available: false})

	if _, err := lib.Add(context.Background(), "bell.wav", bytes.NewReader(conformingWAV())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".convert-oypAfQuy.wav"), conformingWAV(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sounds, err := lib.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sounds) != 1 || sounds[0].Name != "bell.wav" {
		t.Fatalf("List = %+v, want only bell.wav", sounds)
	}
}
