package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Modes of bundle entries (see doc.go).
const (
	modeDir  = 0o755
	modeExec = 0o755
	modeFile = 0o644
)

// isExecContent reports whether a file with this beginning is executable on
// Linux: an ELF binary or a script with a "#!" line.
func isExecContent(head []byte) bool {
	return bytes.HasPrefix(head, []byte("\x7fELF")) || bytes.HasPrefix(head, []byte("#!"))
}

// readHead returns the first n bytes of a file (fewer when it is shorter).
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	k, err := io.ReadFull(f, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:k], nil
}

// mtimeDefault is the time stamp of bundle entries: SOURCE_DATE_EPOCH when
// set (reproducible builds), else now.
func mtimeDefault() (time.Time, error) {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return time.Time{}, fmt.Errorf("invalid SOURCE_DATE_EPOCH %q", s)
		}
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Now().UTC().Truncate(time.Second), nil
}

func cmdPack(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("pack", stderr)
	dir := fs.String("dir", "", "staging directory; its base name is the archive's top-level directory")
	out := fs.String("o", "", "output .tar.gz")
	mt := fs.Int64("mtime", -1, "time stamp of every entry (Unix seconds; default SOURCE_DATE_EPOCH or now)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *dir == "" || *out == "" {
		return usageError{errors.New("-dir and -o are required")}
	}
	var when time.Time
	if *mt >= 0 {
		when = time.Unix(*mt, 0).UTC()
	} else {
		var err error
		if when, err = mtimeDefault(); err != nil {
			return err
		}
	}
	n, err := Pack(*dir, *out, when)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "packed %s (%d entries)\n", *out, n)
	return nil
}

type packEntry struct {
	rel  string // slash path below the top-level directory ("" = the directory itself)
	path string
	dir  bool
}

// Pack writes dir as a gzip-compressed tar file at out (see doc.go for the
// rules) and returns the number of entries. The archive is written to a
// temporary file first and renamed into place.
func Pack(dir, out string, when time.Time) (int, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	if !st.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", dir)
	}
	top := filepath.Base(filepath.Clean(dir))
	if !validName(top) {
		return 0, fmt.Errorf("top-level directory name %q is not allowed", top)
	}
	var ents []packEntry
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: symbolic links are not packed", p)
		case d.IsDir():
		case d.Type().IsRegular():
		default:
			return fmt.Errorf("%s: not a regular file or directory", p)
		}
		if rel != "" {
			for _, part := range strings.Split(rel, "/") {
				if !validName(part) {
					return fmt.Errorf("%s: name %q is not allowed in a bundle (white space or special characters)", p, part)
				}
			}
		}
		ents = append(ents, packEntry{rel: rel, path: p, dir: d.IsDir()})
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".relkit-*.tar.gz")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	gz, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		return 0, err
	}
	gz.ModTime = time.Time{}
	gz.Name = ""
	tw := tar.NewWriter(gz)
	for _, e := range ents {
		name := top
		if e.rel != "" {
			name += "/" + e.rel
		}
		hdr := &tar.Header{Name: name, ModTime: when, Uid: 0, Gid: 0, Uname: "root", Gname: "root", Format: tar.FormatPAX}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			hdr.Mode = modeDir
			if err := tw.WriteHeader(hdr); err != nil {
				return 0, err
			}
			continue
		}
		if err := writeFileEntry(tw, hdr, e.path); err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	// Windows cannot rename over an existing file that is open elsewhere; a
	// stale archive from an earlier build is simply replaced.
	_ = os.Remove(out)
	if err := os.Rename(tmpName, out); err != nil {
		return 0, err
	}
	ok = true
	return len(ents), nil
}

func writeFileEntry(tw *tar.Writer, hdr *tar.Header, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	head := make([]byte, 4)
	k, _ := io.ReadFull(f, head)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hdr.Typeflag = tar.TypeReg
	hdr.Size = st.Size()
	hdr.Mode = modeFile
	if isExecContent(head[:k]) {
		hdr.Mode = modeExec
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != st.Size() {
		return fmt.Errorf("%s changed while it was packed", path)
	}
	return nil
}

// validName accepts the file names a bundle may contain: letters, digits and
// ._+-@ (no white space, quotes, shell or glob characters), not "." or "..".
func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._+-@", r):
		default:
			return false
		}
	}
	return true
}
