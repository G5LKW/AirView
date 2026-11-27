package clock

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// TestSplitDomainHysteresis: a domain is partitioned above
// MaxComponentStations and stays partitioned down to 90 % of it.
func TestSplitDomainHysteresis(t *testing.T) {
	cfg := &Config{MaxComponentStations: 20}
	a := &component{id: 1, domain: 7}
	b := &component{id: 2, domain: 7}
	mk := func(n int, split bool) []*station {
		var set []*station
		for i := 0; i < n; i++ {
			s := newStation(0)
			s.comp = a
			if split && i%2 == 1 {
				s.comp = b
			}
			set = append(set, s)
		}
		return set
	}
	cases := []struct {
		n     int
		split bool
		want  bool
	}{
		{21, false, true},  // over the limit
		{20, false, false}, // dense at the limit
		{19, true, true},   // was partitioned, > 90 %
		{18, true, false},  // was partitioned, ≤ 90 %
		{19, false, false}, // was dense
	}
	for _, c := range cases {
		if got := splitDomain(mk(c.n, c.split), 7, cfg); got != c.want {
			t.Errorf("n=%d split=%v: got %v", c.n, c.split, got)
		}
	}
	if splitDomain(mk(19, true), 0, cfg) {
		t.Error("a new domain is not partitioned below the limit")
	}
}

// TestBisectBalanced: recursive bisection yields parts of at most max
// stations, balanced, and every station exactly once.
func TestBisectBalanced(t *testing.T) {
	var p []*station
	for i := 0; i < 1000; i++ {
		s := newStation(model.StationIdx(i + 2))
		x := float64(i%40) * 1e4
		y := float64(i/40) * 1e4
		s.info.Pos = geo.Vec3{X: 4e6 + x, Y: 6e5 + y, Z: 4.8e6}
		p = append(p, s)
	}
	parts := bisect(p, 100)
	seen := map[*station]bool{}
	for _, q := range parts {
		if len(q) > 100 || len(q) < 50 {
			t.Errorf("part of %d stations", len(q))
		}
		for _, s := range q {
			if seen[s] {
				t.Fatal("station in two parts")
			}
			seen[s] = true
		}
	}
	if len(seen) != 1000 || len(parts) != 10 {
		t.Errorf("%d stations in %d parts", len(seen), len(parts))
	}
}

// gridStations places n stations on a 40-column grid of 10 km cells.
func gridStations(n int) []*station {
	var p []*station
	for i := 0; i < n; i++ {
		s := newStation(model.StationIdx(i + 2))
		s.info.Pos = geo.Vec3{X: 4e6, Y: 6e5 + float64(i%40)*1e4, Z: 4.8e6 + float64(i/40)*1e4}
		p = append(p, s)
	}
	return p
}

// rmsRadius returns the RMS distance of the stations from their centroid.
func rmsRadius(p []*station) float64 {
	c := centroid(p)
	v := 0.0
	for _, s := range p {
		d := s.info.Pos.Dist(c)
		v += d * d
	}
	return math.Sqrt(v / float64(len(p)))
}

// TestPartitionCompactFirstSplit: a dense domain that grows past the limit
// (every station in one component larger than the partition size) is
// bisected into spatially compact parts, not filled in station order
// (regression: 1,000 simnet stations, every partition spanned the network).
func TestPartitionCompactFirstSplit(t *testing.T) {
	f, _ := New(Config{OriginUnixNs: 1})
	set := gridStations(1000)
	dense := &component{id: 1, domain: 3, sts: set}
	for _, s := range set {
		s.comp = dense
	}
	// station order interleaves the grid rows: fill-in-order would give
	// parts of 2.5 rows spanning the full 400 km width
	parts := f.partition(set, 100)
	all := rmsRadius(set)
	seen := 0
	for _, p := range parts {
		seen += len(p)
		if len(p) > 100 {
			t.Errorf("part of %d stations", len(p))
		}
		if r := rmsRadius(p); r > 0.45*all {
			t.Errorf("part of %d stations: RMS radius %.0f km of %.0f km", len(p), r/1e3, all/1e3)
		}
	}
	if seen != 1000 || len(parts) < 10 || len(parts) > 16 {
		t.Errorf("%d stations in %d parts", seen, len(parts))
	}
}

// TestPartitionJoinNearest: new stations join the nearest sticky part even
// when it is full; the over-full part is re-bisected, the other parts keep
// their members (sticky) and every part stays compact.
func TestPartitionJoinNearest(t *testing.T) {
	f, _ := New(Config{OriginUnixNs: 1})
	set := gridStations(400)
	parts := f.partition(set, 100)
	comps := make([]*component, len(parts))
	for i, p := range parts {
		comps[i] = &component{id: uint32(10 + i), domain: 3, sts: p}
		for _, s := range p {
			s.comp = comps[i]
		}
	}
	// 30 new stations in one corner (near the first grid rows)
	var all []*station
	all = append(all, set...)
	for i := 0; i < 30; i++ {
		s := newStation(model.StationIdx(5000 + i))
		s.info.Pos = geo.Vec3{X: 4e6, Y: 6e5 + float64(i%6)*1e4 + 5e3, Z: 4.8e6 + float64(i/6)*1e4 + 5e3}
		all = append(all, s)
	}
	out := f.partition(all, 100)
	untouched := 0
	for _, p := range out {
		if len(p) > 100 {
			t.Errorf("part of %d stations", len(p))
		}
		owner := p[0].comp
		same := owner != nil && len(owner.sts) == len(p)
		for _, s := range p {
			if s.comp != owner {
				same = false
			}
		}
		if same {
			untouched++
		}
		if r := rmsRadius(p); r > 0.6*rmsRadius(all) {
			t.Errorf("part of %d stations: RMS radius %.0f km", len(p), r/1e3)
		}
	}
	if untouched < len(parts)-1 {
		t.Errorf("%d of %d parts kept unchanged", untouched, len(parts))
	}
}

// TestCommonDrift: the partition drift adds q to every θ/δ pair (u·uᵀ),
// leaves every other entry alone, and so keeps the variance of every
// offset difference θ_i − θ_j unchanged.
func TestCommonDrift(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 9))
	c, _ := testComponent(r)
	n := c.n
	before := slices.Clone(c.P)
	c.addCommonDrift(0.5)
	u := c.commonIndices(kTheta, nil)
	if len(u) != 3 {
		t.Fatalf("%d offset states, want 3", len(u))
	}
	in := map[int]bool{}
	for _, i := range u {
		in[i] = true
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			want := before[i*n+j]
			if in[i] && in[j] {
				want += 0.5
			}
			if math.Abs(c.P[i*n+j]-want) > 1e-12*math.Max(1, math.Abs(want)) {
				t.Fatalf("P[%d,%d] = %g, want %g", i, j, c.P[i*n+j], want)
			}
		}
	}
	for _, i := range u {
		for _, j := range u {
			db := before[i*n+i] + before[j*n+j] - 2*before[i*n+j]
			da := c.P[i*n+i] + c.P[j*n+j] - 2*c.P[i*n+j]
			if math.Abs(da-db) > 1e-9*math.Max(1, db) {
				t.Fatalf("var(θ%d − θ%d) %g -> %g", i, j, db, da)
			}
		}
	}
	// default and off
	cfg := Config{OriginUnixNs: 1}
	if err := cfg.normalize(); err != nil || cfg.PartitionDriftNs != 1 {
		t.Fatalf("default drift %g (%v)", cfg.PartitionDriftNs, err)
	}
	cfg = Config{OriginUnixNs: 1, PartitionDriftNs: -1}
	if err := cfg.normalize(); err != nil || cfg.PartitionDriftNs != 0 {
		t.Fatalf("drift off: %g (%v)", cfg.PartitionDriftNs, err)
	}
}

// TestCommonDriftOnlyNonGaugePartitions: a dense component and the gauge
// partition get no drift; the other partitions of a partitioned domain do.
func TestCommonDriftOnlyNonGaugePartitions(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 1))
	f, _ := New(Config{OriginUnixNs: 1})
	mk := func(id, dom uint32, g gaugeKind) *component {
		c, _ := testComponent(r)
		c.id, c.domain, c.gauge, c.tE = id, dom, g, 10
		return c
	}
	dense := mk(1, 1, gaugeNone) // only component of its domain
	gaugeP := mk(2, 2, gaugeAnchor)
	other := mk(3, 2, gaugeNone)
	f.comps = []*component{dense, gaugeP, other}
	type snap struct {
		c *component
		p []float64
	}
	var ss []snap
	for _, c := range f.comps {
		ss = append(ss, snap{c, slices.Clone(c.P)})
	}
	f.partitionDrift(1) // q = 1 ns²
	for _, s := range ss {
		changed := !slices.Equal(s.p, s.c.P)
		if changed != (s.c == other) {
			t.Errorf("component %d: changed %v", s.c.id, changed)
		}
	}
}
