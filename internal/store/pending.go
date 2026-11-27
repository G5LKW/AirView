package store

// PendingIncs is what the tracker has accumulated in memory but not written
// yet (DB-EFFICIENCY-DESIGN §3.3): the coverage / signal / elevation / heat
// increments since their last flush and the partial range_hour rows of the
// open hour. A reader that covers "now" merges it into the stored rows with
// the same rules the writer uses (Merge* / AddRangeHours), so a page answers
// exactly what it would answer if the increments had been written.
//
// Rows may repeat a key (one tracker shard each in aggregator mode): merge
// them additively like the writer does. Every slice is a copy that belongs
// to the caller; nil when there is nothing pending.
type PendingIncs struct {
	Coverage []CoverageInc
	Signal   []SignalHistInc
	Elev     []ElevHistInc
	Heat     []HeatInc
	Range    []RangeHour
}

// Empty reports whether nothing is pending.
func (p *PendingIncs) Empty() bool {
	return len(p.Coverage) == 0 && len(p.Signal) == 0 && len(p.Elev) == 0 && len(p.Heat) == 0 && len(p.Range) == 0
}

// Append adds the rows of q to p.
func (p *PendingIncs) Append(q PendingIncs) {
	p.Coverage = append(p.Coverage, q.Coverage...)
	p.Signal = append(p.Signal, q.Signal...)
	p.Elev = append(p.Elev, q.Elev...)
	p.Heat = append(p.Heat, q.Heat...)
	p.Range = append(p.Range, q.Range...)
}
