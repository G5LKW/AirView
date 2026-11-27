//go:build !windows

package selfupdate

import (
	"os"
	"syscall"
)

// syncDir fsyncs a directory so renames inside it are durable.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// chownLike gives path the owner and group of info (best effort; only
// root can change owners, and then the copy must match the original).
func chownLike(path string, info os.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
