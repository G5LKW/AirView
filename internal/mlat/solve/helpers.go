package solve

import (
	"math"

	"airview/internal/geoid"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/linalg"
)

// ---------------------------------------------------------------------
// altitude (DESIGN §9.4 "Altitude is an extra row")

// GNSSBaroField is the regional GNSS-minus-baro field of mlat/ref /
// mlat/quality (DESIGN §9.6): the geometric height above the ellipsoid
// minus the pressure altitude, for a position and a pressure altitude.
// ok is false when the cell has no estimate.
type GNSSBaroField interface {
	GNSSMinusBaro(latDeg, lonDeg, baroFt float64) (deltaM, sigmaM float64, ok bool)
}

// AltitudeInput describes a Mode S altitude report.
type AltitudeInput struct {
	BaroFt float64 // pressure altitude (1013.25 hPa reference), ft
	// QuantFt is the reporting resolution: 25 (Q-bit) or 100 (Gillham).
	// 0 means 25.
	QuantFt float64
	// AgeS is the age of the altitude report at the transmit instant and
	// VRateMps the vertical rate (m/s); their product inflates σ.
	AgeS, VRateMps float64
}

// Altitude σ defaults (DESIGN §9.4): 15-30 m from the field, 60-100 m
// fallback.
const (
	AltSigmaFieldMin = 15.0
	AltSigmaFallback = 80.0
	AltSigmaNoGeoid  = 100.0
)

// AltitudeFromBaro converts a Mode S pressure altitude near (lat, lon) to a
// height above the ellipsoid with its σ:
//
//   - with a field estimate: HAE = baro + (GNSS − baro), σ₀ = the field σ
//     floored at 15 m (not capped: an uncertain cell keeps its σ);
//   - otherwise: ISA standard pressure altitude taken as MSL, plus the EGM96
//     undulation, σ₀ = 80 m (100 m without a geoid grid);
//
// and σ² = σ₀² + q²/12 + (age·vrate)² with q the reporting resolution. f
// may be nil.
func AltitudeFromBaro(in AltitudeInput, latDeg, lonDeg float64, f GNSSBaroField) Altitude {
	if !finite(in.BaroFt) || !finite(latDeg) || !finite(lonDeg) {
		return Altitude{}
	}
	const ft = 0.3048
	baro := in.BaroFt * ft
	var hae, s0 float64
	if d, sg, ok := fieldAt(f, latDeg, lonDeg, in.BaroFt); ok {
		hae = baro + d
		// floor only: a field cell that is itself uncertain (relaxed, or
		// fallback-dominated) keeps its σ (finding M4)
		s0 = math.Max(sg, AltSigmaFieldMin)
	} else if n, ok := geoid.Undulation(latDeg, lonDeg); ok {
		hae = baro + n
		s0 = AltSigmaFallback
	} else {
		hae = baro
		s0 = AltSigmaNoGeoid
	}
	q := in.QuantFt
	if !(q > 0) {
		q = 25
	}
	qm := q * ft
	drift := in.AgeS * in.VRateMps
	if !finite(drift) {
		drift = 0
	}
	return Altitude{Valid: true, HAE: hae, Sigma: math.Sqrt(s0*s0 + qm*qm/12 + drift*drift)}
}

// AltitudeFromHAE builds the altitude row from an already converted
// height (e.g. internal/mlat/ref Store.HAEFromBaro or Field.BaroToHAE,
// whose σ is the conversion 1σ): σ is floored at AltSigmaFieldMin and
// inflated by the reporting resolution and age × vertical rate of in
// (in.BaroFt is not used).
func AltitudeFromHAE(haeM, sigmaM float64, in AltitudeInput) Altitude {
	if !finite(haeM) || !finite(sigmaM) || !(sigmaM > 0) {
		return Altitude{}
	}
	const ft = 0.3048
	s0 := math.Max(sigmaM, AltSigmaFieldMin)
	q := in.QuantFt
	if !(q > 0) {
		q = 25
	}
	qm := q * ft
	drift := in.AgeS * in.VRateMps
	if !finite(drift) {
		drift = 0
	}
	return Altitude{Valid: true, HAE: haeM, Sigma: math.Sqrt(s0*s0 + qm*qm/12 + drift*drift)}
}

func fieldAt(f GNSSBaroField, lat, lon, baroFt float64) (float64, float64, bool) {
	if f == nil {
		return 0, 0, false
	}
	d, sg, ok := f.GNSSMinusBaro(lat, lon, baroFt)
	if !ok || !finite(d) || !finite(sg) || !(sg > 0) {
		return 0, 0, false
	}
	return d, sg, true
}

// ---------------------------------------------------------------------
// covariance assembly (DESIGN §9.4 "Weighting")

// ClockCovFunc returns the covariance (s²) of the network-time conversion
// errors of the stations at cluster positions i and j at the TOA: the
// marginal clock variance for i == j, the clock filter's cross-covariance
// for a synced pair, and ok = false when unknown (treated as 0).
type ClockCovFunc func(i, j int) (cov float64, ok bool)

// CovFromClock fills dst (m×m, s², row-major) with Σ = clock covariance +
// diag(jitter²). jitterS holds the per-station timestamp jitter (s). dst
// must have at least m² elements. The result is symmetrised and, if a
// clock cross-covariance would make Σ indefinite, the offending
// correlations are shrunk until Σ is positive definite (the diagonal is
// never changed). It returns false when that was necessary.
func CovFromClock(dst []float64, m int, jitterS []float64, clock ClockCovFunc) bool {
	for i := 0; i < m; i++ {
		for j := 0; j <= i; j++ {
			var v float64
			if clock != nil {
				if cv, ok := clock(i, j); ok && finite(cv) {
					v = cv
				}
			}
			if i == j {
				v = math.Max(v, 0) + jitterS[i]*jitterS[i]
			}
			dst[i*m+j], dst[j*m+i] = v, v
		}
	}
	var work [MaxStations * MaxStations]float64
	if m > MaxStations {
		return true
	}

	for t := 0; t < 20; t++ {
		copy(work[:m*m], dst[:m*m])
		if linalg.Cholesky(work[:m*m], m) == nil {
			return t == 0
		}

		for i := 0; i < m; i++ {
			for j := 0; j < i; j++ {
				dst[i*m+j] *= 0.7
				dst[j*m+i] *= 0.7
			}
		}
	}
	for i := 0; i < m; i++ {
		for j := 0; j < m; j++ {
			if i != j {
				dst[i*m+j] = 0
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------
// predicted accuracy (CRLB maps, siting advice; DESIGN §9.6)

// PredictCRLB returns the Cramér-Rao bound of the position (ENU at target,
// m²) and the DOPs for a target at an ECEF position heard by the given
// stations with TOA σ sigmaS (s) each, independent errors, and an altitude
// row of σ altSigmaM (≤ 0: no altitude row). It allocates nothing.
func PredictCRLB(stations []geo.Vec3, sigmaS []float64, target geo.Vec3, altSigmaM float64, atm *geo.Atmosphere) (enu [9]float64, dop DOP, ok bool) {
	llh := geo.FromECEF(target)
	f := geo.NewFrame(llh)
	var N, G linalg.Mat4
	add := func(M *linalg.Mat4, row [4]float64, w float64) {
		for a := 0; a < 4; a++ {
			for b := 0; b < 4; b++ {
				M[a*4+b] += w * row[a] * row[b]
			}
		}
	}
	for i, p := range stations {
		var g geo.Vec3
		if atm != nil {
			_, g = atm.RangeGrad(p, target)
		} else {
			g = target.Sub(p).Unit()
		}
		e := f.Rotate(g)
		sg := sigmaS[i] * c
		row := [4]float64{e.X, e.Y, e.Z, 1}
		add(&N, row, 1/(sg*sg))
		u := f.Rotate(target.Sub(p).Unit())
		add(&G, [4]float64{u.X, u.Y, u.Z, 1}, 1)
	}
	if altSigmaM > 0 {
		add(&N, [4]float64{0, 0, 1, 0}, 1/(altSigmaM*altSigmaM))
	}
	inv, err := N.InvSPD()
	if err != nil {
		return enu, DOP{}, false
	}
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			enu[a*3+b] = inv[a*4+b]
		}
	}
	nan := math.NaN()
	dop = DOP{nan, nan, nan, nan, nan}
	if len(stations) >= 4 {
		if gi, err := G.InvSPD(); err == nil {
			e, n, u, t := gi[0], gi[5], gi[10], gi[15]
			dop = DOP{G: math.Sqrt(e + n + u + t), P: math.Sqrt(e + n + u), H: math.Sqrt(e + n), V: math.Sqrt(u), T: math.Sqrt(t)}
		}
	}
	return enu, dop, true
}
