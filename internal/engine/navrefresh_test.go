package engine

import (
	"testing"
	"time"
)

func TestNavRefreshSchedule(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	off := 3*time.Hour + 17*time.Minute
	if n := nextRefreshAfter(t0.Add(-8*time.Hour), off); !n.Equal(time.Date(2026, 11, 27, 3, 17, 0, 0, time.UTC)) {
		t.Fatalf("same day %v", n)
	}
	if n := nextRefreshAfter(t0, off); !n.Equal(time.Date(2026, 11, 28, 3, 17, 0, 0, time.UTC)) {
		t.Fatalf("next day %v", n)
	}
	// automatic failures back off 1 h, 2 h, 4 h ... up to a day; a success resets
	var st navRefresh
	for i, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
		st.running = true
		st.done(true, t0)
		if got := st.next.Sub(t0); got != want {
			t.Fatalf("failure %d: retry in %v, want %v", i+1, got, want)
		}
	}
	st.running = true
	st.done(false, t0)
	if st.failures != 0 || st.running || st.known {
		t.Fatalf("%+v", st)
	}
}
