package plan

import (
	"testing"

	"airview/internal/model"
)

func tableSize(p *Planner) (n, pend int) {
	for i := range p.t.sh {
		s := &p.t.sh[i]
		s.mu.Lock()
		n += len(s.idx)
		pend += len(s.pend)
		s.mu.Unlock()
	}
	return
}

// Finding ROB-3: visibility lists from one station created a table entry
// per address (4 GB in 60 s from a "seen" flood). A single station's
// report is only pending now; a second station corroborates it.
func TestNoteHearerNeedsCorroboration(t *testing.T) {
	p := newPlanner(t, stMap{}, nil)
	now := t0 + 1e9
	for a := uint32(1); a <= 5000; a++ {
		p.NoteHearer(a, 7, now)
	}
	if n, _ := tableSize(p); n != 0 {
		t.Fatalf("one station's visibility list created %d aircraft", n)
	}
	// a second station within HeardTTL corroborates
	p.NoteHearer(42, 8, now+5e9)
	if p.t.sh[shardOf(42)].lookup(42) == nil {
		t.Fatal("second station did not create the aircraft")
	}
	if n, _ := tableSize(p); n != 1 {
		t.Fatalf("%d aircraft, want 1", n)
	}
	// the same station again does not
	p.NoteHearer(43, 7, now+6e9)
	if p.t.sh[shardOf(43)].lookup(43) != nil {
		t.Fatal("the same station corroborated itself")
	}
	// too late does not either
	p.NoteHearer(44, 8, now+int64(p.cfg.HeardTTL)+2e9)
	if p.t.sh[shardOf(44)].lookup(44) != nil {
		t.Fatal("a report older than HeardTTL corroborated")
	}
	// pending reports are bounded
	for a := uint32(100000); a < 200000; a++ {
		p.NoteHearer(a, 9, now)
	}
	if _, pend := tableSize(p); pend > p.cfg.MaxAircraft+nShards*16 {
		t.Fatalf("%d pending reports, bound %d", pend, p.cfg.MaxAircraft)
	}
}

// Finding ROB-3: the table is bounded by MaxAircraft with approximate LRU
// eviction, whatever creates the entries.
func TestTableBounded(t *testing.T) {
	p := newPlanner(t, stMap{}, func(o *Options) { o.Config.MaxAircraft = 2000 })
	now := t0 + 1e9
	for a := uint32(1); a <= 50000; a++ {
		p.NoteADSB(a, now+int64(a)*1e6, false)
	}
	n, _ := tableSize(p)
	if n > 2000+nShards {
		t.Fatalf("%d aircraft in a table bounded at 2000", n)
	}
	// the most recent aircraft survive
	kept := 0
	for a := uint32(49900); a <= 50000; a++ {
		if p.t.sh[shardOf(a)].lookup(a) != nil {
			kept++
		}
	}
	if kept < 90 {
		t.Fatalf("only %d of the 101 most recent aircraft kept", kept)
	}
	// two-station visibility reports are bounded too
	for a := uint32(60000); a < 120000; a++ {
		p.NoteHearer(a, model.StationIdx(3), now)
		p.NoteHearer(a, model.StationIdx(4), now)
	}
	if n, _ := tableSize(p); n > 2000+nShards {
		t.Fatalf("%d aircraft after a corroborated flood", n)
	}
}
