package group

import "sync/atomic"

type counters struct {
	groups, excluded, notCandidate, tooFew             atomic.Uint64
	copies, untimed, noClock, noSite, usable           atomic.Uint64
	clusters, members, solve, solveAlt, track, dropped atomic.Uint64
	split, leftover, hinted, hintConflict, notAdmitted atomic.Uint64
	budgetOut, lateGroups, lateCopies, lateAttached    atomic.Uint64
	lateClusters                                       atomic.Uint64
}

// Stats are the assembler's counters since New (admin scope).
type Stats struct {
	Groups       uint64 `json:"groups"`       // groups observed
	Excluded     uint64 `json:"excluded"`     // Mode A/C and DF18 CF>=2 groups
	NotCandidate uint64 `json:"notCandidate"` // groups of aircraft the Selector does not want
	TooFew       uint64 `json:"tooFew"`       // groups with < 2 usable copies
	Copies       uint64 `json:"copies"`       // copies seen (incl. late)
	Untimed      uint64 `json:"untimed"`      // untimed, map-only or second-channel copies
	NoClock      uint64 `json:"noClock"`      // copies of unsynced stations
	NoSite       uint64 `json:"noSite"`       // copies of stations without an MLAT position
	Usable       uint64 `json:"usable"`       // copies converted to network time
	Clusters     uint64 `json:"clusters"`     // clusters emitted (incl. late)
	Members      uint64 `json:"members"`      // members of emitted clusters
	Solve        uint64 `json:"solve"`        // clusters with Use = solve
	SolveAlt     uint64 `json:"solveAlt"`     // clusters with Use = solve_alt
	Track        uint64 `json:"track"`        // clusters with Use = track
	Dropped      uint64 `json:"dropped"`      // cliques with < 2 sites (co-located only)
	Split        uint64 `json:"split"`        // groups that gave more than one cluster
	Leftover     uint64 `json:"leftover"`     // usable copies in no cluster
	Hinted       uint64 `json:"hinted"`       // groups clustered with a position hint
	HintConflict uint64 `json:"hintConflict"` // hints rejected as inconsistent
	NotAdmitted  uint64 `json:"notAdmitted"`  // solvable clusters the Selector downgraded
	BudgetOut    uint64 `json:"budgetOut"`    // clique searches that ran out of budget
	LateGroups   uint64 `json:"lateGroups"`   // late deliveries
	LateCopies   uint64 `json:"lateCopies"`   // usable late copies
	LateAttached uint64 `json:"lateAttached"` // late copies joined to a retained cluster
	LateClusters uint64 `json:"lateClusters"` // clusters emitted with Late set
}

// Stats returns a snapshot of the counters.
func (a *Assembler) Stats() Stats {
	c := &a.stats
	return Stats{
		Groups: c.groups.Load(), Excluded: c.excluded.Load(), NotCandidate: c.notCandidate.Load(), TooFew: c.tooFew.Load(),
		Copies: c.copies.Load(), Untimed: c.untimed.Load(), NoClock: c.noClock.Load(), NoSite: c.noSite.Load(),
		Usable: c.usable.Load(), Clusters: c.clusters.Load(), Members: c.members.Load(), Solve: c.solve.Load(),
		SolveAlt: c.solveAlt.Load(), Track: c.track.Load(), Dropped: c.dropped.Load(), Split: c.split.Load(),
		Leftover: c.leftover.Load(), Hinted: c.hinted.Load(), HintConflict: c.hintConflict.Load(),
		NotAdmitted: c.notAdmitted.Load(), BudgetOut: c.budgetOut.Load(), LateGroups: c.lateGroups.Load(),
		LateCopies: c.lateCopies.Load(), LateAttached: c.lateAttached.Load(), LateClusters: c.lateClusters.Load(),
	}
}
