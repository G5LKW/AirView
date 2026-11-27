package survey

import (
	"math"
	"slices"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// chi2Halves999 is the 99.9 % quantile of χ² with 2 degrees of freedom
// (−2·ln 0.001).
const chi2Halves999 = 13.8155

// ellipse95 scales a 1σ ellipse to 95 % (sqrt of χ²₂ at 0.95).
const ellipse95 = 2.4477

// evaluate solves a job's observations and ends the job when the result
// is stable and consistent, when MaxDuration is reached, or keeps it
// collecting.
func (s *Surveyor) evaluate(j *job, now time.Time) {
	defer j.evalBusy.Store(false)
	s.mu.Lock()
	info := j.info
	reason := j.reason
	s.mu.Unlock()
	obs, cen, cenOK, peerH := j.col.snapshot()
	final := now.Sub(j.started) >= s.cfg.MaxDuration
	res := s.solveJob(obs, &info, reason, cen, cenOK, peerH)
	res.Observations = len(obs)

	s.mu.Lock()
	defer s.unlockRun()
	if s.jobs[j.st] != j {
		return
	}
	j.nextEval = now.Add(s.cfg.EvalEvery)
	// an MSL/HAE job goes on until the reference is decided
	altPending := reason == ReasonAltRef && res.AltRef.Chosen == ""
	switch {
	case res.Valid && res.Stable && !res.Suspect && !altPending:
		s.finishLocked(j, StateDone, &res)
	case final && res.Valid:
		s.finishLocked(j, StateUnstable, &res)
	case final && len(obs) < 20:
		s.finishLocked(j, StateNoData, &res)
	case final:
		s.finishLocked(j, StateUnstable, &res)
	default:
		j.best = &res
		if res.Valid && res.Solution.SigmaHM > 0 && res.Solution.SigmaHM < 2000 {
			j.approx.Store(newApprox(res.Solution.ECEF, s.cfg.MaskDeg, 1))
		}
	}
}

// heightPriorAt returns the DESIGN §10.2 height prior at a position: the
// DEM plus the antenna height (σ HeightSigmaM), else the declared height
// when its reference is known, else the peers' median height (σ 200 m).
// ok = false: no prior (free height).
func (s *Surveyor) heightPriorAt(lat, lon float64, info *Station, peerH []float64) (h, sigma float64, ok bool) {
	agl := info.AGLM
	if !(agl > 0) {
		agl = s.cfg.DefaultAGLM
	}
	n, nok := s.o.Undulation(lat, lon)
	if !nok {
		n = 0
	}
	if s.o.Terrain != nil {
		if e, eok := s.o.Terrain.ElevationM(lat, lon); eok && !math.IsNaN(e) {
			return e + n + agl, s.cfg.HeightSigmaM, true
		}
	}
	if d := info.Declared; d != nil {
		switch d.AltRef {
		case model.AltRefHAE:
			return d.Alt, s.cfg.HeightSigmaM, true
		case model.AltRefMSL, "":
			return d.Alt + n, s.cfg.HeightSigmaM, true
		}
		// ambiguous: the two readings differ by N; centre between them
		return d.Alt + n/2, math.Max(s.cfg.HeightSigmaM, math.Abs(n)/2+s.cfg.HeightSigmaM), true
	}
	if len(peerH) > 0 {
		hs := slices.Clone(peerH)
		slices.Sort(hs)
		return hs[len(hs)/2], 200, true
	}
	return 0, 0, false
}

// solveJob runs the solver with the job's seed and height prior and
// applies the stability, consistency, declared-position and MSL/HAE tests.
func (s *Surveyor) solveJob(obs []Obs, info *Station, reason Reason, cen geo.Vec3, cenOK bool, peerH []float64) Result {
	var res Result
	cfg := s.cfg
	atm := s.atmosphere()
	// seed and search radius
	var seed geo.LLH
	radius := cfg.SearchRadiusM
	switch {
	case info.Declared != nil:
		seed = geo.LLH{Lat: info.Declared.Lat, Lon: info.Declared.Lon}
	case info.Hint != nil:
		seed = geo.LLH{Lat: info.Hint.Lat, Lon: info.Hint.Lon}
	case cenOK:
		seed = geo.FromECEF(cen)
		seed.H = 0
		radius = math.Min(3*cfg.SearchRadiusM, 1000e3)
	default:
		return res
	}
	h0, hs, hok := s.heightPriorAt(seed.Lat, seed.Lon, info, peerH)
	if !hok {
		hs = 0
	}
	seed.H = h0
	in := Input{Obs: obs, Seed: seed.ECEF(), SearchRadiusM: radius, HeightHAE: h0, HeightSigmaM: hs, Atm: atm}
	sol, err := Solve(in, cfg)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if sol.OnEdge {
		// the best grid point was on the boundary: search again around it
		in.Seed = sol.ECEF
		if sol2, err2 := Solve(in, cfg); err2 == nil {
			sol = sol2
		}
	}
	// a DEM prior is evaluated at the solution, not at the seed
	if s.o.Terrain != nil && hok {
		if h1, s1, ok1 := s.heightPriorAt(sol.LLH.Lat, sol.LLH.Lon, info, peerH); ok1 && math.Abs(h1-h0) > 1 {
			in2 := in
			in2.Seed, in2.SearchRadiusM, in2.HeightHAE, in2.HeightSigmaM = sol.ECEF, 2000, h1, s1
			if sol2, err2 := Solve(in2, cfg); err2 == nil {
				sol, in = sol2, in2
			}
		}
	}
	res.Valid = true
	// MSL/HAE test for ambiguous and MSL declarations (DESIGN §10.2)
	if d := info.Declared; d != nil && (d.AltRef == model.AltRefAmbiguous || d.AltRef == model.AltRefMSL || d.AltRef == "") && (reason == ReasonAltRef || reason == ReasonCheck || reason == ReasonOwner || reason == ReasonAdmin || reason == ReasonBias) {
		res.AltRef = s.altRefTest(in, sol, d)
		if d.AltRef == model.AltRefAmbiguous && res.AltRef.Chosen != "" && s.o.Terrain == nil {
			// re-solve with the resolved height as the prior
			n := res.AltRef.UndulationM
			h := d.Alt
			if res.AltRef.Chosen == model.AltRefMSL {
				h += n
			}
			in2 := in
			in2.Seed, in2.SearchRadiusM, in2.HeightHAE, in2.HeightSigmaM = sol.ECEF, 2000, h, cfg.HeightSigmaM
			if sol2, err2 := Solve(in2, cfg); err2 == nil {
				sol = sol2
			}
		}
	}
	res.Solution = sol
	s.judge(&res, info)
	return res
}

// altRefTest solves with a free height and compares the two readings of a
// declared altitude (DESIGN §10.2): the reading whose distance from the
// free height is smaller by more than 3σ wins, provided it is itself
// within 4σ.
func (s *Surveyor) altRefTest(in Input, sol Solution, d *Declared) AltRefVerdict {
	v := AltRefVerdict{Tested: true}
	n, ok := s.o.Undulation(sol.LLH.Lat, sol.LLH.Lon)
	if !ok {
		return AltRefVerdict{}
	}
	v.UndulationM = n
	free := in
	free.Seed, free.SearchRadiusM, free.HeightSigmaM = sol.ECEF, 2000, 0
	fs, err := Solve(free, s.cfg)
	if err != nil || !(fs.SigmaVM > 0) {
		return AltRefVerdict{}
	}
	v.FreeHAE, v.FreeSigmaM = fs.LLH.H, fs.SigmaVM
	// the declared altitude is the antenna's; with a DEM the free height is
	// still the antenna's (no prior was used)
	v.ZHAE = math.Abs(fs.LLH.H-d.Alt) / fs.SigmaVM
	v.ZMSL = math.Abs(fs.LLH.H-(d.Alt+n)) / fs.SigmaVM
	switch {
	case v.ZHAE-v.ZMSL > 3 && v.ZMSL < 4:
		v.Chosen = model.AltRefMSL
	case v.ZMSL-v.ZHAE > 3 && v.ZHAE < 4:
		v.Chosen = model.AltRefHAE
	}
	return v
}

// judge applies the stability rule (DESIGN §10.2), the survey's own
// consistency checks, the AutoUse qualification and the declared-position
// check (DESIGN §10.3).
func (s *Surveyor) judge(res *Result, info *Station) {
	cfg := &s.cfg
	sol := &res.Solution
	limit := cfg.StableSigmaM
	if info.Class.GPS() {
		limit = cfg.StableSigmaGPSM
	}
	res.Stable = sol.SigmaHM > 0 && sol.SigmaHM < limit && sol.AzSpreadDeg >= cfg.MinAzSpreadDeg &&
		sol.Aircraft >= cfg.MinAircraft && sol.HalvesOK
	var why []string
	if sol.HalvesOK && sol.HalvesChi2 > chi2Halves999 {
		why = append(why, "halves disagree")
	}
	if sol.OnEdge {
		why = append(why, "outside the search area")
	}
	if n := sol.Used + sol.Rejected; n > 0 && float64(sol.Rejected) > 0.25*float64(n) {
		why = append(why, "many outliers")
	}
	if prior := info.Class.PriorJitterNs(); prior > 0 && sol.JitterNs > math.Max(4*prior, 100) {
		why = append(why, "timing noisier than the clock class")
	}
	res.SuspectWhy = why
	res.Suspect = len(why) > 0
	res.AutoUse = cfg.AutoUse && res.Stable && !res.Suspect && sol.SigmaHM <= cfg.AutoUseMaxSigmaM
	res.Ellipse95A = sol.Ellipse.SemiMajor * ellipse95
	res.Ellipse95B = sol.Ellipse.SemiMinor * ellipse95
	res.Ellipse95AngleDeg = sol.Ellipse.AzimuthDeg
	if d := info.Declared; d != nil && !res.Suspect {
		fr := geo.NewFrameECEF(sol.ECEF)
		dp := fr.ToENU(geo.ToECEF(d.Lat, d.Lon, sol.LLH.H))
		res.DeclaredChecked = true
		res.DeclaredDistM = math.Hypot(dp.X, dp.Y)
		// the discrepancy's σ along its direction, from the ellipse
		res.DeclaredLimitM = math.Max(cfg.SuspectMinM, cfg.SuspectK*sol.SigmaHM)
		res.DeclaredSuspect = res.DeclaredDistM > res.DeclaredLimitM
	}
}
