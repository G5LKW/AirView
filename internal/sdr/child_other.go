//go:build !windows

package sdr

import "os/exec"

func configureChild(cmd *exec.Cmd) {}
