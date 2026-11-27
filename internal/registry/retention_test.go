package registry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"airview/internal/auth"
	"airview/internal/model"
)

// storeLikeTables creates statistics tables shaped like internal/store's
// (stats_minute is WITHOUT ROWID with PK(t, ant) in the real schema) plus
// sid- and feed-keyed tables, as WP-D will create them in stations.db.
func storeLikeTables(t *testing.T, r *Registry) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE stats_minute(t INTEGER NOT NULL, ant TEXT NOT NULL, msgs INTEGER, PRIMARY KEY(t, ant)) WITHOUT ROWID`,
		`CREATE TABLE stats_hour(t INTEGER NOT NULL, ant TEXT NOT NULL, msgs INTEGER, PRIMARY KEY(t, ant)) WITHOUT ROWID`,
		`CREATE TABLE coverage(day INTEGER NOT NULL, ant TEXT NOT NULL, brg_bin INTEGER NOT NULL, n INTEGER, PRIMARY KEY(day, ant, brg_bin)) WITHOUT ROWID`,
		`CREATE TABLE heat(cell INTEGER NOT NULL, ant TEXT NOT NULL, count INTEGER, PRIMARY KEY(cell, ant)) WITHOUT ROWID`,
		`CREATE TABLE station_minute(t INTEGER NOT NULL, sid TEXT NOT NULL, frames INTEGER)`,
		`CREATE TABLE receiver_status(t INTEGER NOT NULL, feed TEXT NOT NULL, json TEXT NOT NULL, PRIMARY KEY(t, feed))`,
	} {
		if _, err := r.wdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, r *Registry, q string, args ...any) int {
	t.Helper()
	var n int
	if err := r.rdb.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetentionJob(t *testing.T) {
	ctx := context.Background()
	r, clk, _ := openTest(t, model.AdmissionOpen)
	storeLikeTables(t, r)
	owner, _ := r.Accounts().Create(ctx, auth.Account{Username: "own", Role: auth.RoleOwner, PasswordHash: "x"})

	start := clk.now()
	owned := mustApproved(t, admitAVF(t, r, 1, 0, ""))
	if _, err := r.AssignOwner(ctx, SystemActor, owned.Idx, owner.ID); err != nil {
		t.Fatal(err)
	}
	stale := mustApproved(t, admitAVF(t, r, 2, 0, ""))
	active := mustApproved(t, admitAVF(t, r, 3, 0, ""))
	host, _ := r.EnsureHost(ctx, "host", 0)

	// Old rows.
	old, _ := r.ConnOpen(ctx, owned.Idx, mustAddr("192.0.2.1"), "avf")
	_ = r.ConnClose(ctx, old, 10)
	_ = r.Audit(ctx, SystemActor, "test.old", owned.SID, "")
	for i := range 8 {
		if _, err := r.AddEstimate(ctx, Estimate{Idx: owned.Idx, Method: "survey", T: clk.now().Add(time.Duration(i) * time.Second), Lat: 1, Lon: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, r, `SELECT COUNT(*) FROM location_estimates WHERE idx=?`, owned.Idx); n != 5 {
		t.Errorf("AddEstimate kept %d, want 5", n)
	}
	ant := model.StationAntKey(owned.Idx, -1)
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO stats_minute VALUES(%d, '%s', 1), (%d, '%s/0', 1)`, start.Unix(), ant, start.Unix(), ant),
		fmt.Sprintf(`INSERT INTO stats_hour VALUES(%d, '%s', 1)`, start.Unix(), ant),
		fmt.Sprintf(`INSERT INTO coverage VALUES(%d, '%s', 1, 1)`, start.Unix()/86400, ant),
	} {
		if _, err := r.wdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	code, _, _ := r.MintClaim(ctx, owned.Idx)
	_ = code
	_, _, _ = r.MintToken(ctx, SystemActor, "", 1, time.Hour)

	// 100 days later: only the active station kept reporting.
	clk.add(100 * 24 * time.Hour)
	r.Seen(active.Idx, clk.now())
	r.Seen(owned.Idx, clk.now().Add(-95*24*time.Hour)) // owned: offline but claimed
	now := clk.now()
	newConn, _ := r.ConnOpen(ctx, owned.Idx, mustAddr("192.0.2.1"), "avf")
	_ = r.ConnClose(ctx, newConn, 1)
	if _, err := r.wdb.Exec(fmt.Sprintf(`INSERT INTO stats_minute VALUES(%d, '%s', 2)`, now.Unix(), ant)); err != nil {
		t.Fatal(err)
	}
	_ = r.Audit(ctx, SystemActor, "test.new", owned.SID, "")

	rep, err := r.RunRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ConnLog != 1 || count(t, r, `SELECT COUNT(*) FROM conn_log`) != 1 {
		t.Errorf("conn_log: removed %d, left %d", rep.ConnLog, count(t, r, `SELECT COUNT(*) FROM conn_log`))
	}
	if rep.Stats["stats_minute"] != 2 || count(t, r, `SELECT COUNT(*) FROM stats_minute`) != 1 {
		t.Errorf("minute stats: %+v", rep.Stats)
	}
	if rep.Stats["stats_hour"] != 0 || rep.Stats["coverage"] != 0 {
		t.Errorf("hourly/daily stats removed after 100 days: %+v", rep.Stats)
	}
	if rep.Claims != 1 || rep.Tokens != 1 {
		t.Errorf("claims %d tokens %d", rep.Claims, rep.Tokens)
	}
	if rep.Purged != 1 || rep.Deleted != 1 {
		t.Errorf("purged %d deleted %d", rep.Purged, rep.Deleted)
	}
	if _, ok := r.Station(stale.Idx); ok {
		t.Error("unclaimed offline station not purged")
	}
	for _, idx := range []model.StationIdx{owned.Idx, active.Idx, host.Idx} {
		if _, ok := r.Station(idx); !ok {
			t.Errorf("station %d purged", idx)
		}
	}

	// 14 months: hourly and daily stats go; audit (24 months) stays.
	clk.add(330 * 24 * time.Hour)
	r.Seen(active.Idx, clk.now())
	rep, err = r.RunRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stats["stats_hour"] != 1 || rep.Stats["coverage"] != 1 {
		t.Errorf("hourly/daily stats after 14 months: %+v", rep.Stats)
	}
	if count(t, r, `SELECT COUNT(*) FROM audit WHERE action='test.old'`) != 1 {
		t.Error("audit entry removed before 24 months")
	}
	clk.add(320 * 24 * time.Hour) // > 24 months after the first entry
	r.Seen(active.Idx, clk.now())
	if _, err := r.RunRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, r, `SELECT COUNT(*) FROM audit WHERE action='test.old'`) != 0 {
		t.Error("audit entry older than 24 months kept")
	}

	// Configurable: a 1-day conn_log retention.
	r.SetPolicy(Policy{Admission: model.AdmissionOpen, Retention: model.AggRetention{ConnLogDays: 1}})
	if r.Policy().Retention.MinuteDays != 30 {
		t.Error("zero retention fields must take defaults")
	}
	c, _ := r.ConnOpen(ctx, active.Idx, mustAddr("192.0.2.2"), "avf")
	_ = r.ConnClose(ctx, c, 1)
	clk.add(36 * time.Hour)
	r.Seen(active.Idx, clk.now())
	if _, err := r.RunRetention(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, r, `SELECT COUNT(*) FROM conn_log WHERE idx=?`, active.Idx) != 0 {
		t.Error("conn_log retention not configurable")
	}
}

func TestDeletionJob(t *testing.T) {
	ctx := context.Background()
	r, clk, path := openTest(t, model.AdmissionOpen)
	storeLikeTables(t, r)
	// Stations 2 and 20 share a key prefix ("s2" vs "s20"); deleting s2 must
	// not touch s20.
	var victim, other Station
	for i := 1; i <= 19; i++ {
		st := mustApproved(t, admitAVF(t, r, i, 0, ""))
		switch st.Idx {
		case 2:
			victim = st
		case 20:
			other = st
		}
	}
	if victim.Idx != 2 || other.Idx != 20 {
		t.Fatalf("idx layout %d %d", victim.Idx, other.Idx)
	}
	locate(t, r, victim.Idx, 48.1, 11.5)
	_, _ = r.SetPrivacy(ctx, victim.Idx, model.PinRegion, true, true)
	now := clk.now().Unix()
	for _, st := range []Station{victim, other} {
		k := model.StationAntKey(st.Idx, -1)
		for _, q := range []string{
			fmt.Sprintf(`INSERT INTO stats_minute VALUES(%d, '%s', 1), (%d, '%s/0', 1), (%d, '%s/1', 1)`, now, k, now, k, now, k),
			fmt.Sprintf(`INSERT INTO heat VALUES(1, '%s', 1)`, k),
			fmt.Sprintf(`INSERT INTO station_minute VALUES(%d, '%s', 1)`, now, st.SID),
			fmt.Sprintf(`INSERT INTO receiver_status VALUES(%d, '%s', '{}')`, now, model.StationFeedID(st.SID, 0)),
		} {
			if _, err := r.wdb.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = r.Audit(ctx, Actor{ID: 1, Name: "admin"}, "station.reveal", victim.SID, fmt.Sprintf(`{"note":"looked at %s"}`, victim.SID))
	_, _ = r.ConnOpen(ctx, victim.Idx, mustAddr("192.0.2.1"), "avf")

	if err := r.DeleteStation(ctx, Actor{ID: 1, Name: "admin"}, victim.Idx, "admin request"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.BySID(victim.SID); ok {
		t.Error("deleting station still visible by SID")
	}
	if _, ok := r.Resolver().Info(victim.Idx); ok {
		t.Error("deleting station still resolves")
	}
	if _, ok := r.PublicPoint(victim.Idx); ok {
		t.Error("deleting station has a public point")
	}
	if res := admitAVF(t, r, 1, 0, ""); res.Status != StatusDenied {
		t.Errorf("reconnect during deletion: %+v", res)
	}
	// The queue survives a restart.
	_ = r.Close()
	r = openAt(t, path, clk, Policy{Admission: model.AdmissionOpen})
	defer r.Close()
	if st, ok := r.Station(victim.Idx); !ok || !st.Deleting {
		t.Fatal("deletion mark lost on restart")
	}
	n, err := r.ProcessDeletions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ProcessDeletions %d %v", n, err)
	}
	vk, ok2 := model.StationAntKey(victim.Idx, -1), model.StationAntKey(other.Idx, -1)
	if c := count(t, r, `SELECT COUNT(*) FROM stats_minute WHERE ant=? OR ant LIKE ?`, vk, vk+"/%"); c != 0 {
		t.Errorf("%d stats rows of the deleted station left", c)
	}
	if c := count(t, r, `SELECT COUNT(*) FROM stats_minute WHERE ant=? OR ant LIKE ?`, ok2, ok2+"/%"); c != 3 {
		t.Errorf("neighbouring key range damaged: %d rows of s20 left", c)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM heat WHERE ant=?`, `SELECT COUNT(*) FROM station_minute WHERE sid=?`} {
		arg := any(vk)
		if q[len(q)-5:] == "sid=?" {
			arg = victim.SID
		}
		if c := count(t, r, q, arg); c != 0 {
			t.Errorf("%s: %d left", q, c)
		}
	}
	if c := count(t, r, `SELECT COUNT(*) FROM receiver_status`); c != 1 {
		t.Errorf("receiver_status rows: %d, want 1 (the other station)", c)
	}
	if c := count(t, r, `SELECT COUNT(*) FROM conn_log WHERE idx=?`, victim.Idx); c != 0 {
		t.Error("conn_log survived")
	}
	if c := count(t, r, `SELECT COUNT(*) FROM feeders WHERE feeder_id=?`, victim.FeederID); c != 0 {
		t.Error("feeder key survived")
	}
	if c := count(t, r, `SELECT COUNT(*) FROM station_public WHERE idx=?`, victim.Idx); c != 0 {
		t.Error("public point survived")
	}
	if c := count(t, r, `SELECT COUNT(*) FROM audit WHERE sid=? OR instr(detail, ?)>0`, victim.SID, victim.SID); c != 0 {
		t.Error("audit not pseudonymised")
	}
	if c := count(t, r, `SELECT COUNT(*) FROM audit WHERE action='station.reveal'`); c != 1 {
		t.Error("pseudonymised audit entry lost")
	}
	if _, ok := r.Station(victim.Idx); ok {
		t.Error("station still cached")
	}
	// The feeder may register again later (as a new station).
	if res := admitAVF(t, r, 1, 0, ""); !res.New || res.Station.Idx == victim.Idx {
		t.Errorf("re-registration: %+v", res)
	}
}
