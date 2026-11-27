package clock

import (
	"slices"

	"airview/internal/model"
)

// restructure recomputes the connected components of the co-observation
// graph (edges seen in the last CoObsWindowS), partitions domains larger
// than MaxComponentStations, and rebuilds every component whose station
// set or block layout changed. Unchanged components keep their state
// untouched.
func (f *Filter) restructure(tE float64) {
	cfg := &f.cfg
	horizon := tE - cfg.CoObsWindowS
	// Every existing component is brought to the epoch time first, while
	// its stations' offsets and layouts still describe it. buildComponent
	// rewrites s.off/s.layout/s.comp of the stations it takes over, so a
	// component that is split over several parts must never be predicted
	// after the first part was built from it: predictBlock would then walk
	// the old station list with the NEW offsets (slice out of range at about
	// 1,000 feeders, finding SCALE-01, or a silently corrupted covariance).
	// Step predicts every component to tE right after restructure anyway, so
	// doing it here changes nothing for components that stay as they are.
	for _, c := range f.comps {
		if c.tE < tE {
			c.predict(tE)
		}
	}
	active := func(s *station) bool {
		return s != nil && s.usable() && s.hasEpoch && tE-s.lastObsS <= cfg.StationTimeoutS
	}
	// union-find over active stations
	parent := map[model.StationIdx]model.StationIdx{}
	var find func(model.StationIdx) model.StationIdx
	find = func(a model.StationIdx) model.StationIdx {
		for parent[a] != a {
			parent[a] = parent[parent[a]]
			a = parent[a]
		}
		return a
	}
	for key, last := range f.edges {
		if last < horizon {
			delete(f.edges, key)
			continue
		}
		a, b := model.StationIdx(key>>32), model.StationIdx(key&0xFFFFFFFF)
		sa, sb := f.station(a, false), f.station(b, false)
		if !active(sa) || !active(sb) {
			continue
		}
		if _, ok := parent[a]; !ok {
			parent[a] = a
		}
		if _, ok := parent[b]; !ok {
			parent[b] = b
		}
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	setsBy := map[model.StationIdx][]*station{}
	for a := range parent {
		r := find(a)
		setsBy[r] = append(setsBy[r], f.st[a])
	}
	// stations that left every component lose their state
	for _, s := range f.st {
		if s == nil || s.comp == nil {
			continue
		}
		if _, ok := parent[s.idx]; !ok {
			s.comp = nil
			s.fresh = true
			s.hasEpoch = false // re-based at the next member
			if s.mode == modeAnchor {
				s.demoteReq = ""
				s.mode = modeFree
				s.selfAsserted = false
				s.addEvent(tE, "anchor_lost", "no co-observations")
			}
		}
	}
	// deterministic order
	roots := make([]model.StationIdx, 0, len(setsBy))
	for r := range setsBy {
		roots = append(roots, r)
	}
	slices.Sort(roots)

	oldDomAnch := map[uint32]bool{}
	for _, c := range f.comps {
		for _, s := range c.sts {
			if s.mode == modeAnchor && s.layout[kPhi] < 0 {
				oldDomAnch[c.domain] = true
			}
		}
	}
	claimed := map[*component]bool{}
	usedDom := map[uint32]bool{}
	var out []*component
	for _, r := range roots {
		set := setsBy[r]
		sortStations(set)
		// time domain: the old domain holding most of the set (anchored first)
		domCount := map[uint32]int{}
		for _, s := range set {
			if s.comp != nil {
				domCount[s.comp.domain]++
			}
		}
		var dom uint32
		best := -1
		for d, n := range domCount {
			score := n
			if oldDomAnch[d] {
				score += 1 << 20
			}
			if score > best || (score == best && d < dom) {
				dom, best = d, score
			}
		}
		same := dom // the old time scale this set continues (0 = none)
		if best < 0 || usedDom[dom] {
			// a new domain, or the second part of a split one (same time
			// scale, new identity)
			f.nextID++
			dom = f.nextID
		}
		usedDom[dom] = true
		// stations from another time scale rejoin with fresh variances
		for _, s := range set {
			if s.comp != nil && s.comp.domain != same && !(oldDomAnch[s.comp.domain] && oldDomAnch[same]) {
				s.rejoin = true
			}
		}
		parts := [][]*station{set}
		if splitDomain(set, same, cfg) {
			parts = f.partition(set, cfg.PartitionStations)
		}
		for _, part := range parts {
			out = append(out, f.buildPart(part, dom, claimed, tE))
		}
	}
	f.comps = out
	f.stats.Components = len(out)
	n := 0
	for _, c := range out {
		n += len(c.sts)
	}
	f.stats.Stations = n
}

// buildPart returns the component for one station set: the old component
// it overlaps most when nothing changed, otherwise a rebuilt one.
func (f *Filter) buildPart(part []*station, dom uint32, claimed map[*component]bool, tE float64) *component {
	over := map[*component]int{}
	for _, s := range part {
		if s.comp != nil && !claimed[s.comp] {
			over[s.comp]++
		}
	}
	var owner *component
	bestN := 0
	for c, n := range over {
		if n > bestN || (n == bestN && owner != nil && c.id < owner.id) {
			owner, bestN = c, n
		}
	}
	if owner != nil {
		claimed[owner] = true
		if owner.domain == dom && sameStations(owner.sts, part) && !needsRebuild(part) {
			return owner
		}
	}
	id := uint32(0)
	if owner != nil {
		id = owner.id
	} else {
		f.nextID++
		id = f.nextID
	}
	// Every source component is already at tE (restructure predicted them
	// all before the first part was built; see SCALE-01 there). Predicting a
	// source here would be wrong: an earlier part may already have rewritten
	// the offsets of some of its stations.
	c := buildComponent(id, dom, part, tE, f.hostVar())
	for _, s := range part {
		if s.freeSince == 0 {
			s.freeSince = tE
		}
	}
	return c
}

func needsRebuild(sts []*station) bool {
	for _, s := range sts {
		if s.fresh || s.rejoin {
			return true
		}
		lay, _ := s.wantLayout()
		if lay != s.layout {
			return true
		}
	}
	return false
}

func sameStations(a, b []*station) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortStations(s []*station) {
	slices.SortFunc(s, func(a, b *station) int { return int(a.idx) - int(b.idx) })
}

func sortIdx(s []model.StationIdx) { slices.Sort(s) }

func equalIdx(a, b []model.StationIdx) bool { return slices.Equal(a, b) }

func sortCompsBySize(cs []*component) {
	slices.SortFunc(cs, func(a, b *component) int { return b.n - a.n })
}
