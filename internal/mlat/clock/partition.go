package clock

import (
	"math"
	"slices"

	"airview/internal/mlat/geo"
)

// Partitioned filter (DESIGN §14 phase 5, §20.2): a time domain (connected
// component) with more than MaxComponentStations stations is split into
// partitions of at most PartitionStations stations, each a dense component
// of its own. The partitions share the domain's network time:
//
//   - a sync group whose members fall into several partitions is processed
//     by each of them; members outside the partition are "external": their
//     clock offset comes from the previous snapshot of their own partition.
//     Up to ExternalStates (station, frame kind) pairs per partition and
//     epoch are epoch-local states marginalised out before the update
//     (external.go), the rest enter the measurement noise D ("consider"
//     treatment); either way the snapshot variance is inflated by
//     ExternalInflate (unknown cross-covariance);
//   - one partition per domain carries the gauge (the one with the most
//     anchors, else the largest); the others are pinned to it through the
//     external members, and their common mode drifts (PartitionDriftNs,
//     below) so that circulating information decays.
//
// The epoch cost then grows linearly with the number of partitions (each
// O(PartitionStations³)) and the partitions are updated in parallel.
//
// Partitions are spatially compact (recursive coordinate bisection of the
// station positions) because co-observations are local, and sticky: a
// station stays in its partition while that partition is part of the same
// domain and not over-full, so the dense state is not rebuilt every epoch.
// New stations join the nearest partition; one that becomes over-full is
// re-bisected.
//
// Measured (scale_test.go TestSimnetScaleCurve, one i9-13900K core,
// Workers 1, 3 simulated minutes, second half): 200 stations dense 61 ms
// per epoch; 500 stations in 5 partitions 51 ms; 1,000 stations in 10
// partitions 104 ms; all synced; GPS / hw12 / sdr12 RMS 7 / 8 / 73 ns
// (200), 10 / 13 / 73 ns (500), 16 / 18 / 73 ns (1,000).

// splitDomain reports whether the station set of one time domain is
// partitioned: above MaxComponentStations, or, with hysteresis, while it
// was partitioned in the previous epoch (it continues old domain same in
// at least two components) and still holds more than 90 % of
// MaxComponentStations. A domain hovering around the threshold therefore
// does not alternate between the dense and the partitioned layout (each
// switch rebuilds the components and moves the gauge).
func splitDomain(set []*station, same uint32, cfg *Config) bool {
	if len(set) > cfg.MaxComponentStations {
		return true
	}
	if same == 0 || len(set)*10 <= cfg.MaxComponentStations*9 {
		return false
	}
	var first *component
	for _, s := range set {
		if c := s.comp; c != nil && c.domain == same && !s.rejoin {
			if first == nil {
				first = c
			} else if c != first {
				return true
			}
		}
	}
	return false
}

// partition splits set (stations of one domain) into parts of at most max
// stations.
func (f *Filter) partition(set []*station, max int) [][]*station {
	// sticky assignment first
	byComp := map[*component][]*station{}
	var loose []*station
	for _, s := range set {
		if s.comp != nil && !s.rejoin && len(s.comp.sts) <= max {
			byComp[s.comp] = append(byComp[s.comp], s)
		} else {
			loose = append(loose, s)
		}
	}
	var parts [][]*station
	keys := make([]*component, 0, len(byComp))
	for c := range byComp {
		keys = append(keys, c)
	}
	slices.SortFunc(keys, func(a, b *component) int { return int(a.id) - int(b.id) })
	for _, c := range keys {
		parts = append(parts, byComp[c])
	}
	// New stations join the nearest part (the centroids of the sticky
	// parts, fixed before anyone joins), full or not: an over-full part is
	// re-bisected below, so every part stays spatially compact. Without
	// sticky parts (a dense domain growing past the limit, or a fresh one)
	// the whole set is bisected.
	//
	// Joining only parts with room filled the partitions in station order
	// when a dense domain first crossed the limit (every station was loose
	// and the first part was nearest until full): on 1,000 simnet stations
	// every partition spanned the whole network (RMS radius 470 km against
	// 150 km when compact), nearly every sync group was external and the
	// GPS clocks ended 40 ns RMS off with e²/σ² ≈ 140.
	if len(parts) == 0 {
		parts = append(parts, loose)
	} else if len(loose) > 0 {
		cs := make([]geo.Vec3, len(parts))
		for i, p := range parts {
			cs[i] = centroid(p)
		}
		for _, s := range loose {
			best, bd := 0, math.Inf(1)
			for i, c := range cs {
				if d := c.Dist(s.info.Pos); d < bd {
					best, bd = i, d
				}
			}
			parts[best] = append(parts[best], s)
		}
	}
	// over-full or degenerate partitions are re-bisected
	var out [][]*station
	for _, p := range parts {
		out = append(out, bisect(p, max)...)
	}
	// merge tiny partitions into their nearest neighbour when it has room
	minSize := max / 8
	for changed := true; changed && len(out) > 1; {
		changed = false
		for i := 0; i < len(out); i++ {
			if len(out[i]) >= minSize {
				continue
			}
			best, bd := -1, math.Inf(1)
			ci := centroid(out[i])
			for j := range out {
				if j == i || len(out[j])+len(out[i]) > max {
					continue
				}
				if d := centroid(out[j]).Dist(ci); d < bd {
					best, bd = j, d
				}
			}
			if best >= 0 {
				out[best] = append(out[best], out[i]...)
				out = slices.Delete(out, i, i+1)
				changed = true
				break
			}
		}
	}
	for _, p := range out {
		sortStations(p)
	}
	return out
}

// bisect splits p recursively along the axis of its largest extent until
// every part has at most max stations.
func bisect(p []*station, max int) [][]*station {
	if len(p) <= max {
		return [][]*station{p}
	}
	var lo, hi geo.Vec3
	for i, s := range p {
		v := s.info.Pos
		if i == 0 {
			lo, hi = v, v
			continue
		}
		lo = geo.Vec3{X: math.Min(lo.X, v.X), Y: math.Min(lo.Y, v.Y), Z: math.Min(lo.Z, v.Z)}
		hi = geo.Vec3{X: math.Max(hi.X, v.X), Y: math.Max(hi.Y, v.Y), Z: math.Max(hi.Z, v.Z)}
	}
	ext := hi.Sub(lo)
	key := func(s *station) float64 { return s.info.Pos.X }
	switch {
	case ext.Y >= ext.X && ext.Y >= ext.Z:
		key = func(s *station) float64 { return s.info.Pos.Y }
	case ext.Z >= ext.X && ext.Z >= ext.Y:
		key = func(s *station) float64 { return s.info.Pos.Z }
	}
	q := slices.Clone(p)
	slices.SortFunc(q, func(a, b *station) int {
		ka, kb := key(a), key(b)
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		}
		return int(a.idx) - int(b.idx)
	})
	// split into a power-balanced pair: k parts needed, left gets ⌈k/2⌉/k
	k := (len(q) + max - 1) / max
	cut := len(q) * ((k + 1) / 2) / k
	return append(bisect(q[:cut], max), bisect(q[cut:], max)...)
}

func centroid(p []*station) geo.Vec3 {
	var c geo.Vec3
	for _, s := range p {
		c = c.Add(s.info.Pos)
	}
	if len(p) > 0 {
		c = c.Scale(1 / float64(len(p)))
	}
	return c
}

// Common-mode drift (Config.PartitionDriftNs). A partition without the
// domain's gauge has no own definition of time: the common mode u of its
// clock offsets (θ of the free clocks, δ of the anchors) is fixed only by
// its external members, i.e. by the previous snapshots of the neighbouring
// partitions. Those estimates were themselves partly informed by this
// partition through the groups that cross the boundary, so the same
// information circulates between partitions every epoch. ExternalInflate
// bounds one round of that reuse; over many rounds it only stays bounded
// when old common-mode information decays. Free clocks forget it through
// their own process noise, anchors (δ walks 1 ns/√h) practically never: on
// 1,000 simnet stations in partitions of 100 the anchored partitions locked
// each other up to 90 ns off with σ of a few ns (GPS e²/σ² 280).
//
// Every epoch such a partition therefore gets P += q·dt·u·uᵀ (θ/δ indices),
// a random walk of its time-scale offset against the domain's network time.
// It leaves every difference inside the partition untouched (u is common to
// all offsets), and with q = 1 ns²/s the common mode is re-pinned by the
// fresh external information within seconds while its steady-state σ stays
// at a few ns.

// domainParts returns the number of components of every time domain.
func (f *Filter) domainParts() map[uint32]int {
	if f.domN == nil {
		f.domN = map[uint32]int{}
	}
	clear(f.domN)
	for _, c := range f.comps {
		f.domN[c.domain]++
	}
	return f.domN
}

// addCommonDrift adds q·u·uᵀ to P, u the indicator of every station's
// clock offset state (θ or δ).
func (c *component) addCommonDrift(q float64) {
	if c.n == 0 || !(q > 0) {
		return
	}
	c.cmIdx = c.commonIndices(kTheta, c.cmIdx)
	n := c.n
	for _, i := range c.cmIdx {
		row := c.P[i*n : i*n+n]
		for _, j := range c.cmIdx {
			row[j] += q
		}
	}
}

// partitionDrift adds the common-mode drift over dt (s) to every component
// that is one partition of a partitioned domain and does not carry the
// domain's gauge (Config.PartitionDriftNs). Called after the prediction and
// before setGauges, so c.gauge is the gauge of the previous epoch: the one
// the information in P was accumulated under.
func (f *Filter) partitionDrift(dt float64) {
	if f.cfg.PartitionDriftNs <= 0 || !(dt > 0) {
		return
	}
	q := f.cfg.PartitionDriftNs * f.cfg.PartitionDriftNs * dt
	parts := f.domainParts()
	for _, c := range f.comps {
		if c.gauge == gaugeNone && parts[c.domain] > 1 {
			c.addCommonDrift(q)
		}
	}
}
