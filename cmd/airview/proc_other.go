//go:build !unix && !windows

package main

// processAlive cannot tell on this platform: assume the process is gone.
func processAlive(pid int) bool { return false }
