//go:build !windows && !unix

package cpustat

import "time"

func processCPU() time.Duration { return 0 }
