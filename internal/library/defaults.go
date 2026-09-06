package library

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed assets/*.wav
var assets embed.FS

// DefaultNames are the sounds shipped inside the binary.
var DefaultNames = []string{"siren.wav", "all-clear.wav", "metronome.wav"}

// EnsureDefaults writes any missing built-in sound to disk. It never overwrites
// an existing file: an operator who replaced the siren with their own recording
// must not lose it on the next restart.
func (l *Library) EnsureDefaults() error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	for _, name := range DefaultNames {
		dst := filepath.Join(l.dir, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
