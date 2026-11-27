package trajectory

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
)

func TestFrameRoundTripAndScale(t *testing.T) {
	for _, o := range [][2]float64{{51.5, -0.1}, {0, 0}, {-33.9, 151.2}, {64.1, -21.9}, {47.4, 179.9}} {
		f := newFrame(o[0], o[1])
		for _, d := range []float64{0, 100, 5000, 20000, 60000, 100000} {
			for brg := 0.0; brg < 360; brg += 30 {
				lat, lon, _ := geo.VincentyDirect(o[0], o[1], brg, d)
				x, y := f.toXY(lat, lon)
				// distance and azimuth from the origin within 1e-4 relative / 0.01 deg
				r := math.Hypot(x, y)
				if math.Abs(r-d) > 1e-4*d+0.01 {
					t.Fatalf("origin %v d=%v brg=%v: plane distance %v", o, d, brg, r)
				}
				if d > 0 {
					if a := math.Abs(angDiff(trackOf(x, y), brg)); a > 0.01 {
						t.Fatalf("origin %v d=%v brg=%v: azimuth off by %v", o, d, brg, a)
					}
				}
				la2, lo2 := f.toLatLon(x, y)
				if math.Abs(la2-lat) > 1e-9 || math.Abs(angDiff(lo2, lon)) > 1e-9 {
					t.Fatalf("round trip %v,%v -> %v,%v", lat, lon, la2, lo2)
				}
			}
		}
	}
}

func TestGridConvergenceRecenter(t *testing.T) {
	f := newFrame(60, 10)
	// 50 km east at 60N: grid north differs from true north by about dLon*sin(lat)
	lat, lon, _ := geo.VincentyDirect(60, 10, 90, 50000)
	g := f.gridConvergence(lat, lon) * rad2deg
	want := (lon - 10) * math.Sin(60*deg2rad)
	if math.Abs(math.Abs(g)-want) > 0.02 {
		t.Fatalf("convergence %v want +-%v", g, want)
	}
	// re-centring keeps the true track of the velocity
	var fl imm
	x := vec5{}
	x[ix], x[iy] = f.toXY(lat, lon)
	x[ivx], x[ivy] = 0, 200 // grid north
	P := ident5()
	fl.init(f, 0, x, P)
	fl.recenter()
	trueTrk := trackOf(fl.x[ivx], fl.x[ivy])
	gridTrk := 0.0
	if math.Abs(angDiff(trueTrk, normDeg(gridTrk-g))) > 0.02 {
		t.Fatalf("after recenter track %v, want %v", trueTrk, normDeg(-g))
	}
	if math.Hypot(fl.x[ix], fl.x[iy]) > 0.01 {
		t.Fatalf("not centred: %v %v", fl.x[ix], fl.x[iy])
	}
}

func TestMotionJacobian(t *testing.T) {
	for _, w := range []float64{0, 1e-6, 0.01, -0.05, 0.2} {
		x := vec5{100, -50, 120, 180, w}
		var F mat5
		y := x
		motion(&y, 7, w, 10, -5, 1, &F)
		for j := 0; j < nx; j++ {
			h := 1e-6 * math.Max(1, math.Abs(x[j]))
			xp, xm := x, x
			xp[j] += h
			xm[j] -= h
			motion(&xp, 7, xp[iw], 10, -5, 1, nil)
			motion(&xm, 7, xm[iw], 10, -5, 1, nil)
			for i := 0; i < nx; i++ {
				num := (xp[i] - xm[i]) / (2 * h)
				if math.Abs(num-F[i][j]) > 1e-3*math.Max(1, math.Abs(num)) {
					t.Fatalf("w=%v dF[%d][%d]: analytic %v numeric %v", w, i, j, F[i][j], num)
				}
			}
		}
	}
	// a full circle returns to the start: 360 deg at 3 deg/s
	x := vec5{0, 0, 0, 100, 3 * deg2rad}
	motion(&x, 120, x[iw], 0, 0, 0, nil)
	if math.Hypot(x[ix], x[iy]) > 1e-6 || math.Abs(x[ivy]-100) > 1e-9 {
		t.Fatalf("circle not closed: %v", x)
	}
	// right turn from north goes east
	x = vec5{0, 0, 0, 100, 3 * deg2rad}
	motion(&x, 30, x[iw], 0, 0, 0, nil)
	if !(x[ix] > 0 && math.Abs(trackOf(x[ivx], x[ivy])-90) < 1e-9) {
		t.Fatalf("right turn wrong: %v", x)
	}
}

func TestEllipse(t *testing.T) {
	e := ellipse(100*100, 0, 10*10)
	if math.Abs(e.SemiMajorM-100) > 1e-9 || math.Abs(e.SemiMinorM-10) > 1e-9 || math.Abs(e.OrientDeg-90) > 1e-9 {
		t.Fatalf("east-west ellipse %+v", e)
	}
	e = ellipse(10*10, 0, 100*100)
	if math.Abs(e.OrientDeg-0) > 1e-9 {
		t.Fatalf("north-south ellipse %+v", e)
	}
	// 45 deg (NE) correlated
	e = ellipse(50, 49, 50)
	if math.Abs(e.OrientDeg-45) > 1e-6 {
		t.Fatalf("NE ellipse %+v", e)
	}
}

func straight() scenario {
	return scenario{lat: 51.0, lon: 4.0, alt: 36000, hdg: 75, tasKt: 450, dur: 600, posSigma: 8, velSigma: 0.5, seed: 1}
}

func TestStraight60s(t *testing.T) {
	r := runScenario(t, straight(), 60, 5, 30)
	imm := collect(r.samples, nil, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, nil, func(e evalSample) float64 { return e.dr })
	t.Logf("straight 60s: imm median %.0f m p95 %.0f m, DR median %.0f m (n=%d)", median(imm), pct(imm, 0.95), median(dr), len(imm))
	if median(imm) > 60 || pct(imm, 0.95) > 150 {
		t.Fatalf("straight 60 s error too large: median %.0f p95 %.0f", median(imm), pct(imm, 0.95))
	}
	vErr := collect(r.samples, nil, func(e evalSample) float64 { return absf(e.immV) })
	if median(vErr) > 30 {
		t.Fatalf("level flight vertical error %v ft", median(vErr))
	}
	// The reported sigma includes the coast turn hypothesis (an aircraft may
	// turn at 25 deg bank during the next 60 s): it must cover the straight
	// truth and stay within that hypothesis (+ the filter's own growth).
	sig := collect(r.samples, nil, func(e evalSample) float64 { return e.sigH })
	v := 450 * ktToMS
	acc := G0 * math.Tan(25*deg2rad)
	rr, th := v*v/acc, acc/v*60
	hyp := rr * math.Hypot(th-math.Sin(th), 1-math.Cos(th))
	if median(sig) < 2*median(imm) || median(sig) > 1.2*hyp+1500 {
		t.Fatalf("sigma %v vs error %v (turn hypothesis %.0f m)", median(sig), median(imm), hyp)
	}
}

func turnScenario(bankDeg float64, turnSec, straightSec float64, seed int64) scenario {
	sc := straight()
	sc.seed = seed
	sc.tasKt = 250
	sc.alt = 12000
	sc.dur = 900
	tas := sc.tasKt * ktToMS
	rate := G0 * math.Tan(bankDeg*deg2rad) / tas * rad2deg
	period := turnSec + straightSec
	sc.ctl = func(t float64, s *simAC) control {
		ph := math.Mod(t, period)
		if t > 60 && ph < turnSec {
			// roll in / out over 3 s
			f := math.Min(1, math.Min(ph, turnSec-ph)/3)
			return control{turnDps: rate * f}
		}
		return control{}
	}
	return sc
}

func TestTurnsBeatDeadReckoning(t *testing.T) {
	for _, bank := range []float64{10, 25, 30} {
		// 90-degree-ish turns separated by straight legs
		tas := 250 * ktToMS
		rate := G0 * math.Tan(bank*deg2rad) / tas * rad2deg
		turnSec := 90 / rate
		sc := turnScenario(bank, turnSec, 120, int64(bank))
		r := runScenario(t, sc, 60, 2, 90)
		sel := func(e evalSample) bool { return e.turning }
		imm := collect(r.samples, sel, func(e evalSample) float64 { return e.imm })
		dr := collect(r.samples, sel, func(e evalSample) float64 { return e.dr })
		cv := collect(r.samples, sel, func(e evalSample) float64 { return e.cv })
		all := collect(r.samples, nil, func(e evalSample) float64 { return e.imm })
		t.Logf("bank %2.0f (%.1f deg/s, 90 deg turns): turning 60s imm median %.0f m, cv %.0f m, DR %.0f m; all imm median %.0f (n=%d)",
			bank, rate, median(imm), median(cv), median(dr), median(all), len(imm))
		if !(median(imm) < 0.75*median(dr)) {
			t.Fatalf("bank %v: imm %v not clearly better than DR %v", bank, median(imm), median(dr))
		}
		if !(median(imm) < 0.8*median(cv)) {
			t.Fatalf("bank %v: imm %v not better than straight line %v", bank, median(imm), median(cv))
		}
	}
}

func TestContinuousTurnWithRoll(t *testing.T) {
	// steady 25 deg bank circle; BDS 5,0 roll reported at 1 Hz
	sc := turnScenario(25, 1e9, 0, 7)
	sc.extra = func(tt float64, s *simAC, o *Obs) {
		if s.turn != 0 {
			o.Has |= HasRoll | HasTAS
			o.RollDeg = math.Atan(s.turn*deg2rad*s.tas/G0) * rad2deg
			o.TASKt = s.tas * msToKt
		}
	}
	r := runScenario(t, sc, 30, 2, 120)
	imm := collect(r.samples, nil, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, nil, func(e evalSample) float64 { return e.dr })
	t.Logf("circling 30s: imm %.0f m, DR %.0f m", median(imm), median(dr))
	if !(median(imm) < 0.5*median(dr)) {
		t.Fatalf("circling: imm %v vs DR %v", median(imm), median(dr))
	}
	pr, ok := r.m.Predict(testICAO, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).Add(899*time.Second))
	if !ok || pr.Model != "ct" || math.Abs(pr.TurnDps-2.04) > 0.3 {
		t.Fatalf("turn model not identified: %+v", pr)
	}
}

func TestSelectedHeadingCapture(t *testing.T) {
	// 90 deg right turn at 25 deg bank from 75 to 165 with selected heading broadcast
	sc := straight()
	sc.tasKt, sc.alt, sc.dur = 250, 12000, 400
	tas := sc.tasKt * ktToMS
	rate := G0 * math.Tan(25*deg2rad) / tas * rad2deg
	startT := 200.0
	sc.ctl = func(t float64, s *simAC) control {
		if t > startT && angDiff(165, s.hdg) > 0.5 {
			return control{turnDps: rate}
		}
		return control{}
	}
	sc.extra = func(t float64, s *simAC, o *Obs) {
		if t > startT-5 {
			o.Has |= HasSelHeading
			o.SelHeadingDeg = 165
		}
	}
	r := runScenario(t, sc, 60, 1, 30)
	sel := func(e evalSample) bool { return e.turning && e.t > startT+8 }
	imm := collect(r.samples, sel, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, sel, func(e evalSample) float64 { return e.dr })
	t.Logf("capture: imm median %.0f m max %.0f, DR %.0f m (n=%d)", median(imm), pct(imm, 1), median(dr), len(imm))
	if len(imm) < 5 || median(imm) > 400 || median(imm) > 0.2*median(dr) {
		t.Fatalf("selected heading capture: imm %v DR %v", median(imm), median(dr))
	}
}

func TestClimbLevelOff(t *testing.T) {
	sc := straight()
	sc.alt, sc.tasKt, sc.dur = 10000, 300, 600
	sel := 18000.0
	sc.ctl = func(t float64, s *simAC) control {
		if t < 60 {
			return control{}
		}
		rem := sel - s.alt
		vr := math.Min(2500, rem*60/12) // exponential capture, tau 12 s
		if rem < 5 {
			vr = 0
		}
		return control{vrFpm: vr}
	}
	sc.extra = func(t float64, s *simAC, o *Obs) {
		o.Has |= HasSelAlt
		o.SelAltFt = sel
	}
	r := runScenario(t, sc, 60, 2, 30)
	// samples whose 60 s horizon spans the level-off
	selF := func(e evalSample) bool {
		return r.ac.at(e.t).alt < sel-500 && r.ac.at(e.t+60).alt > sel-200
	}
	immV := collect(r.samples, selF, func(e evalSample) float64 { return absf(e.immV) })
	drV := collect(r.samples, selF, func(e evalSample) float64 { return absf(e.drV) })
	t.Logf("level-off 60s vertical: imm median %.0f ft max %.0f, DR %.0f ft (n=%d)", median(immV), pct(immV, 1), median(drV), len(immV))
	if len(immV) < 5 || median(immV) > 250 || median(immV) > 0.3*median(drV) {
		t.Fatalf("level-off prediction: imm %v DR %v", median(immV), median(drV))
	}
	in, ok := r.m.Intent(testICAO, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).Add(150*time.Second))
	_ = in
	_ = ok
}

func TestLevelOffIntent(t *testing.T) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	lat, lon := 50.0, 8.0
	alt := 10000.0
	for i := 0; i <= 60; i++ {
		var o Obs
		o.T = base.Add(time.Duration(i) * time.Second)
		la, lo, _ := geo.VincentyDirect(lat, lon, 90, float64(i)*150)
		o.SetPos(la, lo, SrcADSB)
		o.SetVel(150*msToKt, 90)
		o.SetAlt(alt + float64(i)*2000.0/60)
		o.SetVRate(2000)
		o.Has |= HasSelAlt
		o.SelAltFt = 14000
		m.Observe(1, &o)
	}
	in, ok := m.Intent(1, base.Add(60*time.Second))
	if !ok || in.LevelOff == nil || in.LevelOff.Target != "selected" {
		t.Fatalf("intent %+v", in)
	}
	// 12000 -> 14000 at 2000 fpm: about 60 s + capture
	if in.LevelOff.InSec < 50 || in.LevelOff.InSec > 100 || in.Phase != "climb" {
		t.Fatalf("level-off %+v phase %s", in.LevelOff, in.Phase)
	}
}

func TestAccelerating(t *testing.T) {
	sc := straight()
	sc.tasKt, sc.alt, sc.dur = 200, 5000, 400
	sc.ctl = func(t float64, s *simAC) control {
		if t > 60 && t < 330 {
			return control{accel: 0.6}
		}
		return control{}
	}
	r := runScenario(t, sc, 60, 2, 30)
	sel := func(e evalSample) bool { return e.t > 100 && e.t+60 < 330 }
	imm := collect(r.samples, sel, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, sel, func(e evalSample) float64 { return e.dr })
	t.Logf("accelerating 0.6 m/s2: imm median %.0f m, DR %.0f m", median(imm), median(dr))
	// DR misses 0.5*a*t^2 = 1080 m; decaying acceleration recovers most of it
	if median(imm) > 500 || median(imm) > 0.5*median(dr) {
		t.Fatalf("acceleration: imm %v DR %v", median(imm), median(dr))
	}
}

func TestMLATNoisyWithGaps(t *testing.T) {
	sc := turnScenario(20, 30, 90, 11)
	sc.dur = 1200
	sc.posSigma = 100
	sc.posEvery = 2
	sc.src = SrcMLAT
	sc.noVel = true
	sc.gap = func(t float64) bool { return math.Mod(t, 120) > 100 } // 20 s gaps
	// the harness needs a DR velocity: give a sparse, noisy one (as from an SBS feed)
	sc.noVel = false
	sc.velEvery = 5
	sc.velSigma = 4
	r := runScenario(t, sc, 60, 3, 120)
	imm := collect(r.samples, nil, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, nil, func(e evalSample) float64 { return e.dr })
	turn := func(e evalSample) bool { return e.turning }
	immT := collect(r.samples, turn, func(e evalSample) float64 { return e.imm })
	drT := collect(r.samples, turn, func(e evalSample) float64 { return e.dr })
	t.Logf("MLAT 100 m, 20 s gaps: imm median %.0f m p90 %.0f, DR %.0f m; while turning imm %.0f DR %.0f (n=%d)",
		median(imm), pct(imm, 0.9), median(dr), median(immT), median(drT), len(immT))
	// Turn onsets / ends that the 60 s horizon cannot know dominate the
	// overall median, so overall we only require parity with dead reckoning
	// (which uses the same noisy data), but a clear win while turning.
	if median(imm) > 1500 || median(imm) > 1.05*median(dr) {
		t.Fatalf("MLAT: imm %v DR %v", median(imm), median(dr))
	}
	if !(median(immT) < 0.75*median(drT)) {
		t.Fatalf("MLAT turning: imm %v DR %v", median(immT), median(drT))
	}
	// the 1-sigma must not be absurdly over-confident with noisy MLAT
	in := 0
	for _, e := range r.samples {
		if e.imm < 2.45*e.sigH {
			in++
		}
	}
	if f := float64(in) / float64(len(r.samples)); f < 0.8 {
		t.Fatalf("MLAT: only %.2f of errors within 2.45 sigma", f)
	}
}

func TestSmoothingReducesNoise(t *testing.T) {
	sc := straight()
	sc.posSigma, sc.src, sc.posEvery, sc.dur = 100, SrcMLAT, 1, 300
	sc.velSigma, sc.velEvery = 3, 5
	m := runScenario(t, sc, 1, 1, 30)
	var est, raw []float64
	rng := rand.New(rand.NewSource(5))
	for _, e := range m.samples {
		// 1-s-ahead prediction ~ current estimate
		est = append(est, e.imm)
		raw = append(raw, math.Hypot(rng.NormFloat64(), rng.NormFloat64())*100)
	}
	t.Logf("MLAT 100 m: filtered median %.0f m vs raw %.0f m", median(est), median(raw))
	if median(est) > 0.6*median(raw) {
		t.Fatalf("filter does not smooth: %v vs %v", median(est), median(raw))
	}
}

func TestWindTurn(t *testing.T) {
	// circling in 60 kt wind: air vector rotates, ground track does not turn uniformly
	sc := turnScenario(25, 1e9, 0, 3)
	sc.windFrom, sc.windKt = 270, 60
	sc.dur = 600
	withWind := sc
	withWind.extra = func(tt float64, s *simAC, o *Obs) {
		o.Has |= HasWind | HasRoll | HasTAS
		o.WindDirDeg, o.WindKt = 270, 60
		o.RollDeg = math.Atan(s.turn*deg2rad*s.tas/G0) * rad2deg
		o.TASKt = s.tas * msToKt
	}
	noWind := sc
	noWind.extra = func(tt float64, s *simAC, o *Obs) {
		o.Has |= HasRoll | HasTAS
		o.RollDeg = math.Atan(s.turn*deg2rad*s.tas/G0) * rad2deg
		o.TASKt = s.tas * msToKt
	}
	rw := runScenario(t, withWind, 20, 2, 120)
	rn := runScenario(t, noWind, 20, 2, 120)
	a := median(collect(rw.samples, nil, func(e evalSample) float64 { return e.imm }))
	b := median(collect(rn.samples, nil, func(e evalSample) float64 { return e.imm }))
	t.Logf("circling in 60 kt wind, 20 s: with wind %.0f m, without %.0f m", a, b)
	if !(a < b) {
		t.Fatalf("wind does not help: %v vs %v", a, b)
	}
}

func TestGroundDoesNotFlyOff(t *testing.T) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i <= 30; i++ {
		var o Obs
		o.T = base.Add(time.Duration(i) * time.Second)
		la, lo, _ := geo.VincentyDirect(50, 8, 0, float64(i)*8)
		o.SetPos(la, lo, SrcADSB)
		o.SetVel(8*msToKt, 0)
		o.Has |= HasGround
		o.OnGround = true
		m.Observe(2, &o)
	}
	pr, ok := m.Predict(2, base.Add(90*time.Second))
	if !ok {
		t.Fatal("no prediction")
	}
	last, _, _ := geo.VincentyDirect(50, 8, 0, 240)
	d := geo.VincentyKm(last, 8, pr.Lat, pr.Lon) * 1000
	if d > 8*60*0.5 || !pr.OnGround || pr.VRateFpm != 0 {
		t.Fatalf("ground aircraft moved %v m in 60 s at 8 m/s, %+v", d, pr)
	}
	if pr.GSKt > 2 {
		t.Fatalf("taxi speed did not decay: %v kt", pr.GSKt)
	}
}

func TestMaxCoast(t *testing.T) {
	r := runScenario(t, straight(), 60, 100, 1000)
	end := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).Add(600 * time.Second)
	if _, ok := r.m.Predict(testICAO, end.Add(59*time.Second)); !ok {
		t.Fatal("should coast 59 s")
	}
	if _, ok := r.m.Predict(testICAO, end.Add(61*time.Second)); ok {
		t.Fatal("should stop after 60 s")
	}
	p := r.m.Predicted(end.Add(30*time.Second), nil)
	if len(p) != 1 || !p[0].Coasting || p[0].Confidence <= 0 || p[0].Confidence >= 1 {
		t.Fatalf("predicted %+v", p)
	}
	p0 := r.m.Predicted(end, nil)
	if p0[0].Coasting || p0[0].Confidence < p[0].Confidence {
		t.Fatalf("fresh %+v vs coasting %+v", p0[0], p[0])
	}
	if p := r.m.Predicted(end.Add(70*time.Second), nil); len(p) != 0 {
		t.Fatalf("beyond max coast: %+v", p)
	}
}

func TestOutOfOrderDuplicatesOutliers(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mk := func(i float64, jumpKm float64) Obs {
		var o Obs
		o.T = base.Add(time.Duration(i * float64(time.Second)))
		la, lo, _ := geo.VincentyDirect(50, 8, 45, i*200)
		if jumpKm != 0 {
			la, lo, _ = geo.VincentyDirect(la, lo, 135, jumpKm*1000)
		}
		o.SetPos(la, lo, SrcADSB)
		o.SetVel(200*msToKt, 45)
		o.SetAlt(30000)
		return o
	}
	ref := NewManager(Config{})
	m := NewManager(Config{})
	rng := rand.New(rand.NewSource(3))
	var obs []Obs
	for i := 0.0; i <= 120; i += 0.5 {
		obs = append(obs, mk(i, 0))
	}
	for i := range obs {
		ref.Observe(9, &obs[i])
	}
	// shuffle locally (late by <= 1 s), duplicate 20 %, inject 3 isolated 10 km outliers
	sh := append([]Obs(nil), obs...)
	for i := 0; i+2 < len(sh); i += 3 {
		if rng.Intn(2) == 0 {
			sh[i], sh[i+2] = sh[i+2], sh[i]
		}
	}
	for i := range sh {
		m.Observe(9, &sh[i])
		if rng.Intn(5) == 0 {
			m.Observe(9, &sh[i])
		}
		if i == 50 || i == 120 || i == 180 {
			o := mk(sh[i].T.Sub(base).Seconds()+0.1, 10)
			m.Observe(9, &o)
		}
	}
	at := base.Add(180 * time.Second)
	a, _ := ref.Predict(9, at)
	b, ok := m.Predict(9, at)
	d := geo.VincentyKm(a.Lat, a.Lon, b.Lat, b.Lon) * 1000
	st := m.Stats(at)
	t.Logf("shuffled/duplicated/outliers vs clean: %.1f m apart; counters %+v", d, st.Counters)
	if !ok || d > 50 {
		t.Fatalf("robustness: %v m apart", d)
	}
	if st.Counters.Duplicates == 0 || st.Counters.OutOfOrder == 0 || st.Counters.Rejected < 3 {
		t.Fatalf("counters %+v", st.Counters)
	}
	// a real jump (persistent) re-initialises the track
	for i := 121.0; i <= 130; i += 0.5 {
		o := mk(i, 30)
		m.Observe(9, &o)
	}
	p, _ := m.Predict(9, base.Add(130*time.Second))
	la, lo, _ := geo.VincentyDirect(50, 8, 45, 130*200)
	la, lo, _ = geo.VincentyDirect(la, lo, 135, 30000)
	if dd := geo.VincentyKm(la, lo, p.Lat, p.Lon); dd > 0.5 {
		t.Fatalf("did not follow persistent jump: %v km", dd)
	}
	if m.Stats(at).Counters.Resets == 0 {
		t.Fatal("no reset counted")
	}
}

func TestLongRunStability(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	sc := straight()
	sc.dur = 3 * 3600
	sc.posEvery, sc.velEvery = 1, 1
	sc.tasKt = 420
	sc.ctl = func(t float64, s *simAC) control {
		ph := math.Mod(t, 600)
		c := control{}
		if ph > 100 && ph < 130 {
			c.turnDps = 2
		}
		if ph > 300 && ph < 345 {
			c.turnDps = -1.5
		}
		if ph > 200 && ph < 260 {
			c.vrFpm = 1000
		} else if ph > 400 && ph < 460 {
			c.vrFpm = -1000
		}
		return c
	}
	r := runScenario(t, sc, 60, 30, 60)
	imm := collect(r.samples, nil, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, nil, func(e evalSample) float64 { return e.dr })
	early := collect(r.samples, func(e evalSample) bool { return e.t < 3600 }, func(e evalSample) float64 { return e.imm })
	late := collect(r.samples, func(e evalSample) bool { return e.t > 2*3600 }, func(e evalSample) float64 { return e.imm })
	t.Logf("3 h (%.0f km flown): 60 s imm median %.0f m p95 %.0f, DR median %.0f p95 %.0f; first hour median %.0f p95 %.0f, last hour median %.0f p95 %.0f",
		3*3600*420*ktToMS/1000, median(imm), pct(imm, 0.95), median(dr), pct(dr, 0.95), median(early), pct(early, 0.95), median(late), pct(late, 0.95))
	// The p95 is set by unannounced turn onsets (DR has the same); what must
	// not happen is drift: the last hour must be as good as the first.
	if median(late) > 1.5*median(early)+20 || pct(late, 0.95) > 1.2*pct(early, 0.95)+200 || pct(imm, 0.95) > 1.05*pct(dr, 0.95) {
		t.Fatal("degradation over time")
	}
	m := r.m
	sh := m.shard(testICAO)
	tr := sh.m[testICAO]
	for i := 0; i < nx; i++ {
		for j := 0; j < nx; j++ {
			if !finite(tr.f.P[i][j]) || math.Abs(tr.f.P[i][j]-tr.f.P[j][i]) > 1e-9*math.Max(1, math.Abs(tr.f.P[i][j])) {
				t.Fatalf("P not finite/symmetric at %d,%d", i, j)
			}
		}
		if tr.f.P[i][i] <= 0 {
			t.Fatalf("P not positive")
		}
	}
	if math.Hypot(tr.f.x[ix], tr.f.x[iy]) > 25000 {
		t.Fatal("frame not re-centred")
	}
	st := m.Stats(time.Now())
	checkJSON(t, st)
	var found bool
	for _, h := range st.Horizons {
		if h.HorizonSec == 60 && h.Predictor == "imm" && h.Phase == "all" {
			found = true
			t.Logf("self-verification 60 s imm: median %.3f km p90 %.3f calib95 %.2f n=%d", h.MedianKm, h.P90Km, h.Calib95, h.N)
			if h.N < 100 || h.MedianKm > 1 || h.Calib95 < 0.6 {
				t.Fatalf("self-verification stats %+v", h)
			}
		}
	}
	if !found {
		t.Fatal("no 60 s stats")
	}
	for _, s := range st.Skill {
		t.Logf("skill %2ds %-8s imm %.3f km dr %.3f km skill %.0f%% (n=%d)", s.HorizonSec, s.Phase, s.IMMMedKm, s.DRMedKm, s.SkillPct, s.N)
		if s.Phase == "turn" && s.HorizonSec >= 30 && s.SkillPct < 25 {
			t.Fatalf("no skill in turns: %+v", s)
		}
	}
}

func checkJSON(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if s := string(b); strings.Contains(s, "NaN") || strings.Contains(s, "Inf") {
		t.Fatalf("non-finite in JSON: %s", s)
	}
}

func TestSelfVerificationStats(t *testing.T) {
	sc := turnScenario(25, 30, 60, 21)
	sc.dur = 1500
	r := runScenario(t, sc, 60, 1000, 1e9)
	st := r.m.Stats(time.Now())
	checkJSON(t, st)
	get := func(pred, phase string, h int) *HorizonStats {
		for i := range st.Horizons {
			x := &st.Horizons[i]
			if x.Predictor == pred && x.Phase == phase && x.HorizonSec == h {
				return x
			}
		}
		return nil
	}
	for _, h := range []int{5, 15, 30, 60} {
		a, d := get("imm", "all", h), get("dr", "all", h)
		if a == nil || d == nil {
			t.Fatalf("missing stats for %d s", h)
		}
		t.Logf("%2d s: imm median %.3f km (along %.3f cross %.3f, vert %.0f ft, calib95 %.2f) | cv %.3f | dr %.3f  n=%d",
			h, a.MedianKm, a.AlongMedAbsKm, a.CrossMedAbsKm, a.VertMedAbsFt, a.Calib95, get("cv", "all", h).MedianKm, d.MedianKm, a.N)
		if a.N == 0 || a.N != d.N {
			t.Fatalf("sample sets differ %d vs %d", a.N, d.N)
		}
	}
	tt, td := get("imm", "turn", 60), get("dr", "turn", 60)
	if tt == nil || td == nil || !(tt.MedianKm < 0.75*td.MedianKm) {
		t.Fatalf("turn skill missing: %+v %+v", tt, td)
	}
	if c := get("imm", "all", 60).Calib95; c < 0.6 || c > 1 {
		t.Fatalf("calibration %v", c)
	}
}

// ---- holding, approach, CPA, exit ----

func holdScenario() scenario {
	sc := straight()
	sc.tasKt, sc.alt, sc.dur = 230, 9000, 1200
	tas := sc.tasKt * ktToMS
	rate := 3.0 // standard rate
	_ = tas
	turn := 180 / rate
	leg := 60.0
	lap := 2 * (turn + leg)
	sc.ctl = func(t float64, s *simAC) control {
		if t < 120 {
			return control{}
		}
		ph := math.Mod(t-120, lap)
		if ph < turn || (ph >= turn+leg && ph < 2*turn+leg) {
			return control{turnDps: rate}
		}
		return control{}
	}
	return sc
}

func TestHoldingDetectionAndPrediction(t *testing.T) {
	sc := holdScenario()
	r := runScenario(t, sc, 60, 2, 700)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	in, ok := r.m.Intent(testICAO, base.Add(1100*time.Second))
	if !ok || in.Holding == nil || in.Phase != "holding" {
		t.Fatalf("holding not detected: %+v", in)
	}
	h := in.Holding
	t.Logf("hold: %+v", *h)
	if h.Kind != "hold" || h.Direction != "right" || math.Abs(h.LegSec-60) > 15 || math.Abs(h.TurnDps-3) > 0.6 || h.RadiusKm > 15 {
		t.Fatalf("hold info %+v", *h)
	}
	if d := math.Abs(angDiff(h.LegCourse, 75)); d > 10 && math.Abs(d-180) > 10 {
		t.Fatalf("leg course %v", h.LegCourse)
	}
	imm := collect(r.samples, nil, func(e evalSample) float64 { return e.imm })
	dr := collect(r.samples, nil, func(e evalSample) float64 { return e.dr })
	t.Logf("holding 60 s: imm median %.0f m p90 %.0f, DR %.0f m", median(imm), pct(imm, 0.9), median(dr))
	if !(median(imm) < 0.3*median(dr)) {
		t.Fatalf("holding prediction %v vs DR %v", median(imm), median(dr))
	}
	path, ok := r.m.PathFor(testICAO, base.Add(1100*time.Second), 300*time.Second, 10*time.Second)
	if !ok || path.Plan != "hold" {
		t.Fatalf("path plan %v", path.Plan)
	}
	// the predicted racetrack stays near the hold
	for _, p := range path.Points {
		if d := haversineM(p.Lat, p.Lon, h.CenterLat, h.CenterLon); d > 15000 {
			t.Fatalf("hold path leaves the hold: %v m at %v s", d, p.DT)
		}
	}
}

type rwList []Runway

func (l rwList) RunwaysNear(lat, lon, r float64, dst []Runway) []Runway {
	for _, x := range l {
		if haversineM(lat, lon, x.Lat, x.Lon) < r*1000 {
			dst = append(dst, x)
		}
	}
	return dst
}

func TestApproachDetection(t *testing.T) {
	m := NewManager(Config{})
	// runway 27 at (50, 8), elevation 300 ft, landing heading 270
	m.SetRunways(rwList{{Airport: "TEST", Ident: "27", Lat: 50, Lon: 8, HeadingDeg: 270, ElevationFt: 300},
		{Airport: "TEST", Ident: "09", Lat: 50, Lon: 7.95, HeadingDeg: 90, ElevationFt: 300}})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// 15 km east on the centre line, 3 deg glide path, 140 kt decelerating
	start := 20000.0
	gs := 75.0
	for i := 0; i <= 40; i++ {
		d := start - float64(i)*gs
		la, lo, _ := geo.VincentyDirect(50, 8, 90, d)
		var o Obs
		o.T = base.Add(time.Duration(i) * time.Second)
		o.SetPos(la, lo, SrcADSB)
		o.SetVel((gs-float64(i)*0.3)*msToKt, 270)
		o.SetAlt(300 + d*math.Tan(3*deg2rad)*ftPerM)
		o.SetVRate(-gs * math.Tan(3*deg2rad) * ftPerM * 60)
		m.Observe(5, &o)
	}
	in, ok := m.Intent(5, base.Add(40*time.Second))
	if !ok || in.Approach == nil || in.Approach.Runway != "27" || in.Phase != "approach" {
		t.Fatalf("approach not detected: %+v", in)
	}
	a := in.Approach
	t.Logf("approach %+v", *a)
	if math.Abs(a.DistKm-17) > 0.3 || math.Abs(a.CrossTrackM) > 100 || math.Abs(a.GlideDevFt) > 150 || a.ETASec < 200 || a.ETASec > 280 {
		t.Fatalf("approach geometry %+v", *a)
	}
	if in.LevelOff == nil || in.LevelOff.Target != "ground" || math.Abs(in.LevelOff.AltFt-300) > 50 {
		t.Fatalf("level-off at the runway: %+v", in.LevelOff)
	}
}

func TestCPAAndExit(t *testing.T) {
	m := NewManager(Config{})
	rxLat, rxLon := 50.0, 8.0
	m.SetReceiver(rxLat, rxLon)
	m.SetRangeFunc(func(brg, alt float64) float64 { return 100 })
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// track heading north, passing 10 km east of the receiver; starts 50 km south
	east, _, _ := geo.VincentyDirect(rxLat, rxLon, 90, 10000)
	_ = east
	_, eLon, _ := geo.VincentyDirect(rxLat, rxLon, 90, 10000)
	sLat, sLon, _ := geo.VincentyDirect(rxLat, eLon, 180, 50000)
	v := 200.0
	for i := 0; i <= 20; i++ {
		la, lo, _ := geo.VincentyDirect(sLat, sLon, 0, float64(i)*v)
		var o Obs
		o.T = base.Add(time.Duration(i) * time.Second)
		o.SetPos(la, lo, SrcADSB)
		o.SetVel(v*msToKt, 0)
		o.SetAlt(20000)
		m.Observe(7, &o)
	}
	in, ok := m.Intent(7, base.Add(20*time.Second))
	if !ok || in.CPA == nil {
		t.Fatalf("no CPA %+v", in)
	}
	// remaining 46 km at 200 m/s = 230 s; CPA distance 10 km
	t.Logf("CPA %+v exit %v", *in.CPA, *in.ExitSec)
	if math.Abs(in.CPA.InSec-230) > 3 || math.Abs(in.CPA.DistKm-10) > 0.1 {
		t.Fatalf("CPA %+v", *in.CPA)
	}
	// exit at 100 km: north distance sqrt(100^2-10^2)=99.5 km from abeam -> 230 + 497.5 s
	if in.ExitSec == nil || math.Abs(*in.ExitSec-727) > 10 {
		t.Fatalf("exit %v", in.ExitSec)
	}
	checkJSON(t, in)
}

func TestProximityGrid(t *testing.T) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(9))
	feed := func(icao uint32, lat, lon, trk, gs, alt float64) {
		for i := 0; i <= 20; i++ {
			la, lo, _ := geo.VincentyDirect(lat, lon, trk, float64(i)*gs)
			var o Obs
			o.T = base.Add(time.Duration(i) * time.Second)
			o.SetPos(la, lo, SrcADSB)
			o.SetVel(gs*msToKt, trk)
			o.SetAlt(alt)
			m.Observe(icao, &o)
		}
	}
	// head-on pair: 30 km apart after 20 s of flight, closing at 400 m/s -> CPA in 75 s at 0 km
	aLat, aLon, _ := geo.VincentyDirect(52, 5, 270, 15000+20*200)
	bLat, bLon, _ := geo.VincentyDirect(52, 5, 90, 15000+20*200)
	feed(100, aLat, aLon, 90, 200, 30000)
	feed(101, bLat, bLon, 270, 200, 30500)
	// same geometry, 2000 ft apart: not a conflict
	cLat, cLon, _ := geo.VincentyDirect(53, 5, 270, 19000)
	dLat, dLon, _ := geo.VincentyDirect(53, 5, 90, 19000)
	feed(102, cLat, cLon, 90, 200, 30000)
	feed(103, dLat, dLon, 270, 200, 32000)
	// background traffic
	for i := 0; i < 300; i++ {
		feed(uint32(1000+i), 45+rng.Float64()*10, -5+rng.Float64()*20, rng.Float64()*360, 120+rng.Float64()*130, 2000+rng.Float64()*38000)
	}
	now := base.Add(20 * time.Second)
	opt := ProximityOptions{HorizonSec: 120}
	pairs := m.Proximity(now, opt)
	checkJSON(t, pairs)
	var hit *ProximityPair
	for i := range pairs {
		p := &pairs[i]
		if (p.A == "000064" && p.B == "000065") || (p.A == "000065" && p.B == "000064") {
			hit = p
		}
		if strings.Contains(p.A+p.B, "000066") || strings.Contains(p.A+p.B, "000067") {
			t.Fatalf("2000 ft pair reported: %+v", p)
		}
	}
	if hit == nil {
		t.Fatalf("head-on pair not found in %d pairs", len(pairs))
	}
	t.Logf("head-on: %+v (total pairs %d)", *hit, len(pairs))
	if math.Abs(hit.TCPASec-75) > 3 || hit.HDistKm > 0.3 || math.Abs(hit.VSepFt-500) > 30 || math.Abs(hit.NowHDistKm-30) > 0.3 {
		t.Fatalf("head-on CPA %+v", *hit)
	}
	// grid search equals brute force
	brute := 0
	var paths [][]proxPt
	var ids []string
	m.proxMu.Lock()
	sc := &m.prox
	K := int(opt.withDefaults().HorizonSec/opt.withDefaults().StepSec) + 1
	for _, a := range sc.acs {
		paths = append(paths, append([]proxPt(nil), sc.pts[a.k0:a.k0+K]...))
		ids = append(ids, a.hex)
	}
	m.proxMu.Unlock()
	o2 := opt.withDefaults()
	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			if haversineM(paths[i][0].lat, paths[i][0].lon, paths[j][0].lat, paths[j][0].lon) > o2.SearchKm*1000 {
				continue
			}
			if _, ok := pairCPA(paths[i], paths[j], o2); ok {
				brute++
			}
		}
	}
	if brute != len(pairs) {
		t.Fatalf("grid found %d pairs, brute force %d", len(pairs), brute)
	}
}

func TestPathForGrowsUncertainty(t *testing.T) {
	r := runScenario(t, straight(), 60, 1000, 1e9)
	end := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).Add(600 * time.Second)
	p, ok := r.m.PathFor(testICAO, end, 300*time.Second, 10*time.Second)
	if !ok || len(p.Points) != 31 {
		t.Fatalf("path %v %d", ok, len(p.Points))
	}
	checkJSON(t, p)
	for i := 1; i < len(p.Points); i++ {
		if p.Points[i].SigmaHM < p.Points[i-1].SigmaHM-1e-6 {
			t.Fatalf("sigma shrinks at %d", i)
		}
	}
	// 300 s along a straight great circle at 450 kt TAS
	last := r.ac.hist[len(r.ac.hist)-1]
	la, lo, _ := geo.VincentyDirect(last.lat, last.lon, last.trk, last.gs*300)
	end300 := p.Points[len(p.Points)-1]
	if d := geo.VincentyKm(la, lo, end300.Lat, end300.Lon) * 1000; d > 300 {
		t.Fatalf("300 s straight path error %v m", d)
	}
	t.Logf("300 s: sigma %.0f m, ellipse %+v", end300.SigmaHM, end300.Ellipse)
}

func TestNaNFreeOutputs(t *testing.T) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// pathological inputs: NaN, Inf, zero speed, poles, antimeridian
	bad := []Obs{}
	add := func(f func(o *Obs)) {
		var o Obs
		f(&o)
		bad = append(bad, o)
	}
	add(func(o *Obs) { o.SetPos(math.NaN(), 1, SrcADSB) })
	add(func(o *Obs) { o.SetVel(math.Inf(1), 10) })
	add(func(o *Obs) { o.SetPos(89.99, 179.999, SrcADSB); o.SetVel(0, 0); o.SetAlt(math.NaN()) })
	add(func(o *Obs) { o.SetPos(89.99, -179.999, SrcMLAT); o.SetVRate(math.Inf(-1)) })
	add(func(o *Obs) { o.SetPos(0, 179.9999, SrcADSB); o.SetVel(500, 90); o.Has |= HasRoll; o.RollDeg = 89 })
	add(func(o *Obs) { o.SetPos(0.001, -179.9999, SrcADSB); o.SetVel(500, 90); o.SetAlt(1e9) })
	for i := 0; i < 200; i++ {
		for j := range bad {
			o := bad[j]
			o.T = base.Add(time.Duration(i) * 500 * time.Millisecond)
			m.Observe(uint32(j+1), &o)
		}
	}
	now := base.Add(110 * time.Second)
	checkJSON(t, m.Predicted(now, nil))
	for j := range bad {
		if p, ok := m.Predict(uint32(j+1), now); ok {
			checkJSON(t, p)
		}
		if p, ok := m.PathFor(uint32(j+1), now, 300*time.Second, 5*time.Second); ok {
			checkJSON(t, p)
		}
		if in, ok := m.Intent(uint32(j+1), now); ok {
			checkJSON(t, in)
		}
	}
	checkJSON(t, m.Stats(now))
	checkJSON(t, m.Proximity(now, ProximityOptions{}))
	// across the antimeridian the prediction keeps moving east
	p, ok := m.Predict(5, now)
	if !ok || p.Lon > -170 && p.Lon < 170 {
		t.Fatalf("antimeridian prediction %+v", p)
	}
}

func TestGCDropMaxTracks(t *testing.T) {
	m := NewManager(Config{MaxTracks: 3, StaleAfter: 100 * time.Second})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := uint32(0); i < 5; i++ {
		var o Obs
		o.T = base.Add(time.Duration(i) * time.Second)
		o.SetPos(50, 8, SrcADSB)
		m.Observe(i, &o)
	}
	if m.Len() != 3 || m.Stats(base).Counters.Overflow != 2 {
		t.Fatalf("len %d", m.Len())
	}
	m.Drop(0)
	if m.Len() != 2 {
		t.Fatal("drop")
	}
	if n := m.GC(base.Add(101500 * time.Millisecond)); n != 1 || m.Len() != 1 {
		t.Fatalf("gc removed %d, left %d", n, m.Len())
	}
}

func TestHexICAO(t *testing.T) {
	if hexICAO(0x3C6586) != "3C6586" || hexICAO(0x1000ABC|1<<24) != "~000ABC" || hexICAO(1) != "000001" {
		t.Fatal(hexICAO(0x3C6586), hexICAO(1|1<<24))
	}
}

func TestObserveZeroAlloc(t *testing.T) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	i := 0
	obs := func() Obs {
		var o Obs
		o.T = base.Add(time.Duration(i) * 250 * time.Millisecond)
		la, lo, _ := geo.VincentyDirect(50, 8, 30, float64(i)*50)
		if i%2 == 0 {
			o.SetPos(la, lo, SrcADSB)
			o.SetAlt(30000)
		} else {
			o.SetVel(200*msToKt, 30)
			o.SetVRate(0)
		}
		i++
		return o
	}
	for k := 0; k < 400; k++ {
		o := obs()
		m.Observe(42, &o)
	}
	pre := make([]Obs, 1000)
	for k := range pre {
		pre[k] = obs()
	}
	k := 0
	allocs := testing.AllocsPerRun(500, func() {
		m.Observe(42, &pre[k])
		k++
	})
	if allocs != 0 {
		t.Fatalf("Observe allocates %v per call", allocs)
	}
	dst := make([]Predicted, 0, 4)
	allocs = testing.AllocsPerRun(100, func() {
		dst = m.Predicted(base.Add(time.Duration(k)*250*time.Millisecond), dst[:0])
	})
	if allocs != 0 {
		t.Fatalf("Predicted allocates %v", allocs)
	}
}

func TestConcurrentUse(t *testing.T) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 2000; i++ {
				var o Obs
				o.T = base.Add(time.Duration(i) * 100 * time.Millisecond)
				la, lo, _ := geo.VincentyDirect(50, 8, float64(g*90), float64(i)*20)
				o.SetPos(la, lo, SrcADSB)
				o.SetVel(400, float64(g*90))
				o.SetAlt(20000)
				m.Observe(uint32(g*100+i%10), &o)
			}
		}(g)
	}
	go func() {
		defer func() { done <- struct{}{} }()
		for i := 0; i < 200; i++ {
			now := base.Add(time.Duration(i) * time.Second)
			m.Predicted(now, nil)
			m.Stats(now)
			m.Proximity(now, ProximityOptions{})
			m.PathFor(5, now, time.Minute, 5*time.Second)
			m.GC(now)
		}
	}()
	for i := 0; i < 5; i++ {
		<-done
	}
}

func TestSmoothTrail(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	var pts []model.TrailPoint
	var truth [][2]float64
	for i := 0; i < 200; i++ {
		la, lo, _ := geo.VincentyDirect(50, 8, 60, float64(i)*2*150)
		truth = append(truth, [2]float64{la, lo})
		pts = append(pts, model.TrailPoint{T: int64(i) * 2000, Lat: la + rng.NormFloat64()*100/111200,
			Lon: lo + rng.NormFloat64()*100/(111200*math.Cos(50*deg2rad)), Alt: 10000, Src: 1})
	}
	// shuffled input and a duplicate are fine
	pts[10], pts[11] = pts[11], pts[10]
	pts = append(pts, pts[50])
	out := SmoothTrail(pts, SmoothOptions{})
	if len(out) != 200 {
		t.Fatalf("len %d", len(out))
	}
	var raw, sm []float64
	for i := range out {
		if out[i].T != int64(i)*2000 {
			t.Fatal("not sorted")
		}
		sm = append(sm, haversineM(out[i].Lat, out[i].Lon, truth[i][0], truth[i][1]))
	}
	for _, p := range pts[:200] {
		i := p.T / 2000
		raw = append(raw, haversineM(p.Lat, p.Lon, truth[i][0], truth[i][1]))
	}
	t.Logf("smooth trail: raw median %.0f m, smoothed %.0f m", median(raw), median(sm))
	if median(sm) > 0.4*median(raw) {
		t.Fatalf("smoothing weak: %v vs %v", median(sm), median(raw))
	}
}

func TestNavModesAndSource(t *testing.T) {
	m := NavModesFromStrings([]string{"autopilot", "approach", "x"})
	if m != NavAutopilot|NavApproach {
		t.Fatal(m)
	}
	if SourceFromString("adsr") != SrcTISB || SrcMLAT.String() != "mlat" {
		t.Fatal("source")
	}
	c := Config{}.withDefaults()
	if c.MaxCoast != 60*time.Second || c.Now == nil {
		t.Fatal("defaults")
	}
}

// ---- benchmarks ----

func benchManager(b *testing.B, n int) (*Manager, []Obs, time.Time) {
	m := NewManager(Config{})
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(1))
	type ac struct{ lat, lon, trk float64 }
	acs := make([]ac, n)
	for i := range acs {
		acs[i] = ac{45 + rng.Float64()*10, rng.Float64() * 15, rng.Float64() * 360}
	}
	var obs []Obs
	for s := 0; s < 120; s++ {
		for i := range acs {
			var o Obs
			o.T = base.Add(time.Duration(s)*time.Second + time.Duration(i)*time.Millisecond)
			// gentle 0.5 deg/s turn at 430 kt
			trk := acs[i].trk + float64(s)*0.5
			if s > 0 {
				acs[i].lat, acs[i].lon, _ = geo.VincentyDirect(acs[i].lat, acs[i].lon, trk-0.25, 430*ktToMS)
			}
			o.SetPos(acs[i].lat, acs[i].lon, SrcADSB)
			o.SetVel(430, trk)
			o.SetAlt(35000)
			obs = append(obs, o)
		}
	}
	return m, obs, base
}

func BenchmarkObserve(b *testing.B) {
	_, obs, _ := benchManager(b, 1000)
	n := len(obs) / 2
	var m *Manager
	warm := func() {
		m = NewManager(Config{})
		for i := 0; i < n; i++ {
			m.Observe(uint32(i%1000), &obs[i])
		}
	}
	warm()
	b.ReportAllocs()
	b.ResetTimer()
	k := n
	for i := 0; i < b.N; i++ {
		if k == len(obs) { // every observation must be fresh (in order)
			b.StopTimer()
			warm()
			k = n
			b.StartTimer()
		}
		m.Observe(uint32(k%1000), &obs[k])
		k++
	}
}

func BenchmarkPredict60s(b *testing.B) {
	m, obs, base := benchManager(b, 10)
	for i := range obs {
		m.Observe(uint32(i%10), &obs[i])
	}
	at := base.Add(179 * time.Second) // 60 s after the last position
	if _, ok := m.Predict(0, at.Add(-500*time.Millisecond)); !ok {
		b.Fatal("no prediction")
	}
	at = at.Add(-500 * time.Millisecond)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := m.Predict(uint32(i%10), at); !ok {
			b.Fatal("no prediction")
		}
	}
}

func BenchmarkPredicted1000(b *testing.B) {
	m, obs, base := benchManager(b, 1000)
	for i := range obs {
		m.Observe(uint32(i%1000), &obs[i])
	}
	dst := make([]Predicted, 0, 1000)
	at := base.Add(125 * time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = m.Predicted(at, dst[:0])
	}
}

func BenchmarkPathFor300s(b *testing.B) {
	m, obs, base := benchManager(b, 10)
	for i := range obs {
		m.Observe(uint32(i%10), &obs[i])
	}
	at := base.Add(120 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.PathFor(uint32(i%10), at, 300*time.Second, 10*time.Second)
	}
}

func BenchmarkProximity1000(b *testing.B) {
	m, obs, base := benchManager(b, 1000)
	for i := range obs {
		m.Observe(uint32(i%1000), &obs[i])
	}
	at := base.Add(120 * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Proximity(at, ProximityOptions{})
	}
}
