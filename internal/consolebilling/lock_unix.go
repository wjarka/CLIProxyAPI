//go:build !windows

package consolebilling

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// lockStore serializes the read/compare/atomic-rename commit across processes.
// It never covers provider network requests. The empty lock file contains no secrets.
func lockStore(dir string) (func(), error) {
	path := filepath.Join(dir, ".billing.lock")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, failure("storage_error")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, failure("storage_error")
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, failure("storage_busy")
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}
