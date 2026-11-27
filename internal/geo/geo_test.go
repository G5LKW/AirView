package geo

import (
	"math"
	"math/rand"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.9g, want %.9g (±%g)", name, got, want, tol)
	}
}

func TestNormalisers(t *testing.T) {
	near(t, "NormalizeDeg(-1)", NormalizeDeg(-1), 359, 1e-12)
	near(t, "NormalizeDeg(360)", NormalizeDeg(360), 0, 1e-12)
	near(t, "NormalizeDeg(725)", NormalizeDeg(725), 5, 1e-12)
	if v := NormalizeDeg(-1e-17); v < 0 || v >= 360 {
		t.Errorf("NormalizeDeg(-1e-17) = %v out of [0,360)", v)
	}
	near(t, "NormalizeLon(180)", NormalizeLon(180), -180, 1e-12)
	near(t, "NormalizeLon(190)", NormalizeLon(190), -170, 1e-12)
	near(t, "NormalizeLon(-190)", NormalizeLon(-190), 170, 1e-12)
	near(t, "NormalizeLon(-180)", NormalizeLon(-180), -180, 1e-12)
	near(t, "AngleDiff(10,350)", AngleDiff(10, 350), 20, 1e-12)
	near(t, "AngleDiff(350,10)", AngleDiff(350, 10), -20, 1e-12)
	near(t, "AngleDiff(180,0)", AngleDiff(180, 0), 180, 1e-12)
	if ValidLatLon(math.NaN(), 0) || ValidLatLon(91, 0) || ValidLatLon(0, 181) || !ValidLatLon(-90, 180) {
		t.Error("ValidLatLon wrong")
	}
}

func TestGreatCircle(t *testing.T) {
	// London -> Paris, the usual textbook pair: ~343.5 km, bearing ~148°.
	d := HaversineKm(51.5074, -0.1278, 48.8566, 2.3522)
	near(t, "haversine London-Paris", d, 343.5, 0.6)
	near(t, "bearing London-Paris", InitialBearing(51.5074, -0.1278, 48.8566, 2.3522), 148.1, 0.3)
	near(t, "fast London-Paris", FastDistanceKm(51.5074, -0.1278, 48.8566, 2.3522), d, 1.0)

	// One degree of longitude on the equator on the mean sphere.
	near(t, "equator 1°", HaversineKm(0, 0, 0, 1), EarthRadiusKm*math.Pi/180, 1e-9)
	near(t, "bearing east", InitialBearing(0, 0, 0, 1), 90, 1e-9)
	near(t, "bearing north", InitialBearing(10, 20, 11, 20), 0, 1e-9)
	near(t, "bearing across antimeridian", InitialBearing(0, 179.5, 0, -179.5), 90, 1e-9)
	near(t, "final bearing", FinalBearing(0, 0, 0, 1), 90, 1e-9)

	// Destination is the inverse of distance + bearing.
	lat, lon := Destination(51.5, -0.1, 73, 250)
	near(t, "dest dist", HaversineKm(51.5, -0.1, lat, lon), 250, 1e-6)
	near(t, "dest brg", InitialBearing(51.5, -0.1, lat, lon), 73, 1e-6)
	lat, lon = Destination(0, 179.9, 90, 100)
	if lon > -179 || lon < -180 {
		t.Errorf("Destination across antimeridian lon = %v", lon)
	}
	near(t, "dest lat equator", lat, 0, 1e-9)

	mlat, mlon := Midpoint(0, 0, 0, 90)
	near(t, "midpoint lat", mlat, 0, 1e-9)
	near(t, "midpoint lon", mlon, 45, 1e-9)
	mlat, mlon = Midpoint(51.5074, -0.1278, 48.8566, 2.3522)
	near(t, "midpoint equidistant",
		HaversineKm(51.5074, -0.1278, mlat, mlon), HaversineKm(48.8566, 2.3522, mlat, mlon), 1e-6)

	// Cross-track: path along the equator heading east; a point 1° north is
	// to the LEFT (negative), 1° south is to the right (positive).
	oneDeg := EarthRadiusKm * math.Pi / 180
	near(t, "xtrack left", CrossTrackKm(1, 5, 0, 0, 0, 10), -oneDeg, 1e-6)
	near(t, "xtrack right", CrossTrackKm(-1, 5, 0, 0, 0, 10), oneDeg, 1e-6)
	near(t, "along track", AlongTrackKm(1, 5, 0, 0, 0, 10), 5*oneDeg, 1e-6)
	near(t, "along track behind", AlongTrackKm(1, -5, 0, 0, 0, 10), -5*oneDeg, 1e-6)
}

func dms(d, m int, s float64) float64 { return float64(d) + float64(m)/60 + s/3600 }

func TestVincentyReference(t *testing.T) {
	// Vincenty / Geoscience Australia reference line Flinders Peak -> Buninyong
	// (GDA94 technical manual): 54972.271 m, 306°52'05.37", reverse 127°10'25.07".
	lat1, lon1 := -dms(37, 57, 3.72030), dms(144, 25, 29.52440)
	lat2, lon2 := -dms(37, 39, 10.15610), dms(143, 55, 35.38390)
	d, a1, a2, ok := VincentyInverse(lat1, lon1, lat2, lon2)
	if !ok {
		t.Fatal("no convergence")
	}
	near(t, "Flinders-Buninyong dist", d, 54972.271, 0.005)
	near(t, "Flinders-Buninyong az1", a1, dms(306, 52, 5.37), 1e-5)
	near(t, "Flinders-Buninyong az2", a2, dms(127, 10, 25.07)+180, 1e-5)

	// Independent check against Karney's GeographicLib (GeodSolve man page,
	// EXAMPLES): JFK → Singapore Changi,
	//   echo 40:38:23N 073:46:44W 01:21:33N 103:59:22E | GeodSolve -i -: -p 0
	//   003:18:29.9 177:29:09.2 15347628
	d, a1, a2, ok = VincentyInverse(dms(40, 38, 23), -dms(73, 46, 44), dms(1, 21, 33), dms(103, 59, 22))
	if !ok {
		t.Fatal("JFK-SIN: no convergence")
	}
	near(t, "JFK-SIN dist", d, 15347628, 0.5)
	near(t, "JFK-SIN az1", a1, dms(3, 18, 29.9), 0.05/3600)
	near(t, "JFK-SIN az2", a2, dms(177, 29, 9.2), 0.05/3600)
	// The spherical formulas are within their stated ~0.5 % of that.
	near(t, "JFK-SIN haversine", HaversineKm(dms(40, 38, 23), -dms(73, 46, 44), dms(1, 21, 33), dms(103, 59, 22)), 15347.628, 0.005*15347.628)

	// One degree along the equator is a·π/180.
	d, a1, _, ok = VincentyInverse(0, 0, 0, 1)
	near(t, "equator 1°", d, WGS84A*math.Pi/180, 1e-6)
	near(t, "equator az", a1, 90, 1e-9)
	if !ok {
		t.Error("equator: no convergence")
	}
	// Meridian quadrant of WGS84: 10 001 965.729 m.
	d, _, _, _ = VincentyInverse(0, 0, 90, 0)
	near(t, "meridian quadrant", d, 10001965.729, 0.01)

	// Nearly antipodal case that still converges (Wikipedia, "Nearly antipodal points").
	d, _, _, ok = VincentyInverse(0, 0, 0.5, 179.5)
	if !ok {
		t.Error("(0,0)-(0.5,179.5) should converge")
	}
	near(t, "near-antipodal", d, 19936288.579, 0.01)

	// Non-converging antipodal case reports ok=false with a sane estimate.
	d, _, _, ok = VincentyInverse(0, 0, 0.5, 179.7)
	if ok {
		// Convergence here would be fine too, but then it must be right.
		near(t, "antipodal converged", d, 19944127.421, 1)
	} else if d < 19.8e6 || d > 20.1e6 {
		t.Errorf("antipodal fallback distance %v implausible", d)
	}

	d, a1, a2, ok = VincentyInverse(51.5, -0.1, 51.5, -0.1)
	if d != 0 || a1 != 0 || a2 != 0 || !ok {
		t.Errorf("coincident = %v %v %v %v", d, a1, a2, ok)
	}
}

func TestVincentyDirectRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		lat := rng.Float64()*160 - 80
		lon := rng.Float64()*360 - 180
		brg := rng.Float64() * 360
		dist := rng.Float64() * 2e6
		lat2, lon2, fin := VincentyDirect(lat, lon, brg, dist)
		d, a1, a2, ok := VincentyInverse(lat, lon, lat2, lon2)
		if !ok {
			t.Fatalf("no convergence for %v %v %v %v", lat, lon, brg, dist)
		}
		if math.Abs(d-dist) > 1e-3 {
			t.Fatalf("round trip dist %v vs %v", d, dist)
		}
		if dist > 1000 {
			if math.Abs(AngleDiff(a1, brg)) > 1e-6 {
				t.Fatalf("round trip bearing %v vs %v (dist %v)", a1, brg, dist)
			}
			if math.Abs(AngleDiff(a2, fin)) > 1e-6 {
				t.Fatalf("final bearing %v vs %v", a2, fin)
			}
		}
	}
	// Flinders Peak direct.
	lat2, lon2, _ := VincentyDirect(-dms(37, 57, 3.72030), dms(144, 25, 29.52440), dms(306, 52, 5.37), 54972.271)
	near(t, "direct lat", lat2, -dms(37, 39, 10.15610), 2e-7)
	near(t, "direct lon", lon2, dms(143, 55, 35.38390), 2e-7)
}

func TestLambertAgainstVincenty(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	var maxErrM, maxBrgErr float64
	for i := 0; i < 20000; i++ {
		lat := rng.Float64()*170 - 85
		lon := rng.Float64()*360 - 180
		st := NewStation(lat, lon, 0, 0)
		lat2, lon2, _ := VincentyDirect(lat, lon, rng.Float64()*360, 1000+rng.Float64()*800e3)
		want, a1, _, ok := VincentyInverse(lat, lon, lat2, lon2)
		if !ok {
			t.Fatal("vincenty failed")
		}
		km, brg := st.DistanceBearing(lat2, lon2)
		if e := math.Abs(km*1000 - want); e > maxErrM {
			maxErrM = e
		}
		if e := math.Abs(AngleDiff(brg, a1)); e > maxBrgErr {
			maxBrgErr = e
		}
		if brg < 0 || brg >= 360 {
			t.Fatalf("bearing %v out of range", brg)
		}
		km2, brg2 := DistanceBearing(lat, lon, lat2, lon2)
		if math.Abs(km2-km) > 1e-9 || math.Abs(AngleDiff(brg2, brg)) > 1e-9 {
			t.Fatalf("Station and free function disagree: %v/%v vs %v/%v", km, brg, km2, brg2)
		}
		if d := DistanceKm(lat, lon, lat2, lon2); math.Abs(d-km) > 1e-9 {
			t.Fatalf("DistanceKm %v vs %v", d, km)
		}
	}
	t.Logf("Lambert vs Vincenty within 800 km: max distance error %.2f m, max bearing error %.4f°", maxErrM, maxBrgErr)
	if maxErrM > 10 {
		t.Errorf("Lambert distance error %.2f m too large", maxErrM)
	}
	if maxBrgErr > 0.05 {
		t.Errorf("bearing error %.4f° too large", maxBrgErr)
	}
	if km, brg := NewStation(51.5, -0.1, 0, 0).DistanceBearing(51.5, -0.1); km != 0 || brg != 0 {
		t.Errorf("coincident: %v %v", km, brg)
	}
}

func TestStationGeometry(t *testing.T) {
	// Straight overhead.
	near(t, "overhead elev", ElevationDeg(0, 10, 10000, DefaultK), 90, 1e-9)
	near(t, "overhead slant", SlantRangeKm(0, 0, 10000), 10, 1e-9)
	// Chord between two sea-level points 100 km apart: 2R·sin(d/2R).
	near(t, "chord", SlantRangeKm(100, 0, 0), 2*EarthRadiusKm*math.Sin(100/(2*EarthRadiusKm)), 1e-9)
	// Close in the earth is flat: 10 km away, 10 km up => 45°.
	near(t, "45°", ElevationDeg(10, 0, 10000, DefaultK), 45, 0.06)

	// 4/3-earth horizon: d_km ≈ 4.12·sqrt(h_m).
	near(t, "horizon 10 m", HorizonKm(10, DefaultK), 4.12*math.Sqrt(10), 0.05)
	near(t, "horizon 10000 m", HorizonKm(10000, DefaultK), 4.12*math.Sqrt(10000), 1.0)
	near(t, "horizon k=1", HorizonKm(100, 1), 3.57*math.Sqrt(100), 0.05)
	if HorizonKm(-5, DefaultK) != 0 {
		t.Error("negative height horizon should be 0")
	}

	// A target on the surface at the antenna's horizon distance sits exactly
	// at the dip angle −acos(Re/(Re+h)).
	re := DefaultK * EarthRadiusKm
	ant := 50.0
	dip := -math.Acos(re/(re+ant/1000)) * 180 / math.Pi
	near(t, "dip", ElevationDeg(HorizonKm(ant, DefaultK), ant, 0, DefaultK), dip, 1e-6)

	// Minimum visible altitude: zero inside the horizon, then the altitude
	// whose own horizon closes the gap; such a target appears at the dip angle.
	if MinVisibleAltitudeM(HorizonKm(ant, DefaultK)-0.1, ant, DefaultK) != 0 {
		t.Error("inside horizon should be 0")
	}
	for _, d := range []float64{50, 150, 300, 450} {
		h := MinVisibleAltitudeM(d, ant, DefaultK)
		near(t, "horizon sum", HorizonKm(ant, DefaultK)+HorizonKm(h, DefaultK), d, 1e-6)
		near(t, "at dip", ElevationDeg(d, ant, h, DefaultK), dip, 1e-6)
	}
	// Sea-level antenna, 412 km: about 10 km (d²/2Re = 9.99 km).
	near(t, "min alt 412 km", MinVisibleAltitudeM(412, 0, DefaultK), 10001, 5)
	// A larger k (super-refraction) lowers the floor; k = 1 raises it.
	if !(MinVisibleAltitudeM(300, 10, 2) < MinVisibleAltitudeM(300, 10, DefaultK) &&
		MinVisibleAltitudeM(300, 10, DefaultK) < MinVisibleAltitudeM(300, 10, 1)) {
		t.Error("min visible altitude should fall with k")
	}
	// k sanitising.
	near(t, "k=0 default", EffectiveK(0), DefaultK, 0)
	near(t, "k NaN default", EffectiveK(math.NaN()), DefaultK, 0)
	if EffectiveK(-3) != EffectiveK(math.Inf(1)) || EffectiveK(-3) < 100 {
		t.Error("negative/infinite k should be flat earth")
	}

	// AltitudeAtElevationM inverts ElevationDeg.
	for _, e := range []float64{-0.1, 0, 0.5, 3, 20, 60} {
		for _, d := range []float64{5, 80, 250, 400} {
			alt, ok := AltitudeAtElevationM(d, ant, e, DefaultK)
			if !ok {
				continue
			}
			near(t, "elev inverse", ElevationDeg(d, ant, alt, DefaultK), e, 1e-6)
		}
	}
	if _, ok := AltitudeAtElevationM(200, 50, -1, DefaultK); ok {
		t.Error("ray pointing 1° down from 50 m must hit the ground before 200 km")
	}
	if alt, ok := AltitudeAtElevationM(300, 50, 0, DefaultK); !ok || alt < 5000 || alt > 5600 {
		t.Errorf("horizontal ray at 300 km: %v %v (expect ~5.3 km)", alt, ok)
	}

	st := NewStation(51.5074, -0.1278, 35, 0)
	l := st.LookFt(51.5074, -0.1278, 35000)
	near(t, "overhead ground", l.GroundKm, 0, 1e-9)
	near(t, "overhead elevation", l.ElevationDeg, 90, 1e-9)
	near(t, "overhead slant", l.SlantKm, 35000*MetresPerFoot/1000-0.035, 1e-9)
	l = st.Look(52.5, 1.0, math.NaN())
	if !math.IsNaN(l.SlantKm) || !math.IsNaN(l.ElevationDeg) || l.GroundKm < 100 {
		t.Errorf("NaN altitude look = %+v", l)
	}
	// Range ring points really are at the requested distance.
	rlat, rlon := st.Destination(123, 200)
	near(t, "ring distance", st.DistanceKm(rlat, rlon), 200, 0.005)
	// A jet at FL380, 300 km out, is a degree or so above the horizon.
	rlat, rlon = st.Destination(123, 300)
	l = st.LookFt(rlat, rlon, 38000)
	if l.ElevationDeg < 0.5 || l.ElevationDeg > 1.5 || l.SlantKm < 300 || l.SlantKm > 301 {
		t.Errorf("FL380 at 300 km: %+v", l)
	}
	near(t, "bearing", l.BearingDeg, 123, 0.005)
}

func TestGrid(t *testing.T) {
	cell := HeatCell(51.5074, -0.1278)
	if cell != 2830<<16|3597 {
		t.Errorf("HeatCell = %d, want %d", cell, 2830<<16|3597)
	}
	s, w, n, e := HeatCellBounds(cell)
	near(t, "south", s, 51.5, 1e-9)
	near(t, "west", w, -0.15, 1e-9)
	near(t, "north", n, 51.55, 1e-9)
	near(t, "east", e, -0.10, 1e-9)
	clat, clon := HeatCellCenter(cell)
	if HeatCell(clat, clon) != cell {
		t.Error("centre not in own cell")
	}
	li, lo := HeatCellIndices(cell)
	if li != 2830 || lo != 3597 || HeatCellFromIndices(li, lo) != cell {
		t.Error("indices round trip")
	}
	if HeatCellFromIndices(10, -1) != 10<<16|7199 || HeatCellFromIndices(-4, 7200) != 0 {
		t.Error("index wrap/clamp")
	}
	// Extremes stay inside 16 bits each.
	for _, p := range [][2]float64{{90, 180}, {-90, -180}, {90, 179.999}, {-90, 180}, {0, 540}} {
		c := HeatCell(p[0], p[1])
		a, b := HeatCellIndices(c)
		if a < 0 || a >= HeatLatCells || b < 0 || b >= HeatLonCells {
			t.Errorf("HeatCell(%v) indices %d,%d out of range", p, a, b)
		}
	}
	if HeatCell(10, 180) != HeatCell(10, -180) {
		t.Error("lon 180 and -180 should share a cell")
	}

	bearing := []struct {
		in   float64
		want int
	}{{0, 0}, {1.99, 0}, {2, 1}, {359.9, 179}, {360, 0}, {-1, 179}, {725, 2}}
	for _, c := range bearing {
		if got := BearingBin(c.in); got != c.want {
			t.Errorf("BearingBin(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	near(t, "bearing centre", BearingBinCenter(0), 1, 0)
	if SectorBin(359.9) != 35 || SectorBin(10) != 1 || SectorBin(-0.1) != 35 || SectorCenter(0) != 5 {
		t.Error("SectorBin")
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if BearingBin(bad) != -1 || SectorBin(bad) != -1 {
			t.Errorf("bearing bins of %v must be -1", bad)
		}
	}
	if HeatCell(math.NaN(), math.NaN()) != 0 || HeatCell(math.Inf(1), math.Inf(-1)) != uint32(HeatLatCells-1)<<16 {
		t.Errorf("HeatCell of non-finite input: %d %d", HeatCell(math.NaN(), math.NaN()), HeatCell(math.Inf(1), math.Inf(-1)))
	}

	alt := []struct {
		in   float64
		want int
	}{{-500, 0}, {0, 0}, {999, 0}, {1000, 1}, {4999, 1}, {5000, 2}, {9999, 2}, {10000, 3},
		{19999, 3}, {20000, 4}, {30000, 5}, {39999, 5}, {40000, 6}, {60000, 6}, {math.NaN(), -1}}
	for _, c := range alt {
		if got := AltBand(c.in); got != c.want {
			t.Errorf("AltBand(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	for b := 0; b < AltBands; b++ {
		lo, hi := AltBandBoundsFt(b)
		typ := AltBandTypicalFt(b)
		if !(typ >= lo && typ < hi) || AltBand(typ) != b || AltBandLabel(b) == "" {
			t.Errorf("band %d: bounds %v..%v typical %v", b, lo, hi, typ)
		}
	}

	if DistBin(125) != 12 || DistBin(-3) != 0 || DistBin(99999) != DistBins-1 || DistBin(math.NaN()) != -1 {
		t.Error("DistBin")
	}
	if RSSIBin(-23.4) != 23 || RSSIBin(-23) != 22 || RSSIBin(-0.2) != 0 || RSSIBin(0) != 0 || RSSIBin(3) != 0 ||
		RSSIBin(-200) != RSSIBins-1 || RSSIBin(math.NaN()) != -1 || RSSIBinStored(22) != -23 {
		t.Error("RSSIBin")
	}
	near(t, "rssi centre", RSSIBinCenter(23), -23.5, 0)
	if ElevBin(-2) != 0 || ElevBin(-5) != 0 || ElevBin(0) != 4 || ElevBin(0.49) != 4 || ElevBin(0.5) != 5 ||
		ElevBin(90) != ElevBins-1 || ElevBin(89.99) != 183 {
		t.Error("ElevBin")
	}
	near(t, "elev centre", ElevBinCenter(4), 0.25, 1e-12)
	if RangeBin(599.9) != 119 || RangeBin(4.9) != 0 || RangeBin(5) != 1 || RangeBin(1e6) != 119 {
		t.Error("RangeBin")
	}
}

func BenchmarkStationLook(b *testing.B) {
	st := NewStation(51.5074, -0.1278, 35, 0)
	b.ReportAllocs()
	var sink float64
	for i := 0; i < b.N; i++ {
		l := st.Look(52.3+float64(i&255)*0.001, 1.7, 11000)
		sink += l.ElevationDeg
	}
	_ = sink
}
