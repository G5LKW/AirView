package engine

// Reverse-MLAT station survey (internal/mlat/survey, DESIGN Â§10.2-10.3,
// Â§19.3; WIRING.md there). In aggregator mode with the MLAT engine running
// and aggregator.mlat.survey.disabled off, one Surveyor sits in front of
// the timing quarantine (a station under survey usually has no proven
// timing yet: it has no position to prove it with), learns the registry's
// stations from the same sync loop as the engine, hands its results to the
// engine (AutoUseSurvey), the registry (estimates, declared-suspect status)
// and the owner notices, and tells the planner which stations need
// spread-out reference aircraft.
//
// Exact positions are owner/admin data (DESIGN Â§8.2): they reach the
// registry, the engine and the owner/admin routes only; logs and audit
// rows carry no coordinates.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	cgeo "airview/internal/geo"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/survey"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/registry"
)

// surveyFwd forwards the engine's survey requests (mlat.Options.Survey) to
// the surveyor built after the engine.
type surveyFwd struct {
	s atomic.Pointer[survey.Surveyor]
}

func (f *surveyFwd) RequestSurvey(st model.StationIdx, reason string) {
	if s := f.s.Load(); s != nil {
		s.RequestSurvey(st, reason)
	}
}

// surveyRegistry maps the surveyor's verdicts and results onto the
// registry (survey.DeclaredFlagger, survey.Notifier). Both callbacks run on
// the surveyor's goroutine and must not block: the registry writes run on
// their own goroutines.
type surveyRegistry struct {
	reg *registry.Registry
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last map[model.StationIdx]SurveyNotice
}

// SurveyNotice is the owner notice of a station's last finished survey
// (owner/admin data: it holds the exact position).
type SurveyNotice struct {
	Result survey.Result
	At     time.Time
}

func newSurveyRegistry(reg *registry.Registry, log *slog.Logger, now func() time.Time) *surveyRegistry {
	return &surveyRegistry{reg: reg, log: log, now: now, last: map[model.StationIdx]SurveyNotice{}}
}

// SetDeclaredSuspect implements survey.DeclaredFlagger: the registry's
// location status becomes "suspect" (from "declared") or goes back.
// mlatStationInfo turns that into mlat.StationInfo.Suspect / DiscrepancyM.
func (a *surveyRegistry) SetDeclaredSuspect(st model.StationIdx, suspect bool, discrepancyM float64) {
	a.log.Info("survey: declared position check", "station", int(st), "suspect", suspect, "discrepancyM", math.Round(discrepancyM))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := a.reg.Update(ctx, st, func(s *registry.Station) error {
			switch {
			case suspect && (s.LocStatus == model.LocDeclared || s.LocStatus == ""):
				s.LocStatus = model.LocSuspect
			case !suspect && s.LocStatus == model.LocSuspect:
				s.LocStatus = model.LocDeclared
			}
			return nil
		})
		if err != nil {
			a.log.Warn("survey: location status not stored", "station", int(st), "err", err)
			return
		}
		if sid := a.sid(st); sid != "" {
			detail := fmt.Sprintf(`{"suspect":%t,"discrepancyM":%.0f}`, suspect, discrepancyM)
			_ = a.reg.Audit(ctx, registry.SystemActor, "station.survey.declared", sid, detail)
		}
	}()
}

func (a *surveyRegistry) sid(st model.StationIdx) string {
	if s, ok := a.reg.Station(st); ok {
		return s.SID
	}
	return ""
}

// SurveyFinished implements survey.Notifier: the result is kept for the
// owner's notice, and a solution is stored as the station's estimate
// (method "survey") and as its Estimated position.
func (a *surveyRegistry) SurveyFinished(r survey.Result) {
	a.mu.Lock()
	if len(a.last) >= 100_000 {
		clear(a.last)
	}
	a.last[r.Station] = SurveyNotice{Result: r, At: a.now()}
	a.mu.Unlock()
	a.log.Info("survey finished", "station", int(r.Station), "reason", string(r.Reason), "state", string(r.State),
		"observations", r.Observations, "valid", r.Valid, "stable", r.Stable, "sigmaHM", math.Round(r.Solution.SigmaHM),
		"altRef", r.AltRef.Chosen)
	if !r.Valid {
		// No position: only the outcome is recorded (owner notice "not
		// enough data"; cancellations are not worth a notice).
		if r.State != survey.StateCancelled {
			if sid := a.sid(r.Station); sid != "" {
				detail := fmt.Sprintf(`{"reason":%q,"state":%q,"valid":false,"observations":%d}`, r.Reason, r.State, r.Observations)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					_ = a.reg.Audit(ctx, registry.SystemActor, "station.survey", sid, detail)
				}()
			}
		}
		return
	}
	llh := r.Solution.LLH
	sig := func(i int) float64 { return math.Sqrt(math.Max(r.Solution.CovENU[i], 0)) }
	status := "unstable"
	switch {
	case r.Suspect:
		status = "suspect"
	case r.Stable:
		status = "stable"
	}
	est := registry.Estimate{Idx: r.Station, T: r.Finished, Method: "survey", Lat: llh.Lat, Lon: llh.Lon, HAE: llh.H,
		SigmaE: sig(0), SigmaN: sig(4), SigmaU: sig(8), N: r.Solution.Used, Status: status}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := a.reg.AddEstimate(ctx, est); err != nil {
			a.log.Warn("survey: estimate not stored", "station", int(r.Station), "err", err)
			return
		}
		if r.Stable && !r.Suspect {
			p := registry.Position{Lat: llh.Lat, Lon: llh.Lon, Alt: llh.H, Valid: true}
			if _, err := a.reg.SetEstimated(ctx, r.Station, p, r.Solution.SigmaHM); err != nil {
				a.log.Warn("survey: estimated position not stored", "station", int(r.Station), "err", err)
			}
		}
		if sid := a.sid(r.Station); sid != "" {
			detail := fmt.Sprintf(`{"reason":%q,"state":%q,"valid":true,"stable":%t,"suspect":%t,"sigmaHM":%.0f,"autoUse":%t,"altRef":%q}`,
				r.Reason, r.State, r.Stable, r.Suspect, r.Solution.SigmaHM, r.AutoUse, r.AltRef.Chosen)
			_ = a.reg.Audit(ctx, registry.SystemActor, "station.survey", sid, detail)
		}
	}()
}

func (a *surveyRegistry) get(st model.StationIdx) (SurveyNotice, bool) {
	if a == nil {
		return SurveyNotice{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.last[st]
	return n, ok
}

// newSurvey builds the surveyor next to the MLAT engine (nil when the
// survey is disabled).
func (e *Engine) newSurvey(nc *netCore, cfg model.Config, eng *mlat.Engine) (*survey.Surveyor, error) {
	if cfg.Aggregator.MLAT.Survey.Disabled {
		return nil, nil
	}
	nc.survReg = newSurveyRegistry(nc.reg, e.log.With("component", "mlat.survey"), ntp.Now)
	nc.hints = newSurveyHints()
	o := survey.Options{
		Config:     survey.ConfigFromModel(cfg.Aggregator.MLAT),
		Clock:      eng.Filter(),
		Refs:       eng.Refs(),
		Atmosphere: eng.Atmosphere,
		Sink:       eng,
		Declared:   nc.survReg,
		Notifier:   nc.survReg,
		Targets:    eng,
		Now:        ntp.Now,
		Logger:     e.log.With("component", "mlat.survey"),
	}
	if x := e.ext; x != nil && x.terr != nil {
		if p := x.terr.Provider(); p != nil {
			o.Terrain = p // cached tiles only: ElevationAtZoom never downloads
		}
	}
	return survey.New(o)
}

// surveyKey is the comparable form of a survey.Station (change detection).
type surveyKey struct {
	s       survey.Station
	d       survey.Declared
	hasD    bool
	h       geo.LLH
	hasHint bool
}

func keyOf(s survey.Station) surveyKey {
	k := surveyKey{s: s}
	if s.Declared != nil {
		k.d, k.hasD = *s.Declared, true
	}
	if s.Hint != nil {
		k.h, k.hasHint = *s.Hint, true
	}
	k.s.Declared, k.s.Hint = nil, nil
	return k
}

// surveyStation maps a registry station onto the surveyor's view (WIRING.md
// Â§3). Timestamps: the station sends usable timing (map-only Beast 30004
// stations and stations an admin excluded from timing are not surveyed).
func surveyStation(st *registry.Station) survey.Station {
	out := survey.Station{
		Class:      st.ClockCls,
		Timestamps: st.ClockCls.Timed() && !st.Has(registry.FlagBeast) && !st.Has(registry.FlagTimingExcluded),
		GNSS:       st.GNSS.Valid || st.LocStatus == model.LocGNSS,
		Confirmed:  st.LocStatus == model.LocConfirmed,
	}
	if d := st.Declared; d.Valid {
		out.Declared = &survey.Declared{Lat: d.Lat, Lon: d.Lon, Alt: d.Alt, AltRef: st.DeclaredAltRef}
	}
	return out
}

// syncSurveyStations pushes changed stations to the surveyor and removes
// the ones that are gone. restore is called once per newly seen station
// (persisted last result, after a restart).
//
// hints (may be nil) supplies the Locate Me v2 search hint of stations
// without a position (surveyloc.go).
func syncSurveyStations(list []registry.Station, surv *survey.Surveyor, last map[model.StationIdx]surveyKey, restore func(model.StationIdx), hints *surveyHints) {
	seen := make(map[model.StationIdx]struct{}, len(list))
	for i := range list {
		st := &list[i]
		seen[st.Idx] = struct{}{}
		in := surveyStation(st)
		if in.Declared == nil && !in.GNSS && !in.Confirmed {
			if h, ok := hints.get(st.Idx); ok {
				in.Hint = &h
			}
		}
		k := keyOf(in)
		prev, ok := last[st.Idx]
		if ok && prev == k {
			continue
		}
		surv.SetStation(st.Idx, in)
		last[st.Idx] = k
		if !ok && restore != nil {
			restore(st.Idx)
		}
	}
	for idx := range last {
		if _, ok := seen[idx]; !ok {
			surv.RemoveStation(idx)
			delete(last, idx)
		}
	}
}

// restoreSurvey puts a station's last persisted survey estimate back into
// the surveyor (so the 6-hourly checks are not all run at once after a
// restart) and, for a stable one, into the engine (AutoUseSurvey decides).
func (nc *netCore) restoreSurvey(ctx context.Context, idx model.StationIdx) {
	surv := nc.surv
	if surv == nil {
		return
	}
	ests, err := nc.reg.Estimates(ctx, idx)
	if err != nil {
		return
	}
	st, _ := nc.reg.Station(idx)
	best := pickRestoreSurvey(ests, st, surv.Config().SuspectMinM)
	if best == nil {
		return
	}
	llh := geo.LLH{Lat: best.Lat, Lon: best.Lon, H: best.HAE}
	r := survey.Result{Station: idx, Reason: survey.ReasonCheck, State: survey.StateUnstable, Started: best.T, Finished: best.T,
		Observations: best.N, Valid: true, Stable: best.Status == "stable", Suspect: best.Status == "suspect",
		DeclaredSuspect: st.LocStatus == model.LocSuspect}
	if r.Stable {
		r.State = survey.StateDone
	}
	r.Solution.LLH = llh
	r.Solution.ECEF = geo.ToECEF(llh.Lat, llh.Lon, llh.H)
	r.Solution.SigmaHM = restoredSigmaH(best)
	r.Solution.SigmaVM = best.SigmaU
	r.Solution.Used = best.N
	surv.Restore(idx, r)
	if eng := nc.mlat; eng != nil && r.Stable && !r.Suspect {
		eng.SetSurvey(idx, mlat.Survey{Valid: true, ECEF: r.Solution.ECEF, SigmaHM: r.Solution.SigmaHM,
			SigmaVM: r.Solution.SigmaVM, Stable: true})
	}
}

// restoredSigmaH is the horizontal Ïƒ of a persisted survey estimate in the
// same definition as live results (MLAT-R5): the error-ellipse semi-major.
// Only ÏƒE/ÏƒN are stored, so max(ÏƒE, ÏƒN) is used (exact for an axis-aligned
// ellipse); math.Hypot would inflate it by up to âˆš2 and flip AutoUse.
func restoredSigmaH(e *registry.Estimate) float64 {
	return math.Max(e.SigmaE, e.SigmaN)
}

// pickRestoreSurvey chooses the persisted survey estimate to restore:
// the newest whose status is stable or suspect (MLAT-R3: a later unstable
// re-check must not hide a stable one), skipping one that is farther than
// max(suspectMinM, 3Ïƒ_h) from the station's current declared position
// unless the declaration is itself suspect (MLAT-R2: the owner moved the
// receiver while the server was down).
func pickRestoreSurvey(ests []registry.Estimate, st registry.Station, suspectMinM float64) *registry.Estimate {
	var best *registry.Estimate
	for i := range ests {
		e := &ests[i]
		if e.Method != "survey" || (e.Status != "stable" && e.Status != "suspect") {
			continue
		}
		if st.Declared.Valid && st.LocStatus != model.LocSuspect {
			lim := max(suspectMinM, 3*restoredSigmaH(e))
			if cgeo.HaversineKm(e.Lat, e.Lon, st.Declared.Lat, st.Declared.Lon)*1000 > lim {
				continue
			}
		}
		if best == nil || e.T.After(best.T) {
			best = e
		}
	}
	return best
}

// Survey returns the reverse-MLAT surveyor (nil in standalone mode,
// without the MLAT engine or with aggregator.mlat.survey.disabled).
func (e *Engine) Survey() *survey.Surveyor {
	if e.net == nil {
		return nil
	}
	return e.net.surv
}

// SurveyNotice returns the owner notice of station st's last finished
// survey (owner/admin data).
func (e *Engine) SurveyNotice(st model.StationIdx) (SurveyNotice, bool) {
	if e.net == nil {
		return SurveyNotice{}, false
	}
	return e.net.survReg.get(st)
}
