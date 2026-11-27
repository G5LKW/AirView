package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"airview/internal/model"
)

// TestDeleteExtra checks that the deletion job calls Options.DeleteExtra
// with the station's index and SID, and keeps the job queued when it fails.
func TestDeleteExtra(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	var calls []string
	fail := true
	r, err := Open(filepath.Join(t.TempDir(), FileName), Options{
		Policy: Policy{Admission: model.AdmissionOpen}, Now: clk.now, Logger: quiet,
		DeleteExtra: func(_ context.Context, idx model.StationIdx, sid string) error {
			calls = append(calls, sid)
			if fail {
				return errors.New("disk full")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if err := r.DeleteStation(ctx, SystemActor, st.Idx, "test"); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ProcessDeletions(ctx); err == nil || n != 0 {
		t.Fatalf("failing hook: n=%d err=%v", n, err)
	}
	if _, ok := r.Station(st.Idx); !ok {
		t.Fatal("station removed although the hook failed")
	}
	fail = false
	if n, err := r.ProcessDeletions(ctx); err != nil || n != 1 {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
	if len(calls) != 2 || calls[1] != st.SID {
		t.Fatalf("hook calls %v", calls)
	}
}

// TestStoreMaintainedSkipsStatsRetention checks that RunRetention leaves the
// statistics tables to internal/store when StoreMaintained is set.
func TestStoreMaintainedSkipsStatsRetention(t *testing.T) {
	ctx := context.Background()
	for _, maintained := range []bool{false, true} {
		clk := newClock()
		r, err := Open(filepath.Join(t.TempDir(), FileName), Options{
			Policy: Policy{Admission: model.AdmissionOpen}, Now: clk.now, Logger: quiet, StoreMaintained: maintained,
		})
		if err != nil {
			t.Fatal(err)
		}
		storeLikeTables(t, r)
		old := clk.now().Add(-400 * 24 * time.Hour).Unix()
		if _, err := r.wdb.Exec(`INSERT INTO stats_hour VALUES(?, 's1', 1)`, old); err != nil {
			t.Fatal(err)
		}
		if _, err := r.RunRetention(ctx); err != nil {
			t.Fatal(err)
		}
		left := count(t, r, `SELECT COUNT(*) FROM stats_hour`)
		_ = r.Close()
		if maintained && left != 1 {
			t.Errorf("StoreMaintained: registry retention deleted stats_hour")
		}
		if !maintained && left != 0 {
			t.Errorf("default: old stats_hour row kept")
		}
	}
}

// C4: BeforeDelete runs before any row of the station is deleted.
func TestBeforeDeleteRunsFirst(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	var order []string
	r, err := Open(filepath.Join(t.TempDir(), FileName), Options{
		Policy: Policy{Admission: model.AdmissionOpen}, Now: clk.now, Logger: quiet,
		BeforeDelete: func(_ context.Context, idx model.StationIdx) error {
			order = append(order, "before")
			return nil
		},
		DeleteExtra: func(context.Context, model.StationIdx, string) error {
			order = append(order, "extra")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if err := r.DeleteStation(ctx, SystemActor, st.Idx, "test"); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ProcessDeletions(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(order) != 2 || order[0] != "before" {
		t.Fatalf("order %v", order)
	}
}
