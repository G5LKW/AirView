//go:build windows

package ntp

import (
	"syscall"
	"time"
	"unsafe"
)

// On Windows, time.Now() reads the system time from the shared user page,
// which only advances once per timer tick (0.5–15.6 ms). That quantisation
// would limit every NTP measurement to millisecond accuracy, so the client
// uses GetSystemTimePreciseAsFileTime (Windows 8+, <1 µs resolution) for its
// T1/T4 timestamps and falls back to time.Now() if the call is unavailable.
var procPreciseTime = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemTimePreciseAsFileTime")

// windowsToUnix100ns is the number of 100 ns intervals between 1601-01-01
// (FILETIME epoch) and 1970-01-01 (Unix epoch).
const windowsToUnix100ns = 116444736000000000

// preciseNow returns the current system (wall) clock with the best available
// resolution. The result carries no monotonic reading.
func preciseNow() time.Time {
	if procPreciseTime.Find() != nil {
		return time.Now().Round(0)
	}
	var ft syscall.Filetime
	// The call has no return value and cannot fail once the procedure exists.
	// SyscallN (uintptrkeepalive) keeps ft on the stack; LazyProc.Call is
	// uintptrescapes and would allocate on every call (feeds stamp every
	// socket read and the tracker every frame with Now).
	_, _, _ = syscall.SyscallN(procPreciseTime.Addr(), uintptr(unsafe.Pointer(&ft)))
	v := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	if v <= windowsToUnix100ns {
		return time.Now().Round(0)
	}
	v -= windowsToUnix100ns
	return time.Unix(v/1e7, (v%1e7)*100)
}
