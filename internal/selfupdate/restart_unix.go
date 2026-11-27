//go:build !windows

package selfupdate

import (
	"context"
	"fmt"
	"syscall"
)

// Restart implements Restarter: execve(2) of the program file.
func (r SelfRestarter) Restart(context.Context) error {
	args, env := r.argsEnv()
	if r.Before != nil {
		r.Before()
	}
	err := syscall.Exec(r.Exe, args, env)
	return fmt.Errorf("exec %s: %w", r.Exe, err)
}

// processAlive reports whether pid exists (signal 0).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
