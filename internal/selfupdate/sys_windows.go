//go:build windows

package selfupdate

import "os"

// syncDir is a no-op on Windows (NTFS metadata is journaled; directories
// cannot be opened for FlushFileBuffers without backup privileges).
func syncDir(string) {}

// chownLike is a no-op on Windows: the new file inherits the directory ACL,
// which is what the installer configured.
func chownLike(string, os.FileInfo) {}
