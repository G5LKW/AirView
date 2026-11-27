//go:build unix

package ingest

import (
	"syscall"
	"time"
)

// processCPU returns the CPU time (user + system) this process has used, for
// the load test's CPU report (0 = unknown).
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
