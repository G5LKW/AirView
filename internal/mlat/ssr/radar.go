package ssr

import (
	"math"
	"sort"

	"airview/internal/mlat/geo"
)

// Interrogator is a configured or learned SSR radar site.
type Interrogator struct {
	Name       string
	Configured bool
	LLH        geo.LLH
	ECEF       geo.Vec3
	// CovEN is the horizontal covariance (ee, en, nn), m², of the site
	// position before the joint solve.
	CovEN   [3]float64
	SigmaUM float64 // vertical prior 1σ, m
	// Lines is the number of bearing lines (pair observations) that
	// located it; SpreadDeg their azimuth spread seen from the site.
	Lines     int
	SpreadDeg float64
	// Obs is the number of observations assigned to it.
	Obs int
	// Rotation is the antenna rotation recovered from its observations
	// (zero when none was found).
	Rotation Rotation
}

// SigmaHM returns the horizontal 1σ of the site prior.
func (r *Interrogator) SigmaHM() float64 { return math.Sqrt(math.Max(r.CovEN[0]+r.CovEN[2], 0)) }

// Gnomonic is a central (gnomonic) projection of the ellipsoid surface
// onto the tangent plane at an origin, through the earth centre. Great
// circles map to straight lines, so the aircraft that share a beam azimuth
// of a rotating radar (they lie in the radar's vertical plane at that
// azimuth, which contains the earth centre to within the ellipsoid's
// normal offset) project onto one straight line through the radar. The
// error of that approximation is below 100 m at 500 km.
type Gnomonic struct {
	F  geo.Frame
	up geo.Vec3
	k  float64
}

// NewGnomonic returns the projection about o (its height is ignored).
func NewGnomonic(o geo.LLH) Gnomonic {
	o.H = 0
	f := geo.NewFrame(o)
	up := f.Up()
	return Gnomonic{F: f, up: up, k: f.Origin.Dot(up)}
}

// Fwd projects the foot point of p (ECEF) to plane coordinates (m).
func (g *Gnomonic) Fwd(p geo.Vec3) (x, y float64, ok bool) {
	ll := geo.FromECEF(p)
	s := geo.ToECEF(ll.Lat, ll.Lon, 0)
	den := s.Dot(g.up)
	if den <= 0.2*g.k {
		return 0, 0, false
	}
	e := g.F.ToENU(s.Scale(g.k / den))
	return e.X, e.Y, true
}

// Inv returns the surface point of plane coordinates (x, y) at height h.
func (g *Gnomonic) Inv(x, y, h float64) geo.LLH {
	q := g.F.FromENU(geo.Vec3{X: x, Y: y})
	const a, b = geo.WGS84A, geo.WGS84B
	s := 1 / math.Sqrt((q.X*q.X+q.Y*q.Y)/(a*a)+q.Z*q.Z/(b*b))
	ll := geo.FromECEF(q.Scale(s))
	ll.H = h
	return ll
}

// planePts holds the projected aircraft positions of each observation.
type planePts struct {
	g      Gnomonic
	x1, y1 []float64
	x2, y2 []float64
	ok     []bool
}

func projectObs(obs []PairObs) planePts {
	var c geo.Vec3
	for i := range obs {
		c = c.Add(obs[i].PK).Add(obs[i].PJ)
	}
	o := geo.LLH{}
	if len(obs) > 0 {
		o = geo.FromECEF(c.Scale(1 / float64(2*len(obs))))
	}
	pp := planePts{g: NewGnomonic(o)}
	n := len(obs)
	pp.x1, pp.y1, pp.x2, pp.y2 = make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	pp.ok = make([]bool, n)
	for i := range obs {
		a, b, ok1 := pp.g.Fwd(obs[i].PK)
		c, d, ok2 := pp.g.Fwd(obs[i].PJ)
		pp.x1[i], pp.y1[i], pp.x2[i], pp.y2[i], pp.ok[i] = a, b, c, d, ok1 && ok2
	}
	return pp
}

// lineSigma is the 1σ miss distance of the bearing line of a pair at
// ranges r1, r2 from a radar of beam width bw (rad): both aircraft lie
// anywhere inside the beam, so the line through them misses the radar by
// about r1·r2·Δα/|r2 - r1| with Δα uniform over the beam.
func lineSigma(r1, r2, bw, floor float64) float64 {
	d := math.Abs(r2 - r1)
	if d < 1 {
		d = 1
	}
	return math.Max(floor, r1*r2*bw/d/math.Sqrt(6))
}

// LocateInterrogators finds the interrogators from the bearing lines of
// the pair observations (DESIGN §14 phase 5 research, SSR-RESEARCH.md §4):
// a Hough vote over a square of side 2·RadarRegionM about the aircraft
// centroid, peaks with at least MinRadarLines lines, refined by a robust
// weighted least-squares intersection of the lines. It returns the
// projection used, for Assign.
func LocateInterrogators(obs []PairObs, cfg Config) ([]Interrogator, Gnomonic) {
	cfg.Normalize()
	pp := projectObs(obs)
	R, cell := cfg.RadarRegionM, cfg.RadarCellM
	G := int(math.Ceil(2 * R / cell))
	bw := cfg.BeamwidthDeg * math.Pi / 180
	votes := make([]float32, G*G)
	var lines []int
	for i := range obs {
		if !pp.ok[i] {
			continue
		}
		dx, dy := pp.x2[i]-pp.x1[i], pp.y2[i]-pp.y1[i]
		L := math.Hypot(dx, dy)
		if L < cfg.MinLineSepM {
			continue
		}
		lines = append(lines, i)
		ux, uy := dx/L, dy/L
		// Clip the infinite line to the square (slab method).
		t0, t1 := -4*R, 4*R
		for _, ax := range [2][2]float64{{pp.x1[i], ux}, {pp.y1[i], uy}} {
			if math.Abs(ax[1]) < 1e-12 {
				if ax[0] < -R || ax[0] > R {
					t0, t1 = 1, 0
				}
				continue
			}
			a, b := (-R-ax[0])/ax[1], (R-ax[0])/ax[1]
			if a > b {
				a, b = b, a
			}
			t0, t1 = math.Max(t0, a), math.Min(t1, b)
		}
		last := -1
		for t := t0; t <= t1; t += cell / 2 {
			if t > -cell && t < L+cell {
				continue // the radar is never between the two aircraft
			}
			if lineSigma(math.Abs(t), math.Abs(t-L), bw, 0) > cfg.MaxLineSigmaM {
				continue // the line is too vague this far out
			}
			x, y := pp.x1[i]+t*ux, pp.y1[i]+t*uy
			ix, iy := int((x+R)/cell), int((y+R)/cell)
			if ix < 0 || iy < 0 || ix >= G || iy >= G {
				continue
			}
			c := iy*G + ix
			if c != last {
				votes[c]++
				last = c
			}
		}
	}
	// 3x3 box smoothing.
	sm := make([]float32, G*G)
	for iy := 1; iy < G-1; iy++ {
		for ix := 1; ix < G-1; ix++ {
			var s float32
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					s += votes[(iy+dy)*G+ix+dx]
				}
			}
			sm[iy*G+ix] = s
		}
	}
	type peak struct {
		c int
		v float32
	}
	var peaks []peak
	for c, v := range sm {
		if v >= float32(cfg.MinRadarLines) {
			peaks = append(peaks, peak{c, v})
		}
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].v > peaks[j].v })
	var out []Interrogator
	type xy struct{ x, y float64 }
	var acc []xy
	for _, p := range peaks {
		if len(out) >= cfg.MaxRadars {
			break
		}
		x0 := (float64(p.c%G)+0.5)*cell - R
		y0 := (float64(p.c/G)+0.5)*cell - R
		near := false
		for _, a := range acc {
			if math.Hypot(a.x-x0, a.y-y0) < cfg.RadarNMSM {
				near = true
				break
			}
		}
		if near {
			continue
		}
		acc = append(acc, xy{x0, y0})
		r, ok := refineSite(&pp, lines, x0, y0, bw, cfg)
		if !ok {
			continue
		}
		dup := false
		for k := range out {
			x, y, _ := pp.g.Fwd(out[k].ECEF)
			rx, ry, _ := pp.g.Fwd(r.ECEF)
			if math.Hypot(x-rx, y-ry) < cfg.RadarNMSM {
				if r.Lines > out[k].Lines {
					out[k] = r
				}
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, r)
		}
	}
	return out, pp.g
}

// refineSite intersects the bearing lines near (x0, y0).
func refineSite(pp *planePts, lines []int, x0, y0, bw float64, cfg Config) (Interrogator, bool) {
	x, y := x0, y0
	var cov [3]float64
	var used int
	var az []float64
	for it := 0; it < 12; it++ {
		var a11, a12, a22, b1, b2, chi float64
		used = 0
		az = az[:0]
		for _, i := range lines {
			x1, y1, x2, y2 := pp.x1[i], pp.y1[i], pp.x2[i], pp.y2[i]
			v1x, v1y, v2x, v2y := x1-x, y1-y, x2-x, y2-y
			if v1x*v2x+v1y*v2y <= 0 {
				continue
			}
			r1, r2 := math.Hypot(v1x, v1y), math.Hypot(v2x, v2y)
			L := math.Hypot(x2-x1, y2-y1)
			nx, ny := -(y2-y1)/L, (x2-x1)/L
			d := nx*(x-x1) + ny*(y-y1)
			s := lineSigma(r1, r2, bw, 300)
			if s > cfg.MaxLineSigmaM {
				continue
			}
			gate := 3*s + cfg.AssignMinTolM
			if it < 3 {
				gate += 3 * cfg.RadarCellM
			}
			if math.Abs(d) > gate {
				continue
			}
			w := 1 / (s * s)
			if z := math.Abs(d) / s; z > cfg.HuberK {
				w *= cfg.HuberK / z
			}
			c := nx*x1 + ny*y1
			a11 += w * nx * nx
			a12 += w * nx * ny
			a22 += w * ny * ny
			b1 += w * nx * c
			b2 += w * ny * c
			chi += w * d * d
			used++
			az = append(az, math.Atan2(v1x+v2x, v1y+v2y))
		}
		det := a11*a22 - a12*a12
		if used < cfg.MinRadarLines || det <= 0 {
			return Interrogator{}, false
		}
		nxp := (a22*b1 - a12*b2) / det
		nyp := (a11*b2 - a12*b1) / det
		f := math.Max(1, chi/math.Max(1, float64(used-2)))
		cov = [3]float64{f * a22 / det, -f * a12 / det, f * a11 / det}
		moved := math.Hypot(nxp-x, nyp-y)
		x, y = nxp, nyp
		if moved < 1 && it >= 3 {
			break
		}
	}
	spread := azSpreadDeg(az)
	if spread < cfg.MinRadarSpreadDeg {
		return Interrogator{}, false
	}
	ll := pp.g.Inv(x, y, cfg.RadarHeightM)
	// The line fit ignores beam-edge biases and line crossings of other
	// sites: never claim better than LearnedSiteMinSigmaM.
	if m := cfg.LearnedSiteMinSigmaM * cfg.LearnedSiteMinSigmaM / 2; cov[0] < m || cov[2] < m {
		cov[0], cov[2] = math.Max(cov[0], m), math.Max(cov[2], m)
	}
	return Interrogator{
		LLH: ll, ECEF: ll.ECEF(), CovEN: cov, SigmaUM: cfg.RadarHeightSigmaM,
		Lines: used, SpreadDeg: spread,
	}, true
}

// azSpreadDeg returns 360° minus the largest gap between the azimuths.
func azSpreadDeg(az []float64) float64 {
	if len(az) < 2 {
		return 0
	}
	s := append([]float64(nil), az...)
	sort.Float64s(s)
	gap := s[0] + 2*math.Pi - s[len(s)-1]
	for i := 1; i < len(s); i++ {
		gap = math.Max(gap, s[i]-s[i-1])
	}
	return (2*math.Pi - gap) * 180 / math.Pi
}

// SitesFromConfig converts configured sites.
func SitesFromConfig(cfg Config) []Interrogator {
	var out []Interrogator
	for _, s := range cfg.Sites {
		sh := s.SigmaHM
		if sh <= 0 {
			sh = 50
		}
		su := s.SigmaUM
		if su <= 0 {
			su = cfg.RadarHeightSigmaM
		}
		ll := geo.LLH{Lat: s.Lat, Lon: s.Lon, H: s.HeightM}
		out = append(out, Interrogator{
			Name: s.Name, Configured: true, LLH: ll, ECEF: ll.ECEF(),
			CovEN: [3]float64{sh * sh / 2, 0, sh * sh / 2}, SigmaUM: su,
		})
	}
	return out
}

// Assign sets obs[i].Radar to the interrogator whose beam can hold both
// aircraft of the pair: seen from the site both lie on the same side and
// within 1.5 beam widths (plus the site's position uncertainty) of each
// other in azimuth. The second-best candidate is returned in alt (-1
// none); Solve picks between the two by residual. Observations without a
// candidate get Radar -1.
func Assign(obs []PairObs, radars []Interrogator, cfg Config) (alt []int) {
	cfg.Normalize()
	alt = make([]int, len(obs))
	if len(obs) == 0 {
		return alt
	}
	pp := projectObs(obs)
	bw := cfg.BeamwidthDeg * math.Pi / 180
	type site struct {
		x, y, s float64
		f       geo.Frame
		rot     *Rotation
	}
	sites := make([]site, len(radars))
	for k := range radars {
		x, y, ok := pp.g.Fwd(radars[k].ECEF)
		if !ok {
			x, y = math.Inf(1), math.Inf(1)
		}
		sites[k] = site{x: x, y: y, s: radars[k].SigmaHM(), f: geo.NewFrame(radars[k].LLH)}
		if r := &radars[k].Rotation; r.Valid() && r.Coherence >= cfg.MinCoherence {
			sites[k].rot = r
		}
	}
	phaseTol := 1.5*cfg.BeamwidthDeg + cfg.PhaseTolDeg
	for i := range obs {
		obs[i].Radar, alt[i] = -1, -1
		if !pp.ok[i] {
			continue
		}
		best, second := -1, -1
		bs, ss := math.Inf(1), math.Inf(1)
		for k := range sites {
			s := &sites[k]
			v1x, v1y := pp.x1[i]-s.x, pp.y1[i]-s.y
			v2x, v2y := pp.x2[i]-s.x, pp.y2[i]-s.y
			r1, r2 := math.Hypot(v1x, v1y), math.Hypot(v2x, v2y)
			if r1 < 1000 || r2 < 1000 || v1x*v2x+v1y*v2y <= 0 {
				continue
			}
			ang := math.Abs(math.Atan2(v1x*v2y-v1y*v2x, v1x*v2x+v1y*v2y))
			allow := 1.5*bw + 3*s.s/math.Min(r1, r2) + 3*cfg.AssignMinTolM/math.Max(r1, r2)
			score := ang / allow
			if score > 1 {
				continue
			}
			if s.rot != nil {
				az, t := pairAzimuth(&s.f, &obs[i])
				d, ok := s.rot.Residual(t, az)
				if !ok || math.Abs(d) > phaseTol {
					continue
				}
				score = math.Max(score, math.Abs(d)/phaseTol)
			}
			switch {
			case score < bs:
				second, ss = best, bs
				best, bs = k, score
			case score < ss:
				second, ss = k, score
			}
		}
		obs[i].Radar, alt[i] = best, second
	}
	return alt
}
