//go:build !windows && !linux

package main

import (
	"errors"
	"os"
)

// Other platforms read passwords with echo (no terminal control here).
func isTerminal(*os.File) bool { return false }

func noEcho(*os.File) (func(), error) { return nil, errors.New("not supported") }
