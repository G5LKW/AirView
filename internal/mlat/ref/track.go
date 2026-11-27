package ref

import (
	"math"

	"airview/internal/mlat/geo"
)

const (
	deg2rad = math.Pi / 180
	rad2deg = 180 / math.Pi
	ktMps   = 1852.0 / 3600
	fpmMps  = ftM / 60
)

// node kinds (bit set)
const (
	nPosH  uint8 = 1 << iota // horizontal position (lat, lon)
	nPosV                    // height
	nVelH                    // horizontal velocity
	nVelV                    // vertical rate
	nT1                      // position tag is a UTC epoch (not re-tagged)
	nExact                   // tag is the transmit instant itself (Mode S altitude reply; not re-tagged)
)

// obs is one measurement. Measurements are kept in geodetic form so that
// a node can be re-expressed in a new kinematic frame or re-tagged with a
// new latency estimate.
type obs struct {
	t    float64 // tag: time the measurement applies to, s since track.t0
	rawT float64 // transmit instant, s since track.t0 (position nodes)
	kind uint8

	lat, lon, h float64 // position (deg, deg, m HAE)
	re, rn, rh  float64 // position noise variances, m²
	ve, vn, vu  float64 // velocity (local ENU, m/s)
	rv, rvu     float64 // velocity noise variances (per horizontal component, vertical)
}

// node is one measurement of a track with the filtered state after it.
type node struct {
	obs
	st [3]axis // filtered state after this node
}

// track is the reference track of one aircraft (doc.go "Track model").
type track struct {
	icao uint32
	t0   int64 // ns, tag origin

	// kinematic frame: x = Δλ·kx, y = Δφ·ky (radians), z = HAE
	lat0, lon0 float64
	kx, ky     float64

	nodes []node // ring, allocated with the first node
	win   int    // ring capacity
	start int
	n     int

	// position latency (T=0): tags are rawT − lBuilt
	lHat, lVar float64
	lBuilt     float64
	lObs       int
	lastRetag  float64

	info info
	q    qual
}

func (t *track) at(i int) *node { return &t.nodes[(t.start+i)%len(t.nodes)] }
func (t *track) last() *node    { return t.at(t.n - 1) }

// sec converts ns to track seconds.
func (t *track) sec(ns int64) float64 { return float64(ns-t.t0) / 1e9 }

// setFrame chooses the kinematic frame origin.
func (t *track) setFrame(lat, lon float64) {
	t.lat0, t.lon0 = lat, lon
	sl, cl := math.Sincos(lat * deg2rad)
	w := math.Sqrt(1 - geo.WGS84E2*sl*sl)
	nr := geo.WGS84A / w
	mr := geo.WGS84A * (1 - geo.WGS84E2) / (w * w * w)
	t.kx = nr * math.Max(cl, 1e-3)
	t.ky = mr
}

// radii returns the prime-vertical and meridian radii of curvature at a
// latitude.
func radii(lat float64) (nr, mr float64) {
	sl := math.Sin(lat * deg2rad)
	w := math.Sqrt(1 - geo.WGS84E2*sl*sl)
	return geo.WGS84A / w, geo.WGS84A * (1 - geo.WGS84E2) / (w * w * w)
}

// scales returns metres-per-kinematic-unit along x and y at a point:
// dE = sx·dx, dN = sy·dy.
func (t *track) scales(lat, h float64) (sx, sy float64) {
	nr, mr := radii(lat)
	cl := math.Max(math.Cos(lat*deg2rad), 1e-3)
	return (nr + h) * cl / t.kx, (mr + h) / t.ky
}

func (t *track) toXY(lat, lon float64) (x, y float64) {
	return wrap180(lon-t.lon0) * deg2rad * t.kx, (lat - t.lat0) * deg2rad * t.ky
}

func (t *track) fromXY(x, y float64) (lat, lon float64) {
	return t.lat0 + y/t.ky*rad2deg, wrap180(t.lon0 + x/t.kx*rad2deg)
}

// measurement returns a node's measurements in kinematic units.
type meas struct {
	x, y, z    float64
	rx, ry, rz float64
	vx, vy, vz float64
	rvx, rvy   float64
	rvz        float64
}

func (t *track) meas(nd *node) meas {
	var m meas
	h := nd.h
	if nd.kind&nPosV == 0 {
		h = 0
	}
	if nd.kind&nPosH != 0 {
		m.x, m.y = t.toXY(nd.lat, nd.lon)
		sx, sy := t.scales(nd.lat, h)
		m.rx, m.ry = nd.re/(sx*sx), nd.rn/(sy*sy)
	}
	if nd.kind&nPosV != 0 {
		m.z, m.rz = nd.h, nd.rh
	}
	if nd.kind&nVelH != 0 {
		sx, sy := t.scales(nd.lat, h)
		m.vx, m.vy = nd.ve/sx, nd.vn/sy
		m.rvx, m.rvy = nd.rv/(sx*sx), nd.rv/(sy*sy)
	}
	if nd.kind&nVelV != 0 {
		m.vz, m.rvz = nd.vu, nd.rvu
	}
	return m
}

// prior returns a vague initial state for a filter run starting at node
// nd: position from the first position node at or after it, velocity from
// the first velocity node, both with large variances, so that the result
// is dominated by the measurements (doc.go "Initialisation").
func (t *track) prior(from int) [3]axis {
	var s [3]axis
	px, py, pz := 0.0, 0.0, 0.0
	gotP, gotZ := false, false
	vx, vy, vz := 0.0, 0.0, 0.0
	gotV, gotVZ := false, false
	for i := from; i < t.n && !(gotP && gotZ && gotV && gotVZ); i++ {
		nd := t.at(i)
		m := t.meas(nd)
		if !gotP && nd.kind&nPosH != 0 {
			px, py, gotP = m.x, m.y, true
		}
		if !gotZ && nd.kind&nPosV != 0 {
			pz, gotZ = m.z, true
		}
		if !gotV && nd.kind&nVelH != 0 {
			vx, vy, gotV = m.vx, m.vy, true
		}
		if !gotVZ && nd.kind&nVelV != 0 {
			vz, gotVZ = m.vz, true
		}
	}
	const pPos = 1e8 // (10 km)²
	pv := 1e5        // (316 m/s)²
	if gotV {
		pv = 400 // (20 m/s)²: the first velocity node pins it anyway
	}
	s[0] = axis{x: [3]float64{px, vx, 0}, p: [6]float64{pPos, 0, 0, pv, 0, 25}}
	s[1] = axis{x: [3]float64{py, vy, 0}, p: [6]float64{pPos, 0, 0, pv, 0, 25}}
	pvz := 100.0
	if !gotVZ {
		pvz = 400
	}
	s[2] = axis{x: [3]float64{pz, vz, 0}, p: [6]float64{pPos, 0, 0, pvz, 0, 4}}
	return s
}

// step applies node nd to a state that is valid at time t0s.
func (t *track) step(s *[3]axis, t0s float64, nd *node, qh, qv float64) {
	dt := nd.t - t0s
	s[0].predict(dt, qh)
	s[1].predict(dt, qh)
	s[2].predict(dt, qv)
	m := t.meas(nd)
	if nd.kind&nPosH != 0 {
		s[0].update(0, m.x, m.rx)
		s[1].update(0, m.y, m.ry)
	}
	if nd.kind&nPosV != 0 {
		s[2].update(0, m.z, m.rz)
	}
	if nd.kind&nVelH != 0 {
		s[0].update(1, m.vx, m.rvx)
		s[1].update(1, m.vy, m.rvy)
	}
	if nd.kind&nVelV != 0 {
		s[2].update(1, m.vz, m.rvz)
	}
}

// refilter re-runs the forward filter from node i to the newest node.
func (t *track) refilter(i int, qh, qv float64) {
	if t.n == 0 {
		return
	}
	if i <= 0 {
		t.refilterWith(t.prior(0), qh, qv)
		return
	}
	p := t.at(i - 1)
	t.run(i, p.st, p.t, qh, qv)
}

// refilterWith re-runs the whole window from the given initial state
// (valid at the first node's time).
func (t *track) refilterWith(s [3]axis, qh, qv float64) {
	if t.n > 0 {
		t.run(0, s, t.at(0).t, qh, qv)
	}
}

func (t *track) run(i int, s [3]axis, ts, qh, qv float64) {
	for k := i; k < t.n; k++ {
		nd := t.at(k)
		t.step(&s, ts, nd, qh, qv)
		nd.st = s
		ts = nd.t
	}
}

// insert adds a node in tag order and refilters from it. It returns false
// when the node is older than the whole window (dropped).
func (t *track) insert(nd node, qh, qv float64) bool {
	if t.nodes == nil {
		t.nodes = make([]node, max(t.win, 2))
	}
	if t.n > 0 && nd.t < t.at(0).t {
		return false
	}
	// insertion index: after every node with tag <= nd.t
	i := t.n
	for i > 0 && t.at(i-1).t > nd.t {
		i--
	}
	if t.n == len(t.nodes) {
		if i == 0 {
			return false
		}
		t.start = (t.start + 1) % len(t.nodes)
		t.n--
		i--
	}
	for k := t.n; k > i; k-- {
		*t.at(k) = *t.at(k - 1)
	}
	t.n++
	*t.at(i) = nd
	t.refilter(i, qh, qv)
	return true
}

// reset clears the nodes and sets a new frame.
func (t *track) reset(lat, lon float64) {
	t.start, t.n = 0, 0
	t.setFrame(lat, lon)
}

// recenter moves the kinematic frame origin to (lat, lon). The mapping is
// affine per axis (x' = s·x + o), so states and covariances transform
// exactly.
func (t *track) recenter(lat, lon float64) {
	okx, oky, olat0, olon0 := t.kx, t.ky, t.lat0, t.lon0
	t.setFrame(lat, lon)
	sx := t.kx / okx
	ox := wrap180(olon0-t.lon0) * deg2rad * t.kx
	sy := t.ky / oky
	oy := (olat0 - t.lat0) * deg2rad * t.ky
	for i := 0; i < t.n; i++ {
		st := &t.at(i).st
		scaleAxis(&st[0], sx, ox)
		scaleAxis(&st[1], sy, oy)
	}
}

func scaleAxis(a *axis, s, o float64) {
	a.x[0] = s*a.x[0] + o
	a.x[1] *= s
	a.x[2] *= s
	for i := range a.p {
		a.p[i] *= s * s
	}
}

// predictAt returns the filtered (not smoothed) state at time ts: the
// newest node at or before ts predicted forward. ok is false when ts is
// before the window.
func (t *track) predictAt(ts, qh, qv float64) (s [3]axis, ok bool) {
	j := t.n - 1
	for j >= 0 && t.at(j).t > ts {
		j--
	}
	if j < 0 {
		return s, false
	}
	nd := t.at(j)
	s = nd.st
	dt := ts - nd.t
	s[0].predict(dt, qh)
	s[1].predict(dt, qh)
	s[2].predict(dt, qv)
	return s, true
}

// smoothedAt returns the smoothed state at time ts (fixed-interval RTS over
// the window, evaluated lazily from the newest node back to ts) and the
// mode of the answer.
func (t *track) smoothedAt(ts, qh, qv float64) (s [3]axis, mode int) {
	j := t.n - 1
	for j >= 0 && t.at(j).t > ts {
		j--
	}
	if j == t.n-1 { // at or after the newest node: forward prediction
		nd := t.at(j)
		s = nd.st
		dt := ts - nd.t
		s[0].predict(dt, qh)
		s[1].predict(dt, qh)
		s[2].predict(dt, qv)
		return s, modeExtrapolated
	}
	// smoothed state of node j+1 by the backward recursion from the newest
	sm := t.last().st
	for k := t.n - 2; k >= j+1 && k >= 0; k-- {
		nd, nx := t.at(k), t.at(k+1)
		dt := nx.t - nd.t
		sm[0] = smooth(&nd.st[0], dt, qh, &sm[0])
		sm[1] = smooth(&nd.st[1], dt, qh, &sm[1])
		sm[2] = smooth(&nd.st[2], dt, qv, &sm[2])
	}
	if j < 0 { // before the oldest node: backward prediction
		dt := ts - t.at(0).t
		s = sm
		s[0].predict(dt, qh)
		s[1].predict(dt, qh)
		s[2].predict(dt, qv)
		return s, modeBackward
	}
	// pseudo-node at ts: filtered prediction from node j, smoothed with j+1
	nd, nx := t.at(j), t.at(j+1)
	f := nd.st
	d0 := ts - nd.t
	f[0].predict(d0, qh)
	f[1].predict(d0, qh)
	f[2].predict(d0, qv)
	d1 := nx.t - ts
	s[0] = smooth(&f[0], d1, qh, &sm[0])
	s[1] = smooth(&f[1], d1, qh, &sm[1])
	s[2] = smooth(&f[2], d1, qv, &sm[2])
	return s, modeInterpolated
}

const (
	modeInterpolated = iota
	modeExtrapolated
	modeBackward
)

// posAge returns the time from ts to the nearest node with a horizontal
// position (s; +Inf when there is none).
func (t *track) posAge(ts float64) float64 {
	best := math.Inf(1)
	for i := 0; i < t.n; i++ {
		nd := t.at(i)
		if nd.kind&nPosH != 0 {
			if d := math.Abs(nd.t - ts); d < best {
				best = d
			}
		}
	}
	return best
}

// lastPos returns the newest horizontal position node.
func (t *track) lastPos() *node {
	for i := t.n - 1; i >= 0; i-- {
		if nd := t.at(i); nd.kind&nPosH != 0 {
			return nd
		}
	}
	return nil
}

// retag applies the current latency estimate to the position nodes of
// T=0 reports, re-sorts and refilters the whole window.
func (t *track) retag(qh, qv float64) {
	t.lBuilt = t.lHat
	for i := 0; i < t.n; i++ {
		nd := t.at(i)
		if nd.kind&(nPosH|nPosV) != 0 && nd.kind&(nT1|nExact) == 0 {
			nd.t = nd.rawT - t.lBuilt
		}
	}
	// insertion sort (the window is small and nearly sorted)
	for i := 1; i < t.n; i++ {
		for k := i; k > 0 && t.at(k-1).t > t.at(k).t; k-- {
			*t.at(k - 1), *t.at(k) = *t.at(k), *t.at(k - 1)
		}
	}
	t.refilter(0, qh, qv)
}
