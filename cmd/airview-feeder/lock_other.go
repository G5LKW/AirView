//go:build !unix

package main

import "errors"

// errLocked: see lock_unix.go.
var errLocked = errors.New("another airview-feeder is running with this data directory")

// lockDataDir is a no-op on systems without flock (Windows: the service
// manager runs one instance; the status page port catches a second one).
func lockDataDir(string) (func(), error) { return func() {}, nil }

// dataDirLocked cannot tell without flock; callers also probe the status
// page.
func dataDirLocked(string) bool { return false }
