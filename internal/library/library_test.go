package library_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spoloh/spoloh/internal/library"
)

// fakeConv copies the source to the destination and records that it ran.
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
	return os.WriteFile(dst, append([]byte("RIFF....WAVE"), data...), 0o644)
}

func wavBytes() []byte {
	b := make([]byte, 64)
	copy(b, "RIFF")
	copy(b[8:], "WAVE")
	return b
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
// that difference is measurable, so a WAV upload must also land as a WAV.
func TestAddAcceptsWAVWithoutConverter(t *testing.T) {
	dir := t.TempDir()
	conv := &fakeConv{available: false}
	lib := library.New(dir, conv)

	name, err := lib.Add(context.Background(), "bell.wav", bytes.NewReader(wavBytes()))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if name != "bell.wav" {
		t.Fatalf("name = %q, want bell.wav", name)
	}
	if conv.ran != 0 {
		t.Fatal("a WAV upload must not be converted")
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

	if _, err := lib.Add(context.Background(), "a.wav", bytes.NewReader(wavBytes())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := lib.Add(context.Background(), "b.wav", bytes.NewReader(wavBytes())); err != nil {
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

	custom := append(wavBytes(), []byte("operator recording")...)
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
