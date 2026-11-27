package trajectory

import (
	"math"
	"sort"

	"airview/internal/model"
)

// SmoothOptions tunes SmoothTrail. Zero fields take defaults.
type SmoothOptions struct {
	Q       float64 // white-acceleration density, m^2/s^3 (default 0.3)
	MaxGap  float64 // seconds; a longer gap starts a new independent segment (default 60)
	SigmaM  [4]float64
	GateChi float64 // 2-D gate (default 16); gated points are replaced by the smoothed estimate
}

func (o SmoothOptions) withDefaults() SmoothOptions {
	if !(o.Q > 0) {
		o.Q = 0.3
	}
	if !(o.MaxGap > 0) {
		o.MaxGap = 60
	}
	def := [4]float64{15, 120, 150, 50} // adsb, mlat, tisb/adsr, other (TrailPoint.Src codes)
	for i := range o.SigmaM {
		if !(o.SigmaM[i] > 0) {
			o.SigmaM[i] = def[i]
		}
	}
	if !(o.GateChi > 0) {
		o.GateChi = 16
	}
	return o
}

// SmoothTrail returns a smoothed copy of a trail (fixed-interval
// Rauch-Tung-Striebel smoother with a constant-velocity model, per segment).
// Points are sorted by time; exact duplicate times are collapsed. Only Lat/Lon
// change; other fields are copied. Use it for drawing noisy (MLAT) trails.
func SmoothTrail(in []model.TrailPoint, opt SmoothOptions) []model.TrailPoint {
	opt = opt.withDefaults()
	pts := make([]model.TrailPoint, 0, len(in))
	for _, p := range in {
		if finite(p.Lat) && finite(p.Lon) && math.Abs(p.Lat) <= 90 && math.Abs(p.Lon) <= 180 {
			pts = append(pts, p)
		}
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
	w := 0
	for i := range pts {
		if w > 0 && pts[i].T == pts[w-1].T {
			continue
		}
		pts[w] = pts[i]
		w++
	}
	pts = pts[:w]
	start := 0
	for i := 1; i <= len(pts); i++ {
		if i == len(pts) || float64(pts[i].T-pts[i-1].T)/1000 > opt.MaxGap {
			smoothSegment(pts[start:i], &opt)
			start = i
		}
	}
	return pts
}

func smoothSegment(pts []model.TrailPoint, opt *SmoothOptions) {
	n := len(pts)
	if n < 3 {
		return
	}
	fr := newFrame(pts[0].Lat, pts[0].Lon)
	type st struct {
		x, y   [2]float64    // filtered (pos, vel) per axis
		P      [2][2]float64 // filtered covariance (shared by both axes)
		xp, yp [2]float64    // predicted
		Pp     [2][2]float64
		dt     float64
	}
	s := make([]st, n)
	sig := func(p model.TrailPoint) float64 {
		k := int(p.Src)
		if k < 0 || k > 3 {
			k = 3
		}
		return opt.SigmaM[k]
	}
	zx, zy := fr.toXY(pts[0].Lat, pts[0].Lon)
	r0 := sig(pts[0])
	s[0].x = [2]float64{zx, 0}
	s[0].y = [2]float64{zy, 0}
	s[0].P = [2][2]float64{{r0 * r0, 0}, {0, 300 * 300}}
	s[0].xp, s[0].yp, s[0].Pp = s[0].x, s[0].y, s[0].P
	q := opt.Q
	for k := 1; k < n; k++ {
		dt := float64(pts[k].T-pts[k-1].T) / 1000
		s[k].dt = dt
		pv := s[k-1].P
		var Pp [2][2]float64
		Pp[0][0] = pv[0][0] + 2*dt*pv[0][1] + dt*dt*pv[1][1] + q*dt*dt*dt/3
		Pp[0][1] = pv[0][1] + dt*pv[1][1] + q*dt*dt/2
		Pp[1][0] = Pp[0][1]
		Pp[1][1] = pv[1][1] + q*dt
		xp := [2]float64{s[k-1].x[0] + dt*s[k-1].x[1], s[k-1].x[1]}
		yp := [2]float64{s[k-1].y[0] + dt*s[k-1].y[1], s[k-1].y[1]}
		s[k].xp, s[k].yp, s[k].Pp = xp, yp, Pp
		zx, zy := fr.toXY(pts[k].Lat, pts[k].Lon)
		r := sig(pts[k])
		S := Pp[0][0] + r*r
		nx, ny := zx-xp[0], zy-yp[0]
		if (nx*nx+ny*ny)/S > opt.GateChi && k > 2 {
			s[k].x, s[k].y, s[k].P = xp, yp, Pp // outlier: coast
			continue
		}
		k0, k1 := Pp[0][0]/S, Pp[1][0]/S
		s[k].x = [2]float64{xp[0] + k0*nx, xp[1] + k1*nx}
		s[k].y = [2]float64{yp[0] + k0*ny, yp[1] + k1*ny}
		s[k].P = [2][2]float64{
			{(1 - k0) * Pp[0][0], (1 - k0) * Pp[0][1]},
			{Pp[1][0] - k1*Pp[0][0], Pp[1][1] - k1*Pp[0][1]},
		}
		s[k].P[0][1] = 0.5 * (s[k].P[0][1] + s[k].P[1][0])
		s[k].P[1][0] = s[k].P[0][1]
	}
	// backward pass
	xs, ys := s[n-1].x, s[n-1].y
	setLL := func(k int, x, y float64) {
		la, lo := fr.toLatLon(x, y)
		if finite(la) && finite(lo) {
			pts[k].Lat, pts[k].Lon = la, lo
		}
	}
	setLL(n-1, xs[0], ys[0])
	for k := n - 2; k >= 0; k-- {
		dt := s[k+1].dt
		Pf := s[k].P
		Pp := s[k+1].Pp
		// C = Pf F' Pp^-1, F = [[1,dt],[0,1]]
		pft := [2][2]float64{
			{Pf[0][0] + dt*Pf[0][1], Pf[0][1]},
			{Pf[1][0] + dt*Pf[1][1], Pf[1][1]},
		}
		det := Pp[0][0]*Pp[1][1] - Pp[0][1]*Pp[1][0]
		if !(det > 1e-12) {
			xs, ys = s[k].x, s[k].y
			setLL(k, xs[0], ys[0])
			continue
		}
		inv := [2][2]float64{{Pp[1][1] / det, -Pp[0][1] / det}, {-Pp[1][0] / det, Pp[0][0] / det}}
		var C [2][2]float64
		for a := 0; a < 2; a++ {
			for b := 0; b < 2; b++ {
				C[a][b] = pft[a][0]*inv[0][b] + pft[a][1]*inv[1][b]
			}
		}
		dx := [2]float64{xs[0] - s[k+1].xp[0], xs[1] - s[k+1].xp[1]}
		dy := [2]float64{ys[0] - s[k+1].yp[0], ys[1] - s[k+1].yp[1]}
		nxs := [2]float64{s[k].x[0] + C[0][0]*dx[0] + C[0][1]*dx[1], s[k].x[1] + C[1][0]*dx[0] + C[1][1]*dx[1]}
		nys := [2]float64{s[k].y[0] + C[0][0]*dy[0] + C[0][1]*dy[1], s[k].y[1] + C[1][0]*dy[0] + C[1][1]*dy[1]}
		xs, ys = nxs, nys
		setLL(k, xs[0], ys[0])
	}
}
