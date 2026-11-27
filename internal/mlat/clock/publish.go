package clock

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// publish builds the snapshot of the epoch and stores it.
func (f *Filter) publish(tE float64) {
	f.seq++
	cfg := &f.cfg
	snap := &Snapshot{Seq: f.seq, T: tE + float64(f.shiftS), shiftS: f.shiftS, OriginUnixNs: cfg.OriginUnixNs, EpochS: cfg.EpochS,
		frameRef: cfg.FrameRefNs, prop: f.prop}
	if prev := f.snap.Load(); prev != nil && prev.pos != nil && !f.posDirt {
		snap.pos = prev.pos
	} else {
		snap.pos = make(map[model.StationIdx]geo.Vec3, len(f.posMap))
		for k, v := range f.posMap {
			snap.pos[k] = v
		}
		f.posDirt = false
	}
	snap.stations = make([]*StationClock, len(f.st))
	win := int64(math.Ceil(cfg.SyncedWindowS / cfg.EpochS))
	for _, c := range f.comps {
		ci := &ComponentInfo{ID: c.id, Domain: c.domain, Relative: c.relative, Anchors: c.anchors, N: c.n,
			P: append([]float64(nil), c.P...)}
		for _, s := range c.sts {
			ci.Stations = append(ci.Stations, s.idx)
			snap.stations[s.idx] = f.stationClock(s, c, ci, tE, win)
		}
		snap.Components = append(snap.Components, ci)
	}
	for _, s := range f.st {
		if s == nil || s.comp != nil || !s.known {
			continue
		}
		snap.stations[s.idx] = &StationClock{Station: s.idx, Class: s.cls, Declared: s.info.Class, Epoch: s.epoch,
			AnchorState: anchorState(s), AnchorReason: s.anchorReason, JitterNs: s.jitter(), QScale: s.qScale,
			JitterScale: s.jitScale, OutlierPct: 100 * s.outEWMA, Steps: s.nSteps, Events: f.pubEvents(s),
			SigmaThetaNs: math.Inf(1)}
	}
	f.snap.Store(snap)
	pref := make(map[model.StationIdx]bool, len(snap.stations))
	for _, sc := range snap.stations {
		if sc != nil && (sc.Anchor || (sc.Synced && sc.SigmaThetaNs < 50)) {
			pref[sc.Station] = true
		}
	}
	f.mu.Lock()
	f.pref = pref
	f.mu.Unlock()
}

func anchorState(s *station) string {
	switch {
	case s.info.Flagged:
		return AnchorFlagged
	case s.mode == modeAnchor && s.selfAsserted:
		return AnchorSelfAsserted
	case s.mode == modeAnchor:
		return AnchorActive
	case s.demoted:
		return AnchorDemoted
	case s.anchorCapable():
		return AnchorProbation
	}
	return AnchorNone
}

func (f *Filter) stationClock(s *station, c *component, ci *ComponentInfo, tE float64, win int64) *StationClock {
	n := c.n
	sc := &StationClock{
		Station: s.idx, Class: s.cls, Declared: s.info.Class, Epoch: s.epoch,
		BaseTicks: s.baseTicks, BaseS: s.baseS + float64(f.shiftS), baseS: s.baseS, shiftS: f.shiftS, Hz: s.hz,
		Comp: c.id, Domain: c.domain, comp: ci, off: s.off, layout: s.layout, t: tE,
		mode: s.mode, psi: s.psi && s.mode == modeFree, qScale: s.qScale, prior: s.prior,
		Anchor: s.mode == modeAnchor, AnchorState: anchorState(s), AnchorReason: s.anchorReason,
		Suspect: s.suspect >= 2, RelativeTime: c.relative,
		JitterNs: s.jitter(), QScale: s.qScale, JitterScale: s.jitScale,
		Accepted30s: s.accIn(f.k, win), OutlierPct: 100 * s.outEWMA, Steps: s.nSteps,
	}
	var xs [numKinds]float64
	for k := stateKind(0); k < numKinds; k++ {
		lk := s.layout[k]
		if lk < 0 {
			continue
		}
		i := s.off + int(lk)
		xs[k] = c.x[i]
		for j := stateKind(0); j < numKinds; j++ {
			if lj := s.layout[j]; lj >= 0 {
				sc.Cov[k][j] = c.P[i*n+s.off+int(lj)]
			}
		}
	}
	sc.Theta, sc.Phi, sc.Psi, sc.Beta = xs[kTheta], xs[kPhi], xs[kPsi], xs[kBeta]
	if s.mode == modeAnchor {
		sc.Phi, sc.Psi = 0, 0
	}
	sc.SigmaThetaNs = math.Sqrt(math.Max(sc.Cov[0][0], 0))
	sc.ppsVar = f.cfg.PPSCorrelatedNs * f.cfg.PPSCorrelatedNs
	sc.Synced = f.syncedNow(c, s, f.k, win)
	if len(s.events) > 0 {
		sc.Events = f.pubEvents(s)
	}
	if len(s.peers) > 0 {
		cur := f.k / max(win, 1)
		for idx, p := range s.peers {
			cnt := int(p.n)
			if p.win != cur {
				cnt = 0
			}
			if p.win == cur || p.win == cur-1 {
				cnt = max(cnt, int(p.last))
			}
			if cnt == 0 && p.win < cur-1 {
				delete(s.peers, idx)
				continue
			}
			ps := PeerStat{Peer: idx, Count: cnt, RMSNs: math.Sqrt(p.sq)}
			if o := f.station(idx, false); o != nil && o.comp != nil && o.comp.domain == c.domain {
				ps.SameDom = true
				if l := o.layout[kPhi]; l >= 0 && o.mode == modeFree {
					ps.RelPPB = sc.Phi - o.comp.x[o.off+int(l)]
				} else {
					ps.RelPPB = sc.Phi
				}
			}
			sc.Peers = append(sc.Peers, ps)
		}
	}
	return sc
}

// pubEvents copies a station's events with their times in network seconds
// since OriginUnixNs (internally they are kept relative to the moving
// origin).
func (f *Filter) pubEvents(s *station) []EventRecord {
	if len(s.events) == 0 {
		return nil
	}
	out := append([]EventRecord(nil), s.events...)
	for i := range out {
		out[i].T += float64(f.shiftS)
	}
	return out
}
