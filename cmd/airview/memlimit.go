package main

import (
	"log/slog"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"airview/internal/model"
)

// piMemoryLimit is the soft Go heap limit on Raspberry Pi class devices
// (linux/arm, linux/arm64). It is not a hard cap: above it the garbage
// collector runs more often and returns memory to the OS sooner, which keeps
// the resident set of a long-running station small after peaks (imports,
// terrain analysis, big report queries).
const piMemoryLimit = 384 << 20

// memoryLimitFor returns the soft memory limit to set for a platform, or 0
// for none: only on linux/arm and linux/arm64, and only when the user did
// not set GOMEMLIMIT (which the runtime already applies by itself).
func memoryLimitFor(goos, goarch, gomemlimit string) int64 {
	if gomemlimit != "" || goos != "linux" || (goarch != "arm" && goarch != "arm64") {
		return 0
	}
	return piMemoryLimit
}

// applyMemoryLimit sets the soft memory limit on small devices.
func applyMemoryLimit(log *slog.Logger) {
	if lim := memoryLimitFor(runtime.GOOS, runtime.GOARCH, os.Getenv("GOMEMLIMIT")); lim > 0 {
		debug.SetMemoryLimit(lim)
		log.Info("soft memory limit set", "MiB", lim>>20, "hint", "set GOMEMLIMIT to override")
	}
}

// cgroupMemoryMax is the cgroup v2 memory cap of the process (Linux).
const cgroupMemoryMax = "/sys/fs/cgroup/memory.max"

// readCgroupMax returns the content of memory.max ("" when unavailable).
func readCgroupMax() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile(cgroupMemoryMax)
	if err != nil {
		return ""
	}
	return string(b)
}

// cgroupSoftLimit returns 85 % of a cgroup memory cap ("max" or garbage =
// 0). Caps below 256 MiB are ignored (a soft limit that low would make the
// garbage collector run constantly).
func cgroupSoftLimit(memoryMax string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(memoryMax), 10, 64)
	if err != nil || v < 256<<20 {
		return 0
	}
	return v / 100 * 85
}

// readMemTotal returns the MemTotal line value of /proc/meminfo in bytes
// (0 when unavailable).
func readMemTotal() int64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemTotal(string(b))
}

// parseMemTotal parses "MemTotal:  16314172 kB".
func parseMemTotal(meminfo string) int64 {
	for line := range strings.Lines(meminfo) {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return 0
			}
			kb, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil || kb <= 0 {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}

// physSoftLimit is 80 % of the physical memory (0 below 1 GiB: a small
// machine keeps the default behaviour).
func physSoftLimit(total int64) int64 {
	if total < 1<<30 {
		return 0
	}
	return total / 100 * 80
}

// applyProcessConfig applies the "process" block once the configuration
// is loaded (SCALE-02, SCALE-10): log level, log size and rotation, and an
// explicit soft memory limit (GOMEMLIMIT wins).
func applyProcessConfig(log *slog.Logger, lf *rotatingFile, p model.ProcessConfig, aggregator bool) {
	setLogLevels(p)
	maxBytes, files := p.LogLimits(aggregator)
	if lf != nil {
		lf.setLimits(maxBytes, files)
	}
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	switch {
	case p.MemoryLimitMB > 0:
		debug.SetMemoryLimit(int64(p.MemoryLimitMB) << 20)
		log.Info("soft memory limit from process.memoryLimitMb", "MiB", p.MemoryLimitMB)
	case p.MemoryLimitMB == -1:
		debug.SetMemoryLimit(math.MaxInt64)
		log.Info("soft memory limit off (process.memoryLimitMb = -1)")
	}
}
