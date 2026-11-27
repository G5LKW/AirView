package met

import "math"

// angDiff returns a-b wrapped to (-180, 180].
func angDiff(a, b float64) float64 {
	d := math.Mod(a-b, 360)
	if d > 180 {
		d -= 360
	} else if d <= -180 {
		d += 360
	}
	return d
}

// norm360 wraps an angle to [0, 360).
func norm360(a float64) float64 {
	a = math.Mod(a, 360)
	if a < 0 {
		a += 360
	}
	if a >= 360 {
		a = 0
	}
	return a
}
