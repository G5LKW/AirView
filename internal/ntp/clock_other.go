//go:build !windows

package ntp

import "time"

// preciseNow returns the current system (wall) clock without a monotonic
// reading. On Linux time.Now() already has nanosecond resolution (vDSO
// clock_gettime).
func preciseNow() time.Time {
	return time.Now().Round(0)
}
