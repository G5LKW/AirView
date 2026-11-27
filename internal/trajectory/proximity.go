package trajectory

import (
	"math"
	"sort"
	"time"
)

// ProximityOptions configures Manager.Proximity. Zero fields take defaults.
type ProximityOptions struct {
	SearchKm   float64 // only pairs currently closer than this are examined (default 40)
	HorizonSec float64 // look-ahead (default 120, max 600)
	StepSec    float64 // path sampling step (default 5)
	HThreshKm  float64 // horizontal threshold (default 9.26 = 5 NM)
	VThreshFt  float64 // vertical threshold (default 1000)
	MaxPairs   int     // result cap (default 200)
}

func (o ProximityOptions) withDefaults() ProximityOptions {
	if !(o.SearchKm > 0) {
		o.SearchKm = 40
	}
	if !(o.HorizonSec > 0) {
		o.HorizonSec = 120
	}
	if o.HorizonSec > 600 {
		o.HorizonSec = 600
	}
	if !(o.StepSec > 0) {
		o.StepSec = 5
	}
	if o.StepSec < 1 {
		o.StepSec = 1
	}
	if !(o.HThreshKm > 0) {
		o.HThreshKm = 9.26
	}
	if !(o.VThreshFt > 0) {
		o.VThreshFt = 1000
	}
	if o.MaxPairs <= 0 {
		o.MaxPairs = 200
	}
	return o
}

// ProximityPair is a predicted close approach between two aircraft.
type ProximityPair struct {
	A          string  `json:"a"`
	B          string  `json:"b"`
	TCPASec    float64 `json:"tcpa"`       // seconds from now to the closest point of approach
	HDistKm    float64 `json:"hDistKm"`    // horizontal separation at CPA
	VSepFt     float64 `json:"vSepFt"`     // vertical separation at CPA
	NowHDistKm float64 `json:"nowHDistKm"` //
	NowVSepFt  float64 `json:"nowVSepFt"`  //
	Lat        float64 `json:"lat"`        // midpoint at CPA
	Lon        float64 `json:"lon"`        //
	AltA       float64 `json:"altA"`       // ft at CPA
	AltB       float64 `json:"altB"`       //
	// Severity = max(hDist/hThresh, vSep/vThresh) at CPA; <= 1 means both thresholds are violated.
	Severity float64 `json:"severity"`
}

type proxAC struct {
	hex string
	k0  int // index into proxPts
}

type proxPt struct{ lat, lon, alt float64 }

type proxScratch struct {
	acs  []proxAC
	pts  []proxPt
	grid map[uint64][]int32
	out  []ProximityPair
}

// Proximity returns predicted close approaches between airborne aircraft with
// known altitude. A spatial grid limits the pair search to neighbours, so the
// cost is about O(n) for typical traffic.
func (m *Manager) Proximity(now time.Time, opt ProximityOptions) []ProximityPair {
	opt = opt.withDefaults()
	p := &m.env.p
	t := unixSec(now)
	K := int(opt.HorizonSec/opt.StepSec) + 1
	m.proxMu.Lock()
	defer m.proxMu.Unlock()
	sc := &m.prox
	sc.acs = sc.acs[:0]
	sc.pts = sc.pts[:0]
	maxLat := 0.0
	for si := range m.shards {
		sh := &m.shards[si]
		sh.mu.Lock()
		for _, tr := range sh.m {
			if !tr.ready() || tr.ground || !tr.vert.ok || t-tr.lastAccPosT > p.maxCoast || t-tr.lastAccPosT < -p.maxCoast {
				continue
			}
			ps := tr.pstate(p)
			ps.propagate(t-ps.t, p, false)
			if math.Hypot(ps.x[ix], ps.x[iy]) > 5000 {
				ps.recenter()
			}
			sc.acs = append(sc.acs, proxAC{hex: tr.hex, k0: len(sc.pts)})
			for k := 0; k < K; k++ {
				if k > 0 {
					ps.propagate(opt.StepSec, p, false)
				}
				la, lo := ps.fr.toLatLon(ps.x[ix], ps.x[iy])
				alt, _, _ := ps.vertical(p)
				sc.pts = append(sc.pts, proxPt{la, lo, alt})
				if k == 0 && math.Abs(la) > maxLat {
					maxLat = math.Abs(la)
				}
			}
		}
		sh.mu.Unlock()
	}
	sc.out = sc.out[:0]
	n := len(sc.acs)
	if n < 2 {
		return nil
	}
	if sc.grid == nil || len(sc.grid) > 8192 {
		sc.grid = make(map[uint64][]int32, 256)
	}
	for k, v := range sc.grid {
		sc.grid[k] = v[:0]
	}
	dLat := opt.SearchKm / 110.574
	dLon := opt.SearchKm / (111.320 * math.Cos(math.Min(maxLat+1, 89)*deg2rad))
	key := func(ci, cj int64) uint64 { return uint64(ci+1<<20)<<32 | uint64(cj+1<<20)&0xffffffff }
	cellOf := func(pt proxPt) (int64, int64) {
		return int64(math.Floor(pt.lat / dLat)), int64(math.Floor((pt.lon + 180) / dLon))
	}
	for i := 0; i < n; i++ {
		ci, cj := cellOf(sc.pts[sc.acs[i].k0])
		kk := key(ci, cj)
		sc.grid[kk] = append(sc.grid[kk], int32(i))
	}
	nLonCells := int64(math.Ceil(360 / dLon))
	for i := 0; i < n; i++ {
		a0 := sc.pts[sc.acs[i].k0]
		ci, cj := cellOf(a0)
		for di := int64(-1); di <= 1; di++ {
			for dj := int64(-1); dj <= 1; dj++ {
				cjj := cj + dj
				if cjj < 0 {
					cjj += nLonCells
				} else if cjj >= nLonCells {
					cjj -= nLonCells
				}
				for _, j32 := range sc.grid[key(ci+di, cjj)] {
					j := int(j32)
					if j <= i {
						continue
					}
					b0 := sc.pts[sc.acs[j].k0]
					if haversineM(a0.lat, a0.lon, b0.lat, b0.lon) > opt.SearchKm*1000 {
						continue
					}
					if pr, ok := pairCPA(sc.pts[sc.acs[i].k0:sc.acs[i].k0+K], sc.pts[sc.acs[j].k0:sc.acs[j].k0+K], opt); ok {
						pr.A, pr.B = sc.acs[i].hex, sc.acs[j].hex
						sc.out = append(sc.out, pr)
					}
				}
			}
		}
	}
	sort.Slice(sc.out, func(a, b int) bool {
		if sc.out[a].Severity != sc.out[b].Severity {
			return sc.out[a].Severity < sc.out[b].Severity
		}
		return sc.out[a].TCPASec < sc.out[b].TCPASec
	})
	if len(sc.out) > opt.MaxPairs {
		sc.out = sc.out[:opt.MaxPairs]
	}
	res := make([]ProximityPair, len(sc.out))
	copy(res, sc.out)
	return res
}

// pairCPA scans two equally sampled paths for the minimum normalised separation.
func pairCPA(a, b []proxPt, opt ProximityOptions) (ProximityPair, bool) {
	refLat := (a[0].lat + b[0].lat) / 2
	kx := 111320 * math.Cos(refLat*deg2rad)
	const ky = 110574.0
	rel := func(k int) (float64, float64) {
		dl := b[k].lon - a[k].lon
		if dl > 180 {
			dl -= 360
		} else if dl < -180 {
			dl += 360
		}
		return dl * kx, (b[k].lat - a[k].lat) * ky
	}
	hT, vT := opt.HThreshKm*1000, opt.VThreshFt
	best := math.Inf(1)
	var bk int
	var bt, bh, bv float64
	x0, y0 := rel(0)
	nowH := math.Hypot(x0, y0)
	nowV := math.Abs(b[0].alt - a[0].alt)
	for k := 0; k+1 < len(a); k++ {
		x1, y1 := rel(k + 1)
		dx, dy := x1-x0, y1-y0
		cands := [3]float64{0, 1, 0}
		nc := 2
		if d2 := dx*dx + dy*dy; d2 > 1e-9 {
			tau := clamp(-(x0*dx+y0*dy)/d2, 0, 1)
			cands[2] = tau
			nc = 3
		}
		for c := 0; c < nc; c++ {
			tau := cands[c]
			h := math.Hypot(x0+tau*dx, y0+tau*dy)
			va := a[k].alt + tau*(a[k+1].alt-a[k].alt)
			vb := b[k].alt + tau*(b[k+1].alt-b[k].alt)
			v := math.Abs(vb - va)
			s := math.Max(h/hT, v/vT)
			// ties (the vertical term dominates while the horizontal one
			// is still shrinking) go to the smaller horizontal distance
			if s < best-1e-9 || (s <= best+1e-9 && h < bh) {
				best, bk, bt, bh, bv = s, k, tau, h, v
			}
		}
		x0, y0 = x1, y1
	}
	if best > 1 {
		return ProximityPair{}, false
	}
	k := bk
	lerp := func(u, v float64) float64 { return u + bt*(v-u) }
	la := lerp(a[k].lat, a[k+1].lat)
	lb := lerp(b[k].lat, b[k+1].lat)
	oa := lerp(a[k].lon, a[k+1].lon)
	ob := lerp(b[k].lon, b[k+1].lon)
	if ob-oa > 180 {
		ob -= 360
	} else if oa-ob > 180 {
		ob += 360
	}
	pr := ProximityPair{
		TCPASec:    (float64(k) + bt) * opt.StepSec,
		HDistKm:    bh / 1000,
		VSepFt:     bv,
		NowHDistKm: nowH / 1000,
		NowVSepFt:  nowV,
		Lat:        (la + lb) / 2,
		Lon:        normLon((oa + ob) / 2),
		AltA:       lerp(a[k].alt, a[k+1].alt),
		AltB:       lerp(b[k].alt, b[k+1].alt),
		Severity:   best,
	}
	return pr, true
}
