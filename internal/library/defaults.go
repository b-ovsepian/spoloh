package library

import (
	"embed"
	"os"
	"path/filepath"

	"github.com/spoloh/spoloh/internal/atomicfile"
)

//go:embed assets/*.wav
var assets embed.FS

// DefaultNames are the sounds shipped inside the binary.
var DefaultNames = []string{"siren.wav", "all-clear.wav", "metronome.wav"}

// EnsureDefaults writes any missing or unusable built-in sound to disk.
//
// It never overwrites a file that is a usable WAV: an operator who replaced the
// siren with their own recording must not lose it on the next restart. It does
// replace one that is not, because the alternative is worse. These files are
// about 15 MB in total and are written on first boot, so a power cut — the
// scenario the spec's success criteria name explicitly — can leave a truncated
// siren.wav behind; checking only for existence meant EnsureDefaults skipped it
// forever, Has() reported it usable, and the engine handed the wreckage to
// aplay at every single alert.
//
// Each write is atomic, so a power cut during this loop can only ever leave the
// previous file or the complete new one — never a third, truncated thing.
func (l *Library) EnsureDefaults() error {
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	for _, name := range DefaultNames {
		dst := filepath.Join(l.dir, name)
		if err := checkStoredWAV(dst); err == nil {
			continue
		}
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		if err := atomicfile.Write(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
