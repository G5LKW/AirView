package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
	"time"

	"airview/internal/sim/simnet"
)

// writeTrails stores one flight per trail (ids from 1, ICAO = id) with the
// given points (Unix s, lat, lon) and returns the flight ids.
func writeTrails(t testing.TB, st *Store, trails [][]Position) []int64 {
	t.Helper()
	b := NewBatch()
	var ids []int64
	for i, tr := range trails {
		id := st.NextFlightID()
		ids = append(ids, id)
		f := Flight{ID: id, ICAO: uint32(0x400000 + i), MaxDistKm: math.NaN(), MaxDistBrg: math.NaN(), MinDistKm: math.NaN(),
			MinAlt: math.NaN(), MaxAlt: math.NaN(), MaxGS: math.NaN(), RSSIMin: math.NaN(), RSSIMax: math.NaN(), RSSIAvg: math.NaN(),
			FirstLat: math.NaN(), FirstLon: math.NaN(), LastLat: math.NaN(), LastLon: math.NaN()}
		if len(tr) > 0 {
			f.Start, f.End, f.Positions, f.Msgs = tr[0].T, tr[len(tr)-1].T, int64(len(tr)), int64(len(tr))*4
			f.FirstLat, f.FirstLon, f.LastLat, f.LastLon = tr[0].Lat, tr[0].Lon, tr[len(tr)-1].Lat, tr[len(tr)-1].Lon
		}
		b.UpsertFlights([]Flight{f})
		for j := range tr {
			tr[j].FlightID = id
		}
		b.AddPositions(tr)
		if (i+1)%200 == 0 {
			st.Submit(b)
			if err := st.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	st.Submit(b)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func storedTimes(t testing.TB, st *Store, id int64) []int64 {
	t.Helper()
	ps, err := st.Positions(bg, id, 0, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.T
	}
	return out
}

func straightTrail(start int64, n int, step int64) []Position {
	out := make([]Position, n)
	for i := range out {
		out[i] = Position{T: start + int64(i)*step, Lat: 50 + float64(i)*0.001, Lon: 6, Alt: 30000, GS: 400, Track: 0, RSSI: math.NaN()}
	}
	return out
}

// SO-6: ThinTrails keeps exactly the first point of every bucket and the
// last point, only for flights that ended before the cutoff; it is
// idempotent, resumes from its watermark, and Maintain runs it.
func TestThinTrails(t *testing.T) {
	st := openTest(t, manual)
	now := time.Now().Unix()
	old := now - 5*86400
	trails := [][]Position{
		straightTrail(old, 720, 5),           // 1 h at 5 s
		straightTrail(old+7, 100, 3),         // odd start, 3 s interval
		straightTrail(old+100, 1, 5),         // single point
		{},                                   // no position
		straightTrail(now-3600, 720, 5),      // recent: untouched
		straightTrail(old+3*86400, 400, 2),   // 2 days ago + a bit: thinned with cutoff now-1d
		straightTrail(old+3*86400+60, 50, 1), // same, 1 s interval
	}
	want := make([][]int64, len(trails))
	for i, tr := range trails {
		ts := make([]int64, len(tr))
		for j, p := range tr {
			ts[j] = p.T
		}
		want[i] = ts
	}
	ids := writeTrails(t, st, trails)

	check := func(cutoff int64, label string) {
		t.Helper()
		for i, id := range ids {
			got := storedTimes(t, st, id)
			exp := want[i]
			if len(trails[i]) > 0 && trails[i][len(trails[i])-1].T < cutoff {
				keep := thinKeep(exp, 30)
				var k []int64
				for j, ok := range keep {
					if ok {
						k = append(k, exp[j])
					}
				}
				exp = k
			}
			if fmt.Sprint(got) != fmt.Sprint(exp) {
				t.Fatalf("%s: flight %d: %d points kept, want %d", label, i, len(got), len(exp))
			}
			for j := 1; j < len(got); j++ {
				if len(exp) != len(want[i]) && got[j]-got[j-1] > 30+5 {
					t.Fatalf("%s: flight %d: gap %d s", label, i, got[j]-got[j-1])
				}
			}
		}
	}
	c1 := now - 2*86400
	n, err := st.ThinTrails(bg, c1, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing thinned")
	}
	check(c1, "first run")
	if n2, err := st.ThinTrails(bg, c1, 30); err != nil || n2 != 0 {
		t.Fatalf("second run deleted %d (%v)", n2, err)
	}
	// Forget the watermark: a full re-walk deletes nothing (idempotent).
	if err := st.KVDelete(bg, kvTrailsThinned); err != nil {
		t.Fatal(err)
	}
	if n3, err := st.ThinTrails(bg, c1, 30); err != nil || n3 != 0 {
		t.Fatalf("re-walk deleted %d (%v)", n3, err)
	}
	// Maintain with a one-day full tier picks up the two-day-old flights.
	p := DefaultTierPolicy()
	p.TrailFullDays = 1
	rep, err := st.Maintain(bg, p)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ThinnedPositions == 0 {
		t.Fatal("Maintain thinned nothing")
	}
	check(now-86400, "maintain")
	// Disabled tiers do nothing.
	p.TrailFullDays = 0
	if rep, err := st.Maintain(bg, p); err != nil || rep.ThinnedPositions != 0 {
		t.Fatalf("disabled: %d %v", rep.ThinnedPositions, err)
	}
}

// thinKeep matches the SQL statement for arbitrary times.
func TestThinKeepMatchesSQL(t *testing.T) {
	st := openTest(t, manual)
	old := time.Now().Unix() - 10*86400
	var trails [][]Position
	for k := 0; k < 30; k++ {
		var tr []Position
		tt := old + int64(k*977)
		for i := 0; i < 50+k*7; i++ {
			tt += 1 + int64((i*k)%13)
			tr = append(tr, Position{T: tt, Lat: 1, Lon: 1, Alt: math.NaN(), GS: math.NaN(), Track: math.NaN(), RSSI: math.NaN()})
		}
		trails = append(trails, tr)
	}
	want := make([][]int64, len(trails))
	for i, tr := range trails {
		for _, p := range tr {
			want[i] = append(want[i], p.T)
		}
	}
	ids := writeTrails(t, st, trails)
	for _, sec := range []int{7, 30} {
		if err := st.KVDelete(bg, kvTrailsThinned); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ThinTrails(bg, old+86400, sec); err != nil {
			t.Fatal(err)
		}
		for i, id := range ids {
			var exp []int64
			for j, ok := range thinKeep(want[i], int64(sec)) {
				if ok {
					exp = append(exp, want[i][j])
				}
			}
			want[i] = exp
			if got := storedTimes(t, st, id); fmt.Sprint(got) != fmt.Sprint(exp) {
				t.Fatalf("sec %d flight %d: got %v\nwant %v", sec, i, got, exp)
			}
		}
	}
}

// enu returns the local east/north metres of (lat, lon) around (lat0, lon0).
func enu(lat0, lon0, lat, lon float64) (float64, float64) {
	const r = 6371008.8
	return (lon - lon0) * math.Pi / 180 * r * math.Cos(lat0*math.Pi/180), (lat - lat0) * math.Pi / 180 * r
}

// segDist is the distance of p from segment a-b (metres, local plane).
func segDist(a, b, p Position) float64 {
	bx, by := enu(a.Lat, a.Lon, b.Lat, b.Lon)
	px, py := enu(a.Lat, a.Lon, p.Lat, p.Lon)
	l2 := bx*bx + by*by
	u := 0.0
	if l2 > 0 {
		u = math.Max(0, math.Min(1, (px*bx+py*by)/l2))
	}
	return math.Hypot(px-u*bx, py-u*by)
}

// SO-6 against simnet truth: 90 minutes of simulated traffic (arrivals,
// departures, holds, overflights) stored at 5 s and thinned to 30 s. The
// thinned trail stays within the analytic sagitta bound of downsample.go
// (about 1 km) of every full-resolution point, and keeps about 1/6 of the
// rows.
func TestThinTrailsSimnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	start := time.Now().UTC().Add(-3 * 24 * time.Hour).Truncate(time.Second)
	n, err := simnet.New(simnet.Config{Seed: 11, Start: start, Aircraft: 150,
		Receivers: simnet.RandomReceivers(3, 1, simnet.Placement{CenterLat: 51, CenterLon: 6, RadiusKm: 10})})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	trails := map[uint32][]Position{}
	for s := 0; s < 90*60/5; s++ {
		if err := n.Advance(ctx, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		ts := n.Time(n.NowS()).Unix()
		for _, a := range n.Aircraft() {
			tr := trails[a.ICAO]
			if len(tr) > 0 && ts-tr[len(tr)-1].T > 60 {
				continue // left and came back: keep the first visit only
			}
			trails[a.ICAO] = append(tr, Position{T: ts, Lat: a.Lat, Lon: a.Lon, Alt: a.Sim.AltBaro, GS: a.Sim.GS, Track: a.Sim.Track, RSSI: math.NaN(), Src: SrcADSB})
		}
	}
	keys := make([]uint32, 0, len(trails))
	for k := range trails {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var all [][]Position
	full := map[int][]Position{}
	rows := 0
	for i, k := range keys {
		cp := append([]Position(nil), trails[k]...)
		full[i] = cp
		all = append(all, trails[k])
		rows += len(cp)
	}
	st := openTest(t, manual)
	ids := writeTrails(t, st, all)
	del, err := st.ThinTrails(bg, time.Now().Unix()-2*86400, DefaultTrailThinSec)
	if err != nil {
		t.Fatal(err)
	}
	var devs, interp []float64
	var maxGap int64
	for i, id := range ids {
		kept, err := st.Positions(bg, id, 0, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		f := full[i]
		if kept[0].T != f[0].T || kept[len(kept)-1].T != f[len(f)-1].T {
			t.Fatalf("flight %d: first/last point not kept", i)
		}
		k := 0
		for _, p := range f {
			for k+1 < len(kept)-1 && kept[k+1].T <= p.T {
				k++
			}
			a, b := kept[k], kept[min(k+1, len(kept)-1)]
			devs = append(devs, segDist(a, b, p))
			// Position a reader interpolating in time would show.
			q := a
			if b.T > a.T {
				u := float64(p.T-a.T) / float64(b.T-a.T)
				q.Lat, q.Lon = a.Lat+u*(b.Lat-a.Lat), a.Lon+u*(b.Lon-a.Lon)
			}
			x, y := enu(p.Lat, p.Lon, q.Lat, q.Lon)
			interp = append(interp, math.Hypot(x, y))
		}
		for j := 1; j < len(kept); j++ {
			maxGap = max(maxGap, kept[j].T-kept[j-1].T)
		}
	}
	pct := func(v []float64, p float64) float64 {
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		return s[int(p*float64(len(s)-1))]
	}
	t.Logf("%d flights, %d points at 5 s; %d deleted (%.1f%% kept); largest gap %d s", len(ids), rows, del,
		100*float64(int64(rows)-del)/float64(rows), maxGap)
	t.Logf("distance from the thinned polyline: p50 %.0f m, p95 %.0f m, p99 %.0f m, max %.0f m",
		pct(devs, .5), pct(devs, .95), pct(devs, .99), pct(devs, 1))
	t.Logf("time-interpolated position error:  p50 %.0f m, p95 %.0f m, p99 %.0f m, max %.0f m",
		pct(interp, .5), pct(interp, .95), pct(interp, .99), pct(interp, 1))
	if worst := pct(devs, 1); worst > 2000 {
		t.Errorf("thinned trail %.0f m from the full one (bound about 1 km, 2 km for a slow full circle)", worst)
	}
	if maxGap > int64(DefaultTrailThinSec)+5 {
		t.Errorf("gap of %d s between kept points", maxGap)
	}
	if kept := float64(int64(rows)-del) / float64(rows); kept > 0.2 || kept < 0.15 {
		t.Errorf("kept %.3f of the rows, expected about 1/6", kept)
	}
}

// SO-6 acceptance (DB growth per day before / after the trail tiers). It
// stores AIRVIEW_GROWTH_TRAIL_AIRCRAFT (default 400) aircraft for one
// simulated hour at storage.trailIntervalSec = 5 through the real write
// path, measures the positions table on disk (dbstat) at full resolution
// and after ThinTrails + VACUUM, and extrapolates per aircraft-day and to
// the steady state of 7 trail days with 2 full days, for 3,000 and 10,000
// aircraft (the 1,000-feeder design target). Skipped unless
// AIRVIEW_GROWTH=1.
func TestTrailTierGrowth(t *testing.T) {
	if os.Getenv("AIRVIEW_GROWTH") != "1" {
		t.Skip("set AIRVIEW_GROWTH=1 to run the trail growth measurement")
	}
	nAC := 400
	if v, err := fmt.Sscan(os.Getenv("AIRVIEW_GROWTH_TRAIL_AIRCRAFT"), &nAC); err != nil || v != 1 {
		nAC = 400
	}
	start := time.Now().UTC().Add(-4 * 24 * time.Hour).Truncate(time.Second)
	n, err := simnet.New(simnet.Config{Seed: 5, Start: start, Aircraft: nAC,
		Receivers: simnet.RandomReceivers(3, 1, simnet.Placement{CenterLat: 51, CenterLon: 6, RadiusKm: 10})})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const minutes = 60
	type fl struct {
		pts []Position
	}
	flights := map[uint32]*fl{}
	var order []uint32
	acSeconds := 0.0
	for s := 0; s < minutes*60/5; s++ {
		if err := n.Advance(ctx, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		ts := n.Time(n.NowS()).Unix()
		for _, a := range n.Aircraft() {
			f := flights[a.ICAO]
			if f == nil {
				f = &fl{}
				flights[a.ICAO] = f
				order = append(order, a.ICAO)
			}
			f.pts = append(f.pts, Position{T: ts, Lat: a.Lat, Lon: a.Lon, Alt: math.Round(a.Sim.AltBaro/25) * 25,
				GS: math.Round(a.Sim.GS), Track: math.Round(a.Sim.Track), RSSI: math.NaN(), Src: SrcADSB})
			acSeconds += 5
		}
	}
	var all [][]Position
	rows := 0
	for _, k := range order {
		all = append(all, flights[k].pts)
		rows += len(flights[k].pts)
	}
	st := openTest(t, Options{FlushInterval: time.Hour, FlushRows: 1 << 30, MaxQueueRows: 1 << 30})
	writeTrails(t, st, all)
	if err := st.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	posBytes := func() int64 {
		sizes, err := st.TableSizes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, z := range sizes {
			if z.Name == "positions" {
				return z.Bytes
			}
		}
		return 0
	}
	before := posBytes()
	t0 := time.Now()
	del, err := st.ThinTrails(ctx, time.Now().Unix()-2*86400, DefaultTrailThinSec)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	if err := st.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	after := posBytes()
	acDays := acSeconds / 86400 // aircraft-days of positioned traffic
	perACDay := float64(before) / acDays
	ratio := float64(after) / float64(before)
	const trailDays, fullDays = 7, DefaultTrailFullDays
	steady := func(aircraft float64, thin bool) float64 {
		day := perACDay * aircraft
		if !thin {
			return day * trailDays
		}
		return day*fullDays + day*(trailDays-fullDays)*ratio
	}
	gb := func(b float64) float64 { return b / 1e9 }
	t.Logf("trail growth: %d aircraft, %d min, %d rows (%.1f B/row on disk)", nAC, minutes, rows, float64(before)/float64(rows))
	t.Logf("  per aircraft and day at 5 s: %.1f MB full, %.1f MB thinned (%.1f%% of the bytes); thinning %d rows took %v (%.0f rows/s)",
		perACDay/1e6, perACDay*ratio/1e6, 100*ratio, del, took.Round(time.Millisecond), float64(del)/took.Seconds())
	for _, ac := range []float64{1000, 3000, 10000} {
		t.Logf("  %5.0f aircraft: inflow %.2f GB/day; steady state (7 trail days) %.1f GB before, %.1f GB with 2 full days + 30 s tier",
			ac, gb(perACDay*ac), gb(steady(ac, false)), gb(steady(ac, true)))
	}
	if ratio > 0.3 {
		t.Errorf("thinned trails still take %.0f%% of the bytes", 100*ratio)
	}
}
