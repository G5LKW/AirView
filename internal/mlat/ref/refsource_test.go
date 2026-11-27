package ref

import (
	"testing"

	"airview/internal/mlat/clock"
)

var _ clock.RefSource = (*Store)(nil)

// TestRefSource: RefPosition / Usable follow At + State.Usable, and a
// suspension (the clock filter's blacklist) makes the aircraft unusable.
func TestRefSource(t *testing.T) {
	s := New(Options{}, nil)
	const key = 0x3c0042
	fd := newFeeder(s, key, 50, 8, 230, 90)
	fd.run(0, 30)
	if !s.Usable(key) || s.Usable(0x123456) {
		t.Fatal("Usable")
	}
	var st State
	if !s.At(key, fd.ns(29.5), 0, &st) {
		t.Fatal("no state")
	}
	pos, cov, ok := s.RefPosition(key, 17, fd.ns(29.5))
	if !ok || pos != st.Pos || cov[0] != st.Cov[0] || cov[3] != st.Cov[4] || cov[5] != st.Cov[8] || cov[1] != st.Cov[1] {
		t.Fatalf("RefPosition %v %v %v", ok, pos, cov)
	}
	s.Suspend(key, fd.ns(1000))
	if _, _, ok := s.RefPosition(key, 17, fd.ns(29.5)); ok || s.Usable(key) {
		t.Fatal("suspended aircraft still usable")
	}
}
