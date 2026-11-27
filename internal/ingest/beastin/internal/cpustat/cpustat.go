// Package cpustat reads the CPU time used by the current process, for the
// beastin load test and load tool.
package cpustat

import (
	"runtime"
	"time"
)

// Sample is a point-in-time reading.
type Sample struct {
	Wall       time.Time
	CPU        time.Duration // user + kernel time of the whole process
	HeapMB     float64       // heap in use
	SysMB      float64       // memory obtained from the OS by the Go runtime
	Goroutines int           // goroutines
}

// Take reads the current values.
func Take() Sample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return Sample{
		Wall:       time.Now(),
		CPU:        processCPU(),
		HeapMB:     float64(ms.HeapInuse) / (1 << 20),
		SysMB:      float64(ms.Sys) / (1 << 20),
		Goroutines: runtime.NumGoroutine(),
	}
}

// CPUPercent returns the CPU used between a and b as a percentage of one
// core (200 = two cores busy).
func CPUPercent(a, b Sample) float64 {
	wall := b.Wall.Sub(a.Wall)
	if wall <= 0 {
		return 0
	}
	return 100 * float64(b.CPU-a.CPU) / float64(wall)
}
