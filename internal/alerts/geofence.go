package alerts

import "math"

// polygon is a geofence polygon in [lon, lat] degrees. Longitudes are
// unwrapped relative to the first vertex so fences across the antimeridian
// work; the test point is shifted into the same 360-degree window.
type polygon struct {
	xs, ys                 []float64
	minX, maxX, minY, maxY float64
}

func newPolygon(pts [][2]float64) *polygon {
	if n := len(pts); n > 1 && pts[0] == pts[n-1] {
		pts = pts[:n-1]
	}
	p := &polygon{xs: make([]float64, len(pts)), ys: make([]float64, len(pts))}
	p.minX, p.minY = math.Inf(1), math.Inf(1)
	p.maxX, p.maxY = math.Inf(-1), math.Inf(-1)
	for i, q := range pts {
		x := q[0]
		if i > 0 {
			prev := p.xs[i-1]
			for x-prev > 180 {
				x -= 360
			}
			for x-prev < -180 {
				x += 360
			}
		}
		p.xs[i], p.ys[i] = x, q[1]
		p.minX, p.maxX = math.Min(p.minX, x), math.Max(p.maxX, x)
		p.minY, p.maxY = math.Min(p.minY, q[1]), math.Max(p.maxY, q[1])
	}
	return p
}

// contains reports whether (lon, lat) is inside (even-odd rule; points on an
// edge may go either way).
func (p *polygon) contains(lon, lat float64) bool {
	if lat < p.minY || lat > p.maxY {
		return false
	}
	// Bring lon into the polygon's longitude window (unwrapped vertices may
	// extend beyond +-180).
	switch {
	case lon >= p.minX && lon <= p.maxX:
	case lon+360 >= p.minX && lon+360 <= p.maxX:
		lon += 360
	case lon-360 >= p.minX && lon-360 <= p.maxX:
		lon -= 360
	default:
		return false
	}
	in := false
	n := len(p.xs)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		yi, yj := p.ys[i], p.ys[j]
		if (yi > lat) != (yj > lat) {
			x := p.xs[i] + (lat-yi)/(yj-yi)*(p.xs[j]-p.xs[i])
			if lon < x {
				in = !in
			}
		}
	}
	return in
}
