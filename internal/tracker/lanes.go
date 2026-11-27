package tracker

import (
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/geo"
	"airview/internal/geoid"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/store"
)

// Lanes keeps the per-station statistics of aggregator mode
// (docs/aggregator/DESIGN.md §6.3): one lane per station with the same
// accumulators the standalone tracker keeps per antenna (counters, range,
// coverage, signal, elevation, heat), the station's own per-minute and
// per-hour aircraft sets and the per-flight aggregates behind the
// flight_station table.
//
// Every copy of a transmission is credited to the lane of the station that
// heard it: the first copy by Tracker.Handle, later copies by the grouper
// through Credit with the Result Handle returned. Credit never takes the
// tracker mutex; lanes are sharded laneShards (256) ways by station index,
// each shard with its own mutex, and a credit costs well under 120 ns
// without allocating. A worker may buffer its credits in a CreditBatch
// (lanebatch.go), which takes each shard mutex once per batch.
//
// Range, coverage, heat, signal and elevation are credited only while the
// station is located (StationResolver.Geo); counters always. Rows are
// written to LaneOptions.Store (stations.db) keyed "s<idx>" for the station
// and "s<idx>/<ch>" per antenna channel (model.StationAntKey): minute rows
// every FlushEvery (10 s) round-robin across stations, range_hour and the
// day tables (coverage, signal, elevation, heat) every SlowFlushEvery.
//
// Lanes run on data time like the tracker: Tick (driven by the tracker's
// 1 Hz housekeeping) flushes, refreshes station positions and evicts lanes
// offline for EvictAfter (24 h).
type Lanes struct {
	o      LaneOptions
	shards [laneShards]laneShard

	nowMs atomic.Int64 // newest data time credited or ticked

	tickMu sync.Mutex // one Tick / FlushAll at a time

	closedMu sync.Mutex
	closed   []closedFlight // flights the tracker closed since the last tick
	closedX  []closedFlight // tick scratch

	lateDropped atomic.Uint64 // late minutes dropped (bound reached)

	// Tick scratch (guarded by tickMu).
	tMin  []store.StatsRow
	tHour []store.RangeHour
	tFS   []FlightStation

	// tapRows sees every tick's rows before they are written (tests).
	tapRows func(min []store.StatsRow, hour []store.RangeHour)
}

// LaneStore receives the lane rows (stations.db). *store.Store implements it.
type LaneStore interface {
	Submit(b *store.Batch)
}

// LaneOptions configures NewLanes. Zero values select the defaults.
type LaneOptions struct {
	// Resolver supplies station positions (Geo) and SIDs (Info). nil = no
	// station is ever located and flight_station rows carry no SID.
	Resolver model.StationResolver
	// Store receives the statistics rows; nil = discard them (tests).
	Store LaneStore
	// OnFlightStation receives the per-(flight, station) aggregates when a
	// flight closes (or its station stops hearing it for good). Called from
	// Tick without locks held, never concurrently with itself; the slice is
	// reused after the call returns. nil = drop them.
	OnFlightStation func(rows []FlightStation)
	// MaxRangeKm: positions farther than this from a station never enter its
	// range / coverage products. Default 800.
	MaxRangeKm float64
	// FlushEvery is the interval of a lane's stats_minute rows. Default 10 s.
	FlushEvery time.Duration
	// SlowFlushEvery is the interval of a lane's range_hour, coverage,
	// signal, elevation and heat increments. Default 5 min.
	SlowFlushEvery time.Duration
	// GeoRefresh is how often a lane re-reads its station position. Default 30 s.
	GeoRefresh time.Duration
	// EvictAfter drops a lane (after a final flush) when its station has been
	// silent this long. Default 24 h.
	EvictAfter time.Duration
}

const (
	// laneShards spreads the stations over this many mutexes. Every copy
	// takes one; with 16 the ingest workers of a 1000-feeder aggregator
	// spent a tenth of their CPU contending (SO-5). 256 keeps contention
	// low up to 64 cores at about 30 kB of fixed memory.
	laneShards     = 256
	laneChannels   = 8                      // antenna channels with their own stats (0..7)
	laneStaleMs    = flightGapMs + 5*60_000 // aircraft entries unused this long are dropped
	laneLateMax    = 1440                   // late minutes kept per lane before dropping
	laneClosedMax  = 1 << 16                // queued flight closes
	laneSilentMs   = 60_000                 // a lane silent this long does not count coverage
	defaultRangeKm = 800
)

// FlightStation is one row of flight_station (DESIGN §6.4): what one station
// contributed to one flight.
type FlightStation struct {
	FlightID int64
	Station  model.StationIdx
	SID      string // from the resolver ("" = unknown)
	ICAO     uint32 // address key
	// First / Last are the Unix seconds of the station's first and last copy.
	First, Last int64
	Msgs        int64 // copies credited
	Positions   int64 // copies that carried an accepted position
	// MaxDistKm / MinDistKm are the farthest / nearest position from the
	// station (NaN when the station was not located).
	MaxDistKm, MinDistKm float64
	// RSSI statistics of the copies, dBFS (NaN = none).
	RSSIMin, RSSIMax, RSSIAvg float64
}

// LaneInfo is a summary of one lane for status pages.
type LaneInfo struct {
	Station    model.StationIdx
	Located    bool
	Frames     uint64 // copies credited since the lane was created
	Positions  uint64
	LateFrames uint64
	Aircraft   int // aircraft heard in the current minute
	FirstMs    int64
	LastMs     int64 // data time of the last copy
}

type closedFlight struct {
	key uint32
	id  int64
}

type laneShard struct {
	mu    sync.Mutex
	lanes []*lane // by station index / laneShards
	dec   modes.Message
	scr   dayScratch                // dayIncs row buffers of the shard's lanes
	gone  map[model.StationIdx]bool // dropped (deleted) stations: never re-created
	_     [64]byte                  // keep shards on separate cache lines
}

// laneAC is a lane's state for one aircraft: its per-minute and per-hour set
// membership (stamped with the minute / hour it belongs to, so the sets never
// need clearing) and the running flight_station aggregate.
type laneAC struct {
	min      int64  // minute start (Unix s) minMask / posMask belong to
	hour     int64  // hour start hourMask belongs to
	minMask  uint16 // bit 0 = station, bit 1+ch = channel
	posMask  uint16
	hourMask uint16
	cls      uint8
	heardIx  int8 // heardBy slot of this station (-1 = unknown), a hint
	lastMs   int64
	heardSec int64 // data second this station last marked the aircraft's heardBy

	flightID           int64
	fFirst, fLast      int64
	fMsgs, fPos        int64
	fMaxKm, fMinKm     float64
	fRSSIMin, fRSSIMax float64
	fRSSISum           float64
	fRSSIN             int64
}

// lanePos is an accepted position waiting for fold: the geometry products
// (range, coverage, heat, signal, elevation, flight distance) are computed in
// batches, off the per-copy path and with the lane's arrays warm.
type lanePos struct {
	lat, lon, alt, rssi float64
	sec, flight         int64
	key, cell           uint32
	ch                  int8 // antenna channel with its own stats (-1 = none)
	chOnly              bool // a diversity copy: the channel's products only
}

// lanePosMax is the most positions a lane buffers before folding them
// inline.
const lanePosMax = 1024

// lateMinute accumulates backfilled frames of one past minute.
type lateMinute struct {
	frames, crcBad   int64
	df               [modes.NumDFIndex]int64
	positions        int64
	rssiSum          float64
	rssiN            int64
	rssiMin, rssiMax float64
	ac               map[uint32]struct{}
}

type lane struct {
	idx     model.StationIdx
	all     *antStats
	ch      [laneChannels]*antStats
	geo     *geo.Station // nil = not located
	geoG    model.StationGeo
	geoAtMs int64

	minStart   int64 // Unix s of the current minute (0 = nothing yet)
	hourStart  int64
	day        int
	covMs      int64 // data time from which the current minute is counted
	firstMs    int64
	lastMs     int64
	lastIncSec int64

	nextFlushMs int64
	nextSlowMs  int64

	ac acTable
	// gauge holds the current minute's aircraft-set sizes per slot (0 =
	// station, 1+ch = channel): aircraft, with position, ADS-B, MLAT, Mode S.
	// Maintained incrementally as minMask / posMask bits are first set, so a
	// flush never scans the aircraft table; reset when the minute changes.
	gauge [1 + laneChannels][5]int32
	dels  []uint32  // sweep scratch
	pos   []lanePos // positions waiting for fold

	pendMin  []store.StatsRow
	pendHour []store.RangeHour
	pendFS   []FlightStation
	pb       *store.Batch // day-table increments of a completed day
	late     map[int64]*lateMinute

	totFrames, totPos, totLate uint64
}

// NewLanes creates an empty lane set.
func NewLanes(o LaneOptions) *Lanes {
	if !(o.MaxRangeKm > 0) {
		o.MaxRangeKm = defaultRangeKm
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = 10 * time.Second
	}
	if o.SlowFlushEvery <= 0 {
		o.SlowFlushEvery = 5 * time.Minute
	}
	if o.GeoRefresh <= 0 {
		o.GeoRefresh = 30 * time.Second
	}
	if o.EvictAfter <= 0 {
		o.EvictAfter = 24 * time.Hour
	}
	return &Lanes{o: o}
}

// Credit adds one copy of a transmission heard by station st on antenna
// channel ch (< 0 = unknown) with signal sig (dBFS, NaN/-Inf = unknown) to
// the station's lane. r is the Result Tracker.Handle returned for the first
// copy of the same transmission. Credit is safe for concurrent use, never
// takes the tracker mutex and does not allocate in steady state.
func (ls *Lanes) Credit(st model.StationIdx, ch int, sig float64, r *Result) {
	if r == nil || r.Status == ResultNone || r.Status == ResultLate {
		return
	}
	ls.observe(r.TMs)
	sh := &ls.shards[st%laneShards]
	sh.mu.Lock()
	ls.laneLocked(sh, st, r.TMs).credit(ls, ch, sig, r, false)
	sh.mu.Unlock()
}

// CreditChannel credits a diversity copy: the same transmission heard by
// station st again on another antenna channel ch (C1). Only that channel's
// statistics move; the station's combined counters, its heard set and the
// flight aggregates already counted the transmission with its first copy.
// Copies without a valid channel are ignored.
func (ls *Lanes) CreditChannel(st model.StationIdx, ch int, sig float64, r *Result) {
	if r == nil || r.Status == ResultNone || r.Status == ResultLate || ch < 0 || ch >= laneChannels {
		return
	}
	ls.observe(r.TMs)
	sh := &ls.shards[st%laneShards]
	sh.mu.Lock()
	ls.laneLocked(sh, st, r.TMs).credit(ls, ch, sig, r, true)
	sh.mu.Unlock()
}

// observe advances the lanes' data time.
func (ls *Lanes) observe(ms int64) {
	for {
		cur := ls.nowMs.Load()
		if ms <= cur || ls.nowMs.CompareAndSwap(cur, ms) {
			return
		}
	}
}

// laneLocked returns the lane of st, creating it. Caller holds sh.mu.
func (ls *Lanes) laneLocked(sh *laneShard, st model.StationIdx, tMs int64) *lane {
	i := int(st / laneShards)
	if i < len(sh.lanes) {
		if l := sh.lanes[i]; l != nil {
			return l
		}
	} else {
		n := max(2*len(sh.lanes), i+1, 8)
		nl := make([]*lane, n)
		copy(nl, sh.lanes)
		sh.lanes = nl
	}
	l := &lane{
		idx: st, all: newAntStats(model.StationAntKey(st, -1)),
		firstMs: tMs, lastMs: tMs,
	}
	if sh.gone[st] {
		// A deleted station's late copies go to a lane that is never
		// stored, so never flushed (Drop).
		return l
	}
	// Stagger the flushes so that lanes are written round-robin.
	every := ls.o.FlushEvery.Milliseconds()
	l.nextFlushMs = tMs + every + int64(st)%10*every/10
	// SCALE-03: spread the slow flushes (day tables, heat: the big ones)
	// over the whole interval by station index, so that 1,000 lanes that
	// joined together after a restart do not all flush in the same tick.
	slow := ls.o.SlowFlushEvery.Milliseconds()
	l.nextSlowMs = tMs + slow/2 + int64(uint32(st)*2654435761%1000)*slow/1000
	l.refreshGeo(ls, tMs)
	sh.lanes[i] = l
	return l
}

// refreshGeo re-reads the station position from the resolver.
func (l *lane) refreshGeo(ls *Lanes, tMs int64) {
	l.geoAtMs = tMs
	if ls.o.Resolver == nil {
		l.geo = nil
		return
	}
	g, ok := ls.o.Resolver.Geo(l.idx)
	if !ok || !geo.ValidLatLon(g.Lat, g.Lon) {
		l.geo = nil
		return
	}
	if l.geo != nil && g == l.geoG {
		return
	}
	l.geoG = g
	l.geo = stationFromGeo(g)
}

// stationFromGeo builds the geometry station of a network station: its
// ellipsoidal antenna height is turned into MSL with the geoid (when the
// grid is available), the reference of barometric aircraft altitudes.
func stationFromGeo(g model.StationGeo) *geo.Station {
	h := finiteOr(g.HAE, 0)
	if msl, ok := geoid.HAEToMSL(g.Lat, g.Lon, h); ok {
		h = msl
	}
	return geo.NewStation(g.Lat, g.Lon, h, 0)
}

// chanStats returns the statistics of antenna channel ch (nil = none).
func (l *lane) chanStats(ch int) *antStats {
	if ch < 0 || ch >= laneChannels {
		return nil
	}
	s := l.ch[ch]
	if s == nil {
		s = newAntStats(model.StationAntKey(l.idx, ch))
		l.ch[ch] = s
	}
	return s
}

// multiChannel reports whether the station has reported at least two
// antenna channels. Only then are per-channel rows ("s<idx>/<ch>") written:
// for a single-channel station they would duplicate the station rows.
// The channel slots are kept (and reset) either way, so a station that adds
// a second channel later starts writing them at the next row.
func (l *lane) multiChannel() bool {
	n := 0
	for _, s := range l.ch {
		if s != nil {
			n++
		}
	}
	return n >= 2
}

// entry returns the aircraft entry of key, creating it. The pointer is
// valid until the next entry or delete.
func (l *lane) entry(key uint32) *laneAC { return l.ac.getOrAdd(key) }

// startMinute initialises the minute / hour / day clocks at the first copy.
func (l *lane) startMinute(tMs int64) {
	sec := tMs / 1000
	l.minStart = sec - mod(sec, 60)
	l.hourStart = sec - mod(sec, 3600)
	l.day = store.DayOf(sec)
	l.covMs = tMs
	l.gauge = [1 + laneChannels][5]int32{}
}

// credit applies one copy and marks the station in the aircraft's heardBy
// set (r.heard; at most once per data second and aircraft). With chOnly
// (a diversity copy, CreditChannel) only channel ch's statistics move.
// Caller holds the shard lock.
func (l *lane) credit(ls *Lanes, ch int, sig float64, r *Result, chOnly bool) {
	tMs := r.TMs
	switch {
	case l.minStart == 0:
		l.startMinute(tMs)
	case tMs < l.minStart*1000:
		if chOnly {
			return // late minutes are kept per station only
		}
		// Older than the current minute (a slow copy across the boundary):
		// count it in its own minute.
		l.lateCredit(ls, tMs, r.Status, int(r.DFIdx), r.Key, sig, r.Msgs, r.Pos)
		if r.Status == ResultOK {
			r.heard.mark(l.idx, tMs/1000)
		}
		return
	case tMs >= (l.minStart+60)*1000:
		l.roll(ls, tMs)
	}
	if tMs > l.lastMs {
		l.lastMs = tMs
	}
	rssi := sig
	if math.IsInf(rssi, 0) {
		rssi = math.NaN()
	}
	cs := l.chanStats(ch)
	if chOnly && cs == nil {
		return
	}
	all := l.all
	if chOnly {
		all = nil
	} else {
		l.totFrames++
	}
	if r.Msgs > 0 {
		n := uint64(r.Msgs)
		if all != nil {
			all.addUpdate(n, rssi)
		}
		if cs != nil {
			cs.addUpdate(n, rssi)
		}
	} else {
		if all != nil {
			all.addFrame(r.Status, int(r.DFIdx), rssi)
		}
		if cs != nil {
			cs.addFrame(r.Status, int(r.DFIdx), rssi)
		}
	}
	if r.Status != ResultOK || r.Key == 0 {
		return
	}
	e := l.entry(r.Key)
	e.lastMs = tMs
	sec := tMs / 1000
	if e.heardSec != sec && r.heard != nil && !chOnly {
		e.heardSec = sec
		e.heardIx = int8(r.heard.markHint(l.idx, sec, int(e.heardIx)))
	}
	bit := uint16(1)
	if chOnly {
		bit = 0
	}
	if cs != nil {
		bit |= 1 << uint(ch+1)
	}
	if r.Shown {
		if e.min != l.minStart {
			e.min, e.minMask, e.posMask = l.minStart, 0, 0
		}
		if e.minMask != 0 && e.cls != r.Class {
			l.gaugeClass(e.minMask, e.cls, -1) // class changed within the minute
			l.gaugeClass(e.minMask, r.Class, +1)
		}
		if nb := bit &^ e.minMask; nb != 0 {
			for m := nb; m != 0; m &= m - 1 {
				l.gauge[bits.TrailingZeros16(m)][0]++
			}
			l.gaugeClass(nb, r.Class, +1)
			e.minMask |= nb
		}
		e.cls = r.Class
	}
	if r.FlightID != 0 && !chOnly {
		if e.flightID != r.FlightID {
			if e.flightID != 0 {
				l.fold(ls) // the old flight's buffered distances first
				l.emitFlightKey(r.Key, e)
			}
			nan := math.NaN()
			e.flightID, e.fFirst, e.fMsgs, e.fPos = r.FlightID, sec, 0, 0
			e.fMaxKm, e.fMinKm, e.fRSSIMin, e.fRSSIMax, e.fRSSISum, e.fRSSIN = nan, nan, nan, nan, 0, 0
		}
		e.fLast = sec
		e.fMsgs += int64(max(r.Msgs, 1))
		if rssi == rssi {
			if !(e.fRSSIMin <= rssi) {
				e.fRSSIMin = rssi
			}
			if !(e.fRSSIMax >= rssi) {
				e.fRSSIMax = rssi
			}
			e.fRSSISum += rssi
			e.fRSSIN++
		}
	}
	if r.Pos {
		l.creditPos(ls, e, bit, cs, r, rssi, sec, chOnly)
	}
}

// gaugeClass adds d to the class gauge (ADS-B, MLAT, Mode S) of cls for
// every slot in mask (bit 0 = station, bit 1+ch = channel).
func (l *lane) gaugeClass(mask uint16, cls uint8, d int32) {
	var j int
	switch cls {
	case classADSB:
		j = 2
	case classMLAT:
		j = 3
	case classModeS:
		j = 4
	default:
		return
	}
	for m := mask; m != 0; m &= m - 1 {
		l.gauge[bits.TrailingZeros16(m)][j] += d
	}
}

// creditPos adds an accepted position (the same rules as the tracker's
// creditPosition, per station).
func (l *lane) creditPos(ls *Lanes, e *laneAC, bit uint16, cs *antStats, r *Result, rssi float64, sec int64, chOnly bool) {
	all := l.all
	if chOnly {
		all = nil
	} else {
		l.totPos++
	}
	if r.Shown && e.min == l.minStart {
		for m := bit &^ e.posMask; m != 0; m &= m - 1 {
			l.gauge[bits.TrailingZeros16(m)][1]++
		}
		e.posMask |= bit
	}
	if r.FlightID != 0 && e.flightID == r.FlightID && !chOnly {
		e.fPos++
	}
	mlat := r.PosSrc == store.SrcMLAT
	for _, s := range [2]*antStats{all, cs} {
		if s == nil {
			continue
		}
		s.positions++
		s.totPositions++
		if mlat {
			s.mlatPositions++
		}
	}
	l.lastIncSec = sec
	if r.PosSrc != store.SrcADSB && r.PosSrc != store.SrcOther {
		// MLAT results and TIS-B / ADS-R rebroadcasts say nothing about what
		// this station can receive from the aircraft.
		return
	}
	if l.geo == nil {
		return // not located: counters only
	}
	ch := int8(-1)
	if cs != nil {
		ch = int8(bits.TrailingZeros16(bit >> 1))
	}
	if len(l.pos) >= lanePosMax {
		l.fold(ls)
	}
	l.pos = append(l.pos, lanePos{
		lat: r.Lat, lon: r.Lon, alt: r.AltFt, rssi: rssi, sec: sec,
		flight: r.FlightID, key: r.Key, cell: r.Cell, ch: ch, chOnly: chOnly,
	})
}

// fold adds the buffered positions to the geometry products: range,
// coverage, heat, signal and elevation of the station and its channel, the
// hour's aircraft set and the flights' distance extremes. It must run before
// the minute or hour of the buffered positions is closed and before their
// aircraft entries are removed. Caller holds the shard lock.
func (l *lane) fold(ls *Lanes) {
	if len(l.pos) == 0 {
		return
	}
	g := l.geo
	for i := range l.pos {
		if g == nil {
			break // no longer located: drop them
		}
		p := &l.pos[i]
		km, brg := g.DistanceBearing(p.lat, p.lon)
		if !(km <= ls.o.MaxRangeKm) {
			continue
		}
		var cs *antStats
		bit := uint16(1)
		if p.chOnly {
			bit = 0
		}
		if p.ch >= 0 {
			cs = l.ch[p.ch]
			bit |= 2 << uint(p.ch)
		}
		newHour := bit
		if e := l.ac.get(p.key); e != nil {
			if e.hour != l.hourStart {
				e.hour, e.hourMask = l.hourStart, 0
			}
			newHour = ^e.hourMask & bit
			e.hourMask |= bit
			if p.flight != 0 && e.flightID == p.flight && !p.chOnly {
				if !(e.fMaxKm >= km) {
					e.fMaxKm = km
				}
				if !(e.fMinKm <= km) {
					e.fMinKm = km
				}
			}
		}
		elev, slant := math.NaN(), math.NaN()
		if altM := p.alt * geo.MetresPerFoot; altM == altM {
			slant = geo.SlantRangeKm(km, g.AntennaAMSLM, altM)
			elev = geo.ElevationDeg(km, g.AntennaAMSLM, altM, g.K)
		}
		if !p.chOnly {
			if newHour&1 != 0 {
				l.all.hourAircraft++
			}
			l.all.addHeat(p.cell, p.rssi, p.alt, p.sec)
			l.all.addRange(km, brg, elev, slant, p.alt, p.rssi)
		}
		if cs != nil {
			if newHour&^1 != 0 {
				cs.hourAircraft++
			}
			cs.addHeat(p.cell, p.rssi, p.alt, p.sec)
			cs.addRange(km, brg, elev, slant, p.alt, p.rssi)
		}
	}
	l.pos = l.pos[:0]
}

// emitFlight queues the flight_station aggregate of e and clears it.
func (l *lane) emitFlight(e *laneAC) {
	if e.flightID == 0 {
		return
	}
	avg := math.NaN()
	if e.fRSSIN > 0 {
		avg = e.fRSSISum / float64(e.fRSSIN)
	}
	l.pendFS = append(l.pendFS, FlightStation{
		FlightID: e.flightID, Station: l.idx,
		First: e.fFirst, Last: e.fLast, Msgs: e.fMsgs, Positions: e.fPos,
		MaxDistKm: e.fMaxKm, MinDistKm: e.fMinKm,
		RSSIMin: e.fRSSIMin, RSSIMax: e.fRSSIMax, RSSIAvg: avg,
	})
	e.flightID = 0
}

// emitFlightKey is emitFlight with the aircraft key filled in.
func (l *lane) emitFlightKey(key uint32, e *laneAC) {
	n := len(l.pendFS)
	l.emitFlight(e)
	if len(l.pendFS) > n {
		l.pendFS[n].ICAO = key
	}
}

// roll closes the current minute (and hour / day when they ended) and starts
// the one containing tMs.
func (l *lane) roll(ls *Lanes, tMs int64) {
	l.fold(ls)
	end := (l.minStart + 60) * 1000
	if l.lastMs < end-laneSilentMs {
		end = max(l.covMs, l.lastMs+1000) // silent: coverage ended with the last copy
	}
	l.minuteRows(end)
	sec := tMs / 1000
	if h := sec - mod(sec, 3600); h != l.hourStart {
		l.hourRows()
		for _, s := range l.stats() {
			if s != nil {
				s.resetHour()
			}
		}
		l.hourStart = h
	}
	if d := store.DayOf(sec); d != l.day {
		// The day tables of the finished day go out with its own day index.
		if l.pb == nil {
			l.pb = store.NewBatch()
		}
		for _, s := range l.stats() {
			if s != nil {
				s.dayIncs(l.pb, l.day, &ls.shards[l.idx%laneShards].scr)
			}
		}
		l.day = d
	}
	l.minStart = sec - mod(sec, 60)
	l.gauge = [1 + laneChannels][5]int32{}
	l.covMs = l.minStart * 1000
	if l.lastMs < l.covMs-laneSilentMs {
		l.covMs = tMs // the station was silent: count from its return
	}
}

// stats returns the lane's statistics slots: the station first, then the
// channels in use, then nil entries.
func (l *lane) stats() [1 + laneChannels]*antStats {
	var out [1 + laneChannels]*antStats
	out[0] = l.all
	n := 1
	for _, s := range l.ch {
		if s != nil {
			out[n] = s
			n++
		}
	}
	return out
}

// minuteRows appends the stats_minute rows of the accumulated part of the
// current minute (counters since the last row, gauges for the whole minute
// so far) and clears the counters. endMs is the data time the rows cover up
// to.
func (l *lane) minuteRows(endMs int64) {
	if l.minStart == 0 {
		return
	}
	secs := 0.0
	if from := max(l.covMs, l.minStart*1000); endMs > from {
		secs = min(float64(endMs-from)/1000, 60)
	}
	emit := func(s *antStats, c *[5]int32) {
		s.gAircraft, s.gPos, s.gADSB, s.gMLAT, s.gModeS = int(c[0]), int(c[1]), int(c[2]), int(c[3]), int(c[4])
		if r, ok := s.minuteRow(l.minStart); ok {
			r.Secs = secs
			l.pendMin = append(l.pendMin, r)
		}
		s.resetMinute()
	}
	emit(l.all, &l.gauge[0])
	multi := l.multiChannel()
	for i, s := range l.ch {
		switch {
		case s == nil:
		case multi:
			emit(s, &l.gauge[i+1])
		default:
			s.resetMinute()
		}
	}
	if endMs > l.covMs {
		l.covMs = endMs
	}
}

// hourRows appends the range_hour increments of all slots.
func (l *lane) hourRows() {
	multi := l.multiChannel()
	for i, s := range l.stats() {
		if s == nil {
			break
		}
		if r, ok := s.hourRow(l.hourStart); ok && (i == 0 || multi) {
			l.pendHour = append(l.pendHour, r)
		}
	}
}

// lateCredit counts a copy older than the current minute in that minute's
// late accumulator.
func (l *lane) lateCredit(ls *Lanes, tMs int64, status uint8, dfIdx int, key uint32, sig float64, msgs uint32, pos bool) {
	sec := tMs / 1000
	m := sec - mod(sec, 60)
	lm := l.late[m]
	if lm == nil {
		if len(l.late) >= laneLateMax {
			ls.lateDropped.Add(1)
			return
		}
		if l.late == nil {
			l.late = make(map[int64]*lateMinute)
		}
		lm = &lateMinute{rssiMin: math.NaN(), rssiMax: math.NaN()}
		l.late[m] = lm
	}
	l.totLate++
	n := int64(max(msgs, 1))
	lm.frames += n
	switch {
	case msgs > 0:
		lm.df[modes.DFIndexOther] += n
	case status == ResultOK:
		lm.df[dfIdx]++
	case status == ResultUnsupported:
		lm.df[modes.DFIndexOther]++
	default:
		lm.crcBad++
	}
	if status == ResultOK && key != 0 {
		if lm.ac == nil {
			lm.ac = make(map[uint32]struct{})
		}
		lm.ac[key] = struct{}{}
		if pos {
			lm.positions++
		}
	}
	if sig == sig && !math.IsInf(sig, 0) && (status == ResultOK || msgs > 0) {
		lm.rssiSum += sig
		lm.rssiN++
		if !(lm.rssiMin <= sig) {
			lm.rssiMin = sig
		}
		if !(lm.rssiMax >= sig) {
			lm.rssiMax = sig
		}
	}
}

// lateRows appends and clears the late minute rows.
func (l *lane) lateRows() {
	for m, lm := range l.late {
		r := store.StatsRow{
			T: m, Ant: l.all.id, Frames: lm.frames,
			DF0: lm.df[modes.DFIndex0], DF4: lm.df[modes.DFIndex4], DF5: lm.df[modes.DFIndex5],
			DF11: lm.df[modes.DFIndex11], DF16: lm.df[modes.DFIndex16], DF17: lm.df[modes.DFIndex17],
			DF18: lm.df[modes.DFIndex18], DF20: lm.df[modes.DFIndex20], DF21: lm.df[modes.DFIndex21],
			DFOther: lm.df[modes.DFIndexOther], CRCBad: lm.crcBad, Positions: lm.positions,
			Aircraft: len(lm.ac), MaxKm: math.NaN(), RSSIAvg: math.NaN(), RSSIMin: lm.rssiMin, RSSIMax: lm.rssiMax,
		}
		if lm.rssiN > 0 {
			r.RSSIAvg = lm.rssiSum / float64(lm.rssiN)
		}
		l.pendMin = append(l.pendMin, r)
		delete(l.late, m)
	}
}

// flush writes the lane's rows into the tick collectors. slow also writes
// range_hour and the day tables and sweeps stale aircraft entries; final
// writes everything (eviction, shutdown).
func (l *lane) flush(ls *Lanes, now int64, slow, final bool, b *store.Batch) {
	l.fold(ls)
	if l.minStart != 0 {
		if now >= (l.minStart+60)*1000 {
			l.roll(ls, now)
		}
		end := now
		if now-l.lastMs > laneSilentMs {
			end = min(now, l.lastMs+1000) // offline: coverage ends with the last copy
		}
		l.minuteRows(end)
	}
	if slow || final {
		l.hourRows()
		multi := l.multiChannel()
		for i, s := range l.stats() {
			if s != nil {
				// A single-channel station's channel slot is drained
				// without rows (nil batch).
				db := b
				if i > 0 && !multi {
					db = nil
				}
				s.dayIncs(db, l.day, &ls.shards[l.idx%laneShards].scr)
			}
		}
		dels := l.dels[:0]
		for i, k := range l.ac.keys {
			if e := &l.ac.vals[i]; k != 0 && (final || now-e.lastMs > laneStaleMs) {
				l.emitFlightKey(k-1, e)
				dels = append(dels, k-1)
			}
		}
		for _, k := range dels {
			l.ac.del(k)
		}
		l.dels = dels[:0]
		if l.ac.len() == 0 && len(l.ac.keys) > 4*acTableMin {
			l.ac = acTable{} // release the storage of a busy period
		}
		l.nextSlowMs = now + ls.o.SlowFlushEvery.Milliseconds()
	}
	l.lateRows()
	if l.pb != nil {
		if sub := ls.o.Store; sub != nil && l.pb.Len() > 0 {
			sub.Submit(l.pb)
		}
		l.pb = nil
	}
	ls.tMin = append(ls.tMin, l.pendMin...)
	ls.tHour = append(ls.tHour, l.pendHour...)
	ls.tFS = append(ls.tFS, l.pendFS...)
	clear(l.pendMin)
	clear(l.pendHour)
	clear(l.pendFS)
	l.pendMin, l.pendHour, l.pendFS = l.pendMin[:0], l.pendHour[:0], l.pendFS[:0]
	l.nextFlushMs = now + ls.o.FlushEvery.Milliseconds()
}

// closeFlights emits the aggregates of flights the tracker closed.
func (l *lane) closeFlights(ls *Lanes, closed []closedFlight) {
	if l.ac.len() == 0 {
		return
	}
	l.fold(ls) // buffered distances belong to these flights
	for _, c := range closed {
		if e := l.ac.get(c.key); e != nil && e.flightID == c.id {
			l.emitFlightKey(c.key, e)
		}
	}
}

// closeFlight queues a flight the tracker closed; its per-station
// aggregates are emitted at the next Tick.
func (ls *Lanes) closeFlight(key uint32, id int64) {
	if id == 0 {
		return
	}
	ls.closedMu.Lock()
	if len(ls.closed) < laneClosedMax {
		ls.closed = append(ls.closed, closedFlight{key: key, id: id})
	}
	ls.closedMu.Unlock()
}

// Tick runs the lanes' housekeeping at data time nowMs (Unix ms): lanes
// whose flush is due write their rows, station positions are refreshed,
// flights closed by the tracker are emitted, and lanes silent for
// EvictAfter are flushed and dropped. The tracker calls it about once per
// second of data time; it returns at once if another Tick is running.
func (ls *Lanes) Tick(nowMs int64) {
	if !ls.tickMu.TryLock() {
		return
	}
	defer ls.tickMu.Unlock()
	ls.observe(nowMs)
	ls.run(nowMs, false)
}

// FlushAll writes every lane's open accumulators (partial minute, hour and
// day rows, open flight aggregates are kept). Use it on shutdown; it waits
// for a running Tick.
func (ls *Lanes) FlushAll(nowMs int64) {
	ls.tickMu.Lock()
	defer ls.tickMu.Unlock()
	if nowMs <= 0 {
		nowMs = ls.nowMs.Load()
	}
	if nowMs <= 0 {
		return
	}
	ls.run(nowMs, true)
}

// run is Tick / FlushAll. Caller holds tickMu.
func (ls *Lanes) run(now int64, all bool) {
	ls.closedMu.Lock()
	closed := ls.closedX[:0]
	closed = append(closed, ls.closed...)
	ls.closed = ls.closed[:0]
	ls.closedMu.Unlock()
	ls.closedX = closed

	var b *store.Batch
	if ls.o.Store != nil {
		b = store.NewBatch()
	}
	evict := ls.o.EvictAfter.Milliseconds()
	geoEvery := ls.o.GeoRefresh.Milliseconds()
	for i := range ls.shards {
		sh := &ls.shards[i]
		sh.mu.Lock()
		for j, l := range sh.lanes {
			if l == nil {
				continue
			}
			if len(closed) > 0 {
				l.closeFlights(ls, closed)
			}
			switch {
			case now-l.lastMs > evict:
				l.flush(ls, now, true, true, b)
				sh.lanes[j] = nil
				continue
			case all:
				l.flush(ls, now, true, false, b)
			case now >= l.nextFlushMs || l.nextFlushMs-now > 2*ls.o.FlushEvery.Milliseconds():
				l.flush(ls, now, now >= l.nextSlowMs, false, b)
			}
			if now-l.geoAtMs >= geoEvery || now < l.geoAtMs {
				l.refreshGeo(ls, now)
			}
			if len(l.pendFS) > 0 { // closed flights go out with this tick
				ls.tFS = append(ls.tFS, l.pendFS...)
				clear(l.pendFS)
				l.pendFS = l.pendFS[:0]
			}
		}
		sh.mu.Unlock()
	}
	if ls.tapRows != nil && (len(ls.tMin) > 0 || len(ls.tHour) > 0) {
		ls.tapRows(ls.tMin, ls.tHour)
	}
	if b != nil {
		if len(ls.tMin) > 0 {
			b.AddMinuteStats(ls.tMin)
		}
		if len(ls.tHour) > 0 {
			b.AddRangeHours(ls.tHour)
		}
		if b.Len() > 0 {
			ls.o.Store.Submit(b)
		}
	}
	clear(ls.tMin)
	clear(ls.tHour)
	ls.tMin, ls.tHour = ls.tMin[:0], ls.tHour[:0]
	if len(ls.tFS) > 0 {
		if fn := ls.o.OnFlightStation; fn != nil {
			if res := ls.o.Resolver; res != nil {
				for i := range ls.tFS {
					if m, ok := res.Info(ls.tFS[i].Station); ok {
						ls.tFS[i].SID = m.SID
					}
				}
			}
			fn(ls.tFS)
		}
		clear(ls.tFS)
		ls.tFS = ls.tFS[:0]
	}
}

// Backfill credits a late (backfilled) frame to its station's lane at its
// original time (RawFrame.HostNs, else Recv): counters, the minute's
// aircraft set and RSSI. It never touches the live picture. Frames whose
// address is only parity-protected are accepted only for aircraft the
// station heard recently. Positions are not decoded from late frames.
func (ls *Lanes) Backfill(f model.RawFrame) {
	tMs := f.Recv.UnixMilli()
	if f.HostNs > 0 {
		tMs = f.HostNs / 1e6
	}
	if f.Recv.IsZero() && f.HostNs <= 0 {
		return
	}
	n := len(f.Data)
	if (f.Kind != model.FrameModeSShort && f.Kind != model.FrameModeSLong) || (n != 7 && n != 14) {
		return
	}
	sh := &ls.shards[f.Station%laneShards]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	l := ls.laneLocked(sh, f.Station, tMs)
	m := &sh.dec
	status, key := ResultOK, uint32(0)
	err := modes.DecodeOpts(f.Data, m, modes.Options{})
	dfIdx := modes.DFIndex(m.DF)
	switch {
	case err == modes.ErrUnsupportedDF:
		status = ResultUnsupported
	case err != nil:
		status = ResultBadCRC
	default:
		key = m.AddrKey()
		if m.CRC != modes.CRCOK && l.ac.get(key) == nil {
			status, key = ResultRejected, 0
		}
	}
	if l.minStart != 0 && tMs >= l.minStart*1000 {
		r := Result{TMs: tMs, Key: key, Status: status, DF: m.DF, DFIdx: uint8(dfIdx)}
		l.credit(ls, f.Channel, f.Signal, &r, false)
		return
	}
	l.lateCredit(ls, tMs, status, dfIdx, key, f.Signal, 0, false)
}

// BackfillUpdate credits a late pre-decoded update (counters only).
func (ls *Lanes) BackfillUpdate(u model.DecodedUpdate) {
	if u.Recv.IsZero() {
		return
	}
	tMs := u.Recv.UnixMilli()
	n := uint32(max(u.Msgs, 1))
	sig := math.NaN()
	if u.Signal != nil {
		sig = *u.Signal
	}
	key := u.ICAO & (0xFFFFFF | modes.NonICAOFlag)
	sh := &ls.shards[u.Station%laneShards]
	sh.mu.Lock()
	l := ls.laneLocked(sh, u.Station, tMs)
	l.lateCredit(ls, tMs, ResultOK, modes.DFIndexOther, key, sig, n, false)
	sh.mu.Unlock()
}

// Info returns the summary of station st's lane.
func (ls *Lanes) Info(st model.StationIdx) (LaneInfo, bool) {
	sh := &ls.shards[st%laneShards]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	i := int(st / laneShards)
	if i >= len(sh.lanes) || sh.lanes[i] == nil {
		return LaneInfo{}, false
	}
	return sh.lanes[i].info(), true
}

// Drop discards the lane of a station that is being deleted: its pending
// rows are not flushed, and no later copy creates it again (station
// indices are never reused). It waits for a running Tick, so when it
// returns every row of st is either already submitted to the store or
// gone (C4).
func (ls *Lanes) Drop(st model.StationIdx) {
	ls.tickMu.Lock()
	defer ls.tickMu.Unlock()
	sh := &ls.shards[st%laneShards]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.gone == nil {
		sh.gone = map[model.StationIdx]bool{}
	}
	sh.gone[st] = true
	if i := int(st / laneShards); i < len(sh.lanes) {
		sh.lanes[i] = nil
	}
}

// List returns the summaries of all lanes (unordered).
func (ls *Lanes) List() []LaneInfo {
	var out []LaneInfo
	for i := range ls.shards {
		sh := &ls.shards[i]
		sh.mu.Lock()
		for _, l := range sh.lanes {
			if l != nil {
				out = append(out, l.info())
			}
		}
		sh.mu.Unlock()
	}
	return out
}

func (l *lane) info() LaneInfo {
	n := 0
	l.ac.each(func(_ uint32, e *laneAC) {
		if e.min == l.minStart && e.minMask&1 != 0 {
			n++
		}
	})
	return LaneInfo{
		Station: l.idx, Located: l.geo != nil,
		Frames: l.totFrames, Positions: l.totPos, LateFrames: l.totLate,
		Aircraft: n, FirstMs: l.firstMs, LastMs: l.lastMs,
	}
}

// HeardICAOs appends the address keys station st heard since sinceMs (data
// time, at most laneStaleMs back) to dst: the "aircraft only you heard" /
// "my map" view of an owner.
func (ls *Lanes) HeardICAOs(st model.StationIdx, sinceMs int64, dst []uint32) []uint32 {
	sh := &ls.shards[st%laneShards]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	i := int(st / laneShards)
	if i >= len(sh.lanes) || sh.lanes[i] == nil {
		return dst
	}
	sh.lanes[i].ac.each(func(k uint32, e *laneAC) {
		if e.lastMs >= sinceMs {
			dst = append(dst, k)
		}
	})
	return dst
}

// LateDropped returns how many late minutes were dropped because a lane
// already held laneLateMax of them.
func (ls *Lanes) LateDropped() uint64 { return ls.lateDropped.Load() }
