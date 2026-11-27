package ssr

import (
	"math"
	"math/bits"
	"math/rand"
	"strings"
	"testing"

	"airview/internal/mlat/geo"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Enabled {
		t.Fatal("experimental estimator must be off by default")
	}
	d := c
	d.Normalize()
	if d.MatchTolNs != c.MatchTolNs || d.MinMatches != c.MinMatches || d.MaxAircraft != c.MaxAircraft {
		t.Fatal("Normalize not idempotent")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := Config{Sites: []Site{{Name: "x", Lat: 95}}, BeamwidthDeg: 30}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "sites[0]") || !strings.Contains(err.Error(), "beamwidth") {
		t.Fatalf("Validate: %v", err)
	}
}

func TestApplyEnv(t *testing.T) {
	env := map[string]string{
		"AIRVIEW_SSR_ENABLED":         "true",
		"AIRVIEW_SSR_MIN_MATCHES":     "6",
		"AIRVIEW_SSR_MATCH_TOL_NS":    "250",
		"AIRVIEW_SSR_SITES":           "north,52.5,-1.2,90; south,51.1,-0.9",
		"AIRVIEW_SSR_MAX_RADARS":      "x",
		"AIRVIEW_SSR_LATENCY_SIGMA_S": "NaN",
	}
	var c Config
	err := c.ApplyEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err == nil || !strings.Contains(err.Error(), "MAX_RADARS") || !strings.Contains(err.Error(), "LATENCY_SIGMA_S") {
		t.Fatalf("want errors for bad values, got %v", err)
	}
	if !c.Enabled || c.MinMatches != 6 || c.MatchTolNs != 250 {
		t.Fatalf("parsed fields not applied: %+v", c)
	}
	if len(c.Sites) != 2 || c.Sites[0].Name != "north" || c.Sites[0].HeightM != 90 || c.Sites[1].Lat != 51.1 {
		t.Fatalf("sites: %+v", c.Sites)
	}
	if _, err := ParseSites("a,1"); err == nil {
		t.Fatal("short site accepted")
	}
	if _, err := ParseSites("a,91,0"); err == nil {
		t.Fatal("bad latitude accepted")
	}
	keys := EnvKeys()
	if len(keys) < 40 || keys[len(keys)-1] != "AIRVIEW_SSR_SITES" {
		t.Fatalf("EnvKeys: %d %v", len(keys), keys[len(keys)-1])
	}
}

func TestModeCCode(t *testing.T) {
	seen := map[uint16]int{}
	var prev uint16
	for n := 0; n < 1280; n++ {
		alt := float64(n*100 - 1200)
		c, ok := ModeCCode(alt)
		if !ok {
			t.Fatalf("%v ft not encodable", alt)
		}
		if c > 0o7777 {
			t.Fatalf("code %o out of range", c)
		}
		if m, dup := seen[c]; dup {
			t.Fatalf("%v ft and %v ft share code %04o", alt, float64(m*100-1200), c)
		}
		seen[c] = n
		// Gillham: adjacent 100 ft levels differ in exactly one pulse.
		if n > 0 && bits.OnesCount16(c^prev) != 1 {
			t.Fatalf("%v ft: %04o vs %04o differ in %d pulses", alt, c, prev, bits.OnesCount16(c^prev))
		}
		prev = c
		// C pulses are never all zero; D1 is never used.
		if c>>3&7 == 0 || c&1 != 0 {
			t.Fatalf("%v ft: invalid code %04o", alt, c)
		}
		// D pulses only from 30,800 ft on (500 ft count >= 64).
		if (c&7 != 0) != (alt >= 30800) {
			t.Fatalf("%v ft: D pulses %o", alt, c&7)
		}
	}
	// Rounding to the nearest 100 ft.
	a, _ := ModeCCode(35049)
	b, _ := ModeCCode(35000)
	if a != b {
		t.Fatal("rounding")
	}
	for _, v := range []float64{-1300, 126800, math.NaN()} {
		if _, ok := ModeCCode(v); ok {
			t.Fatalf("%v encodable", v)
		}
	}
}

func TestTrackStore(t *testing.T) {
	s := NewTrackStore(120, 15, 2)
	o := geo.ToECEF(52, 0, 10000)
	v := geo.Vec3{X: 100, Y: -50, Z: 20}
	for i := 0; i <= 20; i++ {
		tt := float64(i) * 0.5
		s.Add(1, Position{T: tt, P: o.Add(v.Scale(tt)), SigmaM: 10, Squawk: 0o1234, AltBaroFt: 33000})
	}
	p, sg, ok := s.At(1, 3.3)
	if !ok || p.Dist(o.Add(v.Scale(3.3))) > 1e-6 || sg != 10 {
		t.Fatalf("interpolation: %v %v %v", ok, p.Dist(o.Add(v.Scale(3.3))), sg)
	}
	p, sg, ok = s.At(1, 11)
	if !ok || p.Dist(o.Add(v.Scale(11))) > 1e-6 || sg <= 10 {
		t.Fatalf("extrapolation: %v %v", ok, sg)
	}
	if _, _, ok := s.At(1, 13); ok {
		t.Fatal("extrapolated too far")
	}
	if _, _, ok := s.At(2, 1); ok {
		t.Fatal("unknown aircraft")
	}
	vel, ok := s.VelAt(1, 5)
	if !ok || vel.Dist(v) > 1e-6 {
		t.Fatalf("VelAt %v %v", vel, ok)
	}
	// Out-of-order insert.
	s.Add(3, Position{T: 2, P: o, Squawk: -1, AltBaroFt: math.NaN()})
	s.Add(3, Position{T: 1, P: o.Add(geo.Vec3{X: -100}), Squawk: -1, AltBaroFt: math.NaN()})
	if p, _, ok := s.At(3, 1.5); !ok || p.Dist(o.Add(geo.Vec3{X: -50})) > 1e-6 {
		t.Fatal("out-of-order insert")
	}
	// Mode A and Mode C resolution.
	if id, ok := s.Resolve(0o1234, 5, nil); !ok || id != 1 {
		t.Fatal("squawk resolve")
	}
	cc, _ := ModeCCode(33000)
	if id, ok := s.Resolve(cc, 5, nil); !ok || id != 1 {
		t.Fatal("mode C resolve")
	}
	s.Add(4, Position{T: 5, P: o, Squawk: 0o7000, AltBaroFt: 33020})
	if _, n := s.ResolveN(cc, 5, nil); n != 2 {
		t.Fatalf("shared flight level: %d candidates", n)
	}
	if _, ok := s.Resolve(0o7000, 5, []uint16{0o7000}); ok {
		t.Fatal("conspicuity code resolved")
	}
	if _, ok := s.Resolve(0o4321, 5, nil); ok {
		t.Fatal("unknown code resolved")
	}
	// Pruning.
	s.Add(1, Position{T: 500, P: o, Squawk: 0o1234, AltBaroFt: math.NaN()})
	if s.Len() != 1 {
		t.Fatalf("prune: %d tracks", s.Len())
	}
}

// trains builds the arrival trains of two aircraft answering one staggered
// interrogation sequence, plus their true difference.
func trains(rng *rand.Rand, n int, delta float64, stagger bool, jit float64) (a, b []float64) {
	return trainsPRI(rng, n, delta, stagger, jit, 4e-3)
}

func trainsPRI(rng *rand.Rand, n int, delta float64, stagger bool, jit, base float64) (a, b []float64) {
	t := 1.0
	for i := 0; i < n; i++ {
		pri := base
		if stagger {
			pri *= []float64{0.93, 1.05, 0.98, 1.07, 0.95, 1.02}[i%6]
		}
		t += pri
		if rng.Float64() < 0.9 {
			a = append(a, t+rng.NormFloat64()*jit)
		}
		if rng.Float64() < 0.9 {
			b = append(b, t+delta+rng.NormFloat64()*jit)
		}
	}
	return
}

func TestMatchTrains(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, delta := range []float64{-600e-6, -35e-6, 80e-6, 900e-6} {
		a, b := trains(rng, 12, delta, true, 50e-9)
		m := MatchTrains(a, b, 1.5e-3, 400e-9, 4, 0.6, 3)
		if len(m) != 1 || math.Abs(m[0].Delta-delta) > 60e-9 || m[0].N < 6 {
			t.Fatalf("delta %v: %+v", delta, m)
		}
	}
	// Constant PRI with a window wider than the PRI: alias, rejected.
	a, b := trains(rng, 12, 300e-6, false, 30e-9)
	if m := MatchTrains(a, b, 6e-3, 400e-9, 4, 0.6, 3); len(m) != 0 {
		t.Fatalf("PRI alias accepted: %+v", m)
	}
	// ... but fine when the geometry window excludes the alias.
	if m := MatchTrains(a, b, 1e-3, 400e-9, 4, 0.6, 3); len(m) != 1 || math.Abs(m[0].Delta-300e-6) > 50e-9 {
		t.Fatalf("constant PRI in window: %+v", m)
	}
	// Two interrogators interleaved: two peaks.
	a1, b1 := trains(rng, 10, 200e-6, true, 30e-9)
	a2, b2 := trainsPRI(rng, 10, -450e-6, true, 30e-9, 5.3e-3) // an independent interrogator
	for i := range a2 {
		a2[i] += 1.3e-3
	}
	for i := range b2 {
		b2[i] += 1.3e-3
	}
	ma := append(append([]float64(nil), a1...), a2...)
	mb := append(append([]float64(nil), b1...), b2...)
	sortF(ma)
	sortF(mb)
	m := MatchTrains(ma, mb, 1e-3, 400e-9, 4, 0.6, 3)
	if len(m) != 2 {
		t.Fatalf("two interrogators: %+v", m)
	}
	// Unrelated trains: nothing.
	u := make([]float64, 12)
	w := make([]float64, 12)
	for i := range u {
		u[i] = 1 + float64(i)*4.1e-3 + rng.Float64()*1e-3
		w[i] = 1 + float64(i)*3.7e-3 + rng.Float64()*1e-3
	}
	sortF(u)
	sortF(w)
	if m := MatchTrains(u, w, 1e-3, 400e-9, 4, 0.6, 3); len(m) != 0 {
		t.Fatalf("unrelated trains matched: %+v", m)
	}
	if m := MatchTrains(u[:2], w, 1e-3, 400e-9, 4, 0.6, 3); m != nil {
		t.Fatal("short train")
	}
}

func sortF(x []float64) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j] < x[j-1]; j-- {
			x[j], x[j-1] = x[j-1], x[j]
		}
	}
}

func TestEstimateRotation(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for _, P := range []float64{4.0, 7.3, -9.6} {
		var ts, az []float64
		for i := 0; i < 400; i++ {
			tt := 100 + rng.Float64()*600
			ts = append(ts, tt)
			az = append(az, 37+360*tt/P+(rng.Float64()-0.5)*2.4)
		}
		r := EstimateRotation(ts, az, 60, 2, 15)
		if math.Abs(r.PeriodS-P) > 1e-3*math.Abs(P) || r.Coherence < 0.95 {
			t.Fatalf("period %v: got %v coherence %v", P, r.PeriodS, r.Coherence)
		}
		for i := range ts {
			if d, ok := r.Residual(ts[i], az[i]); !ok || math.Abs(d) > 2 {
				t.Fatalf("residual %v %v", d, ok)
			}
		}
		if d, ok := r.Residual(400, 37+360*400/P+90); !ok || math.Abs(math.Abs(d)-90) > 2 {
			t.Fatalf("off-beam residual %v", d)
		}
	}
	var ts, az []float64
	for i := 0; i < 400; i++ {
		ts = append(ts, rng.Float64()*600)
		az = append(az, rng.Float64()*360)
	}
	if r := EstimateRotation(ts, az, 60, 2, 15); r.Coherence > 0.6 {
		t.Fatalf("random azimuths coherent: %v", r.Coherence)
	}
	if r := EstimateRotation(ts[:3], az[:3], 60, 2, 15); r.Valid() {
		t.Fatal("too few points")
	}
}

func TestGnomonic(t *testing.T) {
	g := NewGnomonic(geo.LLH{Lat: 52, Lon: -1})
	for _, p := range []geo.LLH{{Lat: 52, Lon: -1}, {Lat: 54.5, Lon: 2}, {Lat: 49.8, Lon: -5.5}} {
		x, y, ok := g.Fwd(p.ECEF())
		if !ok {
			t.Fatal("Fwd")
		}
		q := g.Inv(x, y, 0)
		if d := q.ECEF().Dist(p.ECEF()); d > 0.01 {
			t.Fatalf("round trip %v m", d)
		}
	}
	// Great circle through two points is (nearly) straight.
	a, b := geo.LLH{Lat: 51, Lon: -3}, geo.LLH{Lat: 53.5, Lon: 1.5}
	ax, ay, _ := g.Fwd(a.ECEF())
	bx, by, _ := g.Fwd(b.ECEF())
	pa, pb := a.ECEF().Unit(), b.ECEF().Unit()
	mid := pa.Add(pb).Unit().Scale(6.37e6)
	mx, my, _ := g.Fwd(mid)
	L := math.Hypot(bx-ax, by-ay)
	off := math.Abs((bx-ax)*(my-ay)-(by-ay)*(mx-ax)) / L
	if off > 100 {
		t.Fatalf("great circle bends %v m in the projection", off)
	}
}
