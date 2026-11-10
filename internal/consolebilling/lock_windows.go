//go:build windows

package consolebilling

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func lockStore(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".billing.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, failure("storage_error")
	}
	var overlapped windows.Overlapped
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, failure("storage_busy")
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped); _ = f.Close() }, nil
}
