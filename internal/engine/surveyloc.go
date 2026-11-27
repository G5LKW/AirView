package engine

// Survey-driven station location (DESIGN §10.1, §19.3; WP-W3-wire).
//
// The MLAT engine applies AutoUseSurvey itself (internal/mlat stations.go:
// a stable, non-suspect survey with σ_h <= aggregator.mlat.autoUseMaxSigmaM,
// not rejected by the owner, becomes the station's MLAT position). The
// registry must follow, otherwise a feeder that the network located stays
// "not located" everywhere else (station map, range, Locate Me recording,
// owner view). reconcileSurveyLocation is that bridge, run from the MLAT
// station sync loop (one goroutine, so the writes are ordered):
//
//   - the engine uses the survey for a station that has no position of its
//     own (not located, or located earlier by this rule) → the registry's
//     effective position becomes the survey position with source
//     "estimated" and status "estimated" (the "estimated" badge of §10.1);
//   - the engine stops using it (owner rejected it, a later survey was
//     unstable or suspect, AutoUseSurvey switched off) → the station is
//     unlocated again;
//   - an unlocated station with a running survey shows "estimating".
//
// Declared, GNSS, confirmed and suspect stations are never touched: their
// position is the owner's or the receiver's. The rule is idempotent: it
// compares the registry with the engine's current verdict on every pass,
// so the order of the engine's notifications does not matter.
//
// Locate Me v2 seeds the survey: for stations without any position, the
// approximate position from the station's own reception (guess_samples,
// analytics.LocateMe) is computed in the background and handed to the
// survey as its search hint (survey.Station.Hint). One estimate costs
// about 2 s of CPU, so they run one at a time, at most surveyHintBatch per
// pass and at most once an hour per station.

import (
	"context"
	"math"
	"sync"
	"time"

	"airview/internal/analytics"
	ageo "airview/internal/geo"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/registry"
)

// surveyLocSource is the part of mlat.Engine the reconciliation reads.
type surveyLocSource interface {
	PositionSource(st model.StationIdx) mlat.PosSource
	SurveyOf(st model.StationIdx) (mlat.Survey, bool)
}

// surveyLocAction is what reconcileSurveyLocation does to one station.
type surveyLocAction uint8

const (
	locKeep       surveyLocAction = iota
	locBySurvey                   // set the effective position from the survey
	locUnlocate                   // the survey position is no longer used
	locEstimating                 // unlocated, a survey is running
	locUnknown                    // unlocated, no survey running any more
)

// locatedBySurvey reports whether st's effective position was set by
// this rule (and not by the owner, the feeder or the receiver).
func locatedBySurvey(st *registry.Station) bool {
	return st.Located && st.Effective.Source == model.LocSrcEstimated && st.LocStatus == model.LocEstimated
}

// ownPosition reports whether the station has a position of its own,
// which the survey never replaces.
func ownPosition(st *registry.Station) bool {
	if st.Located && !locatedBySurvey(st) {
		return true
	}
	switch st.LocStatus {
	case model.LocDeclared, model.LocGNSS, model.LocConfirmed, model.LocSuspect:
		return true
	}
	return false
}

// surveyMoveM is the least a used survey position must move before the
// registry is rewritten (a new survey of the same station).
const surveyMoveM = 1.0

// surveyRefineSigmas: PRIV-A4. Periodic check surveys refine the position
// by amounts within their own uncertainty. Every registry write
// re-evaluates the station's public pin, and a station a few tens of
// metres from a cell edge would hop between two cells with every
// refinement, which tells the public that one coordinate of the antenna is
// next to that grid line. So the effective position follows the survey
// only when it moved by more than surveyRefineSigmas times the larger 1σ
// (and at least surveyMoveM), or when the survey's σ halved or doubled.
// (The public point keeps its own hysteresis in the registry: HOOKS.md.)
const surveyRefineSigmas = 3.0

// surveyMoved reports whether a new used survey differs enough from the
// survey position e to rewrite it.
func surveyMoved(e model.StationGeo, sv mlat.Survey) bool {
	moved := geo.ToECEF(e.Lat, e.Lon, e.HAE).Sub(sv.ECEF).Norm()
	limit := math.Max(surveyMoveM, surveyRefineSigmas*math.Max(e.SigmaM, sv.SigmaHM))
	if moved >= limit {
		return true
	}
	if e.SigmaM > 0 && (sv.SigmaHM < 0.5*e.SigmaM || sv.SigmaHM > 2*e.SigmaM) {
		return true
	}
	return e.SigmaM <= 0 && sv.SigmaHM > 0
}

// decideSurveyLoc is the pure decision of the reconciliation.
func decideSurveyLoc(st *registry.Station, src mlat.PosSource, sv mlat.Survey, running bool) (surveyLocAction, model.StationGeo) {
	if st.Idx == model.HostStation || ownPosition(st) {
		return locKeep, model.StationGeo{}
	}
	if src == mlat.PosSurvey && sv.Valid {
		llh := geo.FromECEF(sv.ECEF)
		g := model.StationGeo{Lat: llh.Lat, Lon: llh.Lon, HAE: llh.H, SigmaM: sv.SigmaHM, Source: model.LocSrcEstimated}
		if locatedBySurvey(st) && !surveyMoved(st.Effective, sv) {
			return locKeep, model.StationGeo{}
		}
		return locBySurvey, g
	}
	if locatedBySurvey(st) {
		return locUnlocate, model.StationGeo{}
	}
	// Not located, no usable survey.
	switch {
	case running && st.LocStatus != model.LocEstimating:
		return locEstimating, model.StationGeo{}
	case !running && st.LocStatus == model.LocEstimating:
		return locUnknown, model.StationGeo{}
	}
	return locKeep, model.StationGeo{}
}

// reconcileSurveyLocations applies decideSurveyLoc to every listed
// station. running reports a running survey job (nil = none). It returns
// the number of registry writes.
func reconcileSurveyLocations(ctx context.Context, reg *registry.Registry, list []registry.Station, eng surveyLocSource, running func(model.StationIdx) bool) int {
	writes := 0
	for i := range list {
		st := &list[i]
		src := eng.PositionSource(st.Idx)
		sv, _ := eng.SurveyOf(st.Idx)
		run := false
		if running != nil && !st.Located {
			run = running(st.Idx)
		}
		act, g := decideSurveyLoc(st, src, sv, run)
		if act == locKeep {
			continue
		}
		_, err := reg.Update(ctx, st.Idx, func(s *registry.Station) error {
			// Re-check on the current row: the owner may have declared a
			// position since the list was taken.
			if s.Idx == model.HostStation || ownPosition(s) {
				return nil
			}
			switch act {
			case locBySurvey:
				s.Located, s.Effective, s.LocStatus = true, g, model.LocEstimated
			case locUnlocate:
				if locatedBySurvey(s) {
					s.Located, s.Effective, s.LocStatus = false, model.StationGeo{}, model.LocUnknown
				}
			case locEstimating:
				if !s.Located {
					s.LocStatus = model.LocEstimating
				}
			case locUnknown:
				if !s.Located && s.LocStatus == model.LocEstimating {
					s.LocStatus = model.LocUnknown
				}
			}
			return nil
		})
		if err == nil {
			writes++
		}
		if act == locBySurvey || act == locUnlocate {
			detail := `{"located":true,"source":"survey"}`
			if act == locUnlocate {
				detail = `{"located":false,"source":"survey"}`
			}
			_ = reg.Audit(ctx, registry.SystemActor, "station.location.survey", st.SID, detail)
		}
	}
	return writes
}

// ---------------------------------------------------------------------------
// Locate Me v2 hints for the survey
// ---------------------------------------------------------------------------

const (
	surveyHintEvery = 10 * time.Minute // pass period
	surveyHintRetry = time.Hour        // per station
	surveyHintBatch = 10               // estimates per pass
	surveyHintMaxKm = 60.0             // a hint less certain than this (r95) is not used
)

type surveyHint struct {
	ok    bool
	llh   geo.LLH
	r95Km float64
	tried time.Time
}

// surveyHints is the Locate Me v2 hint cache (owner/admin data: it is an
// approximate station position; never logged, never public).
type surveyHints struct {
	mu sync.Mutex
	m  map[model.StationIdx]surveyHint
}

func newSurveyHints() *surveyHints { return &surveyHints{m: map[model.StationIdx]surveyHint{}} }

// get returns the hint of st, if any.
func (h *surveyHints) get(st model.StationIdx) (geo.LLH, bool) {
	if h == nil {
		return geo.LLH{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.m[st]
	return e.llh, ok && e.ok
}

// due lists up to max stations (no own position, not located by the
// survey either) whose hint is missing and was not tried within retry.
func (h *surveyHints) due(list []registry.Station, now time.Time, max int) []registry.Station {
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := make(map[model.StationIdx]struct{}, len(list))
	var out []registry.Station
	for i := range list {
		st := &list[i]
		if st.Idx == model.HostStation || st.Located || ownPosition(st) {
			continue
		}
		keep[st.Idx] = struct{}{}
		e, ok := h.m[st.Idx]
		if ok && now.Sub(e.tried) < surveyHintRetry {
			continue
		}
		if len(out) < max {
			out = append(out, *st)
		}
	}
	for idx := range h.m {
		if _, ok := keep[idx]; !ok {
			delete(h.m, idx) // located or gone: the hint is no longer needed
		}
	}
	return out
}

func (h *surveyHints) set(st model.StationIdx, e surveyHint) {
	h.mu.Lock()
	h.m[st] = e
	h.mu.Unlock()
}

// hintFromLocate turns a Locate Me v2 result into a survey hint.
func hintFromLocate(r *analytics.Locate2Result, now time.Time) surveyHint {
	e := surveyHint{tried: now}
	if r == nil || !r.OK || !ageo.ValidLatLon(r.Lat, r.Lon) {
		return e
	}
	r95 := float64(r.R95Km)
	if !(r95 > 0) || r95 > surveyHintMaxKm {
		return e
	}
	e.ok, e.llh, e.r95Km = true, geo.LLH{Lat: r.Lat, Lon: r.Lon}, r95
	return e
}

// runSurveyHints computes Locate Me v2 hints for unlocated stations until
// ctx ends (startMLAT, only with the survey and Locate Me enabled).
func (e *Engine) runSurveyHints(nc *netCore) {
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	select {
	case <-nc.ctx.Done():
		return
	case <-first.C:
	}
	t := time.NewTicker(surveyHintEvery)
	defer t.Stop()
	for {
		e.surveyHintPass(nc)
		select {
		case <-nc.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// surveyHintPass is one pass of runSurveyHints.
func (e *Engine) surveyHintPass(nc *netCore) {
	if e.locate.Load() == nil || nc.hints == nil {
		return
	}
	now := time.Now()
	for _, st := range nc.hints.due(nc.reg.List(registry.Filter{Admission: model.AdmApproved}), now, surveyHintBatch) {
		if nc.ctx.Err() != nil {
			return
		}
		if sv := nc.surv; sv != nil {
			if stt, ok := sv.Status(st.Idx); ok && stt.Last != nil && stt.Last.Valid && stt.Last.Stable {
				nc.hints.set(st.Idx, surveyHint{tried: now}) // the survey has a better answer
				continue
			}
		}
		an := e.StationAnalyzer(st.SID)
		if an == nil {
			nc.hints.set(st.Idx, surveyHint{tried: now})
			continue
		}
		ctx, cancel := context.WithTimeout(nc.ctx, time.Minute)
		r, err := an.LocateMe(ctx, analytics.LocateRequest{})
		cancel()
		if err != nil {
			r = nil
		}
		nc.hints.set(st.Idx, hintFromLocate(r, now))
	}
}
