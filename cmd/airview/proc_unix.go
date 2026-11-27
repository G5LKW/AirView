//go:build unix

package main

import "syscall"

// processAlive reports whether a process with this PID exists (signal 0).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
