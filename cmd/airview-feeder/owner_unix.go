//go:build unix

package main

import (
	"os"
	"syscall"
)

// keepOwner gives the new configuration file the owner and group of the
// one it replaces (run as root, `config set` would otherwise turn the
// installer's root:airview-feeder 0640 into root:root and the service
// could no longer read its configuration).
func keepOwner(name string, old os.FileInfo) {
	if st, ok := old.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(name, int(st.Uid), int(st.Gid))
	}
}
