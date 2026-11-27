//go:build !unix

package main

import "os"

// keepOwner is a no-op where files have no Unix owner.
func keepOwner(string, os.FileInfo) {}
