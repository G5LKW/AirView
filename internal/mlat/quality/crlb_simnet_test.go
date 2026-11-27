package quality

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
	"airview/internal/sim/simnet"
)

// crlbBin collects fixes whose map prediction falls into one σ band.
type crlbBin struct {
	lo, hi float64
	ratio  []float64 // r / σ_map
	errs   []float64 // r
	act    []float64 // r / s_h of the receiving stations
	far    []float64 // farthest used station, km
	nrx    []float64 // receptions
	nmap   []float64 // map stations − used stations
}

func medOf(v []float64) float64 {
	w := slices.Clone(v)
	slices.Sort(w)
	if len(w) == 0 {
		return math.NaN()
	}
	return w[len(w)/2]
}

// TestCRLBSimnet validates the CRLB admin map against the solver's real
// errors on simnet (default device mix: SDR-heavy, multipath, gross
// outliers, sync error).
//
// Every 3rd transmission heard by >= 4 stations is solved as a Mode S-only
// snapshot fix (altitude row σ 30 m) from up to 12 of the receiving
// stations chosen as the planner chooses them (selectDOptimal at the true
// position) and scored against the truth:
//
//   - CRLB of the stations that actually received (the bound itself):
//     with honest σ, r <= σ_h for 63.2 % and r <= 2σ_h for 98.2 % of
//     fixes (Rayleigh). Gate: 55-75 % and >= 90 %.
//   - The map's prediction at the true position (CRLBIndex.SigmaAt: every
//     station within the radio horizon assumed to hear): the map is a
//     geometry bound, so it is optimistic where far stations miss the
//     message. Gates: the median of r / σ_map lies in 0.5-1.5 for fixes the
//     map rates <= 250 m, the empirical median error grows from band to
//     band (the map ranks coverage correctly), and the map has a value for
//     >= 95 % of solved fixes.
func TestCRLBSimnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	atm := geo.DefaultAtmosphere()
	sv := solve.NewSolver(solve.Options{Atmosphere: atm})
	rng := rand.New(rand.NewPCG(21, 4))
	var ix *CRLBIndex
	var f solve.Fix
	bins := []*crlbBin{{lo: 0, hi: 20}, {lo: 20, hi: 50}, {lo: 50, hi: 100}, {lo: 100, hi: 250}, {lo: 250, hi: math.Inf(1)}}
	var fixes, noMap, in1, in2 int
	var actRatio []float64
	var cand []geo.Vec3
	var candSig []float64
	var pick []int
	var h *harnessQ
	h = newHarnessQ(t, hOpts{seed: 21, receivers: 60, radiusKm: 250, aircraft: 250, minRows: 4}, func(tx *hTx) {
		if tx.seq%3 != 0 {
			return
		}
		// the planner's choice among the receiving stations: the 64 nearest,
		// then greedy D-optimal up to 12
		slices.SortFunc(tx.rows, func(a, b hRow) int {
			da, db := a.pos.Dist(tx.pos), b.pos.Dist(tx.pos)
			if da < db {
				return -1
			}
			if da > db {
				return 1
			}
			return 0
		})
		cand, candSig = cand[:0], candSig[:0]
		for i := range tx.rows {
			cand = append(cand, tx.rows[i].pos)
			candSig = append(candSig, tx.rows[i].sigmaS)
		}
		pick = selectDOptimal(cand, candSig, tx.pos, 12, pick[:0])
		var st [12]solve.Station
		var pos [12]geo.Vec3
		var sig [12]float64
		epoch := math.Floor(tx.txS)
		var far float64
		for k, i := range pick {
			r := &tx.rows[i]
			st[k] = solve.Station{Pos: r.pos, TOA: r.toaS - epoch, Sigma: r.sigmaS, ID: int32(r.rx)}
		}
		var pr solve.Problem
		pr.Stations = st[:len(pick)]
		pr.EpochNs = int64(epoch * 1e9)
		pr.Alt = solve.Altitude{Valid: true, HAE: tx.hae + rng.NormFloat64()*30, Sigma: 30}
		if err := sv.Solve(&pr, &f); err != nil || !f.OK() || f.Flags&solve.FlagAmbiguous != 0 {
			return
		}
		e, n, _ := enuErr(f.ECEF, tx.pos)
		r := math.Hypot(e, n)
		fixes++
		m := 0
		for k := range pick {
			if f.Used&(1<<k) == 0 {
				continue
			}
			pos[m], sig[m] = st[k].Pos, st[k].Sigma
			far = math.Max(far, st[k].Pos.Dist(tx.pos))
			m++
		}
		shAct := math.NaN()
		if enu, _, ok := solve.PredictCRLB(pos[:m], sig[:m], tx.pos, 30, &atm); ok {
			shAct = math.Sqrt(enu[0] + enu[4])
			actRatio = append(actRatio, r/shAct)
			if r <= shAct {
				in1++
			}
			if r <= 2*shAct {
				in2++
			}
		}
		sm, nm, ok := ix.SigmaAt(tx.lat, tx.lon, tx.hae)
		if !ok {
			noMap++
			return
		}
		for _, b := range bins {
			if sm >= b.lo && sm < b.hi {
				b.ratio = append(b.ratio, r/sm)
				b.errs = append(b.errs, r)
				b.act = append(b.act, r/shAct)
				b.far = append(b.far, far/1e3)
				b.nrx = append(b.nrx, float64(len(tx.rows)))
				b.nmap = append(b.nmap, float64(nm)-float64(m))
			}
		}
	})
	var sts []CRLBStation
	for i := range h.rxPos {
		sts = append(sts, CRLBStation{Pos: h.rxPos[i], SigmaS: math.Sqrt(h.rxSig[i]*h.rxSig[i]+h.syncSig[i]*h.syncSig[i]+100) * 1e-9})
	}
	ix = NewCRLBIndex(sts, CRLBParams{Atmosphere: &atm})
	h.run(t, 3*time.Minute)

	nAct := len(actRatio)
	slices.Sort(actRatio)
	var b strings.Builder
	fmt.Fprintf(&b, "fixes %d; CRLB of the receiving stations: r<=σh %.1f%%  r<=2σh %.1f%%  median r/σh %.2f (Rayleigh 0.83)\n",
		fixes, 100*float64(in1)/float64(nAct), 100*float64(in2)/float64(nAct), actRatio[nAct/2])
	fmt.Fprintf(&b, "map has no value for %d fixes (%.2f%%)\n", noMap, 100*float64(noMap)/float64(fixes))
	var prevMed float64
	for i, bn := range bins {
		if len(bn.ratio) == 0 {
			fmt.Fprintf(&b, "σ_map %4.0f-%4.0f m: no fixes\n", bn.lo, bn.hi)
			continue
		}
		slices.Sort(bn.ratio)
		slices.Sort(bn.errs)
		med := bn.errs[len(bn.errs)/2]
		fmt.Fprintf(&b, "σ_map %4.0f-%4.0f m: fixes %6d  median r %7.1f m  P95 r %7.1f m  median r/σ_map %.2f  r/σ_rx %.2f  receptions %.0f  farthest used %.0f km  map−used %.0f\n",
			bn.lo, bn.hi, len(bn.errs), med, bn.errs[int(0.95*float64(len(bn.errs)))], bn.ratio[len(bn.ratio)/2],
			medOf(bn.act), medOf(bn.nrx), medOf(bn.far), medOf(bn.nmap))
		if len(bn.ratio) >= 200 {
			if bn.hi <= 250 {
				if mr := bn.ratio[len(bn.ratio)/2]; mr < 0.5 || mr > 1.5 {
					t.Errorf("band %.0f-%.0f m: median r/σ_map %.2f outside 0.5-1.5", bn.lo, bn.hi, mr)
				}
			}
			if i > 0 && prevMed > 0 && med < prevMed {
				t.Errorf("band %.0f-%.0f m: median error %.1f m below the previous band's %.1f m", bn.lo, bn.hi, med, prevMed)
			}
			prevMed = med
		}
	}
	t.Log(b.String())
	if fixes < 5000 {
		t.Fatalf("only %d fixes", fixes)
	}
	if p := float64(in1) / float64(nAct); p < 0.55 || p > 0.75 {
		t.Errorf("1σ calibration %.3f outside 0.55-0.75", p)
	}
	if p := float64(in2) / float64(nAct); p < 0.90 {
		t.Errorf("2σ calibration %.3f below 0.90", p)
	}
	if p := float64(noMap) / float64(fixes); p > 0.05 {
		t.Errorf("map missing for %.1f%% of fixes", 100*p)
	}
}

// TestCRLBIndexMatchesMap checks that the point query, the serial map and
// the parallel map agree.
func TestCRLBIndexMatchesMap(t *testing.T) {
	var sts []CRLBStation
	rng := rand.New(rand.NewPCG(3, 3))
	for i := 0; i < 40; i++ {
		sts = append(sts, CRLBStation{Pos: geo.ToECEF(49+rng.Float64()*4, 6+rng.Float64()*6, 100+rng.Float64()*400), SigmaS: (20 + rng.Float64()*80) * 1e-9})
	}
	p := CRLBParams{StepDeg: 0.5}
	ix := NewCRLBIndex(sts, p)
	serial := ix.Map()
	p.Workers = 4
	par := CRLBMap(sts, p)
	if len(serial) == 0 || len(serial) != len(par) {
		t.Fatalf("serial %d parallel %d", len(serial), len(par))
	}
	for i := range serial {
		if serial[i] != par[i] {
			t.Fatalf("cell %d: %+v vs %+v", i, serial[i], par[i])
		}
		s, n, ok := ix.SigmaAt(serial[i].Lat, serial[i].Lon, serial[i].AltM)
		if !ok || s != serial[i].SigmaHM || n != serial[i].N {
			t.Fatalf("SigmaAt %v %v %v vs %+v", s, n, ok, serial[i])
		}
	}
	sum := SummarizeCRLB(serial)
	if len(sum) != 3 || sum[0].AltM > sum[2].AltM || sum[2].Cells < sum[0].Cells || sum[2].P50M <= 0 {
		t.Fatalf("summary %+v", sum)
	}
	if NewCRLBIndex(sts[:2], p) != nil || CRLBMap(nil, p) != nil {
		t.Fatal("index with < 3 stations")
	}
	if _, _, ok := (*CRLBIndex)(nil).SigmaAt(50, 9, 1000); ok {
		t.Fatal("nil index")
	}
}

// BenchmarkCRLBMap measures the full admin map for 1,000 stations spread
// over central Europe (radius 1,000 km) at the defaults (0.25°, three
// altitudes), serial and with 8 workers. Budget: the engine computes it
// off the request path on station changes; < 30 s serial.
func BenchmarkCRLBMap(b *testing.B) {
	specs := simnet.RandomReceivers(7, 1000, simnet.Placement{CenterLat: 50, CenterLon: 10, RadiusKm: 1000})
	var sts []CRLBStation
	for _, s := range specs {
		sts = append(sts, CRLBStation{Pos: geo.ToECEF(s.Lat, s.Lon, s.GroundHAE+s.AntennaAGL), SigmaS: 60e-9})
	}
	for _, w := range []int{1, 8} {
		b.Run(fmt.Sprintf("workers%d", w), func(b *testing.B) {
			var cells int
			for i := 0; i < b.N; i++ {
				cells = len(CRLBMap(sts, CRLBParams{Workers: w}))
			}
			b.ReportMetric(float64(cells), "cells")
		})
	}
}
