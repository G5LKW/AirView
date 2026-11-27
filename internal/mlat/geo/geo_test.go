package geo

import (
	"math"
	"math/rand"
	"testing"

	"airview/internal/geoid"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if !(math.Abs(got-want) <= tol) {
		t.Errorf("%s = %.12g, want %.12g ± %g", name, got, want, tol)
	}
}

func TestToECEFKnown(t *testing.T) {
	v := ToECEF(0, 0, 0)
	near(t, "x(0,0)", v.X, WGS84A, 1e-9)
	near(t, "y(0,0)", v.Y, 0, 1e-9)
	v = ToECEF(90, 123, 0)
	near(t, "z(pole)", v.Z, WGS84B, 1e-6)
	near(t, "rho(pole)", math.Hypot(v.X, v.Y), 0, 1e-6)
	v = ToECEF(0, 90, 1000)
	near(t, "y(0,90,1km)", v.Y, WGS84A+1000, 1e-9)
	near(t, "b", WGS84B, 6356752.314245, 1e-6)
	// A published point: NGS-style check, 45°N 45°E, h = 0.
	v = ToECEF(45, 45, 0)
	near(t, "x(45,45)", v.X, 3194419.145, 1e-3)
	near(t, "z(45,45)", v.Z, 4487348.409, 1e-3)
}

func TestECEFRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var maxH, maxLat float64
	for i := 0; i < 200000; i++ {
		lat := rng.Float64()*180 - 90
		lon := rng.Float64()*360 - 180
		h := rng.Float64()*1.2e6 - 1e4
		switch i % 50 {
		case 0:
			lat = 90
		case 1:
			lat = -90
		case 2:
			lat = 0
		}
		g := FromECEF(ToECEF(lat, lon, h))
		maxH = math.Max(maxH, math.Abs(g.H-h))
		dl := math.Abs(g.Lat - lat)
		maxLat = math.Max(maxLat, dl)
		if math.Abs(lat) < 89.9999 {
			dlon := math.Mod(math.Abs(g.Lon-lon), 360)
			if dlon > 180 {
				dlon = 360 - dlon
			}
			if dlon > 1e-9 {
				t.Fatalf("lon %v -> %v", lon, g.Lon)
			}
		}
	}
	if maxH > 1e-6 || maxLat > 1e-11 {
		t.Errorf("round trip: max |dh| = %g m, max |dlat| = %g°", maxH, maxLat)
	}
}

func TestFromECEFDeepInterior(t *testing.T) {
	// Points within the evolute region use the iterative fallback; check it
	// is self-consistent.
	for _, v := range []Vec3{{1000, 2000, 3000}, {0, 0, 1000}, {30000, 0, 0}, {0, 0, 0}} {
		g := FromECEF(v)
		if math.IsNaN(g.Lat) || math.IsNaN(g.H) {
			t.Fatalf("%v -> NaN", v)
		}
		if v != (Vec3{}) {
			back := g.ECEF()
			if back.Dist(v) > 1e-3 {
				t.Errorf("%v -> %+v -> %v", v, g, back)
			}
		}
	}
}

func TestGeocentricHeight(t *testing.T) {
	for _, lat := range []float64{0, 30, 45, 60, 89} {
		for _, h := range []float64{0, 1000, 12000, 40000} {
			got := GeocentricHeight(ToECEF(lat, 10, h))
			// The approximation error is h·(1-cos δ) with δ ≤ 0.19°.
			near(t, "geocentric height", got, h, 1e-5*h+1e-6)
		}
	}
}

func TestRadii(t *testing.T) {
	near(t, "surface radius eq", SurfaceRadius(0), WGS84A, 1e-6)
	near(t, "surface radius pole", SurfaceRadius(90), WGS84B, 1e-6)
	near(t, "gaussian radius 45", GaussianRadius(45), 6378137*math.Sqrt(1-WGS84E2)/(1-WGS84E2/2), 1e-6)
	if r := MeanEarthRadius; r < 6371000 || r > 6371010 {
		t.Errorf("mean radius %v", r)
	}
	u := Up(30, 40)
	near(t, "|up|", u.Norm(), 1, 1e-15)
}

func TestFrame(t *testing.T) {
	o := LLH{51.5, -0.12, 45}
	f := NewFrame(o)
	// Origin maps to zero.
	e := f.ToENU(o.ECEF())
	near(t, "origin", e.Norm(), 0, 1e-8)
	// A point 1000 m straight up.
	e = f.ToENU(LLH{o.Lat, o.Lon, o.H + 1000}.ECEF())
	near(t, "up.U", e.Z, 1000, 1e-6)
	near(t, "up.E", e.X, 0, 1e-6)
	// A point slightly north is north and a bit below the tangent plane.
	e = f.ToENU(LLH{o.Lat + 0.01, o.Lon, o.H}.ECEF())
	if !(e.Y > 1100 && e.Y < 1115 && math.Abs(e.X) < 1e-6 && e.Z < 0) {
		t.Errorf("north point ENU %+v", e)
	}
	// Round trip.
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 1000; i++ {
		p := Vec3{rng.NormFloat64() * 1e5, rng.NormFloat64() * 1e5, rng.NormFloat64() * 1e4}
		back := f.ToENU(f.FromENU(p))
		if back.Dist(p) > 1e-6 {
			t.Fatalf("ENU round trip %v -> %v", p, back)
		}
	}
	// Orthonormal axes.
	near(t, "E·N", f.East().Dot(f.North()), 0, 1e-15)
	near(t, "E×N·U", f.East().Cross(f.North()).Dot(f.Up()), 1, 1e-15)
	// Az/El.
	az, el, r := f.AzElRange(f.FromENU(Vec3{1000, 0, 1000}))
	near(t, "az east", az, 90, 1e-9)
	near(t, "el 45", el, 45, 1e-9)
	near(t, "range", r, math.Sqrt2*1000, 1e-6)
	az, _, _ = f.AzElRange(f.FromENU(Vec3{-1000, -1000, 0}))
	near(t, "az SW", az, 225, 1e-9)
	// Covariance rotation round trip and trace invariance.
	c := [9]float64{4, 1, 0.5, 1, 9, 0.2, 0.5, 0.2, 16}
	ce := f.CovToENU(c)
	near(t, "trace", ce[0]+ce[4]+ce[8], 29, 1e-9)
	back := f.CovToECEF(ce)
	for i := range c {
		near(t, "cov round trip", back[i], c[i], 1e-9)
	}
	// A pure "up" variance in ENU appears along the normal in ECEF.
	cu := f.CovToECEF([9]float64{0, 0, 0, 0, 0, 0, 0, 0, 1})
	u := f.Up()
	near(t, "up cov xz", cu[2], u.X*u.Z, 1e-12)
}

func TestNormalize(t *testing.T) {
	a := Atmosphere{}.Normalize()
	if a != DefaultAtmosphere() {
		t.Errorf("zero -> %+v", a)
	}
	a = Atmosphere{N0: 330, H: 6500, K: 1.5}.Normalize()
	if a.N0 != 330 || a.H != 6500 || a.K != KAtmMax {
		t.Errorf("clamp -> %+v", a)
	}
	a = Atmosphere{N0: 330, H: 6500, K: 0.1}.Normalize()
	if a.K != KAtmMin {
		t.Errorf("clamp low -> %+v", a)
	}
	a = Atmosphere{N0: math.NaN(), H: 6500, K: math.NaN()}.Normalize()
	if a.N0 != DefaultN0 || a.K != 1 {
		t.Errorf("NaN -> %+v", a)
	}
}

func TestExcessProperties(t *testing.T) {
	atm := DefaultAtmosphere()
	rx := LLH{52, 0, 50}.ECEF()
	// Symmetric.
	ac := LLH{54.5, 2, 11000}.ECEF()
	near(t, "symmetric", atm.Range(rx, ac), atm.Range(ac, rx), 1e-6)
	// Zero length.
	if atm.Range(rx, rx) != 0 || atm.Excess(rx, rx) != 0 {
		t.Error("zero-length path")
	}
	// Short horizontal path at the surface: excess = N0·1e-6·D.
	// The profile is referenced to mean sea level (finding M5): heights
	// are the ellipsoid heights minus the geoid undulation.
	und, _ := geoid.Undulation(52, 0.005)
	p2 := LLH{52, 0.01, 50}.ECEF()
	D := rx.Dist(p2)
	near(t, "surface excess", atm.Excess(rx, p2), atm.N(50-und)*1e-6*D, 1e-4)
	// Vertical path: closed form is exact.
	up := LLH{52, 0, 10050}.ECEF()
	near(t, "vertical excess", atm.Excess(rx, up), atm.StraightExcess(rx.Dist(up), 50-und, 10050-und), 1e-3)
	// Excess scales with K (to first order).
	b := atm
	b.K = 1.1
	e1, e2 := atm.Excess(rx, ac), b.Excess(rx, ac)
	if r := e2 / e1; r < 1.09 || r > 1.11 {
		t.Errorf("K scaling ratio %v", r)
	}
	// Excess grows as elevation falls (same range).
	f := NewFrame(LLH{52, 0, 50})
	prev := 0.0
	for _, el := range []float64{30, 10, 5, 2, 1} {
		p := f.FromENU(Vec3{0, 100e3 * math.Cos(el*deg), 100e3 * math.Sin(el*deg)})
		e := atm.Excess(f.Origin, p)
		if e <= prev {
			t.Errorf("excess at %v° = %v not above %v", el, e, prev)
		}
		prev = e
	}
	ns := atm.PropagationNs(rx, ac)
	near(t, "ns", ns, atm.Range(rx, ac)/C*1e9, 1e-9)
}

func TestRangeGradFiniteDifference(t *testing.T) {
	atm := DefaultAtmosphere()
	rx := LLH{52, 0, 50}.ECEF()
	for _, tg := range []LLH{{55.5, 1, 11000}, {52.3, 0.2, 3000}, {52, 0, 9000}, {50, -3, 6000}} {
		tgt := tg.ECEF()
		L, g := atm.RangeGrad(rx, tgt)
		near(t, "L", L, atm.Range(rx, tgt), 1e-9)
		const h = 1.0
		for k, dv := range []Vec3{{h, 0, 0}, {0, h, 0}, {0, 0, h}} {
			fd := (atm.Range(rx, tgt.Add(dv)) - atm.Range(rx, tgt.Sub(dv))) / (2 * h)
			gk := [3]float64{g.X, g.Y, g.Z}[k]
			// The analytic gradient neglects only dvz/dp (< 1e-6).
			if math.Abs(fd-gk) > 1e-6 {
				t.Errorf("%+v axis %d: grad %.9f, finite difference %.9f", tg, k, gk, fd)
			}
		}
		// The gradient is n(tgt) times the arrival tangent.
		n := 1 + atm.N(tg.H)*1e-6
		near(t, "|grad|", g.Norm(), n, 2e-6)
	}
}

func TestPropagationNsDist(t *testing.T) {
	atm := DefaultAtmosphere()
	R := MeanEarthRadius
	// Same as the 3-D model on the sphere.
	theta := 300e3 / R
	p1, p2 := sphereEnds(R, 50, 11000, theta)
	m := atm
	m.sphereR = R
	near(t, "dist form", atm.PropagationNsDist(p1.Dist(p2), 50, 11000), m.PropagationNs(p1, p2), 1e-6)
	// Against the ellipsoid at mid latitudes: < 1 ns.
	rx := LLH{52, 0, 50}.ECEF()
	ac := LLH{54.5, 2, 11000}.ECEF()
	und, _ := geoid.Undulation(53.25, 1) // chord midpoint, roughly
	near(t, "dist vs ellipsoid", atm.PropagationNsDist(rx.Dist(ac), 50-und, 11000-und), atm.PropagationNs(rx, ac), 1)
	if atm.PropagationNsDist(0, 0, 0) != 0 {
		t.Error("zero distance")
	}
}

func TestLineOfSight(t *testing.T) {
	// 4/3-earth radio horizon of a 10 m antenna and a 10 km aircraft is
	// ~425 km.
	hz := RadioHorizon(10, 10000, 0)
	if hz < 420e3 || hz > 430e3 {
		t.Fatalf("radio horizon %v", hz)
	}
	rx := LLH{0, 0, 10}.ECEF()
	for _, c := range []struct {
		km   float64
		want bool
	}{{100, true}, {400, true}, {415, true}, {440, false}, {600, false}} {
		lon := c.km * 1000 / (WGS84A * deg)
		ac := LLH{0, lon, 10000}.ECEF()
		if got := LineOfSight(rx, ac, 0); got != c.want {
			t.Errorf("%v km: LOS %v, want %v (clearance %.0f m)", c.km, got, c.want, Clearance(rx, ac, 0))
		}
	}
	// k = 1 (geometric) is stricter than 4/3.
	ac := LLH{0, 3.5, 10000}.ECEF()
	if Clearance(rx, ac, 1) >= Clearance(rx, ac, DefaultKFactor) {
		t.Error("k=1 should give less clearance")
	}
	// Two points at the same height with nothing between them.
	a, b := LLH{10, 10, 500}.ECEF(), LLH{10, 10.001, 500}.ECEF()
	near(t, "short clearance", Clearance(a, b, 0), 500, 0.1)
}

func BenchmarkToECEF(b *testing.B) {
	var s float64
	for i := 0; i < b.N; i++ {
		s += ToECEF(51.5, float64(i&63)*0.01, 1000).X
	}
	_ = s
}

func BenchmarkFromECEF(b *testing.B) {
	v := ToECEF(51.5, -0.12, 10000)
	var s float64
	for i := 0; i < b.N; i++ {
		v.X += 1e-3
		s += FromECEF(v).H
	}
	_ = s
}

func BenchmarkRange(b *testing.B) {
	atm := DefaultAtmosphere()
	rx := LLH{52, 0, 50}.ECEF()
	ac := LLH{54.5, 2, 11000}.ECEF()
	b.ReportAllocs()
	var s float64
	for i := 0; i < b.N; i++ {
		ac.X += 1e-3
		s += atm.Range(rx, ac)
	}
	_ = s
}

func BenchmarkRangeGrad(b *testing.B) {
	atm := DefaultAtmosphere()
	rx := LLH{52, 0, 50}.ECEF()
	ac := LLH{54.5, 2, 11000}.ECEF()
	b.ReportAllocs()
	var s float64
	for i := 0; i < b.N; i++ {
		ac.X += 1e-3
		l, g := atm.RangeGrad(rx, ac)
		s += l + g.X
	}
	_ = s
}

func BenchmarkClearance(b *testing.B) {
	rx := LLH{52, 0, 50}.ECEF()
	ac := LLH{54.5, 2, 11000}.ECEF()
	b.ReportAllocs()
	var s float64
	for i := 0; i < b.N; i++ {
		s += Clearance(rx, ac, 0)
	}
	_ = s
}

// Finding M5: N0 is a sea-level refractivity, so the profile must be
// evaluated above the geoid, not above the ellipsoid. In the Indian Ocean
// geoid low (N ≈ −100 m) the ellipsoid reference overstated every excess
// by about exp(100/7000) − 1 = 1.4 %.
func TestRefractivityGeoidReference(t *testing.T) {
	atm := DefaultAtmosphere()
	lat, lon := 5.0, 78.0
	und, ok := geoid.Undulation(lat, lon)
	if !ok || und > -80 {
		t.Fatalf("expected a deep geoid low at %v,%v, got %v (ok=%v)", lat, lon, und, ok)
	}
	rx := LLH{lat, lon, und + 50}.ECEF() // 50 m above MSL
	up := LLH{lat, lon, und + 10050}.ECEF()
	got := atm.Excess(rx, up)
	want := atm.StraightExcess(rx.Dist(up), 50, 10050)
	near(t, "vertical excess above MSL", got, want, 1e-3)
	wrong := atm.StraightExcess(rx.Dist(up), und+50, und+10050)
	if math.Abs(got-wrong) < 0.01 {
		t.Errorf("excess %v is the ellipsoid-referenced value %v", got, wrong)
	}
}
