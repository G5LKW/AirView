package latency

// Independent reference for the latency chain. Nothing here reuses the
// package's geometry, epoch or time-base helpers: the truth is generated with
// its own WGS84 ECEF / inverse, a numerically integrated refractive path, a
// brute-force epoch search written from the wording of DO-260A A.1.4.2.3.1,
// and its own midnight arithmetic.

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

const (
	refA  = 6378137.0
	refF  = 1 / 298.257223563
	refC  = 299792458.0
	refFt = 0.3048
)

var refE2 = refF * (2 - refF)

func refToECEF(lat, lon, h float64) [3]float64 {
	la, lo := lat*math.Pi/180, lon*math.Pi/180
	n := refA / math.Sqrt(1-refE2*math.Sin(la)*math.Sin(la))
	return [3]float64{(n + h) * math.Cos(la) * math.Cos(lo), (n + h) * math.Cos(la) * math.Sin(lo), (n*(1-refE2) + h) * math.Sin(la)}
}

// refFromECEF: iterative inverse (converges to < 1 mm in a few rounds).
func refFromECEF(p [3]float64) (lat, lon, h float64) {
	lon = math.Atan2(p[1], p[0])
	r := math.Hypot(p[0], p[1])
	la := math.Atan2(p[2], r*(1-refE2))
	for i := 0; i < 8; i++ {
		n := refA / math.Sqrt(1-refE2*math.Sin(la)*math.Sin(la))
		h = r/math.Cos(la) - n
		la = math.Atan2(p[2], r*(1-refE2*n/(n+h)))
	}
	n := refA / math.Sqrt(1-refE2*math.Sin(la)*math.Sin(la))
	h = r/math.Cos(la) - n
	return la * 180 / math.Pi, lon * 180 / math.Pi, h
}

// refPathNs integrates (1+N(h)e-6)/c along the straight chord, N(h) =
// 315·exp(-h/7350) (ITU-R P.453 reference atmosphere), h the true geodetic
// height of each chord point (the chord dips below the end heights).
func refPathNs(a, b [3]float64) (distM, ns float64) {
	d := [3]float64{b[0] - a[0], b[1] - a[1], b[2] - a[2]}
	distM = math.Sqrt(d[0]*d[0] + d[1]*d[1] + d[2]*d[2])
	if distM == 0 {
		return 0, 0
	}
	const steps = 400
	var s float64
	for i := 0; i <= steps; i++ {
		u := float64(i) / steps
		_, _, h := refFromECEF([3]float64{a[0] + u*d[0], a[1] + u*d[1], a[2] + u*d[2]})
		// The ray is an arc of radius ~4R (standard refraction gradient
		// -40 N/km) bulging above the chord.
		h += distM * distM * u * (1 - u) / (2 * 4 * 6371000)
		if h < 0 {
			h = 0
		}
		w := 1.0
		if i == 0 || i == steps {
			w = 0.5
		}
		s += w * (1 + 315*math.Exp(-h/7000)*1e-6) // atmosphere of internal/mlat/geo (DESIGN §9.1)
	}
	rho := 4 * 6371000.0
	arc := 2 * rho * math.Asin(distM/(2*rho)) // exact arc length over the chord
	return distM, s / steps * arc / refC * 1e9
}

// refEpoch: brute-force nearest "even/odd-numbered 0.2 s epoch" - an even
// (odd) number of 200 ms intervals after an even-numbered UTC second - to
// the reception time, from the wording of the standard. UTC second parity is
// taken from seconds of day (a UTC day has 86400 s, an even number).
func refEpoch(recvUnixNs int64, odd bool) int64 {
	sec := recvUnixNs / 1e9
	best, bestD := int64(0), int64(math.MaxInt64)
	for s := sec - 3; s <= sec+3; s++ {
		sod := ((s % 86400) + 86400) % 86400
		if sod%2 != 0 {
			continue
		}
		for j := int64(0); j < 10; j++ {
			if (j%2 == 1) != odd {
				continue
			}
			e := s*1e9 + j*200e6
			d := e - recvUnixNs
			if d < 0 {
				d = -d
			}
			if d < bestD {
				best, bestD = e, d
			}
		}
	}
	return best
}

// refGPS: seconds-of-day nanoseconds of an absolute Unix ns instant.
func refGPS(unixNs int64) int64 {
	const day = int64(86400e9)
	return ((unixNs % day) + day) % day
}

// refAircraft flies a straight great-circle-free line in the local tangent
// plane of its start point (ECEF east/north vectors) at constant height.
type refAircraft struct {
	icao    uint32
	t0      int64
	lat0    float64
	lon0    float64
	altFt   float64
	gsKt    float64
	trk     float64
	t1      bool
	e, n, o [3]float64   // east, north unit vectors, origin ECEF
	cp      [][2]float64 // loxodrome checkpoints every 1 s (rad)
}

func newRefAircraft(icao uint32, t0 int64, lat, lon, altFt, gs, trk float64, t1 bool) *refAircraft {
	a := &refAircraft{icao: icao, t0: t0, lat0: lat, lon0: lon, altFt: altFt, gsKt: gs, trk: trk, t1: t1}
	la, lo := lat*math.Pi/180, lon*math.Pi/180
	a.e = [3]float64{-math.Sin(lo), math.Cos(lo), 0}
	a.n = [3]float64{-math.Sin(la) * math.Cos(lo), -math.Sin(la) * math.Sin(lo), math.Cos(la)}
	a.o = refToECEF(lat, lon, altFt*refFt)
	return a
}

// at returns the true position at absolute time t (ns) for level flight at
// constant true track and ground speed (GNSS horizontal velocity at the
// aircraft's height): the loxodrome on the WGS84 ellipsoid raised by h,
// integrated with RK4 from 1 s checkpoints.
func (a *refAircraft) at(t int64) (lat, lon float64, ecef [3]float64) {
	h := a.altFt * refFt
	v := a.gsKt * 1852 / 3600
	st, ct := math.Sincos(a.trk * math.Pi / 180)
	deriv := func(la float64) (dla, dlo float64) { // rad/s
		s := math.Sin(la)
		w := 1 - refE2*s*s
		n := refA / math.Sqrt(w)
		m := n * (1 - refE2) / w
		return v * ct / (m + h), v * st / ((n + h) * math.Cos(la))
	}
	step := func(la, lo, dt float64) (float64, float64) {
		k1a, k1o := deriv(la)
		k2a, k2o := deriv(la + dt/2*k1a)
		k3a, k3o := deriv(la + dt/2*k2a)
		k4a, k4o := deriv(la + dt*k3a)
		return la + dt/6*(k1a+2*k2a+2*k3a+k4a), lo + dt/6*(k1o+2*k2o+2*k3o+k4o)
	}
	if len(a.cp) == 0 {
		a.cp = append(a.cp, [2]float64{a.lat0 * math.Pi / 180, a.lon0 * math.Pi / 180})
	}
	k := (t - a.t0) / 1e9
	if k < 0 {
		k = 0
	}
	for int64(len(a.cp)) <= k {
		p := a.cp[len(a.cp)-1]
		la, lo := p[0], p[1]
		for i := 0; i < 10; i++ {
			la, lo = step(la, lo, 0.1)
		}
		a.cp = append(a.cp, [2]float64{la, lo})
	}
	p := a.cp[k]
	frac := float64(t-a.t0-k*1e9) / 1e9
	la, lo := p[0], p[1]
	for n := int(math.Ceil(math.Abs(frac) / 0.1)); n > 0; n-- {
		la, lo = step(la, lo, frac/float64(int(math.Ceil(math.Abs(frac)/0.1))))
	}
	lat, lon = la*180/math.Pi, lo*180/math.Pi
	return lat, lon, refToECEF(lat, lon, h)
}

// atTangent moves s = gs * (t - t0) along the start point's tangent plane and
// re-projects to the flight level (used to place aircraft at a distance).
func (a *refAircraft) atTangent(t int64) (lat, lon float64, ecef [3]float64) {
	s := a.gsKt * 1852 / 3600 * float64(t-a.t0) / 1e9
	st, ct := math.Sincos(a.trk * math.Pi / 180)
	var p [3]float64
	for i := range p {
		p[i] = a.o[i] + s*(st*a.e[i]+ct*a.n[i])
	}
	lat, lon, _ = refFromECEF(p)
	return lat, lon, refToECEF(lat, lon, a.altFt*refFt)
}

type refEvent struct {
	ac        *refAircraft
	tx        int64 // true transmit (start of preamble) UTC ns
	age       float64
	odd       bool
	epoch     int64 // T=1 truth
	recvDelay int64 // true UTC host read - TOA(start of frame)
	proc      int64
	hostOff   time.Duration // true UTC - host clock
	gpsShift  int64         // receiver timestamp error (legacy +1 s, GPS-time +18 s)
	df        uint8
	kind      model.FrameKind
	noiseE    float64
	noiseN    float64
	lat, lon  float64 // reported (NaN allowed)
	propNs    float64 // truth
	toa       int64   // truth, start of frame
}

type refRig struct {
	rx      [3]float64
	rxLat   float64
	rxLon   float64
	rxH     float64
	a       *Analyzer
	clk     *refClock
	maxProp float64
	maxL    float64
}

type refClock struct {
	off    time.Duration
	synced bool
}

func (c *refClock) Offset() time.Duration { return c.off }
func (c *refClock) Synced() bool          { return c.synced }

func newRefRig(t *testing.T) *refRig {
	r := &refRig{rxLat: 47.2601, rxLon: 11.3437, rxH: 612, clk: &refClock{synced: true}}
	r.rx = refToECEF(r.rxLat, r.rxLon, r.rxH)
	r.a = New(Options{})
	r.a.SetReceiver(r.rxLat, r.rxLon, r.rxH)
	r.a.SetClock(r.clk, nil)
	return r
}

// make fills the truth of ev (tx, epoch, positions, propagation, TOA) and
// returns the Obs the tracker would deliver.
func (r *refRig) make(ev *refEvent) Obs {
	ac := ev.ac
	if ac.t1 {
		// Register loaded with a position extrapolated to the epoch; sent
		// up to +-150 ms around it.
		ev.epoch = refEpoch(ev.tx, ev.odd)
		lat, lon, _ := ac.at(ev.epoch)
		ev.lat, ev.lon = lat, lon
	} else {
		lat, lon, _ := ac.at(ev.tx - int64(ev.age*1e9))
		ev.lat, ev.lon = lat, lon
	}
	if ev.noiseE != 0 || ev.noiseN != 0 {
		la := ev.lat * math.Pi / 180
		ev.lat += ev.noiseN / 6367000 * 180 / math.Pi
		ev.lon += ev.noiseE / (6390000 * math.Cos(la)) * 180 / math.Pi
	}
	_, _, p := ac.at(ev.tx) // where the aircraft really is when it transmits
	_, ev.propNs = refPathNs(r.rx, p)
	ev.toa = ev.tx + int64(math.Round(ev.propNs))
	df := ev.df
	if df == 0 {
		df = 17
	}
	end := ev.toa
	switch {
	case ev.kind == model.FrameModeAC:
		end += 20300
	case df >= 16:
		end += 120000
	default:
		end += 64000
	}
	o := NewObs()
	hostTrue := ev.toa + ev.recvDelay
	o.RecvHost = time.Unix(0, hostTrue-int64(ev.hostOff))
	o.Handled = o.RecvHost.Add(time.Duration(ev.proc))
	o.GPSNs = refGPS(end + ev.gpsShift)
	o.FeedID = "rc"
	o.ICAO = ac.icao
	o.DF = df
	o.Kind = ev.kind
	o.TC = 11
	if ac.t1 {
		o.TC = 9
	}
	o.IsPosition = true
	o.TFlag, o.CPROdd = ac.t1, ev.odd
	o.Lat, o.Lon, o.AltFt, o.GS, o.Track = ev.lat, ev.lon, ac.altFt, ac.gsKt, ac.trk
	return o
}

// check compares the analyzer's view of the last message with the truth.
func (r *refRig) check(t *testing.T, ev *refEvent, wantAbs bool) {
	t.Helper()
	i, ok := r.a.acIdx[ev.ac.icao]
	if !ok {
		t.Fatalf("aircraft %06x missing", ev.ac.icao)
	}
	lm := r.a.ac[i].lastMsg
	if !lm.hasTOA || lm.toa != ev.toa+ev.gpsShift {
		t.Fatalf("TOA %d, want %d (shift %d)", lm.toa, ev.toa+ev.gpsShift, ev.gpsShift)
	}
	if ev.gpsShift == 0 && !ev.isGated(r) && r.clk.off == ev.hostOff {
		if got := lm.hostRecv - lm.toa; got != ev.recvDelay {
			t.Errorf("receiver delay %d, want %d", got, ev.recvDelay)
		}
	}
	if lm.hasHandled && lm.handled-lm.hostRecv != ev.proc {
		t.Errorf("processing %d, want %d", lm.handled-lm.hostRecv, ev.proc)
	}
	if !math.IsNaN(ev.lat) {
		if !lm.hasProp {
			t.Fatalf("no propagation")
		}
		d := math.Abs(lm.prop - ev.propNs)
		r.maxProp = math.Max(r.maxProp, d)
		// Reported position is at the epoch / position age, not at transmit:
		// allow the motion (<= 250 m/s * 0.3 s) plus 0.1 us model error.
		lag := 0.15
		if !ev.ac.t1 {
			lag = ev.age
		}
		lim := 20 + ev.ac.gsKt*1852/3600*lag/refC*1e9
		if d > lim {
			t.Errorf("propagation %.1f ns, truth %.1f ns (diff %.1f > %.1f)", lm.prop, ev.propNs, d, lim)
		}
	}
	if ev.ac.t1 && wantAbs {
		if !lm.hasL {
			t.Fatalf("T=1 message without transponder stage")
		}
		// A GPS-time receiver (+18 s = 90 epochs) shifts the epoch with
		// the timestamps; parity and transmit-epoch stay exact.
		if lm.epoch != ev.epoch+ev.gpsShift {
			t.Fatalf("epoch %d, want %d (tx-epoch %d ms)", lm.epoch, ev.epoch+ev.gpsShift, (ev.tx-ev.epoch)/1e6)
		}
		d := math.Abs(float64(lm.l - (ev.tx - ev.epoch)))
		r.maxL = math.Max(r.maxL, d)
		if d > 1000 { // 1 us
			t.Errorf("transmit-epoch %d, truth %d", lm.l, ev.tx-ev.epoch)
		}
	} else if ev.ac.t1 && lm.hasL {
		t.Errorf("T=1 stage computed from a suspect timestamp: l=%d ms (truth %d ms)", lm.l/1e6, (ev.tx-ev.epoch)/1e6)
	}
}

func (ev *refEvent) isGated(r *refRig) bool {
	return ev.recvDelay < int64(r.a.opt.MinReceiverDelay) || ev.recvDelay > int64(r.a.opt.MaxReceiverDelay)
}

// TestReferenceEpochRule checks NearestEpoch against the brute-force reading
// of the standard over many random instants, including around midnight.
func TestReferenceEpochRule(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200000; i++ {
		base := day0 + int64(rng.IntN(3))*nsPerDay - nsPerDay
		if i%4 == 0 {
			base = day0 - 3e9 // straddle midnight
		}
		recv := base + rng.Int64N(6e9)
		if (recv%100e6+100e6)%100e6 == 0 {
			continue // exact tie between two right-parity epochs: undefined
		}
		odd := rng.IntN(2) == 1
		if got, want := NearestEpoch(recv, odd), refEpoch(recv, odd); got != want {
			t.Fatalf("NearestEpoch(%d, %v) = %d, want %d", recv, odd, got, want)
		}
	}
}

// TestReferenceGeometry: propagation vs the integrated refractive path from 0
// to 500 km, overhead and at the horizon.
func TestReferenceGeometry(t *testing.T) {
	r := newRefRig(t)
	cases := []struct{ distKm, brg, altFt float64 }{
		{0, 0, (612 + 2000) / refFt}, // 0 km ground distance, 2 km overhead
		{0, 0, 612 / refFt},          // co-located with the antenna: 0 m
		{1, 45, 3000}, {50, 90, 10000}, {150, 200, 35000}, {300, 300, 39000},
		{450, 10, 45000}, {500, 135, 40000},
	}
	for _, c := range cases {
		ac := newRefAircraft(1, 0, r.rxLat, r.rxLon, c.altFt, 0, c.brg, false)
		// move c.distKm along the tangent plane then re-project
		ac.gsKt = c.distKm * 1000 / (1852.0 / 3600)
		lat, lon, p := ac.atTangent(1e9)
		distM, want := refPathNs(r.rx, p)
		got, _ := r.a.rx.slant(lat, lon, c.altFt*refFt)
		prop := propagationNs(got, r.rxH, c.altFt*refFt)
		if math.Abs(got-distM) > 0.01 {
			t.Errorf("%v: slant %.3f m, reference %.3f m", c, got, distM)
		}
		if d := math.Abs(prop - want); d > 5 { // 5 ns
			t.Errorf("%v: propagation %.2f ns, integrated reference %.2f ns (diff %.2f)", c, prop, want, d)
		}
		vac := distM / refC * 1e9
		t.Logf("ground %.0f km alt %.0f ft: slant %.3f km, vacuum %.3f us, air %.3f us (+%.1f ns), ref diff %.2f ns",
			c.distKm, c.altFt, distM/1e3, vac/1e3, prop/1e3, prop-vac, prop-want)
	}
}

// TestReferenceChain runs mixed T=1 / T=0 traffic through every hostile case
// and compares each message with independent truth.
func TestReferenceChain(t *testing.T) {
	r := newRefRig(t)
	r.clk.off = 37 * time.Millisecond
	rng := rand.New(rand.NewPCG(7, 11))
	start := day0 - 40*nsPerSec // crosses midnight 40 s in
	fleet := []*refAircraft{
		newRefAircraft(0xA00001, start, r.rxLat, r.rxLon, 2000, 140, 90, true),              // ~0 km
		newRefAircraft(0xA00002, start, r.rxLat+1.5, r.rxLon-2.0, 37000, 460, 210, true),    // ~200 km
		newRefAircraft(0xA00003, start, r.rxLat+4.4, r.rxLon+1.0, 40000, 480, 0, true),      // ~500 km
		newRefAircraft(0xA00004, start, r.rxLat-0.5, r.rxLon+0.7, 24000, 380, 45, false),    // T=0
		newRefAircraft(0xA00005, start, r.rxLat+0.01, r.rxLon-0.01, 1500, 120, 300, false),  // T=0 near
		newRefAircraft(0xA00006, start, r.rxLat-3.0, r.rxLon+3.0, 41000, 500, 315, true),    // ~400 km
		newRefAircraft(0xA00007, start, r.rxLat+0.2, r.rxLon+0.2, 5000, 250, 180, true),     // ~30 km
		newRefAircraft(0xA00008, start, r.rxLat, r.rxLon+6.6, 45000, 450, 90, false),        // ~500 km T=0
		newRefAircraft(0xA00009, start, r.rxLat-0.001, r.rxLon+0.001, 1000, 90, 270, false), // ~0 km T=0
		newRefAircraft(0xA0000A, start, r.rxLat+2.0, r.rxLon+2.0, 33000, 420, 135, true),    // ~270 km
		newRefAircraft(0xA0000B, start, r.rxLat-1.0, r.rxLon-1.0, 30000, 400, 20, false),
		newRefAircraft(0xA0000C, start, r.rxLat+3.5, r.rxLon-3.0, 39000, 470, 250, true),
	}
	type item struct {
		ev   refEvent
		abs  bool
		host int64
	}
	var items []item
	for _, ac := range fleet {
		tx := start + rng.Int64N(5e8)
		odd := false
		for tx < start+80*nsPerSec {
			ev := refEvent{ac: ac, tx: tx, odd: odd, proc: rng.Int64N(2e6) + 1000,
				recvDelay: 2e6 + rng.Int64N(30e6), hostOff: 37 * time.Millisecond,
				age: 0.3 + rng.Float64()*0.2}
			if ac.t1 {
				// Sent within +-150 ms of the epoch its register applies to:
				// pick the epoch first, then the transmit time.
				e := refEpoch(tx, odd)
				ev.tx = e + rng.Int64N(300e6) - 150e6
			}
			if rng.IntN(2) == 0 {
				ev.df = 18
			}
			abs := true
			switch rng.IntN(20) {
			case 0:
				ev.recvDelay = -40e6 // NTP error: negative but within the gate
			case 1:
				ev.gpsShift = nsPerSec // legacy Daysec+1 not corrected upstream
				abs = false
			case 2:
				ev.gpsShift = 18 * nsPerSec // GPS time instead of UTC
			case 3:
				// Network stall, gated. With a synced host the timestamp
				// is indistinguishable from a broken one: kept out of T=1.
				ev.recvDelay = 7 * nsPerSec
				abs = false
			}
			items = append(items, item{ev: ev, abs: abs})
			tx += 400e6 + rng.Int64N(200e6)
			odd = !odd
		}
	}
	for i := range items {
		o := r.make(&items[i].ev)
		items[i].host = o.RecvHost.UnixNano()
	}
	// Deliver in host-read order, with local reordering (multiple feeds /
	// TCP batching): swap neighbours now and then.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].host < items[j-1].host; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	for i := 0; i+1 < len(items); i += 7 {
		items[i], items[i+1] = items[i+1], items[i]
	}
	var mlat, nanPos int
	for i := range items {
		it := &items[i]
		o := r.make(&it.ev)
		r.a.Observe(&o)
		r.check(t, &it.ev, it.abs)
		if i%50 == 0 { // MLAT duplicate: must leave no trace in the stats
			m := o
			m.MLAT, m.GPSNs = true, -1
			r.a.Observe(&m)
			mlat++
		}
		if i%61 == 0 { // undecodable position
			n := o
			n.Lat, n.Lon, n.AltFt, n.GS, n.Track = math.NaN(), math.NaN(), math.NaN(), math.NaN(), math.NaN()
			r.a.Observe(&n)
			nanPos++
		}
	}
	t.Logf("%d messages, %d MLAT, %d NaN; max propagation diff %.1f ns, max T=1 diff %.1f ns",
		len(items), mlat, nanPos, r.maxProp, r.maxL)
	if r.a.c.mlat != int64(mlat) {
		t.Errorf("mlat counter %d, want %d", r.a.c.mlat, mlat)
	}
	if r.a.c.legacy1s == 0 || r.a.c.gpsTS == 0 || r.a.c.negD == 0 || r.a.c.absurdD == 0 {
		t.Errorf("counters not exercised: %+v", r.a.c)
	}
	// Every aggregated T=1 value must lie in the compliant window: a legacy
	// timestamp leaking in would show up as a +-200 ms shift.
	s := r.a.Summary()
	for _, st := range s.Stages {
		if st.Key == "transponder" && (float64(st.MinMs) < -150.001 || float64(st.MaxMs) > 150.001) {
			t.Errorf("transponder stage %v..%v ms outside the generated +-150 ms", st.MinMs, st.MaxMs)
		}
		if st.Key == "receiver" && (float64(st.MinMs) < -40.001 || float64(st.MaxMs) > 32.001) {
			t.Errorf("receiver stage %v..%v ms outside the generated -40..32 ms", st.MinMs, st.MaxMs)
		}
	}
	for _, v := range []any{s, r.a.Histograms(), r.a.Aircraft(), r.a.NTPCheck(), r.a.Findings()} {
		b, err := json.Marshal(v)
		if err != nil || strings.Contains(string(b), "NaN") || strings.Contains(string(b), "Inf") {
			t.Fatalf("JSON: %v %.200s", err, b)
		}
	}
}

// TestReferenceUnsyncedHost: the host clock is 7 s fast and NTP is not
// synced (offset 0). The raw receiver delay is wrong by +7 s: it is neither
// counted as an absurd receiver delay nor recorded until the host offset has
// been estimated from the GPS time stamps, then corrected by it; the
// GPS-only stages (T=1 transponder, propagation) stay exact.
func TestReferenceUnsyncedHost(t *testing.T) {
	r := newRefRig(t)
	r.clk.synced, r.clk.off = false, 0
	ac := newRefAircraft(0xB00001, day0, r.rxLat+1, r.rxLon, 35000, 450, 90, true)
	for i := 0; i < 40; i++ {
		tx := day0 + int64(i)*500e6
		e := refEpoch(tx, i%2 == 1)
		ev := refEvent{ac: ac, tx: e + 60e6, odd: i%2 == 1, recvDelay: 10e6, hostOff: -7 * time.Second, proc: 5e5}
		o := r.make(&ev)
		r.a.Observe(&o)
		r.check(t, &ev, true)
	}
	if r.a.Summary().ClockSynced {
		t.Error("summary claims a synced clock")
	}
	if r.a.c.absurdD != 0 {
		t.Errorf("absurd receiver delays %d, want 0", r.a.c.absurdD)
	}
	s := r.a.Summary()
	if !s.HostOffsetCorrected || math.Abs(float64(s.HostOffsetMs)-7010) > 0.01 {
		t.Errorf("host offset %v ms corrected=%v, want 7010 (7 s + 10 ms receiver floor)", s.HostOffsetMs, s.HostOffsetCorrected)
	}
}

// TestReferenceModeACKind: the frame-end correction follows the frame kind
// (mlat-client: Mode A/C stamped at F2 = 20.3 us; status frames as is).
func TestReferenceModeACKind(t *testing.T) {
	cases := []struct {
		k    model.FrameKind
		df   uint8
		want int64
	}{
		{model.FrameModeAC, 0, 20300}, {model.FrameModeSShort, 11, 64000}, {model.FrameModeSLong, 17, 120000},
		{model.FrameStatus, 0, 0}, {0, 4, 64000}, {0, 20, 120000},
	}
	for _, c := range cases {
		if got := frameAdjustNs(c.k, c.df); got != c.want {
			t.Errorf("kind %d df %d: %d, want %d", c.k, c.df, got, c.want)
		}
	}
	var o Obs
	o.SetFrame(&model.RawFrame{Kind: model.FrameModeSLong, GPSNs: 5, Recv: time.Unix(1, 0)})
	if o.Kind != model.FrameModeSLong {
		t.Error("SetFrame does not carry the frame kind")
	}
}

// refPairTrack flies a T=0 aircraft with a given age sequence and returns the
// analyzer's per-aircraft state.
func refPairTrack(t *testing.T, ages func(i int, tx int64) float64, n int, noiseM float64, trk float64, seed uint64) *acState {
	r := newRefRig(t)
	rng := rand.New(rand.NewPCG(seed, 3))
	ac := newRefAircraft(0xC00001, day0, r.rxLat+0.8, r.rxLon-0.5, 36000, 450, trk, false)
	tx := day0
	for i := 0; i < n; i++ {
		ev := refEvent{ac: ac, tx: tx, odd: i%2 == 1, recvDelay: 5e6, proc: 1e5, age: ages(i, tx),
			noiseE: rng.NormFloat64() * noiseM, noiseN: rng.NormFloat64() * noiseM}
		o := r.make(&ev)
		r.a.Observe(&o)
		tx += 400e6 + rng.Int64N(200e6)
	}
	return &r.a.ac[r.a.acIdx[0xC00001]]
}

// TestReferencePairSign: an age step of +100 ms must give delta = +100 ms at
// that pair (age(this) - age(prev)), using no future data.
func TestReferencePairSign(t *testing.T) {
	for _, trk := range []float64{0, 77, 181, 270, 359} {
		r := newRefRig(t)
		ac := newRefAircraft(0xC00002, day0, r.rxLat-0.3, r.rxLon+0.4, 30000, 420, trk, false)
		tx := day0
		for i := 0; i < 12; i++ {
			age := 0.25
			if i >= 6 {
				age = 0.35
			}
			ev := refEvent{ac: ac, tx: tx, recvDelay: 5e6, proc: 1e5, age: age}
			o := r.make(&ev)
			r.a.Observe(&o)
			d := r.a.ac[r.a.acIdx[0xC00002]].lastMsg.ageDelta
			want := 0.0
			if i == 6 {
				want = 0.1
			}
			if i > 0 && math.Abs(d-want) > 0.0005 {
				t.Errorf("track %v msg %d: delta %.4f s, want %.4f", trk, i, d, want)
			}
			tx += 500e6
		}
	}
}

// TestReferencePairUnbiased: i.i.d. ages uniform in [c, c+P]: the delta mean
// must be ~0 (no sign error, no scale bias), the jitter must recover
// P/sqrt(12) after the noise correction, and the constant c must be invisible.
func TestReferencePairUnbiased(t *testing.T) {
	for _, tc := range []struct{ c, p, noise, trk float64 }{
		{0, 0.2, 0, 90}, {0.5, 0.2, 5, 45}, {0, 0.2, 15, 300}, {0.1, 0.6, 5, 180},
	} {
		ar := rand.New(rand.NewPCG(uint64(tc.trk), 17))
		s := refPairTrack(t, func(i int, _ int64) float64 { return tc.c + tc.p*ar.Float64() }, 3000, tc.noise, tc.trk, 9)
		sig, mean, ok := s.ageModel()
		if !ok {
			t.Fatalf("%+v: no estimate", tc)
		}
		trueSig := tc.p / math.Sqrt(12)
		se := math.Sqrt(2) * trueSig / math.Sqrt(float64(s.ageD.n))
		if math.Abs(s.ageD.mean) > 4*se+0.0005 {
			t.Errorf("%+v: mean delta %.4f s (biased, se %.4f)", tc, s.ageD.mean, se)
		}
		if math.Abs(sig-trueSig)/trueSig > 0.08 {
			t.Errorf("%+v: jitter %.1f ms, truth %.1f ms", tc, sig*1e3, trueSig*1e3)
		}
		t.Logf("c=%.1f P=%.1f noise=%.0f m: pairs %d jitter %.1f ms (truth %.1f), model mean %.1f ms (truth %.1f), delta mean %.2f ms",
			tc.c, tc.p, tc.noise, s.ageD.n, sig*1e3, trueSig*1e3, mean*1e3, (tc.c+tc.p/2)*1e3, s.ageD.mean*1e3)
	}
}

// TestReferenceCorrelatedAgesBias documents the model's blind spot: a 1 Hz
// position source sampled every 0.4-0.6 s gives sawtooth (anti-correlated at
// that lag) ages. The estimator then measures sqrt(var(age_i - age_i-1)/2),
// which here is ~350 ms instead of the true 289 ms: the jitter and the √3·σ
// model mean are OVER-estimated. (For other lag / fix-period ratios the bias
// can have either sign.) The test pins that the estimator does what the
// maths says - it is not a claim of accuracy for correlated sources.
func TestReferenceCorrelatedAgesBias(t *testing.T) {
	phase := rand.New(rand.NewPCG(5, 5)).Float64()
	var ages []float64
	s := refPairTrack(t, func(_ int, tx int64) float64 {
		x := float64(tx-day0)/1e9 + phase
		a := x - math.Floor(x) // age since the last 1 Hz fix
		ages = append(ages, a)
		return a
	}, 2000, 5, 60, 5)
	var w welford
	for i := 1; i < len(ages); i++ {
		w.add(ages[i] - ages[i-1])
	}
	pred := math.Sqrt(w.variance() / 2)
	sig, mean, _ := s.ageModel()
	t.Logf("1 Hz sawtooth ages (truth sigma %.0f ms, mean 500 ms): estimated jitter %.0f ms (predicted %.0f), model mean %.0f ms",
		1e3/math.Sqrt(12), sig*1e3, pred*1e3, mean*1e3)
	if math.Abs(sig-pred)/pred > 0.05 {
		t.Errorf("jitter %.1f ms, predicted %.1f ms", sig*1e3, pred*1e3)
	}
	if sig < 1.1/math.Sqrt(12) {
		t.Errorf("expected over-estimation for this correlated source; doc.go needs revisiting")
	}
}

// TestReferenceHostOneSecondNoNTP: no NTP configured and the host clock
// 1 s fast. The analysis must say so ("host clock offset unknown/large (no
// NTP)") with the estimated offset, not blame the receiver, keep the T=1
// transponder stage exact, and show receiver delays relative to the estimate.
func TestReferenceHostOneSecondNoNTP(t *testing.T) {
	for _, hostOff := range []time.Duration{-time.Second, time.Second} { // true UTC - host
		r := newRefRig(t)
		r.a.SetClock(nil, nil) // no NTP at all
		r.clk.synced, r.clk.off = false, 0
		ac := newRefAircraft(0xB00002, day0, r.rxLat+1, r.rxLon, 35000, 450, 90, true)
		const n = 200
		for i := 0; i < n; i++ {
			tx := day0 + int64(i)*500e6
			e := refEpoch(tx, i%2 == 1)
			ev := refEvent{ac: ac, tx: e + 60e6, odd: i%2 == 1, recvDelay: 3e6 + int64(i%7)*1e6, hostOff: hostOff, proc: 5e5}
			o := r.make(&ev)
			r.a.Observe(&o)
			r.check(t, &ev, hostOff < 0) // host ahead: T=1 stays exact
		}
		s := r.a.Summary()
		wantMs := -float64(hostOff)/1e6 + 3 // host - GPS UTC + receiver floor
		if !s.HostOffsetCorrected || math.Abs(float64(s.HostOffsetMs)-wantMs) > 0.01 {
			t.Errorf("host %v: estimated offset %v ms (corrected %v), want %.0f", -hostOff, s.HostOffsetMs, s.HostOffsetCorrected, wantMs)
		}
		var host *Finding
		for i := range s.Findings {
			f := &s.Findings[i]
			t.Logf("host %+v: %s %s: %s", -hostOff, f.Level, f.Key, f.Text)
			switch f.Key {
			case "hostclock":
				host = f
			case "gpstime", "feed:rc":
				t.Errorf("receiver blamed: %s", f.Text)
			}
			if strings.Contains(f.Text, "legacy") && f.Key != "hostclock" {
				t.Errorf("receiver blamed: %s", f.Text)
			}
		}
		if host == nil || !strings.Contains(host.Text, "Host clock offset unknown/large (no NTP)") ||
			!strings.Contains(host.Text, "T=1 latency not measurable") {
			t.Fatalf("host %v: no host clock finding: %+v", -hostOff, host)
		}
		if !strings.Contains(host.Text, fmt.Sprintf("%.3f", math.Abs(wantMs)/1e3)) {
			t.Errorf("finding does not state the estimated offset %.3f s: %s", wantMs/1e3, host.Text)
		}
		for _, st := range s.Stages {
			switch st.Key {
			case "transponder":
				if hostOff < 0 && st.Count < n-1 {
					t.Errorf("T=1 transponder stage has %d of %d messages", st.Count, n)
				}
			case "receiver":
				// relative to the estimate: floor 0, spread 0..6 ms
				if st.Count == 0 || float64(st.MaxMs) > 7 || float64(st.MinMs) < -0.01 {
					t.Errorf("receiver stage %+v not relative", st)
				}
			}
		}
		nc := r.a.NTPCheck()
		for _, is := range nc.Issues {
			if strings.Contains(is, "receiver time stamps") {
				t.Errorf("NTP check blames the receiver: %s", is)
			}
		}
	}
}
