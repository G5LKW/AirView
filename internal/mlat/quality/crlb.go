package quality

import (
	"math"
	"slices"
	"sync"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/solve"
)

// CRLB map (DESIGN §9.6, admin only): the expected horizontal 1σ of a
// snapshot fix at a few altitudes on a regular grid, from the Cramér-Rao
// bound of the stations that can hear a target there. A station hears a
// target when the target is inside its 4/3-earth radio horizon (smooth
// earth: terrain masks are not modelled) and within MaxRangeM. Of those
// (the 64 nearest) MaxStations are chosen as the MLAT planner chooses them
// (greedy D-optimal design, selectDOptimal), each with its timing σ (clock
// + jitter), and an altitude row of σ AltSigmaM. Choosing the nearest
// instead (CRLBParams.Nearest) made the map 1.5-2.5× pessimistic outside
// the network on simnet: the planner reaches for distant stations that open
// the angles.
//
// The map is a coverage-and-geometry bound, not a promise: it assumes every
// station in range hears the target and that the stated σ are right. The
// simnet validation (crlb_simnet_test.go; numbers in WIRING.md) measures
// how the solver's real errors relate to it.

// CRLBStation is one station for the map.
type CRLBStation struct {
	Pos    geo.Vec3 // antenna ECEF (HAE)
	SigmaS float64  // timing 1σ, s
}

// CRLBParams tune CRLBMap. Zero fields take the defaults.
type CRLBParams struct {
	StepDeg     float64   // grid step (default 0.25°)
	AltsM       []float64 // target heights (default 914 m ≈ 3000 ft, 3048 m ≈ FL100, 9144 m ≈ FL300)
	MarginDeg   float64   // grid margin around the stations' bounding box (default 3°)
	MaxRangeM   float64   // largest station-target ground range (default 400 km)
	MaxStations int       // stations per target (default 12)
	// Nearest selects the MaxStations nearest stations instead of the
	// planner's greedy D-optimal choice among the 64 nearest (default).
	Nearest    bool
	AltSigmaM  float64 // altitude row 1σ (default 30 m; < 0 = none)
	MaxSigmaHM float64 // cells above this are omitted (default 2000 m)
	MaxCells   int     // safety bound on grid cells per altitude (default 200,000)
	// Workers > 1 computes grid rows in that many goroutines (joined
	// before CRLBMap returns; the result is identical). 0 or 1 = the
	// calling goroutine only.
	Workers    int
	Atmosphere *geo.Atmosphere
}

func (p *CRLBParams) defaults() {
	if p.StepDeg <= 0 {
		p.StepDeg = 0.25
	}
	if len(p.AltsM) == 0 {
		p.AltsM = []float64{914.4, 3048, 9144}
	}
	if p.MarginDeg <= 0 {
		p.MarginDeg = 3
	}
	if p.MaxRangeM <= 0 {
		p.MaxRangeM = 400e3
	}
	if p.MaxStations <= 0 {
		p.MaxStations = 12
	}
	if p.AltSigmaM == 0 {
		p.AltSigmaM = 30
	}
	if p.MaxSigmaHM <= 0 {
		p.MaxSigmaHM = 2000
	}
	if p.MaxCells <= 0 {
		p.MaxCells = 200000
	}
}

// CRLBCell is one grid point.
type CRLBCell struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	AltM    float64 `json:"altM"`
	SigmaHM float64 `json:"sigmaHM"`
	N       int     `json:"n"` // stations used
}

type crlbStx struct {
	CRLBStation
	lat, lon, h float64
}

// CRLBIndex answers CRLB queries for one station set: stations are
// bucketed by 1° cells so a target only visits stations within range. It
// is immutable after NewCRLBIndex and safe for concurrent use.
type CRLBIndex struct {
	p                              CRLBParams
	xs                             []crlbStx
	bucket                         map[[2]int][]int32
	minLat, maxLat, minLon, maxLon float64
}

// NewCRLBIndex indexes the stations with p (zero fields = defaults). It
// returns nil with fewer than three stations.
func NewCRLBIndex(sts []CRLBStation, p CRLBParams) *CRLBIndex {
	p.defaults()
	if len(sts) < 3 {
		return nil
	}
	ix := &CRLBIndex{p: p, xs: make([]crlbStx, len(sts)), bucket: map[[2]int][]int32{},
		minLat: 90, maxLat: -90, minLon: 180, maxLon: -180}
	for i, s := range sts {
		l := geo.FromECEF(s.Pos)
		ix.xs[i] = crlbStx{s, l.Lat, l.Lon, l.H}
		ix.minLat, ix.maxLat = math.Min(ix.minLat, l.Lat), math.Max(ix.maxLat, l.Lat)
		ix.minLon, ix.maxLon = math.Min(ix.minLon, l.Lon), math.Max(ix.maxLon, l.Lon)
		k := [2]int{int(math.Floor(l.Lat)), int(math.Floor(l.Lon))}
		ix.bucket[k] = append(ix.bucket[k], int32(i))
	}
	return ix
}

type crlbCand struct {
	i int32
	d float64
}

// crlbScratch is per-goroutine working memory.
type crlbScratch struct {
	cs   []crlbCand
	pos  []geo.Vec3
	sig  []float64
	pick []int
}

// crlbMaxCand bounds the candidates of one target (the nearest are kept;
// the planner likewise considers at most 64 hearers).
const crlbMaxCand = 64

// selectDOptimal chooses up to k of the stations (antenna ECEF, timing σ
// in s) for a target at p by greedy D-optimal design, as the MLAT planner
// does (internal/mlat/plan, "Station plan per MLAT aircraft"): with the
// Jacobian row h_j = [u_jᵀ, 1] of station j for (position, c·τ) and
// P = J⁻¹ (a weak prior of 100 km per unknown keeps J invertible), the
// next station maximises h_jᵀ P h_j / σ_j² (the factor by which it
// multiplies det J), and P is updated by Sherman–Morrison. It appends the
// chosen indices to out in the order chosen.
func selectDOptimal(pos []geo.Vec3, sigS []float64, p geo.Vec3, k int, out []int) []int {
	const prior = 1e5
	var P [4][4]float64
	for i := 0; i < 4; i++ {
		P[i][i] = prior * prior
	}
	n := min(len(pos), 64)
	var used uint64
	for len(out) < k {
		bi, bg := -1, 0.0
		var bh, bph [4]float64
		var bs2 float64
		for i := 0; i < n; i++ {
			if used&(1<<uint(i)) != 0 {
				continue
			}
			d := p.Sub(pos[i])
			r := d.Norm()
			if r < 1 {
				continue
			}
			h := [4]float64{d.X / r, d.Y / r, d.Z / r, 1}
			var ph [4]float64
			for a := 0; a < 4; a++ {
				ph[a] = P[a][0]*h[0] + P[a][1]*h[1] + P[a][2]*h[2] + P[a][3]*h[3]
			}
			sm := sigS[i] * geo.C
			s2 := sm * sm
			if g := (h[0]*ph[0] + h[1]*ph[1] + h[2]*ph[2] + h[3]*ph[3]) / s2; g > bg {
				bi, bg, bh, bph, bs2 = i, g, h, ph, s2
			}
		}
		if bi < 0 {
			break
		}
		used |= 1 << uint(bi)
		den := bs2 + bh[0]*bph[0] + bh[1]*bph[1] + bh[2]*bph[2] + bh[3]*bph[3]
		for a := 0; a < 4; a++ {
			for b := 0; b < 4; b++ {
				P[a][b] -= bph[a] * bph[b] / den
			}
		}
		out = append(out, bi)
	}
	return out
}

// SigmaAt returns the expected horizontal 1σ (m) of a snapshot fix of a
// target at (lat, lon, altM HAE) and the number of stations behind it; ok
// is false when fewer than the necessary stations hear it or the geometry
// is singular. MaxSigmaHM does not apply.
func (ix *CRLBIndex) SigmaAt(lat, lon, altM float64) (sigmaHM float64, n int, ok bool) {
	if ix == nil {
		return 0, 0, false
	}
	var s crlbScratch
	return ix.at(&s, lat, lon, altM)
}

func (ix *CRLBIndex) at(s *crlbScratch, lat, lon, alt float64) (float64, int, bool) {
	p := &ix.p
	const degLat = 111e3
	dLat := int(math.Ceil(p.MaxRangeM/degLat)) + 1
	degLon := degLat * math.Max(math.Cos(lat*math.Pi/180), 0.05)
	dLon := int(math.Ceil(p.MaxRangeM/degLon)) + 1
	tgt := geo.ToECEF(lat, lon, alt)
	s.cs = s.cs[:0]
	bl, bo := int(math.Floor(lat)), int(math.Floor(lon))
	for x := bl - dLat; x <= bl+dLat; x++ {
		for y := bo - dLon; y <= bo+dLon; y++ {
			for _, i := range ix.bucket[[2]int{x, y}] {
				st := &ix.xs[i]
				d := tgt.Dist(st.Pos)
				if d > p.MaxRangeM*1.05 || d > geo.RadioHorizon(st.h, alt, 0) {
					continue
				}
				s.cs = append(s.cs, crlbCand{i, d})
			}
		}
	}
	need := 4
	if p.AltSigmaM > 0 {
		need = 3
	}
	if len(s.cs) < need {
		return 0, 0, false
	}
	slices.SortFunc(s.cs, func(a, b crlbCand) int {
		switch {
		case a.d < b.d:
			return -1
		case a.d > b.d:
			return 1
		}
		return int(a.i - b.i)
	})
	if len(s.cs) > crlbMaxCand {
		s.cs = s.cs[:crlbMaxCand]
	}
	s.pos, s.sig = s.pos[:0], s.sig[:0]
	for _, c := range s.cs {
		s.pos = append(s.pos, ix.xs[c.i].Pos)
		s.sig = append(s.sig, ix.xs[c.i].SigmaS)
	}
	if len(s.cs) > p.MaxStations {
		if p.Nearest {
			s.pos, s.sig = s.pos[:p.MaxStations], s.sig[:p.MaxStations]
		} else {
			s.pick = selectDOptimal(s.pos, s.sig, tgt, p.MaxStations, s.pick[:0])
			slices.Sort(s.pick) // pick[k] >= k: the compaction below is safe
			for k, j := range s.pick {
				s.pos[k], s.sig[k] = s.pos[j], s.sig[j]
			}
			s.pos, s.sig = s.pos[:len(s.pick)], s.sig[:len(s.pick)]
		}
	}
	enu, _, ok := solve.PredictCRLB(s.pos, s.sig, tgt, p.AltSigmaM, p.Atmosphere)
	if !ok {
		return 0, 0, false
	}
	sh := math.Sqrt(math.Max(enu[0]+enu[4], 0))
	if math.IsNaN(sh) {
		return 0, 0, false
	}
	return sh, len(s.pos), true
}

// Map computes the grid: cells in row-major order (latitude, longitude,
// altitude), omitting cells above MaxSigmaHM. nil when the grid would
// exceed MaxCells.
func (ix *CRLBIndex) Map() []CRLBCell {
	if ix == nil {
		return nil
	}
	p := &ix.p
	lat0 := math.Max(-85, math.Floor((ix.minLat-p.MarginDeg)/p.StepDeg)*p.StepDeg)
	lat1 := math.Min(85, ix.maxLat+p.MarginDeg)
	lon0 := math.Floor((ix.minLon-p.MarginDeg)/p.StepDeg) * p.StepDeg
	lon1 := ix.maxLon + p.MarginDeg
	nLat := int((lat1-lat0)/p.StepDeg) + 1
	nLon := int((lon1-lon0)/p.StepDeg) + 1
	if nLat*nLon > p.MaxCells {
		return nil
	}
	row := func(s *crlbScratch, a int, out []CRLBCell) []CRLBCell {
		lat := lat0 + float64(a)*p.StepDeg
		for b := 0; b < nLon; b++ {
			lon := lon0 + float64(b)*p.StepDeg
			for _, alt := range p.AltsM {
				sh, n, ok := ix.at(s, lat, lon, alt)
				if !ok || sh > p.MaxSigmaHM {
					continue
				}
				out = append(out, CRLBCell{Lat: lat, Lon: lon, AltM: alt, SigmaHM: sh, N: n})
			}
		}
		return out
	}
	w := min(max(p.Workers, 1), nLat)
	if w == 1 {
		var s crlbScratch
		var out []CRLBCell
		for a := 0; a < nLat; a++ {
			out = row(&s, a, out)
		}
		return out
	}
	rows := make([][]CRLBCell, nLat)
	var wg sync.WaitGroup
	next := make(chan int, nLat)
	for a := 0; a < nLat; a++ {
		next <- a
	}
	close(next)
	for g := 0; g < w; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var s crlbScratch
			for a := range next {
				rows[a] = row(&s, a, nil)
			}
		}()
	}
	wg.Wait()
	var total int
	for _, r := range rows {
		total += len(r)
	}
	out := make([]CRLBCell, 0, total)
	for _, r := range rows {
		out = append(out, r...)
	}
	return out
}

// CRLBMap computes the map (NewCRLBIndex(sts, p).Map()). Cost: cells ×
// nearby stations; see BenchmarkCRLBMap for the figure at 1,000 stations.
func CRLBMap(sts []CRLBStation, p CRLBParams) []CRLBCell {
	return NewCRLBIndex(sts, p).Map()
}

// CRLBSummary condenses a map per altitude (admin overview).
type CRLBSummary struct {
	AltM float64 `json:"altM"`
	// Cells with a value; P50 and P95 of σ_h over them (m).
	Cells int     `json:"cells"`
	P50M  float64 `json:"p50M"`
	P95M  float64 `json:"p95M"`
	// Cells with σ_h <= 50, 100, 250 and 1000 m.
	Below50  int `json:"below50"`
	Below100 int `json:"below100"`
	Below250 int `json:"below250"`
	Below1k  int `json:"below1k"`
}

// SummarizeCRLB returns one summary per altitude present in cells, in
// ascending altitude order.
func SummarizeCRLB(cells []CRLBCell) []CRLBSummary {
	by := map[float64][]float64{}
	for _, c := range cells {
		by[c.AltM] = append(by[c.AltM], c.SigmaHM)
	}
	var out []CRLBSummary
	for alt, v := range by {
		slices.Sort(v)
		s := CRLBSummary{AltM: alt, Cells: len(v), P50M: v[len(v)/2], P95M: v[min(len(v)-1, int(0.95*float64(len(v))))]}
		for _, x := range v {
			if x <= 50 {
				s.Below50++
			}
			if x <= 100 {
				s.Below100++
			}
			if x <= 250 {
				s.Below250++
			}
			if x <= 1000 {
				s.Below1k++
			}
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b CRLBSummary) int {
		if a.AltM < b.AltM {
			return -1
		}
		if a.AltM > b.AltM {
			return 1
		}
		return 0
	})
	return out
}
