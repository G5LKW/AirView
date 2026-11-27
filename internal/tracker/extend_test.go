package tracker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/rfstat"
	"airview/internal/store"
	"airview/internal/trajectory"
)

// dropPositions forwards frames to a tracker but drops the airborne
// position squitters of one aircraft while active is set.
type dropPositions struct {
	t       *Tracker
	icao    uint32
	active  bool
	dropped int
}

func (d *dropPositions) HandleFrame(f model.RawFrame) {
	if d.active && len(f.Data) == 14 {
		df := f.Data[0] >> 3
		icao := uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
		tc := f.Data[4] >> 3
		if (df == 17 || df == 18) && icao == d.icao && (tc >= 9 && tc <= 18 || tc >= 20 && tc <= 22) {
			d.dropped++
			return
		}
	}
	d.t.HandleFrame(f)
}

func (d *dropPositions) HandleUpdate(u model.DecodedUpdate) { d.t.HandleUpdate(u) }

func findAC(s model.LiveSnapshot, hex string) *model.Aircraft {
	for i := range s.Aircraft {
		if s.Aircraft[i].ICAO == hex {
			return &s.Aircraft[i]
		}
	}
	return nil
}

// TestCoastingPrediction: while an aircraft's positions drop out, the live
// picture shows a predicted position (predicted=true) near the truth, but
// nothing predicted reaches the trail or the database.
func TestCoastingPrediction(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenWithOptions(t.TempDir(), store.Options{Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, cfg := newSim(t, 30, nil)
	tr := New(Options{Config: &cfg, Store: st})
	tj := trajectory.NewManager(trajectory.Config{MaxCoast: 120 * time.Second})
	tr.SetTrajectory(tj, 60)
	rs := rfstat.New(rfstat.Config{Antennas: []string{"vertical", "yagi"}})
	tr.SetRFStats(rs)
	enriched := 0
	tr.SetEnricher(func(s *model.LiveSnapshot) { enriched++ })

	sink := &dropPositions{t: tr}
	advance(t, s, 90*time.Second, sink)

	// Pick an airborne ADS-B aircraft with a fresh position.
	snap := tr.Snapshot()
	var target *model.Aircraft
	for i := range snap.Aircraft {
		a := &snap.Aircraft[i]
		if a.Source == model.SrcADSB && a.Lat != nil && a.GS != nil && *a.GS > 150 && a.PosAge != nil && *a.PosAge < 2 &&
			a.OnGround != nil && !*a.OnGround && a.DistKm != nil && *a.DistKm < 250 {
			target = a
			break
		}
	}
	if target == nil {
		t.Fatal("no suitable aircraft")
	}
	hex := target.ICAO
	key := uint32(0)
	for _, c := range hex {
		key = key<<4 | uint32(hexDigit(byte(c)))
	}
	sink.icao, sink.active = key, true
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	trailBefore := len(tr.Trail(key))
	posBefore := storedPositions(t, st, key)
	lastReal := *target.Lat

	advance(t, s, 20*time.Second, sink)
	if sink.dropped == 0 {
		t.Fatal("no position frames were dropped")
	}
	snap = tr.Snapshot()
	a := findAC(snap, hex)
	if a == nil {
		t.Fatalf("%s left the live picture while coasting", hex)
	}
	if !a.Predicted || a.Lat == nil || a.CoastS == nil || *a.CoastS < model.PredictAfterSec || a.PredSigmaM == nil ||
		a.LastRealLat == nil || a.DistKm == nil {
		t.Fatalf("not predicted: %+v", a)
	}
	if *a.Lat == lastReal {
		t.Error("predicted latitude equals the last real one")
	}
	if snap.Counts.Predicted < 1 {
		t.Errorf("counts.predicted = %d", snap.Counts.Predicted)
	}
	// The prediction follows the simulated truth.
	found := false
	for _, tru := range s.Truth() {
		if tru.ICAO != key {
			continue
		}
		found = true
		d := geo.DistanceKm(*a.Lat, *a.Lon, tru.Lat, tru.Lon)
		real := geo.DistanceKm(*a.LastRealLat, *a.LastRealLon, tru.Lat, tru.Lon)
		t.Logf("%s coasting %.1f s: prediction %.2f km from the truth, last real position %.2f km, sigma %.0f m", hex, *a.CoastS, d, real, *a.PredSigmaM)
		if d > 3 || d > real {
			t.Errorf("predicted position %.2f km from the truth after %.0f s", d, *a.CoastS)
		}
	}
	if !found {
		t.Error("aircraft not in the simulator truth")
	}
	if n := len(tr.Trail(key)); n != trailBefore {
		t.Errorf("trail grew from %d to %d points while coasting", trailBefore, n)
	}
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := storedPositions(t, st, key); n != posBefore {
		t.Errorf("stored positions changed from %d to %d while coasting", posBefore, n)
	}

	// Learned-flow blend: the coasting position moves towards the learned
	// one by its weight, and the hook gets the last real fix.
	base := findAC(tr.Snapshot(), hex)
	var gotLat, gotDt float64
	tr.SetLearnedTurn(func(lat, lon, altFt, trk, gs, dtS float64, nowMs int64) (float64, float64, float64, bool) {
		gotLat, gotDt = lat, dtS
		return *base.Lat + 0.2, *base.Lon, 0.5, true
	})
	b := findAC(tr.Snapshot(), hex)
	if b == nil || !b.Predicted || b.PredLearned == nil || *b.PredLearned != 0.5 || gotLat != *base.LastRealLat || gotDt < model.PredictAfterSec {
		t.Fatalf("blend not applied: %+v (hook lat %v dt %v)", b, gotLat, gotDt)
	}
	if d := *b.Lat - *base.Lat; d < 0.099 || d > 0.101 {
		t.Errorf("blended latitude moved %.4f deg, want 0.1", d)
	}
	tr.SetLearnedTurn(func(lat, lon, altFt, trk, gs, dtS float64, nowMs int64) (float64, float64, float64, bool) {
		return 0, 0, 0, false
	})
	if c := findAC(tr.Snapshot(), hex); c.PredLearned != nil || *c.Lat != *base.Lat {
		t.Errorf("no learned answer must leave the filter's position: %+v", c)
	}
	tr.SetLearnedTurn(nil)

	// Beyond coastSec: no predicted position any more.
	advance(t, s, 50*time.Second, sink)
	if a := findAC(tr.Snapshot(), hex); a != nil && a.Predicted {
		t.Errorf("still predicted after %v s", a.CoastS)
	}
	if enriched == 0 {
		t.Error("enricher never called")
	}
	if rep := rs.Report(); rep.Channel.Frames == 0 {
		t.Error("rfstat saw no frames")
	}
}

func hexDigit(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
}

func storedPositions(t *testing.T, st *store.Store, icao uint32) int {
	t.Helper()
	var n int
	err := st.DB().QueryRow(`SELECT COUNT(*) FROM positions p JOIN flights f ON f.id = p.flight_id WHERE f.icao = ?`, int64(icao)).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestObsTimeGPS(t *testing.T) {
	recv := time.Date(2026, 1, 2, 23, 59, 59, 900_000_000, time.UTC)
	f := model.RawFrame{Recv: recv, GPSNs: 86_399_950_000_000} // 23:59:59.950
	got := obsTime(&f, recv.UnixMilli(), 0)
	if d := got.Sub(recv); d != 50*time.Millisecond {
		t.Errorf("same day: %v", d)
	}
	recv2 := time.Date(2026, 1, 3, 0, 0, 0, 20_000_000, time.UTC)
	f2 := model.RawFrame{Recv: recv2, GPSNs: 86_399_990_000_000} // previous day 23:59:59.990
	if d := obsTime(&f2, recv2.UnixMilli(), 0).Sub(recv2); d != -30*time.Millisecond {
		t.Errorf("midnight wrap: %v", d)
	}
	f3 := model.RawFrame{Recv: recv2, GPSNs: -1}
	if !obsTime(&f3, 0, 0).Equal(recv2) {
		t.Error("no GPS: receive time expected")
	}
	// Host clock 0.8 s fast (NTP offset -800 ms): the GPS time of the frame
	// moves onto the host clock, the base of all other observations and
	// of the prediction times.
	recv4 := time.Date(2026, 1, 2, 12, 0, 0, 900_000_000, time.UTC) // host
	f4 := model.RawFrame{Recv: recv4, GPSNs: 12 * 3600 * 1e9}       // true 12:00:00.000
	if d := obsTime(&f4, recv4.UnixMilli(), -800*time.Millisecond).Sub(recv4); d != -100*time.Millisecond {
		t.Errorf("offset applied: %v, want -100ms", d)
	}
}

// BenchmarkHandleFrameFull is BenchmarkHandleFrame with the tracking filter
// the RF statistics and the latency analysis attached (the production
// configuration).
func BenchmarkHandleFrameFull(b *testing.B) {
	s, cfg := newSim(b, 150, nil)
	rec := &recorder{}
	const span = 20 * time.Second
	advance(b, s, span, rec)
	tr := New(Options{Config: &cfg})
	tr.SetTrajectory(trajectory.NewManager(trajectory.Config{MaxCoast: 120 * time.Second}), 60)
	tr.SetRFStats(rfstat.New(rfstat.Config{Antennas: []string{"vertical", "yagi"}}))
	tr.SetLatency(newLatency(&cfg))
	for pass := 0; pass < 3; pass++ {
		for _, f := range rec.frames {
			f.Recv = f.Recv.Add(time.Duration(pass) * span)
			tr.HandleFrame(f)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	pass, i := 3, 0
	shift := time.Duration(pass) * span
	for n := 0; n < b.N; n++ {
		f := rec.frames[i]
		f.Recv = f.Recv.Add(shift)
		tr.HandleFrame(f)
		if i++; i == len(rec.frames) {
			i = 0
			pass++
			shift = time.Duration(pass) * span
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/frame")
}
