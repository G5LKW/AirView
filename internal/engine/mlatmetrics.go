package engine

// MLAT metrics persistence (DESIGN §6.6, §9.6; WP-E2).
//
// The MLAT engine keeps live counters and sliding windows only. In
// aggregator mode the engine records, once a minute,
//
//   - mlat_minute (airview.db): one row per station that took part in MLAT
//     during the minute, plus the network row (sid "");
//   - station_clock_minute (stations.db): the clock state of every station
//     the clock filter knows;
//   - mlat_quality_hour (airview.db): the hold-out error histograms per hour,
//     1° cell and altitude band.
//
// The tiered retention of internal/store (aggregator.storage; minute tier:
// mlat_minute, station_clock_minute; hour tier: mlat_quality_hour) applies
// to all three in the hourly netStoreLoop pass; a deleted station's rows go
// with registry.Options.DeleteExtra.
//
// Sources and definitions (all per station st and minute m, see the package
// "MLAT metrics" for the derivations and their error bounds):
//
//   - Frames: timed copies of st the ingest handed to the MLAT engine
//     (closed and late groups), counted exactly by mlatTap.
//   - Groups: groups st took part in (its first timed copy per group).
//   - SyncFrames: the clock filter's accepted sync observations of st,
//     Accepted30s sampled at the two 30-s marks of the minute.
//   - Fixes / Outliers: solver fixes st contributed a used row to / its rows
//     excluded by RAIM or with |w| > 4, exact from the MLAT engine's
//     cumulative counters (mlatStationTotals); the quality table's one-hour
//     ring is the fallback for a source without them (unit-test fakes).
//   - Peers, SigmaNs, DriftPPM, BetaShortNs: the clock state at the end of
//     the minute; ResidRMSNs: the co-observation-weighted RMS of the
//     station's sync residuals against its peers; SyncedSecs: 10 s per
//     sample in which the station was synced (6 samples a minute).
//   - EpochChanges: new "epoch" events of the station's clock record.
//
// Network row: Frames / Groups summed from mlatTap; SyncFrames summed over
// the stations; Fixes = map updates published (Fixes() stream, hold-out
// excluded); Outliers = solutions rejected (Stats().Rejected delta); Peers =
// synced stations; SigmaNs / ResidRMSNs = medians over the synced stations.

import (
	"context"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/ref"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/store"
)

// mlatTapMaxStation bounds the per-station counter table (indices above it
// are counted in the network totals only).
const mlatTapMaxStation = 1 << 20

// mlatTap sits between the ingest and the MLAT engine (ingest.MLATSink and
// LateObserver) and counts each station's timed copies and groups. Observe
// is called from the ingest sweeper only; the counters are read by the
// metrics loop. Per copy: one atomic add, no allocation.
type mlatTap struct {
	next ingest.MLATSink
	late ingest.LateObserver // nil when next has no late path

	tab    atomic.Pointer[[]*tapCtr]
	growMu sync.Mutex

	groups atomic.Uint64 // network: groups delivered (closed + late)
	frames atomic.Uint64 // network: timed copies delivered
}

type tapCtr struct{ frames, groups atomic.Uint64 }

func newMLATTap(next ingest.MLATSink) *mlatTap {
	t := &mlatTap{next: next}
	t.late, _ = next.(ingest.LateObserver)
	tab := make([]*tapCtr, 0)
	t.tab.Store(&tab)
	return t
}

// ctr returns the counters of st, growing the table when needed.
func (t *mlatTap) ctr(st model.StationIdx) *tapCtr {
	if st == 0 || st > mlatTapMaxStation {
		return nil
	}
	tab := *t.tab.Load()
	if int(st) < len(tab) {
		if c := tab[st]; c != nil {
			return c
		}
	}
	return t.grow(st)
}

// grow is the slow path of ctr (kept apart so the fast path does not
// allocate: the table header escapes here).
func (t *mlatTap) grow(st model.StationIdx) *tapCtr {
	t.growMu.Lock()
	defer t.growMu.Unlock()
	tab := *t.tab.Load()
	if int(st) >= len(tab) {
		n := max(int(st)+1, 2*len(tab), 64)
		nt := make([]*tapCtr, n)
		copy(nt, tab)
		tab = nt
	}
	if tab[st] == nil {
		tab = slices.Clone(tab)
		tab[st] = &tapCtr{}
	}
	t.tab.Store(&tab)
	return tab[st]
}

func (t *mlatTap) count(g *ingest.Group) {
	t.groups.Add(1)
	for i := range g.Copies {
		c := &g.Copies[i]
		if !c.Timed {
			continue
		}
		t.frames.Add(1)
		k := t.ctr(c.Station)
		if k == nil {
			continue
		}
		k.frames.Add(1)
		if !c.SameStation {
			k.groups.Add(1)
		}
	}
}

// Observe implements ingest.MLATSink.
func (t *mlatTap) Observe(g *ingest.Group) {
	t.count(g)
	t.next.Observe(g)
}

// ObserveLate implements ingest.LateObserver.
func (t *mlatTap) ObserveLate(g *ingest.Group) {
	if t.late == nil {
		return
	}
	t.count(g)
	t.late.ObserveLate(g)
}

// station returns the cumulative counters of st (zero when never seen).
func (t *mlatTap) station(st model.StationIdx) (frames, groups uint64) {
	tab := *t.tab.Load()
	if int(st) < len(tab) {
		if c := tab[st]; c != nil {
			return c.frames.Load(), c.groups.Load()
		}
	}
	return 0, 0
}

var (
	_ ingest.MLATSink     = (*mlatTap)(nil)
	_ ingest.LateObserver = (*mlatTap)(nil)
)

// mlatStationTotals is the optional exact source of the per-station fix and
// outlier counts (cumulative since the engine started). The MLAT engine does
// offer it (mlat.Engine.StationTotals); a source without it falls back:
// the counts are then recovered from the hourly quality ring (hourRing).
type mlatStationTotals interface {
	StationTotals(st model.StationIdx) (fixes, outliers uint64, ok bool)
}

var _ mlatStationTotals = (*mlat.Engine)(nil)

// mlatMetricsSource is the part of the MLAT engine the recorder reads (a
// fake in the unit tests).
type mlatMetricsSource interface {
	Station(st model.StationIdx) (mlat.StationReport, bool)
	Stats() mlat.Stats
	// ClockStations returns the clock filter's stations (latest snapshot).
	ClockStations() []*clock.StationClock
	RefState(key uint32, tNs int64, st *ref.State) bool
}

// engineMetrics adapts *mlat.Engine.
type engineMetrics struct{ e *mlat.Engine }

func (m engineMetrics) Station(st model.StationIdx) (mlat.StationReport, bool) {
	return m.e.Station(st)
}
func (m engineMetrics) Stats() mlat.Stats { return m.e.Stats() }
func (m engineMetrics) ClockStations() []*clock.StationClock {
	return m.e.Filter().Snapshot().Stations()
}
func (m engineMetrics) RefState(key uint32, tNs int64, st *ref.State) bool {
	return m.e.Refs().At(key, tNs, 0, st)
}

// StationTotals forwards the exact counters once the MLAT engine has them.
func (m engineMetrics) totals() mlatStationTotals {
	var x any = m.e
	t, _ := x.(mlatStationTotals)
	return t
}

// hourRing recovers per-minute increments of a counter that is only
// observable as a sum over the last hour of one-minute slots (the quality
// table's ContribPerHour and its outlier share).
//
// With C(t) the cumulative count, the reported sum at time t in minute k is
// S(t) = C(t) − C(end of minute k−60): the slots k−59..k. The recorder
// samples once a minute, δ after the minute starts (δ ≈ 0.2 s), and keeps
// its estimate Ĉ_k of C at the sample of minute k:
//
//	Ĉ_k = S_k + Ĉ_{k−59}        (0 before the engine started)
//
// because C(end of k−60) = C(sample of k−59) − e_{k−59}, where e_j are the
// counts of the first δ of minute j. The increment written is
// d̂_k = Ĉ_k − Ĉ_{k−1} = S_k − S_{k−1} + d̂_{k−59}; against the true
// counts between two samples its error obeys ε_k = ε_{k−59} + e_{k−59} −
// e_{k−60}: once an hour a difference of two δ-long counts is added, a
// zero-mean random walk (with δ = 0.2 s and 50 fixes a minute, steps of
// 0 or ±1). Increments are never negative: Ĉ is kept as computed, and the
// written cumulative only moves up.
type hourRing struct {
	ring [64]struct {
		k int64
		c float64
	}
	emitted float64 // cumulative written so far
}

func (h *hourRing) sample(k int64, hourSum float64) uint64 {
	ref := 0.0
	if r := h.ring[(k-59)&63]; r.k == k-59 {
		ref = r.c
	} else {
		// No sample in minute k−59: the latest older one (counts only
		// grow); none = before the engine started.
		best := int64(math.MinInt64)
		for _, r := range h.ring {
			if r.k != 0 && r.k < k-59 && r.k > best {
				best, ref = r.k, r.c
			}
		}
	}
	c := hourSum + ref
	h.ring[k&63] = struct {
		k int64
		c float64
	}{k, c}
	inc := 0.0
	if c > h.emitted {
		inc = c - h.emitted
		h.emitted = c
	}
	return uint64(math.Round(inc))
}

// stMetrics is the recorder's state of one station.
type stMetrics struct {
	frames, groups uint64 // tap counters at the last minute flush
	fixRing        hourRing
	outRing        hourRing
	fixes, outs    uint64 // exact totals at the last flush (mlatStationTotals)

	// this minute (10-s samples)
	syncedS    int  // seconds synced
	clockSeen  bool // the clock filter knows the station
	syncFrames int
	lastEvT    float64 // newest clock event already counted
	epochs     int64
}

// mlatRecorder writes the MLAT metrics. One goroutine (run).
type mlatRecorder struct {
	src    mlatMetricsSource
	tap    *mlatTap
	totals mlatStationTotals
	mainDB *store.Store // mlat_minute, mlat_quality_hour
	stDB   *store.Store // station_clock_minute (nil = not written)
	reg    func() []registry.Station
	fixes  <-chan model.MLATFix
	now    func() time.Time
	// tee (optional) sees every published fix first (federation).
	tee func(*model.MLATFix)

	st         map[model.StationIdx]*stMetrics
	published  uint64 // non-hold-out map updates this minute
	lastRej    uint64
	haveRej    bool
	netFrames  uint64
	netGroups  uint64
	qual       map[qualKey][]uint32
	holdScored uint64 // hold-out fixes scored (lifetime)
	rowsMinute uint64 // rows written (lifetime, for tests and status)
	minute     int64  // current minute (Unix s / 60)
}

type qualKey struct {
	hour       int64
	cell, band int
}

func newMLATRecorder(src mlatMetricsSource, tap *mlatTap, fixes <-chan model.MLATFix, mainDB, stDB *store.Store,
	reg func() []registry.Station, now func() time.Time) *mlatRecorder {
	r := &mlatRecorder{src: src, tap: tap, fixes: fixes, mainDB: mainDB, stDB: stDB, reg: reg, now: now,
		st: map[model.StationIdx]*stMetrics{}, qual: map[qualKey][]uint32{}}
	if em, ok := src.(engineMetrics); ok {
		r.totals = em.totals()
	}
	r.minute = now().Unix() / 60
	return r
}

// mlatSampleEvery is the clock sampling period (6 samples a minute).
const mlatSampleEvery = 10 * time.Second

// mlatSampleOffset places the samples just after the 10-s marks.
const mlatSampleOffset = 200 * time.Millisecond

// run samples every 10 s, flushes at every minute change and consumes the
// fix stream, until ctx ends (then flushes the partial minute).
func (r *mlatRecorder) run(ctx context.Context) {
	// Samples at :00.2, :10.2, ... :50.2: six a minute, the first one just
	// after the minute changed, which also flushes the minute that ended
	// (the hour-ring recovery wants the sample close to the boundary).
	next := func(now time.Time) time.Duration {
		return time.Until(now.Truncate(mlatSampleEvery).Add(mlatSampleEvery + mlatSampleOffset))
	}
	t := time.NewTimer(next(r.now()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.drainFixes()
			r.flush(r.minute)
			return
		case f := <-r.fixes:
			r.onFix(&f)
		case <-t.C:
			now := r.now()
			if m := now.Unix() / 60; m != r.minute {
				r.drainFixes()
				r.flush(r.minute)
				r.minute = m
			}
			r.sample(now)
			t.Reset(next(now))
		}
	}
}

func (r *mlatRecorder) drainFixes() {
	for {
		select {
		case f := <-r.fixes:
			r.onFix(&f)
		default:
			return
		}
	}
}

// onFix counts published fixes and scores hold-out fixes against their
// ADS-B reference (the same rule as the engine's own hold-out statistic:
// confirmed, fresh, not in probation, suspended, unstable, on the ground or
// backward).
func (r *mlatRecorder) onFix(f *model.MLATFix) {
	if r.tee != nil {
		r.tee(f)
	}
	if f.Flags&model.FixFlagHoldout == 0 {
		r.published++
		return
	}
	if !model.Displayable(f.Grade) {
		return
	}
	var st ref.State
	if !r.src.RefState(f.ICAO&0xFFFFFF, f.TUnixNs, &st) {
		return
	}
	bad := ref.FlagStale | ref.FlagProbation | ref.FlagUnstable | ref.FlagSuspended | ref.FlagOnGround | ref.FlagBackward
	if st.Flags&ref.FlagConfirmed == 0 || st.Flags&bad != 0 {
		return
	}
	errH := holdoutErrorH(f.ECEF, &st)
	if math.IsNaN(errH) {
		return
	}
	k := qualKey{hour: f.TUnixNs / 3600e9 * 3600, cell: store.Cell1Deg(st.Lat, st.Lon), band: store.AltBand(st.HAE / 0.3048)}
	if k.cell < 0 {
		return
	}
	h := r.qual[k]
	if h == nil {
		h = make([]uint32, store.MLATErrBins)
		r.qual[k] = h
	}
	h[store.MLATErrBin(errH)]++
	r.holdScored++
}

// holdoutErrorH is the horizontal distance between a fix and the reference
// state, in the reference's local east-north plane.
func holdoutErrorH(ecef [3]float64, st *ref.State) float64 {
	fr := geo.NewFrame(geo.LLH{Lat: st.Lat, Lon: st.Lon, H: st.HAE})
	d := fr.Rotate(geo.Vec3{X: ecef[0], Y: ecef[1], Z: ecef[2]}.Sub(st.Pos))
	return math.Hypot(d.X, d.Y)
}

// sample takes the 10-s clock sample of every station.
func (r *mlatRecorder) sample(now time.Time) {
	// The samples nearest :20 and :50 read Accepted30s (two disjoint 30-s
	// windows per minute).
	slot := now.Add(mlatSampleEvery/2).Unix() % 60 / 10
	mark30 := slot == 2 || slot == 5
	for _, sc := range r.src.ClockStations() {
		m := r.station(sc.Station)
		m.clockSeen = true
		if sc.Synced {
			m.syncedS = min(m.syncedS+int(mlatSampleEvery/time.Second), 60)
		}
		if mark30 {
			m.syncFrames += sc.Accepted30s
		}
	}
}

func (r *mlatRecorder) station(st model.StationIdx) *stMetrics {
	m := r.st[st]
	if m == nil {
		m = &stMetrics{}
		r.st[st] = m
	}
	return m
}

// residRMS is the co-observation-weighted RMS of a station's sync
// residuals against its peers (NaN without co-observations).
func residRMS(sc *clock.StationClock) float64 {
	var s, n float64
	for _, p := range sc.Peers {
		if p.Count > 0 && !math.IsNaN(p.RMSNs) {
			s += float64(p.Count) * p.RMSNs * p.RMSNs
			n += float64(p.Count)
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return math.Sqrt(s / n)
}

// hourCounts turns a quality report's hourly percentages back into counts:
// fixes used and outliers (ok false when they cannot be recovered: no row
// was used, only excluded ones).
func hourCounts(q mlat.StationReport) (used, outliers float64, ok bool) {
	if q.MLAT == nil {
		return 0, 0, true
	}
	u := float64(q.MLAT.ContribPerHour)
	switch {
	case q.MLAT.UsedPct > 0:
		tot := math.Round(u * 100 / q.MLAT.UsedPct)
		return u, math.Round(q.MLAT.OutlierPct * tot / 100), true
	case q.MLAT.ExcludedPct == 0 && q.MLAT.OutlierPct == 0:
		return u, 0, true
	}
	return u, 0, false
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	slices.Sort(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}

// flush writes the rows of minute m and resets the minute state.
func (r *mlatRecorder) flush(m int64) {
	t := m * 60
	// k is the minute the sample is taken in (the hour ring is indexed by it).
	k := r.now().Unix() / 60
	clocks := map[model.StationIdx]*clock.StationClock{}
	for _, sc := range r.src.ClockStations() {
		clocks[sc.Station] = sc
	}
	var rows []store.MLATMinute
	var clk []store.StationClockMinute
	net := store.MLATMinute{T: t, SID: "", SyncedSecs: math.NaN(), DriftPPM: math.NaN()}
	var sig, rms []float64
	stations := r.reg()
	known := make(map[model.StationIdx]struct{}, len(stations))
	for i := range stations {
		s := &stations[i]
		known[s.Idx] = struct{}{}
		ms := r.station(s.Idx)
		fr, gr := r.tap.station(s.Idx)
		row := store.MLATMinute{T: t, SID: s.SID, Frames: int64(fr - ms.frames), Groups: int64(gr - ms.groups),
			SyncFrames: int64(ms.syncFrames), SyncedSecs: math.NaN(), SigmaNs: math.NaN(), ResidRMSNs: math.NaN(), DriftPPM: math.NaN()}
		ms.frames, ms.groups = fr, gr

		// Fixes and outliers.
		if r.totals != nil {
			if f, o, ok := r.totals.StationTotals(s.Idx); ok {
				row.Fixes, row.Outliers = int64(f-min(f, ms.fixes)), int64(o-min(o, ms.outs))
				ms.fixes, ms.outs = f, o
			}
		} else if rep, ok := r.src.Station(s.Idx); ok && rep.MLAT != nil {
			used, outl, okc := hourCounts(rep)
			row.Fixes = int64(ms.fixRing.sample(k, used))
			if okc {
				row.Outliers = int64(ms.outRing.sample(k, outl))
			}
		}

		// Clock state.
		sc := clocks[s.Idx]
		if sc != nil {
			for _, ev := range sc.Events {
				if ev.T > ms.lastEvT {
					if ev.Reason == "epoch" {
						ms.epochs++
					}
					ms.lastEvT = ev.T
				}
			}
			row.Peers = int64(sc.SyncedPeers())
			row.SigmaNs = sc.SigmaThetaNs
			row.DriftPPM = sc.Phi / 1000
			row.ResidRMSNs = residRMS(sc)
		}
		if ms.clockSeen {
			row.SyncedSecs = float64(ms.syncedS)
		}
		if sc != nil && sc.Synced {
			net.Peers++
			if !math.IsNaN(sc.SigmaThetaNs) {
				sig = append(sig, sc.SigmaThetaNs)
			}
			if !math.IsNaN(row.ResidRMSNs) {
				rms = append(rms, row.ResidRMSNs)
			}
		}
		net.SyncFrames += row.SyncFrames
		if row.Frames > 0 || row.Fixes > 0 || row.Outliers > 0 || row.SyncFrames > 0 || ms.syncedS > 0 {
			rows = append(rows, row)
		}
		if sc != nil && r.stDB != nil {
			clk = append(clk, store.StationClockMinute{T: t, SID: s.SID, SyncedSecs: row.SyncedSecs,
				SigmaNs: row.SigmaNs, DriftPPM: row.DriftPPM, BetaShortNs: sc.Beta, ResidRMSNs: row.ResidRMSNs,
				Outliers: row.Outliers, Peers: row.Peers, EpochChanges: ms.epochs})
		}
		ms.syncedS, ms.syncFrames, ms.clockSeen, ms.epochs = 0, 0, false, 0
	}
	for idx := range r.st {
		if _, ok := known[idx]; !ok {
			delete(r.st, idx) // deleted, revoked or denied
		}
	}

	// Network row.
	nf, ng := r.tap.frames.Load(), r.tap.groups.Load()
	net.Frames, net.Groups = int64(nf-r.netFrames), int64(ng-r.netGroups)
	r.netFrames, r.netGroups = nf, ng
	net.Fixes = int64(r.published)
	r.published = 0
	if rej := r.src.Stats().Rejected; r.haveRej {
		net.Outliers = int64(rej - min(rej, r.lastRej))
		r.lastRej = rej
	} else {
		r.lastRej, r.haveRej = rej, true
	}
	net.SigmaNs, net.ResidRMSNs = median(sig), median(rms)
	rows = append(rows, net)

	if r.mainDB != nil {
		r.mainDB.AddMLATMinutes(rows)
		if len(r.qual) > 0 {
			inc := make([]store.MLATQualityInc, 0, len(r.qual))
			for k, h := range r.qual {
				inc = append(inc, store.MLATQualityInc{T: k.hour, Cell: k.cell, AltBand: k.band, Hist: h})
			}
			r.mainDB.MergeMLATQuality(inc)
			clear(r.qual)
		}
	}
	if len(clk) > 0 {
		r.stDB.AddStationClockMinutes(clk)
	}
	r.rowsMinute += uint64(len(rows))

}
