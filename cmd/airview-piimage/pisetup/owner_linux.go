//go:build linux

package main

import (
	"os"
	"syscall"
)

// owner remembers a file's owner and group: "airview-feeder config set"
// replaces the file by a rename, which would leave it owned by root:root and
// unreadable for the feeder service (HOOKS.md: WP-PIIMG -> cmd/airview-feeder).
type owner struct {
	ok       bool
	uid, gid int
	mode     os.FileMode
}

func statOwner(path string) owner {
	st, err := os.Stat(path)
	if err != nil {
		return owner{}
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}
	}
	return owner{ok: true, uid: int(s.Uid), gid: int(s.Gid), mode: st.Mode().Perm()}
}

func (o owner) restore(path string) error {
	if !o.ok {
		return nil
	}
	if err := os.Chown(path, o.uid, o.gid); err != nil {
		return err
	}
	return os.Chmod(path, o.mode)
}
