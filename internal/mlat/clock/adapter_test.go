package clock

import (
	"math"
	"testing"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
	"airview/internal/model"
	"airview/internal/tracker"
)

type fakeRef struct {
	pos geo.Vec3
	ok  bool
	n   int
}

func (r *fakeRef) RefPosition(key uint32, df uint8, t int64) (geo.Vec3, [6]float64, bool) {
	r.n++
	return r.pos, [6]float64{100, 0, 0, 100, 0, 100}, r.ok
}
func (r *fakeRef) Usable(key uint32) bool { return r.ok }

func TestObserveGroup(t *testing.T) {
	f, _ := New(Config{OriginUnixNs: synthOrigin})
	ref := &fakeRef{pos: geo.ToECEF(50, 9, 10000), ok: true}
	g := &ingest.Group{Kind: model.FrameModeSLong, Key: 0x3C1234, DF: 17, HasResult: true,
		Result: tracker.Result{Status: tracker.ResultOK}, FirstNs: synthOrigin + 5e9,
		Copies: []ingest.Copy{
			{Station: 2, ClockCls: model.ClockSDR12, Epoch: 1, Ticks: 100, Timed: true},
			{Station: 3, ClockCls: model.ClockSDR12, Epoch: 1, Ticks: 200, Timed: true},
			{Station: 4, ClockCls: model.ClockSDR12, Epoch: 1, Ticks: 300, Timed: true, SameStation: true},
			{Station: 5, ClockCls: model.ClockHW12, Epoch: 1, Ticks: 400, MapOnly: true},
			{Station: 6, ClockCls: model.ClockNone, Timed: true},
		}}
	if !f.ObserveGroup(g, ref) {
		t.Fatal("group not admitted")
	}
	f.mu.Lock()
	if len(f.inObs) != 1 || f.inObs[0].m1-f.inObs[0].m0 != 2 {
		t.Errorf("admitted members: %+v", f.inObs)
	}
	f.mu.Unlock()
	// not a sync DF, no result, unusable reference, too few timed copies
	g2 := *g
	g2.DF = 20
	if f.ObserveGroup(&g2, ref) {
		t.Error("DF20 admitted")
	}
	g2 = *g
	g2.HasResult = false
	if f.ObserveGroup(&g2, ref) {
		t.Error("group without tracker result admitted")
	}
	ref.ok = false
	if f.ObserveGroup(g, ref) {
		t.Error("aircraft without reference admitted")
	}
	g2 = *g
	g2.Copies = g.Copies[:1]
	if f.ObserveGroup(&g2, &fakeRef{ok: true}) {
		t.Error("single copy admitted")
	}
	if n := testing.AllocsPerRun(100, func() { f.ObserveGroup(&g2, ref) }); n != 0 {
		t.Errorf("ObserveGroup rejection allocates %.0f", n)
	}
}

func TestClockObserver(t *testing.T) {
	f, _ := New(Config{OriginUnixNs: synthOrigin})
	f.ClockStatus(7, ingest.ClockStatus{HostNs: synthOrigin, Settings: 0x11, GPSStatus: 0x01 | 0x02 | 0x04 | 0x08 | 0x10 | 0x80})
	f.ClockStatus(8, ingest.ClockStatus{HostNs: synthOrigin, Settings: 0x11, GPSStatus: 0x01 | 0x02 | 0x20 | 0x80})
	f.ClockStatus(9, ingest.ClockStatus{HostNs: synthOrigin, Settings: 0x01, GPSStatus: 0x9F}) // 12 MHz mode: ignored
	f.ClockEvent(7, ingest.ClockEvent{OldEpoch: 1, NewEpoch: 2, Reason: model.ClockEvRestart})
	f.ClockEvent(7, ingest.ClockEvent{OldEpoch: 2, NewEpoch: 3, Reason: model.ClockEvRestart, MapOnly: true})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inGPS) != 2 || !f.inGPS[0].s.Locked || !f.inGPS[0].s.UTC || f.inGPS[1].s.Locked {
		t.Errorf("GPS statuses %+v", f.inGPS)
	}
	if len(f.inEv) != 1 || f.inEv[0].ev.NewEpoch != 2 {
		t.Errorf("events %+v", f.inEv)
	}
}

// TestSnapshotAPI checks NetTime, Convert and the ingest.ClockView on a
// converged synthetic network.
func TestSnapshotAPI(t *testing.T) {
	w := newSynth(3)
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*1.6), 9+1.2*math.Sin(float64(i)*1.6))
	}
	for i := 0; i < 4; i++ {
		w.addStation(model.ClockSDR12, 50+(w.rng.Float64()-0.5)*2, 9+(w.rng.Float64()-0.5)*3)
	}
	w.addAircraft(40, 50, 9, 200)
	f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 30, Workers: 1})
	w.register(f)
	w.run(f, 0, 120, 100, 0.3, nil)
	snap := f.Snapshot()
	tt := snap.T - 0.2
	var qs []Query
	var truth []float64
	for _, s := range w.sts {
		local := tt + s.clk.theta(tt)
		qs = append(qs, Query{Station: s.idx, Epoch: s.clk.epoch, Ticks: s.clk.ticks(local + DefaultFrameRefNs(s.clk.cls, model.FrameModeSLong)*1e-9), Kind: model.FrameModeSLong})
		truth = append(truth, tt)
	}
	m := len(qs)
	ts := make([]float64, m)
	cov := make([]float64, m*m)
	ok := make([]bool, m)
	snap.Convert(qs, ts, cov, ok)
	if n := testing.AllocsPerRun(50, func() { snap.Convert(qs, ts, cov, ok) }); n != 0 {
		t.Errorf("Convert allocates %.0f", n)
	}
	// errors relative to the anchors' mean (network time = UTC + mean bias)
	cm := 0.0
	for i := 0; i < 4; i++ {
		cm += (ts[i] - truth[i]) * 1e9
	}
	cm /= 4
	for i := range qs {
		if !ok[i] {
			t.Fatalf("query %d not converted", i)
		}
		tn, sig, ok2 := snap.NetTime(qs[i])
		if !ok2 || math.Abs(tn-ts[i]) > 1e-12 {
			t.Errorf("NetTime %v vs Convert %v", tn, ts[i])
		}
		if math.Abs(sig*sig-cov[i*m+i]) > 1e-6*cov[i*m+i]+1e-9 {
			t.Errorf("σ² %g vs cov %g", sig*sig, cov[i*m+i])
		}
		e := (ts[i]-truth[i])*1e9 - cm
		if math.Abs(e) > 4*math.Sqrt(cov[i*m+i])+5 {
			t.Errorf("station %d: error %.1f ns, σ %.1f", qs[i].Station, e, math.Sqrt(cov[i*m+i]))
		}
		for j := range qs {
			if cov[i*m+j] != cov[j*m+i] {
				t.Errorf("cov not symmetric")
			}
		}
	}
	if err := linalg.Cholesky(append([]float64(nil), cov...), m); err != nil {
		t.Errorf("conversion covariance not PD: %v", err)
	}
	// wrong epoch / unknown station
	if _, _, ok := snap.NetTime(Query{Station: qs[5].Station, Epoch: 99, Ticks: qs[5].Ticks}); ok {
		t.Error("wrong epoch converted")
	}
	if _, _, ok := snap.NetTime(Query{Station: 999}); ok {
		t.Error("unknown station converted")
	}
	// ClockView
	v := View{F: f, Ref: &fakeRef{ok: true}}
	tn, sig, okv := v.NetTime(qs[5].Station, qs[5].Epoch, qs[5].Ticks)
	if !okv || math.Abs(tn-ts[5]*1e9) > 1 || sig <= 0 {
		t.Errorf("View.NetTime %v %v %v", tn, sig, okv)
	}
	b, okb := v.BaselineNs(w.sts[0].idx, w.sts[1].idx)
	if want := w.sts[0].pos.Dist(w.sts[1].pos) / cMPerNs; !okb || math.Abs(b-want) > 1e-6 {
		t.Errorf("baseline %v want %v", b, want)
	}
	if !v.SyncCandidate(1, 17) || v.SyncCandidate(1, 20) {
		t.Error("SyncCandidate")
	}
	// group.Clock form
	ns, frac, cv, jv, okU := snap.NetTimeUnix(qs[5])
	if !okU || frac < 0 || frac >= 1 || math.Abs(float64(ns-snap.OriginUnixNs)+frac-ts[5]*1e9) > 0.5 || math.Abs(cv-cov[5*m+5]) > 1e-6*cv || jv <= 0 {
		t.Errorf("NetTimeUnix %d %v %v %v", ns, frac, cv, jv)
	}
	if c := snap.CrossCov(qs[4].Station, qs[5].Station); c != cov[4*m+5] {
		// Convert evaluates at the frame time, CrossCov at the snapshot time
		if math.Abs(c-cov[4*m+5]) > 0.2*math.Sqrt(cov[4*m+4]*cov[5*m+5]) {
			t.Errorf("CrossCov %v vs %v", c, cov[4*m+5])
		}
	}
	d1, ok1 := snap.Domain(qs[0].Station)
	d2, ok2 := snap.Domain(qs[7].Station)
	if !ok1 || !ok2 || d1 != d2 {
		t.Errorf("domains %v %v", d1, d2)
	}
	if snap.Station(qs[6].Station).SyncedPeers() == 0 {
		t.Error("no synced peers")
	}
	// station stats
	for _, s := range w.sts {
		sc := snap.Station(s.idx)
		if !sc.Synced || sc.Accepted30s == 0 || (!sc.Anchor && len(sc.Peers) == 0) {
			t.Errorf("station %d stats: synced %v acc %d peers %d", s.idx, sc.Synced, sc.Accepted30s, len(sc.Peers))
		}
	}
}

// TestEpochChangeDropsOldMembers: members of the old epoch in the batch
// that announces a new epoch are not mixed with the new base.
func TestEpochChangeDropsOldMembers(t *testing.T) {
	w := newSynth(4)
	for i := 0; i < 3; i++ {
		w.addStation(model.ClockGPSUTC, 50+0.8*math.Cos(float64(i)*2), 9+1.2*math.Sin(float64(i)*2))
	}
	sdr := w.addStation(model.ClockSDR12, 50.1, 9.1)
	w.addStation(model.ClockHW12, 49.8, 8.7)
	w.addAircraft(40, 50, 9, 200)
	f, _ := New(Config{OriginUnixNs: synthOrigin, ProbationS: 30, Workers: 1})
	w.register(f)
	w.run(f, 0, 60, 100, 0.3, nil)
	// restart: new epoch, counter restarts, offset jumps by 0.37 s
	sdr.clk.epoch = 2
	sdr.clk.counter0 = 5e6
	sdr.clk.theta0 += 0.37
	f.StationEvent(sdr.idx, Event{OldEpoch: 1, NewEpoch: 2, Reason: model.ClockEvRestart})
	var back float64 = -1
	w.run(f, 60, 120, 100, 0.3, func(tE float64) {
		if back < 0 && tE > 60.5 {
			e, sg, ok := w.errNs(f, sdr, tE)
			cm, _, _ := w.errNs(f, w.sts[0], tE)
			sc := f.Snapshot().Station(sdr.idx)
			if ok && sc.Synced && math.Abs(e-cm) < 3*sg+5 {
				back = tE - 60
			}
		}
	})
	sc := f.Snapshot().Station(sdr.idx)
	t.Logf("re-synced %.1f s after the restart; events %+v", back, sc.Events)
	if back < 0 || back > 30 {
		t.Errorf("not re-synced within 30 s")
	}
	if f.Stats().Steps != 0 {
		t.Errorf("restart seen as %d steps", f.Stats().Steps)
	}
}
