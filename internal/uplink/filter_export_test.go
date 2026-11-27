package uplink

import (
	"testing"

	"airview/internal/avf"
)

// SO-12: the exported filter behaves like the feeder's: full mode passes
// everything; selective mode passes the sync set and reduces the rest.
func TestExportedFilter(t *testing.T) {
	f := NewFilter()
	a, b := df11(0x4840D6), df11(0x3C6586)
	for i := 0; i < 10; i++ {
		if !f.Pass(a, int64(i)*1e8) {
			t.Fatal("full mode dropped a frame")
		}
	}
	f.Apply(&avf.Interest{Mode: avf.ModeSelective, ReduceMs: 1000, SyncAdd: []string{"4840D6"}})
	if !f.Selective() {
		t.Fatal("not selective")
	}
	pa, pb := 0, 0
	for i := 0; i < 20; i++ { // 2 s at 10 Hz
		ns := int64(10e9) + int64(i)*1e8
		if f.Pass(a, ns) {
			pa++
		}
		if f.Pass(b, ns) {
			pb++
		}
	}
	if pa != 20 || pb < 1 || pb > 3 {
		t.Fatalf("sync aircraft passed %d/20, other %d/20 (want about 2)", pa, pb)
	}
}
