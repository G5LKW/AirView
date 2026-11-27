package analytics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"airview/internal/geo"
	"airview/internal/stat"
)

// GuessEstimate is one estimate of the receiver position.
type GuessEstimate struct {
	// Method: "centroid" | "signal" | "floor" | "symmetry" | "fused".
	Method string  `json:"method"`
	Label  string  `json:"label"`
	OK     bool    `json:"ok"`
	Reason string  `json:"reason,omitempty"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	// UncertaintyKm is a heuristic 1-sigma radius (from the sharpness of the
	// method's cost minimum or the scatter of its input).
	UncertaintyKm Num `json:"uncertaintyKm"`
	// ErrorKm is the distance to the configured receiver position (null
	// when none is configured).
	ErrorKm Num `json:"errorKm"`
	Weight  Num `json:"weight"` // share in the fused estimate, 0..1
	// R95Km is the radius of the 95 % accuracy circle (Locate Me v2 and the
	// fused estimate it makes; null for the classic heuristic methods).
	R95Km  Num    `json:"r95Km,omitempty"`
	Detail string `json:"detail"`
}

// CostSurface is a coarse grid of one method's cost function around the
// fused estimate, for a map visualisation. Cost[row*Cols+col] belongs to
// latitude South + row*DLat and longitude West + col*DLon and is
// normalised to 0 (best) .. 1 (worst on the grid).
type CostSurface struct {
	Method string  `json:"method"`
	South  float64 `json:"south"`
	West   float64 `json:"west"`
	DLat   float64 `json:"dLat"`
	DLon   float64 `json:"dLon"`
	Rows   int     `json:"rows"`
	Cols   int     `json:"cols"`
	Cost   Nums    `json:"cost"`
	MinLat float64 `json:"minLat"` // grid node with the lowest cost
	MinLon float64 `json:"minLon"`
}

// Guess is the result of Analyzer.GuessLocation.
type Guess struct {
	Ant        string          `json:"ant"`
	Cells      int             `json:"cells"` // heat cells available
	Positions  int64           `json:"positions"`
	OK         bool            `json:"ok"`
	Reason     string          `json:"reason,omitempty"`
	Estimates  []GuessEstimate `json:"estimates"`
	Fused      *GuessEstimate  `json:"fused,omitempty"`
	Located    bool            `json:"located"`
	Configured *LatLon         `json:"configured,omitempty"`
	Formatted  string          `json:"formatted,omitempty"`  // fused position, decimal degrees
	Maidenhead string          `json:"maidenhead,omitempty"` // 6-character locator
	Surface    *CostSurface    `json:"surface,omitempty"`
	Notes      []string        `json:"notes"`
	// Locate2 is the full Locate Me v2 result when it produced the fused
	// estimate (terms, levels, error against the receiver's GNSS position).
	Locate2 *Locate2Result `json:"locate2,omitempty"`
}

const (
	guessMinCells   = 12
	guessFitCells   = 2500 // cells used by the optimisers
	guessSurfaceN   = 33   // cost-surface nodes per side
	effEarthKm      = geo.EarthRadiusKm * 4 / 3
	guessFloorTau   = 0.1 // quantile of the coverage-floor fit
	guessSectors    = 72
	guessSectorQuan = 0.97
)

// gcell is a heat cell prepared for the optimisers.
type gcell struct {
	x, y, z  float64 // unit vector
	lat, lon float64
	count    float64
	rssi     float64 // NaN unknown
	wRSSI    float64
	altKm    float64 // mid altitude, km
	minAltM  float64 // NaN unknown
}

func unitVec(lat, lon float64) (x, y, z float64) {
	sl, cl := math.Sincos(lat * math.Pi / 180)
	so, co := math.Sincos(lon * math.Pi / 180)
	return cl * co, cl * so, sl
}

// distKm is the great-circle distance from the unit vector (x, y, z).
func (c *gcell) distKm(x, y, z float64) float64 {
	dx, dy, dz := c.x-x, c.y-y, c.z-z
	h := math.Sqrt(dx*dx+dy*dy+dz*dz) / 2
	if h > 1 {
		h = 1
	}
	return 2 * geo.EarthRadiusKm * math.Asin(h)
}

// GuessLocation estimates the receiver position from the received data
// only (the all-time heat grid of antenna ant, "" = combined), with four
// independent methods and a fused answer:
//
//   - centroid: weighted centroid of the heat grid;
//   - signal:   the point that best explains the mean RSSI of every cell
//     with the inverse-square law rssi = offset - 20*log10(slant range)
//     (robust L1 fit, Nelder-Mead from several starts);
//   - floor:    the point under which the lowest-altitude-seen surface
//     follows the radio-horizon cone min_alt >= d^2 / (2 k R)
//     (quantile fit of the coverage floor);
//   - symmetry: the centre of the circle fitted to the outer edge of the
//     coverage.
//
// The configured position is never used for the estimates, only to report
// the error of each.
//
// Since Locate Me v2 (locate2*.go) the answer also carries the v2 estimate
// from the station's reception samples: when it succeeds it becomes the
// fused (best) estimate with its 95 % accuracy circle, and the four
// classic methods stay in the list for comparison (DESIGN §10.4).
func (a *Analyzer) GuessLocation(ctx context.Context, ant string) (*Guess, error) {
	g, err := a.guessClassic(ctx, ant)
	if err != nil {
		return nil, err
	}
	l2, l2err := a.LocateMe(ctx, LocateRequest{})
	if l2err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return withLocate2(g, l2, l2err), nil
}

// withLocate2 returns a copy of the classic guess g (shared, read-only)
// with the Locate Me v2 result merged in.
func withLocate2(g *Guess, l2 *Locate2Result, l2err error) *Guess {
	if l2err == nil && (l2 == nil || l2.Samples == 0) {
		return g // no reception samples at all: the classic answer only
	}
	out := *g
	out.Estimates = make([]GuessEstimate, len(g.Estimates), len(g.Estimates)+1)
	copy(out.Estimates, g.Estimates)
	out.Notes = append([]string(nil), g.Notes...)
	est := GuessEstimate{Method: "locate2", Label: "Locate Me v2 (horizon, skyline, signal)",
		UncertaintyKm: nan(), ErrorKm: nan(), R95Km: nan()}
	switch {
	case l2err != nil:
		est.Reason = l2err.Error()
	case !l2.OK:
		est.Reason = l2.Reason
	default:
		est.OK, est.Lat, est.Lon = true, l2.Lat, l2.Lon
		est.UncertaintyKm, est.R95Km = l2.SigmaKm, l2.R95Km
		est.Weight = 1
		est.Detail = locate2Detail(l2)
		if out.Configured != nil {
			est.ErrorKm = Num(round(geo.HaversineKm(out.Configured.Lat, out.Configured.Lon, l2.Lat, l2.Lon), 3))
		}
	}
	// v2 becomes the answer unless it is still vaguer than the classic
	// fusion (very little data so far): then it is listed with the others.
	if !est.OK || (g.Fused != nil && float64(est.UncertaintyKm) > float64(g.Fused.UncertaintyKm)) {
		if est.OK {
			est.Weight = 0
			out.Notes = append(out.Notes, "Locate Me v2 does not have enough traffic yet to beat the classic methods; it takes over once its uncertainty is the smaller one.")
		}
		out.Estimates = append(out.Estimates, est)
		return &out
	}
	out.Estimates = append(out.Estimates, est)
	f := fuseWithLocate2(out.Estimates, &out.Notes)
	if out.Configured != nil {
		f.ErrorKm = Num(round(geo.HaversineKm(out.Configured.Lat, out.Configured.Lon, f.Lat, f.Lon), 3))
	}
	out.Fused = &f
	out.OK, out.Reason = true, ""
	out.Formatted = geo.FormatDecimal(f.Lat, f.Lon, 4)
	out.Maidenhead = geo.Maidenhead(f.Lat, f.Lon, 6)
	if l2.Surface != nil {
		out.Surface = l2.Surface
	}
	out.Locate2 = l2
	out.Notes = append(out.Notes, l2.Notes...)
	out.Notes = append(out.Notes, fmt.Sprintf("The accuracy circle (radius %.1f km) holds the station with about 95 %% probability; it includes an allowance for model errors on a real site (buildings, trees, refraction, the antenna), so it does not shrink below about 5 km.", float64(f.R95Km)))
	return &out
}

// guessClassicRejectSigma: a classic estimate further from Locate Me v2 than
// this many combined sigmas gets no weight in the fusion.
const guessClassicRejectSigma = 3.0

// fuseWithLocate2 combines the Locate Me v2 estimate (the last element of
// est, OK) with the classic ones by inverse-variance weighting and sets
// every estimate's Weight. The classic uncertainties are heuristic and
// usually 5-20 times v2's, so they carry a few per cent at most; an
// estimate that disagrees with v2 by more than guessClassicRejectSigma
// combined sigmas (the centroid pulled towards airways, a signal fit on
// compressed levels) is left out. The accuracy circle is v2's, widened by
// the shift of the fused point: the classic methods see the same traffic,
// so their errors are not independent of v2's and must not narrow it.
func fuseWithLocate2(est []GuessEstimate, notes *[]string) GuessEstimate {
	v2 := &est[len(est)-1]
	sv := float64(v2.UncertaintyKm)
	wv := 1 / (sv * sv)
	vx, vy, vz := unitVec(v2.Lat, v2.Lon)
	x, y, z, sw := wv*vx, wv*vy, wv*vz, wv
	ws := make([]float64, len(est))
	ws[len(est)-1] = wv
	var used, dropped []string
	for i := 0; i < len(est)-1; i++ {
		e := &est[i]
		e.Weight = 0
		s := float64(e.UncertaintyKm)
		if !e.OK || !(s > 0) {
			continue
		}
		d := geo.HaversineKm(v2.Lat, v2.Lon, e.Lat, e.Lon)
		if d > guessClassicRejectSigma*math.Hypot(sv, s) {
			dropped = append(dropped, fmt.Sprintf("%s (%.0f km off)", e.Method, d))
			continue
		}
		w := 1 / (s * s)
		ux, uy, uz := unitVec(e.Lat, e.Lon)
		x, y, z, sw = x+w*ux, y+w*uy, z+w*uz, sw+w
		ws[i] = w
		used = append(used, e.Method)
	}
	for i := range est {
		est[i].Weight = Num(round(ws[i]/sw, 4))
	}
	n := math.Sqrt(x*x + y*y + z*z)
	f := *v2
	f.Method, f.Label = "fused", "Best estimate (Locate Me v2)"
	f.Lat = round(math.Asin(clamp(z/n, -1, 1))*180/math.Pi, 5)
	f.Lon = round(math.Atan2(y, x)*180/math.Pi, 5)
	f.Weight = 1
	shift := geo.HaversineKm(v2.Lat, v2.Lon, f.Lat, f.Lon)
	f.R95Km = Num(round(float64(v2.R95Km)+shift, 3))
	f.UncertaintyKm = Num(round(math.Hypot(sv, shift), 3))
	classic := 1 - wv/sw
	switch {
	case len(used) > 0:
		f.Detail = fmt.Sprintf("Locate Me v2: %s. Classic methods (%s) weigh in by their uncertainty with %.1f %% in total", v2.Detail, strings.Join(used, ", "), 100*classic)
		*notes = append(*notes, fmt.Sprintf("Weights: Locate Me v2 %.1f %%, classic methods %.1f %%. The weights follow the uncertainties (1/σ²): v2's σ of %.1f km is much smaller than the classic methods' tens of km, so it dominates.", 100*wv/sw, 100*classic, sv))
	default:
		f.Detail = "Locate Me v2: " + v2.Detail + ". No classic method agreed with it well enough to weigh in"
		*notes = append(*notes, "Weights: Locate Me v2 100 %; no classic method is close enough to it (within 3 combined σ) to weigh in.")
	}
	if len(dropped) > 0 {
		*notes = append(*notes, fmt.Sprintf("Left out of the fusion because they disagree with Locate Me v2 by more than 3 combined σ: %s.", strings.Join(dropped, ", ")))
	}
	return f
}

// locate2Detail summarises which terms carried a Locate Me v2 estimate.
func locate2Detail(r *Locate2Result) string {
	var used []string
	for _, t := range r.Terms {
		if t.Used {
			used = append(used, t.Name)
		}
	}
	src := "reception samples"
	if r.Source == "positions" {
		src = "stored trail positions"
	}
	terr := "smooth earth"
	if r.Terrain {
		terr = "terrain model"
	}
	return fmt.Sprintf("%d %s of %d aircraft (%d passes); terms: %s; %s; 95 %% radius %.1f km",
		r.Samples, src, r.Aircraft, r.Sessions, strings.Join(used, ", "), terr, float64(r.R95Km))
}

// guessClassic is the classic four-method guess (cached).
func (a *Analyzer) guessClassic(ctx context.Context, ant string) (*Guess, error) {
	cfg := a.cfg()
	return cached(ctx, a, a.key("guess", ant, cfg), ttlGuess, func(ctx context.Context) (*Guess, error) {
		cells, err := a.loadHeat(ctx, ant)
		if err != nil {
			return nil, err
		}
		g := guessFromCells(cells)
		g.Ant = ant
		s := siteOf(cfg, ant)
		g.Located = s.located
		if s.located {
			g.Configured = s.pos()
			set := func(e *GuessEstimate) {
				if e != nil && e.OK {
					e.ErrorKm = Num(geo.HaversineKm(s.lat, s.lon, e.Lat, e.Lon))
				}
			}
			for i := range g.Estimates {
				set(&g.Estimates[i])
			}
			set(g.Fused)
		}
		return g, nil
	})
}

func guessFromCells(raw []heatCell) *Guess {
	g := &Guess{Cells: len(raw), Estimates: []GuessEstimate{}, Notes: []string{}}
	for i := range raw {
		g.Positions += raw[i].count
	}
	if len(raw) < guessMinCells {
		g.Reason = fmt.Sprintf("not enough data yet: %d map cells with positions (at least %d are needed)", len(raw), guessMinCells)
		return g
	}
	// Thin the grid for the optimisers.
	work := raw
	stepUsed := 1
	for _, step := range [...]int{1, 2, 3, 4, 6, 8, 12, 16} {
		work = aggregateHeat(raw, step, nil)
		stepUsed = step
		if len(work) <= guessFitCells {
			break
		}
	}
	cellKm := geo.HeatCellDeg * float64(stepUsed) * 111.2
	cells := make([]gcell, 0, len(work))
	for i := range work {
		c := &work[i]
		if c.count <= 0 {
			continue
		}
		q := gcell{lat: c.lat, lon: c.lon, count: float64(c.count), rssi: math.NaN(), minAltM: math.NaN(), altKm: 10}
		q.x, q.y, q.z = unitVec(c.lat, c.lon)
		if c.rssiN >= 3 && isNum(c.rssi) {
			q.rssi = c.rssi
			q.wRSSI = math.Sqrt(math.Min(float64(c.rssiN), 400))
		}
		switch {
		case isNum(c.minAlt) && isNum(c.maxAlt):
			q.altKm = geo.FeetToMetres((c.minAlt+c.maxAlt)/2) / 1000
		case isNum(c.maxAlt):
			q.altKm = geo.FeetToMetres(c.maxAlt) / 1000
		}
		if isNum(c.minAlt) && c.count >= 2 {
			q.minAltM = geo.FeetToMetres(c.minAlt)
		}
		cells = append(cells, q)
	}

	// (a) centroid.
	cen := centroidEstimate(cells)
	g.Estimates = append(g.Estimates, cen)
	rms := float64(cen.UncertaintyKm) / 0.3
	starts := [][2]float64{{cen.Lat, cen.Lon}}
	for _, b := range [...]float64{0, 90, 180, 270} {
		la, lo := geo.Destination(cen.Lat, cen.Lon, b, 0.5*rms)
		starts = append(starts, [2]float64{la, lo})
	}
	// The loudest cell is usually close to the antenna.
	best := -1
	for i := range cells {
		if isNum(cells[i].rssi) && cells[i].wRSSI >= 2 && (best < 0 || cells[i].rssi > cells[best].rssi) {
			best = i
		}
	}
	if best >= 0 {
		starts = append(starts, [2]float64{cells[best].lat, cells[best].lon})
	}

	// (b) signal-strength fit, within the coverage.
	box := coverageBox(cells, cen.Lon)
	sigCost, nSig := signalCost(cells)
	sig := GuessEstimate{Method: "signal", Label: "Signal-strength fit", UncertaintyKm: Num(math.NaN()), ErrorKm: Num(math.NaN())}
	if nSig < 25 {
		sig.Reason = fmt.Sprintf("only %d cells with signal levels (25 needed)", nSig)
	} else if la, lo, f, ok := minimise2D(sigCost, starts, box); ok {
		sig.OK, sig.Lat, sig.Lon = true, la, lo
		u := profileRadius(sigCost, la, lo, f, 2*f/math.Sqrt(float64(nSig))+0.05)
		sig.UncertaintyKm = Num(math.Max(u, math.Max(2, cellKm/2)))
		sig.Detail = fmt.Sprintf("%d cells; mean deviation from the inverse-square law %.1f dB", nSig, f)
	} else {
		sig.Reason = "the fit did not converge"
	}
	g.Estimates = append(g.Estimates, sig)

	// (c) coverage-floor fit.
	floorCost, nFloor := floorCostFn(cells)
	fl := GuessEstimate{Method: "floor", Label: "Coverage-floor (radio horizon) fit", UncertaintyKm: Num(math.NaN()), ErrorKm: Num(math.NaN())}
	if nFloor < 25 {
		fl.Reason = fmt.Sprintf("only %d cells with altitudes (25 needed)", nFloor)
	} else {
		st := starts
		if sig.OK {
			st = append(st, [2]float64{sig.Lat, sig.Lon})
		}
		if la, lo, f, ok := minimise2D(floorCost, st, box); ok {
			fl.OK, fl.Lat, fl.Lon = true, la, lo
			u := profileRadius(floorCost, la, lo, f, 2*f/math.Sqrt(float64(nFloor))+5)
			fl.UncertaintyKm = Num(math.Max(u, math.Max(3, cellKm/2)))
			fl.Detail = fmt.Sprintf("%d cells; lowest altitudes follow the horizon cone with a mean quantile loss of %.0f m", nFloor, f)
		} else {
			fl.Reason = "the fit did not converge"
		}
	}
	g.Estimates = append(g.Estimates, fl)

	// (d) range symmetry.
	g.Estimates = append(g.Estimates, symmetryEstimate(cells, cen.Lat, cen.Lon, cellKm))

	g.Fused = fuse(g.Estimates, &g.Notes)
	if g.Fused == nil {
		g.Reason = "no method produced an estimate"
		return g
	}
	g.OK = true
	g.Formatted = geo.FormatDecimal(g.Fused.Lat, g.Fused.Lon, 4)
	g.Maidenhead = geo.Maidenhead(g.Fused.Lat, g.Fused.Lon, 6)
	switch {
	case sig.OK:
		g.Surface = costSurface("signal", sigCost, g)
	case fl.OK:
		g.Surface = costSurface("floor", floorCost, g)
	}
	if len(raw) < 200 {
		g.Notes = append(g.Notes, fmt.Sprintf("Only %d map cells have data so far; the estimate improves as traffic fills in all directions.", len(raw)))
	}
	g.Notes = append(g.Notes, "Uncertainty radii are heuristic: they describe how sharply each method pins the position down, not a guaranteed error bound.")
	return g
}

// centroidEstimate is the log-count weighted centroid of the cells.
func centroidEstimate(cells []gcell) GuessEstimate {
	var x, y, z, sw float64
	for i := range cells {
		w := math.Log1p(cells[i].count)
		x += w * cells[i].x
		y += w * cells[i].y
		z += w * cells[i].z
		sw += w
	}
	e := GuessEstimate{Method: "centroid", Label: "Centroid of all positions", UncertaintyKm: Num(math.NaN()), ErrorKm: Num(math.NaN())}
	n := math.Sqrt(x*x + y*y + z*z)
	if !(n > 0) || !(sw > 0) {
		e.Reason = "no positions"
		return e
	}
	x, y, z = x/n, y/n, z/n
	e.Lat = math.Asin(clamp(z, -1, 1)) * 180 / math.Pi
	e.Lon = math.Atan2(y, x) * 180 / math.Pi
	var s2 float64
	for i := range cells {
		d := cells[i].distKm(x, y, z)
		s2 += math.Log1p(cells[i].count) * d * d
	}
	rms := math.Sqrt(s2 / sw)
	e.OK = true
	e.UncertaintyKm = Num(math.Max(15, 0.3*rms))
	e.Detail = fmt.Sprintf("%d cells, r.m.s. radius %.0f km; biased towards busy airways and away from blocked sectors", len(cells), rms)
	return e
}

// weightedQuantileInPlace sorts idx by v and returns the weighted quantile.
type byVal struct {
	v, w []float64
}

func (b byVal) Len() int           { return len(b.v) }
func (b byVal) Less(i, j int) bool { return b.v[i] < b.v[j] }
func (b byVal) Swap(i, j int)      { b.v[i], b.v[j] = b.v[j], b.v[i]; b.w[i], b.w[j] = b.w[j], b.w[i] }

func weightedQuantileInPlace(v, w []float64, q float64) float64 {
	sort.Sort(byVal{v, w})
	var sw float64
	for _, x := range w {
		sw += x
	}
	target, cum := q*sw, 0.0
	for i := range v {
		cum += w[i]
		if cum >= target {
			return v[i]
		}
	}
	return v[len(v)-1]
}

// Radio-range constraint of the signal fit: an aircraft at altitude h can be
// heard up to its radio horizon plus that of an antenna of at most
// guessMaxAntKm MSL, times guessRangeSlack plus guessRangeExtraKm (ducting,
// position errors). Every km a cell lies beyond costs guessRangePenDB/100 dB
// of the cell's weight.
const (
	guessMaxAntKm     = 0.5
	guessRangeSlack   = 1.1
	guessRangeExtraKm = 30
	guessRangePenDB   = 1.0
)

// guessMaxRangeKm is the farthest a cell at altitude altKm can be heard.
func guessMaxRangeKm(altKm float64) float64 {
	h := math.Max(altKm, 0)
	return (math.Sqrt(2*effEarthKm*h)+math.Sqrt(2*effEarthKm*guessMaxAntKm))*guessRangeSlack + guessRangeExtraKm
}

// signalCost returns the cost function of the signal-strength fit: the
// weighted mean absolute deviation of rssi + 20*log10(slant) from its
// weighted median (the offset is thereby fitted in closed form), plus a
// penalty for cells that would lie beyond radio range. Without the penalty
// levels that hardly fall with distance (gain compression, a detection
// floor, feeds with a flat level) make "very far away" the best fit: all
// slant ranges then look alike and the deviation tends to the plain spread
// of the levels (field bug r101: 1650 km off).
func signalCost(cells []gcell) (func(lat, lon float64) float64, int) {
	var idx []int
	for i := range cells {
		if isNum(cells[i].rssi) && cells[i].wRSSI > 0 {
			idx = append(idx, i)
		}
	}
	n := len(idx)
	v, w, maxR := make([]float64, n), make([]float64, n), make([]float64, n)
	for k, i := range idx {
		maxR[k] = guessMaxRangeKm(cells[i].altKm)
	}
	return func(lat, lon float64) float64 {
		if n == 0 || math.Abs(lat) > 89.9 {
			return math.Inf(1)
		}
		x, y, z := unitVec(lat, lon)
		var sw, pen float64
		for k, i := range idx {
			c := &cells[i]
			d := c.distKm(x, y, z)
			slant2 := d*d*(1+c.altKm/geo.EarthRadiusKm) + c.altKm*c.altKm
			if slant2 < 1 {
				slant2 = 1
			}
			v[k] = c.rssi + 10*math.Log10(slant2)
			w[k] = c.wRSSI
			sw += c.wRSSI
			if ex := d - maxR[k]; ex > 0 {
				pen += c.wRSSI * ex * guessRangePenDB / 100
			}
		}
		off := weightedQuantileInPlace(v, w, 0.5)
		var s float64
		for k := range v {
			s += w[k] * math.Abs(v[k]-off)
		}
		return (s + pen) / sw
	}, n
}

// floorCostFn returns the cost function of the coverage-floor fit: the
// mean pinball (quantile) loss, in metres, of min_alt - d^2/(2kR) around its
// own lower quantile. At the true position the lowest altitudes seen rise
// with distance exactly like the radio horizon, so few cells fall below the
// cone and the loss is smallest.
func floorCostFn(cells []gcell) (func(lat, lon float64) float64, int) {
	var idx []int
	for i := range cells {
		if isNum(cells[i].minAltM) {
			idx = append(idx, i)
		}
	}
	n := len(idx)
	v, w := make([]float64, n), make([]float64, n)
	return func(lat, lon float64) float64 {
		if n == 0 || math.Abs(lat) > 89.9 {
			return math.Inf(1)
		}
		x, y, z := unitVec(lat, lon)
		for k, i := range idx {
			c := &cells[i]
			d := c.distKm(x, y, z)
			v[k] = c.minAltM - d*d/(2*effEarthKm)*1000
			w[k] = 1
		}
		a := weightedQuantileInPlace(v, w, guessFloorTau)
		var s float64
		for k := range v {
			if r := v[k] - a; r >= 0 {
				s += guessFloorTau * r
			} else {
				s -= (1 - guessFloorTau) * r
			}
		}
		return s / float64(n)
	}, n
}

// guessBox is the search region of the optimisers: the bounding box of the
// coverage (the station is inside what it hears), longitudes unwrapped
// around lon0.
type guessBox struct {
	south, north, west, east, lon0 float64
}

// coverageBox returns the bounding box of the cells around lon0, padded by
// half a degree.
func coverageBox(cells []gcell, lon0 float64) guessBox {
	b := guessBox{south: 90, north: -90, west: 180, east: -180, lon0: lon0}
	for i := range cells {
		d := geo.AngleDiff(cells[i].lon, lon0)
		b.south, b.north = math.Min(b.south, cells[i].lat), math.Max(b.north, cells[i].lat)
		b.west, b.east = math.Min(b.west, d), math.Max(b.east, d)
	}
	b.south, b.north = math.Max(-89.5, b.south-0.5), math.Min(89.5, b.north+0.5)
	b.west, b.east = lon0+b.west-0.5, lon0+b.east+0.5
	return b
}

// minimise2D runs Nelder-Mead on (lat, lon) from several starts, within box.
func minimise2D(cost func(lat, lon float64) float64, starts [][2]float64, box guessBox) (lat, lon, f float64, ok bool) {
	f = math.Inf(1)
	obj := func(x []float64) float64 { return cost(x[0], x[1]) }
	for _, s := range starts {
		cl := math.Max(0.2, math.Cos(s[0]*math.Pi/180))
		s0 := [2]float64{clamp(s[0], box.south, box.north), clamp(box.lon0+geo.AngleDiff(s[1], box.lon0), box.west, box.east)}
		r, err := stat.NelderMead(obj, []float64{s0[0], s0[1]}, &stat.NMOptions{
			Step:  []float64{0.4, 0.4 / cl},
			Lower: []float64{box.south, box.west}, Upper: []float64{box.north, box.east},
			TolF: 1e-7, TolX: 1e-5, MaxIter: 400, Restarts: 1,
		})
		if err != nil || len(r.X) != 2 || !isNum(r.F) {
			continue
		}
		if r.F < f {
			lat, lon, f, ok = r.X[0], geo.NormalizeLon(r.X[1]), r.F, true
		}
	}
	return lat, lon, f, ok
}

// profileRadius returns the distance from the optimum at which the cost has
// risen by delta in every direction (the "width" of the minimum).
func profileRadius(cost func(lat, lon float64) float64, lat, lon, best, delta float64) float64 {
	prevR, prevEx := 0.0, 0.0
	for rho := 1.0; rho <= 800; rho *= 2 {
		m := math.Inf(1)
		for b := 0.0; b < 360; b += 45 {
			la, lo := geo.Destination(lat, lon, b, rho)
			m = math.Min(m, cost(la, lo))
		}
		ex := m - best
		if ex >= delta {
			if ex > prevEx {
				return prevR + (rho-prevR)*(delta-prevEx)/(ex-prevEx)
			}
			return rho
		}
		prevR, prevEx = rho, math.Max(ex, 0)
	}
	return 800
}

// symmetryEstimate fits a circle to the outer edge of the coverage.
func symmetryEstimate(cells []gcell, lat0, lon0, cellKm float64) GuessEstimate {
	e := GuessEstimate{Method: "symmetry", Label: "Centre of the coverage edge", UncertaintyKm: Num(math.NaN()), ErrorKm: Num(math.NaN())}
	if len(cells) < 60 {
		e.Reason = fmt.Sprintf("only %d cells (60 needed)", len(cells))
		return e
	}
	const kmPerDeg = geo.EarthRadiusKm * math.Pi / 180
	cl := math.Max(0.05, math.Cos(lat0*math.Pi/180))
	// Tangent-plane coordinates (east, north) in km around the centroid.
	px, py := make([]float64, len(cells)), make([]float64, len(cells))
	for i := range cells {
		px[i] = geo.AngleDiff(cells[i].lon, lon0) * kmPerDeg * cl
		py[i] = (cells[i].lat - lat0) * kmPerDeg
	}
	cx, cy := 0.0, 0.0
	var rmsRes, radius float64
	nSect := 0
	type pt struct{ d, x, y float64 }
	for iter := 0; iter < 4; iter++ {
		var sect [guessSectors][]pt
		for i := range cells {
			dx, dy := px[i]-cx, py[i]-cy
			b := math.Atan2(dx, dy) * 180 / math.Pi
			if b < 0 {
				b += 360
			}
			k := int(b/(360.0/guessSectors)) % guessSectors
			sect[k] = append(sect[k], pt{math.Hypot(dx, dy), px[i], py[i]})
		}
		var X [][]float64
		var zv []float64
		var edge []pt
		for k := range sect {
			s := sect[k]
			if len(s) < 3 {
				continue
			}
			sort.Slice(s, func(i, j int) bool { return s[i].d < s[j].d })
			p := s[int(guessSectorQuan*float64(len(s)-1))]
			edge = append(edge, p)
			X = append(X, []float64{p.x, p.y})
			zv = append(zv, -(p.x*p.x + p.y*p.y))
		}
		nSect = len(edge)
		if nSect < guessSectors/3 {
			e.Reason = fmt.Sprintf("coverage spans only %d of %d directions", nSect, guessSectors)
			return e
		}
		// Kasa circle fit: x^2 + y^2 + D x + E y + F = 0.
		fit, err := stat.OLS(X, zv, true)
		if err != nil || len(fit.Coef) < 3 {
			e.Reason = "circle fit failed"
			return e
		}
		cx, cy = -fit.Coef[1]/2, -fit.Coef[2]/2
		r2 := cx*cx + cy*cy - fit.Coef[0]
		if !(r2 > 0) {
			e.Reason = "circle fit failed"
			return e
		}
		radius = math.Sqrt(r2)
		var ss float64
		for _, p := range edge {
			d := math.Hypot(p.x-cx, p.y-cy) - radius
			ss += d * d
		}
		rmsRes = math.Sqrt(ss / float64(nSect))
	}
	e.Lat = lat0 + cy/kmPerDeg
	e.Lon = geo.NormalizeLon(lon0 + cx/(kmPerDeg*cl))
	if math.Abs(e.Lat) > 89.5 || !isNum(e.Lat) || !isNum(e.Lon) {
		e.Reason = "circle fit failed"
		return e
	}
	e.OK = true
	// The centre of an irregular edge moves by about the edge roughness;
	// averaging over the sectors helps only partly (the irregularities are
	// sector-wide: terrain, coastlines, airways).
	e.UncertaintyKm = Num(math.Max(8, 3*rmsRes/math.Sqrt(float64(nSect))+rmsRes/4+cellKm))
	e.Detail = fmt.Sprintf("circle of %.0f km radius through the coverage edge in %d directions, edge roughness %.0f km", radius, nSect, rmsRes)
	return e
}

// fuse combines the estimates by inverse-variance weighting after dropping
// an estimate that disagrees grossly with the others.
func fuse(est []GuessEstimate, notes *[]string) *GuessEstimate {
	var use []*GuessEstimate
	for i := range est {
		if est[i].OK && float64(est[i].UncertaintyKm) > 0 {
			use = append(use, &est[i])
		}
	}
	if len(use) == 0 {
		return nil
	}
	combine := func(list []*GuessEstimate) (lat, lon, sigma float64) {
		var x, y, z, sw float64
		for _, e := range list {
			w := 1 / (float64(e.UncertaintyKm) * float64(e.UncertaintyKm))
			ux, uy, uz := unitVec(e.Lat, e.Lon)
			x, y, z, sw = x+w*ux, y+w*uy, z+w*uz, sw+w
		}
		n := math.Sqrt(x*x + y*y + z*z)
		lat = math.Asin(clamp(z/n, -1, 1)) * 180 / math.Pi
		lon = math.Atan2(y, x) * 180 / math.Pi
		return lat, lon, math.Sqrt(1 / sw)
	}
	// Outlier rejection: the estimate farthest (in sigmas) from the fusion
	// of the others goes when it is more than 3 combined sigmas + 20 km off.
	for len(use) > 2 {
		worst, worstScore := -1, 1.0
		for i := range use {
			rest := append(append([]*GuessEstimate{}, use[:i]...), use[i+1:]...)
			la, lo, sg := combine(rest)
			d := geo.HaversineKm(la, lo, use[i].Lat, use[i].Lon)
			lim := 3*math.Hypot(sg, float64(use[i].UncertaintyKm)) + 20
			if sc := d / lim; sc > worstScore {
				worst, worstScore = i, sc
			}
		}
		if worst < 0 {
			break
		}
		*notes = append(*notes, fmt.Sprintf("The %q estimate disagrees with the others by more than its uncertainty allows and was left out of the fused position.", use[worst].Method))
		use = append(use[:worst], use[worst+1:]...)
	}
	lat, lon, sigma := combine(use)
	var sw, ss float64
	for _, e := range use {
		w := 1 / (float64(e.UncertaintyKm) * float64(e.UncertaintyKm))
		d := geo.HaversineKm(lat, lon, e.Lat, e.Lon)
		sw += w
		ss += w * d * d
	}
	for _, e := range use {
		e.Weight = Num(1 / (float64(e.UncertaintyKm) * float64(e.UncertaintyKm)) / sw)
	}
	scatter := math.Sqrt(ss / sw)
	f := &GuessEstimate{Method: "fused", Label: "Fused estimate", OK: true, Lat: round(lat, 5), Lon: round(lon, 5),
		UncertaintyKm: Num(math.Sqrt(sigma*sigma + scatter*scatter/float64(len(use)))), ErrorKm: Num(math.NaN()), Weight: 1}
	f.Detail = fmt.Sprintf("inverse-variance combination of %d method(s); they agree to within %.0f km", len(use), scatter)
	for i := range est {
		est[i].Lat, est[i].Lon = round(est[i].Lat, 5), round(est[i].Lon, 5)
	}
	return f
}

// costSurface samples a cost function on a coarse grid around the fused
// estimate, wide enough to show every method's estimate.
func costSurface(method string, cost func(lat, lon float64) float64, g *Guess) *CostSurface {
	lat0, lon0 := g.Fused.Lat, g.Fused.Lon
	half := 60.0
	for _, e := range g.Estimates {
		if e.OK {
			half = math.Max(half, 1.5*geo.HaversineKm(lat0, lon0, e.Lat, e.Lon))
		}
	}
	half = math.Min(half, 400)
	const kmPerDeg = geo.EarthRadiusKm * math.Pi / 180
	cl := math.Max(0.1, math.Cos(lat0*math.Pi/180))
	n := guessSurfaceN
	s := &CostSurface{Method: method, Rows: n, Cols: n, Cost: make(Nums, n*n)}
	s.DLat = 2 * half / kmPerDeg / float64(n-1)
	s.DLon = 2 * half / (kmPerDeg * cl) / float64(n-1)
	s.South = lat0 - half/kmPerDeg
	s.West = lon0 - half/(kmPerDeg*cl)
	lo, hi := math.Inf(1), math.Inf(-1)
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			la, lon := s.South+float64(r)*s.DLat, s.West+float64(c)*s.DLon
			v := cost(la, lon)
			s.Cost[r*n+c] = v
			if isNum(v) {
				if v < lo {
					lo, s.MinLat, s.MinLon = v, round(la, 5), round(geo.NormalizeLon(lon), 5)
				}
				hi = math.Max(hi, v)
			}
		}
	}
	for i, v := range s.Cost {
		switch {
		case !isNum(v):
			s.Cost[i] = math.NaN()
		case hi > lo:
			s.Cost[i] = (v - lo) / (hi - lo)
		default:
			s.Cost[i] = 0
		}
	}
	s.South, s.West, s.DLat, s.DLon = round(s.South, 5), round(s.West, 5), round(s.DLat, 6), round(s.DLon, 6)
	return s
}
