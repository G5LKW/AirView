//go:build windows

package cpustat

import (
	"time"

	"golang.org/x/sys/windows"
)

func ft(f windows.Filetime) time.Duration {
	return time.Duration((uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)) * 100)
}

func processCPU() time.Duration {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err != nil {
		return 0
	}
	return ft(k) + ft(u)
}
