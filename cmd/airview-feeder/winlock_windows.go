package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockDataDirWin is the Windows counterpart of lock_unix.go's lockDataDir:
// an exclusive LockFileEx lock on <dir>\feeder.lock for the life of the
// process, so the service and a tray-hosted feeder (or two services) can
// never run with one identity (the aggregator would refuse the second as a
// clone). The service and the tray use it directly; lock_other.go still
// makes the generic lockDataDir a no-op on Windows (see HOOKS.md, WP-WF).
func lockDataDirWin(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "feeder.lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	// Windows byte-range locks are mandatory for other handles only; the
	// owner may still write the PID (for people looking at the file).
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		f.Close()
	}, nil
}

// dataDirLockedWin reports whether another process holds the lock.
func dataDirLockedWin(dir string) bool {
	release, err := lockDataDirWin(dir)
	if err != nil {
		return errors.Is(err, errLocked)
	}
	release()
	return false
}
