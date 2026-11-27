package tracker

import (
	"airview/internal/acas"
	"encoding/binary"
	"math"
	"math/bits"
	"strconv"
	"time"

	lchain "airview/internal/latency"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/ntp"
	"airview/internal/rfstat"
	"airview/internal/store"
	"airview/internal/trajectory"
)

// Frame outcomes remembered in the de-duplication cache.
const (
	resOK     uint8 = iota + 1 // accepted and applied
	resBad                     // parity failure
	resReject                  // parity-address frame of an unknown / unconfirmed address
	resOther                   // unsupported downlink format
)

const (
	ddBits = 12
	ddSets = 1 << ddBits
)

// ddEntry is one recently seen frame.
type ddEntry struct {
	a, b  uint64 // frame bytes
	t     int64
	src   uint64 // (feed, channel) sources that delivered it
	key   uint32 // aircraft it was applied to
	res   uint8
	dfIdx uint8
	pos   bool  // produced an accepted position
	psrc  uint8 // that position's store.Src* code
}

// dedupe looks the frame up in the cache. dup is true when the same data
// arrived within the window from a different source; a repeat from the same
// source is a new transmission (several radars interrogate the same aircraft
// and get identical replies).
func (t *Tracker) dedupe(a, b uint64, tMs int64, src uint64) (e *ddEntry, dup bool) {
	h := (a*0x9E3779B97F4A7C15 ^ bits.RotateLeft64(b*0xC2B2AE3D27D4EB4F, 31)) >> (64 - ddBits)
	set := &t.dd[h]
	for i := range set {
		e = &set[i]
		if e.a != a || e.b != b || e.t == 0 {
			continue
		}
		d := tMs - e.t
		if d < 0 {
			d = -d
		}
		if d <= dedupeWindowMs && e.src&src == 0 {
			e.src |= src
			return e, true
		}
		e.t, e.src, e.key, e.res, e.pos = tMs, src, 0, 0, false
		return e, false
	}
	e = &set[0]
	if set[1].t < e.t {
		e = &set[1]
	}
	*e = ddEntry{a: a, b: b, t: tMs, src: src}
	return e, false
}

// feedState is the runtime state of one feed ID.
type feedState struct {
	id         string
	idx        int // index in Tracker.feedList and aircraft.feedSeen
	slot       int // antenna slot from Feed.AntennaID (0 = none)
	mlatOnly   bool
	frames     uint64
	updates    uint64
	updateMsgs uint64
	lastMs     int64
	rate       float64
	ew         ewRate
	prevN      uint64
	lat        latency
	statusMs   int64 // when the last receiver status frame was stored
	// latOwn: this tracker samples the feed's GPS latency (always, unless it
	// is a shard that does not own the feed: each feed is sampled by one
	// shard).
	latOwn bool
}

// statusEveryMs is how often a feed's receiver status frame (Radarcape type
// '4', about one per second) is written to receiver_status.
const statusEveryMs = 60_000

// storeStatus samples receiver status frames into the receiver_status table
// (at most one row per feed and minute). The payload is stored as a JSON
// array of byte values, the form GET /api/timing/receiver documents.
// Caller holds mu.
func (t *Tracker) storeStatus(f *model.RawFrame, tMs int64) {
	if t.st == nil || len(f.Data) == 0 {
		return
	}
	fs := t.feedLocked(f.FeedID)
	if fs.statusMs != 0 && tMs-fs.statusMs < statusEveryMs && tMs >= fs.statusMs {
		return
	}
	fs.statusMs = tMs
	b := make([]byte, 0, 2+4*len(f.Data))
	b = append(b, '[')
	for i, v := range f.Data {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendUint(b, uint64(v), 10)
	}
	b = append(b, ']')
	t.st.AddReceiverStatus([]store.ReceiverStatus{{T: tMs / 1000, Feed: f.FeedID, JSON: string(b)}})
}

func (fs *feedState) reset() {
	fs.frames, fs.updates, fs.updateMsgs, fs.lastMs, fs.rate, fs.prevN = 0, 0, 0, 0, 0, 0
	fs.ew = ewRate{}
	fs.lat = latency{}
}

// feedLocked returns the state of a feed ID, creating it on first use. The
// table is bounded: beyond maxFeeds every unknown ID shares the last entry.
func (t *Tracker) feedLocked(id string) *feedState {
	if fs := t.lastFeed; fs != nil && fs.id == id {
		return fs
	}
	fs := t.feeds[id]
	if fs == nil {
		if len(t.feedList) >= maxFeeds {
			return t.feedList[len(t.feedList)-1]
		}
		fs = &feedState{id: id, idx: len(t.feedList), latOwn: t.part.ownsFeed(id)}
		t.feeds[id] = fs
		t.feedList = append(t.feedList, fs)
	}
	t.lastFeed = fs
	return fs
}

// HandleFrame implements model.FrameSink for raw Mode S frames. It is the
// standalone path: identical data from another feed or antenna within the
// de-duplication window is one transmission. Aggregator ingest uses Handle
// (after its own grouper) and Lanes.Credit instead.
func (t *Tracker) HandleFrame(f model.RawFrame) {
	if s := t.sh.Load(); s != nil {
		s.route(f.Data).HandleFrame(f)
		return
	}
	t.mu.Lock()
	ok := false
	defer t.unlockOnPanic(&ok)
	t0, sampled := t.holdStart()
	t.ingest(&f, nil, false)
	t.holdEnd(t0, sampled)
	ok = true
	if !t.slowPending() {
		t.mu.Unlock()
		return
	}
	t.finishSlow()
}

// finishSlow releases mu, then runs the work queued for outside the lock:
// notices, the lanes' housekeeping, guess samples and the EHS hook.
func (t *Tracker) finishSlow() {
	hook := t.hookPending
	ev, key := t.hookEv, t.hookKey
	t.hookPending = false
	laneDue := t.laneDueMs
	t.laneDueMs = 0
	var ls *Lanes
	if t.net != nil {
		ls = t.net.lanes
	}
	var gq []GuessSample
	gfn := t.guessFn
	if t.guessDue {
		t.guessDue = false
		if gfn != nil && len(t.guessQ) > 0 {
			gq = t.guessQ
			t.guessQ, t.guessSpare = t.guessSpare, nil
		} else {
			t.guessQ = t.guessQ[:0]
		}
	}
	t.unlockAndDeliver()
	if ls != nil && laneDue != 0 {
		ls.Tick(laneDue)
	}
	if gq != nil {
		t.deliverGuess(gfn, gq)
	}
	if !hook {
		return
	}
	t.hookMu.Lock()
	defer t.hookMu.Unlock()
	t.mu.Lock()
	fn := t.hook
	t.mu.Unlock()
	if fn == nil {
		return
	}
	res := fn(ev)
	t.mu.Lock()
	if ac := t.ac[key]; ac != nil {
		ms := ev.T.UnixMilli()
		if res.HasWind && finite(res.WindDir) && finite(res.WindSpd) {
			ac.windDir, ac.windSpd, ac.windMs = res.WindDir, res.WindSpd, ms
		}
		if res.HasOAT && finite(res.OAT) {
			ac.oat, ac.oatMs = res.OAT, ms
		}
	}
	t.mu.Unlock()
}

// ingest processes one raw frame. r (nil = not wanted) receives the
// outcome; unique skips the de-duplication cache (the caller has already
// de-duplicated: aggregator Handle). Caller holds mu.
func (t *Tracker) ingest(f *model.RawFrame, r *Result, unique bool) {
	tMs := t.stamp(f.Recv)
	t.cur = f.Station
	if r != nil {
		r.TMs = tMs
	}
	n := len(f.Data)
	if (f.Kind != model.FrameModeSShort && f.Kind != model.FrameModeSLong) || (n != 7 && n != 14) {
		switch f.Kind {
		case model.FrameModeAC:
			t.c.ModeAC++
			if t.rf != nil && !f.MLAT {
				rssi := f.Signal
				if math.IsInf(rssi, 0) {
					rssi = math.NaN()
				}
				fs := t.feedLocked(f.FeedID)
				slot := fs.slot
				if ch := f.Channel; ch >= 0 && ch < len(t.chanSlot) && t.chanSlot[ch] != 0 {
					slot = int(t.chanSlot[ch])
				}
				t.rfReject(f, fs, slot, rssi, rfstat.DFModeAC, rfstat.CRCUnknown, rfstat.KindModeAC)
			}
		case model.FrameStatus:
			t.c.Status++
			t.storeStatus(f, tMs)
		default:
			t.c.BadLength++
		}
		return
	}
	fs := t.feedLocked(f.FeedID)
	fs.frames++
	fs.lastMs = tMs
	slot := fs.slot
	ch := f.Channel
	if ch >= 0 && ch < len(t.chanSlot) && t.chanSlot[ch] != 0 {
		slot = int(t.chanSlot[ch])
	}
	if f.GPSNs >= 0 {
		t.sampleLatency(fs, f, tMs)
	}
	t.c.Frames++
	rssi := f.Signal
	if math.IsInf(rssi, 0) {
		rssi = math.NaN()
	}

	// De-duplicate identical frame data delivered by another antenna/feed.
	d := f.Data
	var a, b uint64
	if n == 14 {
		a = binary.BigEndian.Uint64(d[0:8])
		b = binary.BigEndian.Uint64(d[6:14])
	} else {
		a = uint64(binary.BigEndian.Uint32(d[0:4]))<<24 | uint64(d[4])<<16 | uint64(d[5])<<8 | uint64(d[6])
		b = 1 << 63
	}
	var e *ddEntry
	if unique {
		e = &t.ndd
		*e = ddEntry{a: a, b: b, t: tMs}
	} else {
		src := uint64(1) << (uint(fs.idx*4+ch+1) & 63)
		var dup bool
		e, dup = t.dedupe(a, b, tMs, src)
		if dup {
			t.creditDuplicate(e, slot, fs, rssi, tMs)
			return
		}
	}

	m := &t.msg
	err := modes.DecodeOpts(d, m, t.decOpts)
	dfIdx := modes.DFIndex(m.DF)
	e.dfIdx = uint8(dfIdx)
	if r != nil {
		r.DF, r.DFIdx = m.DF, uint8(dfIdx)
	}
	rfOn := t.rf != nil && !f.MLAT
	if err != nil {
		crc := rfstat.CRCBad
		switch err {
		case modes.ErrBadCRC:
			e.res = resBad
			t.c.CRCBad++
		case modes.ErrUnsupportedDF:
			e.res = resOther
			t.c.Unsupported++
			crc = rfstat.CRCUnknown
		default:
			e.res = resBad
			t.c.BadLength++
		}
		t.creditFrame(slot, e.res, dfIdx, rssi)
		if rfOn {
			t.rfReject(f, fs, slot, rssi, rfDF(m.DF, d), crc, rfstat.KindOther)
		}
		if r != nil {
			r.Status = e.res
		}
		return
	}

	// Address validation.
	key := m.AddrKey()
	ac := t.ac[key]
	confirmed := m.CRC == modes.CRCOK
	if !confirmed {
		// Parity-overlaid address, interrogator overlay or a repaired frame:
		// only for aircraft recently confirmed by a self-checking frame.
		if ac == nil || !fresh(ac.confirmedMs, tMs, confirmWindowMs) || m.AddressCorrected() {
			e.res = resReject
			crc := rfstat.CRCAddrRejected
			if m.CRC == modes.CRCFixed1 || m.CRC == modes.CRCFixed2 {
				t.c.CRCBad++
				e.res = resBad
				crc = rfstat.CRCBad
			} else {
				t.c.Rejected++
				if m.CRC == modes.CRCInterrogator {
					crc = rfstat.CRCOK
				}
			}
			t.creditFrame(slot, e.res, dfIdx, rssi)
			if rfOn {
				t.rfReject(f, fs, slot, rssi, rfDF(m.DF, d), crc, rfKind(m))
			}
			if r != nil {
				r.Status = e.res
			}
			return
		}
		if m.FixedBits > 0 {
			t.c.Fixed++
		}
	}
	if ac == nil {
		if ac = t.newAircraft(key, tMs); ac == nil {
			e.res = resReject
			t.c.Rejected++
			t.creditFrame(slot, e.res, dfIdx, rssi)
			if rfOn {
				t.rfReject(f, fs, slot, rssi, rfDF(m.DF, d), rfstat.CRCAddrRejected, rfKind(m))
			}
			if r != nil {
				r.Status = e.res
			}
			return
		}
		ac.addrType = m.AddrType
	}
	if confirmed {
		ac.confirmedMs = tMs
		if ac.nConfirmed < 255 {
			ac.nConfirmed++
		}
	}
	e.res, e.key = resOK, key
	t.c.Accepted++
	t.c.DF[dfIdx]++
	t.creditFrame(slot, resOK, dfIdx, rssi)
	t.heard(ac, tMs, slot, fs, rssi, 1)

	mlat := f.MLAT || fs.mlatOnly
	rejected := t.c.PosRejected
	t.lastBDS = 0
	t.cprOK = false
	gotPos := t.apply(ac, m, tMs, mlat, rssi, slot)
	if gotPos {
		e.pos, e.psrc = true, ac.posSrc
	}
	if t.onACAS != nil {
		if ra, ok := m.ACASRA(); ok {
			t.onACAS(t.acasReport(ac, m.DF, ra, tMs))
		}
	}
	if t.tj != nil {
		var off time.Duration
		if t.clock != nil && f.GPSNs >= 0 {
			off = t.clock.Offset()
		}
		t.feedTrajectory(ac, m, tMs, obsTime(f, tMs, off), gotPos)
	}
	if rfOn {
		t.rfAccepted(f, fs, slot, rssi, m, ac, tMs, gotPos, t.c.PosRejected != rejected, t.lastBDS)
	}
	if t.lat != nil {
		t.observeLatency(f, m, ac, tMs, gotPos && t.cprOK)
	}
	if gotPos && t.guessFn != nil && !mlat && ac.posSrc == store.SrcADSB {
		t.guessSample(ac, f, slot, rssi, tMs)
	}
	if t.net != nil && t.net.lanes == nil {
		ac.heard.mark(f.Station, tMs/1000) // otherwise Lanes.Credit marks it
	}
	if r != nil {
		t.fillResult(r, ac, tMs, gotPos, m.CPRSurface)
	}
}

// fillResult completes the Result of an accepted message or update.
func (t *Tracker) fillResult(r *Result, ac *aircraft, tMs int64, gotPos, surface bool) {
	r.Status, r.Key = ResultOK, ac.key
	r.Shown = ac.shown()
	r.Class = classOf(ac.sourceWithin(tMs, 120_000, 90_000))
	if ac.flightOpen {
		r.FlightID = ac.flight.ID
	}
	r.heard = &ac.heard
	if gotPos {
		r.Pos, r.Lat, r.Lon = true, ac.lat, ac.lon
		r.AltFt, r.Cell, r.PosSrc, r.Surface = ac.bestAlt(tMs), ac.cell, ac.posSrc, surface
	}
}

// latencyVelMs is the maximum age of the velocity handed to the latency
// analysis.
const latencyVelMs = 10_000

// observeLatency hands a fully handled frame to the latency analysis. It is
// called for every accepted frame (never for de-duplicated copies or parity
// failures); gotPos is true when this message produced an accepted position,
// which handleCPR left in cprLat / cprLon. Caller holds mu; t.lat != nil.
func (t *Tracker) observeLatency(f *model.RawFrame, m *modes.Message, ac *aircraft, tMs int64, gotPos bool) {
	o := &t.obs
	o.Reset()
	o.SetFrame(f)
	o.SetMessage(m)
	if gotPos {
		o.Lat, o.Lon = t.cprLat, t.cprLon
		if m.Has(modes.FAltBaro) {
			o.AltFt = float64(m.AltBaro)
		} else if m.Has(modes.FAltGNSS) {
			o.AltFt = float64(m.AltGNSS)
		}
	}
	if ac != nil && finite(ac.gs) && finite(ac.track) && fresh(ac.velMs, tMs, latencyVelMs) {
		o.GS, o.Track = ac.gs, ac.track
	}
	// Same clock as f.Recv and the NTP offset (ntp.Now: on Windows time.Now
	// only advances once per timer tick).
	o.Handled = ntp.Now()
	t.lat.Observe(o)
}

// SetLatency installs the latency-chain analysis fed with every accepted
// frame (nil removes it).
func (t *Tracker) SetLatency(a *lchain.Analyzer) {
	t.mu.Lock()
	t.lat = a
	t.mu.Unlock()
	if s := t.sh.Load(); s != nil {
		for _, p := range s.parts {
			p.SetLatency(a)
		}
	}
}

// rfDF returns the downlink format of a frame that may not have decoded
// (DF24..31 -> 24).
func rfDF(df uint8, d []byte) uint8 {
	if df == 0 && len(d) > 0 {
		df = d[0] >> 3
	}
	if df > 24 {
		df = 24
	}
	return df
}

// creditFrame adds one frame to the combined slot and the antenna slot.
func (t *Tracker) creditFrame(slot int, res uint8, dfIdx int, rssi float64) {
	t.slots[0].addFrame(res, dfIdx, rssi)
	if slot > 0 && slot < len(t.slots) {
		t.slots[slot].addFrame(res, dfIdx, rssi)
	}
}

// creditDuplicate credits the antenna that delivered a second copy of a
// transmission; the combined counters and the aircraft state are untouched.
func (t *Tracker) creditDuplicate(e *ddEntry, slot int, fs *feedState, rssi float64, tMs int64) {
	t.c.Duplicates++
	if slot <= 0 || slot >= len(t.slots) {
		slot = 0
	} else {
		t.slots[slot].addFrame(e.res, int(e.dfIdx), rssi)
	}
	if e.res != resOK {
		return
	}
	ac := t.ac[e.key]
	if ac == nil {
		return
	}
	t.markHeard(ac, tMs, slot, fs)
	if slot > 0 && e.pos && ac.posValid {
		t.creditPosition(slot, ac, rssi, tMs/1000, e.psrc)
	}
}

// newAircraft creates the state for a new address, or returns nil when the
// table is full.
func (t *Tracker) newAircraft(key uint32, tMs int64) *aircraft {
	if len(t.ac) >= t.maxAircraft {
		// Drop dormant aircraft early before refusing new ones.
		for k, ac := range t.ac {
			if tMs-ac.lastSeen > t.ttlMs {
				t.remove(k, ac)
			}
		}
		if len(t.ac) >= t.maxAircraft {
			return nil
		}
	}
	nan := math.NaN()
	ac := &aircraft{
		key: key, hex: modes.FormatICAO(key), firstSeen: tMs, lastSeen: tMs,
		version: -1, rssi: nan, rssiAvg: nan,
		dist: nan, brg: nan, elev: nan, slant: nan,
		recAlt: nan, recGS: nan, trailTrack: nan, trailAlt: nan,
		maxDistKm: nan, maxAltFt: nan,
	}
	t.initRef(ac)
	t.ac[key] = ac
	return ac
}

// markHeard records which antenna and feed heard the aircraft.
func (t *Tracker) markHeard(ac *aircraft, tMs int64, slot int, fs *feedState) {
	bit := uint32(1)
	if slot > 0 && slot < maxSlots {
		ac.antSeen[slot] = tMs
		bit |= 1 << uint(slot)
	}
	ac.minMask |= bit
	if fs != nil {
		if fs.idx >= len(ac.feedSeen) {
			ns := make([]int64, fs.idx+1)
			copy(ns, ac.feedSeen)
			ac.feedSeen = ns
		}
		ac.feedSeen[fs.idx] = tMs
	}
}

// heard updates the reception state of an aircraft for n new messages and
// maintains its flight session.
func (t *Tracker) heard(ac *aircraft, tMs int64, slot int, fs *feedState, rssi float64, n uint64) {
	if ac.flightOpen && tMs-ac.lastSeen > flightGapMs {
		t.closeFlight(ac)
	}
	if tMs > ac.lastSeen {
		ac.lastSeen = tMs
	}
	t.markHeard(ac, tMs, slot, fs)
	if finite(rssi) {
		ac.rssi, ac.rssiMs = rssi, tMs
		if math.IsNaN(ac.rssiAvg) {
			ac.rssiAvg = rssi
		} else {
			ac.rssiAvg += 0.1 * (rssi - ac.rssiAvg)
		}
	}
	if !ac.flightOpen {
		if !ac.shown() {
			ac.msgs += n
			return
		}
		t.openFlight(ac, tMs) // takes over the messages counted so far
	}
	ac.msgs += n
	fl := &ac.flight
	fl.Msgs += int64(n)
	fl.End = tMs / 1000
	ac.flightDirty, ac.seenDirty = true, true
	if finite(rssi) {
		ac.flRSSISum += rssi
		ac.flRSSIN++
		if !(fl.RSSIMin <= rssi) {
			fl.RSSIMin = rssi
		}
		if !(fl.RSSIMax >= rssi) {
			fl.RSSIMax = rssi
		}
	}
}

// setAltBaro stores a barometric altitude and its flight / record effects.
func (t *Tracker) setAltBaro(ac *aircraft, alt float64, tMs int64) {
	ac.altBaro, ac.altBaroMs = alt, tMs
	if ac.flightOpen {
		fl := &ac.flight
		if !(fl.MinAlt <= alt) {
			fl.MinAlt = alt
		}
		if !(fl.MaxAlt >= alt) {
			fl.MaxAlt = alt
		}
	}
	if !(ac.maxAltFt >= alt) {
		ac.maxAltFt = alt
	}
	// A record needs two consistent readings: a single damaged reply must
	// not become the all-time highest aircraft.
	if fresh(ac.recAltMs, tMs, 30_000) && math.Abs(alt-ac.recAlt) <= 2000 && alt > t.recValue(recMaxAlt) && ac.shown() {
		t.record(recMaxAlt, alt, ac.key, tMs/1000, recordMax, ac)
	}
	ac.recAlt, ac.recAltMs = alt, tMs
}

// setVelocity stores ground speed and track.
func (t *Tracker) setVelocity(ac *aircraft, gs, track float64, tMs int64) {
	ac.gs, ac.track, ac.velMs = gs, track, tMs
	if ac.flightOpen && !(ac.flight.MaxGS >= gs) {
		ac.flight.MaxGS = gs
	}
	if fresh(ac.recGSMs, tMs, 30_000) && math.Abs(gs-ac.recGS) <= 50 && gs > t.recValue(recMaxGS) && ac.shown() {
		t.record(recMaxGS, gs, ac.key, tMs/1000, recordMax, ac)
	}
	ac.recGS, ac.recGSMs = gs, tMs
}

// setCallsign stores a new callsign and refreshes the operator look-up.
func (t *Tracker) setCallsign(ac *aircraft, raw [8]byte, tMs int64) {
	ac.csMs = tMs
	if raw == ac.csRaw && ac.callsign != "" {
		return
	}
	cs := modes.TrimCallsign(raw)
	if cs == "" {
		return
	}
	ac.csRaw, ac.callsign = raw, cs
	if ac.flightOpen {
		ac.flight.Callsign = cs
		ac.flightDirty = true
	}
	t.rebuildRef(ac)
}

// setSquawk stores a Mode A code. Codes from unprotected replies need two
// equal readings in a row.
func (t *Tracker) setSquawk(ac *aircraft, sq uint16, trusted bool, tMs int64) {
	if !trusted && sq != ac.squawk && sq != ac.sqPending {
		ac.sqPending = sq
		return
	}
	ac.sqPending = sq
	changed := sq != ac.squawk || ac.sqMs == 0
	ac.squawk, ac.sqMs = sq, tMs
	if changed && ac.flightOpen {
		// Keep an emergency code once the flight has shown one.
		if cur := ac.flight.Squawk; cur == "" || modes.SquawkEmergency(sq) != "" || !(cur == "7500" || cur == "7600" || cur == "7700") {
			ac.flight.Squawk = modes.FormatSquawk(sq)
			ac.flightDirty = true
		}
	}
}

// apply folds a decoded message into the aircraft state. It reports whether
// the message produced an accepted position.
func (t *Tracker) apply(ac *aircraft, m *modes.Message, tMs int64, mlat bool, rssi float64, slot int) (gotPos bool) {
	v := m.Valid
	es := m.DF == 17 || m.DF == 18
	switch m.Source {
	case modes.SourceADSB:
		if !mlat {
			ac.lastESMs = tMs
		}
	case modes.SourceTISB, modes.SourceADSR:
		if !mlat {
			ac.lastTISBMs = tMs
		}
	}
	if es && ac.addrType != m.AddrType && (ac.addrType == modes.AddrModeS || ac.addrType == modes.AddrUnknown) {
		ac.addrType = m.AddrType
	}

	if v&modes.FCallsign != 0 {
		t.setCallsign(ac, m.Callsign, tMs)
	}
	if v&modes.FCategory != 0 && m.Category != 0 {
		ac.category, ac.categoryOK = m.Category, true
	}
	if v&modes.FSquawk != 0 {
		t.setSquawk(ac, m.Squawk, es, tMs)
	}
	if v&modes.FAltBaro != 0 {
		t.setAltBaro(ac, float64(m.AltBaro), tMs)
	}
	if v&modes.FAltGNSS != 0 {
		ac.altGNSS, ac.altGNSSMs = float64(m.AltGNSS), tMs
	}
	if v&modes.FGNSSDelta != 0 {
		ac.gnssDelta, ac.gnssDMs = float64(m.GNSSDelta), tMs
	}
	if mlat && fresh(ac.velADSBMs, tMs, ehsFreshMs) {
		// Keep the aircraft's own velocity over a multilaterated estimate.
	} else if v&(modes.FGroundSpeed|modes.FTrack) == modes.FGroundSpeed|modes.FTrack {
		t.setVelocity(ac, m.GS, m.Track, tMs)
		if !mlat {
			ac.velADSBMs = tMs
		}
	} else if v&modes.FGroundSpeed != 0 && fresh(ac.velMs, tMs, fieldTTLMs) {
		t.setVelocity(ac, m.GS, ac.track, tMs)
	}
	if v&modes.FHeading != 0 {
		ac.magHdg, ac.magHdgMs = m.Heading, tMs
	}
	if v&modes.FAirspeed != 0 {
		if m.AirspeedTAS {
			ac.tas, ac.tasMs = m.Airspeed, tMs
		} else {
			ac.ias, ac.iasMs = m.Airspeed, tMs
		}
	}
	if v&modes.FVertRate != 0 {
		ac.vertRate, ac.vrMs = float64(m.VertRate), tMs
	}
	if v&modes.FSelAlt != 0 {
		ac.selAlt, ac.selAltMs = float64(m.SelAlt), tMs
	}
	if v&modes.FBaroSetting != 0 {
		ac.qnh, ac.qnhMs = m.BaroSetting, tMs
	}
	if v&modes.FSelHeading != 0 {
		ac.selHdg, ac.selHdgMs = m.SelHeading, tMs
	}
	if v&modes.FNavModes != 0 {
		ac.navModes, ac.navMs = m.NavModes, tMs
	}
	if v&modes.FVersion != 0 {
		ac.version = int8(m.Version)
	}
	if v&modes.FNICSuppA != 0 {
		ac.nicA = m.NICSuppA
	}
	if v&modes.FNICSuppC != 0 {
		ac.nicC = m.NICSuppC
	}
	if v&modes.FNACp != 0 {
		ac.nacp, ac.nacpMs = m.NACp, tMs
	}
	if v&modes.FNACv != 0 {
		ac.nacv, ac.nacvMs = m.NACv, tMs
	}
	if v&modes.FSIL != 0 {
		ac.sil, ac.silMs = m.SIL, tMs
	}
	if v&modes.FEmergency != 0 {
		ac.emergency, ac.emMs = m.Emergency, tMs
	}
	if v&modes.FAlert != 0 {
		ac.alert, ac.alertMs = m.Alert, tMs
	}
	if v&modes.FSPI != 0 {
		ac.spi, ac.spiMs = m.SPI, tMs
	}
	switch m.AirGround {
	case modes.AGGround:
		ac.onGround, ac.groundMs = true, tMs
	case modes.AGAirborne:
		ac.onGround, ac.groundMs = false, tMs
	case modes.AGUncertain:
		// "Airborne" from a field that is also used when the transponder
		// cannot tell: believe it for airborne-format squitters, or when
		// nothing recent says otherwise.
		if es || !fresh(ac.groundMs, tMs, 15_000) || !ac.onGround {
			ac.onGround, ac.groundMs = false, tMs
		}
	}

	if v&modes.FCPR != 0 {
		if nic, rc, ok := m.NICRc(max(int(ac.version), 0), ac.nicA, ac.nicC); ok {
			ac.nic, ac.rcM, ac.nicMs = int8(nic), rc, tMs
		}
		gotPos = t.handleCPR(ac, m, tMs, mlat, rssi, slot)
	}
	if v&modes.FCommB != 0 {
		t.applyCommB(ac, m, tMs)
	}
	if v&(modes.FEmergency|modes.FSquawk) != 0 {
		t.checkEmergency(ac, tMs)
	}
	return gotPos
}

// applyCommB identifies the register of a DF20/21 reply using what is known
// about the aircraft and stores its content.
func (t *Tracker) applyCommB(ac *aircraft, m *modes.Message, tMs int64) {
	nan := math.NaN()
	gs, trk, alt := nan, nan, nan
	if fresh(ac.velMs, tMs, 30_000) {
		gs, trk = ac.gs, ac.track
	}
	if fresh(ac.altBaroMs, tMs, 30_000) {
		alt = ac.altBaro
	}
	b, _, ok := modes.DisambiguateBDS(m, gs, trk, alt)
	if !ok {
		return
	}
	t.lastBDS = uint8(b)
	mb := m.MB()
	ehs := false
	switch b {
	case modes.BDS40:
		t.c.BDS40++
		d := modes.DecodeBDS40(mb)
		switch {
		case d.MCPAltOK:
			ac.selAlt, ac.selAltMs = float64(d.MCPAlt), tMs
		case d.FMSAltOK:
			ac.selAlt, ac.selAltMs = float64(d.FMSAlt), tMs
		}
		if d.BaroOK {
			ac.qnh, ac.qnhMs = d.Baro, tMs
		}
		if d.ModesOK {
			ac.navModes, ac.navMs = d.NavModes(), tMs
		}
	case modes.BDS50:
		t.c.BDS50++
		d := modes.DecodeBDS50(mb)
		if d.RollOK {
			ac.roll, ac.rollMs = d.Roll, tMs
		}
		if d.TrackRateOK {
			ac.trackRate, ac.trackRateMs = d.TrackRate, tMs
		}
		if d.TASOK {
			ac.tas, ac.tasMs = d.TAS, tMs
		}
		// ADS-B velocity is better than the 2 kt / 0.18 deg register values.
		if d.GSOK && d.TrackOK && !fresh(ac.velADSBMs, tMs, ehsFreshMs) {
			t.setVelocity(ac, d.GS, d.Track, tMs)
		}
		ehs = true
	case modes.BDS60:
		t.c.BDS60++
		d := modes.DecodeBDS60(mb)
		if d.HeadingOK {
			ac.magHdg, ac.magHdgMs = d.MagHeading, tMs
		}
		if d.IASOK {
			ac.ias, ac.iasMs = d.IAS, tMs
		}
		if d.MachOK {
			ac.mach, ac.machMs = d.Mach, tMs
		}
		if !fresh(ac.vrMs, tMs, ehsFreshMs) {
			if d.BaroRateOK {
				ac.vertRate, ac.vrMs = float64(d.BaroRate), tMs
			} else if d.InertialRateOK {
				ac.vertRate, ac.vrMs = float64(d.InertialRate), tMs
			}
		}
		ehs = true
	case modes.BDS45:
		t.c.BDS45++
		d := modes.DecodeBDS45(mb)
		if d.TempOK && (d.Temp < -80 || d.Temp > 60) {
			return // implausible: most likely not a 4,5 after all
		}
		if t.onHaz != nil && (d.TurbulenceOK || d.WindShearOK || d.MicroburstOK || d.IcingOK || d.WakeVortexOK || d.TempOK) {
			r := HazardReport{T: tMs, ICAO: ac.key, Callsign: ac.callsign, H: d}
			if ac.posValid && tMs-ac.posMs <= posTTLMs {
				r.Lat, r.Lon, r.PosOK = ac.lat, ac.lon, true
			}
			if fresh(ac.altBaroMs, tMs, fieldTTLMs) {
				r.AltFt, r.AltOK = ac.altBaro, true
			}
			t.onHaz(r)
		}
		return
	case modes.BDS44:
		t.c.BDS44++
		d := modes.DecodeBDS44(mb)
		if d.FOM == 0 || d.FOM > 4 {
			return
		}
		if d.WindOK && d.WindSpeed <= 250 {
			ac.mrarWindSpd, ac.mrarWindDir, ac.mrarWindMs = d.WindSpeed, d.WindDir, tMs
		}
		if d.Temp >= -80 && d.Temp <= 60 {
			ac.mrarTemp, ac.mrarTempMs = d.Temp, tMs
		}
		ehs = true
	default:
		return
	}
	if !ehs || !ac.posValid || tMs-ac.posMs > 15_000 {
		return
	}
	if t.hook == nil {
		// No meteorology engine: show the aircraft's own report directly.
		if fresh(ac.mrarWindMs, tMs, 0) {
			ac.windDir, ac.windSpd, ac.windMs = ac.mrarWindDir, ac.mrarWindSpd, tMs
		}
		if fresh(ac.mrarTempMs, tMs, 0) {
			ac.oat, ac.oatMs = ac.mrarTemp, tMs
		}
		return
	}
	if t.hookPending || tMs-ac.hookMs < hookIntervalMs {
		return
	}
	ac.hookMs = tMs
	pick := func(v float64, at int64) float64 {
		if fresh(at, tMs, ehsFreshMs) {
			return v
		}
		return nan
	}
	t.hookEv = EHSEvent{
		T: time.UnixMilli(tMs).UTC(), ICAO: ac.key & 0xFFFFFF,
		Lat: ac.lat, Lon: ac.lon,
		AltFt:       pick(ac.altBaro, ac.altBaroMs),
		GS:          pick(ac.gs, ac.velMs),
		Track:       pick(ac.track, ac.velMs),
		TAS:         pick(ac.tas, ac.tasMs),
		IAS:         pick(ac.ias, ac.iasMs),
		Mach:        pick(ac.mach, ac.machMs),
		MagHeading:  pick(ac.magHdg, ac.magHdgMs),
		Roll:        pick(ac.roll, ac.rollMs),
		MRARWindSpd: pick(ac.mrarWindSpd, ac.mrarWindMs),
		MRARWindDir: pick(ac.mrarWindDir, ac.mrarWindMs),
		MRARTemp:    pick(ac.mrarTemp, ac.mrarTempMs),
	}
	t.hookKey = ac.key
	t.hookPending = true
}

// HandleUpdate implements model.FrameSink for pre-decoded updates (SBS,
// aircraft.json).
//
// In aggregator mode an update from a station (DECODED records, compat
// SBS / JSON inputs) is also credited to that station's lane, and a late
// (backfilled) update reaches only the lanes. Own MLAT results (Station 0,
// MLAT) are no receptions and credit no lane.
func (t *Tracker) HandleUpdate(u model.DecodedUpdate) {
	if u.Late {
		if ls := t.lanesP.Load(); ls != nil {
			ls.BackfillUpdate(u)
			return
		}
	}
	if s := t.sh.Load(); s != nil {
		s.parts[ShardOf(u.ICAO, s.n)].HandleUpdate(u)
		return
	}
	var r Result
	t.mu.Lock()
	ok := false
	defer t.unlockOnPanic(&ok)
	t.ingestUpdate(&u, &r)
	ok = true
	var ls *Lanes
	if t.net != nil && u.Station != model.LocalStation && !u.MLAT && u.Src == "" {
		ls = t.net.lanes
	}
	if !t.slowPending() {
		t.mu.Unlock()
	} else {
		t.finishSlow()
	}
	if ls != nil {
		sig := math.NaN()
		if u.Signal != nil {
			sig = *u.Signal
		}
		ls.Credit(u.Station, -1, sig, &r)
	}
}

// unlockOnPanic releases t.mu when ingest panicked (*ok still false), so a
// panic recovered further up (feed.runGuarded) cannot leave the tracker
// locked forever. The panic itself keeps propagating.
func (t *Tracker) unlockOnPanic(ok *bool) {
	if !*ok {
		t.mu.Unlock()
	}
}

func (t *Tracker) ingestUpdate(u *model.DecodedUpdate, r *Result) {
	tMs := t.stamp(u.Recv)
	t.cur = u.Station
	r.TMs = tMs
	fs := t.feedLocked(u.FeedID)
	n := uint64(1)
	if u.Msgs > 1 {
		n = uint64(u.Msgs)
	}
	fs.updates++
	fs.updateMsgs += n
	fs.lastMs = tMs
	t.c.Updates++
	t.c.UpdateMsgs += n
	key := u.ICAO & (0xFFFFFF | modes.NonICAOFlag)
	if key&0xFFFFFF == 0 {
		t.c.Rejected++
		return
	}
	ac := t.ac[key]
	if ac == nil {
		if ac = t.newAircraft(key, tMs); ac == nil {
			t.c.Rejected++
			return
		}
		ac.addrType = modes.AddrModeS
		if key&modes.NonICAOFlag != 0 {
			ac.addrType = modes.AddrADSBOther
		}
	}
	ac.fromUpdate = true
	rssi := math.NaN()
	if u.Signal != nil && finite(*u.Signal) {
		rssi = *u.Signal
	}
	slot := fs.slot
	t.slots[0].addUpdate(n, rssi)
	if slot > 0 && slot < len(t.slots) {
		t.slots[slot].addUpdate(n, rssi)
	}
	t.heard(ac, tMs, slot, fs, rssi, n)

	if u.Callsign != nil {
		var raw [8]byte
		copy(raw[:], "        ")
		copy(raw[:], *u.Callsign)
		for i, c := range raw {
			if c >= 'a' && c <= 'z' {
				raw[i] = c - 32
			}
		}
		t.setCallsign(ac, raw, tMs)
	}
	if u.Squawk != nil {
		if sq, ok := parseSquawk(*u.Squawk); ok {
			t.setSquawk(ac, sq, true, tMs)
		}
	}
	if u.AltBaro != nil && finite(*u.AltBaro) {
		t.setAltBaro(ac, *u.AltBaro, tMs)
	}
	if u.AltGeom != nil && finite(*u.AltGeom) {
		ac.altGNSS, ac.altGNSSMs = *u.AltGeom, tMs
	}
	if u.GS != nil && finite(*u.GS) && *u.GS >= 0 {
		trk := ac.track
		if u.Track != nil && finite(*u.Track) {
			trk = *u.Track
		}
		if u.Track != nil || fresh(ac.velMs, tMs, fieldTTLMs) {
			t.setVelocity(ac, *u.GS, trk, tMs)
		}
	}
	if u.VertRate != nil && finite(*u.VertRate) {
		ac.vertRate, ac.vrMs = *u.VertRate, tMs
	}
	if u.OnGround != nil {
		ac.onGround, ac.groundMs = *u.OnGround, tMs
	}
	gotPos := false
	if u.Lat != nil && u.Lon != nil {
		src, label := store.SrcOther, model.SrcOther
		netMLAT := u.Src == model.SrcMLATAV || u.Src == model.SrcMLATExt
		if u.MLAT || fs.mlatOnly || netMLAT {
			src, label = store.SrcMLAT, model.SrcMLAT
		}
		// A decoded position (SBS / JSON input, an MLAT result) does not
		// depend on the previous one: it is an independent fix, so it can
		// start a track on an aircraft also heard through Mode S frames
		// (with a second agreeing fix, like a global CPR decode).
		gotPos = t.offerPosition(ac, *u.Lat, *u.Lon, tMs, src, label, rssi, slot, true, ac.onGround && fresh(ac.groundMs, tMs, fieldTTLMs))
		if gotPos && netMLAT {
			ac.mlatPosMs, ac.mlatGrade, ac.mlatSigma = ac.posMs, u.Grade, math.NaN()
			if u.SigmaM != nil && finite(*u.SigmaM) && *u.SigmaM >= 0 {
				ac.mlatSigma = *u.SigmaM
			}
		}
	}
	if u.Squawk != nil {
		t.checkEmergency(ac, tMs)
	}
	t.fillResult(r, ac, tMs, gotPos, ac.onGround && fresh(ac.groundMs, tMs, fieldTTLMs))
	r.Msgs, r.DFIdx = uint32(n), modes.DFIndexOther
	if t.net != nil && t.net.lanes == nil && u.Station != model.LocalStation && !u.MLAT && u.Src == "" {
		ac.heard.mark(u.Station, tMs/1000)
	}
	if t.tj != nil {
		var o trajectory.Obs
		if gotPos {
			o.SetPos(ac.lat, ac.lon, trajectory.SourceFromString(ac.posLabel))
		}
		if ac.velMs == tMs && finite(ac.gs) && finite(ac.track) {
			o.SetVel(ac.gs, ac.track)
		}
		if u.AltBaro != nil && ac.altBaroMs == tMs {
			o.SetAlt(ac.altBaro)
		}
		if u.VertRate != nil && ac.vrMs == tMs {
			o.SetVRate(ac.vertRate)
		}
		if u.OnGround != nil {
			o.OnGround = *u.OnGround
			o.Has |= trajectory.HasGround
		}
		if o.Has&(trajectory.HasPos|trajectory.HasVel|trajectory.HasAlt|trajectory.HasVRate) != 0 {
			o.T = u.Recv
			if o.T.IsZero() {
				o.T = time.UnixMilli(tMs)
			}
			t.tj.Observe(ac.key, &o)
		}
	}
}

// parseSquawk converts four octal digits to the nibble form (0x7700).
func parseSquawk(s string) (uint16, bool) {
	if len(s) != 4 {
		return 0, false
	}
	var v uint16
	for i := 0; i < 4; i++ {
		c := s[i]
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v<<4 | uint16(c-'0')
	}
	return v, true
}

// acasReport builds the ACAS hook's report from the aircraft state. Caller holds mu.
func (t *Tracker) acasReport(ac *aircraft, df uint8, ra modes.ACASRA, tMs int64) acas.Report {
	r := acas.Report{T: tMs, ICAO: ac.key, Callsign: ac.callsign, DF: df, RA: ra}
	if ac.posValid && tMs-ac.posMs <= posTTLMs {
		r.Lat, r.Lon, r.PosOK = ac.lat, ac.lon, true
	}
	if fresh(ac.altBaroMs, tMs, fieldTTLMs) {
		r.AltFt, r.AltOK = ac.altBaro, true
	}
	return r
}
