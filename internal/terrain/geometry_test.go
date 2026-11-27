package terrain

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/radio"
)

const (
	stLat = 50.0
	stLon = 10.0
)

var kR = geo.DefaultK * geo.EarthRadiusKm // effective earth radius, km

// exactKnifeEdge evaluates the exact Fresnel-Kirchhoff knife-edge loss
// J(v) = −20·log10( sqrt((1−C−S)² + (C−S)²) / 2 ) by Simpson integration of
// the Fresnel integrals C(v), S(v).
func exactKnifeEdge(v float64) float64 {
	const n = 20000
	var c, s float64
	if v != 0 {
		h := v / n
		for i := 0; i <= n; i++ {
			t := float64(i) * h
			w := 2.0
			if i == 0 || i == n {
				w = 1
			} else if i%2 == 1 {
				w = 4
			}
			c += w * math.Cos(math.Pi*t*t/2)
			s += w * math.Sin(math.Pi*t*t/2)
		}
		c, s = c*h/3, s*h/3
	}
	return -20 * math.Log10(math.Hypot(1-c-s, c-s)/2)
}

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.6g, want %.6g ± %g", name, got, want, tol)
	}
}

func TestKnifeEdgeLoss(t *testing.T) {
	// Reference values of ITU-R P.526 eq. (31), hand-computed.
	near(t, "J(0)", KnifeEdgeLossDB(0), 6.9+20*math.Log10(math.Sqrt(1.01)-0.1), 1e-12)
	near(t, "J(0)", KnifeEdgeLossDB(0), 6.03, 0.01) // ≈ 6 dB at grazing incidence
	near(t, "J(1)", KnifeEdgeLossDB(1), 13.93, 0.01)
	near(t, "J(2.4)", KnifeEdgeLossDB(2.4), 20.54, 0.01)
	near(t, "J(-0.7)", KnifeEdgeLossDB(-0.7), 0.536, 0.005)
	near(t, "J(-0.78)", KnifeEdgeLossDB(-0.78), 0, 0)
	near(t, "J(-3)", KnifeEdgeLossDB(-3), 0, 0)
	near(t, "J(NaN)", KnifeEdgeLossDB(math.NaN()), 0, 0)
	// Large-v asymptote 13 + 20·log10(v).
	near(t, "J(20)", KnifeEdgeLossDB(20), 13+20*math.Log10(20), 0.15)
	// The approximation stays within 0.15 dB of the exact Fresnel integral.
	for v := -0.75; v <= 10; v += 0.25 {
		if d := math.Abs(KnifeEdgeLossDB(v) - exactKnifeEdge(v)); d > 0.15 {
			t.Errorf("J(%.2f): approx %.3f exact %.3f (diff %.3f dB)", v, KnifeEdgeLossDB(v), exactKnifeEdge(v), d)
		}
	}
	near(t, "exact J(0)", exactKnifeEdge(0), 20*math.Log10(2), 1e-9)
}

func TestFresnel(t *testing.T) {
	// λ = 0.27504 m: r1 at mid-path of 20 km = sqrt(λ·5000) = 37.08 m.
	near(t, "r1", FresnelRadiusM(10000, 10000, 0), 37.0836, 0.001)
	near(t, "v", FresnelParameter(10, 10000, 10000, 0), 10*math.Sqrt(2/radio.WavelengthM*2e-4), 1e-12)
	if !math.IsInf(FresnelParameter(1, 0, 100, 0), 1) {
		t.Error("v at the end point should be +Inf for a positive height")
	}
}

func TestDeygoutTwoEdges(t *testing.T) {
	lambda := radio.WavelengthM
	x := []float64{0, 10000, 20000, 30000}
	h := []float64{0, 20, 15, 0}
	loss, edges := deygout(x, h, 0, 3, 0, 1, lambda, nil)
	v1 := 20 * math.Sqrt(2/lambda*(1/10000.0+1/20000.0)) // principal edge at 10 km
	v2 := 5 * math.Sqrt(2/lambda*(1/10000.0+1/10000.0))  // 15 m edge, 5 m above the sub-path 10→30 km
	want := KnifeEdgeLossDB(v1) + KnifeEdgeLossDB(v2)
	near(t, "Deygout loss", loss, want, 1e-9)
	if len(edges) != 2 || edges[0].Index != 1 || edges[1].Index != 2 || edges[0].Level != 0 || edges[1].Level != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	// Single-edge mode.
	l1, e1 := deygout(x, h, 0, 3, 0, 0, lambda, nil)
	near(t, "principal only", l1, KnifeEdgeLossDB(v1), 1e-9)
	if len(e1) != 1 {
		t.Fatalf("edges = %+v", e1)
	}
}

func TestBuildProfileBulge(t *testing.T) {
	o := newOrigin(stLat, stLon)
	la, lo := o.destKm(45, 100)
	p := BuildProfile(FlatSource{}, stLat, stLon, la, lo, 101, 0)
	near(t, "distance", p.DistKm, 100, 1e-6)
	near(t, "bearing", p.BearingDeg, 45, 1e-6)
	mid := p.Points[50]
	near(t, "mid distance", mid.DistKm, 50, 1e-9)
	// Bulge at mid path: d1·d2/(2kR) = 50·50/(2·8494.68) km = 147.15 m.
	near(t, "bulge", mid.BulgeM, 50*50/(2*kR)*1000, 1e-6)
	near(t, "bulge value", mid.BulgeM, 147.15, 0.01)
	if p.Points[0].BulgeM != 0 || p.Points[100].BulgeM != 0 {
		t.Error("bulge at the end points must be 0")
	}
}

// Closed-form 4/3-earth line-of-sight distance with the parabolic bulge:
// d = sqrt(2kR)·(sqrt(h1) + sqrt(h2)).
func closedFormLOSKm(h1M, h2M float64) float64 {
	return math.Sqrt(2*kR) * (math.Sqrt(h1M/1000) + math.Sqrt(h2M/1000))
}

func TestLOSFlatMatchesClosedForm(t *testing.T) {
	o := newOrigin(stLat, stLon)
	for _, tc := range []struct{ agl, altFt float64 }{{10, 10000}, {30, 1000}, {5, 35000}} {
		altM := tc.altFt * geo.MetresPerFoot
		d := closedFormLOSKm(tc.agl, altM)
		for _, f := range []float64{0.98, 1.02} {
			la, lo := o.destKm(123, d*f)
			r := LOS(FlatSource{}, LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: tc.agl, TgtLat: la, TgtLon: lo, TgtAltM: altM, MinObstacleKm: -1})
			if want := f < 1; r.Clear != want {
				t.Errorf("agl %g alt %g ft at %.1f km (%.0f%% of %.1f): clear=%v want %v (minClear %.1f m)",
					tc.agl, tc.altFt, d*f, f*100, d, r.Clear, want, r.MinClearanceM)
			}
			if !r.Clear && r.Obstruction == nil {
				t.Error("blocked path without obstruction point")
			}
			// Free-space part.
			near(t, "FSPL", r.FSPLDB, radio.FSPL1090DB(r.SlantKm), 1e-9)
			near(t, "total", r.TotalLossDB, r.FSPLDB+r.DiffractionDB+r.GasDB, 1e-9)
			if r.GasDB <= 0 || r.GasDB > 3 {
				t.Errorf("gas loss %.3f dB implausible", r.GasDB)
			}
		}
		// Exactly at the closed-form distance the ray grazes the earth:
		// diffraction ≈ J(0) ≈ 6 dB.
		la, lo := o.destKm(123, d)
		r := LOS(FlatSource{}, LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: tc.agl, TgtLat: la, TgtLon: lo, TgtAltM: altM, Samples: 4000, MinObstacleKm: -1})
		if r.DiffractionDB < 4.5 || r.DiffractionDB > 7.5 {
			t.Errorf("grazing diffraction %.2f dB, want ≈ 6", r.DiffractionDB)
		}
		if r.Fresnel60Clear {
			t.Error("grazing path cannot have 0.6 F1 clearance")
		}
		// The closed form agrees with the exact arccos horizon of radio.
		near(t, "closed form vs radio", d, radio.LineOfSightKm(tc.agl, altM, 0), d*0.002)
	}
}

func TestPanoramaFlatHorizon(t *testing.T) {
	pt, err := SamplePolar(context.Background(), FlatSource{}, PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 500, BearingStepDeg: 2, SampleKm: 0.25}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pt.Rays() != 180 || pt.SamplesPerRay() != 2000 {
		t.Fatalf("grid %d×%d", pt.Rays(), pt.SamplesPerRay())
	}
	const agl = 10.0
	p := NewPanorama(pt, agl, 0, -1)
	hk, dip := p.SmoothHorizon()
	near(t, "smooth horizon", hk, geo.HorizonKm(agl, 0), 1e-9)
	h := p.Horizon()
	for i := range h.ElevationDeg {
		// The skyline is the sea-level horizon: dip angle at ~13 km.
		near(t, "skyline", h.ElevationDeg[i], dip, 0.002)
		near(t, "skyline distance", h.DistanceKm[i], hk, 0.3)
		if h.NearestObstacleKm[i] != -1 {
			t.Fatalf("flat terrain has no obstacle above the horizontal (got %v)", h.NearestObstacleKm[i])
		}
	}
	alts := []float64{1000, 10000, 30000, 40000}
	rs := p.LOSRanges(alts)
	for _, r := range rs {
		want := radio.LineOfSightKm(agl, r.AltM, 0)
		if want > p.RadiusKm {
			if r.Capped != len(r.RangeKm) {
				t.Errorf("alt %g: %d capped rays, want all", r.AltFt, r.Capped)
			}
			continue
		}
		near(t, "smoothKm", r.SmoothKm, want, 0.001)
		for i, km := range r.RangeKm {
			if math.Abs(km-want) > 0.5 {
				t.Fatalf("alt %g ray %d: range %.3f km, closed form %.3f km", r.AltFt, i, km, want)
			}
		}
		near(t, "closed-form 4/3 earth", r.MeanKm, closedFormLOSKm(agl, r.AltM), 0.6)
		near(t, "area", r.AreaKm2, math.Pi*want*want, math.Pi*want*want*0.01)
	}
	// Visibility just inside / outside the horizon at 10 000 ft.
	o := newOrigin(stLat, stLon)
	d := radio.LineOfSightKm(agl, 10000*geo.MetresPerFoot, 0)
	la, lo := o.destKm(77, d-2)
	if v := p.Visible(la, lo, 10000); !v.Visible || v.MarginDeg <= 0 {
		t.Errorf("inside horizon not visible: %+v", v)
	}
	la, lo = o.destKm(77, d+2)
	if v := p.Visible(la, lo, 10000); v.Visible {
		t.Errorf("beyond horizon visible: %+v", v)
	}
	// FloorFt at that distance ≈ 10 000 ft.
	la, lo = o.destKm(200, d)
	near(t, "floor at horizon distance", p.Visible(la, lo, 10000).FloorFt, 10000, 60)
}

func ridgeSource() ArcRidgeSource {
	return ArcRidgeSource{Lat: stLat, Lon: stLon, BearingDeg: 0, HalfAngleDeg: 20, DistKm: 30, WidthKm: 1, HeightM: 500}
}

// expectedRidgeMinAltM is the parabolic-bulge altitude needed at distKm
// north to clear the ridge edge at 30 km from a 10 m antenna.
func expectedRidgeMinAltM(distKm float64) float64 {
	best := 0.0
	for _, x := range []float64{30, 31} {
		t := 500 + x*(distKm-x)/(2*kR)*1000
		if h := 10 + (t-10)*distKm/x; h > best {
			best = h
		}
	}
	return best
}

func TestLOSRidge(t *testing.T) {
	src := ridgeSource()
	o := newOrigin(stLat, stLon)
	la, lo := o.destKm(0, 100)
	want := expectedRidgeMinAltM(100) // ≈ 2055 m
	near(t, "expected", want, 2055.3, 0.5)
	in := LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: 10, TgtLat: la, TgtLon: lo, KeepProfile: true}

	in.TgtAltM = want + 40
	r := LOS(src, in)
	if !r.Clear || r.Obstruction != nil {
		t.Fatalf("40 m above the critical altitude should be clear: %+v", r.Obstruction)
	}
	if r.Fresnel60Clear {
		t.Error("a path 40 m over the ridge cannot clear 0.6 F1 (r1 ≈ 75 m)")
	}
	if r.DiffractionDB <= 0 || r.DiffractionDB >= 6 {
		t.Errorf("near-grazing clear path: diffraction %.2f dB, want 0 < J < 6", r.DiffractionDB)
	}
	if len(r.Profile) != r.Samples || r.Samples < 1000 {
		t.Errorf("profile %d samples", len(r.Profile))
	}

	in.TgtAltM = want - 40
	r = LOS(src, in)
	if r.Clear || r.Obstruction == nil {
		t.Fatal("40 m below the critical altitude should be blocked")
	}
	if d := r.Obstruction.DistKm; d < 29.9 || d > 31.1 {
		t.Errorf("obstruction at %.2f km, want the ridge at 30-31 km", d)
	}
	near(t, "obstruction terrain", r.Obstruction.TerrainM, 500, 0)
	if r.DiffractionDB < 6 || len(r.Edges) == 0 {
		t.Errorf("blocked: diffraction %.2f dB, edges %d", r.DiffractionDB, len(r.Edges))
	}
	if d := r.Edges[0].DistKm; d < 29.9 || d > 31.1 {
		t.Errorf("principal edge at %.2f km", d)
	}
	near(t, "total loss", r.TotalLossDB, r.FSPLDB+r.GasDB+r.DiffractionDB, 1e-9)

	// The southern path is not affected by the ridge.
	la, lo = o.destKm(180, 100)
	in.TgtLat, in.TgtLon, in.TgtAltM = la, lo, 1500
	if r := LOS(src, in); !r.Clear || r.DiffractionDB != 0 {
		t.Errorf("south path blocked: %+v", r.Obstruction)
	}
	// JSON round trip (no NaN anywhere).
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
}

func ridgePanorama(t *testing.T) (*PolarTerrain, *Panorama) {
	t.Helper()
	pt, err := SamplePolar(context.Background(), ridgeSource(), PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 200, BearingStepDeg: 1, SampleKm: 0.25}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pt, NewPanorama(pt, 10, 0, 0)
}

func TestPanoramaRidge(t *testing.T) {
	_, p := ridgePanorama(t)
	h := p.Horizon()
	hi := geo.ElevationDeg(30, 10, 500, 0)
	lo := geo.ElevationDeg(30.25, 10, 500, 0)
	for _, brg := range []int{0, 10, 350, 345, 15} {
		a := h.ElevationDeg[brg]
		if a > hi+1e-4 || a < lo-1e-4 {
			t.Errorf("bearing %d: skyline %.4f°, want %.4f..%.4f", brg, a, lo, hi)
		}
		if d := h.DistanceKm[brg]; d < 29.99 || d > 30.26 {
			t.Errorf("bearing %d: limiting obstacle at %.3f km", brg, d)
		}
		if d := h.NearestObstacleKm[brg]; d < 29.99 || d > 30.26 {
			t.Errorf("bearing %d: nearest obstacle at %.3f km", brg, d)
		}
		near(t, "obstacle terrain", h.TerrainM[brg], 500, 0)
	}
	_, dip := p.SmoothHorizon()
	for _, brg := range []int{90, 180, 270, 30, 330} {
		near(t, "open skyline", h.ElevationDeg[brg], dip, 0.002)
		if h.NearestObstacleKm[brg] != -1 {
			t.Errorf("bearing %d: unexpected obstacle", brg)
		}
	}
	near(t, "max skyline", h.MaxSkylineDeg, hi, 0.01)

	// Per-aircraft visibility north of the ridge agrees with the geometry.
	o := newOrigin(stLat, stLon)
	ang := geo.ElevationDeg(30, 10, 500, 0)
	floor := altitudeOnRay(100, 10, ang, p.K)
	near(t, "floor vs parabolic LOS", floor, expectedRidgeMinAltM(100), 15)
	la, lon := o.destKm(0, 100)
	vUp := p.Visible(la, lon, (floor+60)*geo.FeetPerMetre)
	vDn := p.Visible(la, lon, (floor-60)*geo.FeetPerMetre)
	if !vUp.Visible || vDn.Visible {
		t.Errorf("visibility around the floor: above=%+v below=%+v", vUp, vDn)
	}
	near(t, "margin sign", math.Copysign(1, vDn.MarginDeg), -1, 0)
	floorLo := altitudeOnRay(100, 10, geo.ElevationDeg(30.25, 10, 500, 0), p.K)
	if f := vUp.FloorFt * geo.MetresPerFoot; f < floorLo-1 || f > floor+1 {
		t.Errorf("floor %.1f m, want %.1f..%.1f", f, floorLo, floor)
	}
	// Before the ridge, low aircraft are visible.
	la, lon = o.destKm(0, 20)
	if v := p.Visible(la, lon, 1000); !v.Visible {
		t.Errorf("aircraft before the ridge hidden: %+v", v)
	}
	// LOS and the panorama agree.
	for _, alt := range []float64{1900, 2250} {
		la, lon = o.destKm(0, 100)
		l := LOS(ridgeSource(), LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: 10, TgtLat: la, TgtLon: lon, TgtAltM: alt})
		v := p.Visible(la, lon, alt*geo.FeetPerMetre)
		if l.Clear != v.Visible {
			t.Errorf("alt %g m: LOS clear=%v, panorama visible=%v", alt, l.Clear, v.Visible)
		}
	}
	// Coverage: north range at 5 000 ft is ridge-limited.
	rs := p.LOSRanges([]float64{5000})
	altM := 5000 * geo.MetresPerFoot
	// distance where the ridge line reaches 5000 ft
	dWant := bisect(func(d float64) float64 { return altitudeOnRay(d, 10, ang, p.K) - altM }, 30, 200)
	near(t, "north range", rs[0].RangeKm[0], dWant, 1.0)
	near(t, "south range", rs[0].RangeKm[180], radio.LineOfSightKm(10, altM, 0), 0.6)
}

func bisect(f func(float64) float64, lo, hi float64) float64 {
	for i := 0; i < 100; i++ {
		m := (lo + hi) / 2
		if (f(lo) < 0) == (f(m) < 0) {
			lo = m
		} else {
			hi = m
		}
	}
	return (lo + hi) / 2
}

func TestForecastRidge(t *testing.T) {
	_, p := ridgePanorama(t)
	o := newOrigin(stLat, stLon)
	const altM = 1500.0
	ang := geo.ElevationDeg(30, 10, 500, 0)
	dLost := bisect(func(d float64) float64 { return altitudeOnRay(d, 10, ang, p.K) - altM }, 31, 150)
	// Fly north from 20 km to 120 km, one point per km, 10 s per km.
	var path []PathPoint
	for d := 20.0; d <= 120; d++ {
		la, lo := o.destKm(0, d)
		path = append(path, PathPoint{TMs: int64((d - 20) * 10000), Lat: la, Lon: lo, AltFt: altM * geo.FeetPerMetre})
	}
	f := p.Forecast(path)
	if !f.Known || !f.VisibleNow || f.LostAtMs == 0 || f.AcquiredAtMs != 0 {
		t.Fatalf("forecast %+v", f)
	}
	// The sampled ridge edge lies between 30.0 and 30.25 km.
	angLo := geo.ElevationDeg(30.25, 10, 500, 0)
	dLostLo := bisect(func(d float64) float64 { return altitudeOnRay(d, 10, angLo, p.K) - altM }, 31, 150)
	if lt := float64(f.LostAtMs); lt < (dLost-20)*10000-1000 || lt > (dLostLo-20)*10000+1000 {
		t.Errorf("lost at %.0f ms, want %.0f..%.0f", lt, (dLost-20)*10000, (dLostLo-20)*10000)
	}
	// Reverse: an inbound aircraft is acquired at the same place.
	rev := make([]PathPoint, len(path))
	for i := range path {
		rev[i] = path[len(path)-1-i]
		rev[i].TMs = path[i].TMs
	}
	f = p.Forecast(rev)
	if f.VisibleNow || f.AcquiredAtMs == 0 || f.LostAtMs != 0 {
		t.Fatalf("reverse forecast %+v", f)
	}
	if at := float64(f.AcquiredAtMs); at < (120-dLostLo)*10000-1000 || at > (120-dLost)*10000+1000 {
		t.Errorf("acquired at %.0f ms, want %.0f..%.0f", at, (120-dLostLo)*10000, (120-dLost)*10000)
	}
	if g := p.Forecast(nil); g.Known {
		t.Error("empty path should be unknown")
	}
}

func TestAdviceRidge(t *testing.T) {
	pt, p := ridgePanorama(t)
	c := &AntennaCoverage{ID: "a", HeightAGLM: 10, AntennaM: 10, pano: p}
	alts := []float64{1000, 10000}
	c.LOS = p.LOSRanges(alts)
	adv := computeAdvice(pt, c, p.K, p.MinObstacleKm, alts, nil)
	if len(adv.Heights) != 4 || adv.Heights[0].ExtraM != 0 || adv.Heights[3].ExtraM != 10 {
		t.Fatalf("heights %+v", adv.Heights)
	}
	for i := range alts {
		if !(adv.Heights[3].AreaKm2[i] > adv.Heights[0].AreaKm2[i]) || !(adv.Km2PerMetre[i] > 0) {
			t.Errorf("alt %g: +10 m gives no more area (%v vs %v)", alts[i], adv.Heights[3].AreaKm2[i], adv.Heights[0].AreaKm2[i])
		}
		if !(adv.Heights[1].AreaGainPct[i] > 0) || adv.Heights[0].AreaGainPct[i] != 0 {
			t.Errorf("gain pct %+v", adv.Heights[1].AreaGainPct)
		}
	}
	if adv.RefAltFt != 10000 || len(adv.Sectors) != 36 {
		t.Fatalf("ref %g sectors %d", adv.RefAltFt, len(adv.Sectors))
	}
	worst := map[int]bool{}
	for _, s := range adv.WorstSectors {
		worst[s] = true
	}
	for _, s := range []int{0, 1, 34, 35} { // the ridge spans 340°..20°
		if !worst[s] {
			t.Errorf("sector %d not among the worst: %v", s, adv.WorstSectors)
		}
		if adv.Sectors[s].NearestObstacleKm < 29.9 || adv.Sectors[s].NearestObstacleKm > 30.3 {
			t.Errorf("sector %d nearest obstacle %.2f km", s, adv.Sectors[s].NearestObstacleKm)
		}
	}
	if adv.Sectors[18].BlockedPct > 1 || adv.Sectors[18].NearestObstacleKm != -1 {
		t.Errorf("south sector blocked: %+v", adv.Sectors[18])
	}
	if len(adv.Summary) < 2 {
		t.Errorf("summary %v", adv.Summary)
	}
	checkAdviceLines(t, adv)
	if _, err := json.Marshal(adv); err != nil {
		t.Fatal(err)
	}
}

func TestPanoramaCone(t *testing.T) {
	o := newOrigin(stLat, stLon)
	cLat, cLon := o.destKm(90, 20)
	src := ConeSource{Lat: cLat, Lon: cLon, PeakM: 1000, RadiusKm: 5}
	pt, err := SamplePolar(context.Background(), src, PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 100, BearingStepDeg: 0.5, SampleKm: 0.1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPanorama(pt, 10, 0, 0)
	h := p.Horizon()
	i := int(90 / h.StepDeg)
	near(t, "cone skyline", h.ElevationDeg[i], geo.ElevationDeg(20, 10, 1000, 0), 0.01)
	near(t, "cone distance", h.DistanceKm[i], 20, 0.11)
	near(t, "max bearing", h.MaxSkylineBrgDeg, 90, 0.01)
	// The cone subtends ±asin(5/20) ≈ ±14.5°; at 80° the skyline is lower.
	if !(h.ElevationDeg[int(80/h.StepDeg)] < h.ElevationDeg[i]) || !(h.ElevationDeg[int(80/h.StepDeg)] > 0) {
		t.Errorf("cone flank skyline %.3f", h.ElevationDeg[int(80/h.StepDeg)])
	}
	near(t, "cone outside", h.ElevationDeg[int(70/h.StepDeg)], -0.1, 0.1)

	// Single-edge diffraction behind the cone: target 40 km east at an
	// altitude 100 m below the line over the peak.
	la, lo := o.destKm(90, 40)
	need := 10 + (1000+20*20/(2*kR)*1000-10)*2 // parabolic geometry over the peak
	r := LOS(src, LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: 10, TgtLat: la, TgtLon: lo, TgtAltM: need - 100, Samples: 2001, MaxEdges: 1})
	if r.Clear || len(r.Edges) != 1 {
		t.Fatalf("behind the cone: clear=%v edges=%d", r.Clear, len(r.Edges))
	}
	near(t, "edge at peak", r.Edges[0].DistKm, 20, 0.05)
	// h ≈ 50 m above the ray at mid path, d1 = d2 = 20 km.
	v := 50 * math.Sqrt(2/radio.WavelengthM*(2/20000.0))
	near(t, "v", r.Edges[0].V, v, 0.05)
	near(t, "J", r.DiffractionDB, KnifeEdgeLossDB(v), 0.3)
}

func TestRadioRanges(t *testing.T) {
	pt, err := SamplePolar(context.Background(), FlatSource{}, PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 600, BearingStepDeg: 10, SampleKm: 0.5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPanorama(pt, 10, 0, 0)
	ant := model.Antenna{ID: "v", Type: model.AntVertical, GainDBi: 5, HeightAGLM: 10}
	alts := []float64{10000, 35000}
	los := p.LOSRanges(alts)
	// A strong link: range extends somewhat beyond the LOS horizon by diffraction.
	strong := radio.NewLink(ant, 10, radio.LinkParams{TxPowerW: 500, ReceiverSensitivityDBm: -100})
	rr := p.RadioRanges(strong, alts)
	for i := range alts {
		if !(rr[i].MeanKm > los[i].MeanKm) || rr[i].MeanKm > los[i].MeanKm+80 {
			t.Errorf("alt %g: radio %.1f km vs LOS %.1f km", alts[i], rr[i].MeanKm, los[i].MeanKm)
		}
		// Omni antenna: all bearings equal.
		near(t, "omni spread", rr[i].MaxKm-rr[i].MinKm, 0, 1e-9)
	}
	// A deaf receiver is power-limited well inside the horizon, consistent
	// with radio.Link.MaxRangeKm (which has no terrain).
	weak := radio.NewLink(ant, 10, radio.LinkParams{ReceiverSensitivityDBm: -80})
	rw := p.RadioRanges(weak, []float64{35000})
	km, limit := weak.MaxRangeKm(0, 35000*geo.MetresPerFoot)
	if limit != radio.LimitPower {
		t.Fatalf("expected a power-limited link, got %s", limit)
	}
	near(t, "power-limited range", rw[0].MeanKm, km, 1.5)
}

func TestPolarCodecRoundTrip(t *testing.T) {
	pt, err := SamplePolar(context.Background(), ridgeSource(), PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 50, BearingStepDeg: 5, SampleKm: 0.5, Zoom: 11}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := pt.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ReadPolarTerrain(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if back.Spec != pt.Spec || back.rays != pt.rays || back.samples != pt.samples || len(back.elev) != len(pt.elev) {
		t.Fatalf("spec %+v vs %+v", back.Spec, pt.Spec)
	}
	for i := range pt.elev {
		if back.elev[i] != pt.elev[i] {
			t.Fatalf("sample %d differs", i)
		}
	}
	if _, err := ReadPolarTerrain(bytes.NewReader([]byte("garbage"))); err == nil {
		t.Error("garbage accepted")
	}
}

func TestSamplePolarCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SamplePolar(ctx, FlatSource{}, PolarSpec{Lat: stLat, Lon: stLon}, nil); err == nil {
		t.Error("cancelled sampling returned no error")
	}
}

func TestNoDataSource(t *testing.T) {
	pt, err := SamplePolar(context.Background(), NoDataSource{}, PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 10, BearingStepDeg: 10, SampleKm: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "no data fraction", pt.NoDataFraction(), 1, 0)
	r := LOS(NoDataSource{}, LOSInput{RxLat: stLat, RxLon: stLon, RxAGLM: 10, TgtLat: 50.5, TgtLon: 10, TgtAltM: 3000})
	if r.NoDataSamples != r.Samples || !r.Clear {
		t.Errorf("no-data LOS: %+v", r)
	}
}

func TestVisibleNoAlloc(t *testing.T) {
	_, p := ridgePanorama(t)
	a := &Analysis{Antennas: []*AntennaCoverage{{ID: "x", pano: p}, {ID: "y", pano: p}}}
	allocs := testing.AllocsPerRun(1000, func() {
		_ = a.Visible("", 50.5, 10.2, 12000)
		_ = p.Visible(50.5, 10.2, 12000)
	})
	if allocs != 0 {
		t.Errorf("Visible allocates %.1f times", allocs)
	}
	var nilA *Analysis
	if v := nilA.Visible("", 50, 10, 1000); v.Known {
		t.Error("nil analysis known")
	}
}

func BenchmarkPanoramaVisible(b *testing.B) {
	pt, _ := SamplePolar(context.Background(), ridgeSource(), PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 450, BearingStepDeg: 0.5}, nil)
	p := NewPanorama(pt, 10, 0, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Visible(50.3+float64(i%100)*0.01, 10.5, 15000)
	}
}

func BenchmarkNewPanorama(b *testing.B) {
	pt, _ := SamplePolar(context.Background(), ridgeSource(), PolarSpec{Lat: stLat, Lon: stLon, RadiusKm: 450, BearingStepDeg: 0.5, SampleKm: 0.2}, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewPanorama(pt, 10, 0, 0)
	}
}
