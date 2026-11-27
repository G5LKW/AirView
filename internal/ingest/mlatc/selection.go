package mlatc

import (
	"slices"
	"sync"

	"airview/internal/ingest"
)

// selection decides which aircraft a client is asked to send
// (start_sending / stop_sending, MLATC.md §7).
//
// The reference client sends nothing for an aircraft until the server
// requests it: neither sync pairs (DF17 positions) nor mlat frames. Two
// modes:
//
//   - full (default, small networks): every aircraft the client reports as
//     seen is requested, up to Policy.MaxRequested. The client itself
//     sends mlat frames only for requested aircraft without a good ADS-B
//     position, and sync pairs for requested ADS-B aircraft.
//   - selective (the ingest's traffic-mode policy, DESIGN §4.4): only the
//     MLAT planner's interest sets (sync ∪ mlat, ingest.SetInterest) are
//     requested, plus, for a station in timing quarantine, a small probe
//     set of its highest-rate ADS-B aircraft from its rate_report (the
//     quarantine needs sync frames to judge the station's timing).
//
// Forced entries (Server.StartSending / StopSending) override both.
type selection struct {
	mu        sync.Mutex
	selective bool
	seen      map[uint32]struct{}
	interest  map[uint32]struct{}
	probe     map[uint32]struct{} // quarantine probe (selective mode): top rate_report aircraft
	rates     map[uint32]float32  // last rate_report (ADS-B position msgs/s)
	forced    map[uint32]bool
	requested map[uint32]struct{}
	dirty     map[uint32]bool // pending change: true = start, false = stop
	maxReq    int
	maxSeen   int
	capped    uint64      // requests not made because of maxReq
	ac        acSelection // Mode A/C requests (modeac.go)
}

func newSelection(maxReq, maxSeen int) *selection {
	return &selection{
		seen: map[uint32]struct{}{}, interest: map[uint32]struct{}{}, forced: map[uint32]bool{},
		probe: map[uint32]struct{}{}, rates: map[uint32]float32{},
		requested: map[uint32]struct{}{}, dirty: map[uint32]bool{}, maxReq: maxReq, maxSeen: maxSeen,
	}
}

// wanted reports whether a should be requested. Caller holds mu.
func (s *selection) wanted(a uint32) bool {
	if f, ok := s.forced[a]; ok {
		return f
	}
	if s.selective {
		if _, ok := s.interest[a]; ok {
			return true
		}
		_, ok := s.probe[a]
		return ok
	}
	_, ok := s.seen[a]
	return ok
}

// update brings a's request state in line with wanted. Caller holds mu.
func (s *selection) update(a uint32) {
	_, req := s.requested[a]
	w := s.wanted(a)
	switch {
	case w && !req:
		if len(s.requested) >= s.maxReq {
			s.capped++
			return
		}
		s.requested[a] = struct{}{}
		s.mark(a, true)
	case !w && req:
		delete(s.requested, a)
		s.mark(a, false)
	}
}

// mark records a pending change; a change and its reversal cancel out.
func (s *selection) mark(a uint32, start bool) {
	if prev, ok := s.dirty[a]; ok && prev != start {
		delete(s.dirty, a)
		return
	}
	s.dirty[a] = start
}

// seenAdd handles a "seen" list. Caller holds mu. It appends to fresh the
// addresses that entered the seen set (new ones, within MaxSeen) and
// returns it: only those are news for the rest of the server (finding
// ROB-3: a flood of "seen" lists must not reach the MLAT planner beyond
// the per-client cap).
func (s *selection) seenAdd(addrs []uint32, fresh []uint32) []uint32 {
	for _, a := range addrs {
		if _, ok := s.seen[a]; ok {
			s.update(a) // a slot may have been freed since
			continue
		}
		if len(s.seen) >= s.maxSeen {
			continue
		}
		s.seen[a] = struct{}{}
		s.update(a)
		fresh = append(fresh, a)
	}
	return fresh
}

// seenDel handles a "lost" list. The client forgets a lost aircraft, so a
// request it held is dropped silently (no stop_sending) unless the
// selective or forced sets still want it.
func (s *selection) seenDel(addrs []uint32) {
	for _, a := range addrs {
		delete(s.seen, a)
		if f, ok := s.forced[a]; ok && !f {
			delete(s.forced, a) // a stop for an aircraft the client forgot
		}
		if _, req := s.requested[a]; req && !s.wanted(a) {
			delete(s.requested, a)
			delete(s.dirty, a)
		}
	}
}

// force sets an override. Caller holds mu. The override table is bounded:
// beyond 4 × maxReq entries new overrides are ignored (counted in capped).
func (s *selection) force(a uint32, on bool) {
	if _, ok := s.forced[a]; !ok && len(s.forced) >= 4*s.maxReq {
		s.capped++
		return
	}
	s.forced[a] = on
	s.update(a)
}

// interestCmd applies an ingest traffic-mode / interest update. Caller
// holds mu.
func (s *selection) interestCmd(in ingest.InterestCmd) {
	sel := in.Mode == ingest.ModeSelective
	if in.Mode != "" && sel != s.selective {
		s.selective = sel
	}
	if in.Reset {
		clear(s.interest)
	}
	del := func(list []uint32) {
		for _, a := range list {
			delete(s.interest, a)
		}
	}
	add := func(list []uint32) {
		for _, a := range list {
			if a <= 0xffffff { // non-ICAO keys cannot be requested
				s.interest[a] = struct{}{}
			}
		}
	}
	del(in.SyncDel)
	del(in.MLATDel)
	add(in.SyncAdd)
	add(in.MLATAdd)
	// Re-evaluate everything that may have changed: cheap at these sizes.
	for a := range s.requested {
		s.update(a)
	}
	for a := range s.interest {
		s.update(a)
	}
	for a := range s.probe {
		s.update(a)
	}
	if !s.selective {
		for a := range s.seen {
			s.update(a)
		}
	}
	for a := range s.forced {
		s.update(a)
	}
}

// setProbe makes the k highest-rate aircraft of the last rate_report the
// probe set (k = 0 clears it). Caller holds mu.
func (s *selection) setProbe(k int) {
	old := s.probe
	s.probe = make(map[uint32]struct{}, k)
	if k > 0 && len(s.rates) > 0 {
		type ar struct {
			a uint32
			r float32
		}
		all := make([]ar, 0, len(s.rates))
		for a, r := range s.rates {
			all = append(all, ar{a, r})
		}
		slices.SortFunc(all, func(x, y ar) int {
			switch {
			case x.r > y.r:
				return -1
			case x.r < y.r:
				return 1
			case x.a < y.a:
				return -1
			case x.a > y.a:
				return 1
			}
			return 0
		})
		for _, e := range all[:min(k, len(all))] {
			s.probe[e.a] = struct{}{}
		}
	}
	for a := range old {
		s.update(a)
	}
	for a := range s.probe {
		s.update(a)
	}
}

// take returns the pending start and stop lists (sorted) and clears them.
func (s *selection) take(start, stop []uint32) ([]uint32, []uint32) {
	for a, on := range s.dirty {
		if on {
			start = append(start, a)
		} else {
			stop = append(stop, a)
		}
	}
	clear(s.dirty)
	slices.Sort(start)
	slices.Sort(stop)
	return start, stop
}
