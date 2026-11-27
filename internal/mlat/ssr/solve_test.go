package ssr

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"airview/internal/mlat/geo"
)

type synthRadar struct {
	pos    geo.LLH
	period float64
}

// synthObs builds pair observations straight from the measurement model:
// two aircraft in one beam azimuth of a rotating radar, Delta from the
// bistatic ranges plus per-airframe biases (s) and Gaussian noise (s).
func synthObs(rng *rand.Rand, u geo.LLH, radars []synthRadar, n int, biasS, noiseS float64) []PairObs {
	atm := geo.DefaultAtmosphere()
	U := u.ECEF()
	bias := make([]float64, 80)
	for i := range bias {
		bias[i] = (2*rng.Float64() - 1) * biasS
	}
	var out []PairObs
	for len(out) < n {
		k := len(out) % len(radars)
		r := radars[k]
		f := geo.NewFrame(geo.LLH{Lat: r.pos.Lat, Lon: r.pos.Lon})
		R := r.pos.ECEF()
		t := float64(len(out))*0.37 + rng.Float64()*0.01
		az := math.Mod(360*t/r.period, 360) * math.Pi / 180
		r1 := 20e3 + rng.Float64()*180e3
		r2 := r1 + 15e3 + rng.Float64()*120e3
		mk := func(rr float64) geo.Vec3 {
			a := az + (rng.Float64()-0.5)*1.2*math.Pi/180
			l := geo.FromECEF(f.FromENU(geo.Vec3{X: rr * math.Sin(a), Y: rr * math.Cos(a)}))
			return geo.ToECEF(l.Lat, l.Lon, 3000+rng.Float64()*9000)
		}
		pk, pj := mk(r1), mk(r2)
		if rng.Intn(2) == 0 {
			pk, pj = pj, pk
		}
		if pk.Dist(U) > 330e3 || pj.Dist(U) > 330e3 {
			continue
		}
		ik, ij := rng.Intn(len(bias)), rng.Intn(len(bias))
		if ik == ij {
			continue
		}
		d := (atm.Range(R, pj)-atm.Range(R, pk)+atm.Range(U, pj)-atm.Range(U, pk))/geo.C + bias[ij] - bias[ik] + rng.NormFloat64()*noiseS
		out = append(out, PairObs{
			K: AircraftKey{uint32(ik + 1), ClassModeAC}, J: AircraftKey{uint32(ij + 1), ClassModeAC},
			TK: t, TJ: t + d, PK: pk, PJ: pj, SK: 1, SJ: 1, Delta: d, N: 8, Radar: -1,
		})
	}
	return out
}

func synthSetup() (geo.LLH, []synthRadar) {
	u := geo.LLH{Lat: 52, Lon: -1, H: 40}
	return u, []synthRadar{
		{pos: geo.LLH{Lat: 52.6, Lon: -0.3, H: 90}, period: 4},
		{pos: geo.LLH{Lat: 50.9, Lon: -2.4, H: 120}, period: 8},
	}
}

func horizErr(u geo.LLH, p geo.Vec3) float64 {
	f := geo.NewFrame(u)
	e := f.ToENU(p)
	return math.Hypot(e.X, e.Y)
}

func TestSolveSyntheticSites(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	u, rs := synthSetup()
	obs := synthObs(rng, u, rs, 1500, 300e-9, 20e-9)
	cfg := DefaultConfig()
	for _, r := range rs {
		cfg.Sites = append(cfg.Sites, Site{Lat: r.pos.Lat, Lon: r.pos.Lon, HeightM: r.pos.H, SigmaHM: 5, SigmaUM: 5})
	}
	cfg.NoLatency = true
	res := SolveObs(obs, Hint{HeightKnown: true, HeightM: u.H, HeightSigmaM: 5}, cfg, geo.DefaultAtmosphere())
	if !res.OK {
		t.Fatalf("not OK: %s", res.Reason)
	}
	if e := horizErr(u, res.ECEF); e > 3 || e > 4*res.SigmaHM+1 {
		t.Fatalf("horizontal error %.2f m (σ %.2f)", e, res.SigmaHM)
	}
	if res.NRadars != 2 || res.NUsed < 1400 || !res.Experimental {
		t.Fatalf("result %+v", res)
	}
	for _, r := range res.Radars {
		if !r.Rotation.Valid() || r.Rotation.Coherence < 0.9 {
			t.Fatalf("rotation not recovered: %+v", r.Rotation)
		}
	}
}

func TestSolveSyntheticLearned(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	u, rs := synthSetup()
	obs := synthObs(rng, u, rs, 2500, 300e-9, 60e-9)
	cfg := DefaultConfig()
	sites, _ := LocateInterrogators(obs, cfg)
	if len(sites) < 2 {
		t.Fatalf("found %d interrogators", len(sites))
	}
	for _, r := range rs {
		best := math.Inf(1)
		for _, s := range sites {
			best = math.Min(best, horizErr(r.pos, s.ECEF))
		}
		if best > 2000 {
			t.Fatalf("site %v: nearest learned at %.0f m", r.pos, best)
		}
	}
	res := SolveObs(obs, Hint{}, cfg, geo.DefaultAtmosphere())
	if !res.OK {
		t.Fatalf("not OK: %s", res.Reason)
	}
	if e := horizErr(u, res.ECEF); e > 15 {
		t.Fatalf("horizontal error %.1f m (σ %.1f)", e, res.SigmaHM)
	}
	if res.NRadars != 2 {
		t.Fatalf("radars %d", res.NRadars)
	}
}

func TestSolveRejects(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	u, rs := synthSetup()
	cfg := DefaultConfig()
	res := SolveObs(synthObs(rng, u, rs, 10, 0, 0), Hint{}, cfg, geo.DefaultAtmosphere())
	if res.OK || !strings.Contains(res.Reason, "observations") {
		t.Fatalf("few obs: %+v", res.Reason)
	}
	// Random deltas: no coherent interrogator / no fit.
	obs := synthObs(rng, u, rs, 400, 0, 0)
	for i := range obs {
		obs[i].PK, obs[i].PJ = obs[(i*7)%len(obs)].PK, obs[(i*13)%len(obs)].PJ
	}
	res = SolveObs(obs, Hint{}, cfg, geo.DefaultAtmosphere())
	if res.OK {
		t.Fatalf("scrambled geometry accepted: err %.0f m", horizErr(u, res.ECEF))
	}
}

func TestAssignBeamAndSide(t *testing.T) {
	site := geo.LLH{Lat: 52, Lon: 0, H: 50}
	f := geo.NewFrame(site)
	at := func(az, r float64) geo.Vec3 {
		a := az * math.Pi / 180
		l := geo.FromECEF(f.FromENU(geo.Vec3{X: r * math.Sin(a), Y: r * math.Cos(a)}))
		return geo.ToECEF(l.Lat, l.Lon, 9000)
	}
	obs := []PairObs{
		{PK: at(30, 50e3), PJ: at(30.5, 120e3)}, // same beam
		{PK: at(30, 50e3), PJ: at(40, 120e3)},   // 10° apart
		{PK: at(30, 50e3), PJ: at(210, 50e3)},   // opposite sides
		{PK: at(100, 80e3), PJ: at(101, 200e3)}, // same beam
	}
	rs := SitesFromConfig(Config{Sites: []Site{{Lat: site.Lat, Lon: site.Lon, HeightM: 50}}, RadarHeightSigmaM: 300})
	Assign(obs, rs, DefaultConfig())
	want := []int{0, -1, -1, 0}
	for i := range obs {
		if obs[i].Radar != want[i] {
			t.Fatalf("obs %d: radar %d want %d", i, obs[i].Radar, want[i])
		}
	}
}
