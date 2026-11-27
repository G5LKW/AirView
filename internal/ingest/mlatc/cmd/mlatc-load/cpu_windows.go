package main

import "golang.org/x/sys/windows"

// processCPU returns the process's user + kernel CPU time in seconds.
func processCPU() float64 {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err != nil {
		return 0
	}
	ft := func(f windows.Filetime) float64 {
		return float64(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100e-9
	}
	return ft(k) + ft(u)
}
