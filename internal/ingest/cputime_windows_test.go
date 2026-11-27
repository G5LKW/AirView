//go:build windows

package ingest

import (
	"syscall"
	"time"
)

// processCPU returns the CPU time (user + kernel) this process has used, for
// the load test's CPU report (0 = unknown).
func processCPU() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0
	}
	ft := func(f syscall.Filetime) time.Duration {
		return time.Duration((int64(f.HighDateTime)<<32 | int64(f.LowDateTime)) * 100)
	}
	return ft(k) + ft(u)
}
