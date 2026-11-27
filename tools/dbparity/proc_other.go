//go:build !windows

package main

import (
	"io"
	"os"
	"os/exec"
	"syscall"
)

func ensureConsole() {}

// launchCommand starts AirView directly in its own process group.
func launchCommand(exe string, args []string, log io.Writer) (*exec.Cmd, func() (int, error), error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, func() (int, error) { return cmd.Process.Pid, nil }, nil
}

// interruptProcess sends SIGTERM (the systemd stop signal).
func interruptProcess(_ *os.Process, pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func helperMain([]string) bool { return false }
