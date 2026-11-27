package quality

import (
	"math"
	"math/rand/v2"
	"testing"
)

// TestLatencyCalLearn checks the per-airframe Kalman estimate, the sample
// gates and the calibrated state.
func TestLatencyCalLearn(t *testing.T) {
	c := NewLatencyCal(LatencyCalParams{})
	rng := rand.New(rand.NewPCG(1, 1))
	const key = 0x4ca123
	const trueL = 0.21
	const v = 230.0
	now := int64(1.8e18)
	// rejected: low NACp, slow, imprecise, NaN
	for _, s := range []LatencySample{
		{Key: key, NowNs: now, AlongM: 10, SigmaAlongM: 10, SpeedMps: v, NACp: 7},
		{Key: key, NowNs: now, AlongM: 10, SigmaAlongM: 10, SpeedMps: 30, NACp: 9},
		{Key: key, NowNs: now, AlongM: 10, SigmaAlongM: 40, SpeedMps: v, NACp: 9},
		{Key: key, NowNs: now, AlongM: math.NaN(), SigmaAlongM: 10, SpeedMps: v, NACp: 9},
	} {
		if c.Learn(s) {
			t.Fatalf("sample accepted: %+v", s)
		}
	}
	if _, _, n, _ := c.Latency(key); n != 0 {
		t.Fatal("rejected samples counted")
	}
	// the reference applies the prior (0): along = v·L + noise
	for i := 0; i < 200; i++ {
		applied, _, _ := c.ForReference(key, false)
		a := v*(trueL-applied) + rng.NormFloat64()*20
		c.Learn(LatencySample{Key: key, NowNs: now + int64(i)*5e8, AlongM: a, SigmaAlongM: 20, SpeedMps: v, AppliedS: applied, NACp: 9})
	}
	l, sg, n, cal := c.Latency(key)
	if !cal || n < 190 || math.Abs(l-trueL) > 3*sg+0.005 || sg > 0.02 {
		t.Fatalf("estimate %.3f ± %.3f n %d cal %v", l, sg, n, cal)
	}
	// a gross sample is gated
	if c.Learn(LatencySample{Key: key, NowNs: now + 2e11, AlongM: 2000, SigmaAlongM: 20, SpeedMps: v, NACp: 9}) {
		t.Fatal("gross sample accepted")
	}
	// not enabled: outside the hold-out the prior applies; in the
	// hold-out the arm decides
	if _, _, ap := c.ForReference(key, false); ap {
		t.Fatal("applied outside the hold-out while disabled")
	}
	if _, _, ap := c.ForReference(key, true); ap != c.Treated(key) {
		t.Fatal("hold-out reference does not follow the arm")
	}
	// estimate bounds
	c2 := NewLatencyCal(LatencyCalParams{})
	for i := 0; i < 50; i++ {
		c2.Learn(LatencySample{Key: 1, NowNs: now, AlongM: v * 0.39, SigmaAlongM: 5, SpeedMps: v, NACp: 9})
	}
	for i := 0; i < 50; i++ {
		c2.Learn(LatencySample{Key: 1, NowNs: now, AlongM: v * 0.39, SigmaAlongM: 5, SpeedMps: v, AppliedS: 0.4, NACp: 9})
	}
	if l, _, _, _ := c2.Latency(1); l > 0.4 {
		t.Fatalf("bound: %.3f", l)
	}
	// TTL
	if r := c.Decide(now + 2e11); r.Airframes != 1 || r.Calibrated != 1 {
		t.Fatalf("before TTL: %+v", r)
	}
	if r := c.Decide(now + 2e11 + 25*hourNs); r.Airframes != 0 {
		t.Fatalf("after TTL: %+v", r)
	}
	if _, _, n, _ := c.Latency(key); n != 0 {
		t.Fatal("airframe not forgotten")
	}
}

// TestLatencyCalArms checks the treated fraction.
func TestLatencyCalArms(t *testing.T) {
	c := NewLatencyCal(LatencyCalParams{Salt: 99})
	var tr int
	for k := uint32(0); k < 20000; k++ {
		if c.Treated(k) {
			tr++
		}
	}
	if f := float64(tr) / 20000; math.Abs(f-0.5) > 0.02 {
		t.Fatalf("treated fraction %.3f", f)
	}
	all := NewLatencyCal(LatencyCalParams{TreatFraction: 1})
	if !all.Treated(12345) {
		t.Fatal("TreatFraction 1")
	}
}

// abFeed calibrates n airframes and scores them: control airframes with
// along errors of 1σ ctrlM, treated with treatM (plus a common MLAT noise
// of 10 m).
func abFeed(c *LatencyCal, rng *rand.Rand, nowNs int64, n, perAc int, ctrlM, treatM float64) {
	for k := uint32(1); k <= uint32(n); k++ {
		for i := 0; i < 30; i++ {
			c.Learn(LatencySample{Key: k, NowNs: nowNs, AlongM: rng.NormFloat64() * 5, SigmaAlongM: 5, SpeedMps: 200, NACp: 9})
		}
		s := ctrlM
		if c.Treated(k) {
			s = treatM
		}
		bias := rng.NormFloat64() * s // per-airframe reference error
		for i := 0; i < perAc; i++ {
			c.Score(LatencySample{Key: k, NowNs: nowNs, AlongM: bias + rng.NormFloat64()*10, SigmaAlongM: 15, SpeedMps: 200, NACp: 9})
		}
	}
}

// TestLatencyCalDecide checks the A/B decisions: insufficient data,
// enable on a significant gain, no enable without one, disable on harm.
func TestLatencyCalDecide(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 2))
	now := int64(1.8e18)
	c := NewLatencyCal(LatencyCalParams{Salt: 3})
	abFeed(c, rng, now, 10, 20, 40, 10)
	if r := c.Decide(now); r.Decision != "insufficient" || r.Enabled {
		t.Fatalf("few airframes: %+v", r)
	}
	abFeed(c, rng, now, 120, 20, 40, 10)
	r := c.Decide(now)
	if r.Decision != "enable" || !r.Enabled || r.Gain < 0.5 || r.Z < 3 {
		t.Fatalf("clear gain: %+v", r)
	}
	if _, _, ap := c.ForReference(5, false); !ap {
		t.Fatal("not applied outside the hold-out after enable")
	}
	// later: treated becomes worse (a new window) → disable
	later := now + 7*hourNs
	abFeed(c, rng, later, 120, 20, 10, 40)
	if r := c.Decide(later); r.Decision != "disable" || r.Enabled {
		t.Fatalf("harm: %+v", r)
	}
	// no difference → never enabled
	c3 := NewLatencyCal(LatencyCalParams{Salt: 3})
	abFeed(c3, rng, now, 120, 20, 20, 20)
	if r := c3.Decide(now); r.Enabled {
		t.Fatalf("no gain enabled: %+v", r)
	}
	// imprecise or low-NACp samples do not score
	if c3.Score(LatencySample{Key: 1, NowNs: now, AlongM: 1, SigmaAlongM: 80, NACp: 9}) ||
		c3.Score(LatencySample{Key: 1, NowNs: now, AlongM: 1, SigmaAlongM: 10, NACp: 6}) ||
		c3.Score(LatencySample{Key: 99999, NowNs: now, AlongM: 1, SigmaAlongM: 10, NACp: 9}) {
		t.Fatal("score gates")
	}
	if c3.Last().Decision == "" {
		t.Fatal("Last")
	}
}
