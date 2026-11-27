package server

import (
	"context"
	"math"
	"sync"
	"time"

	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/quality"
	"airview/internal/model"
	"airview/internal/registry"
)

// MLAT console history (admin MLAT console, owner Timing section;
// DESIGN §9.6, §11).
//
// The MLAT engine keeps cumulative counters and sliding hold-out windows but
// no time series. The server samples it once a minute and keeps
//
//   - a network ring (mlatHistNet points, 24 h): the hold-out error of the
//     preceding mlatHistWindowMin minutes (fix and track: N, P50, P95,
//     cross-track P95, share within 1σ), per-minute stage rates derived
//     from counter deltas (groups, clusters, solves, accepted solves,
//     rejections, published map updates, results sent, queue drops), the
//     clock filter (stations, components, synced stations, last epoch cost)
//     and the worker load;
//   - a small ring per station (mlatHistStation points, 2 h): σθ, drift,
//     clock outlier share, residual bias, contributions per hour, synced.
//
// Rates: r = Δcounter / Δt (per minute). A counter that went backwards (the
// engine was rebuilt by a factory reset or a restart of the network part)
// starts a new baseline; that point has no rates (null), never negative or
// absurd ones. Points are only recorded while an engine runs.
//
// Memory: 1,440 network points × ~180 B ≈ 260 kB; per station 120 × 28 B ≈
// 3.4 kB, i.e. 3.4 MB at 1,000 stations (DESIGN §20.2). Stations the
// registry no longer lists are dropped at the next sample.
//
// The percentiles of a point are those of an overlapping 10-minute window:
// a 1-minute window holds too few hold-out samples on a small network (the
// hold-out is 10 % of the NACp ≥ 9 aircraft) to give a usable P95. N is
// reported with every point so a reader can judge it.

const (
	mlatHistEvery     = time.Minute
	mlatHistNet       = 24 * 60
	mlatHistStation   = 120
	mlatHistWindowMin = 10
)

// mlatSource is what the server reads from the MLAT engine. *mlat.Engine
// implements it through engineMLAT; tests substitute a fake (Server.testMLAT)
// to drive the golden crawls with populated reports.
type mlatSource interface {
	Stats() mlat.Stats
	QualityReport(windowMin int, cells bool) mlat.Quality
	PublicQuality() mlat.PublicQuality
	Station(st model.StationIdx) (mlat.StationReport, bool)
	SyncMatrix() []mlat.SyncEdge
	CRLBMap(p quality.CRLBParams) []quality.CRLBCell
	// ClockOriginUnixNs is the origin of the clock filter's network seconds
	// (clock event times are relative to it).
	ClockOriginUnixNs() int64
	// Key identifies the engine instance (a rebuilt engine has another).
	Key() any
}

type engineMLAT struct{ *mlat.Engine }

func (e engineMLAT) ClockOriginUnixNs() int64 {
	if sn := e.Filter().Snapshot(); sn != nil {
		return sn.OriginUnixNs
	}
	return 0
}

func (e engineMLAT) Key() any { return e.Engine }

// mlatSrc returns the running MLAT engine, or nil.
func (s *Server) mlatSrc() mlatSource {
	if s.testMLAT != nil {
		return s.testMLAT
	}
	if s.eng == nil {
		return nil
	}
	m := s.eng.MLAT()
	if m == nil {
		return nil
	}
	return engineMLAT{m}
}

// mlatPoint is one network sample.
type mlatPoint struct {
	T int64 // Unix ms

	FixN, TrackN         uint64
	FixP50, FixP95       float64
	TrackP50, TrackP95   float64
	TrackCross95, Within float64 // track cross-track P95 (m), track share within 1σ (0..1)

	HasRates                                       bool
	Groups, Clusters, Solves, SolveOK, Rejected    float64 // per minute
	Published, Results, Dropped, HoldFix, HoldTrks float64 // per minute

	Stations, Components, Synced int
	EpochMs, ProcUs              float64
	QueueFill                    int
}

// mlatStPoint is one per-station sample (float32: the ring is per station).
type mlatStPoint struct {
	T                                   int64
	SigmaThetaNs, DriftPPM, OutlierPct  float32
	ResidualBiasNs                      float32
	Contrib                             int32
	Synced, HasClock, HasMLAT, Assigned bool
}

type stRing struct {
	pts  [mlatHistStation]mlatStPoint
	head int // next write
	n    int
	seen int64 // sample generation that last saw the station
}

func (r *stRing) add(p mlatStPoint) {
	r.pts[r.head] = p
	r.head = (r.head + 1) % len(r.pts)
	r.n = min(r.n+1, len(r.pts))
}

// list returns the points oldest first, newer than sinceMs.
func (r *stRing) list(sinceMs int64) []mlatStPoint {
	out := make([]mlatStPoint, 0, r.n)
	for i := 0; i < r.n; i++ {
		p := r.pts[(r.head-r.n+i+len(r.pts))%len(r.pts)]
		if p.T >= sinceMs {
			out = append(out, p)
		}
	}
	return out
}

type mlatHistory struct {
	mu   sync.Mutex
	pts  []mlatPoint // ring
	head int
	n    int

	key      any
	prev     mlat.Stats
	prevT    time.Time
	havePrev bool

	gen int64
	st  map[model.StationIdx]*stRing
	// pending holds station rings restored from the history file by SID
	// until the first sample maps them to station indexes
	// (mlat_history_store.go).
	pending map[string][]mlatStPoint
}

func newMLATHistory() *mlatHistory {
	return &mlatHistory{pts: make([]mlatPoint, mlatHistNet), st: map[model.StationIdx]*stRing{}}
}

// run samples every mlatHistEvery until ctx ends (started by Server.Start).
// It restores the saved history first, saves it every mlatHistSaveEvery and
// once more when ctx ends (mlat_history_store.go).
func (h *mlatHistory) run(ctx context.Context, s *Server) {
	s.loadMLATHistory(time.Now())
	t := time.NewTicker(mlatHistEvery)
	defer t.Stop()
	save := time.NewTicker(mlatHistSaveEvery)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			s.saveMLATHistory(time.Now())
			return
		case now := <-save.C:
			s.saveMLATHistory(now)
		case now := <-t.C:
			src := s.mlatSrc()
			if src == nil {
				continue
			}
			var ids []model.StationIdx
			if reg, err := s.reg(); err == nil && reg != nil {
				var bySID map[string]model.StationIdx
				if h.hasPending() {
					bySID = map[string]model.StationIdx{}
				}
				for _, st := range reg.List(registry.Filter{}) {
					ids = append(ids, st.Idx)
					if bySID != nil {
						bySID[st.SID] = st.Idx
					}
				}
				if bySID != nil {
					h.attachPending(bySID)
				}
			}
			h.sample(src, ids, now)
		}
	}
}

// rate is Δ/Δt per minute.
func perMin(cur, prev uint64, dtMin float64) float64 { return float64(cur-prev) / dtMin }

// sample records one network point and one point per listed station the
// engine knows (ids: the registry's stations; rings of stations no longer
// listed or known are dropped).
func (h *mlatHistory) sample(src mlatSource, ids []model.StationIdx, now time.Time) {
	st := src.Stats()
	q := src.QualityReport(mlatHistWindowMin, false)
	p := mlatPoint{T: now.UnixMilli(),
		FixN: q.Fix.N, FixP50: fin(q.Fix.P50), FixP95: fin(q.Fix.P95),
		TrackN: q.Track.N, TrackP50: fin(q.Track.P50), TrackP95: fin(q.Track.P95),
		TrackCross95: fin(q.Track.CrossP95), Within: fin(q.Track.Within1Sigma),
		Stations: st.Clock.Stations, Components: st.Clock.Components,
		EpochMs: float64(epochNs(st.Clock)) / 1e6, ProcUs: fin(st.ProcUsPerGroup), QueueFill: st.QueueFill}

	h.mu.Lock()
	defer h.mu.Unlock()
	key := src.Key()
	if h.havePrev && h.key == key && !counterReset(st, h.prev) {
		if dt := now.Sub(h.prevT).Minutes(); dt > 0.05 {
			p.HasRates = true
			p.Groups = perMin(st.Groups, h.prev.Groups, dt)
			p.Clusters = perMin(st.Clusters, h.prev.Clusters, dt)
			p.Solves = perMin(st.Solves, h.prev.Solves, dt)
			p.SolveOK = perMin(st.SolveOK, h.prev.SolveOK, dt)
			p.Rejected = perMin(st.Rejected, h.prev.Rejected, dt)
			p.Published = perMin(st.Published, h.prev.Published, dt)
			p.Results = perMin(st.ResultsSent, h.prev.ResultsSent, dt)
			p.Dropped = perMin(st.Dropped, h.prev.Dropped, dt)
			p.HoldFix = perMin(st.HoldoutFixes, h.prev.HoldoutFixes, dt)
			p.HoldTrks = perMin(st.HoldoutTracks, h.prev.HoldoutTracks, dt)
		}
	}
	if h.key != nil && h.key != key {
		clear(h.st) // another engine: the station rings describe the old one
	}
	h.key, h.prev, h.prevT, h.havePrev = key, st, now, true

	h.gen++
	for _, idx := range ids {
		rep, ok := src.Station(idx)
		if !ok {
			if r := h.st[idx]; r != nil {
				r.seen = h.gen // still registered (e.g. a restored ring before the engine knows it)
			}
			continue
		}
		sp := mlatStPoint{T: p.T, Assigned: true}
		if c := rep.Clock; c != nil {
			sp.HasClock = true
			sp.Synced = c.Synced
			sp.SigmaThetaNs = float32(fin(c.SigmaThetaNs))
			sp.DriftPPM = float32(fin(c.DriftPPM))
			sp.OutlierPct = float32(fin(c.OutlierPct))
			if c.Synced {
				p.Synced++
			}
		}
		if m := rep.MLAT; m != nil {
			sp.HasMLAT = true
			sp.ResidualBiasNs = float32(fin(m.ResidualBiasNs))
			sp.Contrib = int32(m.ContribPerHour)
		}
		r := h.st[idx]
		if r == nil {
			r = &stRing{}
			h.st[idx] = r
		}
		r.seen = h.gen
		r.add(sp)
	}
	for idx, r := range h.st {
		if r.seen != h.gen {
			delete(h.st, idx)
		}
	}

	h.pts[h.head] = p
	h.head = (h.head + 1) % len(h.pts)
	h.n = min(h.n+1, len(h.pts))
}

// counterReset reports whether any cumulative counter went backwards.
func counterReset(cur, prev mlat.Stats) bool {
	return cur.Groups < prev.Groups || cur.Clusters < prev.Clusters || cur.Solves < prev.Solves ||
		cur.SolveOK < prev.SolveOK || cur.Rejected < prev.Rejected || cur.Published < prev.Published ||
		cur.ResultsSent < prev.ResultsSent || cur.Dropped < prev.Dropped ||
		cur.HoldoutFixes < prev.HoldoutFixes || cur.HoldoutTracks < prev.HoldoutTracks
}

// fin maps NaN / Inf to 0.
func fin(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// points returns the network points of the last `minutes`, oldest first.
func (h *mlatHistory) points(now time.Time, minutes int) []mlatPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	since := now.Add(-time.Duration(minutes) * time.Minute).UnixMilli()
	out := make([]mlatPoint, 0, h.n)
	for i := 0; i < h.n; i++ {
		p := h.pts[(h.head-h.n+i+len(h.pts))%len(h.pts)]
		if p.T >= since {
			out = append(out, p)
		}
	}
	return out
}

// station returns a station's points (oldest first), nil when unknown.
func (h *mlatHistory) station(idx model.StationIdx) []mlatStPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r := h.st[idx]; r != nil {
		return r.list(0)
	}
	return nil
}

// ---------------------------------------------------------------- DTOs

// mlatHistoryDTO is GET /api/admin/mlat/history: columns of equal length
// (one entry per sample, oldest first). Rates are per minute; null where
// the engine restarted.
type mlatHistoryDTO struct {
	Running     bool    `json:"running"`
	IntervalSec int     `json:"intervalSec"`
	WindowMin   int     `json:"windowMin"`
	T           []int64 `json:"t"`

	FixN         []uint64  `json:"fixN"`
	FixP50       []float64 `json:"fixP50M"`
	FixP95       []float64 `json:"fixP95M"`
	TrackN       []uint64  `json:"trackN"`
	TrackP50     []float64 `json:"trackP50M"`
	TrackP95     []float64 `json:"trackP95M"`
	TrackCross95 []float64 `json:"trackCrossP95M"`
	Within1Sigma []float64 `json:"within1Sigma"`

	Groups    []*float64 `json:"groupsPerMin"`
	Clusters  []*float64 `json:"clustersPerMin"`
	Solves    []*float64 `json:"solvesPerMin"`
	SolveOK   []*float64 `json:"solveOkPerMin"`
	Rejected  []*float64 `json:"rejectedPerMin"`
	Published []*float64 `json:"publishedPerMin"`
	Results   []*float64 `json:"resultsPerMin"`
	Dropped   []*float64 `json:"droppedPerMin"`

	Stations   []int     `json:"stations"`
	Synced     []int     `json:"synced"`
	Components []int     `json:"components"`
	EpochMs    []float64 `json:"epochMs"`
	ProcUs     []float64 `json:"procUsPerGroup"`
	QueueFill  []int     `json:"queueFill"`
}

func round2(v float64) float64 { return math.Round(fin(v)*100) / 100 }

func historyDTO(pts []mlatPoint) mlatHistoryDTO {
	n := len(pts)
	d := mlatHistoryDTO{Running: true, IntervalSec: int(mlatHistEvery / time.Second), WindowMin: mlatHistWindowMin,
		T: make([]int64, n), FixN: make([]uint64, n), FixP50: make([]float64, n), FixP95: make([]float64, n),
		TrackN: make([]uint64, n), TrackP50: make([]float64, n), TrackP95: make([]float64, n),
		TrackCross95: make([]float64, n), Within1Sigma: make([]float64, n),
		Groups: make([]*float64, n), Clusters: make([]*float64, n), Solves: make([]*float64, n), SolveOK: make([]*float64, n),
		Rejected: make([]*float64, n), Published: make([]*float64, n), Results: make([]*float64, n), Dropped: make([]*float64, n),
		Stations: make([]int, n), Synced: make([]int, n), Components: make([]int, n), EpochMs: make([]float64, n),
		ProcUs: make([]float64, n), QueueFill: make([]int, n)}
	opt := func(ok bool, v float64) *float64 {
		if !ok {
			return nil
		}
		v = round2(v)
		return &v
	}
	for i, p := range pts {
		d.T[i] = p.T
		d.FixN[i], d.FixP50[i], d.FixP95[i] = p.FixN, rnd1(p.FixP50), rnd1(p.FixP95)
		d.TrackN[i], d.TrackP50[i], d.TrackP95[i] = p.TrackN, rnd1(p.TrackP50), rnd1(p.TrackP95)
		d.TrackCross95[i], d.Within1Sigma[i] = rnd1(p.TrackCross95), math.Round(p.Within*1000)/1000
		d.Groups[i], d.Clusters[i], d.Solves[i] = opt(p.HasRates, p.Groups), opt(p.HasRates, p.Clusters), opt(p.HasRates, p.Solves)
		d.SolveOK[i], d.Rejected[i], d.Published[i] = opt(p.HasRates, p.SolveOK), opt(p.HasRates, p.Rejected), opt(p.HasRates, p.Published)
		d.Results[i], d.Dropped[i] = opt(p.HasRates, p.Results), opt(p.HasRates, p.Dropped)
		d.Stations[i], d.Synced[i], d.Components[i] = p.Stations, p.Synced, p.Components
		d.EpochMs[i], d.ProcUs[i], d.QueueFill[i] = round2(p.EpochMs), round2(p.ProcUs), p.QueueFill
	}
	return d
}

// rnd1 rounds to 0.1 (NaN / Inf -> 0).
func rnd1(v float64) float64 { return math.Round(fin(v)*10) / 10 }

// mlatStHistDTO is a station's history (columns, oldest first). Values of
// samples without a clock / MLAT report are null.
type mlatStHistDTO struct {
	IntervalSec int        `json:"intervalSec"`
	T           []int64    `json:"t"`
	Synced      []bool     `json:"synced"`
	SigmaTheta  []*float64 `json:"sigmaThetaNs"`
	DriftPPM    []*float64 `json:"driftPpm"`
	OutlierPct  []*float64 `json:"outlierPct"`
	BiasNs      []*float64 `json:"residualBiasNs"`
	Contrib     []*int     `json:"contribPerHour"`
}

func stHistDTO(pts []mlatStPoint) mlatStHistDTO {
	n := len(pts)
	d := mlatStHistDTO{IntervalSec: int(mlatHistEvery / time.Second), T: make([]int64, n), Synced: make([]bool, n),
		SigmaTheta: make([]*float64, n), DriftPPM: make([]*float64, n), OutlierPct: make([]*float64, n),
		BiasNs: make([]*float64, n), Contrib: make([]*int, n)}
	f := func(ok bool, v float32, dec float64) *float64 {
		if !ok {
			return nil
		}
		x := math.Round(float64(v)*dec) / dec
		return &x
	}
	for i, p := range pts {
		d.T[i], d.Synced[i] = p.T, p.Synced
		d.SigmaTheta[i] = f(p.HasClock, p.SigmaThetaNs, 10)
		d.DriftPPM[i] = f(p.HasClock, p.DriftPPM, 1e4)
		d.OutlierPct[i] = f(p.HasClock, p.OutlierPct, 100)
		d.BiasNs[i] = f(p.HasMLAT, p.ResidualBiasNs, 10)
		if p.HasMLAT {
			c := int(p.Contrib)
			d.Contrib[i] = &c
		}
	}
	return d
}

// epochNs is the clock-filter epoch cost to show: the running mean when the
// filter reports one (a single epoch often reads 0 on Windows, whose
// monotonic clock ticks in about 0.5 ms steps), else the last epoch.
func epochNs(c clock.Stats) int64 {
	if c.MeanEpochNs > 0 {
		return c.MeanEpochNs
	}
	return c.LastEpochNs
}
