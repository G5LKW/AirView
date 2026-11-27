package trajectory

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"airview/internal/geo"
)

// coastRun flies one aircraft straight with 2 Hz ADS-B position/velocity for
// warm seconds, then stops all data and (turnDps != 0) starts a constant
// turn at that very moment. It returns, per coast horizon, whether the truth
// lies inside the predicted 1-sigma circle, plus the predicted sigmas.
func coastRun(seed int64, tasKt, turnDps float64, horizons []float64) (inside []bool, sig []float64, sig2 float64) {
	rng := rand.New(rand.NewSource(seed))
	m := NewManager(Config{MaxCoast: 120 * time.Second})
	s := &simAC{lat: 50 + rng.Float64(), lon: 5 + rng.Float64(), alt: 6000, hdg: rng.Float64() * 360, tas: tasKt * ktToMS}
	s.ground3()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tm := func(x float64) time.Time { return base.Add(time.Duration(x * float64(time.Second))) }
	const dt, warm = 0.05, 90.0
	next := 0.0
	for s.t < warm-1e-9 {
		s.step(dt, control{})
		if s.t >= next {
			next += 0.5
			var o Obs
			o.T = tm(s.t)
			dn, de := rng.NormFloat64()*8, rng.NormFloat64()*8
			o.SetPos(s.lat+dn/111200, s.lon+de/(111200*math.Cos(s.lat*deg2rad)), SrcADSB)
			o.Has |= HasNACp
			o.NACp = 9
			o.SetAlt(math.Round(s.alt/25) * 25)
			m.Observe(testICAO, &o)
			var v Obs
			v.T = tm(s.t)
			sn, cs := math.Sincos(s.trk * deg2rad)
			vx, vy := s.gs*sn+rng.NormFloat64()*0.5, s.gs*cs+rng.NormFloat64()*0.5
			v.SetVel(math.Round(math.Hypot(vx, vy)*msToKt*10)/10, trackOf(vx, vy))
			v.SetVRate(0)
			v.Has |= HasNACv
			v.NACv = 2
			m.Observe(testICAO, &v)
		}
	}
	t0 := s.t
	if p, ok := m.Predict(testICAO, tm(t0+2)); ok {
		sig2 = p.SigmaHM
	}
	if rng.Intn(2) == 0 {
		turnDps = -turnDps
	}
	for _, h := range horizons {
		for s.t < t0+h-1e-9 {
			s.step(dt, control{turnDps: turnDps})
		}
		p, ok := m.Predict(testICAO, tm(s.t))
		if !ok {
			inside = append(inside, false)
			sig = append(sig, math.NaN())
			continue
		}
		d := geo.VincentyKm(s.lat, s.lon, p.Lat, p.Lon) * 1000
		inside = append(inside, d <= p.SigmaHM)
		sig = append(sig, p.SigmaHM)
	}
	return inside, sig, sig2
}

// TestCoastSigmaCoversTurns: aircraft that start a standard-rate (3 deg/s)
// turn the moment their data stops must mostly lie inside the reported
// 1-sigma circle (the filter covariance alone gave ~0 %), while a straight
// flyer keeps a small circle as long as it is being received.
func TestCoastSigmaCoversTurns(t *testing.T) {
	hz := []float64{10, 20, 40}
	runs := 120
	if testing.Short() {
		runs = 40
	}
	type cls struct {
		name      string
		minKt     float64
		maxKt     float64
		turnDps   float64
		lo, hi    float64 // acceptable 1-sigma coverage
		wantSmall bool
	}
	classes := []cls{
		// standard-rate turns at speeds where 3 deg/s needs <= 25 deg bank
		{"turn 3 deg/s", 100, 300, 3, 0.30, 0.90, false},
		// straight flight: the circle is a conservative bound
		{"straight", 100, 450, 0, 0.90, 1.00, true},
	}
	for _, c := range classes {
		in := make([]int, len(hz))
		sum := make([]float64, len(hz))
		var sig2 []float64
		for r := 0; r < runs; r++ {
			kt := c.minKt + (c.maxKt-c.minKt)*float64(r)/float64(runs-1)
			ins, sig, s2 := coastRun(int64(1000+r), kt, c.turnDps, hz)
			for k := range hz {
				if ins[k] {
					in[k]++
				}
				sum[k] += sig[k]
			}
			sig2 = append(sig2, s2)
		}
		for k, h := range hz {
			f := float64(in[k]) / float64(runs)
			t.Logf("%-13s coast %2.0f s: %5.1f %% inside 1-sigma (mean sigma %.0f m, n=%d)", c.name, h, 100*f, sum[k]/float64(runs), runs)
			if f < c.lo || f > c.hi {
				t.Errorf("%s at %.0f s: coverage %.1f %% outside [%.0f, %.0f] %%", c.name, h, 100*f, 100*c.lo, 100*c.hi)
			}
		}
		if c.wantSmall {
			if m := median(sig2); m > 40 {
				t.Errorf("%s: median 1-sigma 2 s after the last position %.0f m, want small", c.name, m)
			} else {
				t.Logf("%s: median 1-sigma 2 s after the last position %.1f m", c.name, m)
			}
		}
	}
}

// TestCoastFloorGeometry checks the closed form of the turn hypothesis.
func TestCoastFloorGeometry(t *testing.T) {
	p := newParams(DefaultConfig().withDefaults())
	var ps pstate
	ps.x[ivx] = 0
	ps.x[ivy] = 80 // north at 80 m/s: 3 deg/s needs 4.19 m/s^2 < bank limit
	ps.since = 0.01
	xx, xy, yy := ps.manoeuvreFloor(&p)
	a := 80 * 3 * deg2rad
	if want := a * 1e-4 / 2; math.Abs(math.Sqrt(xx)-want) > 1e-3*want || yy > 1e-12 || math.Abs(xy) > 1e-9 {
		t.Fatalf("small-t floor xx=%g yy=%g xy=%g want across %g", xx, yy, xy, want)
	}
	ps.since = 30 // 90 deg turn: across = R, along = R*(pi/2 - 1)
	xx, _, yy = ps.manoeuvreFloor(&p)
	r := 80 / (3 * deg2rad)
	if math.Abs(math.Sqrt(xx)-r) > 1e-6*r || math.Abs(math.Sqrt(yy)-r*(math.Pi/2-1)) > 1e-6*r {
		t.Fatalf("90 deg floor across %g along %g, want %g %g", math.Sqrt(xx), math.Sqrt(yy), r, r*(math.Pi/2-1))
	}
	// fast jet: bank-limited (25 deg) lateral acceleration
	ps.x[ivy] = 240
	ps.since = 1e-3
	xx, _, _ = ps.manoeuvreFloor(&p)
	if want := G0 * math.Tan(25*deg2rad) * 1e-6 / 2; math.Abs(math.Sqrt(xx)-want) > 1e-3*want {
		t.Fatalf("bank-limited floor %g want %g", math.Sqrt(xx), want)
	}
	// velocities still received for 10 s after the last position
	ps.since, ps.velGap = 0, 10
	xx, _, yy = ps.manoeuvreFloor(&p)
	if want := 2 * 15.0 * 15.0; math.Abs(xx+yy-want) > 1e-6 {
		t.Fatalf("velocity-only floor variance %g, want %g", xx+yy, want)
	}
	ps.velGap = 0
	// taxiing aircraft get no turn floor
	ps.plan = planGround
	ps.since = 30
	ps.x[ivy] = 10
	ps.P[ix][ix], ps.P[iy][iy] = 25, 25
	ps.fr = newFrame(50, 5)
	if o := ps.output(&p); math.Abs(o.sigH-math.Sqrt(50)) > 1e-9 {
		t.Fatalf("ground sigma %g", o.sigH)
	}
}
