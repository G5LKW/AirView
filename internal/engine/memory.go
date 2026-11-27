package engine

import (
	"runtime"
	"runtime/debug"
	"time"
)

// releaseMemory returns freed heap to the operating system after a big job
// (navigation data import, terrain analysis, aircraft database import), so
// the resident set of a small device (Raspberry Pi) shrinks back instead of
// staying at the job's peak until the runtime's background scavenger gets
// to it. debug.FreeOSMemory forces a GC; call it only after rare jobs.
func (e *Engine) releaseMemory(job string) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	t := time.Now()
	debug.FreeOSMemory()
	runtime.ReadMemStats(&after)
	e.log.Debug("memory released", "job", job, "heapSysMB", mb(after.HeapSys), "heapReleasedMB", mb(after.HeapReleased),
		"releasedNowMB", mb(after.HeapReleased-min(after.HeapReleased, before.HeapReleased)), "ms", time.Since(t).Milliseconds())
}

func mb(b uint64) float64 { return float64(b>>10) / 1024 }
