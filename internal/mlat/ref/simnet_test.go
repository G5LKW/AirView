package ref

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/modes"
	"airview/internal/sim/simnet"
)

// truthQuery is a transmission whose reference state is evaluated after the
// processing delay and compared with the simulator's truth.
type truthQuery struct {
	icao uint32
	txS  float64
	pos  geo.Vec3
	lat  float64
	lon  float64
	df   uint8
	pos2 bool // an airborne position squitter
	lat0 float64
}

type errSample struct {
	h, v  float64
	nees  float64 // horizontal normalised error squared (2 dof)
	fk    float64 // horizontal error / smoother-only 1σ
	t1    bool
	usabl bool
	short bool
	extra bool
	posM  bool
	flags Flags
	age   float64
}

// harness feeds simnet transmissions into a Store at their true transmit
// instant (plus optional timing noise) and evaluates the reference state of
// every transmission of a tracked aircraft DelayS later against truth.
type harness struct {
	n       *simnet.Network
	s       *Store
	seen    map[uint64]struct{}
	seenOld map[uint64]struct{}
	queue   []truthQuery
	delayS  float64
	txNoise float64 // 1σ of the transmit-instant error fed to Observe, s
	rng     *rand.Rand
	errs    []errSample
	msg     modes.Message
	results map[Result]int
	// fix feeding for the latency estimator: every fixEveryS per aircraft
	// a truth position with fixSigmaM noise is passed to ObserveFix
	fixEveryS float64
	fixSigmaM float64
	lastFix   map[uint32]float64
	warmS     float64
	latency   map[uint32]float64 // true latency per aircraft (from truth)
}

func newHarness(n *simnet.Network, s *Store, delayS float64) *harness {
	return &harness{n: n, s: s, seen: map[uint64]struct{}{}, seenOld: map[uint64]struct{}{},
		delayS: delayS, rng: rand.New(rand.NewPCG(7, 9)), results: map[Result]int{},
		lastFix: map[uint32]float64{}, latency: map[uint32]float64{}, warmS: 60}
}

func (h *harness) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if _, ok := h.seen[o.Seq]; ok {
			continue
		}
		if _, ok := h.seenOld[o.Seq]; ok {
			continue
		}
		h.seen[o.Seq] = struct{}{}
		if len(h.seen) > 200000 {
			h.seenOld, h.seen = h.seen, map[uint64]struct{}{}
		}
		tx := o.Tx
		txNs := h.n.UnixNs(tx.TxS + h.rng.NormFloat64()*h.txNoise)
		if err := modes.Decode(tx.Data, &h.msg); err == nil {
			r := h.s.Observe(&h.msg, txNs, h.txNoise*1e9)
			h.results[r]++
		}
		if tx.Position {
			h.latency[tx.ICAO] = tx.PosLatencyS
		}
		h.queue = append(h.queue, truthQuery{icao: tx.ICAO, txS: tx.TxS, pos: gv(tx.Pos), lat: tx.Lat, lon: tx.Lon,
			df: tx.Data[0] >> 3, pos2: tx.Position || (tx.Data[0]>>3 == 17 && len(tx.Data) == 14 && tx.Data[4]>>3 >= 9 && tx.Data[4]>>3 <= 18)})
		if h.fixEveryS > 0 && tx.TxS-h.lastFix[tx.ICAO] >= h.fixEveryS && h.s.Has(tx.ICAO) {
			h.lastFix[tx.ICAO] = tx.TxS
			p := gv(tx.Pos).Add(geo.Vec3{X: h.rng.NormFloat64(), Y: h.rng.NormFloat64(), Z: h.rng.NormFloat64()}.Scale(h.fixSigmaM))
			s2 := h.fixSigmaM * h.fixSigmaM
			h.s.ObserveFix(tx.ICAO, h.n.UnixNs(tx.TxS), p, [9]float64{s2, 0, 0, 0, s2, 0, 0, 0, s2})
		}
	}
}

func (h *harness) Advance(hor float64) {
	k := 0
	for ; k < len(h.queue) && h.queue[k].txS <= hor-h.delayS; k++ {
		q := &h.queue[k]
		if q.txS < h.warmS {
			continue
		}
		var st State
		if !h.s.At(q.icao, h.n.UnixNs(q.txS), 0, &st) || st.Flags&FlagConfirmed == 0 {
			continue
		}
		f := geo.NewFrame(geo.LLH{Lat: q.lat, Lon: q.lon})
		d := f.Rotate(st.Pos.Sub(q.pos))
		c := f.CovToENU(st.Cov)
		nees := 0.0
		if det := c[0]*c[4] - c[1]*c[3]; det > 0 {
			nees = (c[4]*d.X*d.X - 2*c[1]*d.X*d.Y + c[0]*d.Y*d.Y) / det
		}
		h.errs = append(h.errs, errSample{h: math.Hypot(d.X, d.Y), v: math.Abs(d.Z), nees: nees, fk: math.Hypot(d.X, d.Y) / st.SigmaFilterH,
			t1: st.Flags&FlagT1 != 0, usabl: st.Usable(), short: q.df < 16, extra: st.Flags&FlagExtrapolated != 0,
			posM: q.pos2, flags: st.Flags, age: st.PosAgeS})
	}
	h.queue = h.queue[k:]
}

func gv(v simnet.Vec3) geo.Vec3 { return geo.Vec3{X: v[0], Y: v[1], Z: v[2]} }

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

type errReport struct {
	n                  int
	h50, h95, h99, hMx float64
	v50, v95           float64
	in1, in2           float64 // fraction of horizontal errors within the 1σ / 2σ ellipse
	f1, f2             float64 // the same for the smoother-only σ (isotropic approximation)
}

func (r errReport) String() string {
	return fmt.Sprintf("n=%d horiz P50 %.2f P95 %.2f P99 %.2f max %.1f m; vert P50 %.2f P95 %.2f m; calib 1σ %.0f%% 2σ %.0f%% (smoother-only %.0f%% / %.0f%%)",
		r.n, r.h50, r.h95, r.h99, r.hMx, r.v50, r.v95, 100*r.in1, 100*r.in2, 100*r.f1, 100*r.f2)
}

func report(es []errSample, keep func(*errSample) bool) errReport {
	var hs, vs []float64
	in1, in2, f1, f2 := 0, 0, 0, 0
	for i := range es {
		e := &es[i]
		if keep != nil && !keep(e) {
			continue
		}
		hs = append(hs, e.h)
		vs = append(vs, e.v)
		// 2-dof chi-square: P(χ² ≤ 2.2789) = 68.27 %, P(χ² ≤ 6.1801) = 95.45 %
		if e.nees <= 2.2789 {
			in1++
		}
		if e.nees <= 6.1801 {
			in2++
		}
		// isotropic 2-D: P(r ≤ kσ) = 1 − exp(−k²/2)
		if e.fk <= 1.5151 {
			f1++
		}
		if e.fk <= 2.4860 {
			f2++
		}
	}
	r := errReport{n: len(hs), h50: pct(hs, 50), h95: pct(hs, 95), h99: pct(hs, 99), hMx: pct(hs, 100), v50: pct(vs, 50), v95: pct(vs, 95)}
	if len(hs) > 0 {
		n := float64(len(hs))
		r.in1, r.in2, r.f1, r.f2 = float64(in1)/n, float64(in2)/n, float64(f1)/n, float64(f2)/n
	}
	return r
}

func runSim(t testing.TB, cfg simnet.Config, opt Options, dur time.Duration, delayS float64, tune func(*harness)) *harness {
	t.Helper()
	if cfg.Receivers == nil {
		cfg.Receivers = simnet.RandomReceivers(cfg.Seed, 8, simnet.Placement{CenterLat: 50.5, CenterLon: 8, RadiusKm: 150})
	}
	n, err := simnet.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(n, New(opt, nil), delayS)
	if tune != nil {
		tune(h)
	}
	n.AddSink(h)
	if err := n.Advance(context.Background(), dur); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestSimnetInterpolation is the WP-RF acceptance gate: reference states
// interpolated to the true transmit instant of every message of an ADS-B
// aircraft (DF17 positions/velocities/identity and also DF11/DF0/4/5/20/21)
// compared with simnet truth. Position latency and NACp error are switched
// off so that the error is the interpolation (and CPR/altitude
// quantisation) alone.
func TestSimnetInterpolation(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cfg := simnet.Config{Seed: 11, Aircraft: 200, PosLatencyMaxMs: -1, PosErrorScale: -1}
	h := runSim(t, cfg, Options{}, 6*time.Minute, 0.4, nil)
	all := report(h.errs, nil)
	usable := report(h.errs, func(e *errSample) bool { return e.usabl })
	short := report(h.errs, func(e *errSample) bool { return e.usabl && e.short })
	t1 := report(h.errs, func(e *errSample) bool { return e.usabl && e.t1 })
	t0 := report(h.errs, func(e *errSample) bool { return e.usabl && !e.t1 })
	t.Logf("results: %v", h.results)
	t.Logf("all confirmed: %v", all)
	t.Logf("usable:        %v", usable)
	t.Logf("usable DF0/4/5/11: %v", short)
	t.Logf("usable T=1:    %v", t1)
	t.Logf("usable T=0:    %v", t0)
	t.Logf("store: %+v", h.s.Stats())
	if usable.n < 20000 {
		t.Fatalf("too few usable samples: %d", usable.n)
	}
	if !(usable.h95 < 5) {
		t.Errorf("horizontal interpolation P95 %.2f m, gate < 5 m", usable.h95)
	}
	if !(short.h95 < 5) {
		t.Errorf("DF0/4/5/11 horizontal interpolation P95 %.2f m, gate < 5 m", short.h95)
	}
	if !(usable.v95 < 10) {
		t.Errorf("vertical P95 %.2f m (25 ft quantisation of altitude and GNSS delta)", usable.v95)
	}
}

// TestSimnetImmediate measures the same with no processing delay: the
// instant is at or just after the newest report, so the answer is mostly a
// forward prediction.
func TestSimnetImmediate(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cfg := simnet.Config{Seed: 12, Aircraft: 150, PosLatencyMaxMs: -1, PosErrorScale: -1}
	h := runSim(t, cfg, Options{}, 4*time.Minute, 0, nil)
	usable := report(h.errs, func(e *errSample) bool { return e.usabl })
	t.Logf("usable, no delay: %v", usable)
	if !(usable.h95 < 8) {
		t.Errorf("horizontal P95 %.2f m without delay", usable.h95)
	}
}

// TestSimnetLatency: with the default simnet position latency (0..300 ms
// per airframe) the uncorrected T=0 track lags by up to 75 m; ObserveFix
// with noisy independent fixes (σ 30 m every 2 s, e.g. hold-out MLAT)
// learns the latency and brings the error back to the interpolation level.
func TestSimnetLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cfg := simnet.Config{Seed: 13, Aircraft: 150, PosErrorScale: -1}
	base := runSim(t, cfg, Options{}, 4*time.Minute, 0.4, nil)
	b := report(base.errs, func(e *errSample) bool { return e.usabl && !e.t1 })
	t.Logf("T=0 without latency estimation: %v", b)
	h := runSim(t, cfg, Options{}, 8*time.Minute, 0.4, func(h *harness) {
		h.fixEveryS, h.fixSigmaM = 2, 30
		h.warmS = 300
	})
	r := report(h.errs, func(e *errSample) bool { return e.usabl && !e.t1 && e.flags&FlagLatencyEstimated != 0 })
	t.Logf("T=0 with ObserveFix (σ 30 m / 2 s), after 300 s: %v", r)
	if !(r.h50 < 5) || !(r.h95 < 15) {
		t.Errorf("latency-corrected T=0 error too large: %v", r)
	}
	if !(r.h95 < b.h95/2) {
		t.Errorf("latency estimation did not help: %.1f vs %.1f", r.h95, b.h95)
	}
}

// TestSimnetCalibration: with the full simnet impairments (NACp-consistent
// position error, latency) the reported covariance must cover the errors.
func TestSimnetCalibration(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	cfg := simnet.Config{Seed: 14, Aircraft: 150}
	h := runSim(t, cfg, Options{}, 4*time.Minute, 0.4, nil)
	r := report(h.errs, func(e *errSample) bool { return e.usabl })
	t.Logf("full impairments, usable: %v", r)
	if r.in1 < 0.55 || r.in1 > 0.9 || r.in2 < 0.9 {
		t.Errorf("covariance calibration off: %.0f%% within 1σ, %.0f%% within 2σ (want ≈68 / ≈95)", 100*r.in1, 100*r.in2)
	}
	cfg.PosLatencyMaxMs = -1
	h = runSim(t, cfg, Options{}, 4*time.Minute, 0.4, nil)
	r = report(h.errs, func(e *errSample) bool { return e.usabl })
	t.Logf("NACp error only, usable: %v", r)
	if r.in1 < 0.55 || r.in2 < 0.9 {
		t.Errorf("covariance calibration off: %.0f%% within 1σ, %.0f%% within 2σ", 100*r.in1, 100*r.in2)
	}
}
