package tracker

import (
	"math"
	"time"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/rfstat"
	"airview/internal/store"
	"airview/internal/trajectory"
)

// Extensions wired by the engine: the trajectory filter (coasting aircraft),
// the RF statistics and the 1 Hz snapshot enrichment. All hooks are cheap
// and allocation-free on the frame path.

// predictAfterMs is model.PredictAfterSec in milliseconds.
const predictAfterMs = model.PredictAfterSec * 1000

// altObsGapMs throttles altitude-only observations (Mode S roll-call replies
// carry the altitude many times per second) fed to the tracking filter.
const altObsGapMs = 1000

// SetLearnedTurn installs (nil removes) the learned-flow blend of coasting
// positions: fn gets the last real fix, its track and ground speed and the
// coasting time and returns the learned position and its weight (0..1;
// flowlearn.Learner.Blend). The trajectory filter's prediction is moved
// towards it by that weight.
func (t *Tracker) SetLearnedTurn(fn func(lat, lon, altFt, trk, gs, dtS float64, nowMs int64) (lat2, lon2, w float64, ok bool)) {
	t.mu.Lock()
	t.learnTurn = fn
	t.snap = nil
	t.mu.Unlock()
}

// SetTrajectory installs the tracking filter (nil removes it). coastSec is
// how long after the last real position the live picture shows the
// predicted position (config.prediction.coastSec); the manager's MaxCoast
// must be at least that long.
func (t *Tracker) SetTrajectory(m *trajectory.Manager, coastSec int) {
	t.mu.Lock()
	t.tj = m
	t.coastMs = int64(coastSec) * 1000
	if m == nil || t.coastMs < predictAfterMs {
		t.coastMs = 0
	}
	t.snap = nil
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		for _, p := range s.parts {
			p.SetTrajectory(m, coastSec)
		}
		s.invalidate()
	}
}

// SetRFStats installs the RF statistics engine fed with every frame (nil removes it).
func (t *Tracker) SetRFStats(rs *rfstat.Stats) {
	t.mu.Lock()
	t.rf = rs
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		for _, p := range s.parts {
			p.SetRFStats(rs)
		}
	}
}

// SetNewAirframeHook installs fn, called (without tracker locks, from the
// tracker's Run goroutine or Flush) with the ICAO address of an aircraft
// that has never been stored before: once per airframe and session, within
// about a second of its first frame, also when a flush already wrote its
// row (see checkNewAirframes). Needs a store. nil removes it.
func (t *Tracker) SetNewAirframeHook(fn func(icao uint32)) {
	t.mu.Lock()
	t.onNewAF = fn
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		s.setHooks(t)
	}
}

// SetEnricher installs fn, called once for every newly built live snapshot
// (about 1 Hz) before it is published. fn may modify the snapshot's
// aircraft in place (the slice is private to the new snapshot) but must not
// call back into the tracker's Snapshot.
func (t *Tracker) SetEnricher(fn func(s *model.LiveSnapshot)) {
	t.mu.Lock()
	t.enrich = fn
	t.snap = nil
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		s.invalidate() // the front enriches the merged snapshot; shards never do
	}
}

// obsTime is the time of applicability of a frame for the tracking filter,
// on the host clock (the time base of every other observation and of the
// prediction times): the receiver's GPS timestamp (nanoseconds since UTC
// midnight) placed on the day of the host receive time and moved to the host
// clock with the NTP offset off (true UTC - host clock), else the receive
// time.
func obsTime(f *model.RawFrame, tMs int64, off time.Duration) time.Time {
	recv := f.Recv
	if recv.IsZero() {
		return time.UnixMilli(tMs)
	}
	if f.GPSNs >= 0 && f.GPSNs < 86_400_000_000_000 {
		ns := recv.UnixNano()
		day := ns - ns%86_400_000_000_000
		g := day + f.GPSNs - int64(off)
		switch d := g - ns; {
		case d > 43_200_000_000_000:
			g -= 86_400_000_000_000
		case d < -43_200_000_000_000:
			g += 86_400_000_000_000
		}
		if d := g - ns; d < 2_000_000_000 && d > -2_000_000_000 {
			return time.Unix(0, g)
		}
	}
	return recv
}

// feedTrajectory hands what the message just applied to ac to the tracking
// filter. Caller holds mu; t.tj != nil.
func (t *Tracker) feedTrajectory(ac *aircraft, m *modes.Message, tMs int64, at time.Time, gotPos bool) {
	var o trajectory.Obs
	v := m.Valid
	if gotPos {
		o.SetPos(ac.lat, ac.lon, trajectory.SourceFromString(ac.posLabel))
		if fresh(ac.nacpMs, tMs, fieldTTLMs) && ac.posSrc == store.SrcADSB {
			o.NACp = int8(ac.nacp)
			o.Has |= trajectory.HasNACp
		}
		o.OnGround = m.CPRSurface
		o.Has |= trajectory.HasGround
		if fresh(ac.windMs, tMs, fieldTTLMs) {
			o.WindDirDeg, o.WindKt = ac.windDir, ac.windSpd
			o.Has |= trajectory.HasWind
		}
	}
	if ac.velMs == tMs && finite(ac.gs) && finite(ac.track) {
		o.SetVel(ac.gs, ac.track)
		if fresh(ac.nacvMs, tMs, fieldTTLMs) {
			o.NACv = int8(ac.nacv)
			o.Has |= trajectory.HasNACv
		}
	}
	if ac.vrMs == tMs {
		o.SetVRate(ac.vertRate)
	}
	if ac.rollMs == tMs {
		o.RollDeg = ac.roll
		o.Has |= trajectory.HasRoll
	}
	if ac.trackRateMs == tMs {
		o.TrackRateDps = ac.trackRate
		o.Has |= trajectory.HasTrackRate
	}
	if ac.tasMs == tMs {
		o.TASKt = ac.tas
		o.Has |= trajectory.HasTAS
	}
	if ac.magHdgMs == tMs {
		o.MagHeadingDeg = ac.magHdg
		o.Has |= trajectory.HasMagHeading
	}
	if ac.selAltMs == tMs {
		o.SelAltFt = ac.selAlt
		o.Has |= trajectory.HasSelAlt
	}
	if ac.selHdgMs == tMs {
		o.SelHeadingDeg = ac.selHdg
		o.Has |= trajectory.HasSelHeading
	}
	if ac.navMs == tMs {
		o.NavModes = trajectory.NavMode(ac.navModes) // same bit layout as modes.NavModes
		o.Has |= trajectory.HasNavModes
	}
	if v&modes.FAltBaro != 0 {
		if o.Has != 0 || tMs-ac.tjAltMs >= altObsGapMs || tMs < ac.tjAltMs {
			o.SetAlt(float64(m.AltBaro))
			o.AltQ100 = m.AltBaroGillham
		}
	}
	if o.Has == 0 {
		return
	}
	if o.Has&trajectory.HasAlt != 0 {
		ac.tjAltMs = tMs
	}
	o.T = at
	t.tj.Observe(ac.key, &o)
}

// rfKind maps a decoded message to its rfstat frame kind.
func rfKind(m *modes.Message) rfstat.Kind {
	switch m.DF {
	case 17, 18:
		if m.Valid&modes.FTypeCode == 0 {
			return rfstat.KindOther
		}
		switch tc := m.TC; {
		case tc >= 9 && tc <= 18, tc >= 20 && tc <= 22:
			if m.CPROdd {
				return rfstat.KindPosOdd
			}
			return rfstat.KindPosEven
		case tc >= 5 && tc <= 8:
			if m.CPROdd {
				return rfstat.KindSurfOdd
			}
			return rfstat.KindSurfEven
		case tc == 19:
			return rfstat.KindVelocity
		case tc >= 1 && tc <= 4:
			return rfstat.KindIdent
		case tc == 28:
			return rfstat.KindAircraftStatus
		case tc == 29:
			return rfstat.KindTargetState
		case tc == 31:
			return rfstat.KindOpStatus
		}
		return rfstat.KindOther
	case 4:
		return rfstat.KindSurvAlt
	case 5:
		return rfstat.KindSurvIdent
	case 20:
		return rfstat.KindCommBAlt
	case 21:
		return rfstat.KindCommBIdent
	case 11:
		return rfstat.KindAllCall
	case 0:
		return rfstat.KindACASShort
	case 16:
		return rfstat.KindACASLong
	}
	return rfstat.KindOther
}

// rfCRC maps the decoder's CRC status for an accepted frame.
func rfCRC(c modes.CRCStatus) rfstat.CRCStatus {
	switch c {
	case modes.CRCOK, modes.CRCInterrogator:
		return rfstat.CRCOK
	case modes.CRCFixed1, modes.CRCFixed2:
		return rfstat.CRCFixed
	case modes.CRCAddressParity:
		return rfstat.CRCAddrAccepted
	case modes.CRCBad:
		return rfstat.CRCBad
	}
	return rfstat.CRCUnknown
}

// rfBase returns the frame fields every rfstat frame carries.
func rfBase(f *model.RawFrame, fs *feedState, slot int, rssi float64) rfstat.Frame {
	r := rfstat.NewFrame(f.Recv)
	r.GPSNs = f.GPSNs
	r.Signal = rssi
	if fs != nil && fs.idx < 16 {
		r.Feed = int8(fs.idx)
	}
	if slot > 0 && slot <= rfstat.MaxAntennas {
		r.Antenna = int8(slot - 1)
	}
	return r
}

// rfReject reports a frame that was not applied to any aircraft. Caller holds mu.
func (t *Tracker) rfReject(f *model.RawFrame, fs *feedState, slot int, rssi float64, df uint8, crc rfstat.CRCStatus, kind rfstat.Kind) {
	r := rfBase(f, fs, slot, rssi)
	r.DF, r.CRC, r.Kind = df, crc, kind
	t.rf.Observe(r)
}

// rfAccepted reports an applied frame with the aircraft's geometry and
// equipage. Caller holds mu.
func (t *Tracker) rfAccepted(f *model.RawFrame, fs *feedState, slot int, rssi float64, m *modes.Message, ac *aircraft, tMs int64, gotPos, implausible bool, bds uint8) {
	r := rfBase(f, fs, slot, rssi)
	df := m.DF
	if df > 24 {
		df = 24
	}
	r.DF, r.CRC, r.ICAO, r.Kind = df, rfCRC(m.CRC), ac.key, rfKind(m)
	if df == 17 || df == 18 {
		r.TC = m.TC
		r.GNSSAlt = m.TC >= 20 && m.TC <= 22
		if m.Valid&modes.FGNSSDelta != 0 {
			r.GNSSDeltaFt = float64(m.GNSSDelta)
		}
	}
	if df == 20 || df == 21 {
		r.BDS = bds
	}
	if m.Valid&modes.FIID != 0 {
		r.IID = m.IID
	}
	if ra, ok := m.ACASRA(); ok {
		r.RA = ra.Active()
	}
	if ac.posValid && tMs-ac.posMs <= posTTLMs && ac.geoValid {
		r.DistKm, r.BearingDeg, r.ElevDeg = ac.dist, ac.brg, ac.elev
	}
	if fresh(ac.altBaroMs, tMs, fieldTTLMs) {
		r.AltFt = ac.altBaro
	}
	if fresh(ac.velMs, tMs, fieldTTLMs) {
		r.TrackDeg = ac.track
	}
	if gotPos {
		r.Lat, r.Lon = ac.lat, ac.lon
	}
	r.Implausible = implausible
	r.Version = ac.version
	if fresh(ac.nicMs, tMs, fieldTTLMs) {
		r.NIC = ac.nic
	}
	if fresh(ac.nacpMs, tMs, fieldTTLMs) {
		r.NACp = int8(ac.nacp)
	}
	if fresh(ac.silMs, tMs, fieldTTLMs) {
		r.SIL = int8(ac.sil)
	}
	if ac.categoryOK {
		r.Category = ac.category
	}
	r.OnGround = ac.onGround && fresh(ac.groundMs, tMs, fieldTTLMs)
	t.rf.Observe(r)
}

// predictedPos fills the predicted position of a coasting aircraft into a.
// It reports false when no prediction applies (the caller then shows the
// last real position the usual way). Caller holds mu.
func (t *Tracker) predictedPos(ac *aircraft, now int64, a *model.Aircraft, al *dtoAlloc) bool {
	if t.tj == nil || t.coastMs == 0 || !ac.posValid {
		return false
	}
	age := now - ac.posMs
	if age <= predictAfterMs || age > t.coastMs {
		return false
	}
	pr, ok := t.tj.Predict(ac.key, time.UnixMilli(now))
	if !ok || !geo.ValidLatLon(pr.Lat, pr.Lon) {
		return false
	}
	if fn := t.learnTurn; fn != nil && fresh(ac.velMs, ac.posMs, fieldTTLMs) {
		// the turns learned from the surrounding traffic, from the last real fix
		if lat2, lon2, w, ok := fn(ac.lat, ac.lon, ac.bestAlt(ac.posMs), ac.track, ac.gs, float64(age)/1000, now); ok && w > 0 && geo.ValidLatLon(lat2, lon2) {
			if lon2-pr.Lon > 180 {
				lon2 -= 360
			} else if lon2-pr.Lon < -180 {
				lon2 += 360
			}
			pr.Lat, pr.Lon = pr.Lat*(1-w)+lat2*w, geo.NormalizeLon(pr.Lon*(1-w)+lon2*w)
			a.PredLearned = al.fp(math.Round(w*100) / 100)
		}
	}
	a.Predicted = true
	a.Lat, a.Lon = al.fp(pr.Lat), al.fp(pr.Lon)
	a.PosAge = al.fp(round1(float64(age) / 1000))
	a.CoastS = a.PosAge
	a.PredSigmaM = al.fp(math.Round(pr.SigmaHM))
	a.LastRealLat, a.LastRealLon = al.fp(ac.lat), al.fp(ac.lon)
	if st := t.station; st != nil {
		km, brg := st.DistanceBearing(pr.Lat, pr.Lon)
		a.DistKm, a.Bearing = al.fp(km), al.fp(brg)
		alt := math.NaN()
		if pr.AltOK {
			alt = pr.AltFt
		} else {
			alt = ac.bestAlt(now)
		}
		if finite(alt) {
			altM := alt * geo.MetresPerFoot
			a.SlantKm = al.fp(geo.SlantRangeKm(km, st.AntennaAMSLM, altM))
			a.Elevation = al.fp(geo.ElevationDeg(km, st.AntennaAMSLM, altM, st.K))
		}
	}
	return true
}

// coasting reports whether ac is shown at a predicted position at time now
// (so it stays in the live picture even when otherwise silent).
func (t *Tracker) coasting(ac *aircraft, now int64) bool {
	return t.tj != nil && t.coastMs > 0 && ac.posValid && now-ac.posMs <= t.coastMs
}
