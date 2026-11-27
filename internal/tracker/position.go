package tracker

import (
	"math"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/store"
)

const ktToKmPerS = 1.852 / 3600

// handleCPR stores the compact position report of m and tries to turn it
// into a position: global decode with a fresh even/odd pair, else local
// decode against the last position. It reports whether a position was
// accepted.
func (t *Tracker) handleCPR(ac *aircraft, m *modes.Message, tMs int64, mlat bool, rssi float64, slot int) bool {
	idx := 0
	if m.CPROdd {
		idx = 1
	}
	cur := &ac.cpr[idx]
	*cur = cprFrame{lat: m.CPRLat, lon: m.CPRLon, t: tMs, valid: true, surface: m.CPRSurface, mlat: mlat}
	other := &ac.cpr[1-idx]
	surface := m.CPRSurface

	pairMs := int64(cprPairMs)
	if surface {
		pairMs = cprSurfPairMs
	}
	var lat, lon float64
	got, independent := false, false
	if other.valid && other.surface == surface && other.mlat == mlat && tMs-other.t <= pairMs && tMs >= other.t {
		even, odd := &ac.cpr[0], &ac.cpr[1]
		if surface {
			// The surface encoding is ambiguous over 90 degrees: the last
			// position or the receiver picks the quadrant.
			if rlat, rlon, ok := t.surfaceRef(ac, tMs); ok {
				lat, lon, got = modes.CPRSurfaceGlobal(rlat, rlon, even.lat, even.lon, odd.lat, odd.lon, m.CPROdd)
			}
		} else {
			lat, lon, got = modes.CPRAirborneGlobal(even.lat, even.lon, odd.lat, odd.lon, m.CPROdd)
		}
		independent = got
	}
	if !got && ac.posValid && tMs-ac.posMs <= localRefMs {
		if surface {
			lat, lon, got = modes.CPRSurfaceLocal(ac.lat, ac.lon, m.CPRLat, m.CPRLon, m.CPROdd)
		} else {
			lat, lon, got = modes.CPRAirborneLocal(ac.lat, ac.lon, m.CPRLat, m.CPRLon, m.CPROdd)
		}
	}
	if !got {
		return false
	}
	// Report the position decoded from this very message (for the latency
	// analysis; it is used only when offerPosition accepts it).
	t.cprLat, t.cprLon, t.cprOK = lat, lon, true

	src, label := store.SrcADSB, model.SrcADSB
	switch {
	case mlat:
		src, label = store.SrcMLAT, model.SrcMLAT
	case m.Source == modes.SourceTISB:
		src, label = store.SrcTISB, model.SrcTISB
	case m.Source == modes.SourceADSR:
		src, label = store.SrcTISB, model.SrcADSR
	}
	return t.offerPosition(ac, lat, lon, tMs, src, label, rssi, slot, independent, surface)
}

// surfaceRef returns the reference position for a surface CPR decode.
func (t *Tracker) surfaceRef(ac *aircraft, tMs int64) (lat, lon float64, ok bool) {
	if ac.posValid && tMs-ac.posMs <= 10*60_000 {
		return ac.lat, ac.lon, true
	}
	if t.station != nil {
		return t.station.Lat, t.station.Lon, true
	}
	if t.net != nil {
		return t.netSurfaceRef(ac, tMs)
	}
	return 0, 0, false
}

// plausible reports whether an aircraft could have moved from fix 1 to fix 2.
func (ac *aircraft) plausible(lat1, lon1 float64, t1 int64, lat2, lon2 float64, t2 int64, src uint8, surface bool) bool {
	dt := float64(t2-t1) / 1000
	if dt < 0 {
		dt = -dt
	}
	speed, slack := 900.0, 1.5 // kt, km
	if surface {
		speed, slack = 150, 0.4
	}
	if fresh(ac.velMs, t2, 30_000) {
		if surface {
			speed = ac.gs*1.5 + 40
		} else {
			speed = ac.gs*1.3 + 80
		}
	}
	if src == store.SrcMLAT {
		slack = 5
	}
	return geo.FastDistanceKm(lat1, lon1, lat2, lon2) <= speed*ktToKmPerS*dt+slack
}

// offerPosition runs the sanity filters on a decoded position and accepts
// it, parks it as an unconfirmed fix, or rejects it. independent is true for
// a fix that did not use the previous position as its reference (a global
// CPR decode, or a position from a decoded feed).
func (t *Tracker) offerPosition(ac *aircraft, lat, lon float64, tMs int64, src uint8, label string, rssi float64, slot int, independent, surface bool) bool {
	if !geo.ValidLatLon(lat, lon) || (lat == 0 && lon == 0) {
		t.c.PosRejected++
		return false
	}
	if src != store.SrcADSB && ac.posValid && ac.posSrc == store.SrcADSB && tMs-ac.posMs <= 30_000 {
		// The aircraft's own ADS-B position beats MLAT, TIS-B and feed data.
		return false
	}
	km, brg := math.NaN(), math.NaN()
	if t.station != nil {
		km, brg = t.station.DistanceBearing(lat, lon)
		if !(km <= t.maxRangeKm) {
			t.c.PosRejected++
			return false
		}
	} else if t.net != nil && !t.netInRange(ac, lat, lon, tMs) {
		t.c.PosRejected++
		return false
	}
	decodedFeed := src == store.SrcOther || (ac.fromUpdate && ac.nConfirmed == 0)
	if decodedFeed {
		// An SBS / JSON source has already filtered its positions; only the
		// range limit and a gross speed check apply.
		if ac.posValid && tMs-ac.posMs <= speedRefMs && !ac.plausible(ac.lat, ac.lon, ac.posMs, lat, lon, tMs, src, surface) {
			if !(ac.pend.valid && tMs-ac.pend.t <= pendingMs && ac.plausible(ac.pend.lat, ac.pend.lon, ac.pend.t, lat, lon, tMs, src, surface)) {
				ac.pend = pendingFix{lat: lat, lon: lon, t: tMs, valid: true}
				t.c.PosRejected++
				return false
			}
		}
		t.acceptPosition(ac, lat, lon, tMs, src, label, rssi, slot, km, brg, surface)
		return true
	}

	if ac.posValid && tMs-ac.posMs <= speedRefMs &&
		ac.plausible(ac.lat, ac.lon, ac.posMs, lat, lon, tMs, src, surface) {
		t.acceptPosition(ac, lat, lon, tMs, src, label, rssi, slot, km, brg, surface)
		return true
	}
	// No usable previous position, or the new fix is an outlier against it:
	// only an independent fix may start (or restart) the track, and only
	// once a second fix agrees with it.
	if !independent {
		t.c.PosRejected++
		return false
	}
	if ac.pend.valid && tMs-ac.pend.t <= pendingMs && tMs > ac.pend.t &&
		ac.plausible(ac.pend.lat, ac.pend.lon, ac.pend.t, lat, lon, tMs, src, surface) {
		if ac.posValid && tMs-ac.posMs <= speedRefMs {
			t.c.PosJumps++
		}
		t.acceptPosition(ac, lat, lon, tMs, src, label, rssi, slot, km, brg, surface)
		return true
	}
	ac.pend = pendingFix{lat: lat, lon: lon, t: tMs, valid: true}
	t.c.PosPending++
	return false
}

// acceptPosition makes (lat, lon) the aircraft's position and updates
// everything that hangs off a position: geometry, trail, flight aggregates,
// persisted trail points, statistics and records.
func (t *Tracker) acceptPosition(ac *aircraft, lat, lon float64, tMs int64, src uint8, label string, rssi float64, slot int, km, brg float64, surface bool) {
	ac.lat, ac.lon, ac.posMs, ac.posValid = lat, lon, tMs, true
	ac.posSrc, ac.posLabel = src, label
	ac.pend.valid = false
	ac.nPos++
	ac.srcMask |= 1 << src
	t.c.Positions++
	if src == store.SrcMLAT {
		t.c.MLATPositions++
	}
	sec := tMs / 1000

	alt := ac.bestAlt(tMs)
	if st := t.station; st != nil {
		ac.dist, ac.brg, ac.geoValid = km, brg, true
		altM := alt * geo.MetresPerFoot
		if math.IsNaN(alt) && surface {
			altM = t.groundElevM // on an airfield near the receiver
		}
		if math.IsNaN(altM) {
			ac.slant, ac.elev = math.NaN(), math.NaN()
		} else {
			ac.slant = geo.SlantRangeKm(km, st.AntennaAMSLM, altM)
			ac.elev = geo.ElevationDeg(km, st.AntennaAMSLM, altM, st.K)
		}
		if !(ac.maxDistKm >= km) {
			ac.maxDistKm = km
		}
	} else {
		ac.geoValid = false
	}
	ac.cell = geo.HeatCell(lat, lon)

	// Live trail.
	gs := math.NaN()
	if fresh(ac.velMs, tMs, fieldTTLMs) {
		gs = ac.gs
	}
	add := true
	if lp := ac.trail.last(); lp != nil {
		gap := tMs - lp.T
		switch {
		case gap >= trailMinGapMs:
		case gap >= trailTurnGapMs && fresh(ac.velMs, tMs, 5000) && math.Abs(geo.AngleDiff(ac.track, ac.trailTrack)) >= 4:
		case gap >= trailTurnGapMs && math.Abs(alt-ac.trailAlt) >= 250:
		default:
			add = false
		}
	}
	if add {
		ac.trail.dropBefore(tMs - t.trailKeepMs)
		ac.trail.push(model.TrailPoint{T: tMs, Lat: lat, Lon: lon, Alt: alt, GS: gs, RSSI: rssi, Src: src})
		ac.trailTrack, ac.trailAlt = math.NaN(), alt
		if fresh(ac.velMs, tMs, 5000) {
			ac.trailTrack = ac.track
		}
	}

	// Flight aggregates and the persisted, down-sampled trail.
	if ac.flightOpen {
		fl := &ac.flight
		fl.Positions++
		fl.SrcMask |= 1 << src
		if math.IsNaN(fl.FirstLat) {
			fl.FirstLat, fl.FirstLon = lat, lon
		}
		fl.LastLat, fl.LastLon = lat, lon
		if t.station != nil {
			if !(fl.MaxDistKm >= km) {
				fl.MaxDistKm, fl.MaxDistBrg = km, brg
			}
			if !(fl.MinDistKm <= km) {
				fl.MinDistKm = km
			}
		}
		ac.flightDirty = true
		if t.st != nil && sec-ac.lastStoredSec >= t.trailEveryS && len(t.pendPos) < maxPendingPos {
			ac.lastStoredSec = sec
			trk := math.NaN()
			if fresh(ac.velMs, tMs, fieldTTLMs) {
				trk = ac.track
			}
			t.pendPos = append(t.pendPos, store.Position{
				FlightID: fl.ID, T: sec, Lat: lat, Lon: lon, Alt: alt, GS: gs, Track: trk, RSSI: rssi, Src: src,
			})
		}
	}

	t.creditPosition(0, ac, rssi, sec, src)
	if slot > 0 {
		t.creditPosition(slot, ac, rssi, sec, src)
	}

	// Records: only positions received off-air from the aircraft itself.
	if src == store.SrcADSB && t.station != nil {
		if km > t.recValue(recMaxDist) {
			t.record(recMaxDist, km, ac.key, sec, recordMax, ac)
		}
		if finite(rssi) && rssi < t.recValueOr(recMinRSSIPos, math.Inf(1)) {
			t.record(recMinRSSIPos, rssi, ac.key, sec, recordMin, ac)
		}
	}
}

// creditPosition adds an accepted position of ac to one statistics slot.
func (t *Tracker) creditPosition(slot int, ac *aircraft, rssi float64, sec int64, src uint8) {
	if slot < 0 || slot >= len(t.slots) {
		return
	}
	t.noteIncDay(sec)
	s := t.slots[slot]
	bit := uint32(1) << uint(slot)
	ac.minPosMask |= bit
	s.positions++
	s.totPositions++
	t.lastIncSec = sec
	if src == store.SrcMLAT {
		s.mlatPositions++
	}
	if src != store.SrcADSB && src != store.SrcOther {
		// MLAT results and TIS-B / ADS-R rebroadcasts say nothing about
		// what this antenna can receive from the aircraft.
		return
	}
	if ac.hourMask&bit == 0 {
		ac.hourMask |= bit
		s.hourAircraft++
	}
	alt := ac.bestAlt(ac.posMs)
	s.addHeat(ac.cell, rssi, alt, sec)
	if !ac.geoValid {
		return
	}
	s.addRange(ac.dist, ac.brg, ac.elev, ac.slant, alt, rssi)
}
