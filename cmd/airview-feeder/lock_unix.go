//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// errLocked means another airview-feeder process runs with the same data
// directory (two processes with one identity would be refused by the
// aggregator as a cloned feeder).
var errLocked = errors.New("another airview-feeder is running with this data directory")

// lockDataDir takes an exclusive advisory lock on <dir>/feeder.lock for the
// life of the process. The returned function releases it.
func lockDataDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "feeder.lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// dataDirLocked reports whether a running process holds the lock.
func dataDirLocked(dir string) bool {
	release, err := lockDataDir(dir)
	if err != nil {
		return errors.Is(err, errLocked)
	}
	release()
	return false
}
