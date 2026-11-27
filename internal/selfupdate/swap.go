package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Suffixes of the files placed next to the program file.
const (
	suffixNew    = ".new"    // staged copy on the program's file system
	suffixOld    = ".old"    // previous binary (rename-aside / hard link)
	suffixFailed = ".failed" // a rolled-back binary, kept for diagnosis
	suffixDel    = ".del"    // could not be deleted yet (running image on Windows)
)

// swapStrategy selects how the new binary is put in place.
type swapStrategy int

const (
	// strategyRenameAside renames the running program to .old and the new
	// file to the program name. Required on Windows, where a running
	// image cannot be replaced or deleted but can be renamed.
	strategyRenameAside swapStrategy = iota
	// strategyLinkReplace hard-links the program to .old and renames the
	// new file over the program name: on POSIX the program path never
	// disappears, and rename(2) is atomic.
	strategyLinkReplace
)

func defaultStrategy() swapStrategy {
	if runtime.GOOS == "windows" {
		return strategyRenameAside
	}
	return strategyLinkReplace
}

// stageNext copies the verified download next to exe as exe+".new" with
// the mode (and on POSIX the owner) of exe, and fsyncs it.
func stageNext(exe, verified string) (string, error) {
	info, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	next := exe + suffixNew
	removeOrDefer(next)
	if err := copyFile(verified, next, info.Mode().Perm()|0o100); err != nil {
		os.Remove(next)
		return "", fmt.Errorf("stage %s: %w", next, err)
	}
	chownLike(next, info)
	return next, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

// swapIn puts next (from stageNext) in place of exe and keeps the previous
// binary as exe+".old". On error the program file is unchanged.
func swapIn(exe, next string, st swapStrategy) (old string, err error) {
	old = exe + suffixOld
	removeOrDefer(old)
	switch st {
	case strategyLinkReplace:
		if err := os.Link(exe, old); err != nil {
			info, serr := os.Stat(exe)
			if serr != nil {
				return "", serr
			}
			if cerr := copyFile(exe, old, info.Mode().Perm()); cerr != nil {
				os.Remove(old)
				return "", fmt.Errorf("keep previous binary: %v / %v", err, cerr)
			}
			chownLike(old, info)
		}
		if err := os.Rename(next, exe); err != nil {
			os.Remove(old)
			return "", fmt.Errorf("replace %s: %w", exe, err)
		}
	default:
		if err := os.Rename(exe, old); err != nil {
			return "", fmt.Errorf("rename %s aside: %w", exe, err)
		}
		if err := os.Rename(next, exe); err != nil {
			if rerr := os.Rename(old, exe); rerr != nil {
				return "", fmt.Errorf("place new binary: %v; RESTORE FAILED: %v (the previous binary is %s)", err, rerr, old)
			}
			return "", fmt.Errorf("place new binary: %w", err)
		}
	}
	syncDir(filepath.Dir(exe))
	return old, nil
}

// errNothingToRestore: the previous binary is gone and the program file is
// not the previous version either.
var errNothingToRestore = errors.New("the previous binary is missing; cannot roll back")

// restoreOld puts the previous binary back. It is idempotent: when exe
// already has fromSHA (another process rolled back first) it does nothing.
// The rolled-back binary is kept as exe+".failed".
func restoreOld(exe, old, fromSHA string, st swapStrategy) error {
	if fromSHA != "" {
		if h, err := fileSHA256(exe); err == nil && h == fromSHA {
			return nil
		}
	}
	if _, err := os.Stat(old); err != nil {
		return errNothingToRestore
	}
	if fromSHA != "" {
		if h, err := fileSHA256(old); err != nil || h != fromSHA {
			return fmt.Errorf("%s does not hold the previous version (sha %s, want %s)", old, h, fromSHA)
		}
	}
	failed := exe + suffixFailed
	removeOrDefer(failed)
	switch st {
	case strategyLinkReplace:
		if err := os.Link(exe, failed); err != nil {
			_ = err // keeping the failed binary is best effort
		}
		if err := os.Rename(old, exe); err != nil {
			return fmt.Errorf("restore %s: %w", exe, err)
		}
	default:
		if _, err := os.Stat(exe); err == nil {
			if err := os.Rename(exe, failed); err != nil {
				return fmt.Errorf("move the failed binary aside: %w", err)
			}
		}
		if err := os.Rename(old, exe); err != nil {
			if rerr := os.Rename(failed, exe); rerr != nil {
				return fmt.Errorf("restore %s: %v; and the failed binary could not be put back: %v", exe, err, rerr)
			}
			return fmt.Errorf("restore %s: %w", exe, err)
		}
	}
	syncDir(filepath.Dir(exe))
	return nil
}

// removeOrDefer deletes path; when that is impossible (a running image on
// Windows) it renames it to path.<unix-ns>.del, removed by cleanupDeferred
// later. A missing file is not an error.
func removeOrDefer(path string) {
	if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = os.Rename(path, path+"."+strconv.FormatInt(time.Now().UnixNano(), 10)+suffixDel)
}

// cleanupDeferred removes exe.*.del files and a stale exe.new.
func cleanupDeferred(exe string) {
	dir, base := filepath.Split(exe)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, base+".") && strings.HasSuffix(n, suffixDel) {
			os.Remove(filepath.Join(dir, n))
		}
	}
	os.Remove(exe + suffixNew)
}

// ExecutablePath returns the resolved path of the running program (symbolic
// links followed, so the real file is swapped, not the link).
func ExecutablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

// samePath reports whether a and b name the same program file: equal after
// cleaning (case-insensitively on Windows), or the same file on disk (an
// 8.3 short name, a different spelling of the same directory).
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb || (runtime.GOOS == "windows" && strings.EqualFold(ca, cb)) {
		return true
	}
	ia, err := os.Stat(ca)
	if err != nil {
		return false
	}
	ib, err := os.Stat(cb)
	return err == nil && os.SameFile(ia, ib)
}

// CheckWritable reports whether the updater can replace exe: the directory
// must allow creating and renaming files. It creates and removes a probe
// file next to exe.
func CheckWritable(exe string) error {
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, ".selfupdate-probe-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (run the update as an administrator/root, or let the package manager or installer update this program): %w", dir, err)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
