//go:build windows

package main

import "golang.org/x/sys/windows"

// processCPU returns the CPU time (user + kernel) of this process in seconds.
func processCPU() float64 {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err != nil {
		return 0
	}
	ft := func(f windows.Filetime) float64 {
		return float64(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) / 1e7
	}
	return ft(k) + ft(u)
}
