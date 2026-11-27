package radio

import (
	"encoding/json"
	"math"
	"testing"

	"airview/internal/geo"
)

func TestRadioHorizon(t *testing.T) {
	// 4/3-earth rule of thumb: d_km = 4.12·sqrt(h_m); for the true earth
	// (k = 1) the optical horizon is 3.57·sqrt(h_m).
	for _, h := range []float64{2, 10, 30, 100, 1000, 10000, 12500} {
		near(t, "4.12·sqrt(h)", RadioHorizonKm(h, KStandard), 4.12*math.Sqrt(h), 0.003*4.12*math.Sqrt(h))
		near(t, "3.57·sqrt(h)", RadioHorizonKm(h, 1), 3.57*math.Sqrt(h), 0.003*3.57*math.Sqrt(h))
		near(t, "k = 0 means 4/3", RadioHorizonKm(h, 0), RadioHorizonKm(h, KStandard), 0)
		// Closed form sqrt(2·k·R·h) agrees with the exact arc to < 0.1 %.
		exact := RadioHorizonKm(h, KStandard)
		near(t, "approximation", RadioHorizonApproxKm(h, KStandard), exact, 0.001*exact)
	}
	if RadioHorizonKm(0, KStandard) != 0 || RadioHorizonKm(-5, KStandard) != 0 || RadioHorizonApproxKm(-5, 0) != 0 ||
		RadioHorizonKm(math.NaN(), 0) != 0 {
		t.Error("non-positive heights have no horizon")
	}

	// d = 4.12·(sqrt(h1) + sqrt(h2)): 10 m mast and an airliner at 10 km → 425 km.
	near(t, "line of sight", LineOfSightKm(10, 10000, KStandard), 4.12*(math.Sqrt(10)+math.Sqrt(10000)), 0.6)
	near(t, "line of sight ft", LineOfSightFtKm(10, 10000/0.3048, 0), LineOfSightKm(10, 10000, 0), 1e-9)
	// FL380 from a 15 m mast: 4.12·(3.873 + 107.62) = 459 km.
	near(t, "FL380", LineOfSightFtKm(15, 38000, 0), 459.4, 0.7)

	// MinAltitudeM is the inverse of LineOfSightKm.
	for _, alt := range []float64{300, 3000, 11000} {
		d := LineOfSightKm(25, alt, KStandard)
		near(t, "min altitude inverse", MinAltitudeM(d, 25, KStandard), alt, 1e-4*alt)
	}
	if MinAltitudeM(5, 25, 0) != 0 {
		t.Error("inside the antenna's own horizon the floor is 0")
	}
}

func TestHorizonTable(t *testing.T) {
	const ant = 20.0
	tab := HorizonTable(ant, 0)
	if len(tab) != geo.AltBands {
		t.Fatalf("len = %d", len(tab))
	}
	own := RadioHorizonKm(ant, 0)
	for i, r := range tab {
		if r.Band != i || r.Label != geo.AltBandLabel(i) || r.AltFt != geo.AltBandTypicalFt(i) {
			t.Errorf("row %d: %+v", i, r)
		}
		if !(r.LOSLoKm <= r.LOSKm && r.LOSKm <= r.LOSHiKm) {
			t.Errorf("row %d not ordered: %+v", i, r)
		}
		near(t, "row LOS", r.LOSKm, LineOfSightFtKm(ant, r.AltFt, 0), 1e-9)
		if i > 0 {
			near(t, "bands are contiguous", r.LOSLoKm, tab[i-1].LOSHiKm, 1e-9)
			if r.LOSKm <= tab[i-1].LOSKm {
				t.Errorf("row %d: range must grow with altitude", i)
			}
		}
	}
	near(t, "lowest edge = antenna horizon", tab[0].LOSLoKm, own, 1e-9)
	// 35 000 ft: 4.12·(sqrt(20) + sqrt(10668)) = 444 km.
	near(t, "30-40k band", tab[5].LOSKm, 4.12*(math.Sqrt(ant)+math.Sqrt(10668)), 0.7)
	near(t, "top edge = 50 000 ft", tab[6].LOSHiKm, LineOfSightFtKm(ant, 50000, 0), 1e-9)
	if _, err := json.Marshal(tab); err != nil {
		t.Errorf("json: %v", err)
	}
}

func TestHorizonShift(t *testing.T) {
	s := HorizonShiftForK(10, 10000, KStandard)
	near(t, "standard factor", s.Factor, 1, 1e-12)
	near(t, "standard delta", s.DeltaKm, 0, 1e-9)
	if s.Trapping {
		t.Error("standard atmosphere is not trapping")
	}
	s0 := HorizonShiftForK(10, 10000, 0)
	near(t, "k=0 selects the standard", s0.RangeKm, s.RangeKm, 0)

	// The horizon scales with sqrt(k): k = 1 (dN/dh = 0) loses 13.4 %,
	// dN/dh = −100 N/km (k = 2.75) gains 44 %.
	s = HorizonShiftForK(10, 10000, 1)
	near(t, "k=1 factor", s.Factor, math.Sqrt(0.75), 0.001)
	if s.DeltaKm >= 0 {
		t.Error("k = 1 must shorten the horizon")
	}
	k := KFactor(-100)
	s = HorizonShiftForK(10, 10000, k)
	near(t, "k=2.75 factor", s.Factor, math.Sqrt(k/KStandard), 0.002)
	near(t, "k=2.75 factor matches HorizonFactor", s.Factor, HorizonFactor(k), 0.002)
	near(t, "delta", s.DeltaKm, s.RangeKm-s.StandardKm, 0)
	if s.DeltaKm < 150 || s.Trapping {
		t.Errorf("super-refraction shift: %+v", s)
	}

	// Trapping: flagged, finite and JSON-encodable.
	for _, kk := range []float64{KMax, KFactor(-200), -2, math.Inf(1)} {
		s = HorizonShiftForK(10, 10000, kk)
		if !s.Trapping || math.IsInf(s.RangeKm, 0) || math.IsNaN(s.RangeKm) || s.RangeKm < 5*s.StandardKm {
			t.Errorf("k = %v: %+v", kk, s)
		}
		if _, err := json.Marshal(s); err != nil {
			t.Errorf("json: %v", err)
		}
	}
	// Both ends on the ground: no range, factor defined as 1.
	s = HorizonShiftForK(0, 0, 2)
	if s.RangeKm != 0 || s.Factor != 1 {
		t.Errorf("zero heights: %+v", s)
	}
}
