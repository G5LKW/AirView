package tracker

import (
	"math"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/store"
)

type covAcc struct {
	maxKm   float64
	count   int64
	rssiSum float64
	rssiN   int64
}

type elevAcc struct {
	count    int64
	rssiSum  float64
	residSum float64
}

type heatAcc struct {
	count   int64
	rssiSum float64
	rssiN   int64
	minAlt  float64
	maxAlt  float64
	lastT   int64
}

// antStats holds the statistics accumulators of one antenna slot (slot 0 =
// all antennas combined). Everything here is a delta since the last flush
// unless noted.
type antStats struct {
	id string

	// Lifetime totals (Counters).
	totFrames    uint64
	totPositions uint64
	prevFrames   uint64
	rate         float64
	ew           ewRate

	// Current minute.
	frames        int64
	df            [modes.NumDFIndex]int64
	crcBad        int64
	positions     int64
	mlatPositions int64
	rssiSum       float64
	rssiN         int64
	rssiMin       float64
	rssiMax       float64
	minMaxKm      float64
	// Gauges of the current minute, filled when the minute row is built.
	gAircraft, gPos, gADSB, gMLAT, gModeS int

	// Current hour (range_hour). hist / positions / far / resid are deltas;
	// sectorMax, hourMaxKm and hourAircraft are cumulative for the hour.
	hist          [geo.RangeBins]uint32
	sectorMax     [geo.Sectors]float32
	hourPositions int64
	hourFar       int64
	residSum      float64
	residN        int64
	hourMaxKm     float64
	hourAircraft  int
	hourDirty     bool

	// Day tables.
	cov      [geo.BearingBins * geo.AltBands]covAcc
	covDirty bool
	sig      [geo.DistBins * geo.RSSIBins]uint32
	sigDirty bool
	elev     [geo.ElevBins]elevAcc
	elvDirty bool
	heat     map[uint32]heatAcc
}

func newAntStats(id string) *antStats {
	s := &antStats{id: id, heat: make(map[uint32]heatAcc, 256)}
	s.resetMinute()
	s.hourMaxKm = math.NaN()
	return s
}

func (s *antStats) resetMinute() {
	s.frames, s.crcBad, s.positions, s.mlatPositions = 0, 0, 0, 0
	s.df = [modes.NumDFIndex]int64{}
	s.rssiSum, s.rssiN = 0, 0
	s.rssiMin, s.rssiMax, s.minMaxKm = math.NaN(), math.NaN(), math.NaN()
	s.gAircraft, s.gPos, s.gADSB, s.gMLAT, s.gModeS = 0, 0, 0, 0, 0
}

func (s *antStats) addFrame(res uint8, dfIdx int, rssi float64) {
	s.frames++
	s.totFrames++
	switch res {
	case resOK:
		s.df[dfIdx]++
		s.addRSSI(rssi)
	case resOther:
		s.df[modes.DFIndexOther]++
	default:
		s.crcBad++
	}
}

// addUpdate counts the messages behind a pre-decoded update.
func (s *antStats) addUpdate(n uint64, rssi float64) {
	s.frames += int64(n)
	s.totFrames += n
	s.df[modes.DFIndexOther] += int64(n)
	s.addRSSI(rssi)
}

func (s *antStats) addRSSI(rssi float64) {
	if rssi != rssi { // NaN
		return
	}
	s.rssiSum += rssi
	s.rssiN++
	if !(s.rssiMin <= rssi) {
		s.rssiMin = rssi
	}
	if !(s.rssiMax >= rssi) {
		s.rssiMax = rssi
	}
}

func (s *antStats) addHeat(cell uint32, rssi, alt float64, sec int64) {
	h, ok := s.heat[cell]
	if !ok {
		h.minAlt, h.maxAlt = math.NaN(), math.NaN()
	}
	h.count++
	if finite(rssi) {
		h.rssiSum += rssi
		h.rssiN++
	}
	if alt == alt {
		if !(h.minAlt <= alt) {
			h.minAlt = alt
		}
		if !(h.maxAlt >= alt) {
			h.maxAlt = alt
		}
	}
	h.lastT = sec
	s.heat[cell] = h
}

// addRange adds a located position to the range / coverage / signal /
// elevation accumulators.
func (s *antStats) addRange(km, brg, elev, slant, alt, rssi float64) {
	if !(s.minMaxKm >= km) {
		s.minMaxKm = km
	}
	if !(s.hourMaxKm >= km) {
		s.hourMaxKm = km
	}
	s.hourDirty = true
	s.hourPositions++
	if km > FarKm {
		s.hourFar++
	}
	if b := geo.RangeBin(km); b >= 0 {
		s.hist[b]++
	}
	if b := geo.SectorBin(brg); b >= 0 && float32(km) > s.sectorMax[b] {
		s.sectorMax[b] = float32(km)
	}
	hasRSSI := finite(rssi)
	resid := math.NaN()
	if hasRSSI && slant > 0.05 {
		resid = rssi + 20*math.Log10(slant)
		s.residSum += resid
		s.residN++
	}
	// Positions without a fresh altitude (surface positions, readsb
	// "ground") have no altitude band; the coverage grid is skipped for them.
	if bb, ab := geo.BearingBin(brg), geo.AltBand(alt); bb >= 0 && ab >= 0 {
		c := &s.cov[bb*geo.AltBands+ab]
		if km > c.maxKm {
			c.maxKm = km
		}
		c.count++
		if hasRSSI {
			c.rssiSum += rssi
			c.rssiN++
		}
		s.covDirty = true
	}
	if hasRSSI {
		if db, rb := geo.DistBin(km), geo.RSSIBin(rssi); db >= 0 && rb >= 0 {
			s.sig[db*geo.RSSIBins+rb]++
			s.sigDirty = true
		}
		if resid == resid && elev == elev {
			if eb := geo.ElevBin(elev); eb >= 0 {
				e := &s.elev[eb]
				e.count++
				e.rssiSum += rssi
				e.residSum += resid
				s.elvDirty = true
			}
		}
	}
}

// minuteRow builds the stats_minute row of the accumulated part of the
// current minute.
func (s *antStats) minuteRow(minStart int64) (store.StatsRow, bool) {
	if s.frames == 0 && s.gAircraft == 0 && s.positions == 0 {
		return store.StatsRow{}, false
	}
	r := store.StatsRow{
		T: minStart, Ant: s.id, Frames: s.frames,
		DF0: s.df[modes.DFIndex0], DF4: s.df[modes.DFIndex4], DF5: s.df[modes.DFIndex5],
		DF11: s.df[modes.DFIndex11], DF16: s.df[modes.DFIndex16], DF17: s.df[modes.DFIndex17],
		DF18: s.df[modes.DFIndex18], DF20: s.df[modes.DFIndex20], DF21: s.df[modes.DFIndex21],
		DFOther: s.df[modes.DFIndexOther], CRCBad: s.crcBad,
		Positions: s.positions, MLATPositions: s.mlatPositions,
		Aircraft: s.gAircraft, AircraftPos: s.gPos, ADSB: s.gADSB, MLAT: s.gMLAT, ModeS: s.gModeS,
		MaxKm: s.minMaxKm, RSSIAvg: math.NaN(), RSSIMin: s.rssiMin, RSSIMax: s.rssiMax,
	}
	if s.rssiN > 0 {
		r.RSSIAvg = s.rssiSum / float64(s.rssiN)
	}
	return r, true
}

// hourRow builds the range_hour increment and clears the delta parts.
func (s *antStats) hourRow(hourStart int64) (store.RangeHour, bool) {
	r, ok := s.peekHourRow(hourStart)
	if ok {
		s.hist = [geo.RangeBins]uint32{}
		s.hourPositions, s.hourFar, s.residSum, s.residN = 0, 0, 0, 0
		s.hourDirty = false
	}
	return r, ok
}

// peekHourRow builds the range_hour increment without clearing anything.
func (s *antStats) peekHourRow(hourStart int64) (store.RangeHour, bool) {
	if !s.hourDirty {
		return store.RangeHour{}, false
	}
	nan := math.NaN()
	r := store.RangeHour{
		T: hourStart, Ant: s.id, MaxKm: s.hourMaxKm,
		P99Km: nan, P95Km: nan, P90Km: nan, P50Km: nan,
		Positions: s.hourPositions, Aircraft: s.hourAircraft, FarCount: s.hourFar,
		ResidDB: nan, ResidN: s.residN,
		DistHist:  append([]uint32(nil), s.hist[:]...),
		SectorMax: append([]float32(nil), s.sectorMax[:]...),
	}
	if s.residN > 0 {
		r.ResidDB = s.residSum / float64(s.residN)
	}
	// Percentiles of this increment; the store recomputes them from the
	// merged histogram when the hour already has a row.
	r.P99Km = store.HistPercentile(r.DistHist, geo.RangeBinKm, 0.99)
	r.P95Km = store.HistPercentile(r.DistHist, geo.RangeBinKm, 0.95)
	r.P90Km = store.HistPercentile(r.DistHist, geo.RangeBinKm, 0.90)
	r.P50Km = store.HistPercentile(r.DistHist, geo.RangeBinKm, 0.50)
	return r, true
}

// resetHour starts a new hour.
func (s *antStats) resetHour() {
	s.hist = [geo.RangeBins]uint32{}
	s.sectorMax = [geo.Sectors]float32{}
	s.hourPositions, s.hourFar, s.residSum, s.residN = 0, 0, 0, 0
	s.hourMaxKm = math.NaN()
	s.hourAircraft = 0
	s.hourDirty = false
}

// dayScratch holds reusable row buffers for dayIncs (the store's Merge*
// calls copy their rows). Not safe for concurrent use: one per lock domain.
type dayScratch struct {
	cov  []store.CoverageInc
	sig  []store.SignalHistInc
	elev []store.ElevHistInc
	heat []store.HeatInc
}

// dayIncs appends the coverage / signal / elevation / heat increments to b
// (nil = discard them) and clears the accumulators. scr supplies the row
// buffers (nil = allocate).
func (s *antStats) dayIncs(b *store.Batch, day int, scr *dayScratch) {
	s.incRows(b, day, scr, true, true)
}

// incRows appends the day increments (dayIncs: coverage, signal, elevation)
// and / or the heat increments (heat) to b (nil = discard) and clears those
// accumulators.
func (s *antStats) incRows(b *store.Batch, day int, scr *dayScratch, dayIncs, heat bool) {
	if scr == nil {
		scr = &dayScratch{}
	}
	if dayIncs {
		cov, sig, elev := s.dayRows(day, scr.cov[:0], scr.sig[:0], scr.elev[:0], b != nil)
		if b != nil {
			if len(cov) > 0 {
				b.MergeCoverage(cov)
			}
			if len(sig) > 0 {
				b.MergeSignalHist(sig)
			}
			if len(elev) > 0 {
				b.MergeElevHist(elev)
			}
		}
		scr.cov, scr.sig, scr.elev = cov[:0], sig[:0], elev[:0]
		s.clearDay()
	}
	if heat && len(s.heat) > 0 {
		if b != nil {
			rows := s.heatRows(scr.heat[:0])
			b.MergeHeat(rows)
			scr.heat = rows[:0]
		}
		clear(s.heat)
	}
}

// dayRows appends the pending coverage / signal / elevation increments (all
// of them belong to day) without clearing them. With want = false nothing is
// built (the rows would be discarded).
func (s *antStats) dayRows(day int, cov []store.CoverageInc, sig []store.SignalHistInc, elev []store.ElevHistInc, want bool) ([]store.CoverageInc, []store.SignalHistInc, []store.ElevHistInc) {
	if !want {
		return cov, sig, elev
	}
	if s.covDirty {
		for i := range s.cov {
			c := &s.cov[i]
			if c.count == 0 {
				continue
			}
			cov = append(cov, store.CoverageInc{
				Day: day, Ant: s.id, BrgBin: i / geo.AltBands, AltBand: i % geo.AltBands,
				MaxKm: c.maxKm, Count: c.count, RSSISum: c.rssiSum, RSSIN: c.rssiN,
			})
		}
	}
	if s.sigDirty {
		for i, n := range s.sig {
			if n == 0 {
				continue
			}
			sig = append(sig, store.SignalHistInc{
				// The table stores floor(dBFS): geo bin k covers (-(k+1), -k].
				Day: day, Ant: s.id, DistBin: i / geo.RSSIBins, RSSIBin: geo.RSSIBinStored(i % geo.RSSIBins), Count: int64(n),
			})
		}
	}
	if s.elvDirty {
		for i := range s.elev {
			e := &s.elev[i]
			if e.count == 0 {
				continue
			}
			elev = append(elev, store.ElevHistInc{
				Day: day, Ant: s.id, ElevBin: i, Count: e.count, RSSISum: e.rssiSum, ResidSum: e.residSum,
			})
		}
	}
	return cov, sig, elev
}

// clearDay clears the coverage / signal / elevation accumulators.
func (s *antStats) clearDay() {
	if s.covDirty {
		s.cov = [geo.BearingBins * geo.AltBands]covAcc{}
		s.covDirty = false
	}
	if s.sigDirty {
		s.sig = [geo.DistBins * geo.RSSIBins]uint32{}
		s.sigDirty = false
	}
	if s.elvDirty {
		s.elev = [geo.ElevBins]elevAcc{}
		s.elvDirty = false
	}
}

// dayDirty reports whether coverage / signal / elevation increments are
// pending.
func (s *antStats) dayDirty() bool { return s.covDirty || s.sigDirty || s.elvDirty }

// heatRows appends the pending heat increments without clearing them.
func (s *antStats) heatRows(rows []store.HeatInc) []store.HeatInc {
	for cell, h := range s.heat {
		rows = append(rows, store.HeatInc{
			Cell: int64(cell), Ant: s.id, Count: h.count, RSSISum: h.rssiSum, RSSIN: h.rssiN,
			MinAlt: h.minAlt, MaxAlt: h.maxAlt, LastT: h.lastT,
		})
	}
	return rows
}

// classify adds an aircraft's minute gauges to the slots that heard it and
// clears its minute masks.
func (t *Tracker) classify(ac *aircraft, now int64) {
	if ac.minMask == 0 {
		return
	}
	if ac.shown() {
		// What it was while it was heard during this minute.
		src := ac.sourceWithin(now, 120_000, 90_000)
		for i, s := range t.slots {
			bit := uint32(1) << uint(i)
			if ac.minMask&bit == 0 {
				continue
			}
			s.gAircraft++
			if ac.minPosMask&bit != 0 {
				s.gPos++
			}
			switch src {
			case model.SrcADSB:
				s.gADSB++
			case model.SrcMLAT:
				s.gMLAT++
			case model.SrcModeS:
				s.gModeS++
			}
		}
	}
	ac.minMask, ac.minPosMask = 0, 0
}

// minuteRows appends the minute rows of all slots to b and clears the minute
// accumulators. endMs is the data time the accumulated part of the minute
// ends at (the minute's end on a roll, now on a final flush); the rows
// carry the covered seconds from t.minCovMs to it, so a partial first or
// last minute is not read as a full minute at a lower rate.
func (t *Tracker) minuteRows(b *store.Batch, endMs int64) {
	now := t.nowMs
	for _, ac := range t.ac {
		t.classify(ac, now)
	}
	secs := 0.0 // unknown (clock stepped back): a full minute
	if from := max(t.minCovMs, t.minStart*1000); endMs > from {
		secs = min(float64(endMs-from)/1000, 60)
	}
	var rows []store.StatsRow
	var rssiN []int64
	for _, s := range t.slots {
		if r, ok := s.minuteRow(t.minStart); ok {
			r.Secs = secs
			rows = append(rows, r)
			rssiN = append(rssiN, s.rssiN)
		}
		s.resetMinute()
	}
	t.minCovMs = endMs
	if p := t.part; p != nil {
		// A shard's minute is one part of the network's minute: the
		// shards' rows are summed before they are written (shard.go).
		p.set.mm.add(p.idx, t.minStart, rows, rssiN)
	} else if b != nil && len(rows) > 0 {
		b.AddMinuteStats(rows)
	}
	// Per-feed latency history, once per minute.
	for _, fs := range t.feedList {
		fs.lat.closeMinute(t.minStart)
	}
}

// hourRows appends the range_hour increments of all slots to b.
func (t *Tracker) hourRows(b *store.Batch) {
	var rows []store.RangeHour
	for _, s := range t.slots {
		if r, ok := s.hourRow(t.hourStart); ok {
			rows = append(rows, r)
		}
	}
	if b != nil && len(rows) > 0 {
		b.AddRangeHours(rows)
	}
}

// rollMinute closes the current minute (and hour, when it ended) and starts
// the one containing sec. Besides the minute (and closed hour) rows it
// writes only the streams that are due.
func (t *Tracker) rollMinute(sec int64) {
	b := t.batch()
	t.minuteRows(b, min((t.minStart+60)*1000, t.nowMs))
	newHour := sec - mod(sec, 3600)
	if newHour != t.hourStart {
		t.hourRows(b)
		for _, s := range t.slots {
			s.resetHour()
		}
		for _, ac := range t.ac {
			ac.hourMask = 0
		}
		t.hourStart = newHour
		t.lastHourPartialMs = t.nowMs
	}
	t.minStart = sec - mod(sec, 60)
	t.minCovMs = t.minStart * 1000 // counting went on through the boundary
	// The row check runs at every minute roll and every rowCheckMs after it,
	// as 1.0.0's flush did: the flights' 60-s phases (record samples) and
	// the records / events see the same moments.
	t.lastRowMs = t.nowMs - rowCheckMs
	t.flushStreams(b, false)
	t.submit(b)
}

// flush writes the streams that are due. final writes everything: the
// partial minute and hour rows, every pending increment and trail point and
// every dirty flight / aircraft row. Caller holds mu.
func (t *Tracker) flush(final bool) {
	if t.nowMs == 0 {
		return
	}
	b := t.batch()
	if final {
		t.minuteRows(b, t.nowMs)
	}
	t.flushStreams(b, final)
	t.submit(b)
}

// setStreamClocks restarts every stream clock at ms.
func (t *Tracker) setStreamClocks(ms int64) {
	t.lastRowMs, t.lastTrailMs, t.lastIncMs, t.lastHeatMs = ms, ms, ms, ms
	t.lastRefreshSec, t.lastAcRefreshSec = ms/1000, ms/1000
}

// streamsDue reports whether any stream clock has run out at now.
func (t *Tracker) streamsDue(now int64) bool {
	return now-t.lastRowMs >= rowCheckMs || now-t.lastTrailMs >= t.cad.trailMs ||
		now-t.lastIncMs >= t.cad.incMs || now-t.lastHeatMs >= t.cad.heatMs ||
		now-t.lastHourPartialMs >= t.cad.rangeMs
}

// flushStreams appends the due streams (all of them when final) to b (nil =
// discard: no store) and restarts their clocks.
func (t *Tracker) flushStreams(b *store.Batch, final bool) {
	now := t.nowMs
	if final || now-t.lastHourPartialMs >= t.cad.rangeMs {
		t.hourRows(b)
		t.lastHourPartialMs = now
	}
	incDue := final || now-t.lastIncMs >= t.cad.incMs
	heatDue := final || now-t.lastHeatMs >= t.cad.heatMs
	if incDue || heatDue {
		day := t.incDayNow()
		for _, s := range t.slots {
			s.incRows(b, day, &t.dayScr, incDue, heatDue)
		}
		if incDue {
			t.lastIncMs = now
		}
		if heatDue {
			t.lastHeatMs = now
		}
	}
	if final || now-t.lastRowMs >= rowCheckMs {
		t.flushRows(b, final)
		t.lastRowMs = now
	}
	if final || now-t.lastTrailMs >= t.cad.trailMs {
		if b != nil && len(t.pendPos) > 0 {
			b.AddPositions(t.pendPos)
		}
		t.pendPos = t.pendPos[:0]
		t.lastTrailMs = now
	}
}

// incDayNow is the UTC day of the accumulated day increments.
func (t *Tracker) incDayNow() int {
	if t.incDayOK {
		return t.incDay
	}
	return store.DayOf(t.lastIncSec)
}

// noteIncDay is called before a position is credited at data second sec:
// when sec belongs to another UTC day than the accumulated day increments,
// those are written first (with their own day), so increments are never
// attributed to the wrong day however long the flush interval is.
func (t *Tracker) noteIncDay(sec int64) {
	d := store.DayOf(sec)
	if t.incDayOK && d <= t.incDay {
		return // a late point of the previous day counts for the newest day, as in 1.0.0
	}
	if t.incDayOK {
		dirty := false
		for _, s := range t.slots {
			dirty = dirty || s.dayDirty()
		}
		if dirty {
			b := t.batch()
			for _, s := range t.slots {
				s.incRows(b, t.incDay, &t.dayScr, true, false)
			}
			t.submit(b)
		}
	}
	t.incDay, t.incDayOK = d, true
}

func (t *Tracker) batch() *store.Batch {
	if t.st == nil {
		return nil
	}
	return store.NewBatch()
}

func (t *Tracker) submit(b *store.Batch) {
	if b != nil && b.Len() > 0 {
		t.st.Submit(b)
	}
}

// flushRows appends flights, aircraft sightings, events and records to b
// (nil = discard: no store).
func (t *Tracker) flushRows(b *store.Batch, final bool) {
	sec := t.nowMs / 1000
	// The active rows are refreshed together on one tracker-wide tick, not
	// each on its own 60-s phase: a table page holds the rows of ~40
	// airframes, and staggered refreshes rewrote it in up to four commits a
	// minute (WAL frames x4 on the aircraft table). A row never written
	// (a new flight or airframe) is still written at the next row check.
	refresh := final || sec-t.lastRefreshSec >= rowSaveEveryS
	if refresh {
		t.lastRefreshSec = sec
	}
	// The aircraft rows are refreshed on their own, slower tick (see
	// Options.AircraftRefresh; readers force it with FlushAircraftRows).
	acRefresh := final || sec-t.lastAcRefreshSec >= t.cad.acSec
	if acRefresh {
		t.lastAcRefreshSec = sec
	}
	for _, ac := range t.ac {
		if ac.flightOpen && (ac.flightDirty || ac.flightRecSec < ac.flightSavedSec) &&
			(final || ac.flightRecSec == 0 || sec-ac.flightRecSec >= rowSaveEveryS) {
			t.flightRecord(ac, sec)
		}
		if ac.flightOpen && ac.flightDirty && (refresh || ac.flightSavedSec == 0) {
			t.saveFlight(ac, sec)
		}
		if ac.seenDirty && ac.shown() && (acRefresh || ac.seenSavedSec == 0) {
			t.saveSeen(ac, sec)
		}
	}
	t.flushRecords(b, sec)
	if b == nil {
		t.pendFlights, t.pendSeen, t.pendEvents = t.pendFlights[:0], t.pendSeen[:0], t.pendEvents[:0]
		t.heldSeen = t.heldSeen[:0]
		return
	}
	if acRefresh && len(t.heldSeen) > 0 {
		t.pendSeen = append(t.pendSeen, t.heldSeen...)
		t.heldSeen = t.heldSeen[:0]
	}
	if len(t.pendFlights) > 0 {
		b.UpsertFlights(t.pendFlights)
		t.pendFlights = t.pendFlights[:0]
	}
	if len(t.pendSeen) > 0 {
		b.UpsertAircraft(t.pendSeen)
		t.pendSeen = t.pendSeen[:0]
	}
	if len(t.pendEvents) > 0 {
		b.AddEvents(t.pendEvents)
		t.pendEvents = t.pendEvents[:0]
	}
}

// PendingIncs returns copies of the coverage / signal / elevation / heat
// increments and the partial range_hour rows accumulated since their last
// flush (nil slices when none), so that readers covering "now" can merge
// them into the stored rows. Safe for concurrent use; O(dirty bins).
func (t *Tracker) PendingIncs() store.PendingIncs {
	if s := t.sh.Load(); s != nil {
		var out store.PendingIncs
		for _, p := range s.parts {
			out.Append(p.PendingIncs())
		}
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pendingIncsLocked()
}

func (t *Tracker) pendingIncsLocked() store.PendingIncs {
	var out store.PendingIncs
	if t.nowMs == 0 {
		return out
	}
	day := t.incDayNow()
	for _, s := range t.slots {
		out.Coverage, out.Signal, out.Elev = s.dayRows(day, out.Coverage, out.Signal, out.Elev, true)
		out.Heat = s.heatRows(out.Heat)
		if r, ok := s.peekHourRow(t.hourStart); ok {
			out.Range = append(out.Range, r)
		}
	}
	return out
}
