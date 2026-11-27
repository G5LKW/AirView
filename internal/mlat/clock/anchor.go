package clock

import (
	"math"
	"slices"
	"strings"

	"airview/internal/model"
)

// anchorPass applies the anchor rules of DESIGN §9.2.5 once per epoch:
// demotions (status lost, step, drift, flag, failed revalidation), the
// k·1 s leap/legacy offset of class 2, promotion after probation, the
// first (self-asserted or consensus) anchor of an anchor-less domain, and
// the revalidation of a self-asserted anchor.
func (f *Filter) anchorPass(tE float64) {
	for _, c := range f.comps {
		for _, s := range c.sts {
			if s.mode == modeAnchor && (s.demoteReq != "" || s.info.Flagged) {
				r := s.demoteReq
				if r == "" {
					r = "flagged"
				}
				f.demote(c, s, r, tE)
			}
			s.demoteReq = ""
			f.reprobation(s, tE)
		}
	}
	doms := map[uint32][]*component{}
	var order []uint32
	for _, c := range f.comps {
		if _, ok := doms[c.domain]; !ok {
			order = append(order, c.domain)
		}
		doms[c.domain] = append(doms[c.domain], c)
	}
	for _, d := range order {
		f.anchorDomain(doms[d], tE)
	}
}

func (f *Filter) condA(s *station, tE float64) bool {
	return s.info.NoStatus || (s.gpsKnown && s.gpsOK && tE-s.gpsOKSince >= f.cfg.ProbationS)
}

// hostKnown reports whether the station's server-time comparison has
// enough samples, hostAgrees whether its clock is within AnchorHostMaxS of
// the server's time, and hostAgreesAfter whether it would be after a leap
// of k seconds (finding ROB-1).
func (f *Filter) hostKnown(s *station) bool { return s.hostDevN >= f.cfg.AnchorHostMinObs }

func (f *Filter) hostAgrees(s *station) bool {
	return f.hostKnown(s) && math.Abs(s.hostDevS) < f.cfg.AnchorHostMaxS
}

func (f *Filter) hostAgreesAfter(s *station, k float64) bool {
	return f.hostKnown(s) && math.Abs(s.hostDevS-k) < f.cfg.AnchorHostMaxS
}

// reprobation lets a station that was demoted for a transient reason (GPS
// status loss, receiver epoch event) back into probation once condition
// (a) holds again: its declared GPS class is restored and the usual
// probation rules (condition (b) against the other anchors, or the first-
// anchor rules) decide its promotion (finding ROB-5).
func (f *Filter) reprobation(s *station, tE float64) {
	if !s.demoted || !s.demotedTransient || s.mode != modeFree || s.info.Flagged {
		return
	}
	cls := s.info.Class
	if cls != model.ClockGPSUTC && cls != model.ClockGPSGPST {
		return
	}
	ok := s.gpsKnown && s.gpsOK && tE-s.gpsOKSince >= f.cfg.ProbationS
	if s.info.NoStatus {
		ok = tE-s.freeSince >= f.cfg.ProbationS
	}
	if !ok {
		return
	}
	s.demoted, s.demotedTransient = false, false
	if cls == model.ClockGPSGPST && s.leapApplied {
		// the GPS-time offset is already in the station's base (MLAT-R1);
		// a later epoch re-base that brings it back is caught by the
		// > 0.5 s leap rule, which applies to every GPS class
		cls = model.ClockGPSUTC
	}
	s.cls = cls
	s.prior = priorFor(cls)
	s.addEvent(tE, "probation", "condition (a) restored")
	f.logf("clock: station %d re-enters anchor probation", s.idx)
}

// est returns a station's θ, φ and their sigmas.
func est(s *station) (th, ph, sth, sph float64) {
	c := s.comp
	n := c.n
	it := s.off + int(s.layout[kTheta])
	th, sth = c.x[it], math.Sqrt(math.Max(c.P[it*n+it], 0))
	if l := s.layout[kPhi]; l >= 0 && s.mode == modeFree {
		ip := s.off + int(l)
		ph, sph = c.x[ip], math.Sqrt(math.Max(c.P[ip*n+ip], 0))
	}
	return
}

func (f *Filter) anchorDomain(comps []*component, tE float64) {
	cfg := &f.cfg
	win := int64(math.Ceil(cfg.SyncedWindowS / cfg.EpochS))
	var anchors, cands []*station
	for _, c := range comps {
		for _, s := range c.sts {
			switch {
			case s.mode == modeAnchor:
				anchors = append(anchors, s)
			case s.anchorCapable() && s.mode == modeFree && s.dim > 0:
				cands = append(cands, s)
			}
		}
	}
	if len(cands) == 0 {
		return
	}
	if len(anchors) == 0 {
		f.firstAnchors(comps, cands, tE, win)
		return
	}
	var ds []float64
	for _, a := range anchors {
		th, _, _, _ := est(a)
		ds = append(ds, th)
	}
	slices.Sort(ds)
	med := ds[len(ds)/2]
	if len(ds)%2 == 0 {
		med = (ds[len(ds)/2-1] + ds[len(ds)/2]) / 2
	}
	// Validated: at least one anchor is not self-asserted. Against an
	// unvalidated (self-asserted) anchor no k·1 s leap is applied: a
	// station that disagrees by whole seconds while its own clock agrees
	// with server time exposes the anchor instead (finding ROB-1).
	validated := false
	for _, a := range anchors {
		if !a.selfAsserted {
			validated = true
		}
	}
	var disagree []*station
	agree, offAgainst := 0, 0
	for _, s := range cands {
		if !f.condA(s, tE) || !f.syncedNow(s.comp, s, f.k, win) {
			continue
		}
		th, ph, _, sph := est(s)
		d := th - med
		// class 2 or a legacy firmware: an offset of exactly k·1 s
		if math.Abs(d) > 0.5e9 || s.cls == model.ClockGPSGPST {
			k := math.Round(d / 1e9)
			if k != 0 && math.Abs(d-k*1e9) < cfg.LeapToleranceNs {
				switch {
				case !validated:
					if f.hostAgrees(s) {
						offAgainst++
					}
				case f.hostAgreesAfter(s, k):
					// the leap brings the station to server time
					f.applyLeap(s, k, tE)
					d -= k * 1e9
				}
			}
		}
		if s.cls == model.ClockGPSGPST {
			continue // GPS time base without a confirmed offset
		}
		if !validated && math.Abs(d) > 2*cfg.AnchorOffsetMaxNs && math.Abs(d) <= 0.5e9 && f.hostAgrees(s) {
			// a sub-second offset against the self-asserted anchor
			if _, _, sth, _ := est(s); math.Abs(d) > 2*cfg.AnchorOffsetMaxNs+3*sth {
				offAgainst++
			}
		}
		free := tE - s.freeSince
		if free >= math.Min(cfg.ProbationS, 120) && sph < cfg.AnchorFreqMaxPPB/4 {
			if math.Abs(ph) > cfg.AnchorFreqMaxPPB+3*sph {
				disagree = append(disagree, s)
			} else if math.Abs(ph) < cfg.AnchorFreqMaxPPB {
				agree++
			}
		}
		if free >= cfg.ProbationS && math.Abs(ph) < cfg.AnchorFreqMaxPPB && math.Abs(d) < cfg.AnchorOffsetMaxNs {
			f.promote(s, false, "probation", tE)
		}
	}
	// revalidation of a self-asserted first anchor
	if len(anchors) == 1 && anchors[0].selfAsserted {
		a := anchors[0]
		switch {
		case f.hostKnown(a) && !f.hostAgrees(a):
			// its clock does not tell server time (to within seconds)
			f.demote(a.comp, a, "host_time", tE)
		case offAgainst > 0 && !f.hostAgrees(a):
			f.demote(a.comp, a, "revalidation", tE)
		case offAgainst >= 2:
			// two status-bearing stations that agree with server time
			// disagree with it: it is the odd one out
			f.demote(a.comp, a, "revalidation", tE)
		case agree > 0 && offAgainst == 0:
			a.selfAsserted = false
			a.addEvent(tE, "validated", "agrees with a probation station")
		case len(disagree) >= 2 && consistentSet(disagree, cfg.AnchorFreqMaxPPB, 2*cfg.AnchorOffsetMaxNs):
			f.demote(a.comp, a, "revalidation", tE)
		}
	} else if len(anchors) >= 2 {
		for _, a := range anchors {
			a.selfAsserted = false
		}
	}
}

// consistentSet reports whether every pair of the stations agrees in
// frequency and offset.
func consistentSet(set []*station, fmax, omax float64) bool {
	for i := range set {
		for j := 0; j < i; j++ {
			if !consistent(set[i], set[j], fmax, omax) {
				return false
			}
		}
	}
	return true
}

func consistent(a, b *station, fmax, omax float64) bool {
	ta, pa, _, _ := est(a)
	tb, pb, _, _ := est(b)
	return math.Abs(pa-pb) < fmax && math.Abs(ta-tb) < omax
}

// firstAnchors admits the first anchors of an anchor-less domain on
// condition (a) alone: the largest mutually consistent set of candidates
// (two or more), or the only candidate (self-asserted). The domain is
// re-gauged to the new anchors' mean clock first.
func (f *Filter) firstAnchors(comps []*component, cands []*station, tE float64, win int64) {
	cfg := &f.cfg
	var ok []*station
	for _, s := range cands {
		if s.cls != model.ClockGPSUTC || !f.condA(s, tE) {
			continue
		}
		// condition (a) is the feeder's own word: the clock must also
		// tell server time to within AnchorHostMaxS (finding ROB-1)
		if !f.hostAgrees(s) {
			continue
		}
		if s.accIn(f.k, win) < cfg.SyncedMinObs || s.suspect >= 2 {
			continue
		}
		if _, _, sth, _ := est(s); sth > 1000 {
			continue
		}
		ok = append(ok, s)
	}
	if len(ok) == 0 {
		return
	}
	var best []*station
	for i, a := range ok {
		set := []*station{a}
		for j, b := range ok {
			if i == j {
				continue
			}
			good := true
			for _, x := range set {
				if !consistent(x, b, cfg.AnchorFreqMaxPPB, 2*cfg.AnchorOffsetMaxNs) {
					good = false
					break
				}
			}
			if good {
				set = append(set, b)
			}
		}
		if len(set) > len(best) {
			best = set
		}
	}
	if len(best) < 2 && len(ok) > 1 {
		return // candidates disagree: wait
	}
	var mth, mph float64
	for _, s := range best {
		th, ph, _, _ := est(s)
		mth += th
		mph += ph
	}
	mth /= float64(len(best))
	mph /= float64(len(best))
	for _, c := range comps {
		c.regauge(mth*1e-9, mph)
	}
	self := len(best) == 1
	reason := "consensus"
	if self {
		reason = "first_anchor"
	}
	for _, s := range best {
		f.promote(s, self, reason, tE)
	}
}

// promote turns a free GPS clock into an anchor: φ (and ψ) are conditioned
// to 0 in place; the block shrinks to (δ, β) at the next restructure.
func (f *Filter) promote(s *station, self bool, reason string, tE float64) {
	c := s.comp
	if s.psi && s.layout[kPsi] >= 0 {
		c.condition(s.off+int(s.layout[kPsi]), 0)
	}
	if l := s.layout[kPhi]; l >= 0 {
		c.condition(s.off+int(l), 0)
	}
	s.mode = modeAnchor
	s.selfAsserted = self
	s.anchorSince = tE
	s.anchorReason = reason
	s.drift = s.drift[:0]
	s.looSum, s.looN, s.driftBin0 = 0, 0, 0
	s.addEvent(tE, "promoted", reason)
	f.logf("clock: station %d promoted to anchor (%s)", s.idx, reason)
}

// demote turns an anchor into a class-3 free clock (DESIGN §9.2.5): the
// block gains φ at the next restructure, var(θ) grows by (1 µs)² (a step
// has already reset it).
func (f *Filter) demote(c *component, s *station, reason string, tE float64) {
	s.mode = modeFree
	s.demoted = true
	s.demotedTransient = reason == "gps_status" || strings.HasPrefix(reason, "epoch_")
	s.selfAsserted = false
	s.cls = model.ClockGPSEmul
	s.prior = priorFor(model.ClockGPSEmul)
	s.psi = false
	s.freeSince = tE
	s.anchorReason = reason
	if reason != "step" {
		it := s.off + int(s.layout[kTheta])
		c.P[it*c.n+it] += 1e6
	}
	s.addEvent(tE, "demoted", reason)
	f.logf("clock: station %d demoted (%s)", s.idx, reason)
}

// applyLeap applies an offset of exactly k seconds (GPS time base or legacy
// firmware) to a GPS station and makes its class UTC.
func (f *Filter) applyLeap(s *station, k, tE float64) {
	c := s.comp
	s.baseS -= k
	s.hostDevS -= k
	c.x[s.off+int(s.layout[kTheta])] -= k * 1e9
	if s.cls == model.ClockGPSGPST {
		s.cls = model.ClockGPSUTC
		s.prior = priorFor(model.ClockGPSUTC)
	}
	s.leapApplied = true
	s.addEvent(tE, "leap", formatSec(k))
}

func formatSec(k float64) string {
	b := []byte{}
	if k < 0 {
		b = append(b, '-')
		k = -k
	}
	n := int(k)
	var d []byte
	for n > 0 || len(d) == 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(append(append(b, d...), " s"...))
}
