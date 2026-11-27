package terrain

import (
	"math"

	"airview/internal/radio"
)

// KnifeEdgeLossDB is the ITU-R P.526 (eq. 31) approximation of the single
// knife-edge diffraction loss J(v) in dB:
//
//	J(v) = 6.9 + 20·log10( sqrt((v−0.1)² + 1) + v − 0.1 )   for v > −0.78
//	J(v) = 0                                                 otherwise
//
// J(0) ≈ 6.0 dB (grazing), J(v) → 13 + 20·log10(v) for large v.
func KnifeEdgeLossDB(v float64) float64 {
	if !(v > -0.78) { // also NaN
		return 0
	}
	w := v - 0.1
	return 6.9 + 20*math.Log10(math.Sqrt(w*w+1)+w)
}

// FresnelParameter is the dimensionless diffraction parameter
// v = h·sqrt(2/λ·(1/d1 + 1/d2)) for an edge h metres above (positive) or
// below (negative) the direct ray, d1/d2 metres from the two ends, at
// wavelength λ metres (0 = 1090 MHz).
func FresnelParameter(hM, d1M, d2M, lambdaM float64) float64 {
	if !(lambdaM > 0) {
		lambdaM = radio.WavelengthM
	}
	if !(d1M > 0) || !(d2M > 0) {
		if hM > 0 {
			return math.Inf(1)
		}
		return math.Inf(-1)
	}
	return hM * math.Sqrt(2/lambdaM*(1/d1M+1/d2M))
}

// FresnelRadiusM is the radius of the first Fresnel zone in metres at a point
// d1/d2 metres from the ends of a path, at wavelength λ (0 = 1090 MHz):
// r1 = sqrt(λ·d1·d2/(d1+d2)).
func FresnelRadiusM(d1M, d2M, lambdaM float64) float64 {
	if !(lambdaM > 0) {
		lambdaM = radio.WavelengthM
	}
	if !(d1M > 0) || !(d2M > 0) {
		return 0
	}
	return math.Sqrt(lambdaM * d1M * d2M / (d1M + d2M))
}

// Edge is one diffracting edge found by the Deygout construction.
type Edge struct {
	Index    int     `json:"index"`    // profile sample index
	DistKm   float64 `json:"distKm"`   // from the receiver
	Lat      float64 `json:"lat"`      //
	Lon      float64 `json:"lon"`      //
	TerrainM float64 `json:"terrainM"` // terrain elevation AMSL (without bulge)
	HeightM  float64 `json:"heightM"`  // height above the (sub-)path ray; + = obstructing
	V        float64 `json:"v"`        // Fresnel-Kirchhoff parameter
	LossDB   float64 `json:"lossDb"`   // J(v)
	Level    int     `json:"level"`    // 0 = principal edge, 1 = secondary
}

// deygout computes the multiple knife-edge loss over effective heights h at
// distances x (metres) between sample indices a and b (exclusive), recursing
// depth levels (0 = principal edge only). It appends the edges found.
func deygout(x, h []float64, a, b, level, maxLevel int, lambda float64, edges []Edge) (float64, []Edge) {
	if b-a < 2 {
		return 0, edges
	}
	xa, xb := x[a], x[b]
	ha, hb := h[a], h[b]
	span := xb - xa
	if !(span > 0) {
		return 0, edges
	}
	// Excess height of sample i over the (sub-)path ray.
	excess := func(i int) float64 { return h[i] - (ha + (hb-ha)*(x[i]-xa)/span) }
	best, bestV, bestH := -1, math.Inf(-1), 0.0
	prev, cur := 0.0, excess(a+1) // excess(a) = 0
	for i := a + 1; i < b; i++ {
		next := 0.0 // excess(b) = 0
		if i+1 < b {
			next = excess(i + 1)
		}
		hh := cur
		// Only distinct obstacles qualify: local maxima of the excess height.
		// On a smooth hump (the earth bulge, a rounded hill) the sub-paths
		// next to the principal edge have their maximum at the edge itself,
		// so the same obstacle is not counted twice (classic Deygout
		// overestimate of ~6 dB per spurious grazing edge).
		if hh >= prev && hh >= next {
			d1, d2 := x[i]-xa, xb-x[i]
			if d1 > 0 && d2 > 0 {
				v := hh * math.Sqrt(2/lambda*(1/d1+1/d2))
				if v > bestV {
					best, bestV, bestH = i, v, hh
				}
			}
		}
		prev, cur = hh, next
	}
	if best < 0 || !(bestV > -0.78) {
		return 0, edges
	}
	loss := KnifeEdgeLossDB(bestV)
	edges = append(edges, Edge{Index: best, HeightM: bestH, V: bestV, LossDB: loss, Level: level})
	if level < maxLevel {
		var l1, l2 float64
		l1, edges = deygout(x, h, a, best, level+1, maxLevel, lambda, edges)
		l2, edges = deygout(x, h, best, b, level+1, maxLevel, lambda, edges)
		loss += l1 + l2
	}
	return loss, edges
}
