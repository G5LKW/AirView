//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The setup program is Windows-only; this keeps "go build ./..." and
// "go vet ./..." working on every platform. Installers are assembled on
// any platform with ./cmd/airview-setup/mkinstaller.
func main() {
	fmt.Fprintln(os.Stderr, "airview-setup is the Windows installer stub; build it with GOOS=windows (see deploy/installer/README.md)")
	os.Exit(1)
}
