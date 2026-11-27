package analytics

import (
	"math"
	"sync"
)

// l2FarFrom (km): the lowest terrain from here to the end of a panorama ray
// is the level of the smooth earth assumed beyond it.
const l2FarFrom = 15.0

// l2DEM is the terrain elevation sampled once on a regular grid of the
// search plane (east/north km around the search centre), so that the
// thousands of candidate panoramas read memory instead of the tile cache.
type l2DEM struct {
	x0, y0 float64 // plane coordinates of z[0]
	step   float64 // km
	n      int     // n × n samples
	z      []float32
	noData float64 // share of samples without data
}

// newL2DEM samples p.src every step km over the square of half width half
// around (cx, cy).
func newL2DEM(p *l2Problem, cx, cy, half, step float64) *l2DEM {
	d := &l2DEM{step: step}
	d.n = int(math.Ceil(2*half/d.step)) + 1
	d.x0, d.y0 = cx-half, cy-half
	d.z = make([]float32, d.n*d.n)
	var missing int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	rows := make(chan int, d.n)
	for r := 0; r < d.n; r++ {
		rows <- r
	}
	close(rows)
	for w := 0; w < p.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var miss int64
			for r := range rows {
				y := d.y0 + float64(r)*d.step
				for c := 0; c < d.n; c++ {
					x := d.x0 + float64(c)*d.step
					la, lo := p.planeToLatLon(x, y)
					z, ok := p.src.ElevationM(la, lo)
					if !ok || !isNum(z) {
						miss++
						z = math.NaN()
					}
					d.z[r*d.n+c] = float32(z)
				}
			}
			mu.Lock()
			missing += miss
			mu.Unlock()
		}()
	}
	wg.Wait()
	d.noData = float64(missing) / float64(len(d.z))
	return d
}

// covers reports whether the grid holds the square of half width half
// around (cx, cy).
func (d *l2DEM) covers(cx, cy, half float64) bool {
	x1 := d.x0 + float64(d.n-1)*d.step
	y1 := d.y0 + float64(d.n-1)*d.step
	return cx-half >= d.x0 && cy-half >= d.y0 && cx+half <= x1 && cy+half <= y1
}

// at returns the bilinearly interpolated elevation (NaN outside the grid or
// without data).
func (d *l2DEM) at(x, y float64) float64 {
	fx := (x - d.x0) / d.step
	fy := (y - d.y0) / d.step
	if !(fx >= 0 && fy >= 0) || fx > float64(d.n-1) || fy > float64(d.n-1) {
		return math.NaN()
	}
	c, r := int(fx), int(fy)
	if c >= d.n-1 {
		c = d.n - 2
	}
	if r >= d.n-1 {
		r = d.n - 2
	}
	ax, ay := fx-float64(c), fy-float64(r)
	i := r*d.n + c
	v00, v10 := float64(d.z[i]), float64(d.z[i+1])
	v01, v11 := float64(d.z[i+d.n]), float64(d.z[i+d.n+1])
	top := v00 + (v10-v00)*ax
	bot := v01 + (v11-v01)*ax
	return top + (bot-top)*ay
}

// panorama fills sc.pano[i] (one per antenna height hr[i], m MSL) with the
// cumulative maximum, along each of l2PanoRays rays, of the tangent of the
// terrain elevation angle seen from the node (effective earth k·R):
// pano[ray·steps + j] = max over steps <= j of (z - hr)/d - d/(2kR).
func (d *l2DEM) panorama(nd *l2Node, hr []float64, sc *l2Scratch) {
	nSteps := len(sc.steps)
	nn := len(hr)
	prev := sc.prev[:nn]
	for ray := 0; ray < l2PanoRays; ray++ {
		sb, cb := math.Sincos(2 * math.Pi * float64(ray) / l2PanoRays)
		base := ray * nSteps
		for i := range prev {
			prev[i] = float32(math.Inf(-1))
		}
		zLow := math.Inf(1)
		for j := 0; j < nSteps; j++ {
			dm := sc.steps[j]
			z := d.at(nd.px+dm/1000*sb, nd.py+dm/1000*cb)
			if dm >= l2FarFrom*1000 && isNum(z) && z < zLow {
				zLow = z
			}
			for i := 0; i < nn; i++ {
				v := prev[i]
				if isNum(z) {
					t := float32((z-hr[i])/dm - dm/sc.kR2)
					if t > v {
						v = t
					}
				}
				prev[i] = v
				sc.pano[i][base+j] = v
			}
		}
		// beyond the panorama: the smooth earth at the lowest level of the
		// ray's outer part (the antenna's own ground when there is none)
		for i := 0; i < nn; i++ {
			above := hr[i] - zLow
			if !isNum(above) {
				above = hr[i] - nd.g
			}
			if !(above > 1) {
				above = 1
			}
			sc.far[i][ray] = float32(-math.Sqrt(4 * above / sc.kR2)) // -sqrt(2h/kR)
		}
	}
}
