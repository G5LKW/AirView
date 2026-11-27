package mlat

import (
	"math"
	"sync"
	"sync/atomic"

	"airview/internal/mlat/clock"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// PosSource is where a station's effective MLAT antenna position comes
// from (DESIGN §10.1 resolution order).
type PosSource uint8

// Position sources, best first.
const (
	PosNone      PosSource = iota // not located: never in the solver geometry
	PosGNSS                       // the receiver's own GNSS (Radarcape type '5', ShareGNSS, host): auto-confirmed
	PosConfirmed                  // a position the owner confirmed ("Use this location")
	PosSurvey                     // a reverse-MLAT survey used automatically (AutoUseSurvey, σ_h small enough)
	PosDeclared                   // the owner's declared position (used immediately, cross-checked by the survey)
)

var posNames = [...]string{"none", "gnss", "confirmed", "survey", "declared"}

// String returns the source name.
func (s PosSource) String() string {
	if int(s) < len(posNames) {
		return posNames[s]
	}
	return "?"
}

// Position is an antenna position: ECEF metres (height above the WGS-84
// ellipsoid; convert MSL with internal/geoid first) and its 1σ (m).
type Position struct {
	Valid  bool
	ECEF   geo.Vec3
	SigmaM float64
}

// StationInfo is what the registry tells the engine about a station.
type StationInfo struct {
	// Class is the declared clock class (the clock filter may only
	// downgrade it).
	Class model.ClockClass
	// Declared, GNSS and Confirmed are the known positions (any may be
	// absent). Default σ when SigmaM is 0: GNSS 5 m, confirmed 10 m,
	// declared 30 m.
	Declared  Position
	GNSS      Position
	Confirmed Position
	// Suspect marks a declared position contradicted by the survey
	// (DESIGN §10.3); DiscrepancyM inflates its σ, which effectively
	// removes it from the solver geometry.
	Suspect      bool
	DiscrepancyM float64
	// NoStatus: GPS station without Radarcape status frames (mlat-client
	// radarcape_gps; anchor condition (a) is skipped).
	NoStatus bool
	// Flagged: admin flag (never an anchor).
	Flagged bool
}

// Survey is a reverse-MLAT result for a station (phase 3, mlat/survey).
type Survey struct {
	Valid   bool
	ECEF    geo.Vec3
	SigmaHM float64
	SigmaVM float64
	// Stable is the survey's own stability verdict (DESIGN §10.2).
	Stable bool
	// Suspect: the survey failed its consistency checks.
	Suspect bool
	// OwnerRejected: the owner overrode the automatic use.
	OwnerRejected bool
}

// effective is the resolved MLAT position of a station.
type effective struct {
	pos      geo.Vec3
	sigmaM   float64
	located  bool
	src      PosSource
	class    model.ClockClass
	noStatus bool
	flagged  bool
	// suspect: a declared position the survey contradicted (never used
	// for ADS-B verification).
	suspect bool
}

type stEntry struct {
	info   StationInfo
	survey Survey
	eff    effective
	jitter float64 // learned jitter fed to the clock filter (0 = prior)
	// rejected: the owner's standing OverrideSurvey rejection
	rejected bool
}

// stationTable keeps the registry's stations and publishes an immutable
// snapshot of the effective positions for the hot path.
type stationTable struct {
	mu   sync.Mutex
	m    map[model.StationIdx]*stEntry
	snap atomic.Pointer[stSnap]

	autoUse    bool
	autoMaxSig float64
	// suspectMin is survey.SuspectMinM (default 100 m): a known position
	// that moves by more than max(suspectMin, 3σ_h of the survey)
	// invalidates the stored survey (finding MLAT-R2).
	suspectMin float64
}

// stSnap is an immutable map of effective positions.
type stSnap struct {
	m map[model.StationIdx]effective
}

func newStationTable(autoUse bool, autoMaxSig float64) *stationTable {
	t := &stationTable{m: map[model.StationIdx]*stEntry{}, autoUse: autoUse, autoMaxSig: autoMaxSig, suspectMin: 100}
	t.snap.Store(&stSnap{m: map[model.StationIdx]effective{}})
	return t
}

func (t *stationTable) load() *stSnap { return t.snap.Load() }

func (s *stSnap) get(st model.StationIdx) (effective, bool) {
	e, ok := s.m[st]
	return e, ok
}

func sigmaOr(s, d float64) float64 {
	if s > 0 {
		return s
	}
	return d
}

// resolve applies the DESIGN §10.1 order: GNSS, owner-confirmed, the
// survey when AutoUseSurvey allows it (σ_h <= AutoUseMaxSigmaM, stable,
// not suspect, not rejected by the owner), then the declared position
// (with its σ inflated by a survey discrepancy when suspect).
func (t *stationTable) resolve(e *stEntry) effective {
	in := &e.info
	out := effective{class: in.Class, noStatus: in.NoStatus, flagged: in.Flagged}
	sv := &e.survey
	switch {
	case in.GNSS.Valid:
		out.pos, out.sigmaM, out.src = in.GNSS.ECEF, sigmaOr(in.GNSS.SigmaM, 5), PosGNSS
	case in.Confirmed.Valid:
		out.pos, out.sigmaM, out.src = in.Confirmed.ECEF, sigmaOr(in.Confirmed.SigmaM, 10), PosConfirmed
	case t.autoUse && sv.Valid && sv.Stable && !sv.Suspect && !sv.OwnerRejected && !e.rejected && sv.SigmaHM > 0 && sv.SigmaHM <= t.autoMaxSig:
		out.pos, out.sigmaM, out.src = sv.ECEF, math.Hypot(sv.SigmaHM, sv.SigmaVM)/math.Sqrt2, PosSurvey
	case in.Declared.Valid:
		out.pos, out.sigmaM, out.src = in.Declared.ECEF, sigmaOr(in.Declared.SigmaM, 30), PosDeclared
		if in.Suspect {
			out.sigmaM = math.Hypot(out.sigmaM, math.Max(in.DiscrepancyM, 100))
			out.suspect = true
		}
	default:
		return out
	}
	out.located = true
	return out
}

// publishLocked rebuilds the snapshot. Caller holds mu.
func (t *stationTable) publishLocked() {
	m := make(map[model.StationIdx]effective, len(t.m))
	for k, e := range t.m {
		m[k] = e.eff
	}
	t.snap.Store(&stSnap{m: m})
}

// set updates a station and returns its clock-filter description and the
// position change (changed: the source changed). ok is false when a survey
// arrives for a station the table does not know (removed or never
// registered): it is ignored, so a survey job that finishes after
// RemoveStation cannot bring the station back, located, into the clock
// filter (finding MLAT-R6).
//
// A survey result replaces the stored one only when it is at least as
// conclusive: an inconclusive (valid but not stable, not suspect) result
// never replaces a stable survey, so a quiet-traffic re-check cannot drop a
// good position back to a suspect declared one (finding MLAT-R3). A
// suspect result always replaces it (safety), and Survey{} clears it.
//
// A registry update that moves the declared, GNSS or confirmed position by
// more than max(suspectMin, 3σ_h) drops the stored survey: it describes
// where the antenna was, not where the owner says it is now (finding
// MLAT-R2: the old survey kept priority over the new declared position
// indefinitely when the new survey was inconclusive).
func (t *stationTable) set(st model.StationIdx, in *StationInfo, sv *Survey) (ci clock.StationInfo, ch PositionChange, changed, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[st]
	if e == nil {
		if in == nil {
			return clock.StationInfo{}, PositionChange{}, false, false
		}
		e = &stEntry{}
		t.m[st] = e
	}
	if in != nil {
		if e.survey.Valid && t.moved(&e.info, in, e.survey.SigmaHM) {
			e.survey = Survey{}
		}
		e.info = *in
	}
	if sv != nil && keepSurvey(&e.survey, sv) {
		sv = nil
	}
	if sv != nil {
		e.survey = *sv
	}
	ci, ch, changed = t.apply(st, e)
	return ci, ch, changed, true
}

// moved reports whether a registry update moves one of the station's known
// positions by more than max(suspectMin, 3σ_h) (or adds or drops one), so
// that a survey made at the old place no longer applies.
func (t *stationTable) moved(old, in *StationInfo, sigmaH float64) bool {
	lim := math.Max(t.suspectMin, 3*sigmaH)
	far := func(a, b Position) bool {
		if a.Valid != b.Valid {
			return a.Valid // a position dropped; a new one is checked by the survey
		}
		return a.Valid && a.ECEF.Dist(b.ECEF) > lim
	}
	return far(old.Declared, in.Declared) || far(old.GNSS, in.GNSS) || far(old.Confirmed, in.Confirmed)
}

// keepSurvey reports whether the stored survey cur stays in force instead
// of the new result nw: a stable, non-suspect survey is only replaced by
// another stable result, a suspect result or an explicit clear.
func keepSurvey(cur, nw *Survey) bool {
	if !cur.Valid || !cur.Stable || cur.Suspect {
		return false
	}
	if !nw.Valid || nw.Stable || nw.Suspect {
		return false
	}
	return true
}

// apply re-resolves e and publishes. Caller holds mu.
func (t *stationTable) apply(st model.StationIdx, e *stEntry) (clock.StationInfo, PositionChange, bool) {
	old := e.eff.src
	e.eff = t.resolve(e)
	t.publishLocked()
	ch := PositionChange{Station: st, Old: old, New: e.eff.src, AutoSurvey: e.eff.src == PosSurvey}
	if e.eff.located {
		ch.ECEF, ch.SigmaM = e.eff.pos, e.eff.sigmaM
	}
	return t.clockInfo(e), ch, old != e.eff.src
}

// override sets the owner's decision on the automatic survey use. It
// reports ok = false for an unknown station.
func (t *stationTable) override(st model.StationIdx, reject bool) (clock.StationInfo, PositionChange, bool, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[st]
	if e == nil {
		return clock.StationInfo{}, PositionChange{}, false, false
	}
	e.rejected = reject
	ci, ch, changed := t.apply(st, e)
	return ci, ch, changed, true
}

// surveyOf returns a station's last survey.
func (t *stationTable) surveyOf(st model.StationIdx) (Survey, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.m[st]; e != nil {
		sv := e.survey
		sv.OwnerRejected = sv.OwnerRejected || e.rejected
		return sv, true
	}
	return Survey{}, false
}

func (t *stationTable) clockInfo(e *stEntry) clock.StationInfo {
	// A station without a confirmed-quality position (only declared and
	// suspect) still syncs: the filter needs the position. Its σ goes to
	// the filter too (PosSigmaM): the inflated σ of a suspect declared
	// position inflates its observation variance there, as it keeps the
	// station out of the solver geometry (DESIGN §10.1; finding MLAT-R4).
	return clock.StationInfo{Class: e.eff.class, Pos: e.eff.pos, Located: e.eff.located,
		NoStatus: e.eff.noStatus, Flagged: e.eff.flagged, JitterNs: e.jitter, PosSigmaM: e.eff.sigmaM}
}

func (t *stationTable) remove(st model.StationIdx) {
	t.mu.Lock()
	delete(t.m, st)
	t.publishLocked()
	t.mu.Unlock()
}

// setJitter records a learned jitter; it returns the clock info when the
// value changed by more than 10 %.
func (t *stationTable) setJitter(st model.StationIdx, j float64) (clock.StationInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[st]
	if e == nil || (e.jitter > 0 && math.Abs(j/e.jitter-1) < 0.1) {
		return clock.StationInfo{}, false
	}
	e.jitter = j
	return t.clockInfo(e), true
}

// clockInfoOf returns a station's clock-filter description.
func (t *stationTable) clockInfoOf(st model.StationIdx) (clock.StationInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[st]
	if e == nil {
		return clock.StationInfo{}, false
	}
	return t.clockInfo(e), true
}

// source returns a station's effective position source.
func (t *stationTable) source(st model.StationIdx) PosSource {
	if e, ok := t.load().get(st); ok {
		return e.src
	}
	return PosNone
}

// all returns the indices of every known station.
func (t *stationTable) all() []model.StationIdx {
	s := t.load()
	out := make([]model.StationIdx, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	return out
}
