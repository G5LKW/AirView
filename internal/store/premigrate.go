package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pre-migration backup (docs/perf/DB-EFFICIENCY-DESIGN.md §5.3).
//
// Profile.BackupBeforeMigrate (storage.backupBeforeMigrate):
//
//   - "auto" (default): before the first data-migration step, when free disk
//     space >= file size x 1.3 + 512 MiB, write
//     <data>/backup/<db>-pre-1.1-<UTC date>.db with VACUUM INTO on its own
//     read connection (a consistent snapshot; the writer is not blocked).
//     Otherwise log that no backup was made and migrate anyway: every step
//     is non-destructive, a crash never loses data.
//   - "always": no backup, no migration (the station keeps running on the
//     legacy layout, which every reader supports; state
//     "waiting_for_backup", re-checked every 10 minutes).
//   - "never": no backup.
//
// The backup is deleted 14 days after every data migration completed (by
// the hourly job "store.premig-backup"), and only a file this store wrote:
// its path is recorded in kv and must match the name pattern.

// backupKeep is how long the pre-migration backup is kept after the
// migrations completed.
const backupKeep = 14 * 24 * time.Hour

// BackupDirName is the directory (inside the data directory) of the
// pre-migration backups.
const BackupDirName = "backup"

var backupName = regexp.MustCompile(`^(airview|stations)-pre-1\.1-\d{8}(-\d+)?\.db$`)

func init() {
	registerHourly("store.premig-backup", expireBackup)
}

// backupBeforeMigrate takes the backup decision for this database (once;
// the decision is recorded in kv).
func (m *migrator) backupBeforeMigrate(ctx context.Context) error {
	s := m.s
	mode := s.opts.Profile.BackupBeforeMigrate
	if how, ok, err := s.KVGet(ctx, kvPreMigDecision); err != nil {
		return err
	} else if ok && how != "" {
		if p, ok, _ := s.KVGet(ctx, kvPreMigBackup); ok {
			m.mu.Lock()
			m.backup = p
			m.mu.Unlock()
		}
		return nil
	}
	switch mode {
	case "never":
		return s.KVSet(ctx, kvPreMigDecision, "never")
	case "always", "auto":
	default:
		mode = "auto"
	}
	size := s.DBSize()
	need := size*13/10 + 512<<20
	free, err := m.diskFree(s.dir)
	enough := err != nil || int64(free) >= need
	if !enough {
		if mode == "always" {
			s.log.Warn("store: data migration waits for its backup: not enough free disk space",
				"free_mb", free>>20, "needed_mb", need>>20)
			return errWaitBackup
		}
		s.log.Warn("store: no pre-migration backup (not enough free disk space); migrating anyway, every step is non-destructive",
			"free_mb", free>>20, "needed_mb", need>>20)
		return s.KVSet(ctx, kvPreMigDecision, "skipped")
	}
	path, err := s.BackupNow(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if mode == "always" {
			s.log.Warn("store: pre-migration backup failed; the migration waits", "err", err)
			return fmt.Errorf("%w: %v", errWaitBackup, err)
		}
		s.log.Warn("store: pre-migration backup failed; migrating anyway, every step is non-destructive", "err", err)
		return s.KVSet(ctx, kvPreMigDecision, "skipped")
	}
	m.mu.Lock()
	m.backup = path
	m.mu.Unlock()
	if err := s.KVSet(ctx, kvPreMigBackup, path); err != nil {
		return err
	}
	return s.KVSet(ctx, kvPreMigDecision, "written")
}

// BackupNow writes a consistent copy of the database to
// <data>/backup/<db>-pre-1.1-<UTC date>.db (VACUUM INTO on a separate
// connection; writers keep running) and returns its path. Used before the
// data migrations and by `airview db migrate --backup`.
func (s *Store) BackupNow(ctx context.Context) (string, error) {
	if s.closed.Load() {
		return "", ErrClosed
	}
	dir := filepath.Join(s.dir, BackupDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("store: backup: %w", err)
	}
	base := strings.TrimSuffix(s.kind.fileName(), ".db") + "-pre-1.1-" + time.Now().UTC().Format("20060102")
	var path string
	for n := 1; n < 1000; n++ {
		name := base + ".db"
		if n > 1 {
			name = base + "-" + strconv.Itoa(n) + ".db"
		}
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			path = p
			break
		}
	}
	if path == "" {
		return "", errors.New("store: backup: too many backup files today")
	}
	// A copy interrupted by a crash or power cut is left under the partial
	// name only (removed here on the next attempt), never under a backup
	// name: SQLite neither syncs the output of VACUUM INTO nor makes it
	// atomic, so the copy is synced and renamed before it counts.
	removeStalePartialBackups(dir, s.kind)
	tmp := path + partialSuffix
	q := baseValues(busyTimeoutMs)
	db, err := sql.Open("sqlite", dsn(s.path, q))
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	t0 := time.Now()
	// VACUUM INTO builds the copy in the target file; temp tables on disk
	// would double the I/O, so keep temp_store=MEMORY (default of q).
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store: backup: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store: backup: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store: backup: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // persists the rename where directories can be synced
		_ = d.Close()
	}
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	s.log.Info("store: pre-migration backup written", "path", path, "mb", size>>20, "took", time.Since(t0).Round(time.Millisecond))
	return path, nil
}

// partialSuffix marks a backup copy that is still being written.
const partialSuffix = ".partial"

// syncFile flushes a file's contents to stable storage.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// removeStalePartialBackups deletes copies of this database kind that an
// interrupted BackupNow left behind (only names this store writes:
// <db>-pre-1.1-<date>[-n].db.partial and their SQLite side files).
func removeStalePartialBackups(dir string, kind Kind) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := strings.TrimSuffix(kind.fileName(), ".db") + "-pre-1.1-"
	for _, e := range ents {
		name := e.Name()
		i := strings.Index(name, partialSuffix)
		if i < 0 || e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[i+len(partialSuffix):]
		if rest != "" && rest != "-journal" && rest != "-wal" && rest != "-shm" {
			continue
		}
		if !backupName.MatchString(name[:i]) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// expireBackup deletes the pre-migration backup 14 days after the data
// migrations completed (only the file this store recorded, matching the
// name pattern, inside <data>/backup).
func expireBackup(ctx context.Context, s *Store) error {
	return s.expireBackupAt(ctx, time.Now())
}

func (s *Store) expireBackupAt(ctx context.Context, now time.Time) error {
	path, ok, err := s.KVGet(ctx, kvPreMigBackup)
	if err != nil || !ok || path == "" {
		return err
	}
	doneAt, ok, err := kvInt(ctx, s.DB(), kvDataMigDoneAt)
	if err != nil || !ok {
		return err
	}
	if now.Sub(time.Unix(doneAt, 0)) < backupKeep {
		return nil
	}
	if filepath.Dir(path) != filepath.Join(s.dir, BackupDirName) || !backupName.MatchString(filepath.Base(path)) {
		s.log.Warn("store: not deleting an unexpected backup path", "path", path)
		return s.KVDelete(ctx, kvPreMigBackup)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.log.Info("store: pre-migration backup deleted (14 days after the migration)", "path", path)
	s.mig.mu.Lock()
	s.mig.backup = ""
	s.mig.mu.Unlock()
	return s.KVDelete(ctx, kvPreMigBackup)
}
