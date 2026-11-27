package tracker

import (
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// C1: a diversity copy (the same transmission on another channel of the
// same station) moves only that channel's counters, never the station's.
func TestCreditChannelOnlyChannel(t *testing.T) {
	ls := NewLanes(LaneOptions{})
	const st = model.StationIdx(2)
	tMs := int64(1_700_000_000_000)
	for i := range 10 {
		r := Result{Status: ResultOK, Key: 0x4840D6, DF: 11, TMs: tMs + int64(i)*100, Pos: true, Shown: true,
			Lat: 52, Lon: 9, AltFt: 3000}
		ls.Credit(st, 0, -20, &r)        // first copy, channel 0
		ls.CreditChannel(st, 1, -22, &r) // diversity copy, channel 1
	}
	info, ok := ls.Info(st)
	if !ok || info.Frames != 10 || info.Positions != 10 {
		t.Fatalf("station counters %+v, want 10 frames and 10 positions", info)
	}
	sh := &ls.shards[st%laneShards]
	sh.mu.Lock()
	l := sh.lanes[int(st/laneShards)]
	ch0, ch1, all := l.ch[0].totFrames, l.ch[1].totFrames, l.all.totFrames
	p1 := l.ch[1].totPositions
	sh.mu.Unlock()
	if ch0 != 10 || ch1 != 10 || all != 10 || p1 != 10 {
		t.Fatalf("ch0 %d ch1 %d all %d ch1 positions %d", ch0, ch1, all, p1)
	}
	// No channel: nothing to credit.
	r := Result{Status: ResultOK, Key: 1, TMs: tMs + 2000}
	ls.CreditChannel(st, -1, -20, &r)
	if info2, _ := ls.Info(st); info2.Frames != 10 {
		t.Fatalf("channel-less diversity copy counted: %+v", info2)
	}
}

// C4: a dropped (deleted) station's lane writes no more rows and is not
// re-created by later copies.
func TestLanesDrop(t *testing.T) {
	st := &countStore{}
	ls := NewLanes(LaneOptions{Store: st})
	const idx = model.StationIdx(7)
	tMs := int64(1_700_000_000_000)
	r := Result{Status: ResultOK, Key: 0x4840D6, DF: 11, TMs: tMs}
	ls.Credit(idx, 0, -20, &r)
	ls.Drop(idx)
	if _, ok := ls.Info(idx); ok {
		t.Fatal("lane still listed after Drop")
	}
	for i := range 100 {
		r.TMs = tMs + int64(i)*1000
		ls.Credit(idx, 0, -20, &r)
	}
	if _, ok := ls.Info(idx); ok {
		t.Fatal("lane re-created after Drop")
	}
	ls.FlushAll(tMs + 3_600_000)
	if st.rows != 0 {
		t.Fatalf("%d rows written for the dropped station", st.rows)
	}
}

// countStore counts the rows submitted by the lanes.
type countStore struct{ rows int }

func (c *countStore) Submit(b *store.Batch) { c.rows += b.Len() }
