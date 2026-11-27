package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"airview/internal/config"
	"airview/internal/engine"
	"airview/internal/model"
	"airview/internal/store"
)

// dbUsage lists the "airview db" subcommands.
const dbUsage = `Usage:
  airview db snapshot [-data dir] -o outdir      consistent copies of every *.db (safe while running)
  airview db profile  [-data dir] [-config file] [-set path=value]... [-json]
                                                 the storage profile in force and its values
  airview db migrate  [-data dir] --status | --backup | --now [-json]
                                                 storage-layout migration: progress, backup, run now
  airview db vacuum   [-data dir] [-config file]   rewrite every database compactly (AirView stopped)`

// runDB implements "airview db <subcommand>": "snapshot" (consistent
// copies of every SQLite database of the data directory, VACUUM INTO, safe
// while the server runs, for backup scripts that package the files
// themselves: deploy/server/backup.sh --online), "profile" (the resolved
// storage profile) and "migrate" (the migration to the current storage
// layout).
func runDB(args []string, stdout, stderr io.Writer) int {
	usage := dbUsage
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "snapshot":
		return runDBSnapshot(args[1:], stdout, stderr)
	case "profile":
		return runDBProfile(args[1:], stdout, stderr, os.Environ())
	case "migrate":
		return runDBMigrate(args[1:], stdout, stderr)
	case "vacuum":
		return runDBVacuum(args[1:], stdout, stderr)
	case "-h", "--help", "-help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown db command %q\n%s\n", args[0], usage)
	return 2
}

func runDBSnapshot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview db snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	out := fs.String("o", "", "output `directory` (must exist; one <name>.db per database, files must not exist yet)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: airview db snapshot [-data dir] -o outdir\n\nWrites a consistent copy of every SQLite database (*.db) of the data directory\ninto outdir with VACUUM INTO. Safe while AirView is running.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || *out == "" {
		fs.Usage()
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	if st, err := os.Stat(*out); err != nil || !st.IsDir() {
		fmt.Fprintf(stderr, "output directory %s does not exist\n", *out)
		return 1
	}
	n, err := snapshotAll(context.Background(), dir, *out)
	if err != nil {
		fmt.Fprintf(stderr, "snapshot failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%d database(s) of %s copied to %s\n", n, dir, *out)
	return 0
}

// snapshotAll copies every *.db file of dir into out. A partial copy is
// removed on failure.
func snapshotAll(ctx context.Context, dir, out string) (int, error) {
	dbs, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return 0, err
	}
	sort.Strings(dbs)
	n := 0
	for _, src := range dbs {
		if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
			continue
		}
		dst := filepath.Join(out, filepath.Base(src))
		if _, err := os.Stat(dst); err == nil {
			return n, fmt.Errorf("%s already exists", dst)
		}
		if err := snapshotDB(ctx, src, dst); err != nil {
			_ = os.Remove(dst)
			return n, fmt.Errorf("%s: %w", filepath.Base(src), err)
		}
		n++
	}
	return n, nil
}

// dbProfileJSON is one database of "airview db profile -json".
type dbProfileJSON struct {
	DB        string            `json:"db"`
	Profile   store.ProfileView `json:"profile"`
	Overrides []string          `json:"overrides"`
}

// runDBProfile implements "airview db profile": the storage profile each
// database of the data directory runs with under the effective
// configuration (the same resolution as the server: storage.profile, the
// platform, storage.tuning.*). It changes nothing.
func runDBProfile(args []string, stdout, stderr io.Writer, environ []string) int {
	fs := flag.NewFlagSet("airview db profile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	cfgFile := fs.String("config", "", "operator config `file` (default: $AIRVIEW_CONFIG, else airview.jsonc in the data directory)")
	var sets []string
	fs.Func("set", "override one setting, `path=value` (repeatable)", func(v string) error {
		if !strings.Contains(v, "=") {
			return fmt.Errorf("-set %q: want path=value", v)
		}
		sets = append(sets, v)
		return nil
	})
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: airview db profile [-data dir] [-config file] [-set path=value]... [-json]\n\n"+
			"Prints the storage profile (storage.profile: standard, low-wear or server) each database\n"+
			"runs with, why \"auto\" picked it, and its values after the storage.tuning overrides.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: dir, File: *cfgFile, Environ: environ, Sets: sets, ReadOnly: true})
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v (run \"airview config check\")\n", err)
		return 1
	}
	out := dbProfiles(cfg, dir)
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{"dataDir": dir, "databases": out}, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Data directory: %s\n", dir)
	for _, d := range out {
		writeProfile(stdout, d)
	}
	return 0
}

// dbProfiles resolves the profile of airview.db and, in aggregator mode,
// of stations.db.
func dbProfiles(cfg model.Config, dir string) []dbProfileJSON {
	agg := cfg.Mode == model.ModeAggregator
	kinds := []struct {
		name string
		kind store.Kind
	}{{"airview.db", store.KindMain}}
	if agg {
		kinds = append(kinds, struct {
			name string
			kind store.Kind
		}{"stations.db", store.KindStations})
	}
	var out []dbProfileJSON
	for _, k := range kinds {
		p, over := engine.StorageProfile(cfg, k.kind, dir, agg)
		if over == nil {
			over = []string{}
		}
		out = append(out, dbProfileJSON{DB: k.name, Profile: p.View(), Overrides: over})
	}
	return out
}

func writeProfile(w io.Writer, d dbProfileJSON) {
	p := d.Profile
	how := "storage.profile = " + p.Requested
	if p.Reason != "" {
		how = p.Requested + ": " + p.Reason
	}
	fmt.Fprintf(w, "\n%s: profile %s (%s)\n", d.DB, p.Name, how)
	dur := func(ms int64) string {
		if ms <= 0 {
			return "-"
		}
		return (time.Duration(ms) * time.Millisecond).String()
	}
	mib := func(b int64) string {
		if b <= 0 {
			return "off"
		}
		return fmt.Sprintf("%d MiB", b>>20)
	}
	row := func(k, v string) { fmt.Fprintf(w, "  %-24s %s\n", k, v) }
	row("group commit", dur(p.FlushIntervalMs))
	if p.TrailFlushMs > 0 || p.IncFlushMs > 0 || p.HeatFlushMs > 0 {
		row("trail / stats / heat", dur(p.TrailFlushMs)+" / "+dur(p.IncFlushMs)+" / "+dur(p.HeatFlushMs))
		row("range_hour partial", dur(p.RangePartialMs))
	} else {
		row("statistics flushes", "aggregator lanes (aggregator.lanes.*)")
	}
	row("trail seal", dur(p.SealIntervalMs))
	row("checkpoint", fmt.Sprintf("%s or %s (hard cap %s, WAL limit %s)", mib(p.CheckpointSoftBytes), dur(p.CheckpointMs),
		mib(int64(p.WALHardCapPages)*4096), mib(p.JournalSizeLimit)))
	idle := "never"
	if p.ReaderIdleCloseMs > 0 {
		idle = dur(p.ReaderIdleCloseMs)
	}
	row("page caches", fmt.Sprintf("writer %d MiB, readers %d x %d MiB (idle close %s)", p.WriterCacheKiB>>10, p.ReadConns,
		p.ReaderCacheKiB>>10, idle))
	row("memory map", mib(p.MmapBytes))
	row("backup before migrate", p.BackupBeforeMigrate)
	if d.DB == "airview.db" {
		row("tile cache default", fmt.Sprintf("%d MB", p.TileCacheMB))
	}
	if len(d.Overrides) > 0 {
		row("overridden", strings.Join(d.Overrides, ", "))
	}
}

// dbFiles are the databases a data directory can hold, with their kind.
var dbFiles = []struct {
	name string
	kind store.Kind
}{{"airview.db", store.KindMain}, {"stations.db", store.KindStations}}

// dbMigrationJSON is one database of "airview db migrate --status -json".
type dbMigrationJSON struct {
	DB     string              `json:"db"`
	Source string              `json:"source"` // "server" (the running instance) or "file"
	Info   store.MigrationInfo `json:"migration"`
}

// runDBMigrate implements "airview db migrate --status | --backup | --now":
// the background conversion of the databases to the current storage layout
// (docs/perf/DB-EFFICIENCY-DESIGN.md §5) seen and driven from the command
// line, for headless installs.
//
//   - --status prints the progress. While AirView runs it asks the running
//     server (/api/db/stats), else (or when that needs a login) it reads
//     the progress table read-only; it never changes a database.
//   - --backup writes a consistent copy of every database to
//     <data>/backup/<db>-pre-1.1-<UTC date>.db (VACUUM INTO; safe while
//     AirView runs). The copy is the operator's: AirView never deletes it.
//   - --now runs every pending migration to completion without the pauses
//     of the background run (AirView must be stopped; Ctrl+C stops between
//     two steps, the next start resumes there).
func runDBMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview db migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	cfgFile := fs.String("config", "", "operator config `file` (default: $AIRVIEW_CONFIG, else airview.jsonc in the data directory)")
	status := fs.Bool("status", false, "print the migration progress (safe while AirView runs)")
	backup := fs.Bool("backup", false, "write a backup of every database to <data>/backup/ (safe while AirView runs)")
	now := fs.Bool("now", false, "run the pending migrations to completion now (AirView must be stopped)")
	asJSON := fs.Bool("json", false, "print JSON (--status)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: airview db migrate [-data dir] --status [-json] | --backup | --now\n\n"+
			"AirView converts its databases to the current storage layout in the background after an\n"+
			"upgrade; the station keeps working meanwhile. --status shows the progress, --backup writes\n"+
			"a copy of every database first, --now finishes the conversion at once (service stopped).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	n := 0
	for _, b := range []bool{*status, *backup, *now} {
		if b {
			n++
		}
	}
	if fs.NArg() > 0 || n != 1 {
		fs.Usage()
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	ctx := context.Background()
	switch {
	case *status:
		return dbMigrateStatus(ctx, dir, *asJSON, stdout, stderr)
	case *backup:
		return dbMigrateBackup(ctx, dir, time.Now().UTC(), stdout, stderr)
	default:
		return dbMigrateNow(ctx, dir, *cfgFile, stdout, stderr)
	}
}

// presentDBs lists the databases that exist in dir.
func presentDBs(dir string) []struct {
	name string
	kind store.Kind
} {
	var out []struct {
		name string
		kind store.Kind
	}
	for _, d := range dbFiles {
		if st, err := os.Stat(filepath.Join(dir, d.name)); err == nil && st.Mode().IsRegular() {
			out = append(out, d)
		}
	}
	return out
}

// instanceRunning reports whether an AirView server runs on dir and the URL
// of its web interface ("" = unknown).
func instanceRunning(dir string) (url string, running bool) {
	release, already := acquireInstance(dir)
	if !already {
		release()
	}
	info, ok := readInstanceInfo(dir)
	if ok && processAlive(info.PID) {
		return info.URL, true
	}
	return "", already
}

func dbMigrateStatus(ctx context.Context, dir string, asJSON bool, stdout, stderr io.Writer) int {
	dbs := presentDBs(dir)
	if len(dbs) == 0 {
		fmt.Fprintf(stderr, "no database in %s\n", dir)
		return 1
	}
	var out []dbMigrationJSON
	url, running := instanceRunning(dir)
	for _, d := range dbs {
		// The running server knows rows left and the ETA of airview.db.
		if d.kind == store.KindMain && running && url != "" {
			if info, ok := serverMigrationInfo(ctx, url); ok {
				out = append(out, dbMigrationJSON{DB: d.name, Source: "server", Info: info})
				continue
			}
		}
		if !store.HasDataMigrations(d.kind) {
			// Only DDL migrations, applied whenever AirView opens the file.
			out = append(out, dbMigrationJSON{DB: d.name, Source: "file", Info: store.MigrationInfo{
				State: "none", Migrations: []store.MigrationState{},
				Hint: "no data conversion needed: its schema changes are applied at start-up"}})
			continue
		}
		info, err := fileMigrationInfo(ctx, filepath.Join(dir, d.name))
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", d.name, err)
			return 1
		}
		out = append(out, dbMigrationJSON{DB: d.name, Source: "file", Info: info})
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"dataDir": dir, "running": running, "databases": out}, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Data directory: %s (AirView %s)\n", dir, map[bool]string{true: "running", false: "stopped"}[running])
	for _, d := range out {
		writeMigration(stdout, d, running)
	}
	return 0
}

// serverMigrationInfo asks the running server (/api/db/stats "migration").
// A server that wants a login answers 401/403: the caller then reads the
// file.
func serverMigrationInfo(ctx context.Context, base string) (store.MigrationInfo, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/api/db/stats", nil)
	if err != nil {
		return store.MigrationInfo{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return store.MigrationInfo{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return store.MigrationInfo{}, false
	}
	var body struct {
		Migration *store.MigrationInfo `json:"migration"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil || body.Migration == nil {
		return store.MigrationInfo{}, false
	}
	return *body.Migration, true
}

// fileMigrationInfo reads the progress table of a database read-only.
// Migrations this build knows but the file has no row for yet are not
// listed (they are "pending" until AirView starts them); a file without the
// table has not been touched by this version yet.
func fileMigrationInfo(ctx context.Context, path string) (store.MigrationInfo, error) {
	info := store.MigrationInfo{State: store.MigPending, Migrations: []store.MigrationState{}}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return info, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_data_migrations'`).Scan(&n); err != nil {
		return info, err
	}
	if n == 0 {
		info.Hint = "not started: the database still has the layout of an earlier version"
		return info, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT name, version, done, COALESCE(started, 0), COALESCE(updated, 0),
		COALESCE(rows_done, 0), COALESCE(last_error, '') FROM schema_data_migrations ORDER BY COALESCE(started, 0), name, version`)
	if err != nil {
		return info, err
	}
	defer rows.Close()
	all := true
	for rows.Next() {
		var st store.MigrationState
		var done int
		if err := rows.Scan(&st.Name, &st.Version, &done, &st.Started, &st.Updated, &st.RowsDone, &st.LastError); err != nil {
			return info, err
		}
		switch {
		case done >= 2:
			st.State = store.MigDone
		case st.LastError != "":
			st.State = store.MigFailed
			all = false
		case st.Started > 0:
			st.State = store.MigRunning
			all = false
		default:
			st.State = store.MigPending
			all = false
		}
		info.Migrations = append(info.Migrations, st)
	}
	if err := rows.Err(); err != nil {
		return info, err
	}
	switch {
	case len(info.Migrations) == 0:
		info.State = store.MigPending
	case all:
		info.State = store.MigDone
	default:
		info.State = store.MigRunning
		for _, st := range info.Migrations {
			if st.State == store.MigFailed {
				info.State = store.MigFailed
			}
		}
	}
	return info, nil
}

func writeMigration(w io.Writer, d dbMigrationJSON, running bool) {
	in := d.Info
	state := in.State
	if state == store.MigRunning && d.Source == "file" && !running {
		state = "paused (continues when AirView starts; or run --now)"
	}
	fmt.Fprintf(w, "\n%s: %s\n", d.DB, state)
	if in.Hint != "" {
		fmt.Fprintf(w, "  %s\n", in.Hint)
	}
	if in.Backup != "" {
		fmt.Fprintf(w, "  backup: %s\n", in.Backup)
	}
	for _, m := range in.Migrations {
		line := fmt.Sprintf("  %-14s v%d  %-18s", m.Name, m.Version, m.State)
		if m.RowsDone > 0 {
			line += fmt.Sprintf("  %d rows done", m.RowsDone)
		}
		if m.RowsLeft > 0 && m.State != store.MigDone {
			line += fmt.Sprintf(", about %d left", m.RowsLeft)
		}
		if m.ETA > 0 && m.State == store.MigRunning {
			line += ", done about " + time.UnixMilli(m.ETA).Format("15:04")
		}
		if m.LastError != "" {
			line += "  error: " + m.LastError
		}
		fmt.Fprintln(w, line)
	}
}

// dbMigrateBackup copies every database to <data>/backup/<db>-pre-1.1-<UTC
// date>[-n].db with VACUUM INTO (consistent; AirView may run meanwhile).
func dbMigrateBackup(ctx context.Context, dir string, now time.Time, stdout, stderr io.Writer) int {
	dbs := presentDBs(dir)
	if len(dbs) == 0 {
		fmt.Fprintf(stderr, "no database in %s\n", dir)
		return 1
	}
	bdir := filepath.Join(dir, store.BackupDirName)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		fmt.Fprintf(stderr, "backup directory: %v\n", err)
		return 1
	}
	for _, d := range dbs {
		dst := ""
		base := strings.TrimSuffix(d.name, ".db") + "-pre-1.1-" + now.Format("20060102")
		for i := 1; i < 1000 && dst == ""; i++ {
			name := base + ".db"
			if i > 1 {
				name = fmt.Sprintf("%s-%d.db", base, i)
			}
			if _, err := os.Lstat(filepath.Join(bdir, name)); errors.Is(err, os.ErrNotExist) {
				dst = filepath.Join(bdir, name)
			}
		}
		if dst == "" {
			fmt.Fprintf(stderr, "%s: too many backups today\n", d.name)
			return 1
		}
		src := filepath.Join(dir, d.name)
		t0 := time.Now()
		if err := snapshotDB(ctx, src, dst); err != nil {
			_ = os.Remove(dst)
			fmt.Fprintf(stderr, "%s: backup failed: %v\n", d.name, err)
			return 1
		}
		size := int64(0)
		if st, err := os.Stat(dst); err == nil {
			size = st.Size()
		}
		fmt.Fprintf(stdout, "%s -> %s (%d MiB, %s)\n", d.name, dst, size>>20, time.Since(t0).Round(100*time.Millisecond))
	}
	fmt.Fprintln(stdout, "Keep these files to be able to go back to the earlier version (restore them in place of the databases while AirView is stopped).")
	return 0
}

// dbMigrateNow opens every database with its profile and runs the pending
// data migrations without pauses. AirView must not run on the directory.
func dbMigrateNow(ctx context.Context, dir, cfgFile string, stdout, stderr io.Writer) int {
	if _, running := instanceRunning(dir); running {
		fmt.Fprintf(stderr, "AirView is running with %s: it migrates in the background already; stop it to use --now\n", dir)
		return 1
	}
	release, already := acquireInstance(dir)
	if already {
		fmt.Fprintf(stderr, "AirView is running with %s: stop it to use --now\n", dir)
		return 1
	}
	defer release()
	cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: dir, File: cfgFile, Environ: os.Environ(), ReadOnly: true})
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v (run \"airview config check\")\n", err)
		return 1
	}
	dbs := presentDBs(dir)
	if len(dbs) == 0 {
		fmt.Fprintf(stderr, "no database in %s\n", dir)
		return 1
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	agg := cfg.Mode == model.ModeAggregator
	for _, d := range dbs {
		prof, _ := engine.StorageProfile(cfg, d.kind, dir, agg)
		opts := store.Options{Logger: log, Profile: prof, MigrationDelay: -1}
		var st *store.Store
		if d.kind == store.KindStations {
			st, err = store.OpenStations(dir, opts)
		} else {
			st, err = store.OpenWithOptions(dir, opts)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: open: %v\n", d.name, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: migrating (profile %s, backup %s)\n", d.name, prof.Name, prof.BackupBeforeMigrate)
		t0 := time.Now()
		done := make(chan error, 1)
		go func() { done <- st.MigrateNow(ctx) }()
		tick := time.NewTicker(5 * time.Second)
		var merr error
	wait:
		for {
			select {
			case merr = <-done:
				break wait
			case <-tick.C:
				for _, m := range st.MigrationStatus() {
					if m.State == store.MigRunning {
						fmt.Fprintf(stdout, "  %s v%d: %d rows done, about %d left\n", m.Name, m.Version, m.RowsDone, m.RowsLeft)
					}
				}
			}
		}
		tick.Stop()
		info := st.MigrationInfo()
		if cerr := st.Close(); cerr != nil && merr == nil {
			merr = cerr
		}
		if merr != nil {
			fmt.Fprintf(stderr, "%s: migration stopped: %v (state %s; the next start resumes it)\n", d.name, merr, info.State)
			return 1
		}
		if info.Backup != "" {
			fmt.Fprintf(stdout, "  backup: %s\n", info.Backup)
		}
		fmt.Fprintf(stdout, "%s: %s in %s\n", d.name, info.State, time.Since(t0).Round(time.Second))
	}
	return 0
}

// readOnlyDSN opens path read-only (URI form, also for Windows paths).
func readOnlyDSN(path string) string {
	s := filepath.ToSlash(path)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	u := url.URL{Scheme: "file", Path: s, RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"}
	return u.String()
}

// runDBVacuum implements "airview db vacuum": VACUUM of every database of
// the data directory while AirView is stopped. A file created without
// incremental auto-vacuum (some files of 1.0) keeps the pages the 1.1
// migration freed for reuse; this returns them to the disk. VACUUM needs
// free disk space of about the file size; when it runs out SQLite stops
// with an error and the database stays as it was.
func runDBVacuum(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("airview db vacuum", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", "", "data `directory` (default: $AIRVIEW_DATA, else the default data directory)")
	cfgFile := fs.String("config", "", "operator config `file` (default: $AIRVIEW_CONFIG, else airview.jsonc in the data directory)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: airview db vacuum [-data dir] [-config file]\n\n"+
			"Rewrites every database of the data directory without free pages (AirView must be\n"+
			"stopped; while it runs use Settings > Storage & database > Compact, or POST /api/db/vacuum).\n"+
			"Needs free disk space of about the size of the largest database.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	dir, err := dataDirFromFlag(*data)
	if err != nil {
		fmt.Fprintf(stderr, "data directory: %v\n", err)
		return 1
	}
	if _, running := instanceRunning(dir); running {
		fmt.Fprintf(stderr, "AirView is running with %s: stop it, or compact from the web interface (POST /api/db/vacuum)\n", dir)
		return 1
	}
	release, already := acquireInstance(dir)
	if already {
		fmt.Fprintf(stderr, "AirView is running with %s: stop it first\n", dir)
		return 1
	}
	defer release()
	cfg, _, _, err := config.LoadLayered(config.LoadOptions{Dir: dir, File: *cfgFile, Environ: os.Environ(), ReadOnly: true})
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v (run \"airview config check\")\n", err)
		return 1
	}
	dbs := presentDBs(dir)
	if len(dbs) == 0 {
		fmt.Fprintf(stderr, "no database in %s\n", dir)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	agg := cfg.Mode == model.ModeAggregator
	for _, d := range dbs {
		path := filepath.Join(dir, d.name)
		before := fileSize(path)
		prof, _ := engine.StorageProfile(cfg, d.kind, dir, agg)
		// No background migration while the file is rewritten; it resumes
		// at the next start.
		opts := store.Options{Logger: log, Profile: prof, MigrationDelay: -1}
		var st *store.Store
		if d.kind == store.KindStations {
			st, err = store.OpenStations(dir, opts)
		} else {
			st, err = store.OpenWithOptions(dir, opts)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: open: %v\n", d.name, err)
			return 1
		}
		t0 := time.Now()
		verr := st.Vacuum(ctx)
		if cerr := st.Close(); cerr != nil && verr == nil {
			verr = cerr
		}
		if verr != nil {
			fmt.Fprintf(stderr, "%s: vacuum failed: %v (the database is unchanged)\n", d.name, verr)
			return 1
		}
		fmt.Fprintf(stdout, "%s: %.1f MB -> %.1f MB in %s\n", d.name, float64(before)/1e6, float64(fileSize(path))/1e6,
			time.Since(t0).Round(time.Second))
	}
	return 0
}

// fileSize returns the size of path (0 when it does not exist).
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
