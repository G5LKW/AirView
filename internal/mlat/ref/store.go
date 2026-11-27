package ref

import (
	"math"
	"sync"
	"sync/atomic"

	"airview/internal/mlat/geo"
	"airview/internal/modes"
)

// Result is the outcome of Store.Observe for one message.
type Result uint8

const (
	// Ignored: not an ADS-B message of the aircraft itself (TIS-B, ADS-R,
	// Mode S), a bad or corrected CRC, or a type code without track data.
	Ignored Result = iota
	// Noted: quality, version or altitude information was taken.
	Noted
	// Pending: a position that cannot be decoded yet (waiting for the
	// other CPR format).
	Pending
	// Accepted: a position or velocity was added to the track.
	Accepted
	// Started: a new track was started (or restarted after a gap).
	Started
	// Rejected: a position or velocity failed the gates.
	Rejected
	// Jumped: the track was re-initialised at a new, consistent position.
	Jumped
	// TooOld: the message is older than the track's window.
	TooOld
)

var resultNames = [...]string{"ignored", "noted", "pending", "accepted", "started", "rejected", "jumped", "too-old"}

func (r Result) String() string {
	if int(r) < len(resultNames) {
		return resultNames[r]
	}
	return "?"
}

const nShards = 64

type shard struct {
	mu sync.Mutex
	m  map[uint32]*track
}

// Store holds the reference tracks of all ADS-B aircraft and the GNSS−baro
// field. It is safe for concurrent use; tracks are sharded by address.
type Store struct {
	opt    Options
	field  *Field
	shards [nShards]shard

	nObs, nAcc, nRej, nJump, nStart, nVelRej atomic.Uint64
	nReply, nReplyRej, nPair                 atomic.Uint64
}

// New returns an empty store. wx (may be nil) feeds the fallback model of
// the GNSS−baro field.
func New(opt Options, wx Weather) *Store {
	opt = opt.withDefaults()
	s := &Store{opt: opt, field: NewField(opt.Field, wx)}
	for i := range s.shards {
		s.shards[i].m = make(map[uint32]*track)
	}
	return s
}

// Options returns the effective options.
func (s *Store) Options() Options { return s.opt }

// Field returns the GNSS−baro field.
func (s *Store) Field() *Field { return s.field }

func (s *Store) shard(key uint32) *shard {
	h := key * 0x9E3779B1
	return &s.shards[h>>26]
}

// info is what is known about an aircraft apart from its kinematics.
type info struct {
	version            uint8
	hasVersion         bool
	nacp               uint8
	hasNACp            bool
	nicA, nicC         bool
	nic                uint8
	hasNIC             bool
	nucpNACp           uint8
	baroFt             float64
	baroNs             int64
	hasBaro            bool
	deltaM             float64
	deltaNs            int64
	deltaBaroFt        float64
	hasDelta           bool
	gnssM              float64 // last reported GNSS height (TC 20-22), m HAE
	gnssNs             int64
	hasGNSS            bool
	lastVNs            int64   // last vertical measurement taken from an altitude reply
	hBiasVar           float64 // variance of the height bias of the latest height
	altSrc             AltSource
	cpr                [2]cprRec
	ground             bool
	lastSeen           int64
	facc               fieldAcc
	t1                 bool
	lastPosNs          int64
	nicSuppHasVersion2 bool
}

type cprRec struct {
	lat, lon uint32
	ns       int64
	ok       bool
}

type fieldAcc struct {
	k            cellKey
	sum, baroSum float64
	lat, lon     float64
	n            int
	t0           int64
	ok           bool
}

// qual is the integrity state of a track.
type qual struct {
	positions, velocities, rejected, velRejected uint64
	accepted                                     int
	velSeen                                      bool
	cand                                         [4]obs
	candGlobal                                   [4]bool
	nCand                                        int
	rejects                                      [8]int64
	nRejects                                     int
	jumps                                        [4]int64
	nJumps                                       int
	velBad                                       [4]int64
	nVelBad                                      int
	horizonBad                                   [4]int64
	nHorizonBad                                  int
	probationUntil                               int64
	suspendUntil                                 int64
	unstableUntil                                int64
	velBadUntil                                  int64
	horizonUntil                                 int64
	t1Bad                                        int
	t1Untrusted                                  bool
}

func pushTime(ring []int64, n *int, t int64) {
	copy(ring[1:], ring[:len(ring)-1])
	ring[0] = t
	if *n < len(ring) {
		*n++
	}
}

func countSince(ring []int64, n int, since int64) int {
	c := 0
	for i := 0; i < n; i++ {
		if ring[i] >= since {
			c++
		}
	}
	return c
}

// Observe adds one decoded message of an aircraft, transmitted at txNs
// (network time ≈ UTC, ns) with 1σ txSigmaNs. DF17 and DF18 ADS-B
// messages with a clean parity build the track; DF0/4/16/20 altitude replies
// of an aircraft with a track feed its vertical channel (observeAltReply);
// everything else is Ignored. The
// caller decodes the frame once (modes.Decode) and passes the same Message
// to the tracker, the grouper and here.
func (s *Store) Observe(m *modes.Message, txNs int64, txSigmaNs float64) Result {
	if m.DF == 0 || m.DF == 4 || m.DF == 16 || m.DF == 20 {
		return s.observeAltReply(m, txNs)
	}
	if (m.DF != 17 && m.DF != 18) || m.Source != modes.SourceADSB || m.CRC != modes.CRCOK || m.Valid&modes.FTypeCode == 0 {
		return Ignored
	}
	tc := m.TC
	useful := (tc >= 5 && tc <= 22) || tc == 29 || tc == 31 || tc == 0
	if !useful {
		return Ignored
	}
	s.nObs.Add(1)
	key := m.AddrKey()
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil {
		tr = &track{icao: key, t0: txNs, win: s.opt.Window}
		tr.lHat, tr.lBuilt = s.opt.DefaultLatencyS, s.opt.DefaultLatencyS
		tr.lVar = s.opt.LatencySigmaS * s.opt.LatencySigmaS
		sh.m[key] = tr
	}
	if txNs > tr.info.lastSeen {
		tr.info.lastSeen = txNs
	}
	switch {
	case tc == 31 || tc == 29:
		return s.obsStatus(tr, m)
	case tc >= 5 && tc <= 8:
		tr.info.ground = true
		tr.q.accepted = 0
		return Noted
	case tc == 19:
		return s.obsVelocity(tr, m, txNs)
	default:
		return s.obsPosition(tr, m, txNs, txSigmaNs)
	}
}

// ObserveFrame decodes a frame and calls Observe.
func (s *Store) ObserveFrame(frame []byte, txNs int64, txSigmaNs float64) Result {
	var m modes.Message
	if err := modes.Decode(frame, &m); err != nil {
		return Ignored
	}
	return s.Observe(&m, txNs, txSigmaNs)
}

func (s *Store) obsStatus(tr *track, m *modes.Message) Result {
	in := &tr.info
	if m.Valid&modes.FVersion != 0 {
		in.version, in.hasVersion = m.Version, true
	}
	if m.Valid&modes.FNACp != 0 {
		in.nacp, in.hasNACp = m.NACp, true
	}
	if m.Valid&modes.FNICSuppA != 0 {
		in.nicA = m.NICSuppA
	}
	if m.Valid&modes.FNICSuppC != 0 {
		in.nicC = m.NICSuppC
	}
	return Noted
}

// nucpToNACp maps a version-0 NUCp to the NACp whose EPU bound covers the
// NUCp's 95 % accuracy bound (doc.go "Quality").
func nucpToNACp(nucp int) uint8 {
	switch {
	case nucp >= 9:
		return 10
	case nucp == 8:
		return 9
	case nucp == 7:
		return 8
	case nucp == 6:
		return 7
	case nucp == 5:
		return 6
	}
	return 0
}

// nearestEpoch returns the 0.2 s UTC epoch of the given parity nearest to
// tNs (DO-260A Appendix A §A.1.4.2.3.1; the same rule as
// latency.NearestEpoch).
func nearestEpoch(tNs int64, odd bool) int64 {
	const ep = 200_000_000
	k := floorDiv(tNs+ep/2, ep)
	if (k&1 == 1) != odd {
		if tNs >= k*ep {
			k++
		} else {
			k--
		}
	}
	return k * ep
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// cprSigma returns the 1σ of the airborne CPR quantisation (m) in east and
// north at a latitude: the cell size over √12.
func cprSigma(lat float64, odd bool) (se, sn float64) {
	dlat := 360.0 / 60
	if odd {
		dlat = 360.0 / 59
	}
	nl := modes.NL(lat)
	ni := nl
	if odd {
		ni--
	}
	if ni < 1 {
		ni = 1
	}
	dlon := 360.0 / float64(ni)
	nr, mr := radii(lat)
	const cells = 131072.0
	sn = dlat / cells * deg2rad * mr / math.Sqrt(12)
	se = dlon / cells * deg2rad * nr * math.Cos(lat*deg2rad) / math.Sqrt(12)
	return
}

// height derives the height of a position report and the variances of its
// white and bias parts.
func (s *Store) height(tr *track, m *modes.Message, lat, lon float64, txNs int64) (h, rWhite, rBias float64, src AltSource, ok bool) {
	in := &tr.info
	const q25 = 25 * ftM / 3.4641016151377544 // 25 ft / √12
	if m.Valid&modes.FAltGNSS != 0 {
		h = float64(m.AltGNSS) * ftM
		return h, q25 * q25, 3 * 3, AltGNSS, true
	}
	if m.Valid&modes.FAltBaro == 0 {
		return 0, 0, 0, AltNone, false
	}
	h, rWhite, rBias, src = s.heightBaro(in, float64(m.AltBaro), m.AltBaroGillham, lat, lon, txNs)
	return h, rWhite, rBias, src, true
}

// heightBaro converts a pressure altitude (ft) of the aircraft into a
// height above the ellipsoid: with its own recent GNSS−baro difference, else
// the field. It returns the white (quantisation) and bias variances.
func (s *Store) heightBaro(in *info, b float64, gillham bool, lat, lon float64, txNs int64) (h, rWhite, rBias float64, src AltSource) {
	const q25 = 25 * ftM / 3.4641016151377544 // 25 ft / √12
	qb := q25
	if gillham {
		qb = 4 * q25
	}
	if in.hasDelta && txNs-in.deltaNs <= 30e9 && math.Abs(b-in.deltaBaroFt) <= 1000 {
		d := math.Abs(b-in.deltaBaroFt) * ftM * 0.03
		h = b*ftM + in.deltaM
		return h, qb * qb, q25*q25 + d*d, AltOwnDelta
	}
	dm, sd, src := s.field.Offset(lat, lon, b, txNs)
	return b*ftM + dm, qb * qb, sd * sd, src
}

func (s *Store) obsPosition(tr *track, m *modes.Message, txNs int64, txSig float64) Result {
	in, q, o := &tr.info, &tr.q, &s.opt
	if m.Valid&modes.FAltBaro != 0 {
		in.baroFt, in.baroNs, in.hasBaro = float64(m.AltBaro), txNs, true
	}
	if m.Valid&modes.FAltGNSS != 0 && txNs >= in.gnssNs {
		in.gnssM, in.gnssNs, in.hasGNSS = float64(m.AltGNSS)*ftM, txNs, true
	}
	if (m.Valid&(modes.FAltGNSS|modes.FAltBaro) != 0) && tr.n > 0 {
		// TC 20-22 GNSS height with a pressure altitude from a nearby
		// Mode S reply (or the reverse): GNSS−baro where pairable
		s.pairGNSS(tr, tr.last().st[2].x[1], txNs)
	}
	if m.Valid&modes.FCPR == 0 || m.CPRSurface {
		return Noted
	}
	in.ground = false
	if nic, _, ok := m.NICRc(int(in.version), in.nicA, in.nicC); ok {
		in.nic, in.hasNIC = uint8(nic), true
	}
	if nucp, ok := modes.NUCp(m.TC); ok {
		in.nucpNACp = nucpToNACp(nucp)
	}

	// time tag (doc.go "Time tags")
	t1 := m.CPRTime && !q.t1Untrusted
	rawT := tr.sec(txNs)
	tag := rawT - tr.lBuilt
	if t1 {
		e := nearestEpoch(txNs, m.CPROdd)
		lim := 0.150e9 + 3*txSig + 10e6
		if d := math.Abs(float64(txNs - e)); d > lim {
			q.t1Bad++
			if q.t1Bad >= 3 {
				q.t1Untrusted = true
				q.probationUntil = max(q.probationUntil, txNs+int64(o.ProbationS*1e9))
				// re-tag the T=1 nodes already in the window as T=0
				for i := 0; i < tr.n; i++ {
					tr.at(i).kind &^= nT1
				}
				tr.retag(o.QHorizontal, o.QVertical)
			}
			t1 = false
		} else {
			tag = tr.sec(e)
			in.t1 = true
		}
	}

	odd := 0
	if m.CPROdd {
		odd = 1
	}
	in.cpr[odd] = cprRec{lat: m.CPRLat, lon: m.CPRLon, ns: txNs, ok: true}
	other := in.cpr[1-odd]
	var gLat, gLon float64
	gOK := false
	if other.ok && math.Abs(float64(txNs-other.ns)) <= o.PairMaxS*1e9 {
		e, od := in.cpr[0], in.cpr[1]
		gLat, gLon, gOK = modes.CPRAirborneGlobal(e.lat, e.lon, od.lat, od.lon, odd == 1)
	}

	lp := tr.lastPos()
	have := lp != nil && math.Abs(tag-lp.t) <= o.StaleS
	var lat, lon float64
	var pred [3]axis
	if have {
		var ok bool
		pred, ok = tr.predictAt(tag, o.QHorizontal, o.QVertical)
		if !ok {
			return TooOld
		}
		plat, plon := tr.fromXY(pred[0].x[0], pred[1].x[0])
		lat, lon, ok = modes.CPRAirborneLocal(plat, plon, m.CPRLat, m.CPRLon, odd == 1)
		if !ok && !gOK {
			return Pending
		}
		if gOK && (!ok || surfDist(lat, lon, gLat, gLon) > 1000) {
			lat, lon = gLat, gLon
		}
	} else {
		if !gOK {
			return Pending
		}
		lat, lon = gLat, gLon
	}

	nd := node{obs: obs{t: tag, rawT: rawT, kind: nPosH, lat: lat, lon: lon}}
	if t1 {
		nd.kind |= nT1
	}
	se, sn := cprSigma(lat, odd == 1)
	w := o.PosWhiteSigmaM
	nd.re, nd.rn = se*se+w*w, sn*sn+w*w
	h, rw, rb, src, hok := s.height(tr, m, lat, lon, txNs)
	if hok {
		nd.kind |= nPosV
		nd.h, nd.rh = h, rw
	}
	q.positions++

	if !have {
		// (re)start from a global decode
		tr.reset(lat, lon)
		tr.insert(nd, o.QHorizontal, o.QVertical)
		q.accepted, q.velSeen, q.nCand = 1, false, 0
		if hok {
			in.hBiasVar, in.altSrc = rb, src
		}
		in.lastPosNs = txNs
		s.nStart.Add(1)
		return Started
	}

	// gate (doc.go "Integrity")
	x, y := tr.toXY(lat, lon)
	sx, sy := tr.scales(lat, nd.h)
	nx, ny := x-pred[0].x[0], y-pred[1].x[0]
	mx, my := nx*sx, ny*sy
	sxv := pred[0].p[0]*sx*sx + nd.re
	syv := pred[1].p[0]*sy*sy + nd.rn
	d2 := mx*mx/sxv + my*my/syv
	dist := math.Hypot(mx, my)
	bad := d2 > o.GateChi2 && dist > o.GateFloorM
	if dt := math.Abs(tag - lp.t); dt > 0.2 && surfDist(lat, lon, lp.lat, lp.lon)/dt > o.MaxSpeedMps {
		bad = true
	}
	if bad {
		return s.reject(tr, nd, gOK && surfDist(lat, lon, gLat, gLon) < 100, txNs)
	}
	q.nCand = 0
	if hok {
		nz := nd.h - pred[2].x[0]
		sz := pred[2].p[0] + nd.rh + rb
		if nz*nz/sz > 25 && math.Abs(nz) > 100 {
			nd.kind &^= nPosV
		} else {
			in.hBiasVar, in.altSrc = rb, src
		}
	}
	// keep the kinematic frame near the aircraft
	if math.Abs(lat-tr.lat0) > 1 || math.Abs(wrap180(lon-tr.lon0))*math.Cos(lat*deg2rad) > 1 {
		tr.recenter(lat, lon)
	}
	if !tr.insert(nd, o.QHorizontal, o.QVertical) {
		return TooOld
	}
	q.accepted++
	if txNs > in.lastPosNs {
		in.lastPosNs = txNs
	}
	s.nAcc.Add(1)
	return Accepted
}

// reject handles a position that failed the gates: it may be the first of a
// jump to a new, consistent position (re-initialisation) or part of a
// conflict (duplicate address, spoofing).
func (s *Store) reject(tr *track, nd node, global bool, txNs int64) Result {
	q, o := &tr.q, &s.opt
	q.rejected++
	s.nRej.Add(1)
	pushTime(q.rejects[:], &q.nRejects, txNs)
	win := txNs - int64(o.UnstableWindowS*1e9)
	if countSince(q.rejects[:], q.nRejects, win) >= o.ConflictRejects {
		q.unstableUntil = txNs + int64(o.SuspendS*1e9)
		q.suspendUntil = max(q.suspendUntil, q.unstableUntil)
	}
	// candidate chain: consecutive rejected positions consistent with each
	// other
	if q.nCand > 0 {
		pc := &q.cand[q.nCand-1]
		dt := math.Abs(nd.t - pc.t)
		if surfDist(nd.lat, nd.lon, pc.lat, pc.lon) > o.MaxSpeedMps*dt+2000 {
			q.nCand = 0
		}
	}
	if q.nCand == len(q.cand) {
		copy(q.cand[:], q.cand[1:])
		copy(q.candGlobal[:], q.candGlobal[1:])
		q.nCand--
	}
	q.cand[q.nCand], q.candGlobal[q.nCand] = nd.obs, global
	q.nCand++
	anyGlobal := false
	for i := 0; i < q.nCand; i++ {
		anyGlobal = anyGlobal || q.candGlobal[i]
	}
	if q.nCand < o.JumpConfirm || !anyGlobal {
		return Rejected
	}
	// jump: re-initialise at the candidates
	s.nJump.Add(1)
	pushTime(q.jumps[:], &q.nJumps, txNs)
	q.probationUntil = max(q.probationUntil, txNs+int64(o.ProbationS*1e9))
	if countSince(q.jumps[:], q.nJumps, win) >= o.UnstableJumps {
		q.unstableUntil = txNs + int64(o.SuspendS*1e9)
		q.suspendUntil = max(q.suspendUntil, q.unstableUntil)
	}
	c0 := q.cand[0]
	tr.reset(c0.lat, c0.lon)
	for i := 0; i < q.nCand; i++ {
		tr.insert(node{obs: q.cand[i]}, o.QHorizontal, o.QVertical)
	}
	q.accepted, q.velSeen = q.nCand, false
	q.nCand = 0
	return Jumped
}

func (s *Store) obsVelocity(tr *track, m *modes.Message, txNs int64) Result {
	in, q, o := &tr.info, &tr.q, &s.opt
	// GNSS − baro difference: the aircraft's own value and the field
	if m.Valid&modes.FGNSSDelta != 0 && in.hasBaro && txNs-in.baroNs <= 10e9 {
		in.deltaM, in.deltaNs, in.deltaBaroFt, in.hasDelta = float64(m.GNSSDelta)*ftM, txNs, in.baroFt, true
		if lp := tr.lastPos(); lp != nil && txNs-in.lastPosNs <= 10e9 {
			s.accumulateField(tr, lp.lat, lp.lon, in.baroFt, in.deltaM, txNs)
		}
	}
	hasH := m.Valid&modes.FGroundSpeed != 0
	hasV := m.Valid&modes.FVertRate != 0
	if (!hasH && !hasV) || tr.n == 0 {
		return Noted
	}
	tag := tr.sec(txNs)
	pred, ok := tr.predictAt(tag, o.QHorizontal, o.QVertical)
	if !ok {
		return TooOld
	}
	plat, plon := tr.fromXY(pred[0].x[0], pred[1].x[0])
	nd := node{obs: obs{t: tag, rawT: tag, lat: plat, lon: plon, h: pred[2].x[0]}}
	if hasH {
		gs := m.GS * ktMps
		var ve, vn float64
		if m.Valid&modes.FTrack != 0 {
			st, ct := math.Sincos(m.Track * deg2rad)
			ve, vn = gs*st, gs*ct
		}
		quant := ktMps / math.Sqrt(12)
		if m.Subtype == 2 || m.Subtype == 4 {
			quant *= 4
		}
		sig := 1.0
		if m.Valid&modes.FNACv != 0 {
			if hf := modes.NACvHFOM(m.NACv); hf > 0 {
				sig = hf / 2.45
			}
		}
		sig = math.Min(math.Max(math.Hypot(sig, quant), o.VelSigmaMinMps), o.VelSigmaMaxMps)
		nd.kind |= nVelH
		nd.ve, nd.vn, nd.rv = ve, vn, sig*sig
		// gate
		sx, sy := tr.scales(plat, nd.h)
		ex := ve - pred[0].x[1]*sx
		ey := vn - pred[1].x[1]*sy
		vx := pred[0].p[3]*sx*sx + nd.rv
		vy := pred[1].p[3]*sy*sy + nd.rv
		if q.accepted >= 2 && ex*ex/vx+ey*ey/vy > o.GateChi2 && math.Hypot(ex, ey) > o.VelGateMps {
			q.velRejected++
			s.nVelRej.Add(1)
			pushTime(q.velBad[:], &q.nVelBad, txNs)
			if countSince(q.velBad[:], q.nVelBad, txNs-30e9) >= 3 {
				q.velBadUntil = txNs + int64(o.ProbationS*1e9)
			}
			return Rejected
		}
	}
	if hasV {
		vu := float64(m.VertRate) * fpmMps
		r := o.VertRateSigmaMps * o.VertRateSigmaMps
		if m.VertRateBaro {
			r += 0.05 * 0.05 * vu * vu
		}
		nd.kind |= nVelV
		nd.vu, nd.rvu = vu, r
	}
	if !tr.insert(nd, o.QHorizontal, o.QVertical) {
		return TooOld
	}
	q.velocities++
	if hasH {
		q.velSeen = true
	}
	return Accepted
}

// accumulateField averages an aircraft's GNSS−baro reports per field cell
// and hands one observation per cell and DecimateS to the field.
func (s *Store) accumulateField(tr *track, lat, lon, baroFt, deltaM float64, tNs int64) {
	a := &tr.info.facc
	f := s.field
	k := f.key(lat, lon, baroFt)
	if a.ok && (k != a.k || float64(tNs-a.t0) > f.opt.DecimateS*1e9) {
		s.flushField(a)
	}
	if !a.ok {
		*a = fieldAcc{k: k, t0: tNs, ok: true}
	}
	a.sum += deltaM
	a.baroSum += baroFt
	a.lat, a.lon = lat, lon
	a.n++
}

func (s *Store) flushField(a *fieldAcc) {
	if a.ok && a.n > 0 {
		n := float64(a.n)
		s.field.Update(a.lat, a.lon, a.baroSum/n, a.sum/n, 0, a.t0)
	}
	a.ok = false
}

// state computes the state of a track at tNs (shard lock held).
func (s *Store) state(tr *track, tNs int64, tSigNs float64, st *State) bool {
	o := &s.opt
	if tr.n == 0 || tr.lastPos() == nil {
		return false
	}
	ts := tr.sec(tNs)
	if ts > tr.last().t+o.MaxExtrapS || ts < tr.at(0).t-o.MaxExtrapS {
		return false
	}
	sm, mode := tr.smoothedAt(ts, o.QHorizontal, o.QVertical)
	in, q := &tr.info, &tr.q
	lat, lon := tr.fromXY(sm[0].x[0], sm[1].x[0])
	h := sm[2].x[0]
	sx, sy := tr.scales(lat, h)
	ve, vn, vu := sm[0].x[1]*sx, sm[1].x[1]*sy, sm[2].x[1]

	*st = State{ICAO: tr.icao, TNs: tNs, Lat: lat, Lon: lon, HAE: h}
	st.Pos = geo.ToECEF(lat, lon, h)
	slt, clt := math.Sincos(lat * deg2rad)
	sln, cln := math.Sincos(lon * deg2rad)
	// rows of R (ECEF -> ENU): E, N, U
	E := [3]float64{-sln, cln, 0}
	N := [3]float64{-slt * cln, -slt * sln, clt}
	U := [3]float64{clt * cln, clt * sln, slt}
	st.Vel = geo.Vec3{
		X: E[0]*ve + N[0]*vn + U[0]*vu,
		Y: E[1]*ve + N[1]*vn + U[1]*vu,
		Z: E[2]*ve + N[2]*vn + U[2]*vu,
	}

	// quality numbers
	nacp := in.nacp
	if !in.hasNACp {
		st.Flags |= FlagNUCp
		nacp = in.nucpNACp
		if o.NoNUCp {
			nacp = 0
		}
	}
	st.NACp, st.NIC, st.Version = nacp, in.nic, in.version
	epu := modes.NACpEPU(nacp)
	if !(epu > 0) {
		epu = 185.2
	}
	bH := epu / 2.448
	st.SigmaBiasH = bH

	// latency
	var sL float64
	if in.t1 && !q.t1Untrusted {
		st.Flags |= FlagT1
	} else {
		st.LatencyS = tr.lBuilt
		sL = math.Sqrt(tr.lVar)
		st.LatencySigmaS = sL
		if tr.lObs >= 3 {
			st.Flags |= FlagLatencyEstimated
		}
	}
	if q.t1Untrusted {
		st.Flags |= FlagT1Untrusted
	}

	// covariance in ENU (doc.go "Output covariance")
	pe := sm[0].p[0] * sx * sx
	pn := sm[1].p[0] * sy * sy
	pu := sm[2].p[0]
	st.SigmaFilterH = math.Sqrt((pe + pn) / 2)
	tsig := tSigNs / 1e9
	vt := tsig*tsig + sL*sL
	v := [3]float64{ve, vn, vu}
	c := [9]float64{
		pe + bH*bH, 0, 0,
		0, pn + bH*bH, 0,
		0, 0, pu + in.hBiasVar,
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			c[i*3+j] += vt * v[i] * v[j]
		}
	}
	st.CovENU = c
	st.SigmaV = math.Sqrt(c[8])
	// ECEF: Rᵀ C R
	R := [3][3]float64{E, N, U}
	var tmp [3][3]float64
	for i := 0; i < 3; i++ { // tmp = C R
		for j := 0; j < 3; j++ {
			tmp[i][j] = c[i*3+0]*R[0][j] + c[i*3+1]*R[1][j] + c[i*3+2]*R[2][j]
		}
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			st.Cov[i*3+j] = R[0][i]*tmp[0][j] + R[1][i]*tmp[1][j] + R[2][i]*tmp[2][j]
		}
	}
	st.AltSrc = in.altSrc

	// flags
	st.PosAgeS = tr.posAge(ts)
	switch mode {
	case modeExtrapolated:
		st.Flags |= FlagExtrapolated
	case modeBackward:
		st.Flags |= FlagBackward
	}
	if st.PosAgeS > o.MaxPosAgeS {
		st.Flags |= FlagStale
	}
	if q.accepted >= o.ConfirmPositions && q.velSeen {
		st.Flags |= FlagConfirmed
	}
	if nacp < o.MinNACp || !in.hasNIC || in.nic < o.MinNIC {
		st.Flags |= FlagLowQuality
	}
	if st.SigmaV > o.MaxVertSigmaM || in.altSrc == AltNone {
		st.Flags |= FlagNoAltitude
	}
	if in.ground {
		st.Flags |= FlagOnGround
	}
	if tNs < q.probationUntil {
		st.Flags |= FlagProbation
	}
	if tNs < q.suspendUntil {
		st.Flags |= FlagSuspended
	}
	if tNs < q.unstableUntil {
		st.Flags |= FlagUnstable
	}
	if tNs < q.velBadUntil {
		st.Flags |= FlagVelInconsistent
	}
	if tNs < q.horizonUntil {
		st.Flags |= FlagBeyondHorizon
	}
	return true
}

// At returns the state of aircraft key (modes.Message.AddrKey) at tNs
// (network time ≈ UTC, ns) whose own uncertainty is tSigmaNs: the
// smoothed position, velocity and covariance of its reference track,
// interpolated to that instant (doc.go "Queries"). ok is false when there
// is no track or the instant is more than MaxExtrapS outside its window.
// State.Usable tells whether it may serve as a sync reference.
func (s *Store) At(key uint32, tNs int64, tSigmaNs float64, st *State) bool {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil {
		return false
	}
	return s.state(tr, tNs, tSigmaNs, st)
}

// Has reports whether a reference track exists for key.
func (s *Store) Has(key uint32) bool {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	return tr != nil && tr.n > 0
}

// Suspend makes an aircraft unusable as a reference until untilNs (e.g.
// the clock filter's blacklist after biased residuals across ≥ 3 station
// pairs, DESIGN §9.2.4, or a failed TDOA verification). untilNs <= 0 lifts
// a suspension.
func (s *Store) Suspend(key uint32, untilNs int64) {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if tr := sh.m[key]; tr != nil {
		if untilNs <= 0 {
			tr.q.suspendUntil = 0
		} else {
			tr.q.suspendUntil = max(tr.q.suspendUntil, untilNs)
		}
	}
}

// CheckHeard verifies that the aircraft's position at tNs is above the
// radio horizon (effective earth HorizonK, margin HorizonMarginM) of every
// receiver that heard it. A violation (typical of a spoofer near the
// receivers announcing a distant position) puts the track on probation;
// three within UnstableWindowS suspend it. It returns false on a violation
// or when there is no state.
func (s *Store) CheckHeard(key uint32, tNs int64, receivers []geo.Vec3) bool {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil {
		return false
	}
	var st State
	if !s.state(tr, tNs, 0, &st) {
		return false
	}
	o, q := &s.opt, &tr.q
	for _, rx := range receivers {
		if geo.Clearance(rx, st.Pos, o.HorizonK) < -o.HorizonMarginM {
			pushTime(q.horizonBad[:], &q.nHorizonBad, tNs)
			q.horizonUntil = max(q.horizonUntil, tNs+int64(o.ProbationS*1e9))
			if countSince(q.horizonBad[:], q.nHorizonBad, tNs-int64(o.UnstableWindowS*1e9)) >= 3 {
				q.suspendUntil = max(q.suspendUntil, tNs+int64(o.SuspendS*1e9))
			}
			return false
		}
	}
	return true
}

// HAEFromBaro converts a pressure altitude (ft) of aircraft key at a
// position into a height above the ellipsoid (m) with its 1σ: from the
// aircraft's own recent GNSS−baro difference when it reports one, else from
// the field (the solver's altitude row, DESIGN §9.4).
func (s *Store) HAEFromBaro(key uint32, lat, lon, baroFt float64, tNs int64) (haeM, sigmaM float64, src AltSource) {
	sh := s.shard(key)
	sh.mu.Lock()
	tr := sh.m[key]
	if tr != nil {
		in := &tr.info
		if in.hasDelta && math.Abs(float64(tNs-in.deltaNs)) <= 30e9 && math.Abs(baroFt-in.deltaBaroFt) <= 1000 {
			d := math.Abs(baroFt-in.deltaBaroFt) * ftM * 0.03
			h := baroFt*ftM + in.deltaM
			sh.mu.Unlock()
			return h, math.Hypot(25*ftM/math.Sqrt(12), d), AltOwnDelta
		}
	}
	sh.mu.Unlock()
	return s.field.BaroToHAE(lat, lon, baroFt, tNs)
}

// Info returns a diagnostic summary of one track.
func (s *Store) Info(key uint32) (Info, bool) {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil {
		return Info{}, false
	}
	var st State
	var f Flags
	if s.state(tr, tr.info.lastSeen, 0, &st) {
		f = st.Flags
	}
	return Info{
		ICAO: key, Nodes: tr.n, Positions: tr.q.positions, Velocities: tr.q.velocities,
		Rejected: tr.q.rejected, VelRejected: tr.q.velRejected, Jumps: tr.q.nJumps,
		NACp: st.NACp, NIC: tr.info.nic, Version: tr.info.version,
		LatencyS: tr.lHat, LatencySigmaS: math.Sqrt(tr.lVar), LastSeenNs: tr.info.lastSeen,
		Flags: f, FlagNames: f.String(),
	}, true
}

// Expire drops tracks not updated since nowNs − TrackTimeoutS and prunes
// the field. It returns the number of tracks dropped.
func (s *Store) Expire(nowNs int64) int {
	lim := nowNs - int64(s.opt.TrackTimeoutS*1e9)
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, tr := range sh.m {
			if tr.info.lastSeen < lim {
				s.flushField(&tr.info.facc)
				delete(sh.m, k)
				n++
			}
		}
		sh.mu.Unlock()
	}
	s.field.Prune(nowNs)
	return n
}

// Stats are store counters.
type Stats struct {
	Tracks      int    `json:"tracks"`
	Observed    uint64 `json:"observed"`
	Accepted    uint64 `json:"accepted"`
	Rejected    uint64 `json:"rejected"`
	VelRejected uint64 `json:"velRejected"`
	Jumps       uint64 `json:"jumps"`
	Started     uint64 `json:"started"`
	// AltReplies: Mode S altitude replies taken for a track; AltReplyRejected:
	// replies whose height failed the track's gate; GNSSPairs: GNSS−baro
	// differences from a TC 20-22 height paired with a pressure altitude.
	AltReplies       uint64 `json:"altReplies"`
	AltReplyRejected uint64 `json:"altReplyRejected"`
	GNSSPairs        uint64 `json:"gnssPairs"`
	Field            FieldStats
}

// Stats returns the counters.
func (s *Store) Stats() Stats {
	st := Stats{
		Observed: s.nObs.Load(), Accepted: s.nAcc.Load(), Rejected: s.nRej.Load(),
		VelRejected: s.nVelRej.Load(), Jumps: s.nJump.Load(), Started: s.nStart.Load(),
		AltReplies: s.nReply.Load(), AltReplyRejected: s.nReplyRej.Load(), GNSSPairs: s.nPair.Load(),
		Field: s.field.Stats(),
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		st.Tracks += len(sh.m)
		sh.mu.Unlock()
	}
	return st
}

// surfDist is the great-circle distance in metres (spherical, Gaussian
// radius at the first point; plenty for gating).
func surfDist(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*deg2rad, lat2*deg2rad
	dp := p2 - p1
	dl := wrap180(lon2-lon1) * deg2rad
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * geo.GaussianRadius(lat1) * math.Asin(math.Min(1, math.Sqrt(a)))
}
