//go:build windows

package sdr

import (
	"os/exec"
	"syscall"
)

// configureChild keeps rtl_tcp from opening a console window when AirView
// runs as a tray application or a service.
func configureChild(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000 // CREATE_NO_WINDOW (process creation flags)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
