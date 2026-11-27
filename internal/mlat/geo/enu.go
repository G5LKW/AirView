package geo

import "math"

// Frame is a local East-North-Up tangent frame anchored at an origin on or
// above the ellipsoid. The axes are the geodetic east, north and up
// (ellipsoid normal) directions at the origin. A Frame is an immutable
// value; build one per solve/track re-centre and reuse it.
type Frame struct {
	Origin    Vec3 // ECEF of the origin
	OriginLLH LLH  // geodetic origin
	// r holds the rotation ECEF -> ENU, row-major: rows are E, N, U.
	r [9]float64
}

// NewFrame builds the ENU frame at a geodetic origin.
func NewFrame(o LLH) Frame {
	sl, cl := math.Sincos(o.Lat * deg)
	so, co := math.Sincos(o.Lon * deg)
	return Frame{
		Origin:    o.ECEF(),
		OriginLLH: o,
		r: [9]float64{
			-so, co, 0,
			-sl * co, -sl * so, cl,
			cl * co, cl * so, sl,
		},
	}
}

// NewFrameECEF builds the ENU frame whose origin is the given ECEF point.
func NewFrameECEF(v Vec3) Frame { return NewFrame(FromECEF(v)) }

// Rotation returns the 3×3 rotation matrix R (row-major) with
// enu = R·(ecef - origin). Its transpose maps ENU back to ECEF.
func (f *Frame) Rotation() [9]float64 { return f.r }

// East, North and Up return the frame axes as ECEF unit vectors.
func (f *Frame) East() Vec3 { return Vec3{f.r[0], f.r[1], f.r[2]} }

// North returns the north axis as an ECEF unit vector.
func (f *Frame) North() Vec3 { return Vec3{f.r[3], f.r[4], f.r[5]} }

// Up returns the up axis (ellipsoid normal at the origin).
func (f *Frame) Up() Vec3 { return Vec3{f.r[6], f.r[7], f.r[8]} }

// ToENU converts an ECEF position to local ENU metres.
func (f *Frame) ToENU(p Vec3) Vec3 { return f.Rotate(p.Sub(f.Origin)) }

// FromENU converts local ENU metres to an ECEF position.
func (f *Frame) FromENU(e Vec3) Vec3 { return f.Origin.Add(f.Unrotate(e)) }

// Rotate maps an ECEF displacement (or direction) to ENU.
func (f *Frame) Rotate(d Vec3) Vec3 {
	r := &f.r
	return Vec3{
		r[0]*d.X + r[1]*d.Y + r[2]*d.Z,
		r[3]*d.X + r[4]*d.Y + r[5]*d.Z,
		r[6]*d.X + r[7]*d.Y + r[8]*d.Z,
	}
}

// Unrotate maps an ENU displacement (or direction) to ECEF.
func (f *Frame) Unrotate(e Vec3) Vec3 {
	r := &f.r
	return Vec3{
		r[0]*e.X + r[3]*e.Y + r[6]*e.Z,
		r[1]*e.X + r[4]*e.Y + r[7]*e.Z,
		r[2]*e.X + r[5]*e.Y + r[8]*e.Z,
	}
}

// CovToENU rotates a 3×3 ECEF covariance (row-major) into the frame:
// R·C·Rᵀ.
func (f *Frame) CovToENU(c [9]float64) [9]float64 { return congruence3(&f.r, &c, false) }

// CovToECEF rotates a 3×3 ENU covariance (row-major) back to ECEF: Rᵀ·C·R.
func (f *Frame) CovToECEF(c [9]float64) [9]float64 { return congruence3(&f.r, &c, true) }

// congruence3 returns R·C·Rᵀ (or Rᵀ·C·R when transpose is set).
func congruence3(r, c *[9]float64, transpose bool) [9]float64 {
	var a [9]float64 // a = R(ᵀ)
	if transpose {
		a = [9]float64{r[0], r[3], r[6], r[1], r[4], r[7], r[2], r[5], r[8]}
	} else {
		a = *r
	}
	var t, out [9]float64 // t = A·C
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			t[3*i+j] = a[3*i]*c[j] + a[3*i+1]*c[3+j] + a[3*i+2]*c[6+j]
		}
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out[3*i+j] = t[3*i]*a[3*j] + t[3*i+1]*a[3*j+1] + t[3*i+2]*a[3*j+2]
		}
	}
	return out
}

// AzElRange returns the azimuth (degrees true, [0,360)), the geometric
// elevation (degrees, relative to the frame's tangent plane) and the
// straight-line range (m) from the frame origin to an ECEF point.
func (f *Frame) AzElRange(p Vec3) (azDeg, elDeg, rangeM float64) {
	e := f.ToENU(p)
	rangeM = e.Norm()
	if rangeM == 0 {
		return 0, 90, 0
	}
	azDeg = math.Atan2(e.X, e.Y) / deg
	if azDeg < 0 {
		azDeg += 360
	}
	elDeg = math.Asin(math.Max(-1, math.Min(1, e.Z/rangeM))) / deg
	return
}
