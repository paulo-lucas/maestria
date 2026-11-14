// Package privfile writes and checks files that must be readable only by the current user.
//
// On POSIX systems directories are 0700 and files 0600. On Windows the mode bits are not
// meaningful (NTFS ACLs inherited from the user profile protect %AppData%), so the checks are
// skipped there.
package privfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// EnsureDir creates dir (and parents) and restricts it to the owner on POSIX.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// WriteAtomic writes data to path via a temp file in the same directory and a rename,
// with 0600 permissions on POSIX. The directory is created (0700) when missing.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			cleanup()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// RestrictBestEffort sets 0600 on path on POSIX, ignoring errors (e.g. a file that does not exist yet).
func RestrictBestEffort(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	_ = os.Chmod(path, 0o600)
}

// ErrLoosePermissions is wrapped by Check when a private file is readable by others.
var ErrLoosePermissions = errors.New("file permissions are too open")

// Check refuses a file whose group/other permission bits are set (POSIX only).
func Check(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has permissions %s; it must be 600 (run: chmod 600 %s)",
			ErrLoosePermissions, path, strconv.FormatUint(uint64(perm), 8), path)
	}
	return nil
}
