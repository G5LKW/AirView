//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func isTerminal(f *os.File) bool {
	var m uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &m) == nil
}

// noEcho switches console echo off and returns a function restoring it.
func noEcho(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	var m uint32
	if err := windows.GetConsoleMode(h, &m); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(h, (m&^windows.ENABLE_ECHO_INPUT)|windows.ENABLE_LINE_INPUT); err != nil {
		return nil, err
	}
	return func() { _ = windows.SetConsoleMode(h, m) }, nil
}
