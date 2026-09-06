// Package atomicfile writes files so that a power cut can never leave a
// half-written one behind. The device this daemon runs on loses mains power
// without warning, and every file it owns — the config, the alert state, the
// default sounds — is useless if it can be found truncated on the next boot.
// One helper, one set of rules, rather than three packages each getting the
// sequence slightly differently right.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write stores data at path durably.
//
// The sequence is: write a temporary file in the same directory (the same
// directory so that the rename below is within one filesystem and therefore
// atomic), fsync it so the bytes are on the medium and not merely in the page
// cache, rename it over path, then fsync the directory so the rename itself
// survives the cut. Interrupted at any point, path is either absent or the
// complete previous contents — never a mixture of the two.
//
// A failure leaves path untouched and removes the temporary file.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// os.CreateTemp always makes the file 0600; anything more permissive has
	// to be asked for explicitly.
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir flushes a directory entry. Without it the rename can be lost even
// though the file's own contents were fsynced.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
