package mlat

import (
	"math"
	"sort"

	"airview/internal/ingest/mlatc"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/group"
	"airview/internal/mlat/plan"
	"airview/internal/mlat/quality"
	"airview/internal/mlat/ref"
	"airview/internal/mlat/track"
	"airview/internal/model"
)

// Stats are the engine's admin counters (admin MLAT console). They contain
// no station positions.
type Stats struct {
	Workers   int    `json:"workers"`
	QueueLen  int    `json:"queueLen"`
	QueueFill int    `json:"queueFill"` // groups waiting over all workers
	Groups    uint64 `json:"groups"`    // groups received
	Late      uint64 `json:"late"`      // late deliveries received
	Dropped   uint64 `json:"dropped"`   // groups dropped: worker queue full (backpressure)
	// ProcUsPerGroup is the mean worker time per group (µs).
	ProcUsPerGroup float64 `json:"procUsPerGroup"`

	SyncAdmitted uint64 `json:"syncAdmitted"` // groups admitted to the clock filter
	RefObserved  uint64 `json:"refObserved"`  // ADS-B messages accepted by the reference store

	Clusters       uint64            `json:"clusters"`
	LateSkipped    uint64            `json:"lateSkipped"` // late re-emissions of already used clusters
	Solves         uint64            `json:"solves"`
	SolveOK        uint64            `json:"solveOk"`
	SolveErr       uint64            `json:"solveErr"` // no solution (too few, no convergence)
	Rejected       uint64            `json:"rejected"` // solutions rejected (reasons in Rejects)
	Rejects        map[string]uint64 `json:"rejects"`
	FixGrades      map[string]uint64 `json:"fixGrades"` // accepted snapshot fixes by grade
	FixToTrack     uint64            `json:"fixToTrack"`
	ClusterToTrack uint64            `json:"clusterToTrack"`
	Applied        uint64            `json:"applied"`
	Gated          uint64            `json:"gated"`
	Velocities     uint64            `json:"velocities"`
	Published      uint64            `json:"published"` // map updates
	Deleted        uint64            `json:"deleted"`   // tracks dropped because ADS-B took over
	// Restarts counts tracks dropped and restarted because redundant fixes
	// contradicted them or redundant clusters kept failing their gate;
	// GateSolves the clusters solved only to check a gated track (H-track).
	Restarts     uint64 `json:"restarts"`
	GateSolves   uint64 `json:"gateSolves"`
	FixesDropped uint64 `json:"fixesDropped"`

	HoldoutFixes  uint64 `json:"holdoutFixes"`  // hold-out snapshot fixes scored
	HoldoutTracks uint64 `json:"holdoutTracks"` // hold-out track estimates scored

	ADSBConfirmed    uint64 `json:"adsbConfirmed"`
	ADSBContradicted uint64 `json:"adsbContradicted"`
	ADSBSuspects     uint64 `json:"adsbSuspects"`
	// ADSBBlamed counts contradictions charged to a declared-only station
	// instead of the aircraft (blame.go).
	ADSBBlamed uint64 `json:"adsbBlamed"`
	// ADSBCulprit counts phase-4 checks whose leave-one-out test blamed one
	// station row (not the aircraft).
	ADSBCulprit uint64 `json:"adsbCulprit"`
	// ADSBMuted counts contradictions that were not charged to the
	// aircraft because the network-wide contradicted share was implausibly
	// high (the clocks, not the aircraft, are wrong; verifybreaker.go).
	ADSBMuted uint64 `json:"adsbMuted"`
	// AdvancePanics counts recovered panics of the time-driven work (Run;
	// the clock filter's own are Clock.PanicResets).
	AdvancePanics uint64 `json:"advancePanics"`

	ResultsSent    uint64 `json:"resultsSent"`
	ResultsRefused uint64 `json:"resultsRefused"` // minimum-contribution rule or no link

	Clock clock.Stats  `json:"clock"`
	Ref   ref.Stats    `json:"ref"`
	Group group.Stats  `json:"group"`
	Track track.Stats  `json:"track"`
	Plan  plan.Summary `json:"plan"`

	KAtm KAtmReport `json:"kAtm"`
	// LatencyCal is the airframe-latency calibration's A/B report (nil while
	// aggregator.mlat.latencyCal is off).
	LatencyCal *quality.ABReport `json:"latencyCal,omitempty"`
	// ModeAC is the Mode A/C path (nil while aggregator.mlat.modeAc is off).
	ModeAC *ModeACStats `json:"modeAc,omitempty"`
}

// KAtmReport is the refractivity-scale estimate (DESIGN §9.2.6).
type KAtmReport struct {
	Model    float64 `json:"model"`    // k_atm in use
	Estimate float64 `json:"estimate"` // last hourly estimate (= Model before the first hour)
	SigmaB   float64 `json:"sigmaB"`   // 1σ of the fitted slope
	Hours    int     `json:"hours"`
	Applied  bool    `json:"applied"` // ApplyKAtm
	// Report is the projected estimator's state (phase 4): method, k, σ,
	// hours, χ²/hour and Ready (applied only when Ready).
	Report quality.KAtmReport `json:"report"`
}

var rejectNames = [...]string{"sigma", "altitude", "horizon", "speed", "hosttime", "implausible", "notconverged", "raim"}

// Stats returns the counters.
func (e *Engine) Stats() Stats {
	c := &e.ctr
	s := Stats{Workers: len(e.workers), QueueLen: e.o.QueueLen,
		Groups: c.groups.Load(), Late: c.late.Load(), Dropped: c.dropped.Load(),
		SyncAdmitted: c.syncAdmitted.Load(), RefObserved: c.refObs.Load(),
		Clusters: c.clusters.Load(), LateSkipped: c.lateSkipped.Load(), Solves: c.solves.Load(),
		SolveOK: c.solveOK.Load(), SolveErr: c.solveErr.Load(), Rejected: c.rejected.Load(),
		FixToTrack: c.fixToTrack.Load(), ClusterToTrack: c.clusterTrack.Load(), Applied: c.applied.Load(),
		Gated: c.gated.Load(), Velocities: c.velocities.Load(), Published: c.published.Load(),
		Deleted: c.deleted.Load(), FixesDropped: c.fixesDropped.Load(), Restarts: c.restarts.Load(), GateSolves: c.gateSolves.Load(),
		HoldoutFixes: c.holdoutFix.Load(), HoldoutTracks: c.holdoutTrack.Load(),
		ADSBConfirmed: c.verified.Load(), ADSBContradicted: c.contradicted.Load(), ADSBSuspects: c.suspect.Load(), ADSBBlamed: c.verifyBlamed.Load(), ADSBCulprit: c.verifyCulprit.Load(), ADSBMuted: c.verifyMuted.Load(), AdvancePanics: c.advancePanics.Load(),
		ResultsSent: c.resultsSent.Load(), ResultsRefused: c.resultsRefused.Load(),
		Rejects: map[string]uint64{}, FixGrades: map[string]uint64{},
		Clock: e.filter.Stats(), Ref: e.refs.Stats(), Plan: e.planner.Summary()}
	if n := s.Groups + s.Late; n > 0 {
		s.ProcUsPerGroup = float64(c.procNs.Load()) / float64(n) / 1e3
	}
	for i, n := range rejectNames {
		if v := c.rejects[i].Load(); v > 0 {
			s.Rejects[n] = v
		}
	}
	for g := range c.grades {
		if v := c.grades[g].Load(); v > 0 {
			name := model.GradeString(uint8(g))
			if name == "" {
				name = "none"
			}
			s.FixGrades[name] = v
		}
	}
	for _, w := range e.workers {
		if w.jobs != nil {
			s.QueueFill += len(w.jobs)
		}
		addGroupStats(&s.Group, w.asm.Stats())
		if ts := w.trStats.Load(); ts != nil {
			addTrackStats(&s.Track, ts)
		}
	}
	m := e.Atmosphere().K
	s.KAtm.Model, s.KAtm.Applied = m, e.o.ApplyKAtm
	s.KAtm.Estimate, s.KAtm.SigmaB, s.KAtm.Hours, _ = e.katm.Estimate(m)
	s.KAtm.Report = e.katm.Report()
	s.LatencyCal = e.LatencyCalReport()
	if e.modeAC.Load() != nil {
		ac := e.ModeACStats()
		s.ModeAC = &ac
	}
	return s
}

func addGroupStats(a *group.Stats, b group.Stats) {
	a.Groups += b.Groups
	a.Excluded += b.Excluded
	a.NotCandidate += b.NotCandidate
	a.TooFew += b.TooFew
	a.Copies += b.Copies
	a.Untimed += b.Untimed
	a.NoClock += b.NoClock
	a.NoSite += b.NoSite
	a.Usable += b.Usable
	a.Clusters += b.Clusters
	a.Members += b.Members
	a.Solve += b.Solve
	a.SolveAlt += b.SolveAlt
	a.Track += b.Track
	a.Dropped += b.Dropped
	a.Split += b.Split
	a.Leftover += b.Leftover
	a.Hinted += b.Hinted
	a.HintConflict += b.HintConflict
	a.NotAdmitted += b.NotAdmitted
	a.BudgetOut += b.BudgetOut
	a.LateGroups += b.LateGroups
	a.LateCopies += b.LateCopies
	a.LateAttached += b.LateAttached
	a.LateClusters += b.LateClusters
}

func addTrackStats(a *track.Stats, b *track.Stats) {
	a.Tracks += b.Tracks
	a.Confirmed += b.Confirmed
	a.Hypotheses += b.Hypotheses
	a.Clusters += b.Clusters
	a.Fixes += b.Fixes
	a.Applied += b.Applied
	a.Gated += b.Gated
	a.Excluded += b.Excluded
	a.OOSM += b.OOSM
	a.TooLate += b.TooLate
	a.Created += b.Created
	a.Confirmations += b.Confirmations
	a.Pruned += b.Pruned
	a.Deleted += b.Deleted
	a.Respawns += b.Respawns
}

// --- quality ---

// Quality is the hold-out self-validation report (admin; DESIGN §9.6).
type Quality struct {
	Fraction float64 `json:"fraction"` // configured hold-out fraction
	// Fix: snapshot solver fixes of hold-out aircraft (displayable grades);
	// Track: the tracker estimates that would have been published. Window =
	// the last WindowMin minutes; Total = since start.
	WindowMin  int             `json:"windowMin"`
	Fix        quality.Summary `json:"fix"`
	Track      quality.Summary `json:"track"`
	FixTotal   quality.Summary `json:"fixTotal"`
	TrackTotal quality.Summary `json:"trackTotal"`
	// Alarm: P95 (track) of the last 7 days regressed by > 25 % against
	// the 7 days before.
	Alarm       bool    `json:"alarm"`
	AlarmThisP  float64 `json:"alarmThisP95M,omitempty"`
	AlarmLastP  float64 `json:"alarmLastP95M,omitempty"`
	AlarmActive bool    `json:"alarmActive"` // both weeks have enough data
	// FixEst / TrackEst: the horizontal error percentiles estimated from the
	// cross-track errors alone (EstimateFromCross), free of the hold-out
	// reference's unknown airframe position latency, which inflates the
	// along-track part of Fix and Track.
	FixEst   ErrorEstimate `json:"fixEst"`
	TrackEst ErrorEstimate `json:"trackEst"`
	// Cells is the error map (every cell with data).
	Cells []quality.Cell `json:"cells,omitempty"`
}

// ErrorEstimate is a latency-free estimate of the horizontal error
// percentiles (m), see EstimateFromCross.
type ErrorEstimate struct {
	P50 float64 `json:"p50M"`
	P95 float64 `json:"p95M"`
}

// Ratios of the horizontal (Rayleigh) to the one-axis (half-normal)
// quantiles of an isotropic 2-D Gaussian error: P50 1.1774σ / 0.6745σ,
// P95 2.4477σ / 1.9600σ.
const (
	crossToH50 = 1.1774 / 0.6745
	crossToH95 = 2.4477 / 1.9600
)

// EstimateFromCross estimates the horizontal error percentiles of the MLAT
// positions from their cross-track errors against the ADS-B reference
// (H-track item 8). The reference position of a hold-out aircraft lags by
// its unknown airframe latency (DO-260B allows −0.2..+0.4 s): along track
// that is up to 100 m at cruise speed, so the full error against the
// reference overstates the MLAT error (simnet: live P50 32-50 m where the
// same tracks are 4-7 m from the truth). Across track the latency does not
// enter. The MLAT error's orientation follows the station geometry, not the
// aircraft's heading, so over many aircraft it is isotropic relative to the
// track and the one-axis cross-track distribution determines the
// horizontal one; the ratios are those of an isotropic Gaussian. This
// replaces calibrating the reference latency with the MLAT fixes themselves
// (LatencyFeedback), which would make the hold-out score itself.
func EstimateFromCross(s quality.Summary) ErrorEstimate {
	if math.IsNaN(s.CrossP50) || s.CrossP50 <= 0 {
		return ErrorEstimate{}
	}
	return ErrorEstimate{P50: crossToH50 * s.CrossP50, P95: crossToH95 * s.CrossP95}
}

// QualityReport returns the hold-out report over the last windowMin
// minutes (1..60).
func (e *Engine) QualityReport(windowMin int, cells bool) Quality {
	now := e.nowNs.Load()
	windowMin = min(max(windowMin, 1), 60)
	w := int64(windowMin) * 60e9
	q := Quality{Fraction: e.hold.Fraction(), WindowMin: windowMin,
		Fix: e.qFix.Window(now, w), Track: e.qTrack.Window(now, w),
		FixTotal: e.qFix.Total(now), TrackTotal: e.qTrack.Total(now)}
	q.AlarmThisP, q.AlarmLastP, q.Alarm, q.AlarmActive = e.qTrack.Alarm(now, 200)
	q.FixEst, q.TrackEst = EstimateFromCross(q.Fix), EstimateFromCross(q.Track)
	if cells {
		q.Cells = e.cells.List(1, 0)
	}
	return q
}

// PublicQuality is the public "empirical MLAT quality" (DESIGN §8.2):
// network percentiles of the hold-out error and the 1° cells with at least
// 5 contributing stations. No station identities or positions.
type PublicQuality struct {
	// P50M/P95M/P99M: horizontal error of the hold-out tracks against the
	// ADS-B reference (includes the reference's airframe latency, so it
	// overstates the MLAT error).
	P50M float64 `json:"p50M"`
	P95M float64 `json:"p95M"`
	P99M float64 `json:"p99M"`
	// CrossP50M/CrossP95M: the cross-track part alone; EstP50M/EstP95M: the
	// horizontal percentiles estimated from it (EstimateFromCross), the
	// honest headline figure (H-track item 8).
	CrossP50M   float64        `json:"crossP50M"`
	CrossP95M   float64        `json:"crossP95M"`
	EstP50M     float64        `json:"estP50M"`
	EstP95M     float64        `json:"estP95M"`
	Samples     uint64         `json:"samples"`
	FixesPerMin float64        `json:"fixesPerMin"` // map updates per minute over the last hour
	Cells       []quality.Cell `json:"cells,omitempty"`
}

// PublicQuality returns the public summary over the last hour (zero values
// and no cells while there are fewer than 20 samples).
func (e *Engine) PublicQuality() PublicQuality {
	now := e.nowNs.Load()
	s := e.qTrack.Window(now, 3600e9)
	p := PublicQuality{Samples: s.N}
	if s.N >= 20 {
		p.P50M, p.P95M, p.P99M = round1(s.P50), round1(s.P95), round1(s.P99)
		p.CrossP50M, p.CrossP95M = round1(s.CrossP50), round1(s.CrossP95)
		est := EstimateFromCross(s)
		p.EstP50M, p.EstP95M = round1(est.P50), round1(est.P95)
		p.Cells = e.cells.List(20, 5)
		for i := range p.Cells {
			p.Cells[i].P50, p.Cells[i].P95 = round1(p.Cells[i].P50), round1(p.Cells[i].P95)
		}
	}
	p.FixesPerMin = e.pubRate()
	return p
}

func round1(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Round(v*10) / 10
}

// pubRate is the published updates per minute since start (coarse).
func (e *Engine) pubRate() float64 {
	first := e.firstNs.Load()
	if first == 0 {
		return 0
	}
	d := float64(e.nowNs.Load()-first) / 60e9
	if d <= 0 {
		return 0
	}
	return float64(e.ctr.published.Load()) / d
}

// --- per station (owner / admin) ---

// ClockReport is a station's clock state (DESIGN §11 "Timing"). Peers carry
// raw station indices: the server maps them to unlinkable tags for owners
// (reg.PeerTag) and never shows them to the public.
type ClockReport struct {
	Class        string              `json:"clockClass"`
	Declared     string              `json:"declaredClass"`
	AnchorState  string              `json:"anchorState"`
	AnchorReason string              `json:"anchorReason"`
	Synced       bool                `json:"synced"`
	RelativeTime bool                `json:"relativeTime"`
	SigmaThetaNs float64             `json:"sigmaThetaNs"`
	DriftPPM     float64             `json:"driftPpm"`
	BetaShortNs  float64             `json:"betaShortNs"`
	JitterNs     float64             `json:"jitterNs"`
	OutlierPct   float64             `json:"outlierPct"`
	Steps        int                 `json:"steps"`
	Events       []clock.EventRecord `json:"events"`
	Peers        []clock.PeerStat    `json:"peers"`
	SyncedPeers  int                 `json:"syncedPeers"`
}

// StationReport is everything the engine knows about one station's MLAT
// participation.
type StationReport struct {
	PositionSource string                 `json:"positionSource"`
	Clock          *ClockReport           `json:"clock,omitempty"`
	MLAT           *quality.StationReport `json:"mlat,omitempty"`
	// TimedPerMin: timed frames delivered while synced in the last minute;
	// ResultsEligible: the station passes the frame-rate half of the
	// minimum-contribution rule.
	TimedPerMin     int64 `json:"timedPerMin"`
	ResultsEligible bool  `json:"resultsEligible"`
}

// Station returns a station's report (ok false for an unknown station).
func (e *Engine) Station(st model.StationIdx) (StationReport, bool) {
	r := StationReport{PositionSource: e.st.source(st).String()}
	known := e.st.source(st) != PosNone
	if sc := e.filter.Snapshot().Station(st); sc != nil {
		known = true
		r.Clock = &ClockReport{Class: sc.Class.String(), Declared: sc.Declared.String(), AnchorState: sc.AnchorState,
			AnchorReason: sc.AnchorReason, Synced: sc.Synced, RelativeTime: sc.RelativeTime,
			SigmaThetaNs: sc.SigmaThetaNs, DriftPPM: sc.Phi / 1000, BetaShortNs: sc.Beta, JitterNs: sc.JitterNs,
			OutlierPct: sc.OutlierPct, Steps: sc.Steps, Events: append([]clock.EventRecord(nil), sc.Events...),
		}
		peers, nSynced := e.symPeers(st)
		r.Clock.Peers, r.Clock.SyncedPeers = append([]clock.PeerStat(nil), peers...), nSynced
	}
	if q, ok := e.stq.Report(st, e.nowNs.Load()); ok {
		known = true
		r.MLAT = &q
	}
	r.TimedPerMin = e.rates.perMin(st)
	r.ResultsEligible = r.TimedPerMin >= int64(e.o.MinTimedPerMin)
	return r, known
}

// SyncEdge is one entry of the admin sync matrix.
type SyncEdge struct {
	A, B       model.StationIdx
	Count      int     `json:"count"`  // accepted co-observations in the last 30 s
	RMSNs      float64 `json:"rmsNs"`  // RMS leave-one-out residual of A against B
	RelPPB     float64 `json:"relPpb"` // φ_A − φ_B
	SameDomain bool    `json:"sameDomain"`
}

// SyncMatrix returns the admin sync matrix from the latest clock snapshot
// (one entry per station and peer with co-observations), sorted.
func (e *Engine) SyncMatrix() []SyncEdge {
	var out []SyncEdge
	for _, sc := range e.filter.Snapshot().Stations() {
		for _, p := range sc.Peers {
			if p.Count == 0 {
				continue
			}
			out = append(out, SyncEdge{A: sc.Station, B: p.Peer, Count: p.Count, RMSNs: p.RMSNs, RelPPB: p.RelPPB, SameDomain: p.SameDom})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

// ClientStats implements mlatc.StatsSource ("stats" messages to
// mlat-clients): synced peers and the outlier share from the clock
// filter. BadSyncTimeout is 0: the filter re-syncs a stepped clock instead
// of excluding it.
func (e *Engine) ClientStats(st model.StationIdx) (mlatc.ClientStats, bool) {
	sc := e.filter.Snapshot().Station(st)
	if sc == nil {
		return mlatc.ClientStats{}, false
	}
	_, nSynced := e.symPeers(st)
	return mlatc.ClientStats{PeerCount: nSynced, OutlierPercent: sc.OutlierPct}, true
}

var _ mlatc.StatsSource = (*Engine)(nil)

// NoteSeen feeds an mlat-client "seen" list into the planner (mlatc
// Hooks.OnSeen).
func (e *Engine) NoteSeen(st model.StationIdx, addrs []uint32) {
	now := e.o.Now().UnixNano()
	for _, a := range addrs {
		e.planner.NoteHearer(a, st, now)
	}
}

// CRLBMap computes the admin CRLB map (expected σ_h at 3000 ft, FL100 and
// FL300 on a 0.25° grid) from the located stations and their current
// timing σ. It is CPU-heavy (seconds for a continent): call it on station
// changes, not per request.
func (e *Engine) CRLBMap(p quality.CRLBParams) []quality.CRLBCell {
	snap := e.filter.Snapshot()
	var sts []quality.CRLBStation
	for st, ef := range e.st.load().m {
		if !ef.located {
			continue
		}
		sig := ef.class.PriorJitterNs()
		if sc := snap.Station(st); sc != nil && sc.Synced {
			sig = math.Hypot(sc.SigmaThetaNs, sc.JitterNs)
		}
		if !(sig > 0) {
			continue
		}
		sts = append(sts, quality.CRLBStation{Pos: ef.pos, SigmaS: sig * 1e-9})
	}
	if p.Atmosphere == nil {
		a := e.Atmosphere()
		p.Atmosphere = &a
	}
	return quality.CRLBMap(sts, p)
}

// StationTotals returns a station's cumulative solver counts since the
// engine started: accepted fixes in which the station had a used row, and
// its outlier rows (excluded by RAIM or |w| > 4, the rule of the hourly
// station report). The metrics recorder (internal/engine) takes per-minute
// differences.
func (e *Engine) StationTotals(st model.StationIdx) (fixes, outliers uint64, ok bool) {
	return e.stq.Totals(st)
}
