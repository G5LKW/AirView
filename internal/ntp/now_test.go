package ntp

import "testing"

// Now runs for every socket read of the feeds and every handled frame: it
// must not allocate.
func TestNowNoAlloc(t *testing.T) {
	if a := testing.AllocsPerRun(1000, func() { _ = Now() }); a != 0 {
		t.Fatalf("Now allocates %.1f times per call", a)
	}
}
