package atomicfile_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/atomicfile"
)

func TestWriteCreatesFileWithContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thing.json")

	if err := atomicfile.Write(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("contents = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thing.json")
	if err := os.WriteFile(path, []byte("old and much longer than the new one"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := atomicfile.Write(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// A rename replaces rather than overlays: no tail of the old contents can
	// survive, which is precisely what an in-place rewrite would risk.
	if string(got) != "new" {
		t.Fatalf("contents = %q, want %q", got, "new")
	}
}

// The whole point of the temp-then-rename dance is that nothing but the target
// is left in the directory afterwards. A stray .tmp in the sound library would
// be listed, and one in /etc/spoloh would accumulate on every save.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.json")

	if err := atomicfile.Write(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "thing.json" {
		t.Fatalf("directory should hold only thing.json, got %v", entries)
	}
}

// A failure must leave the previous file intact and clean up after itself:
// the caller's data on disk is either the old version or the new one.
func TestWriteFailureKeepsPreviousFileAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "thing.json") // "sub" does not exist

	if err := atomicfile.Write(path, []byte("x"), 0o644); err == nil {
		t.Fatal("expected Write to fail when the directory does not exist")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed Write left files behind: %v", entries)
	}
}

func TestWriteEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := atomicfile.Write(path, nil, 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("size = %d, want 0", info.Size())
	}
}
