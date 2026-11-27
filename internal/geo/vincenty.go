package geo

import "math"

// Vincenty's formulae on the WGS84 ellipsoid.
//
// T. Vincenty, "Direct and inverse solutions of geodesics on the ellipsoid
// with application of nested equations", Survey Review XXIII (176), 1975.
// Accuracy is better than 1 mm when the iteration converges.

const (
	vincentyEps     = 1e-12
	vincentyMaxIter = 500
)

// VincentyInverse solves the inverse geodesic problem on WGS84: the distance
// in metres between two points, the initial bearing at point 1 and the final
// bearing (direction of travel on arrival) at point 2, both degrees true.
//
// ok is false when the iteration does not converge, which only happens for
// nearly antipodal points; in that case spherical estimates are returned.
// Coincident points return 0, 0, 0, true.
func VincentyInverse(lat1, lon1, lat2, lon2 float64) (distM, initialBearingDeg, finalBearingDeg float64, ok bool) {
	sinU1, cosU1 := reducedLat(lat1)
	sinU2, cosU2 := reducedLat(lat2)
	L := NormalizeLon(lon2-lon1) * degToRad

	lambda := L
	var sinLambda, cosLambda, sinSigma, cosSigma, sigma, sinAlpha, cos2Alpha, cos2SigmaM float64
	converged := false
	for i := 0; i < vincentyMaxIter; i++ {
		sinLambda, cosLambda = math.Sincos(lambda)
		t1 := cosU2 * sinLambda
		t2 := cosU1*sinU2 - sinU1*cosU2*cosLambda
		sinSigma = math.Sqrt(t1*t1 + t2*t2)
		cosSigma = sinU1*sinU2 + cosU1*cosU2*cosLambda
		if sinSigma == 0 {
			if cosSigma > 0 {
				return 0, 0, 0, true // coincident
			}
			break // exactly antipodal: not unique
		}
		sigma = math.Atan2(sinSigma, cosSigma)
		sinAlpha = cosU1 * cosU2 * sinLambda / sinSigma
		cos2Alpha = 1 - sinAlpha*sinAlpha
		if cos2Alpha > 1e-15 {
			cos2SigmaM = cosSigma - 2*sinU1*sinU2/cos2Alpha
		} else {
			cos2SigmaM = 0 // equatorial line
		}
		C := WGS84F / 16 * cos2Alpha * (4 + WGS84F*(4-3*cos2Alpha))
		next := L + (1-C)*WGS84F*sinAlpha*
			(sigma+C*sinSigma*(cos2SigmaM+C*cosSigma*(-1+2*cos2SigmaM*cos2SigmaM)))
		if math.Abs(next) > math.Pi {
			break // diverging (antipodal region)
		}
		if math.Abs(next-lambda) < vincentyEps {
			lambda = next
			converged = true
			break
		}
		lambda = next
	}
	if !converged {
		d := HaversineKm(lat1, lon1, lat2, lon2) * 1000
		return d, InitialBearing(lat1, lon1, lat2, lon2), FinalBearing(lat1, lon1, lat2, lon2), false
	}

	sinLambda, cosLambda = math.Sincos(lambda)
	a2, b2 := WGS84A*WGS84A, WGS84B*WGS84B
	u2 := cos2Alpha * (a2 - b2) / b2
	A := 1 + u2/16384*(4096+u2*(-768+u2*(320-175*u2)))
	B := u2 / 1024 * (256 + u2*(-128+u2*(74-47*u2)))
	dSigma := B * sinSigma * (cos2SigmaM + B/4*(cosSigma*(-1+2*cos2SigmaM*cos2SigmaM)-
		B/6*cos2SigmaM*(-3+4*sinSigma*sinSigma)*(-3+4*cos2SigmaM*cos2SigmaM)))
	distM = WGS84B * A * (sigma - dSigma)

	a1 := math.Atan2(cosU2*sinLambda, cosU1*sinU2-sinU1*cosU2*cosLambda)
	a2f := math.Atan2(cosU1*sinLambda, -sinU1*cosU2+cosU1*sinU2*cosLambda)
	return distM, NormalizeDeg(a1 * radToDeg), NormalizeDeg(a2f * radToDeg), true
}

// VincentyKm is a convenience wrapper returning only the WGS84 distance in km.
func VincentyKm(lat1, lon1, lat2, lon2 float64) float64 {
	d, _, _, _ := VincentyInverse(lat1, lon1, lat2, lon2)
	return d / 1000
}

// VincentyDirect solves the direct geodesic problem on WGS84: the point
// reached by travelling distM metres from (lat,lon) on initial bearing
// bearingDeg, and the final bearing there. Use it for accurate range rings.
func VincentyDirect(lat, lon, bearingDeg, distM float64) (lat2, lon2, finalBearingDeg float64) {
	if distM == 0 {
		return lat, NormalizeLon(lon), NormalizeDeg(bearingDeg)
	}
	sinU1, cosU1 := reducedLat(lat)
	sinA1, cosA1 := math.Sincos(bearingDeg * degToRad)
	sigma1 := math.Atan2(sinU1, cosU1*cosA1)
	sinAlpha := cosU1 * sinA1
	cos2Alpha := 1 - sinAlpha*sinAlpha
	a2, b2 := WGS84A*WGS84A, WGS84B*WGS84B
	u2 := cos2Alpha * (a2 - b2) / b2
	A := 1 + u2/16384*(4096+u2*(-768+u2*(320-175*u2)))
	B := u2 / 1024 * (256 + u2*(-128+u2*(74-47*u2)))

	sigma := distM / (WGS84B * A)
	var sinSigma, cosSigma, cos2SigmaM float64
	for i := 0; i < vincentyMaxIter; i++ {
		cos2SigmaM = math.Cos(2*sigma1 + sigma)
		sinSigma, cosSigma = math.Sincos(sigma)
		dSigma := B * sinSigma * (cos2SigmaM + B/4*(cosSigma*(-1+2*cos2SigmaM*cos2SigmaM)-
			B/6*cos2SigmaM*(-3+4*sinSigma*sinSigma)*(-3+4*cos2SigmaM*cos2SigmaM)))
		next := distM/(WGS84B*A) + dSigma
		if math.Abs(next-sigma) < vincentyEps {
			sigma = next
			break
		}
		sigma = next
	}
	cos2SigmaM = math.Cos(2*sigma1 + sigma)
	sinSigma, cosSigma = math.Sincos(sigma)

	tmp := sinU1*sinSigma - cosU1*cosSigma*cosA1
	phi2 := math.Atan2(sinU1*cosSigma+cosU1*sinSigma*cosA1,
		(1-WGS84F)*math.Sqrt(sinAlpha*sinAlpha+tmp*tmp))
	lambda := math.Atan2(sinSigma*sinA1, cosU1*cosSigma-sinU1*sinSigma*cosA1)
	C := WGS84F / 16 * cos2Alpha * (4 + WGS84F*(4-3*cos2Alpha))
	L := lambda - (1-C)*WGS84F*sinAlpha*
		(sigma+C*sinSigma*(cos2SigmaM+C*cosSigma*(-1+2*cos2SigmaM*cos2SigmaM)))
	alpha2 := math.Atan2(sinAlpha, -tmp)
	return phi2 * radToDeg, NormalizeLon(lon + L*radToDeg), NormalizeDeg(alpha2 * radToDeg)
}
