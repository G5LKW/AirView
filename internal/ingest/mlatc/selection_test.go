package mlatc

import "testing"

// Finding ROB-3: seenAdd capped the client's own seen set at MaxSeen but
// the full list went on to the MLAT planner. Only addresses that enter the
// set are reported now; repeats and those beyond the cap are not.
func TestSeenAddReportsOnlyFresh(t *testing.T) {
	s := newSelection(10, 100)
	var buf []uint32
	list := make([]uint32, 8000)
	for i := range list {
		list[i] = uint32(i + 1)
	}
	buf = s.seenAdd(list, buf[:0])
	if len(buf) != 100 || len(s.seen) != 100 {
		t.Fatalf("first flood: %d forwarded, %d remembered; want 100 (MaxSeen)", len(buf), len(s.seen))
	}
	buf = s.seenAdd(list, buf[:0])
	if len(buf) != 0 {
		t.Fatalf("a repeated list forwarded %d addresses", len(buf))
	}
	s.seenDel(list[:10])
	buf = s.seenAdd(list[5000:5020], buf[:0])
	if len(buf) != 10 {
		t.Fatalf("after 10 lost, %d new addresses forwarded, want 10", len(buf))
	}
}
