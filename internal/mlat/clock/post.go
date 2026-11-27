package clock

import (
	"math"
	"slices"
	"strconv"
)

// postEpoch runs the single-threaded bookkeeping after the component
// updates: statistics, step detection, the per-aircraft blacklist, anchor
// probation and demotion, adaptive noise and the anchor drift monitor.
func (f *Filter) postEpoch(tE float64) {
	cfg := &f.cfg
	k := f.k
	winEpochs := int64(math.Ceil(cfg.SyncedWindowS / cfg.EpochS))
	f.stats.ExternalStates = 0
	for _, c := range f.comps {
		f.influenceCap(c)
		f.stats.ExternalStates += c.xs.k
		if c.xsFailed {
			f.stats.ExternalFailures++
		}
		f.stats.Processed += uint64(c.nAcc + c.nRej)
		f.stats.Accepted += uint64(c.nAcc)
		f.stats.Rejected += uint64(c.nRej)
		if len(c.ar.grp) > 0 && !c.updated {
			f.stats.UpdateFailures++
			f.logf("clock: component %d: update failed (n=%d)", c.id, c.n)
		}
		n := c.n
		for _, s := range c.sts {
			s.rate = float64(s.accIn(k, winEpochs)) / cfg.SyncedWindowS
			if s.epAcc > 0 {
				s.countAcc(k, s.epAcc)
				s.lastAccS = tE
				if c.updated {
					// covariance matching: E[δx²] = P⁻ − P⁺ for a
					// consistent filter (φ for free clocks, δ for anchors)
					kk := kPhi
					if s.mode == modeAnchor {
						kk = kTheta
					}
					if l := s.layout[kk]; l >= 0 {
						i := s.off + int(l)
						dx := c.x[i] - s.preX[kk]
						drop := s.pre[kk] - c.P[i*n+i]
						if drop > 0 {
							s.adDx2 += dx * dx
							s.adDrop += drop
							s.adN++
						}
					}
				}
			}
			// step detection (DESIGN §9.2.4)
			if s.streakN >= cfg.StepCount {
				f.stats.Steps++
				s.nSteps++
				it := s.off + int(s.layout[kTheta])
				step := s.streakSum / float64(s.streakN)
				detail := ""
				// A run whose residuals grow linearly is a frequency error,
				// not an offset step: correcting θ alone keeps the wrong φ,
				// which drives the next step a few seconds later. When the
				// slope is significant (4 standard errors) and beyond 3σ of
				// φ, φ is corrected by it and its variance opened up.
				if l := s.layout[kPhi]; l >= 0 && s.mode == modeFree {
					ip := s.off + int(l)
					off, slope, se := s.stepEstimate(tE)
					sphi := math.Sqrt(math.Max(c.P[ip*n+ip], 0))
					if !math.IsInf(se, 1) && math.Abs(slope) > 4*se && math.Abs(slope) > 3*sphi {
						step = off
						c.x[ip] += slope
						c.resetState(ip, math.Max(c.P[ip*n+ip], slope*slope/4+se*se))
						f.stats.FreqSteps++
						detail = ", φ " + strconv.FormatFloat(slope, 'f', 0, 64) + " ppb"
					}
				}
				v := math.Max(cfg.ResetThetaSigmaNs, math.Abs(step)/2)
				c.x[it] += step
				c.resetState(it, v*v)
				s.addEvent(tE, "step", fmtNs(step)+detail)
				s.streakN, s.streakSign, s.streakSum = 0, 0, 0
				if s.mode == modeAnchor {
					s.demoteReq = "step"
				}
				f.stepRun(c, s, tE, step)
			}
			f.adapt(s, tE)
			f.driftBin(c, s, tE)
		}
		for _, r := range c.peerRes {
			f.notePeer(r, k, winEpochs)
		}
	}
	f.blacklist(tE)
	f.anchorPass(tE)
}

// stepRun counts same-sign steps of a free clock; StepRunCount of them
// within StepRunWindowS reset its frequency states to the prior variance
// (keeping their means): a θ step corrects the offset but keeps a wrong φ,
// which then drives the next step a few seconds later (HOOKS WP-M-engine
// -> internal/mlat/clock, SDR-only networks).
func (f *Filter) stepRun(c *component, s *station, tE, step float64) {
	cfg := &f.cfg
	if cfg.StepRunCount <= 0 || s.mode != modeFree {
		s.stepRunN = 0
		return
	}
	sg := int8(1)
	if step < 0 {
		sg = -1
	}
	if s.stepRunN == 0 || sg != s.stepRunSign || tE-s.stepRunT0 > cfg.StepRunWindowS {
		s.stepRunSign, s.stepRunN, s.stepRunT0 = sg, 0, tE
	}
	s.stepRunN++
	if s.stepRunN < cfg.StepRunCount {
		return
	}
	s.stepRunN = 0
	if l := s.layout[kPhi]; l >= 0 {
		c.resetState(s.off+int(l), s.prior.phiSig*s.prior.phiSig)
	}
	if l := s.layout[kPsi]; s.psi && l >= 0 {
		c.resetState(s.off+int(l), s.prior.psiSig*s.prior.psiSig)
	}
	f.stats.FreqResets++
	s.addEvent(tE, "freq_reset", strconv.Itoa(cfg.StepRunCount)+" steps")
}

func fmtNs(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64) + " ns"
}

// adapt closes an adaptation window: the process-noise scale follows the
// ratio of the observed state corrections to their predicted variance, the
// jitter scale the mean squared studentised innovation (bounded).
func (f *Filter) adapt(s *station, tE float64) {
	cfg := &f.cfg
	if s.adStart == 0 {
		s.adStart = tE
	}
	if tE-s.adStart < cfg.AdaptWindowS {
		return
	}
	if s.adN >= 20 && s.adDrop > 0 {
		r := math.Sqrt(s.adDx2 / s.adDrop)
		s.qScale = clampF(s.qScale*clampF(r, 0.5, 2), cfg.QScaleMin, cfg.QScaleMax)
	}
	if !cfg.NoJitterLearning && s.adWN >= 50 {
		r := math.Sqrt(s.adW2 / float64(s.adWN))
		s.jitScale = clampF(s.jitScale*clampF(r, 0.7, 1.4), cfg.JitterScaleMin, cfg.JitterScaleMax)
	}
	s.adStart = tE
	s.adDx2, s.adDrop, s.adN, s.adW2, s.adWN = 0, 0, 0, 0, 0
}

func clampF(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

// driftBin maintains an anchor's apparent offset δ̂ + mean leave-one-out
// residual in 60-s bins and demotes it when the regression slope over
// AnchorDriftWindowS exceeds AnchorDriftMaxNsPerH (DESIGN §9.2.5).
func (f *Filter) driftBin(c *component, s *station, tE float64) {
	if s.mode != modeAnchor {
		s.drift = s.drift[:0]
		s.looSum, s.looN, s.driftBin0 = 0, 0, 0
		return
	}
	if s.driftBin0 == 0 {
		s.driftBin0 = tE
	}
	if tE-s.driftBin0 < 60 {
		return
	}
	if s.looN > 0 {
		it := s.off + int(s.layout[kTheta])
		v := c.x[it] + s.looSum/float64(s.looN)
		s.drift = append(s.drift, driftPt{t: tE, v: v})
	}
	s.looSum, s.looN, s.driftBin0 = 0, 0, tE
	cfg := &f.cfg
	for len(s.drift) > 0 && tE-s.drift[0].t > cfg.AnchorDriftWindowS {
		s.drift = s.drift[1:]
	}
	if len(s.drift) < 8 || s.drift[len(s.drift)-1].t-s.drift[0].t < 0.75*cfg.AnchorDriftWindowS {
		return
	}
	slope, se := linFit(s.drift)
	lim := math.Max(cfg.AnchorDriftMaxNsPerH/3600, 3*se)
	if math.Abs(slope) > lim {
		s.demoteReq = "drift"
		s.anchorReason = "drift"
	}
}

// linFit returns the least-squares slope of v(t) and its standard error.
func linFit(p []driftPt) (slope, se float64) {
	n := float64(len(p))
	var mt, mv float64
	for _, x := range p {
		mt += x.t
		mv += x.v
	}
	mt /= n
	mv /= n
	var stt, stv float64
	for _, x := range p {
		stt += (x.t - mt) * (x.t - mt)
		stv += (x.t - mt) * (x.v - mv)
	}
	if stt == 0 {
		return 0, math.Inf(1)
	}
	slope = stv / stt
	var rss float64
	for _, x := range p {
		r := x.v - mv - slope*(x.t-mt)
		rss += r * r
	}
	if n > 2 {
		se = math.Sqrt(rss / (n - 2) / stt)
	}
	return slope, se
}

func (f *Filter) notePeer(r peerResult, k, win int64) {
	s := f.station(r.a, false)
	if s == nil {
		return
	}
	p := s.peers[r.b]
	if p == nil {
		if len(s.peers) >= 64 {
			return
		}
		p = &peerAcc{sq: r.r * r.r}
		s.peers[r.b] = p
	}
	w := k / max(win, 1)
	if p.win != w {
		p.last, p.n, p.win = p.n, 0, w
	}
	p.n++
	p.sq += 0.05 * (r.r*r.r - p.sq)
}

// blacklist merges the per-aircraft outcomes and blacklists aircraft whose
// groups keep failing across stations (bad NACp, spoofing).
func (f *Filter) blacklist(tE float64) {
	cfg := &f.cfg
	var add []uint32
	for _, c := range f.comps {
		for _, r := range c.acRes {
			a := f.acft[r.key]
			if a == nil {
				a = &acftStat{}
				f.acft[r.key] = a
			}
			a.n++
			if r.fail {
				a.fails++
			}
			a.lastS = tE
			if a.n >= cfg.BlacklistMinGrp && float64(a.fails) > cfg.BlacklistRate*float64(a.n) {
				add = append(add, r.key)
				a.n, a.fails = 0, 0
			} else if a.n >= 4*cfg.BlacklistMinGrp {
				a.n, a.fails = a.n/2, a.fails/2
			}
		}
	}
	if f.k%120 == 0 {
		for key, a := range f.acft {
			if tE-a.lastS > cfg.BlacklistS {
				delete(f.acft, key)
			}
		}
	}
	f.mu.Lock()
	// Cap (finding ROB-2): when more than BlacklistMaxFrac of the sync
	// aircraft would be blacklisted the fault is in stations, not in the
	// aircraft; stop blacklisting and count it instead.
	limit := max(int(cfg.BlacklistMaxFrac*float64(len(f.acft)+len(f.black))), 1)
	room := max(limit-len(f.black), 0)
	if len(add) > room {
		capped := len(add) - room
		f.stats.BlacklistCapped += uint64(capped)
		add = add[:room]
		f.mu.Unlock()
		f.logf("clock: %d aircraft not blacklisted: %.0f %% of the sync aircraft already are (station fault?)", capped, 100*cfg.BlacklistMaxFrac)
		f.mu.Lock()
	}
	for _, key := range add {
		f.black[key] = tE + cfg.BlacklistS
	}
	f.stats.BlacklistedAc = len(f.black)
	f.mu.Unlock()
	if len(add) > 0 {
		f.logf("clock: %d aircraft blacklisted from sync", len(add))
		if cfg.OnBlacklist != nil {
			for _, key := range add {
				cfg.OnBlacklist(key, cfg.OriginUnixNs+f.shiftS*1e9+int64((tE+cfg.BlacklistS)*1e9))
			}
		}
	}
}

// influenceCap sets each station's measurement-variance inflation for the
// next epoch: a station whose effective observation count exceeds
// InfluenceCap × the component median is scaled back to the cap.
func (f *Filter) influenceCap(c *component) {
	var cnt []float64
	for _, s := range c.sts {
		if s.influ > 0 {
			cnt = append(cnt, s.influ)
		}
	}
	capV := math.Inf(1)
	if len(cnt) >= 3 {
		slices.Sort(cnt)
		capV = f.cfg.InfluenceCap * cnt[len(cnt)/2]
	}
	for _, s := range c.sts {
		s.capInfl = 1
		if s.influ > capV {
			s.capInfl = s.influ / capV
		}
	}
}
