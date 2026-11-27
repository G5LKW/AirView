package group

import (
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// ClusterMin emits the small clusters (2-3 stations) that Cluster drops;
// the size is clamped to 2..ModeACMinStations; chance pairs of different
// codes or inconsistent times are not joined.
func TestModeACClusterMin(t *testing.T) {
	o := geo.ToECEF(52, 0, 50)
	f := geo.NewFrameECEF(o)
	sites := siteMap{}
	for i, d := range [][2]float64{{0, 0}, {40e3, 5e3}, {-35e3, 20e3}, {10e3, -45e3}} {
		sites[model.StationIdx(i+2)] = f.FromENU(geo.Vec3{X: d[0], Y: d[1]})
	}
	m, err := NewModeAC(DefaultConfig(), sites, nil)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(4, 5))
	p := f.FromENU(geo.Vec3{X: 20e3, Y: 30e3, Z: 6000})
	t0 := Time{Ns: 1_763_000_000_000_000_000}
	var obs []ACObs
	heard := func(at Time, code uint16, sts ...model.StationIdx) {
		for _, st := range sts {
			r, _, _ := sites.Site(st)
			obs = append(obs, ACObs{Station: st, Code: code, T: at.Add(r.Dist(p)/mPerNs + 30*rng.NormFloat64()), Var: 900})
		}
	}
	heard(t0, 0x0123, 2, 3)              // pair
	heard(t0.Add(20e6), 0x0416, 2, 3, 4) // triple
	heard(t0.Add(40e6), 0x0789, 5)       // single: never
	// different codes at the same instant are never joined
	heard(t0.Add(60e6), 0x0AAA, 2)
	heard(t0.Add(60e6), 0x0BBB, 3)
	sizes := map[uint16]int{}
	n := m.ClusterMin(obs, 2, func(c *ACCluster) { sizes[c.Code&0x1FBF] = len(c.Members) })
	if n != 2 || sizes[0x0123] != 2 || sizes[0x0416] != 3 {
		t.Fatalf("ClusterMin(2): n=%d sizes %v", n, sizes)
	}
	if n := m.Cluster(obs, func(*ACCluster) {}); n != 0 {
		t.Fatalf("Cluster emitted %d small clusters", n)
	}
	// clamped: 1 -> 2, 10 -> ModeACMinStations
	if n := m.ClusterMin(obs, 1, func(*ACCluster) {}); n != 2 {
		t.Fatalf("ClusterMin(1) = %d", n)
	}
	if n := m.ClusterMin(obs, 10, func(*ACCluster) {}); n != 0 {
		t.Fatalf("ClusterMin(10) = %d", n)
	}
	// an inconsistent pair (time difference beyond the baseline) is split
	obs = obs[:0]
	r2, _, _ := sites.Site(2)
	obs = append(obs, ACObs{Station: 2, Code: 0x0321, T: t0.Add(r2.Dist(p) / mPerNs), Var: 900},
		ACObs{Station: 3, Code: 0x0321, T: t0.Add(r2.Dist(p)/mPerNs + 300e3), Var: 900})
	if n := m.ClusterMin(obs, 2, func(*ACCluster) {}); n != 0 {
		t.Fatalf("inconsistent pair clustered (%d)", n)
	}
}
