package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"airview/internal/config"
	"airview/internal/model"
)

// Backup and restore (DESIGN §20.1).
//
// "airview backup" writes one ZIP with a consistent snapshot of everything
// that cannot be rebuilt:
//
//   - the databases airview.db and (aggregator) stations.db, copied with
//     SQLite's VACUUM INTO, which is safe while the server is running (WAL
//     readers do not block the writer, and the copy is one consistent
//     transaction);
//   - config.json and the operator file airview.jsonc when it lives in the
//     data directory (an operator file elsewhere, such as
//     /etc/airview/airview.jsonc, belongs to the system backup);
//   - the uplink identity and state (uplink/: identity.key, uuid,
//     per-uplink state; not the spool) and the aggregator's AVF TLS
//     certificate (avf-tls/), so feeders keep trusting a restored server
//     and an aggregator keeps recognising a restored feeder.
//
// Caches (map tiles, terrain, navigation data, weather), the log and the
// uplink spool are left out. MANIFEST.json lists every file with its size
// and SHA-256.
//
// "airview restore" checks the manifest, refuses while a server runs on
// the data directory, moves every file it replaces to
// <data>/pre-restore-<time>/ and extracts the archive. Restore never
// deletes anything.

// backupManifest is MANIFEST.json in the archive.
type backupManifest struct {
	Format  int            `json:"format"` // 1
	App     string         `json:"app"`
	Version string         `json:"version"`
	Created string         `json:"created"` // RFC 3339 UTC
	Mode    string         `json:"mode"`    // configured mode at backup time
	Files   []backupedFile `json:"files"`
}

type backupedFile struct {
	Path   string `json:"path"` // slash-separated, relative to the data directory
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

const (
	backupFormat   = 1
	manifestName   = "MANIFEST.json"
	registryDBName = "stations.db"
	storeDBName    = "airview.db"
)

// backupDatabases are snapshotted with VACUUM INTO.
var backupDatabases = []string{storeDBName, registryDBName}

// backupFiles are copied as they are (missing ones are skipped).
var backupFiles = []string{"config.json", "airview.jsonc", "sharelinks.json"}

// backupTrees are copied recursively, without the excluded sub-trees.
var backupTrees = []string{"uplink", "avf-tls"}

// backupExcluded reports whether a path inside a backup tree is left out.
func backupExcluded(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "spool" {
			return true
		}
	}
	return false
}

// dataDirFromFlag resolves the data directory like the server does.
func dataDirFromFlag(data string) (string, error) {
	if data == "" {
		data = os.Getenv(config.EnvDataDir)
	}
	dir, err := config.ResolveDataDir(data)
	if err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir, nil
}

// runBackup implements "airview backup".
func runBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	out := fs.String("o", "", "output `file` (default: airview-backup-<UTC time>.zip in the current directory; \"-\" = standard output)")
	tmpDir := fs.String("tmp", "", "`directory` for the database snapshots while the archive is written (default: the data directory; needs free space for a copy of airview.db and stations.db)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: airview backup [-data dir] [-o file.zip]\n\nWrites a consistent backup (databases, settings, uplink identity, AVF TLS certificate).\nIt is safe to run while AirView is running.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "data directory %s does not exist\n", dir)
		return 1
	}
	now := time.Now().UTC()
	target := *out
	if target == "" {
		target = "airview-backup-" + now.Format("20060102-150405") + ".zip"
	}
	var w io.Writer
	var f *os.File
	if target == "-" {
		w = stdout
	} else {
		f, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "backup file: %v\n", err)
			return 1
		}
		w = f
	}
	man, err := writeBackupTmp(context.Background(), dir, *tmpDir, w, now)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(target)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "backup failed: %v\n", err)
		return 1
	}
	if target != "-" {
		var total int64
		for _, bf := range man.Files {
			total += bf.Size
		}
		fmt.Fprintf(stdout, "Backup of %s written to %s (%d files, %d bytes before compression).\n", dir, target, len(man.Files), total)
	}
	return 0
}

// writeBackup writes the archive of dir to w, staging the database
// snapshots in dir.
func writeBackup(ctx context.Context, dir string, w io.Writer, now time.Time) (backupManifest, error) {
	return writeBackupTmp(ctx, dir, "", w, now)
}

// writeBackupTmp is writeBackup with the snapshots staged in tmpParent
// ("" = the data directory). Not the system temp directory: that is a
// RAM-backed tmpfs on current Debian and other systemd distributions, too
// small for a full VACUUM INTO copy of a busy aggregator's databases
// (SO-11). The data directory is on the same file system as the databases,
// which already have to fit there.
func writeBackupTmp(ctx context.Context, dir, tmpParent string, w io.Writer, now time.Time) (backupManifest, error) {
	man := backupManifest{Format: backupFormat, App: model.AppName, Version: version, Created: now.Format(time.RFC3339)}
	if cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: dir, ReadOnly: true}); err == nil {
		man.Mode = cfg.Mode
	}
	if tmpParent == "" {
		tmpParent = dir
	}
	tmp, err := os.MkdirTemp(tmpParent, ".backup-tmp-")
	if err != nil {
		return man, err
	}
	defer os.RemoveAll(tmp)

	zw := zip.NewWriter(w)
	add := func(rel, src string) error {
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		hdr := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: info.ModTime()}
		hdr.SetMode(0o600)
		zf, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(zf, h), in)
		if err != nil {
			return err
		}
		man.Files = append(man.Files, backupedFile{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	}

	for _, name := range backupDatabases {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		snap := filepath.Join(tmp, name)
		if err := snapshotDB(ctx, src, snap); err != nil {
			return man, fmt.Errorf("%s: %w", name, err)
		}
		if err := add(name, snap); err != nil {
			return man, fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, name := range backupFiles {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := add(name, src); err != nil {
			return man, fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, tree := range backupTrees {
		root := filepath.Join(dir, tree)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		var files []string
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if backupExcluded(rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				files = append(files, rel)
			}
			return nil
		})
		if err != nil {
			return man, fmt.Errorf("%s: %w", tree, err)
		}
		sort.Strings(files)
		for _, rel := range files {
			if err := add(rel, filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
				return man, fmt.Errorf("%s: %w", rel, err)
			}
		}
	}
	if len(man.Files) == 0 {
		return man, errors.New("nothing to back up (no database, settings or identity in the data directory)")
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return man, err
	}
	mw, err := zw.Create(manifestName)
	if err != nil {
		return man, err
	}
	if _, err := mw.Write(b); err != nil {
		return man, err
	}
	return man, zw.Close()
}

// sqliteDSN builds a modernc.org/sqlite DSN for path with a busy timeout.
func sqliteDSN(p string) string {
	q := url.Values{}
	q.Set("_busy_timeout", "10000")
	if !strings.ContainsRune(p, '?') {
		return p + "?" + q.Encode()
	}
	s := filepath.ToSlash(p)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	u := url.URL{Scheme: "file", Path: s}
	return u.String() + "?" + q.Encode()
}

// snapshotDB writes a consistent copy of the SQLite database src to dst
// (which must not exist) and checks the copy's integrity.
func snapshotDB(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", sqliteDSN(src))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	cp, err := sql.Open("sqlite", sqliteDSN(dst))
	if err != nil {
		return err
	}
	defer cp.Close()
	var res string
	if err := cp.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&res); err != nil {
		return fmt.Errorf("check snapshot: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("snapshot failed its integrity check: %s", res)
	}
	return nil
}

// runRestore implements "airview restore".
func runRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` to restore into (default: $AIRVIEW_DATA, else the default data directory)")
	force := fs.Bool("force", false, "restore even if the data directory looks in use (stale airview.run)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: airview restore [-data dir] [-force] backup.zip\n\nRestores a backup made by \"airview backup\". Stop AirView first. Files that are\nreplaced are moved to <data>/pre-restore-<time>/; nothing is deleted.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	// Refuse while a server holds the data directory.
	release, already := acquireInstance(dir)
	if already {
		fmt.Fprintf(stderr, "AirView is running with %s: stop it before restoring\n", dir)
		return 1
	}
	defer release()
	if info, ok := readInstanceInfo(dir); ok && processAlive(info.PID) && !*force {
		fmt.Fprintf(stderr, "AirView (pid %d) appears to be running with %s: stop it first (or use -force if that process is not AirView)\n", info.PID, dir)
		return 1
	}
	moved, n, err := restoreBackup(fs.Arg(0), dir, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "restore failed: %v\n", err)
		if moved != "" {
			fmt.Fprintf(stderr, "replaced files were moved to %s\n", moved)
		}
		return 1
	}
	fmt.Fprintf(stdout, "Restored %d files into %s.\n", n, dir)
	if moved != "" {
		fmt.Fprintf(stdout, "The files that were replaced are in %s.\n", moved)
	}
	return 0
}

// restoreBackup verifies the archive and extracts it into dir. It returns
// the directory the replaced files were moved to ("" = none) and the
// number of files restored.
func restoreBackup(archive, dir string, now time.Time) (string, int, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return "", 0, err
	}
	defer zr.Close()
	man, err := readManifest(&zr.Reader)
	if err != nil {
		return "", 0, err
	}
	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	// Verify everything before touching the data directory.
	for _, bf := range man.Files {
		if !safeRelPath(bf.Path) {
			return "", 0, fmt.Errorf("unsafe path %q in the archive", bf.Path)
		}
		zf := byName[bf.Path]
		if zf == nil {
			return "", 0, fmt.Errorf("%s is listed in the manifest but missing", bf.Path)
		}
		sum, n, err := hashZipFile(zf)
		if err != nil {
			return "", 0, fmt.Errorf("%s: %w", bf.Path, err)
		}
		if n != bf.Size || sum != bf.SHA256 {
			return "", 0, fmt.Errorf("%s does not match the manifest (damaged archive)", bf.Path)
		}
	}

	// Move aside what will be replaced (including the WAL and shared-memory
	// files of replaced databases, which belong to the old contents).
	asideDir := filepath.Join(dir, "pre-restore-"+now.Format("20060102-150405"))
	moved := ""
	moveAside := func(rel string) error {
		src := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Lstat(src); err != nil {
			return nil
		}
		dst := filepath.Join(asideDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		moved = asideDir
		return nil
	}
	for _, bf := range man.Files {
		if err := moveAside(bf.Path); err != nil {
			return moved, 0, err
		}
		if strings.HasSuffix(bf.Path, ".db") {
			for _, suf := range []string{"-wal", "-shm", "-journal"} {
				if err := moveAside(bf.Path + suf); err != nil {
					return moved, 0, err
				}
			}
		}
	}
	n := 0
	for _, bf := range man.Files {
		if err := extractFile(byName[bf.Path], filepath.Join(dir, filepath.FromSlash(bf.Path))); err != nil {
			return moved, n, fmt.Errorf("%s: %w", bf.Path, err)
		}
		n++
	}
	return moved, n, nil
}

func readManifest(zr *zip.Reader) (backupManifest, error) {
	var man backupManifest
	for _, f := range zr.File {
		if f.Name != manifestName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return man, err
		}
		defer rc.Close()
		dec := json.NewDecoder(io.LimitReader(rc, 4<<20))
		if err := dec.Decode(&man); err != nil {
			return man, fmt.Errorf("manifest: %w", err)
		}
		if man.Format != backupFormat || man.App != model.AppName {
			return man, fmt.Errorf("not an AirView backup (format %d, app %q)", man.Format, man.App)
		}
		if len(man.Files) == 0 {
			return man, errors.New("the backup is empty")
		}
		return man, nil
	}
	return man, errors.New("not an AirView backup (no " + manifestName + ")")
}

// safeRelPath accepts clean, relative, slash-separated paths inside the
// data directory only.
func safeRelPath(p string) bool {
	if p == "" || strings.Contains(p, "\\") || strings.Contains(p, ":") || path.IsAbs(p) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}

func hashZipFile(f *zip.File) (string, int64, error) {
	rc, err := f.Open()
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func extractFile(f *zip.File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dst + ".restore-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
