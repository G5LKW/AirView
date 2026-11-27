package track

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// maxRows is the largest measurement vector: MaxClusterMembers−1 TDOA rows
// plus the altitude row (fixes have 3 rows, velocities 2-3).
const maxRows = MaxClusterMembers

type measKind uint8

const (
	kTOA measKind = iota // TDOA rows (+ altitude row)
	kFix                 // ENU position (3 rows)
	kVel                 // ENU velocity (2 or 3 rows)
	kAlt                 // altitude only (1 row)
)

// member is one station of a TOA measurement with its refraction excess
// linearised around p0: ρ(p) = |p − s| + exc + g·(p − p0).
type member struct {
	st      uint32
	pos     geo.Vec3
	p0      geo.Vec3
	exc     float64
	g       geo.Vec3
	syncVar float64 // correlated clock-sync variance of the TOA, s²
	ci      int     // index of the station's entry in the hypothesis cache
}

// meas is a prepared measurement. R is rows×rows with stride rows.
type meas struct {
	kind  measKind
	rows  int
	z     [maxRows]float64
	R     [maxRows * maxRows]float64
	nst   int
	st    [MaxClusterMembers]member // st[0] is the TDOA reference
	alt   bool                      // last row is the altitude (kTOA, kAlt)
	altZ  float64
	velUp bool
}

// work is per-tracker scratch space (a Tracker is single-goroutine).
type work struct {
	h  [maxRows]float64
	H  [maxRows * nx]float64
	r  [maxRows]float64
	L  [maxRows * maxRows]float64
	nv [nx]float64
}

// eval computes h(x) and the Jacobian H (rows×nx, row-major) of the
// measurement at state x in frame fr.
func (w *work) eval(ms *meas, fr *geo.Frame, x *vec) {
	rows := ms.rows
	H := w.H[:rows*nx]
	clear(H)
	switch ms.kind {
	case kFix:
		for i := range 3 {
			w.h[i] = x[i]
			H[i*nx+i] = 1
		}
		return
	case kVel:
		for i := range rows {
			w.h[i] = x[iVE+i]
			H[i*nx+iVE+i] = 1
		}
		return
	}
	p := fr.FromENU(geo.Vec3{X: x[iE], Y: x[iN], Z: x[iU]})
	if ms.kind == kTOA && ms.nst >= 2 {
		var rho0 float64
		var g0 geo.Vec3
		for j := 0; j < ms.nst; j++ {
			m := &ms.st[j]
			d := p.Sub(m.pos)
			r := d.Norm()
			rho := r + m.exc + m.g.Dot(p.Sub(m.p0))
			grad := fr.Rotate(d.Scale(1 / r).Add(m.g))
			if j == 0 {
				rho0, g0 = rho, grad
				continue
			}
			k := j - 1
			w.h[k] = rho - rho0
			H[k*nx+iE] = grad.X - g0.X
			H[k*nx+iN] = grad.Y - g0.Y
			H[k*nx+iU] = grad.Z - g0.Z
		}
	}
	if ms.alt {
		k := rows - 1
		llh := geo.FromECEF(p)
		w.h[k] = llh.H
		up := fr.Rotate(geo.Up(llh.Lat, llh.Lon))
		H[k*nx+iE], H[k*nx+iN], H[k*nx+iU] = up.X, up.Y, up.Z
	}
}

// modelOut is the result of updating one motion model.
type modelOut struct {
	x       vec
	P       mat
	nis, ll float64
	logdetS float64
	ok      bool
}

// updateModel applies measurement ms to (x, P) of model k with an iterated
// EKF in information form (doc.go §4). back < 0 selects the
// out-of-sequence form: the measurement refers to back seconds before the
// state's time (doc.go §6).
func (w *work) updateModel(ms *meas, fr *geo.Frame, k int, x *vec, P *mat, back float64, p *Params, out *modelOut) {
	out.ok = false
	rows := ms.rows
	// Diagonal scaling: Ps = D⁻¹ P D⁻¹ has a unit diagonal.
	var d [nx]float64
	for i := range nx {
		v := P[i*nx+i]
		if !(v > 0) || math.IsInf(v, 0) {
			return
		}
		d[i] = math.Sqrt(v)
	}
	var Pinv mat
	for i := range nx {
		for j := range nx {
			Pinv[i*nx+j] = P[i*nx+j] / (d[i] * d[j])
		}
	}
	if linalg.Cholesky(Pinv[:], nx) != nil {
		return
	}
	logdetPs := linalg.CholLogDet(Pinv[:], nx)
	if linalg.CholToInverse(Pinv[:], nx, w.nv[:]) != nil {
		return
	}

	iters := p.Iterations
	xhat := *x
	xi := xhat
	var F mat
	L := w.L[:rows*rows]
	if back < 0 {
		// Retrodict: linearise at x_b = f(x, back); H_eff = H(x_b)·F,
		// R_eff = R + H Q(|back|) Hᵀ.
		var xb vec
		xb, F = motion(k, x, back, p)
		xi = xb
		iters = 1
	}
	var logdetR float64
	if back >= 0 {
		copy(L, ms.R[:rows*rows])
		if linalg.Cholesky(L, rows) != nil {
			return
		}
		logdetR = linalg.CholLogDet(L, rows)
	}
	var A mat
	var b, ds vec
	for range iters {
		w.eval(ms, fr, &xi)
		H := w.H[:rows*nx]
		if back < 0 {
			copy(L, ms.R[:rows*rows])
			// R_eff = R + H Q Hᵀ with H at x_b, then H ← H·F.
			Q := processNoise(k, back, p, 0)
			for a := range rows {
				ha := H[a*nx : a*nx+nx]
				var hq [nx]float64
				for c := range nx {
					var s float64
					for e := range nx {
						s += ha[e] * Q[e*nx+c]
					}
					hq[c] = s
				}
				for bb := 0; bb <= a; bb++ {
					hb := H[bb*nx : bb*nx+nx]
					var s float64
					for c := range nx {
						s += hq[c] * hb[c]
					}
					L[a*rows+bb] += s
					if bb != a {
						L[bb*rows+a] += s
					}
				}
			}
			for a := range rows {
				ha := H[a*nx : a*nx+nx]
				var t [nx]float64
				for c := range nx {
					var s float64
					for e := range nx {
						s += ha[e] * F[e*nx+c]
					}
					t[c] = s
				}
				copy(ha, t[:])
			}
			// effective innovation at the prior: z − h(x_b)
			for a := range rows {
				w.r[a] = ms.z[a] - w.h[a]
			}
		} else {
			for a := range rows {
				ha := H[a*nx : a*nx+nx]
				var s float64
				for c := range nx {
					s += ha[c] * (xhat[c] - xi[c])
				}
				w.r[a] = ms.z[a] - w.h[a] - s
			}
		}
		if back < 0 {
			if linalg.Cholesky(L, rows) != nil {
				return
			}
			logdetR = linalg.CholLogDet(L, rows)
		}
		// Whiten: r ← L⁻¹ r, H ← L⁻¹ H.
		for a := range rows {
			la := L[a*rows : a*rows+a+1]
			ha := H[a*nx : a*nx+nx]
			ra := w.r[a]
			for bb := 0; bb < a; bb++ {
				if v := la[bb]; v != 0 {
					ra -= v * w.r[bb]
					hb := H[bb*nx : bb*nx+nx]
					for c := range nx {
						ha[c] -= v * hb[c]
					}
				}
			}
			inv := 1 / la[a]
			w.r[a] = ra * inv
			for c := range nx {
				ha[c] *= inv
			}
		}
		// Scale columns by D and form A = Ps⁻¹ + HsᵀHs, b = Hsᵀ r.
		A = Pinv
		b = vec{}
		for a := range rows {
			ha := H[a*nx : a*nx+nx]
			for c := range nx {
				ha[c] *= d[c]
			}
			ra := w.r[a]
			for c := range nx {
				hc := ha[c]
				if hc == 0 {
					continue
				}
				b[c] += hc * ra
				for e := 0; e <= c; e++ {
					A[c*nx+e] += hc * ha[e]
				}
			}
		}
		for c := range nx {
			for e := 0; e < c; e++ {
				A[e*nx+c] = A[c*nx+e]
			}
		}
		if linalg.Cholesky(A[:], nx) != nil {
			return
		}
		ds = b
		linalg.CholSolve(A[:], nx, ds[:])
		var xn vec
		var moved float64
		for c := range nx {
			xn[c] = xhat[c] + d[c]*ds[c]
			if c < iVE {
				moved = math.Max(moved, math.Abs(xn[c]-xi[c]))
			}
		}
		if back < 0 {
			xi = xn
			break
		}
		conv := moved < 0.05
		xi = xn
		if conv {
			break
		}
	}
	logdetA := linalg.CholLogDet(A[:], nx)
	var rr, bd float64
	for a := range rows {
		rr += w.r[a] * w.r[a]
	}
	for c := range nx {
		bd += b[c] * ds[c]
	}
	nis := rr - bd
	if nis < 0 {
		nis = 0
	}
	if linalg.CholToInverse(A[:], nx, w.nv[:]) != nil {
		return
	}
	for i := range nx {
		for j := range nx {
			out.P[i*nx+j] = A[i*nx+j] * d[i] * d[j]
		}
	}
	symmetrize(&out.P)
	out.x = xi
	out.nis = nis
	out.logdetS = logdetR + logdetPs + logdetA
	out.ll = -0.5 * (nis + out.logdetS + float64(rows)*log2Pi)
	out.ok = !math.IsNaN(out.ll) && !math.IsInf(out.ll, 0)
}

var log2Pi = math.Log(2 * math.Pi)
