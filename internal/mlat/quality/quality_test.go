package quality

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

func exactQ(v []float64, q float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	return s[int(q*float64(len(s)-1))]
}

func TestHistQuantile(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var h Hist
	var v []float64
	for i := 0; i < 20000; i++ {
		x := math.Exp(r.NormFloat64()*1.5 + 3)
		v = append(v, x)
		h.Add(x)
	}
	for _, q := range []float64{0.5, 0.95, 0.99} {
		got, want := h.Quantile(q), exactQ(v, q)
		if math.Abs(got/want-1) > 0.03 {
			t.Errorf("q%.2f: %.2f want %.2f", q, got, want)
		}
	}
	if f := h.FracBelow(exactQ(v, 0.5)); math.Abs(f-0.5) > 0.02 {
		t.Errorf("FracBelow median %.3f", f)
	}
}

// A consistent circular Gaussian error gives 63.2 % within σ_h and 98.2 %
// within 2σ_h, and the NEES fractions match the χ²(2) quantiles.
func TestErrorStatsCalibration(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var e ErrorStats
	t0 := int64(1.8e18)
	s := 20.0
	var errs []float64
	for i := 0; i < 20000; i++ {
		smp := Sample{TNs: t0 + int64(i)*int64(100e6), Lat: 51, Lon: 9, AltM: 9000,
			EastM: r.NormFloat64() * s, NorthM: r.NormFloat64() * s, CovEE: s * s, CovNN: s * s, Grade: model.GradeA,
			VelE: 200}
		errs = append(errs, smp.ErrH())
		e.Add(&smp)
	}
	now := t0 + 20000*int64(100e6)
	tot := e.Total(now)
	if tot.N != 20000 {
		t.Fatalf("n %d", tot.N)
	}
	if math.Abs(tot.Within1Sigma-0.632) > 0.015 || math.Abs(tot.Within2Sigma-0.982) > 0.005 {
		t.Errorf("calibration %.3f %.3f", tot.Within1Sigma, tot.Within2Sigma)
	}
	if math.Abs(tot.NEES68-0.6827) > 0.015 || math.Abs(tot.NEES95-0.95) > 0.01 {
		t.Errorf("NEES %.3f %.3f", tot.NEES68, tot.NEES95)
	}
	if math.Abs(tot.P50/exactQ(errs, 0.5)-1) > 0.03 || math.Abs(tot.P95/exactQ(errs, 0.95)-1) > 0.03 {
		t.Errorf("P50 %.1f/%.1f P95 %.1f/%.1f", tot.P50, exactQ(errs, 0.5), tot.P95, exactQ(errs, 0.95))
	}
	// cross-track of an east-bound aircraft is the north error
	if math.Abs(tot.CrossP50/(s*0.6745)-1) > 0.05 {
		t.Errorf("cross P50 %.1f want %.1f", tot.CrossP50, s*0.6745)
	}
	w := e.Window(now, 10*60e9)
	if w.N < 6000 || w.N > 6600 || math.Abs(w.PerMin-600) > 61 {
		t.Errorf("window n %d perMin %.1f", w.N, w.PerMin)
	}
}

func TestErrorStatsAlarm(t *testing.T) {
	var e ErrorStats
	t0 := int64(1.8e18)
	t0 -= t0 % hourNs
	week := 7 * 24 * hourNs
	for h := int64(0); h < 14*24; h++ {
		err := 10.0
		if h >= 7*24 {
			err = 14 // +40 %
		}
		for i := 0; i < 20; i++ {
			e.Add(&Sample{TNs: t0 + h*hourNs + int64(i), EastM: err, CovEE: 1, CovNN: 1})
		}
	}
	cur, last, alarm, ok := e.Alarm(t0+2*week-1, 100)
	if !ok || !alarm || cur < last*1.3 {
		t.Fatalf("alarm %v ok %v cur %.1f last %.1f", alarm, ok, cur, last)
	}
}

func TestHoldOutSelection(t *testing.T) {
	h := NewHoldOut(0.1, 10*time.Minute, 42, 9)
	now := int64(1.8e18)
	n, in := 0, 0
	for k := uint32(0x400000); k < 0x400000+20000; k++ {
		if h.Selected(k, now) {
			n++
			h.NoteNACp(k, 9, now)
			if h.In(k, now) {
				in++
			}
		}
	}
	if math.Abs(float64(n)/20000-0.1) > 0.01 || in != n {
		t.Fatalf("selected %d in %d", n, in)
	}
	// NACp 8 is not enough; a stale announcement does not count
	var k uint32
	for k = 0x500000; !h.Selected(k, now); k++ {
	}
	h.NoteNACp(k, 8, now)
	if h.In(k, now) {
		t.Fatal("NACp 8 in hold-out")
	}
	h.NoteNACp(k, 10, now)
	if !h.In(k, now) || h.In(k, now+61e9) {
		t.Fatal("NACp TTL")
	}
	// rotation changes the set, deterministic for the same salt
	same, h2 := 0, NewHoldOut(0.1, 10*time.Minute, 42, 9)
	for k := uint32(0x400000); k < 0x400000+20000; k++ {
		if h.Selected(k, now) != h2.Selected(k, now) {
			t.Fatal("not deterministic")
		}
		if h.Selected(k, now) && h.Selected(k, now+int64(10*time.Minute)) {
			same++
		}
	}
	if same > n/4 {
		t.Fatalf("rotation keeps %d of %d", same, n)
	}
	if NewHoldOut(0, 0, 1, 0).Selected(1, now) {
		t.Fatal("fraction 0 selects")
	}
}

// Residuals whose w has scale 2 under the jitter in use: the learned jitter
// explains a 4x variance (clamped to the ×4 prior).
func TestStationsLearning(t *testing.T) {
	q := NewStations()
	var flagged []model.StationIdx
	q.OnFlag = func(st model.StationIdx, _ float64) { flagged = append(flagged, st) }
	r := rand.New(rand.NewPCG(5, 6))
	prior := func(st model.StationIdx) float64 { return 400 }
	t0 := int64(1.8e18)
	for i := 0; i < 3000; i++ {
		now := t0 + int64(i)*int64(2e9) // 100 minutes
		w1 := r.NormFloat64() * 2
		w2 := r.NormFloat64()
		w3 := r.NormFloat64() + 1 // biased by 1σ
		q.Observe(now, []Residual{
			{Station: 1, W: w1, ResidNs: w1 * 300, ClockVarNs2: 100 * 100, JitterVarNs2: 300 * 300},
			{Station: 2, W: w2, ResidNs: w2 * 300, ClockVarNs2: 100 * 100, JitterVarNs2: 300 * 300},
			{Station: 3, W: w3, ResidNs: w3 * 300, ClockVarNs2: 100 * 100, JitterVarNs2: 300 * 300},
			{Station: 4, Excluded: true},
		}, prior)
	}
	now := t0 + 3000*int64(2e9)
	r1, _ := q.Report(1, now)
	r2, _ := q.Report(2, now)
	r3, _ := q.Report(3, now)
	r4, _ := q.Report(4, now)
	// station 1: s = 2 → J' = 4(300²+100²) − 100² → 624 ns
	if math.Abs(r1.WScale-2) > 0.2 || math.Abs(r1.LearnedJitterNs-624) > 70 {
		t.Errorf("st1 scale %.2f jitter %.0f", r1.WScale, r1.LearnedJitterNs)
	}
	if math.Abs(r2.WScale-1) > 0.1 || math.Abs(r2.LearnedJitterNs-300) > 40 {
		t.Errorf("st2 scale %.2f jitter %.0f", r2.WScale, r2.LearnedJitterNs)
	}
	if !r3.BiasFlag || r1.BiasFlag || r2.BiasFlag || len(flagged) != 1 || flagged[0] != 3 {
		t.Errorf("bias flags %v %v %v (%v)", r1.BiasFlag, r2.BiasFlag, r3.BiasFlag, flagged)
	}
	if math.Abs(r3.ResidualBiasNs-300) > 100 {
		t.Errorf("st3 bias %.0f", r3.ResidualBiasNs)
	}
	if r4.ExcludedPct != 100 || r4.ContribPerHour != 0 || r1.ContribPerHour < 1740 || r1.ContribPerHour > 1800 {
		t.Errorf("st4 %+v st1 contrib %d", r4, r1.ContribPerHour)
	}
	if r1.OutlierPct < 3 || r2.OutlierPct > 0.1 {
		t.Errorf("outliers %.2f %.2f", r1.OutlierPct, r2.OutlierPct)
	}
}

func TestVerifyTDOA(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	ac := geo.ToECEF(51.2, 9.3, 10000)
	var toas []TOA
	sig := make([]float64, 6)
	for i := 0; i < 6; i++ {
		p := geo.ToECEF(51+0.5*math.Cos(float64(i)), 9+0.8*math.Sin(float64(i)), 200)
		toas = append(toas, TOA{Pos: p})
		sig[i] = 50e-9
	}
	rng := func(rx, a geo.Vec3) float64 { return rx.Dist(a) }
	tau := 0.123
	set := func(p geo.Vec3) {
		for i := range toas {
			toas[i].T = tau + rng(toas[i].Pos, p)/geo.C + r.NormFloat64()*50e-9
		}
	}
	cov := [9]float64{100, 0, 0, 0, 100, 0, 0, 0, 400}
	conf, contra := 0, 0
	for k := 0; k < 500; k++ {
		set(ac)
		v, _, dof, _ := VerifyTDOA(ac, cov, toas, nil, sig, rng, VerifyParams{})
		if dof != 5 {
			t.Fatal("dof")
		}
		switch v {
		case VerdictConfirmed:
			conf++
		case VerdictContradicted:
			contra++
		}
	}
	if conf < 490 || contra != 0 {
		t.Fatalf("truth: confirmed %d contradicted %d", conf, contra)
	}
	// reported position 2 km off: contradicted
	fr := geo.NewFrameECEF(ac)
	off := fr.FromENU(geo.Vec3{X: 2000})
	contra = 0
	for k := 0; k < 100; k++ {
		set(off)
		if v, _, _, _ := VerifyTDOA(ac, cov, toas, nil, sig, rng, VerifyParams{}); v == VerdictContradicted {
			contra++
		}
	}
	if contra < 99 {
		t.Fatalf("2 km off: contradicted %d/100", contra)
	}
	v := NewVerifier()
	now := int64(1.8e18)
	if v.Record(1, now, VerdictContradicted) || v.Record(1, now+1e9, VerdictContradicted) {
		t.Fatal("suspect too early")
	}
	if !v.Record(1, now+2e9, VerdictContradicted) {
		t.Fatal("not suspect after 3")
	}
	if s, _ := v.State(1, now+3e9); !s {
		t.Fatal("state")
	}
	v.Record(1, now+4e9, VerdictConfirmed)
	if s, ver := v.State(1, now+5e9); s || !ver {
		t.Fatal("confirmation clears")
	}
}

func TestKAtmSlope(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	var k KAtm
	t0 := int64(1.8e18)
	t0 -= t0 % hourNs
	// model k = 1, true k* = 1.1: residual = (1 − 1.1)·E
	for f := 0; f < 2000; f++ {
		ex := make([]float64, 6)
		res := make([]float64, 6)
		sg := make([]float64, 6)
		tau := r.NormFloat64() * 100
		for i := range ex {
			ex[i] = 2 + 30*r.Float64()
			res[i] = -0.1*ex[i] + tau + r.NormFloat64()*1
			sg[i] = 1
		}
		k.AddFix(t0+int64(f)*int64(1e9), ex, res, sg)
	}
	k.Flush()
	est, s, hours, ok := k.Estimate(1)
	if !ok || hours != 1 || math.Abs(est-1.1) > 0.01 || s <= 0 {
		t.Fatalf("k %.3f σ %.4f ok %v", est, s, ok)
	}
}

func TestCRLBMap(t *testing.T) {
	var sts []CRLBStation
	for i := 0; i < 8; i++ {
		a := float64(i) / 8 * 2 * math.Pi
		sts = append(sts, CRLBStation{Pos: geo.ToECEF(51+0.7*math.Cos(a), 9+1.1*math.Sin(a), 200), SigmaS: 30e-9})
	}
	cells := CRLBMap(sts, CRLBParams{StepDeg: 0.5})
	if len(cells) == 0 {
		t.Fatal("empty")
	}
	var centre, edge float64 = math.NaN(), math.NaN()
	for _, c := range cells {
		if c.AltM == 9144 && c.Lat == 51 && c.Lon == 9 {
			centre = c.SigmaHM
		}
		if c.AltM == 9144 && c.Lat == 51 && c.Lon == 13 {
			edge = c.SigmaHM
		}
	}
	if !(centre < 15) || !(edge > 2*centre) {
		t.Fatalf("centre %.1f edge %.1f", centre, edge)
	}
}

func TestCells(t *testing.T) {
	c := NewCells()
	for i := 0; i < 100; i++ {
		s := Sample{TNs: int64(i), Lat: 51.5, Lon: 9.5, AltM: 9000, EastM: float64(i)}
		c.Add(&s, []model.StationIdx{model.StationIdx(i % 7)})
	}
	if l := c.List(10, 5); len(l) != 1 || l[0].Stations != 7 || l[0].Band != BandHigh || l[0].Lat != 51 {
		t.Fatalf("%+v", l)
	}
	if l := c.List(10, 8); len(l) != 0 {
		t.Fatal("station threshold")
	}
	if c.Prune(1000) != 1 {
		t.Fatal("prune")
	}
}
