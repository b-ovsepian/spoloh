// Package library stores the sounds the device can play. Uploads are converted
// to WAV on arrival so that playback never has to decode anything: when an
// alert fires the device does the least work possible.
package library

import (
	"context"
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

type Sound struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Converter turns an arbitrary audio file into a WAV.
type Converter interface {
	Available() bool
	ToWAV(ctx context.Context, src, dst string) error
}

// FFmpeg is the production Converter.
type FFmpeg struct {
	Bin string // default "ffmpeg"
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

func (f FFmpeg) ToWAV(ctx context.Context, src, dst string) error {
	out, err := exec.CommandContext(ctx, f.bin(),
		"-y", "-i", src,
		"-ac", "2", "-ar", "44100", "-acodec", "pcm_s16le",
		dst,
	).CombinedOutput()
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
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Sound{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
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

	tmp, err := os.CreateTemp(l.dir, ".upload-*")
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
		if err := checkRIFF(tmpName); err != nil {
			return "", err
		}
		if err := os.Rename(tmpName, dst); err != nil {
			return "", err
		}
		return target, nil
	}

	if !l.conv.Available() {
		return "", ErrConversionUnavailable
	}
	if err := l.conv.ToWAV(ctx, tmpName, dst); err != nil {
		return "", err
	}
	return target, nil
}

// checkRIFF rejects a file that claims to be a WAV but is not, so that aplay
// never fails at the moment an alert fires.
func checkRIFF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("not a WAV file: %w", err)
	}
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return errors.New("not a WAV file: missing RIFF/WAVE header")
	}
	return nil
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
