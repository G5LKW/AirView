package analytics

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"airview/internal/geo"
)

// guessTestCells builds a synthetic all-time heat grid around a station at
// (lat0, lon0): coverage out to ~horizon range with more traffic in one
// direction (airways), mean altitudes rising with distance, and signal
// levels that follow the inverse-square law only loosely: strong signals
// compress (AGC / saturation near -3 dBFS), weak ones sit on the detection
// floor (only the lucky, strong replies of distant aircraft are decoded), as
// on a real Beast feed.
func guessTestCells(seed int64, lat0, lon0 float64, compress bool) []heatCell {
	rng := rand.New(rand.NewSource(seed))
	var cells []heatCell
	seen := map[[2]int]bool{}
	for i := 0; i < 1800; i++ {
		brg := rng.Float64() * 360
		if rng.Float64() < 0.5 {
			brg = 290 + rng.NormFloat64()*25 // busy airway sector
		}
		d := 20 + 360*math.Sqrt(rng.Float64())
		la, lo := geo.Destination(lat0, lon0, brg, d)
		li, oi := int(math.Floor(la/geo.HeatCellDeg)), int(math.Floor(lo/geo.HeatCellDeg))
		if seen[[2]int{li, oi}] {
			continue
		}
		seen[[2]int{li, oi}] = true
		// lowest altitude seen ~ radio horizon from a 50 m antenna
		hor := d - math.Sqrt(2*effEarthKm*0.05)
		minFt := 1000.0
		if hor > 0 {
			minFt = math.Max(minFt, hor*hor/(2*effEarthKm)*1000/0.3048)
		}
		minFt *= 1 + 0.2*rng.Float64()
		maxFt := math.Max(minFt+2000, 38000)
		altKm := geo.FeetToMetres((minFt+maxFt)/2) / 1000
		slant := math.Sqrt(d*d + altKm*altKm)
		rssi := -5 - 20*math.Log10(slant/10) + rng.NormFloat64()*2
		if compress {
			// saturation and detection floor
			rssi = math.Max(-3-0.15*(-3-rssi), math.Min(rssi, -3))
			if rssi < -30 {
				rssi = -30 + 0.2*(rssi+30) + rng.NormFloat64()
			}
		}
		cells = append(cells, heatCell{latIdx: li, lonIdx: oi,
			lat: (float64(li) + 0.5) * geo.HeatCellDeg, lon: (float64(oi) + 0.5) * geo.HeatCellDeg,
			count: int64(5 + rng.Intn(200)), rssi: rssi, rssiN: int64(10 + rng.Intn(100)),
			minAlt: minFt, maxAlt: maxFt})
	}
	return cells
}

func guessEst(g *Guess, method string) *GuessEstimate {
	for i := range g.Estimates {
		if g.Estimates[i].Method == method {
			return &g.Estimates[i]
		}
	}
	return nil
}

// Field bug r101: the signal-strength fit landed 1650 km (893 nm) away from
// the station (63.8N 14.7W for a UK site). The fit is bounded to the
// coverage and penalised where heard aircraft would be beyond the radio
// horizon, so it can never run off to where nothing could be heard.
func TestGuessSignalFitStaysInCoverage(t *testing.T) {
	for _, st := range []struct{ lat, lon float64 }{{51.5, -1.0}, {54.0, -2.5}, {47.3, 8.5}} {
		for seed := int64(1); seed <= 3; seed++ {
			for _, compress := range []bool{false, true} {
				g := guessFromCells(guessTestCells(seed, st.lat, st.lon, compress))
				sig := guessEst(g, "signal")
				if sig == nil {
					t.Fatal("no signal estimate")
				}
				if !sig.OK {
					t.Fatalf("seed %d: signal fit failed: %s", seed, sig.Reason)
				}
				e := geo.HaversineKm(st.lat, st.lon, sig.Lat, sig.Lon)
				t.Logf("station %.1f,%.1f seed %d compress %v: signal fit %.1f km off (unc %.1f km)", st.lat, st.lon, seed, compress, e, float64(sig.UncertaintyKm))
				lim := 60.0
				if compress {
					lim = 250 // a flat level profile pins little down, but stays in coverage
				}
				if e > lim {
					t.Errorf("station %.1f,%.1f seed %d compress %v: signal fit %.0f km off", st.lat, st.lon, seed, compress, e)
				}
				if g.Fused == nil {
					t.Fatal("no fused estimate")
				}
				if fe := geo.HaversineKm(st.lat, st.lon, g.Fused.Lat, g.Fused.Lon); fe > 80 {
					t.Errorf("fused classic estimate %.0f km off", fe)
				}
			}
		}
	}
}

// A signal cost that always prefers "far away" (levels that do not fall
// with distance at all) must not move the estimate beyond the coverage.
func TestGuessSignalCostHorizonPenalty(t *testing.T) {
	cells := guessTestCells(7, 51.5, -1.0, false)
	for i := range cells {
		cells[i].rssi = -20 // flat: the bare inverse-square fit is best at infinity
	}
	g := guessFromCells(cells)
	sig := guessEst(g, "signal")
	if sig == nil || !sig.OK {
		t.Fatalf("signal fit: %+v", sig)
	}
	if e := geo.HaversineKm(51.5, -1.0, sig.Lat, sig.Lon); e > 400 {
		t.Fatalf("flat levels: signal fit %.0f km off, outside the coverage", e)
	}
}

func TestWithLocate2ClassicWeights(t *testing.T) {
	g := guessFromCells(guessTestCells(3, 51.5, -1.0, false))
	if g.Fused == nil {
		t.Fatal("no classic fusion")
	}
	cfgd := LatLon{Lat: 51.5, Lon: -1.0}
	g.Configured = &cfgd
	// v2 within the classic estimates' reach: classic methods get their
	// inverse-variance share (small but non-zero), the answer stays v2-led.
	l2 := &Locate2Result{OK: true, Lat: 51.52, Lon: -1.03, SigmaKm: 2.5, R95Km: 6.2, Samples: 5000, Aircraft: 200, Sessions: 400,
		Terms: []L2Term{{Name: "horizon", Used: true}}, Notes: []string{}}
	out := withLocate2(g, l2, nil)
	if out.Fused == nil || !strings.Contains(out.Fused.Label, "Locate Me v2") {
		t.Fatalf("fused: %+v", out.Fused)
	}
	var sum, classic float64
	for _, e := range out.Estimates {
		sum += float64(e.Weight)
		if e.Method != "locate2" {
			classic += float64(e.Weight)
		}

	}
	if math.Abs(sum-1) > 1e-3 {
		t.Errorf("weights sum to %.4f", sum)
	}
	if !(classic > 0) {
		t.Errorf("classic methods got no weight at all")
	}
	if d := geo.HaversineKm(l2.Lat, l2.Lon, out.Fused.Lat, out.Fused.Lon); d > 1 {
		t.Errorf("fused estimate moved %.2f km away from v2", d)
	}
	if out.Fused.R95Km < l2.R95Km*0.95 {
		t.Errorf("fused r95 %.2f shrank below v2's %.2f", float64(out.Fused.R95Km), float64(l2.R95Km))
	}
	explained := false
	for _, n := range out.Notes {
		if strings.Contains(n, "weight") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("no note explains the weights: %q", out.Notes)
	}
	mustJSON(t, "guess", out)
}
