package modes

import (
	"math"
	"math/rand"
	"testing"
)

// nlFormula is the defining equation of the longitude zone count
// (The 1090 MHz Riddle, eq. for NL(lat); DO-260B A.1.7.2 d).
func nlFormula(lat float64) int {
	if lat == 0 {
		return 59
	}
	if math.Abs(lat) == 87 {
		return 2
	}
	if math.Abs(lat) > 87 {
		return 1
	}
	const nz = 15
	a := 1 - math.Cos(math.Pi/(2*nz))
	b := math.Cos(math.Pi / 180 * lat)
	return int(math.Floor(2 * math.Pi / math.Acos(1-a/(b*b))))
}

func TestNL(t *testing.T) {
	// Transition latitudes as tabulated with the CPR definition (DO-260B):
	// NL(lat) = n for lat below limit[n] (and at or above limit[n+1]).
	limits := map[int]float64{
		59: 10.47047130, 58: 14.82817437, 57: 18.18626357, 56: 21.02939493, 55: 23.54504487,
		54: 25.82924707, 53: 27.93898710, 52: 29.91135686, 51: 31.77209708, 50: 33.53993436,
		49: 35.22899598, 48: 36.85025108, 47: 38.41241892, 46: 39.92256684, 45: 41.38651832,
		44: 42.80914012, 43: 44.19454951, 42: 45.54626723, 41: 46.86733252, 40: 48.16039128,
		39: 49.42776439, 38: 50.67150166, 37: 51.89342469, 36: 53.09516153, 35: 54.27817472,
		34: 55.44378444, 33: 56.59318756, 32: 57.72747354, 31: 58.84763776, 30: 59.95459277,
		29: 61.04917774, 28: 62.13216659, 27: 63.20427479, 26: 64.26616523, 25: 65.31845310,
		24: 66.36171008, 23: 67.39646774, 22: 68.42322022, 21: 69.44242631, 20: 70.45451075,
		19: 71.45986473, 18: 72.45884545, 17: 73.45177442, 16: 74.43893416, 15: 75.42056257,
		14: 76.39684391, 13: 77.36789461, 12: 78.33374083, 11: 79.29428225, 10: 80.24923213,
		9: 81.19801349, 8: 82.13956981, 7: 83.07199445, 6: 83.99173563, 5: 84.89166191,
		4: 85.75541621, 3: 86.53536998, 2: 87.00000000,
	}
	if len(limits) != 58 {
		t.Fatalf("table has %d rows", len(limits))
	}
	for n, lim := range limits {
		const eps = 1e-7
		for _, sign := range []float64{1, -1} {
			if got := NL(sign * (lim - eps)); got != n {
				t.Errorf("NL(%v) = %d, want %d", sign*(lim-eps), got, n)
			}
			if got := NL(sign * (lim + eps)); got != n-1 {
				t.Errorf("NL(%v) = %d, want %d", sign*(lim+eps), got, n-1)
			}
		}
	}
	for _, c := range []struct {
		lat  float64
		want int
	}{
		{0, 59}, {87, 2}, {-87, 2}, {86.9999999, 2}, {-86.9999999, 2}, {87.0000001, 1}, {87.5, 1}, {90, 1}, {-90, 1},
		{52.2572, 36}, {52.26578, 36}, {-43.4856, 43}, {math.NaN(), 1}, {math.Inf(1), 1},
	} {
		if got := NL(c.lat); got != c.want {
			t.Errorf("NL(%v) = %d, want %d", c.lat, got, c.want)
		}
	}
	// Against the closed formula away from the transition latitudes, where
	// floating point rounding of the formula itself could go either way.
	rng := rand.New(rand.NewSource(4))
sample:
	for i := 0; i < 20000; i++ {
		lat := rng.Float64()*180 - 90
		for _, lim := range limits {
			if math.Abs(math.Abs(lat)-lim) < 1e-6 {
				continue sample
			}
		}
		if got, want := NL(lat), nlFormula(lat); got != want {
			t.Fatalf("NL(%v) = %d, formula gives %d", lat, got, want)
		}
	}
}

func TestCPRAirborneGlobalKnownAnswers(t *testing.T) {
	// The 1090 MHz Riddle, airborne position chapter:
	// 8D40621D58C382D690C8AC2863A7 (even, most recent) and
	// 8D40621D58C386435CC412692AD6 (odd) = 52.2572021484375, 3.91937255859375.
	even := mustDecode(t, bookPosEven)
	odd := mustDecode(t, bookPosOdd)
	lat, lon, ok := CPRAirborneGlobal(even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, false)
	if !ok || !near(lat, 52.25720214843750, 1e-12) || !near(lon, 3.91937255859375, 1e-12) {
		t.Fatalf("book pair (even latest) = %.14f, %.14f, %v", lat, lon, ok)
	}
	// With the odd frame as the most recent one the odd latitude of the book
	// is returned.
	lat, lon, ok = CPRAirborneGlobal(even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, true)
	if !ok || !near(lat, 52.26578017412606, 1e-12) || !near(lon, 3.9389, 1e-3) {
		t.Fatalf("book pair (odd latest) = %.14f, %.14f, %v", lat, lon, ok)
	}

	// Self-generated DF17 pair: even at 48.1102 N 16.5697 E, odd 5 s later
	// at 48.1115 N 16.5745 E, 39000 ft. The result is the encoded position
	// to within the CPR quantisation.
	even = mustDecode(t, esHex(0x8D, 0x440ABC, posME(11, 39000, false, 48.1102, 16.5697)))
	odd = mustDecode(t, esHex(0x8D, 0x440ABC, posME(11, 39000, true, 48.1115, 16.5745)))
	if even.CPROdd || !odd.CPROdd || even.AltBaro != 39000 {
		t.Fatal("frame parity")
	}
	lat, lon, ok = CPRAirborneGlobal(even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, true)
	if !ok || !near(lat, 48.1115, 3e-5) || !near(lon, 16.5745, 5e-5) {
		t.Fatalf("own pair (odd latest) = %.6f, %.6f, %v", lat, lon, ok)
	}
	lat, lon, ok = CPRAirborneGlobal(even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, false)
	if !ok || !near(lat, 48.1102, 3e-5) || !near(lon, 16.5697, 5e-5) {
		t.Fatalf("own pair (even latest) = %.6f, %.6f, %v", lat, lon, ok)
	}
}
func TestCPRAirborneLocalKnownAnswers(t *testing.T) {
	// The 1090 MHz Riddle: reference 52.258, 3.918.
	m := mustDecode(t, bookPosEven)
	lat, lon, ok := CPRAirborneLocal(52.258, 3.918, m.CPRLat, m.CPRLon, m.CPROdd)
	if !ok || !near(lat, 52.2572021484375, 1e-12) || !near(lon, 3.91937255859375, 1e-12) {
		t.Fatalf("book = %.14f, %.14f, %v", lat, lon, ok)
	}
	// Self-generated frames, reference 48.0, 16.0 (about 45 km away).
	m = mustDecode(t, esHex(0x8D, 0x440ABC, posME(11, 39000, false, 48.1102, 16.5697)))
	lat, lon, ok = CPRAirborneLocal(48.0, 16.0, m.CPRLat, m.CPRLon, m.CPROdd)
	if !ok || !near(lat, 48.1102, 3e-5) || !near(lon, 16.5697, 5e-5) {
		t.Fatalf("own even = %.6f, %.6f, %v", lat, lon, ok)
	}
	m = mustDecode(t, esHex(0x8D, 0x440ABC, posME(11, 39000, true, 48.1115, 16.5745)))
	lat, lon, ok = CPRAirborneLocal(48.0, 16.0, m.CPRLat, m.CPRLon, m.CPROdd)
	if !ok || !near(lat, 48.1115, 3e-5) || !near(lon, 16.5745, 5e-5) {
		t.Fatalf("own odd = %.6f, %.6f, %v", lat, lon, ok)
	}
}
func TestCPRSurfaceKnownAnswers(t *testing.T) {
	// The 1090 MHz Riddle, surface position chapter: reference 51.990, 4.375;
	// the odd frame is the most recent.
	even := mustDecode(t, bookSurfEven)
	odd := mustDecode(t, bookSurfOdd)
	lat, lon, ok := CPRSurfaceGlobal(51.990, 4.375, even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, true)
	if !ok || !near(lat, 52.320607072215964, 1e-12) || !near(lon, 4.734734671456474, 1e-12) {
		t.Fatalf("book global = %.15f, %.15f, %v", lat, lon, ok)
	}
	lat, lon, ok = CPRSurfaceGlobal(51.990, 4.375, even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, false)
	if !ok || !near(lat, 52.323040008544920, 1e-12) || !near(lon, 4.73, 0.01) {
		t.Fatalf("book global (even latest) = %.15f, %.15f, %v", lat, lon, ok)
	}
	// Same pair seen from the other three quadrants / the southern solution.
	for _, c := range []struct{ refLat, refLon, lat, lon float64 }{
		{52, 94, 52.320607072215964, 94.734734671456474},
		{52, 179, 52.320607072215964, -175.265265328543526},
		{52, -80, 52.320607072215964, -85.265265328543526},
		{-38, 4, -37.679392927784036, 4.734734671456474},
	} {
		lat, lon, ok := CPRSurfaceGlobal(c.refLat, c.refLon, even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, true)
		if !ok || !near(lat, c.lat, 1e-9) || (c.refLat > 0 && !near(lon, c.lon, 1e-9)) {
			t.Errorf("ref %v,%v = %.9f, %.9f, %v; want %.9f, %.9f", c.refLat, c.refLon, lat, lon, ok, c.lat, c.lon)
		}
	}

	// Locally unambiguous, same chapter: 52.320561, 4.735735.
	m := mustDecode(t, bookSurf)
	for _, ref := range [][2]float64{{51.990, 4.375}, {52.320607, 4.734735}} {
		lat, lon, ok = CPRSurfaceLocal(ref[0], ref[1], m.CPRLat, m.CPRLon, m.CPROdd)
		if !ok || !near(lat, 52.32056051997815, 1e-12) || !near(lon, 4.735735212053571, 1e-12) {
			t.Fatalf("book local (ref %v) = %.15f, %.15f, %v", ref, lat, lon, ok)
		}
	}

	// Self-generated surface pairs: southern hemisphere close to the date
	// line (DF17 CA 4 even, CA 7 odd), and the south-west quadrant (DF18).
	for _, c := range []struct {
		first    [2]byte
		lat, lon float64
		ref      [2]float64
	}{
		{[2]byte{0x8C, 0x8F}, -43.48564, 172.53942, [2]float64{-43.496, 172.558}},
		{[2]byte{0x90, 0x90}, -23.43210, -46.47890, [2]float64{-23.4265448, -46.4816258}},
	} {
		even = mustDecode(t, esHex(c.first[0], 0xC8F00A, surfME(7, 43, true, 15, false, c.lat, c.lon)))
		odd = mustDecode(t, esHex(c.first[1], 0xC8F00A, surfME(7, 43, true, 15, true, c.lat, c.lon)))
		if even.CPROdd || !odd.CPROdd {
			t.Fatal("frame parity")
		}
		lat, lon, ok = CPRSurfaceGlobal(c.ref[0], c.ref[1], even.CPRLat, even.CPRLon, odd.CPRLat, odd.CPRLon, true)
		if !ok || !near(lat, c.lat, 1.5e-5) || !near(lon, c.lon, 3e-5) {
			t.Fatalf("surface global %v = %.6f, %.6f, %v", c, lat, lon, ok)
		}
		lat, lon, ok = CPRSurfaceLocal(c.ref[0]-0.004, c.ref[1]-0.058, odd.CPRLat, odd.CPRLon, true)
		if !ok || !near(lat, c.lat, 1.5e-5) || !near(lon, c.lon, 3e-5) {
			t.Fatalf("surface local %v = %.6f, %.6f, %v", c, lat, lon, ok)
		}
	}
}

// cprTolerance returns the worst case quantisation error of an encoded
// position, in degrees of latitude and longitude.
func cprTolerance(lat float64, surface bool) (dLat, dLon float64) {
	span := 360.0
	bits := cprScale
	if surface {
		bits = 524288
	}
	nl := NL(lat) - 1
	if nl < 1 {
		nl = 1
	}
	return span / 59 / bits * 0.51, span / float64(nl) / bits * 0.51
}

// zoneWidth returns the longitude zone width in degrees of an even or odd
// frame at a latitude.
func zoneWidth(lat float64, odd bool, span float64) float64 {
	ni := NL(lat)
	if odd {
		ni--
	}
	if ni < 1 {
		ni = 1
	}
	return span / float64(ni)
}

func nearNLBoundary(lat float64) bool {
	a := math.Abs(lat)
	for n := 2; n <= 59; n++ {
		if math.Abs(a-nlLimit[n]) < 2e-4 {
			return true
		}
	}
	return false
}

func TestCPRRoundTripAirborne(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	check := func(lat, lon float64) {
		if nearNLBoundary(lat) {
			return
		}
		eLat, eLon := CPREncode(lat, lon, false, false)
		oLat, oLon := CPREncode(lat, lon, true, false)
		tolLat, tolLon := cprTolerance(lat, false)
		for _, latestOdd := range []bool{false, true} {
			gLat, gLon, ok := CPRAirborneGlobal(eLat, eLon, oLat, oLon, latestOdd)
			if !ok || !near(gLat, lat, tolLat) || lonDiff(gLon, lon) > tolLon {
				t.Fatalf("global %v,%v (latestOdd=%v) = %v, %v, %v", lat, lon, latestOdd, gLat, gLon, ok)
			}
			if gLon < -180 || gLon >= 180 {
				t.Fatalf("longitude %v out of [-180,180)", gLon)
			}
		}
		// Local decoding from a reference up to ~150 NM away.
		for _, odd := range []bool{false, true} {
			cLat, cLon := eLat, eLon
			if odd {
				cLat, cLon = oLat, oLon
			}
			refLat := lat + (rng.Float64()*2-1)*2.4
			if refLat > 90 {
				refLat = 90
			}
			if refLat < -90 {
				refLat = -90
			}
			refLon := lon + (rng.Float64()*2-1)*zoneWidth(lat, odd, 360)*0.45
			lLat, lLon, ok := CPRAirborneLocal(refLat, refLon, cLat, cLon, odd)
			if !ok || !near(lLat, lat, tolLat) || lonDiff(lLon, lon) > tolLon {
				t.Fatalf("local %v,%v (odd=%v, ref %v,%v) = %v, %v, %v", lat, lon, odd, refLat, refLon, lLat, lLon, ok)
			}
		}
	}
	for i := 0; i < 20000; i++ {
		check(rng.Float64()*179-89.5, rng.Float64()*360-180)
	}
	// Corner cases: equator, Greenwich, date line, both hemispheres, poles.
	for _, lat := range []float64{0, 1e-6, -1e-6, 5.99, 6.01, -6.01, 45, -45, 86.9, -86.9, 87.5, -87.5, 89.9, -89.9} {
		for _, lon := range []float64{0, 1e-6, -1e-6, 90, -90, 179.999, -179.999, -180, 123.456, -123.456} {
			check(lat, lon)
		}
	}
}

func TestCPRRoundTripSurface(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for i := 0; i < 20000; i++ {
		lat := rng.Float64()*170 - 85
		lon := rng.Float64()*360 - 180
		if nearNLBoundary(lat) {
			continue
		}
		eLat, eLon := CPREncode(lat, lon, false, true)
		oLat, oLon := CPREncode(lat, lon, true, true)
		tolLat, tolLon := cprTolerance(lat, true)
		// The receiver is somewhere within a few hundred kilometres.
		refLat := math.Max(-90, math.Min(90, lat+(rng.Float64()*2-1)*3))
		refLon := lon + (rng.Float64()*2-1)*3
		for _, latestOdd := range []bool{false, true} {
			gLat, gLon, ok := CPRSurfaceGlobal(refLat, refLon, eLat, eLon, oLat, oLon, latestOdd)
			if !ok || !near(gLat, lat, tolLat) || lonDiff(gLon, lon) > tolLon {
				t.Fatalf("global %v,%v (latestOdd=%v, ref %v,%v) = %v, %v, %v", lat, lon, latestOdd, refLat, refLon, gLat, gLon, ok)
			}
		}
		// Local: the reference must be within half a zone (45 NM).
		for _, odd := range []bool{false, true} {
			cLat, cLon := eLat, eLon
			if odd {
				cLat, cLon = oLat, oLon
			}
			rLat := lat + (rng.Float64()*2-1)*0.6
			rLon := lon + (rng.Float64()*2-1)*zoneWidth(lat, odd, 90)*0.45
			lLat, lLon, ok := CPRSurfaceLocal(rLat, rLon, cLat, cLon, odd)
			if !ok || !near(lLat, lat, tolLat) || lonDiff(lLon, lon) > tolLon {
				t.Fatalf("local %v,%v (odd=%v, ref %v,%v) = %v, %v, %v", lat, lon, odd, rLat, rLon, lLat, lLon, ok)
			}
		}
	}
}

func TestCPRInconsistentPairs(t *testing.T) {
	// The aircraft crossed a longitude zone boundary between the two frames:
	// NL(even) != NL(odd), so the pair must be refused.
	const boundary = 10.47047130
	eLat, eLon := CPREncode(boundary-0.0005, 20, false, false) // NL 59
	oLat, oLon := CPREncode(boundary+0.0005, 20, true, false)  // NL 58
	for _, latestOdd := range []bool{false, true} {
		if lat, lon, ok := CPRAirborneGlobal(eLat, eLon, oLat, oLon, latestOdd); ok {
			t.Errorf("zone crossing accepted: %v, %v", lat, lon)
		}
	}
	// The same on the ground (surface zones use the same NL table).
	eLat, eLon = CPREncode(boundary-0.0005, 20, false, true)
	oLat, oLon = CPREncode(boundary+0.0005, 20, true, true)
	if lat, lon, ok := CPRSurfaceGlobal(10.4, 20.1, eLat, eLon, oLat, oLon, true); ok {
		t.Errorf("surface zone crossing accepted: %v, %v", lat, lon)
	}
	// Both sides of the boundary are fine on their own.
	for _, lat := range []float64{boundary - 0.01, boundary + 0.01} {
		eLat, eLon = CPREncode(lat, 20, false, false)
		oLat, oLon = CPREncode(lat, 20, true, false)
		if got, _, ok := CPRAirborneGlobal(eLat, eLon, oLat, oLon, true); !ok || !near(got, lat, 1e-4) {
			t.Errorf("lat %v: %v, %v", lat, got, ok)
		}
	}

	// Frames of two aircraft far apart: whatever comes out must either be
	// refused or be a legal coordinate - never NaN or out of range.
	rng := rand.New(rand.NewSource(7))
	refused := 0
	for i := 0; i < 20000; i++ {
		a, b, c, d := rng.Uint32()&0x1FFFF, rng.Uint32()&0x1FFFF, rng.Uint32()&0x1FFFF, rng.Uint32()&0x1FFFF
		lat, lon, ok := CPRAirborneGlobal(a, b, c, d, i&1 == 0)
		if !ok {
			refused++
			continue
		}
		if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon >= 180 {
			t.Fatalf("random pair gave %v, %v", lat, lon)
		}
	}
	if refused == 0 {
		t.Error("no random pair was refused: latitude range / NL checks are not effective")
	}
}

func TestCPRLocalRejectsBadReference(t *testing.T) {
	m := mustDecode(t, bookPosEven)
	for _, ref := range [][2]float64{
		{math.NaN(), 3.9}, {52.2, math.NaN()}, {91, 3.9}, {-91, 3.9}, {52.2, math.Inf(1)},
	} {
		if lat, lon, ok := CPRAirborneLocal(ref[0], ref[1], m.CPRLat, m.CPRLon, m.CPROdd); ok {
			t.Errorf("airborne ref %v accepted: %v, %v", ref, lat, lon)
		}
		if lat, lon, ok := CPRSurfaceLocal(ref[0], ref[1], m.CPRLat, m.CPRLon, m.CPROdd); ok {
			t.Errorf("surface ref %v accepted: %v, %v", ref, lat, lon)
		}
	}
	// A reference in the wrong part of the world still yields a legal (but
	// wrong) position near that reference: range checking is the caller's job.
	lat, lon, ok := CPRAirborneLocal(-33.9, 151.2, m.CPRLat, m.CPRLon, m.CPROdd)
	if !ok || math.Abs(lat+33.9) > 3 || lonDiff(lon, 151.2) > zoneWidth(lat, false, 360)/2+1e-9 {
		t.Errorf("far reference: %v, %v, %v", lat, lon, ok)
	}
	// Next to the pole the nearest solution lies beyond 90 degrees: refused.
	if lat, lon, ok := CPRAirborneLocal(89.99, 0, 1000, 0, false); ok {
		t.Errorf("latitude beyond the pole accepted: %v, %v", lat, lon)
	}
	if lat, lon, ok := CPRAirborneLocal(-89.99, 0, 130000, 0, false); ok {
		t.Errorf("latitude beyond the south pole accepted: %v, %v", lat, lon)
	}
}

func TestCPREncodeRange(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	for i := 0; i < 5000; i++ {
		lat := rng.Float64()*180 - 90
		lon := rng.Float64()*720 - 360 // any longitude representation
		for _, odd := range []bool{false, true} {
			for _, surface := range []bool{false, true} {
				a, b := CPREncode(lat, lon, odd, surface)
				if a > 0x1FFFF || b > 0x1FFFF {
					t.Fatalf("CPREncode(%v,%v) = %d,%d exceeds 17 bits", lat, lon, a, b)
				}
			}
		}
	}
	// The encoder reproduces the book's frames from their decoded position.
	lat, lon := 52.25720214843750, 3.91937255859375
	if a, b := CPREncode(lat, lon, false, false); a != 93000 || b != 51372 {
		t.Errorf("even encode = %d,%d, want 93000,51372", a, b)
	}
}
