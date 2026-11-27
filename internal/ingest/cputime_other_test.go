//go:build !windows && !unix

package ingest

import "time"

// processCPU is unknown on this platform (the load test skips the report).
func processCPU() time.Duration { return 0 }
