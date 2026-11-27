package ref

import (
	"math"

	"airview/internal/modes"
)

// observeAltReply takes the barometric altitude of a Mode S reply (DF0,
// DF4, DF16, DF20: AC13 field) of an aircraft that already has a reference
// track (doc.go "Mode S altitude replies"):
//
//   - it is the aircraft's latest pressure altitude, so a later TC 19
//     GNSS−baro difference and a TC 20-22 GNSS height pair with it
//     (pairGNSS);
//   - at most every AltReplyMinS it is converted to a height above the
//     ellipsoid (own GNSS−baro difference, else the field) and added to the
//     track's vertical channel as a height-only node tagged with the
//     transmit instant itself (a reply carries the current altitude, there
//     is no position latency). Between position squitters, and for
//     transmitters whose squitters carry no altitude, this keeps the
//     reference height fresh.
//
// The address of these replies is overlaid on the parity (CRC
// "address-parity"), so a corrupted frame can carry any address: replies
// are used only for an existing track, and a height more than 5σ and
// 100 m off the track's prediction is rejected.
func (s *Store) observeAltReply(m *modes.Message, txNs int64) Result {
	if s.opt.NoAltReplies || m.Valid&modes.FAltBaro == 0 || m.Valid&modes.FAltMetric != 0 {
		return Ignored
	}
	if m.CRC != modes.CRCAddressParity && m.CRC != modes.CRCOK {
		return Ignored
	}
	if m.AirGround == modes.AGGround {
		return Ignored
	}
	key := m.AddrKey()
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil || tr.n == 0 || tr.lastPos() == nil || tr.info.ground {
		return Ignored
	}
	in, o := &tr.info, &s.opt
	b := float64(m.AltBaro)
	tag := tr.sec(txNs)
	pred, ok := tr.predictAt(tag, o.QHorizontal, o.QVertical)
	if !ok {
		return TooOld
	}
	lat, lon := tr.fromXY(pred[0].x[0], pred[1].x[0])
	h, rw, rb, src := s.heightBaro(in, b, m.AltBaroGillham, lat, lon, txNs)
	// Gate. Against a recent pressure altitude of the aircraft itself the
	// conversion bias cancels: the change must match the vertical rate.
	// Otherwise against the track height, conversion bias included.
	var nz, sz float64
	if dtS := float64(txNs-in.baroNs) / 1e9; in.hasBaro && math.Abs(dtS) <= 10 {
		nz = (b-in.baroFt)*ftM - pred[2].x[1]*dtS
		sz = 2*rw + pred[2].p[3]*dtS*dtS + 4
	} else {
		nz = h - pred[2].x[0]
		sz = pred[2].p[0] + rw + rb
	}
	if nz*nz/sz > 25 && math.Abs(nz) > 100 {
		s.nReplyRej.Add(1)
		return Rejected
	}
	s.nReply.Add(1)
	if txNs >= in.baroNs {
		in.baroFt, in.baroNs, in.hasBaro = b, txNs, true
	}
	s.pairGNSS(tr, pred[2].x[1], txNs)
	if in.lastVNs != 0 && float64(txNs-in.lastVNs) < o.AltReplyMinS*1e9 {
		return Noted
	}
	nd := node{obs: obs{t: tag, rawT: tag, kind: nPosV | nExact, lat: lat, lon: lon, h: h, rh: rw}}
	if !tr.insert(nd, o.QHorizontal, o.QVertical) {
		return TooOld
	}
	in.lastVNs = txNs
	if in.altSrc == AltNone || in.altSrc == AltFallback {
		in.hBiasVar, in.altSrc = rb, src
	}
	return Accepted
}

// pairGNSS pairs the aircraft's latest GNSS height (TC 20-22) and latest
// pressure altitude (TC 9-18 or a Mode S altitude reply) when they were
// reported within PairBaroS of each other: their difference, moved to a
// common instant with the track's vertical rate vz (m/s), is the
// aircraft's own GNSS−baro difference (used like a TC 19 value for its
// barometric heights) and one observation of the GNSS−baro field (doc.go
// "GNSS−baro field"). Caller holds the shard lock.
func (s *Store) pairGNSS(tr *track, vz float64, nowNs int64) {
	in := &tr.info
	if !in.hasGNSS || !in.hasBaro {
		return
	}
	dt := float64(in.baroNs-in.gnssNs) / 1e9 // baro instant minus GNSS instant
	if math.Abs(dt) > s.opt.PairBaroS {
		return
	}
	if math.IsNaN(vz) || math.Abs(vz) > 150 {
		vz = 0
	}
	// GNSS height at the baro instant minus the pressure altitude
	d := in.gnssM + vz*dt - in.baroFt*ftM
	at := max(in.gnssNs, in.baroNs)
	if in.hasDelta && in.deltaNs >= at {
		return // already paired (or a newer TC 19 value)
	}
	s.nPair.Add(1)
	in.deltaM, in.deltaNs, in.deltaBaroFt, in.hasDelta = d, at, in.baroFt, true
	if lp := tr.lastPos(); lp != nil && nowNs-in.lastPosNs <= 10e9 {
		s.accumulateField(tr, lp.lat, lp.lon, in.baroFt, d, at)
	}
}
