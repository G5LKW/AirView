package latency

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"airview/internal/modes"
	"airview/internal/ntp"
)

// Num is a float64 that marshals NaN / Inf as JSON null (never NaN on the
// wire). Values are written with at most 4 decimals.
type Num float64

// MarshalJSON implements json.Marshaler.
func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, math.Round(f*1e4)/1e4, 'f', -1, 64), nil
}

// UnmarshalJSON implements json.Unmarshaler (null -> NaN).
func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*n = Num(math.NaN())
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// Valid reports whether n is a finite number.
func (n Num) Valid() bool { return !math.IsNaN(float64(n)) && !math.IsInf(float64(n), 0) }

func msNum(ns float64) Num { return Num(ns / 1e6) }

// MinuteSummary is one persisted minute (table latency_minute). Feed "" is
// the all-feeds aggregate (which also carries the propagation, processing
// and T=1 transponder medians); per-feed rows describe the receiver delay.
type MinuteSummary struct {
	T         int64  `json:"t"` // Unix seconds, start of the minute (host clock)
	Feed      string `json:"feed"`
	N         int64  `json:"n"`
	MinUs     int64  `json:"minUs"`
	P50Us     int64  `json:"p50Us"`
	P95Us     int64  `json:"p95Us"`
	MaxUs     int64  `json:"maxUs"`
	MeanUs    int64  `json:"meanUs"`
	JitterUs  int64  `json:"jitterUs"`
	Neg       int64  `json:"neg"`
	Absurd    int64  `json:"absurd"`
	PropP50Us int64  `json:"propP50Us"`
	ProcP50Us int64  `json:"procP50Us"`
	T1N       int64  `json:"t1N"`
	T1P50Us   int64  `json:"t1P50Us"`
}

// FeedMinute is one point of a feed's receiver-delay time series.
type FeedMinute struct {
	T        int64 `json:"t"` // Unix ms, minute start (host clock)
	N        int64 `json:"n"`
	MinMs    Num   `json:"minMs"`
	P50Ms    Num   `json:"p50Ms"`
	P95Ms    Num   `json:"p95Ms"`
	MaxMs    Num   `json:"maxMs"`
	MeanMs   Num   `json:"meanMs"`
	FloorMs  Num   `json:"floorMs"`  // clock-filter floor at the end of the minute
	JitterMs Num   `json:"jitterMs"` // RMS of successive 10 s minima
	Neg      int64 `json:"neg"`
	Absurd   int64 `json:"absurd"`
}

// StageStats summarises one stage over the summary window.
type StageStats struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Count       int64  `json:"count"`
	MinMs       Num    `json:"minMs"`
	P50Ms       Num    `json:"p50Ms"`
	P95Ms       Num    `json:"p95Ms"`
	P99Ms       Num    `json:"p99Ms"`
	MaxMs       Num    `json:"maxMs"`
	MeanMs      Num    `json:"meanMs"`
	// UncertaintyMs is the measurement uncertainty of a single value of this
	// stage (not its spread).
	UncertaintyMs Num  `json:"uncertaintyMs"`
	Exact         bool `json:"exact"` // measured, not estimated
	// Ref is the reference instant of the GPS time of arrival for stages
	// that use it ("receiver"): RefStartOfFrame (default) or RefEndOfFrame
	// (Options.NoFrameEndCorrection). Empty for the other stages.
	Ref string `json:"ref,omitempty"`
}

// Counts are the message and gate counters of a Summary (since start).
type Counts struct {
	Frames           int64 `json:"frames"`
	GPSFrames        int64 `json:"gpsFrames"`
	NoGPS            int64 `json:"noGps"`
	MLATExcluded     int64 `json:"mlatExcluded"`
	Positions        int64 `json:"positions"`
	PositionsRanged  int64 `json:"positionsRanged"`
	PositionsT1      int64 `json:"positionsT1"`
	PositionsT0      int64 `json:"positionsT0"`
	NegativeDelay    int64 `json:"negativeDelay"`
	AbsurdDelay      int64 `json:"absurdDelay"`
	OneSecondAhead   int64 `json:"oneSecondAhead"`     // receiver delay ~ -1 s (legacy day-second range)
	GPSTimeNotUTC    int64 `json:"gpsTimeNotUtc"`      // receiver delay ~ -18 s (GPS time stamps)
	LegacyRange      int64 `json:"legacyRange"`        // GPS ns >= 86400 s seen
	ProcessingGated  int64 `json:"processingGated"`    // Handled before Recv or > 10 s later
	RangeGated       int64 `json:"rangeGated"`         // slant range > 1000 km
	T1OutOfSpec      int64 `json:"t1OutOfSpec"`        // |transmit - ToA| > 150 ms
	T1UnexpectedTC   int64 `json:"t1UnexpectedTc"`     // T=1 outside TC 5, 6, 9, 10, 20, 21
	AgePairs         int64 `json:"agePairs"`           // position pairs used by the age estimator
	AgeTurnsSkipped  int64 `json:"ageTurnsSkipped"`    //
	AgeStaleRepeats  int64 `json:"ageStaleRepeats"`    // identical position while moving
	AgePairsGated    int64 `json:"agePairsGated"`      // |delta age| > 1 s
	FrameEndCorrUs   Num   `json:"frameEndCorrUs"`     // 64/120 us, or 0 when disabled
	ReceiverDelayMin Num   `json:"receiverDelayMinMs"` // gate
	ReceiverDelayMax Num   `json:"receiverDelayMaxMs"` // gate
}

// Summary is the payload of GET /api/timing/latency.
type Summary struct {
	Now           int64 `json:"now"` // Unix ms (host clock of the latest message)
	WindowSec     int   `json:"windowSec"`
	ReceiverSet   bool  `json:"receiverSet"`
	ClockSynced   bool  `json:"clockSynced"`
	ClockOffsetMs Num   `json:"clockOffsetMs"`
	// HostOffsetMs is the host clock error estimated from the receiver's
	// GPS time stamps while the host clock is not NTP-synchronised (host
	// minus GPS UTC, including the receiver -> host delay floor of a few
	// ms); null when synced or not yet estimated. HostOffsetCorrected: it is
	// large (> 100 ms) and the host-side stages (receiver, airToSystem,
	// endToEnd) are corrected by it, i.e. relative (their floor is ~0).
	HostOffsetMs        Num          `json:"hostOffsetMs"`
	HostOffsetCorrected bool         `json:"hostOffsetCorrected"`
	Aircraft            int          `json:"aircraft"`   // ADS-B aircraft with positions in the table
	AircraftT1          int          `json:"aircraftT1"` // ... whose latest position had T=1
	ShareT1             Num          `json:"shareT1"`    // AircraftT1 / Aircraft
	MsgShareT1          Num          `json:"msgShareT1"` // share of positions (since start) with T=1
	Stages              []StageStats `json:"stages"`
	Counts              Counts       `json:"counts"`
	Feeds               []FeedClock  `json:"feeds"`
	Findings            []Finding    `json:"findings"`
}

// Bin is one histogram bin.
type Bin struct {
	LoMs Num   `json:"lo"`
	HiMs Num   `json:"hi"`
	N    int64 `json:"n"`
}

// Histogram is a chart-ready distribution.
type Histogram struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`  // "ms"
	Scale string `json:"scale"` // "log" (signed log bins) or "linear"
	Count int64  `json:"count"`
	Bins  []Bin  `json:"bins"`
}

// AircraftRow is one line of the per-aircraft latency table.
type AircraftRow struct {
	ICAO      string `json:"icao"`
	FirstSeen int64  `json:"firstSeen"` // Unix ms (host)
	LastSeen  int64  `json:"lastSeen"`
	N         int64  `json:"n"`         // ADS-B messages
	Positions int64  `json:"positions"` //
	T1        int64  `json:"t1"`        // positions with T=1
	T0        int64  `json:"t0"`
	TFlag     bool   `json:"tFlag"` // latest position's T bit
	LastTC    int    `json:"lastTc"`
	// Transponder latency (transmit - time of applicability), T=1 only:
	XpdrN      int64 `json:"xpdrN"`
	XpdrMeanMs Num   `json:"xpdrMeanMs"`
	XpdrStdMs  Num   `json:"xpdrStdMs"`
	XpdrMinMs  Num   `json:"xpdrMinMs"`
	XpdrMaxMs  Num   `json:"xpdrMaxMs"`
	// Kinematic position-age estimate (all aircraft with enough pairs):
	AgePairs      int64    `json:"agePairs"`
	AgeJitterMs   Num      `json:"ageJitterMs"`   // sigma of the position age
	AgeModelMs    Num      `json:"ageModelMs"`    // mean age, uniform-register model
	AgeCheckMs    Num      `json:"ageCheckMs"`    // T=1: RMS(pair method - exact)
	PositionAgeMs Num      `json:"positionAgeMs"` // best estimate: exact mean (T=1) else model
	AgeExact      bool     `json:"ageExact"`
	PropMeanUs    Num      `json:"propMeanUs"`
	PropLastUs    Num      `json:"propLastUs"`
	SlantKm       Num      `json:"slantKm"`
	Flags         []string `json:"flags"`
}

// Event is one instant of a Waterfall.
type Event struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	UTC   string `json:"utc"`   // RFC 3339 with ns
	AtMs  Num    `json:"atMs"`  // relative to the time of applicability
	UncMs Num    `json:"uncMs"` // +- uncertainty of this instant
}

// WStage is one stage (interval) of a Waterfall.
type WStage struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Ns     int64  `json:"ns"`
	Ms     Num    `json:"ms"`
	UncMs  Num    `json:"uncMs"`
	Method string `json:"method"` // "exact", "measured", "estimated", "unavailable"
}

// Waterfall breaks the latest position message of one aircraft into stages.
type Waterfall struct {
	ICAO       string   `json:"icao"`
	T          int64    `json:"t"` // Unix ms, true UTC of host reception
	DF         int      `json:"df"`
	TC         int      `json:"tc"`
	TFlag      bool     `json:"tFlag"`
	CPROdd     bool     `json:"cprOdd"`
	Feed       string   `json:"feed"`
	Exact      bool     `json:"exact"` // time of applicability known exactly (T=1)
	SlantKm    Num      `json:"slantKm"`
	ElevDeg    Num      `json:"elevDeg"`
	Events     []Event  `json:"events"`
	Stages     []WStage `json:"stages"`
	TotalMs    Num      `json:"totalMs"` // applicability -> handled
	TotalUncMs Num      `json:"totalUncMs"`
	Notes      []string `json:"notes"`
}

// FeedClock is the NTP/GPS consistency state of one feed.
type FeedClock struct {
	FeedID        string        `json:"feedId"`
	Samples       int64         `json:"samples"`
	Negative      int64         `json:"negative"`
	Absurd        int64         `json:"absurd"`
	LastMs        Num           `json:"lastMs"`
	FloorMs       Num           `json:"floorMs"`       // min of the last 8 x 10 s minima
	JitterMs      Num           `json:"jitterMs"`      // RMS of successive 10 s minima
	DriftUsPerMin Num           `json:"driftUsPerMin"` // LS slope of the 10 s minima (~10 min)
	Intervals     int           `json:"intervals"`
	Status        string        `json:"status"` // "ok", "warn", "bad", "unknown"
	Issues        []string      `json:"issues"`
	Minima        []FilterPoint `json:"minima"` // the 10 s minima, oldest first
	Ref           string        `json:"ref"`    // receiver-delay reference: "startOfFrame" / "endOfFrame"
}

// FilterPoint is one 10 s receiver-delay minimum.
type FilterPoint struct {
	T  int64 `json:"t"` // Unix ms (true UTC), interval start
	Ms Num   `json:"ms"`
}

// NTPCheck is the payload of GET /api/timing/ntpcheck.
type NTPCheck struct {
	NTP NTPInfo `json:"ntp"`
	// Quality: "good" (synced GNSS/PPS stratum 1), "degraded", "bad",
	// "unknown" (no NTP configured).
	Quality string `json:"quality"`
	// OffsetUncertaintyMs bounds the error of the host->UTC correction:
	// delay/2 + root delay/2 + root dispersion + jitter.
	OffsetUncertaintyMs Num         `json:"offsetUncertaintyMs"`
	Issues              []string    `json:"issues"`
	Feeds               []FeedClock `json:"feeds"`
	Explanation         string      `json:"explanation"`
}

// Finding is a plain-language observation.
type Finding struct {
	Level string `json:"level"` // "info", "warn", "bad"
	Key   string `json:"key"`
	Text  string `json:"text"`
}

// SamplerInfo returns an NTP quality getter for SetClock backed by an
// ntp.Sampler.
func SamplerInfo(s *ntp.Sampler) func() NTPInfo {
	if s == nil {
		return nil
	}
	return func() NTPInfo {
		st := s.Status()
		inf := NTPInfo{
			Enabled: st.Enabled, Synced: st.Synced, Server: st.Server, Stratum: st.Stratum,
			RefID: st.RefID, OffsetMs: Num(st.OffsetMs), DelayMs: Num(st.DelayMs),
			JitterMs: Num(st.JitterMs), LastOK: st.LastOK, LastError: st.LastError,
			RootDelayMs: Num(math.NaN()), RootDispMs: Num(math.NaN()),
		}
		if smp, ok := s.Latest(); ok {
			inf.RootDelayMs, inf.RootDispMs, inf.Leap = Num(smp.RootDelayMs), Num(smp.RootDispMs), smp.Leap
		}
		return inf
	}
}

// --- stage metadata -------------------------------------------------------------

var stageLabels = [nStages][2]string{
	stProp:     {"Propagation", "Slant range from the antenna to the aircraft divided by the radio speed (c / (1+N·1e-6), tropospheric refractivity included)."},
	stRecv:     {"Receiver → host", "Host socket read time corrected to true UTC by NTP, minus the Radarcape GPS time of arrival (start of the preamble). Contains Radarcape buffering, TCP/network and OS socket latency."},
	stProc:     {"AirView processing", "Tracker handled minus socket read (both host clock)."},
	stXpdr:     {"Transponder (T=1)", "UTC-coupled transmitters only: transmit time (arrival − propagation) minus the 0.2 s UTC epoch the position applies to. Negative = sent before the epoch (extrapolated ahead)."},
	stAgeDelta: {"Position-age change", "Kinematic pair method: change of the position age between consecutive positions, (GS·Δt − along-track distance)/GS, Δt from GPS arrival times."},
	stAirToSys: {"Air → AirView", "Propagation + receiver→host + processing: transmit time to tracker handled."},
	stEndToEnd: {"End to end (T=1)", "Time of applicability of the position to tracker handled, UTC-coupled transmitters only."},
}

// uncertainties returns the per-value measurement uncertainty (ms) of each
// stage given the NTP offset uncertainty.
func stageUnc(st int, ntpUncMs float64) (float64, bool) {
	const toaUncMs = 0.001 // GPS TOA: ~50 ns tick + frame-end model
	switch st {
	case stProp:
		// Barometric (pressure) altitude stands in for geometric height: QNH
		// and temperature deviations reach several hundred metres at FL300+,
		// which enters the slant range as dh*sin(elevation): up to ~2 us for
		// steep geometry, well under 0.5 us for distant aircraft.
		return 0.002, true
	case stRecv:
		return ntpUncMs + toaUncMs, true
	case stProc:
		return 0.001, true
	case stXpdr:
		return toaUncMs + 0.002, true
	case stAgeDelta:
		return math.NaN(), false
	case stAirToSys:
		return ntpUncMs + toaUncMs + 0.002, true
	case stEndToEnd:
		return ntpUncMs + 0.001, true
	}
	return math.NaN(), false
}

func (a *Analyzer) ntpLocked() NTPInfo {
	if a.ntpInfo != nil {
		return a.ntpInfo()
	}
	inf := NTPInfo{OffsetMs: Num(math.NaN()), DelayMs: Num(math.NaN()), JitterMs: Num(math.NaN()),
		RootDelayMs: Num(math.NaN()), RootDispMs: Num(math.NaN())}
	if a.clk != nil {
		inf.Enabled = true
		inf.Synced = a.clk.Synced()
		inf.OffsetMs = Num(float64(a.clk.Offset()) / 1e6)
	}
	return inf
}

// offsetUncertainty returns the NTP correction error bound in ms (NaN when
// there is no NTP).
func offsetUncertainty(n NTPInfo) float64 {
	if !n.Enabled || !n.Synced {
		return math.NaN()
	}
	u := 0.0
	add := func(v Num, k float64) {
		if v.Valid() {
			u += float64(v) * k
		}
	}
	add(n.DelayMs, 0.5)
	add(n.RootDelayMs, 0.5)
	add(n.RootDispMs, 1)
	add(n.JitterMs, 1)
	return u
}

func (a *Analyzer) mergeWindow() *[nStages]hist {
	for i := range a.scratch {
		a.scratch[i].reset()
	}
	w := int64(len(a.slots))
	for i := range a.slots {
		s := &a.slots[i]
		if s.minute == math.MinInt64 || s.minute <= a.curMinute-w || s.minute > a.curMinute {
			continue
		}
		for k := range s.st {
			a.scratch[k].merge(&s.st[k])
		}
	}
	return &a.scratch
}

// Summary returns the per-stage statistics over the window, counters, the
// per-feed clock state and findings.
func (a *Analyzer) Summary() Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	ntpI := a.ntpLocked()
	unc := offsetUncertainty(ntpI)
	uncForStage := unc
	if math.IsNaN(uncForStage) {
		uncForStage = math.NaN()
	}
	m := a.mergeWindow()
	s := Summary{
		Now:           a.lastNs / 1e6,
		WindowSec:     len(a.slots) * 60,
		ReceiverSet:   a.rx.ok,
		ClockSynced:   a.synced,
		ClockOffsetMs: Num(float64(a.offNs) / 1e6),
		HostOffsetMs:  Num(math.NaN()),
		Stages:        make([]StageStats, 0, nStages),
		Counts:        a.countsLocked(),
	}
	if !a.synced && a.host.have {
		s.HostOffsetMs, s.HostOffsetCorrected = Num(float64(a.host.est)/1e6), a.host.active
	}
	for k := 0; k < nStages; k++ {
		h := &m[k]
		u, _ := stageUnc(k, uncForStage)
		st := StageStats{
			Key: stageKeys[k], Label: stageLabels[k][0], Description: stageLabels[k][1],
			Count: h.n, MinMs: Num(math.NaN()), MaxMs: Num(math.NaN()),
			P50Ms: msNum(h.quantile(0.5)), P95Ms: msNum(h.quantile(0.95)), P99Ms: msNum(h.quantile(0.99)),
			MeanMs: msNum(h.mean()), UncertaintyMs: Num(u), Exact: k != stAgeDelta,
		}
		if k == stRecv {
			st.Ref = a.frameRef()
		}
		if h.n > 0 {
			st.MinMs, st.MaxMs = msNum(float64(h.min)), msNum(float64(h.max))
		}
		s.Stages = append(s.Stages, st)
	}
	for i := range a.ac {
		ac := &a.ac[i]
		if ac.pos == 0 {
			continue
		}
		s.Aircraft++
		if ac.lastTF {
			s.AircraftT1++
		}
	}
	s.ShareT1 = Num(math.NaN())
	if s.Aircraft > 0 {
		s.ShareT1 = Num(float64(s.AircraftT1) / float64(s.Aircraft))
	}
	s.MsgShareT1 = Num(math.NaN())
	if a.c.positions > 0 {
		s.MsgShareT1 = Num(float64(a.c.t1) / float64(a.c.positions))
	}
	s.Feeds = a.feedClocksLocked(false)
	s.Findings = a.findingsLocked(m, ntpI, s.Feeds)
	return s
}

func (a *Analyzer) countsLocked() Counts {
	c := a.c
	fe := 64.0
	if a.opt.NoFrameEndCorrection {
		fe = 0
	}
	return Counts{
		Frames: c.frames, GPSFrames: c.gps, NoGPS: c.noGPS, MLATExcluded: c.mlat,
		Positions: c.positions, PositionsRanged: c.posRange, PositionsT1: c.t1, PositionsT0: c.t0,
		NegativeDelay: c.negD, AbsurdDelay: c.absurdD, OneSecondAhead: c.legacy1s, GPSTimeNotUTC: c.gpsTS,
		LegacyRange: c.legacyRange, ProcessingGated: c.procBad, RangeGated: c.propBad,
		T1OutOfSpec: c.xpdrOutOfSpec, T1UnexpectedTC: c.t1BadTC,
		AgePairs: c.pairs, AgeTurnsSkipped: c.turns, AgeStaleRepeats: c.stale, AgePairsGated: c.pairGated,
		FrameEndCorrUs:   Num(fe),
		ReceiverDelayMin: Num(float64(a.opt.MinReceiverDelay) / 1e6),
		ReceiverDelayMax: Num(float64(a.opt.MaxReceiverDelay) / 1e6),
	}
}

// Histograms returns the window distributions of every stage plus
// "aircraftTransponder" (per-aircraft mean T=1 latency, linear 10 ms bins)
// and "aircraftAgeJitter" (per-aircraft position-age sigma, 5 ms bins).
func (a *Analyzer) Histograms() []Histogram {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.mergeWindow()
	out := make([]Histogram, 0, nStages+2)
	for k := 0; k < nStages; k++ {
		out = append(out, logHistogram(stageKeys[k], stageLabels[k][0], &m[k]))
	}
	xp := Histogram{Key: "aircraftTransponder", Label: "Mean T=1 transponder latency per aircraft", Unit: "ms", Scale: "linear"}
	aj := Histogram{Key: "aircraftAgeJitter", Label: "Position-age jitter per aircraft", Unit: "ms", Scale: "linear"}
	var xc [40]int64 // -200..+200 ms
	var ac [40]int64 // 0..200 ms
	for i := range a.ac {
		s := &a.ac[i]
		if s.xpdr.n >= 3 {
			b := int(math.Floor((s.xpdr.mean + 200) / 10))
			b = max(0, min(39, b))
			xc[b]++
			xp.Count++
		}
		if sig, _, ok := s.ageModel(); ok {
			b := int(math.Floor(sig * 1e3 / 5))
			b = max(0, min(39, b))
			ac[b]++
			aj.Count++
		}
	}
	for i := range xc {
		xp.Bins = append(xp.Bins, Bin{LoMs: Num(-200 + 10*float64(i)), HiMs: Num(-190 + 10*float64(i)), N: xc[i]})
		aj.Bins = append(aj.Bins, Bin{LoMs: Num(5 * float64(i)), HiMs: Num(5*float64(i) + 5), N: ac[i]})
	}
	return append(out, xp, aj)
}

func logHistogram(key, label string, h *hist) Histogram {
	out := Histogram{Key: key, Label: label, Unit: "ms", Scale: "log", Count: h.n, Bins: []Bin{}}
	lo, hi := -1, -1
	for b, c := range h.c {
		if c > 0 {
			if lo < 0 {
				lo = b
			}
			hi = b
		}
	}
	if lo < 0 {
		return out
	}
	for b := lo; b <= hi; b++ {
		l, u := binEdges(b)
		out.Bins = append(out.Bins, Bin{LoMs: msNum(l), HiMs: msNum(u), N: int64(h.c[b])})
	}
	return out
}

// FeedSeries returns the receiver-delay minute series of a feed (oldest
// first) at or after sinceMs (Unix ms; 0 = all). ok is false for an unknown
// feed.
func (a *Analyzer) FeedSeries(feedID string, sinceMs int64) ([]FeedMinute, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.feeds[feedID]
	if !ok {
		if a.otherFeed != nil && feedID == a.otherFeed.id {
			f = a.otherFeed
		} else {
			return nil, false
		}
	}
	out := make([]FeedMinute, 0, f.histN)
	for i := f.histN; i >= 1; i-- {
		p := f.hist[(f.histH-i+len(f.hist))%len(f.hist)]
		if p.T >= sinceMs {
			out = append(out, p)
		}
	}
	return out, true
}

// FeedIDs lists the tracked feeds.
func (a *Analyzer) FeedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.feedList)+1)
	for _, f := range a.feedList {
		out = append(out, f.id)
	}
	if a.otherFeed != nil {
		out = append(out, a.otherFeed.id)
	}
	return out
}

// Seed preloads per-feed minute history (e.g. from latency_minute at start
// up), oldest first. Rows with Feed "" are ignored.
func (a *Analyzer) Seed(rows []MinuteSummary) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range rows {
		if r.Feed == "" {
			continue
		}
		f := a.feed(r.Feed)
		fm := FeedMinute{T: r.T * 1000, N: r.N, Neg: r.Neg, Absurd: r.Absurd,
			MinMs: Num(float64(r.MinUs) / 1e3), P50Ms: Num(float64(r.P50Us) / 1e3), P95Ms: Num(float64(r.P95Us) / 1e3),
			MaxMs: Num(float64(r.MaxUs) / 1e3), MeanMs: Num(float64(r.MeanUs) / 1e3),
			FloorMs: Num(math.NaN()), JitterMs: Num(float64(r.JitterUs) / 1e3)}
		f.hist[f.histH] = fm
		f.histH = (f.histH + 1) % len(f.hist)
		if f.histN < len(f.hist) {
			f.histN++
		}
	}
}

// Aircraft returns the per-aircraft table (aircraft with at least one
// position), most positions first.
func (a *Analyzer) Aircraft() []AircraftRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AircraftRow, 0, len(a.ac))
	for i := range a.ac {
		if a.ac[i].pos > 0 {
			out = append(out, a.ac[i].row())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Positions != out[j].Positions {
			return out[i].Positions > out[j].Positions
		}
		return out[i].ICAO < out[j].ICAO
	})
	return out
}

// AircraftDetail returns one aircraft's row and the Waterfall of its latest
// position message.
func (a *Analyzer) AircraftDetail(icao uint32) (AircraftRow, Waterfall, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i, ok := a.acIdx[icao]
	if !ok {
		return AircraftRow{}, Waterfall{}, false
	}
	s := &a.ac[i]
	unc := offsetUncertainty(a.ntpLocked())
	return s.row(), s.waterfall(unc, a.synced), true
}

func (s *acState) row() AircraftRow {
	r := AircraftRow{
		ICAO: modes.FormatICAO(s.icao), FirstSeen: s.first / 1e6, LastSeen: s.last / 1e6,
		N: s.n, Positions: s.pos, T1: s.t1, T0: s.t0, TFlag: s.lastTF, LastTC: int(s.lastTC),
		XpdrN: s.xpdr.n, XpdrMeanMs: Num(math.NaN()), XpdrStdMs: Num(s.xpdr.std()),
		XpdrMinMs: Num(math.NaN()), XpdrMaxMs: Num(math.NaN()),
		AgePairs: s.ageD.n, AgeJitterMs: Num(math.NaN()), AgeModelMs: Num(math.NaN()),
		AgeCheckMs: Num(math.NaN()), PositionAgeMs: Num(math.NaN()),
		PropMeanUs: Num(math.NaN()), PropLastUs: Num(math.NaN()), SlantKm: Num(math.NaN()),
		Flags: []string{},
	}
	if s.xpdr.n > 0 {
		r.XpdrMeanMs = Num(s.xpdr.mean)
		r.PositionAgeMs, r.AgeExact = Num(s.xpdr.mean), true
	}
	if s.prop.n > 0 {
		r.PropMeanUs = Num(s.prop.mean)
	}
	if s.lastMsg.hasProp {
		r.PropLastUs = Num(s.lastMsg.prop / 1e3)
		r.SlantKm = Num(s.lastMsg.slantM / 1e3)
	}
	if sig, mean, ok := s.ageModel(); ok {
		r.AgeJitterMs, r.AgeModelMs = Num(sig*1e3), Num(mean*1e3)
		if !r.AgeExact {
			r.PositionAgeMs = Num(mean * 1e3)
		}
	}
	if s.pairCheck.n > 0 {
		r.AgeCheckMs = Num(math.Sqrt(s.pairCheck.m2/float64(s.pairCheck.n)+s.pairCheck.mean*s.pairCheck.mean) * 1e3)
	}
	// Flags.
	if s.xpdr.n >= 10 {
		sd := s.xpdr.std()
		// A transmitter that sets T=1 without really synchronising produces
		// values spread uniformly over the +-200 ms epoch ambiguity window
		// (sigma ~115 ms).
		if sd > 80 {
			r.Flags = append(r.Flags, "t1-not-synchronised")
		} else if math.Abs(s.xpdr.mean) > float64(t1ToleranceNs)/1e6 {
			r.Flags = append(r.Flags, "t1-out-of-tolerance")
		}
	}
	if s.outSpec > 0 {
		r.Flags = append(r.Flags, "t1-outliers")
	}
	if s.badTC {
		r.Flags = append(r.Flags, "t1-unexpected-tc")
	}
	if sig, mean, ok := s.ageModel(); ok && s.xpdr.n == 0 {
		if mean > uncompensatedLimitS {
			r.Flags = append(r.Flags, "age-over-0.6s-est")
		}
		if sig > 0.1 {
			r.Flags = append(r.Flags, "age-jitter-high")
		}
	}
	if s.stale >= 3 && s.stale*10 >= s.pos {
		r.Flags = append(r.Flags, "stale-positions")
	}
	return r
}

func fmtUTC(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (s *acState) waterfall(ntpUncMs float64, synced bool) Waterfall {
	lm := &s.lastMsg
	w := Waterfall{ICAO: modes.FormatICAO(s.icao), DF: int(lm.df), TC: int(lm.tc), TFlag: lm.tflag,
		CPROdd: lm.odd, Feed: lm.feed, SlantKm: Num(math.NaN()), ElevDeg: Num(math.NaN()),
		TotalMs: Num(math.NaN()), TotalUncMs: Num(math.NaN()), Events: []Event{}, Stages: []WStage{}, Notes: []string{}}
	if !lm.valid {
		w.Notes = append(w.Notes, "no position message yet")
		return w
	}
	w.T = lm.hostRecv / 1e6
	if lm.hasProp {
		w.SlantKm, w.ElevDeg = Num(lm.slantM/1e3), Num(lm.elev)
	}
	if !lm.hasTOA {
		w.Notes = append(w.Notes, "no GPS time of arrival (not a Radarcape GPS-timestamped feed): only the host-side stages are available")
	}
	ntpU := ntpUncMs
	if math.IsNaN(ntpU) {
		w.Notes = append(w.Notes, "host clock not NTP-corrected: receiver→host delay contains the host clock error")
	}

	// Applicability instant and its uncertainty.
	var applic int64
	var applicUnc float64 // ms
	method := "unavailable"
	propNs := int64(math.Round(lm.prop))
	tx := lm.toa - propNs
	switch {
	case lm.hasL:
		applic, applicUnc, method = lm.epoch, 0, "exact"
		w.Exact = true
	case lm.hasTOA && lm.hasProp:
		sig, mean, ok := s.ageModel()
		if ok {
			est := mean
			k := 0.0
			if v := s.ageD.variance(); v > 0 && !math.IsNaN(lm.ageDelta) {
				k = math.Min(0.5, sig*sig/v)
				est = mean + k*(lm.ageDelta-s.ageD.mean)
			}
			applic = tx - int64(math.Round(est*1e9))
			applicUnc = math.Sqrt(math.Max(0, sig*sig*(1-k))+mean*mean/3) * 1e3 // incl. model doubt on the mean
			method = "estimated"
			w.Notes = append(w.Notes, "T=0: time of applicability estimated from the kinematic position-age model (constant latency is not observable without T=1); uncertainty includes the model doubt")
		} else {
			applic, applicUnc, method = tx, 200, "assumed"
			w.Notes = append(w.Notes, "T=0 and too few position pairs: applicability assumed equal to transmit time (DO-260A: T=0 data is never more than 200 ms from the time it is held)")
		}
	}
	ref := applic
	hasRef := method != "unavailable"
	ev := func(key, label string, t int64, unc float64) {
		e := Event{Key: key, Label: label, UTC: fmtUTC(t), AtMs: Num(math.NaN()), UncMs: Num(unc)}
		if hasRef {
			e.AtMs = msNum(float64(t - ref))
		}
		w.Events = append(w.Events, e)
	}
	stg := func(key, label string, ns int64, unc float64, m string) {
		w.Stages = append(w.Stages, WStage{Key: key, Label: label, Ns: ns, Ms: msNum(float64(ns)), UncMs: Num(unc), Method: m})
	}
	if hasRef {
		ev("applicability", "Time of applicability", applic, applicUnc)
	}
	if lm.hasTOA && lm.hasProp {
		ev("transmit", "Transmitted", tx, 0.0015)
	}
	if lm.hasTOA {
		ev("arrival", "Arrived at antenna (GPS)", lm.toa, 0.001)
	}
	ev("received", "Read from socket (UTC)", lm.hostRecv, ntpU)
	if lm.hasHandled {
		ev("handled", "Handled by tracker (UTC)", lm.handled, ntpU)
	}
	if hasRef && lm.hasTOA && lm.hasProp {
		stg("positionAge", "Position age at transmit", tx-applic, applicUnc, method)
	} else {
		stg("positionAge", "Position age at transmit", 0, math.NaN(), "unavailable")
	}
	if lm.hasProp && lm.hasTOA {
		stg("propagation", "Propagation", propNs, 0.0005, "exact")
	} else if lm.hasProp {
		stg("propagation", "Propagation", propNs, 0.0005, "computed")
	}
	if lm.hasTOA {
		m := "measured"
		if lm.recvGated {
			m = "gated"
		}
		stg("receiver", "Receiver → host", lm.hostRecv-lm.toa, ntpU+0.001, m)
	}
	if lm.hasHandled {
		stg("processing", "AirView processing", lm.handled-lm.hostRecv, 0.001, "measured")
	}
	if hasRef && lm.hasHandled && lm.hasTOA && !lm.recvGated {
		w.TotalMs = msNum(float64(lm.handled - applic))
		w.TotalUncMs = Num(math.Sqrt(applicUnc*applicUnc+ntpU*ntpU) + 0.002)
		if math.IsNaN(ntpU) {
			w.TotalUncMs = Num(math.NaN())
		}
	}
	if lm.recvGated {
		w.Notes = append(w.Notes, "receiver→host delay outside the plausibility gate: check host clock / NTP / receiver time base")
	}
	if !synced && !math.IsNaN(ntpU) {
		w.Notes = append(w.Notes, "NTP not currently synchronised")
	}
	return w
}

// feedClocksLocked evaluates every feed's clock filter.
func (a *Analyzer) feedClocksLocked(withMinima bool) []FeedClock {
	list := a.feedList
	if a.otherFeed != nil {
		list = append(list[:len(list):len(list)], a.otherFeed)
	}
	out := make([]FeedClock, 0, len(list))
	for _, f := range list {
		floor, jit, drift, n := f.filterStats()
		fc := FeedClock{FeedID: f.id, Samples: f.total, Negative: f.neg, Absurd: f.absurd, Ref: a.frameRef(),
			LastMs: Num(math.NaN()), FloorMs: msNum(floor), JitterMs: msNum(jit),
			DriftUsPerMin: Num(drift), Intervals: n, Status: "unknown", Issues: []string{}, Minima: []FilterPoint{}}
		if f.total > 0 {
			fc.LastMs = msNum(float64(f.lastD))
		}
		if n > 0 {
			fc.Status = "ok"
			worst := func(s string) {
				if s == "bad" || fc.Status == "ok" {
					fc.Status = s
				}
			}
			if floor < -0.2e6 {
				worst("bad")
				fc.Issues = append(fc.Issues, fmt.Sprintf("receiver delay floor is negative (%.2f ms): the NTP-corrected host clock is behind GPS time - check the NTP offset, an asymmetric network path to the NTP server, or a receiver time-base offset", floor/1e6))
			}
			if jit > 2e6 {
				worst("warn")
				fc.Issues = append(fc.Issues, fmt.Sprintf("floor jitter %.2f ms: the host clock wanders against GPS (NTP corrections stepping, power saving, overloaded host or a congested link)", jit/1e6))
			}
			if !math.IsNaN(drift) && math.Abs(drift) > 50 {
				worst("warn")
				fc.Issues = append(fc.Issues, fmt.Sprintf("floor drifting %.0f us/min (%.2f ppm): the host clock frequency is not disciplined between NTP polls", drift, drift/60))
			}
		}
		if f.absurd > 0 && f.absurd*20 > f.total {
			fc.Status = "bad"
			fc.Issues = append(fc.Issues, "many receiver delays are implausible (whole seconds or more): receiver time base or host date is wrong")
		}
		if withMinima {
			var buf [filterRing + 1]ivMin
			for _, x := range f.minima(buf[:0], filterRing) {
				fc.Minima = append(fc.Minima, FilterPoint{T: x.t / 1e6, Ms: msNum(float64(x.v))})
			}
		}
		out = append(out, fc)
	}
	return out
}

var gnssRefIDs = map[string]bool{"GPS": true, "PPS": true, "GNSS": true, "GPSD": true, "GNS": true,
	"GAL": true, "GLO": true, "BDS": true, "NMEA": true, "PPS0": true, "PPS1": true, "SHM": true}

// NTPCheck returns the NTP server quality and the GPS/NTP consistency state.
func (a *Analyzer) NTPCheck() NTPCheck {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.ntpLocked()
	c := NTPCheck{NTP: n, Quality: "good", Issues: []string{}, Feeds: a.feedClocksLocked(true),
		OffsetUncertaintyMs: Num(offsetUncertainty(n)),
		Explanation: "The Radarcape time-stamps every frame from its GPS receiver and the NTP server is GPS-locked too, so " +
			"(host receive time + NTP offset) - GPS arrival time is a direct measurement of the receiver→host latency. " +
			"Its minimum over 10 s intervals (like NTP's clock filter) is the irreducible latency; it should be small, " +
			"positive and steady. A negative floor means the corrected host clock is behind GPS; jitter or drift of the " +
			"floor exposes host clock wander between NTP polls."}
	bad := func(s string) { c.Quality = "bad"; c.Issues = append(c.Issues, s) }
	deg := func(s string) {
		if c.Quality == "good" {
			c.Quality = "degraded"
		}
		c.Issues = append(c.Issues, s)
	}
	switch {
	case !n.Enabled || (a.ntpInfo != nil && n.Server == ""):
		c.Quality = "unknown"
		c.Issues = append(c.Issues, "no NTP server configured (Settings → NTP): the receiver→host delay includes the host clock error")
	case !n.Synced:
		bad("NTP is not synchronised (no recent good sample)")
	default:
		if n.Leap == 3 {
			bad("NTP server announces 'unsynchronised' (leap indicator 3)")
		}
		if n.Stratum != 1 && a.ntpInfo != nil {
			deg(fmt.Sprintf("NTP server is stratum %d, not a GPS-locked stratum-1", n.Stratum))
		} else if a.ntpInfo != nil && !gnssRefIDs[strings.ToUpper(strings.TrimSpace(n.RefID))] {
			deg(fmt.Sprintf("stratum-1 reference id %q is not a GNSS/PPS source", n.RefID))
		}
		if n.RootDispMs.Valid() && n.RootDispMs > 1 {
			deg(fmt.Sprintf("root dispersion %.2f ms is high for a GPS stratum-1", float64(n.RootDispMs)))
		}
		if n.DelayMs.Valid() && n.DelayMs > 5 {
			deg(fmt.Sprintf("NTP round-trip delay %.2f ms: the offset may be wrong by up to half of it if the path is asymmetric", float64(n.DelayMs)))
		}
		if n.JitterMs.Valid() && n.JitterMs > 1 {
			deg(fmt.Sprintf("NTP jitter %.2f ms", float64(n.JitterMs)))
		}
	}
	for _, f := range c.Feeds {
		switch f.Status {
		case "bad":
			c.Quality = "bad"
		case "warn":
			if c.Quality == "good" {
				c.Quality = "degraded"
			}
		}
		for _, is := range f.Issues {
			c.Issues = append(c.Issues, f.FeedID+": "+is)
		}
	}
	if a.c.gpsTS > 0 {
		bad("receiver time stamps are ~18 s ahead: the Radarcape is in GPS-time mode (UTC offset not yet received) - see systemstatus 'TimeGPS'")
	}
	if a.c.legacy1s > 0 {
		bad("receiver time stamps are ~1 s ahead: legacy day-second range (1..86400) without the status frame - enable the Radarcape 'true UTC' range or let AirView see the status frames")
	}
	return c
}

// Findings returns plain-language findings for the current window.
func (a *Analyzer) Findings() []Finding {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.mergeWindow()
	return a.findingsLocked(m, a.ntpLocked(), a.feedClocksLocked(false))
}

func (a *Analyzer) findingsLocked(m *[nStages]hist, n NTPInfo, feeds []FeedClock) []Finding {
	out := []Finding{}
	add := func(level, key, format string, args ...any) {
		out = append(out, Finding{Level: level, Key: key, Text: fmt.Sprintf(format, args...)})
	}
	if a.c.frames == 0 {
		add("info", "nodata", "No messages analysed yet.")
		return out
	}
	if !a.rx.ok {
		add("warn", "receiver", "Receiver position not set: propagation delay and the transponder (T=1) analysis need it.")
	}
	if a.c.gps == 0 {
		add("warn", "nogps", "No GPS time stamps: the feed is not a Radarcape in GPS time-stamp mode, so only host-side stages can be measured.")
	}
	if h := &m[stProp]; h.n > 0 {
		add("info", "propagation", "Radio propagation takes %.0f µs at the median (%.0f–%.0f µs): light needs 3.34 µs per km.",
			h.quantile(0.5)/1e3, float64(h.min)/1e3, float64(h.max)/1e3)
	}
	hostBad := !a.synced && a.host.have && a.host.active
	if hostBad {
		if a.host.ambiguous1s {
			add("bad", "hostclock", "Host clock offset unknown/large (no NTP): T=1 latency not measurable. The receiver's GPS time stamps are %.3f s ahead of the host clock: either the host clock is about 1 s behind or the receiver uses the legacy day-second range (1..86400) - an odd second flips the 0.2 s epoch parity, so T=1 positions are kept out until NTP is configured.",
				-float64(a.host.est)/1e9)
		} else {
			add("bad", "hostclock", "Host clock offset unknown/large (no NTP): T=1 latency not measurable end to end. The host clock is about %+.3f s off the receiver's GPS time (estimated from the GPS time stamps). The transponder (T=1) stage uses the GPS time stamps only and stays exact; receiver→host and end-to-end figures are shown relative to the estimate. This is not a receiver fault - configure NTP.",
				float64(a.host.est)/1e9)
		}
	}
	if h := &m[stRecv]; h.n > 0 && hostBad {
		add("info", "receiver", "Receiver→host delay relative to the estimated host clock offset: %.2f ms above its floor at the median (95%% below %.2f ms). The absolute delay needs NTP.",
			(h.quantile(0.5)-float64(h.min))/1e6, (h.quantile(0.95)-float64(h.min))/1e6)
	} else if h.n > 0 {
		add("info", "receiver", "From the antenna to AirView's socket takes %.2f ms at the median (floor %.2f ms, 95%% below %.2f ms).",
			h.quantile(0.5)/1e6, float64(h.min)/1e6, h.quantile(0.95)/1e6)
		if a.c.negD > 0 {
			add("warn", "negative", "%d messages appeared to arrive at the host before the antenna received them: the host clock or NTP correction is off by at least that much.", a.c.negD)
		}
	}
	if h := &m[stProc]; h.n > 0 {
		add("info", "processing", "AirView's own processing adds %.3f ms at the median (p99 %.3f ms).", h.quantile(0.5)/1e6, h.quantile(0.99)/1e6)
	}
	if a.c.positions > 0 {
		share := float64(a.c.t1) / float64(a.c.positions) * 100
		add("info", "t1share", "%.1f%% of ADS-B positions carry T=1 (UTC-coupled time of applicability); field studies report about 1%% of aircraft.", share)
	}
	if h := &m[stXpdr]; h.n > 0 {
		add("info", "transponder", "UTC-coupled transmitters send their position %.0f ms after its 0.2 s epoch at the median (p05..p95 %.0f..%.0f ms; DO-260A tolerance ±150 ms).",
			h.quantile(0.5)/1e6, h.quantile(0.05)/1e6, h.quantile(0.95)/1e6)
	}
	nonSync, est, overLimit := 0, 0, 0
	var ages []float64
	for i := range a.ac {
		s := &a.ac[i]
		if s.xpdr.n >= 10 && s.xpdr.std() > 80 {
			nonSync++
		}
		if sig, mean, ok := s.ageModel(); ok && s.xpdr.n == 0 {
			est++
			ages = append(ages, sig)
			if mean > uncompensatedLimitS {
				overLimit++
			}
		}
	}
	if nonSync > 0 {
		add("warn", "t1nonsync", "%d aircraft set T=1 but their positions are not synchronised to the 0.2 s UTC epochs (values spread over the whole ±200 ms window) — a known non-compliance.", nonSync)
	}
	if est > 0 {
		sort.Float64s(ages)
		add("info", "agemodel", "For %d T=0 aircraft the kinematic method gives a median position-age jitter of %.0f ms (a register refreshed every 200 ms gives ≈58 ms). The constant part of the age is not observable without T=1; the model value assumes uncompensated register sampling.",
			est, ages[len(ages)/2]*1e3)
	}
	if overLimit > 0 {
		add("warn", "agelimit", "%d aircraft have an estimated position age above the 0.6 s uncompensated-latency limit (14 CFR 91.227).", overLimit)
	}
	if a.c.mlat > 0 {
		add("info", "mlat", "%d MLAT results were excluded: their time stamps are synthetic.", a.c.mlat)
	}
	switch {
	case !n.Enabled:
		add("warn", "ntp", "No NTP server configured: receiver→host delays include the host clock error.")
	case !n.Synced:
		add("bad", "ntp", "NTP is not synchronised: receiver→host delays are unreliable.")
	default:
		if u := offsetUncertainty(n); !math.IsNaN(u) {
			add("info", "ntp", "Host clock corrected by NTP (offset %.3f ms, uncertainty ±%.3f ms).", float64(n.OffsetMs), u)
		}
	}
	for _, f := range feeds {
		for _, is := range f.Issues {
			add("warn", "feed:"+f.FeedID, "%s: %s", f.FeedID, is)
		}
	}
	if a.c.gpsTS > 0 {
		add("bad", "gpstime", "Some time stamps are ~18 s ahead of UTC: the receiver is using GPS time, not UTC.")
	}
	if a.c.legacy1s > 0 {
		add("bad", "legacy", "Some time stamps are 1 s ahead: legacy day-second numbering.")
	}
	if a.c.absurdD > 0 {
		add("warn", "absurd", "%d receiver delays were implausible and excluded.", a.c.absurdD)
	}
	return out
}
