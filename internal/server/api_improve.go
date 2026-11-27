package server

import (
	"context"
	"math"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/quality"
	"airview/internal/model"
	"airview/internal/registry"
)

// Coverage improvement (admin, DESIGN §9.6 "CRLB map ... admin only", §15
// "admin CRLB map guides where to add GPS hardware"): where would one more
// station help MLAT most?
//
// The analysis rebuilds the CRLB station set (located, approved stations;
// timing σ from the clock filter when synced, else the class prior, as
// Engine.CRLBMap does), lays an evaluation grid over the network (stepDeg,
// default 0.5°) at one target height, and for every candidate site on that
// grid adds a hypothetical station with the given timing σ (default 30 ns,
// a GPS-disciplined receiver) and recomputes σ_h on the grid cells within
// the CRLB range (400 km) of the candidate. The gain of a candidate is the
// area-weighted sum over those cells of
//
//	u(σ_after) − u(σ_before),  u(σ) = log10(2000 m / min(σ, 2000 m)), u(none) = 0
//
// so new coverage and large relative improvements count, and tiny gains in
// already excellent areas do not. The result lists the best sites (with
// cells newly covered, cells newly at grade A ≤ 50 m and B ≤ 150 m) and the
// gain of every candidate cell (normalised 0..1) for the admin map layer.
// Admin only: the candidates are derived from exact station positions.

const (
	improveTTL           = 10 * time.Minute
	improveMaxCandidates = 900
	improveTop           = 12
	improveDefaultStep   = 0.5
	improveDefaultSigma  = 30.0 // ns: GPS-disciplined receiver (class prior)
	improveDefaultAltM   = 3048.0
	improveRangeKm       = 400.0
)

type improveCache struct {
	mu  sync.Mutex
	at  time.Time
	key improveKey
	res *improveResult
}

type improveKey struct {
	eng    any
	alt    float64
	step   float64
	sigma  float64
	nSta   int
	sumPos float64
}

// improveSite is one candidate site.
type improveSite struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Gain     float64 `json:"gain"`     // area-weighted log gain (km²·decades)
	Score    float64 `json:"score"`    // gain normalised to the best site (0..1)
	NewCells int     `json:"newCells"` // cells without coverage before
	NewA     int     `json:"newA"`     // cells that reach σ_h ≤ 50 m
	NewB     int     `json:"newB"`     // cells that reach σ_h ≤ 150 m
	Better   int     `json:"better"`   // cells improved by ≥ 10 %
	// MedianBeforeM / MedianAfterM: median σ_h of the covered cells within
	// range of the site before and after (0 = none covered).
	MedianBeforeM float64 `json:"medianBeforeM"`
	MedianAfterM  float64 `json:"medianAfterM"`
}

type improveCell struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Score float64 `json:"score"` // 0..1
}

type improveResult struct {
	Running    bool          `json:"running"`
	Computed   int64         `json:"computed"`
	AltM       float64       `json:"altM"`
	StepDeg    float64       `json:"stepDeg"`
	SigmaNs    float64       `json:"sigmaNs"`
	Stations   int           `json:"stations"`
	Candidates int           `json:"candidates"`
	Reason     string        `json:"reason,omitempty"`
	Baseline   improveBase   `json:"baseline"`
	Sites      []improveSite `json:"sites"`
	Cells      []improveCell `json:"cells"`
	ElapsedMs  int64         `json:"elapsedMs"`
}

type improveBase struct {
	Cells int     `json:"cells"` // covered grid cells (σ_h ≤ 2 km)
	P50M  float64 `json:"p50M"`
	P90M  float64 `json:"p90M"`
}

func init() {
	routes = append(routes, Route{Method: "GET", Path: "/api/admin/mlat/crlb/improve", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true,
		API: apiAdminCRLBImprove, NoAuto: true,
		Params: []Param{{"alt", "target height in m (default 3048 = FL100; 300..15000)"}, {"step", "grid step in degrees (0.25..2, default 0.5)"},
			{"sigmaNs", "timing σ of the hypothetical station in ns (5..1000, default 30 = GPS)"}, {"refresh", "1 = recompute now"}},
		Summary: "Where one more station would improve MLAT most: candidate sites ranked by the CRLB gain (area-weighted log improvement of σ_h), with newly covered and newly grade-A/B cells, plus the gain of every candidate cell for the map.",
		Returns: "`{\"running\":true,\"altM\",\"stepDeg\",\"sigmaNs\",\"stations\",\"candidates\",\"baseline\":{cells,p50M,p90M},\"sites\":[{lat,lon,gain,score,newCells,newA,newB,better,medianBeforeM,medianAfterM}],\"cells\":[{lat,lon,score}]}`",
		Notes:   "Admin only (derived from exact station positions). Cached for 10 minutes per parameter set; a computation takes up to a few seconds for 1,000 stations."})
}

// crlbStations rebuilds the CRLB station set from the registry and the
// engine's clock view.
func (s *Server) crlbStations(m mlatSource) []quality.CRLBStation {
	reg := s.registry()
	if reg == nil {
		return nil
	}
	var out []quality.CRLBStation
	for _, st := range reg.List(registry.Filter{Admission: model.AdmApproved}) {
		if !st.Located || !st.ClockCls.Timed() {
			continue
		}
		sig := st.ClockCls.PriorJitterNs()
		if rep, ok := m.Station(st.Idx); ok {
			if rep.PositionSource == "none" {
				continue
			}
			if c := rep.Clock; c != nil && c.Synced {
				if v := math.Hypot(c.SigmaThetaNs, c.JitterNs); v > 0 {
					sig = v
				}
			}
		}
		if !(sig > 0) {
			continue
		}
		g := st.Effective
		out = append(out, quality.CRLBStation{Pos: geo.ToECEF(g.Lat, g.Lon, g.HAE), SigmaS: sig * 1e-9})
	}
	return out
}

func apiAdminCRLBImprove(s *Server, r *http.Request) (any, error) {
	m := s.mlatSrc()
	if m == nil {
		return mlatOff, nil
	}
	alt, err := qFloat(r, "alt", improveDefaultAltM)
	if err != nil || alt < 300 || alt > 15000 {
		return nil, badRequest("alt: 300..15000 m")
	}
	step, err := qFloat(r, "step", improveDefaultStep)
	if err != nil || step < 0.25 || step > 2 {
		return nil, badRequest("step: 0.25..2 degrees")
	}
	sigma, err := qFloat(r, "sigmaNs", improveDefaultSigma)
	if err != nil || sigma < 5 || sigma > 1000 {
		return nil, badRequest("sigmaNs: 5..1000")
	}
	sts := s.crlbStations(m)
	k := improveKey{eng: m.Key(), alt: alt, step: step, sigma: sigma, nSta: len(sts)}
	for _, st := range sts {
		k.sumPos += st.Pos.X*1e-3 + st.Pos.Y*1e-6 + st.Pos.Z*1e-9 + st.SigmaS*1e9
	}
	c := &s.improve
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.res != nil && c.key == k && time.Since(c.at) < improveTTL && qStr(r, "refresh") != "1" {
		return c.res, nil
	}
	select {
	case s.heavySem <- struct{}{}:
		defer func() { <-s.heavySem }()
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	res := improveAnalysis(r.Context(), sts, alt, step, sigma, runtime.GOMAXPROCS(0))
	if r.Context().Err() != nil {
		return nil, r.Context().Err()
	}
	c.res, c.key, c.at = res, k, time.Now()
	return res, nil
}

// improveUtil is u(σ) (see the file comment).
func improveUtil(sig float64, ok bool) float64 {
	if !ok || !(sig > 0) || sig >= 2000 {
		return 0
	}
	return math.Log10(2000 / sig)
}

// improveAnalysis ranks candidate sites (see the file comment). workers
// bounds the goroutines; ctx cancels.
func improveAnalysis(ctx context.Context, sts []quality.CRLBStation, alt, step, sigmaNs float64, workers int) *improveResult {
	t0 := time.Now()
	res := &improveResult{Running: true, Computed: t0.UnixMilli(), AltM: alt, StepDeg: step, SigmaNs: sigmaNs, Stations: len(sts),
		Sites: []improveSite{}, Cells: []improveCell{}}
	if len(sts) < 3 {
		res.Reason = "at least 3 located, timed stations are needed"
		return res
	}
	p := quality.CRLBParams{Atmosphere: nil}
	base := quality.NewCRLBIndex(sts, p)
	if base == nil {
		res.Reason = "no CRLB geometry"
		return res
	}
	// The grid: the stations' bounding box plus 2° (candidates) and the
	// CRLB range (evaluation), on step-aligned cell centres.
	minLat, maxLat, minLon, maxLon := 90.0, -90.0, 180.0, -180.0
	for _, st := range sts {
		g := geo.FromECEF(st.Pos)
		minLat, maxLat = min(minLat, g.Lat), max(maxLat, g.Lat)
		minLon, maxLon = min(minLon, g.Lon), max(maxLon, g.Lon)
	}
	for {
		nLat := math.Ceil((maxLat-minLat+4)/step) + 1
		nLon := math.Ceil((maxLon-minLon+4)/step) + 1
		if nLat*nLon <= improveMaxCandidates || step >= 2 {
			break
		}
		step = math.Min(2, step*1.5)
	}
	res.StepDeg = step
	snap := func(v float64) float64 { return (math.Floor(v/step) + 0.5) * step }
	margin := 2.0
	evalMargin := margin + improveRangeKm/111.32
	type cell struct {
		lat, lon, area float64
		before         float64
		ok             bool
	}
	var cells []cell
	idx := map[[2]int]int{}
	key := func(lat, lon float64) [2]int {
		return [2]int{int(math.Floor(lat / step)), int(math.Floor(lon / step))}
	}
	for lat := snap(minLat - evalMargin); lat <= maxLat+evalMargin; lat += step {
		if lat < -85 || lat > 85 {
			continue
		}
		for lon := snap(minLon - evalMargin/math.Max(0.2, math.Cos(lat*math.Pi/180))); lon <= maxLon+evalMargin/math.Max(0.2, math.Cos(lat*math.Pi/180)); lon += step {
			sg, _, ok := base.SigmaAt(lat, lon, alt)
			ok = ok && sg < 2000
			idx[key(lat, lon)] = len(cells)
			cells = append(cells, cell{lat: lat, lon: lon, area: step * step * 111.32 * 111.32 * math.Cos(lat*math.Pi/180), before: sg, ok: ok})
		}
		if len(cells) > 200000 {
			break
		}
	}
	var covered []float64
	for _, c := range cells {
		if c.ok {
			covered = append(covered, c.before)
		}
	}
	slices.Sort(covered)
	if n := len(covered); n > 0 {
		res.Baseline = improveBase{Cells: n, P50M: rnd1(covered[n/2]), P90M: rnd1(covered[min(n-1, int(0.9*float64(n)))])}
	}
	var cands []int
	for i, c := range cells {
		if c.lat >= minLat-margin && c.lat <= maxLat+margin && c.lon >= minLon-margin/math.Max(0.2, math.Cos(c.lat*math.Pi/180)) &&
			c.lon <= maxLon+margin/math.Max(0.2, math.Cos(c.lat*math.Pi/180)) {
			cands = append(cands, i)
		}
	}
	res.Candidates = len(cands)
	sigS := sigmaNs * 1e-9
	reachDeg := improveRangeKm / 111.32
	sites := make([]improveSite, len(cands))
	var next int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers = max(1, min(workers, 8))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ext := make([]quality.CRLBStation, len(sts)+1)
			copy(ext, sts)
			var before, after []float64
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if int(i) >= len(cands) || ctx.Err() != nil {
					return
				}
				c := cells[cands[i]]
				ext[len(sts)] = quality.CRLBStation{Pos: geo.ToECEF(c.lat, c.lon, 50), SigmaS: sigS}
				ix := quality.NewCRLBIndex(ext, p)
				site := improveSite{Lat: round4(c.lat), Lon: round4(c.lon)}
				before, after = before[:0], after[:0]
				ck := key(c.lat, c.lon)
				nLat := int(math.Ceil(reachDeg / step))
				nLon := int(math.Ceil(reachDeg / math.Max(0.2, math.Cos(c.lat*math.Pi/180)) / step))
				for di := -nLat; di <= nLat; di++ {
					for dj := -nLon; dj <= nLon; dj++ {
						j, ok := idx[[2]int{ck[0] + di, ck[1] + dj}]
						if !ok {
							continue
						}
						e := &cells[j]
						if geoKm(c.lat, c.lon, e.lat, e.lon) > improveRangeKm {
							continue
						}
						sa, _, okA := ix.SigmaAt(e.lat, e.lon, alt)
						okA = okA && sa < 2000
						g := improveUtil(sa, okA) - improveUtil(e.before, e.ok)
						if g > 0 {
							site.Gain += g * e.area
						}
						if okA && !e.ok {
							site.NewCells++
						}
						if okA && sa <= 50 && (!e.ok || e.before > 50) {
							site.NewA++
						}
						if okA && sa <= 150 && (!e.ok || e.before > 150) {
							site.NewB++
						}
						if okA && e.ok && sa < 0.9*e.before {
							site.Better++
						}
						if e.ok {
							before = append(before, e.before)
						}
						if okA {
							after = append(after, sa)
						}
					}
				}
				site.MedianBeforeM, site.MedianAfterM = medianOf(before), medianOf(after)
				site.Gain = math.Round(site.Gain*10) / 10
				sites[i] = site
			}
		}()
	}
	wg.Wait()
	best := 0.0
	for _, st := range sites {
		best = math.Max(best, st.Gain)
	}
	if best <= 0 {
		res.Reason = "no candidate site improves the expected accuracy at this height"
	}
	for _, st := range sites {
		if best > 0 && st.Gain > 0 {
			res.Cells = append(res.Cells, improveCell{Lat: st.Lat, Lon: st.Lon, Score: math.Round(st.Gain/best*1000) / 1000})
		}
	}
	slices.SortFunc(sites, func(a, b improveSite) int {
		switch {
		case a.Gain > b.Gain:
			return -1
		case a.Gain < b.Gain:
			return 1
		case a.Lat != b.Lat:
			if a.Lat < b.Lat {
				return -1
			}
			return 1
		case a.Lon < b.Lon:
			return -1
		case a.Lon > b.Lon:
			return 1
		}
		return 0
	})
	// The best sites, at least 1.5 steps apart (neighbouring cells of one
	// good area are one suggestion).
	for _, st := range sites {
		if len(res.Sites) >= improveTop || !(st.Gain > 0) {
			break
		}
		near := false
		for _, o := range res.Sites {
			if math.Abs(o.Lat-st.Lat) < 1.5*step && math.Abs(o.Lon-st.Lon) < 1.5*step/math.Max(0.2, math.Cos(st.Lat*math.Pi/180)) {
				near = true
				break
			}
		}
		if near {
			continue
		}
		st.Score = math.Round(st.Gain/best*1000) / 1000
		res.Sites = append(res.Sites, st)
	}
	res.ElapsedMs = time.Since(t0).Milliseconds()
	return res
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	slices.Sort(v)
	return rnd1(v[len(v)/2])
}

// geoKm is the great-circle distance (spherical, km).
func geoKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0088
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := p2-p1, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}
