package ref

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/modes"
)

// Synthetic ADS-B frames for unit tests (our own encoders, built from the
// field layout the modes decoder documents).

// setBits writes v into ME bits first..last (1 = most significant of 56).
func setBits(me *uint64, first, last uint, v uint64) {
	n := last - first + 1
	shift := 56 - last
	mask := (uint64(1)<<n - 1) << shift
	*me = *me&^mask | (v<<shift)&mask
}

func esFrame(icao uint32, me uint64) []byte {
	f := make([]byte, 14)
	f[0] = 17<<3 | 5
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (48 - 8*i))
	}
	modes.SetParity(f, 0)
	return f
}

// posFrame encodes an airborne position (TC tc, barometric altitude ft).
func posFrame(icao uint32, tc uint8, altFt float64, t1, odd bool, lat, lon float64) []byte {
	var me uint64
	setBits(&me, 1, 5, uint64(tc))
	code, _ := modes.EncodeAC12(int32(math.Round(altFt/25) * 25))
	setBits(&me, 9, 20, uint64(code))
	if t1 {
		setBits(&me, 21, 21, 1)
	}
	if odd {
		setBits(&me, 22, 22, 1)
	}
	cl, cn := modes.CPREncode(lat, lon, odd, false)
	setBits(&me, 23, 39, uint64(cl))
	setBits(&me, 40, 56, uint64(cn))
	return esFrame(icao, me)
}

// velFrame encodes TC19 subtype 1 (ground speed components in m/s,
// vertical rate m/s, GNSS−baro difference ft).
func velFrame(icao uint32, ve, vn, vu, deltaFt float64, nacv uint8) []byte {
	var me uint64
	setBits(&me, 1, 5, 19)
	setBits(&me, 6, 8, 1)
	setBits(&me, 11, 13, uint64(nacv))
	ew := math.Round(ve / ktMps)
	ns := math.Round(vn / ktMps)
	if ew < 0 {
		setBits(&me, 14, 14, 1)
	}
	setBits(&me, 15, 24, uint64(math.Abs(ew))+1)
	if ns < 0 {
		setBits(&me, 25, 25, 1)
	}
	setBits(&me, 26, 35, uint64(math.Abs(ns))+1)
	vr := math.Round(vu / fpmMps / 64)
	if vr < 0 {
		setBits(&me, 37, 37, 1)
	}
	setBits(&me, 38, 46, uint64(math.Abs(vr))+1)
	if !math.IsNaN(deltaFt) { // NaN: no GNSS−baro difference
		d := math.Round(deltaFt / 25)
		if d < 0 {
			setBits(&me, 49, 49, 1)
		}
		setBits(&me, 50, 56, uint64(math.Abs(d))+1)
	}
	return esFrame(icao, me)
}

// opStatusFrame encodes TC31 airborne operational status (version 2).
func opStatusFrame(icao uint32, nacp uint8) []byte {
	var me uint64
	setBits(&me, 1, 5, 31)
	setBits(&me, 41, 43, 2)
	setBits(&me, 45, 48, uint64(nacp))
	setBits(&me, 51, 52, 3)
	return esFrame(icao, me)
}

// kin is a synthetic trajectory: piecewise straight / coordinated turn at
// constant height in a local ENU frame.
type kin struct {
	f     geo.Frame
	v     float64 // m/s
	psi0  float64 // initial track, rad (from north, clockwise)
	h     float64 // m, ENU up
	turnA float64 // turn start, s
	turnB float64 // turn end, s
	omega float64 // rad/s
	vu    float64
}

// at returns ENU position and velocity at t ≥ 0.
func (k *kin) at(t float64) (e, n, u, ve, vn float64) {
	seg := func(t0, t1, psi float64, w float64, e0, n0 float64) (float64, float64, float64) {
		dt := t1 - t0
		if w == 0 {
			s, c := math.Sincos(psi)
			return e0 + k.v*s*dt, n0 + k.v*c*dt, psi
		}
		p1 := psi + w*dt
		return e0 + k.v/w*(math.Cos(psi)-math.Cos(p1)), n0 + k.v/w*(math.Sin(p1)-math.Sin(psi)), p1
	}
	psi := k.psi0
	e, n = 0, 0
	if t <= k.turnA || k.omega == 0 {
		e, n, psi = seg(0, t, psi, 0, 0, 0)
	} else {
		e, n, psi = seg(0, k.turnA, psi, 0, 0, 0)
		if t <= k.turnB {
			e, n, psi = seg(k.turnA, t, psi, k.omega, e, n)
		} else {
			e, n, psi = seg(k.turnA, k.turnB, psi, k.omega, e, n)
			e, n, psi = seg(k.turnB, t, psi, 0, e, n)
		}
	}
	s, c := math.Sincos(psi)
	w := 0.0
	if t > k.turnA && t <= k.turnB {
		w = k.omega
	}
	_ = w
	return e, n, k.h + k.vu*t, k.v * s, k.v * c
}

// truth returns the ECEF position, geodetic position and local ENU
// velocity at t.
func (k *kin) truth(t float64) (p geo.Vec3, llh geo.LLH, ve, vn, vu float64) {
	e, n, u, ve, vn := k.at(t)
	p = k.f.FromENU(geo.Vec3{X: e, Y: n, Z: u})
	llh = geo.FromECEF(p)
	// rotate the frame velocity into the local ENU at the point
	vEC := k.f.Unrotate(geo.Vec3{X: ve, Y: vn, Z: k.vu})
	lf := geo.NewFrame(llh)
	lv := lf.Rotate(vEC)
	return p, llh, lv.X, lv.Y, lv.Z
}

// feeder drives a store with a synthetic aircraft: positions at 2 Hz
// (alternating CPR format), velocities at 2 Hz in between, operational
// status every 5 s.
type feeder struct {
	s     *Store
	icao  uint32
	k     *kin
	t0ns  int64
	odd   bool
	nacp  uint8
	t1    bool
	msg   modes.Message
	posFn func(t float64) (lat, lon float64) // override of the reported position (spoofing)
	velFn func(t float64) (ve, vn float64)   // override of the reported velocity
}

func (fd *feeder) ns(t float64) int64 { return fd.t0ns + int64(math.Round(t*1e9)) }

func (fd *feeder) obs(frame []byte, t float64) Result {
	if err := modes.Decode(frame, &fd.msg); err != nil {
		panic(err)
	}
	return fd.s.Observe(&fd.msg, fd.ns(t), 0)
}

// run feeds frames for t in [from, to) and returns the results of the
// position frames.
func (fd *feeder) run(from, to float64) []Result {
	var res []Result
	for t := from; t < to-1e-9; t += 0.25 {
		k := int(math.Round(t / 0.25))
		switch {
		case k%20 == 0:
			fd.obs(opStatusFrame(fd.icao, fd.nacp), t)
		case k%2 == 0:
			_, llh, _, _, _ := fd.k.truth(t)
			lat, lon := llh.Lat, llh.Lon
			if fd.posFn != nil {
				lat, lon = fd.posFn(t)
			}
			alt := llh.H / ftM
			res = append(res, fd.obs(posFrame(fd.icao, 11, alt, fd.t1, fd.odd, lat, lon), t))
			fd.odd = !fd.odd
		default:
			_, _, ve, vn, vu := fd.k.truth(t)
			if fd.velFn != nil {
				ve, vn = fd.velFn(t)
			}
			fd.obs(velFrame(fd.icao, ve, vn, vu, 0, 2), t)
		}
	}
	return res
}

func newFeeder(s *Store, icao uint32, lat, lon float64, v, psiDeg float64) *feeder {
	return &feeder{s: s, icao: icao, nacp: 9, t0ns: 1_700_000_000_000_000_000,
		k: &kin{f: geo.NewFrame(geo.LLH{Lat: lat, Lon: lon, H: 0}), v: v, psi0: psiDeg * math.Pi / 180, h: 10000}}
}
