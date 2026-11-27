package registry

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// TestRegistryDSNFollowsServerProfile checks the WAL settings of the
// registry's connections (DB-EFFICIENCY-DESIGN §3.1): a bounded WAL file
// and modest caches always; while internal/store has stations.db open (its
// checkpointer serves the file) no automatic checkpoint inside the
// registry's commits, alone SQLite's own 1,000-page checkpoint.
func TestRegistryDSNFollowsServerProfile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := newClock()
	r := openAt(t, filepath.Join(dir, FileName), clk, Policy{Admission: model.AdmissionOpen})
	defer r.Close()
	pragma := func(name string) int64 {
		t.Helper()
		var v int64
		if err := r.wdb.QueryRowContext(ctx, `PRAGMA `+name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	var rcache int64
	if err := r.rdb.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&rcache); err != nil {
		t.Fatal(err)
	}
	if limit, wcache := pragma("journal_size_limit"), pragma("cache_size"); limit != 128<<20 || wcache != -8192 || rcache != -4096 {
		t.Fatalf("journal_size_limit %d, cache %d / %d", limit, wcache, rcache)
	}

	// Alone: SQLite's checkpoint bounds the WAL.
	mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if a := pragma("wal_autocheckpoint"); a != 1000 {
		t.Fatalf("registry alone: wal_autocheckpoint %d, want 1000", a)
	}

	// With the store on the same file: the hard cap, and many small writes
	// never checkpoint inside a commit.
	st, err := store.OpenStations(dir, store.Options{MigrationDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Profile: store.Profile{Name: store.ProfileServer, CheckpointInterval: 1 << 40, CheckpointSoftBytes: 1 << 40}})
	if err != nil {
		t.Fatal(err)
	}
	if !store.CheckpointServes(filepath.Join(dir, ".", FileName)) {
		t.Fatal("CheckpointServes: false with the store open")
	}
	for i := 2; i < 600; i++ {
		mustApproved(t, admitAVF(t, r, i, 0, ""))
	}
	if a := pragma("wal_autocheckpoint"); a != 131072 {
		t.Fatalf("with the store: wal_autocheckpoint %d, want 131072", a)
	}
	var busy, logFrames, ckpt int64
	if err := r.wdb.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &ckpt); err != nil {
		t.Fatal(err)
	}
	if logFrames < 1000 {
		t.Fatalf("WAL holds %d frames after 600 admissions: an automatic checkpoint ran", logFrames)
	}

	// The store closes first (shutdown): back to SQLite's checkpoint.
	_ = st.Close()
	mustApproved(t, admitAVF(t, r, 700, 0, ""))
	if a := pragma("wal_autocheckpoint"); a != 1000 {
		t.Fatalf("after the store closed: wal_autocheckpoint %d, want 1000", a)
	}

	// Explicit values win.
	r2, err := Open(filepath.Join(t.TempDir(), FileName), Options{Logger: quiet, WALHardCapPages: 4096, JournalSizeLimit: 1 << 20,
		WriterCacheKiB: 2048, ReaderCacheKiB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	var limit int64
	_ = r2.wdb.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit)
	_ = r2.rdb.QueryRowContext(ctx, `PRAGMA cache_size`).Scan(&rcache)
	if limit != 1<<20 || rcache != -1024 || r2.opts.WALHardCapPages != 4096 {
		t.Fatalf("options: journal_size_limit %d, reader cache %d, cap %d", limit, rcache, r2.opts.WALHardCapPages)
	}
}
