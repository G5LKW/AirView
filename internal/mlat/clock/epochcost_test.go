package clock

import (
	"math"
	"testing"
	"time"

	"airview/internal/model"
)

// TestEpochCostNonZero (HOOKS WP-W2-web: LastEpochNs read 0 on Windows,
// where the monotonic clock ticks in about 0.5 ms steps): with the precise
// clock even an epoch of a few microseconds is measured.
func TestEpochCostNonZero(t *testing.T) {
	w := newSynth(3)
	for i := 0; i < 8; i++ {
		w.addStation(model.ClockSDR12, 50+0.6*math.Cos(float64(i)*0.8), 9+0.9*math.Sin(float64(i)*0.8))
	}
	w.addAircraft(30, 50, 9, 150)
	f, err := New(Config{OriginUnixNs: synthOrigin, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.register(f)
	zero, n := 0, 0
	w.run(f, 0, 100, 60, 0, func(float64) {
		n++
		if f.Stats().LastEpochNs <= 0 {
			zero++
		}
	})
	st := f.Stats()
	t.Logf("last %d ns, mean %d ns, %d of %d epochs read 0", st.LastEpochNs, st.MeanEpochNs, zero, n)
	if zero > n/20 || st.MeanEpochNs <= 0 {
		t.Fatalf("%d of %d epochs read 0 (mean %d ns)", zero, n, st.MeanEpochNs)
	}
}

// TestEpochCostFallback: a wall-clock adjustment during the epoch (negative
// or far from the monotonic reading) falls back to the monotonic value.
func TestEpochCostFallback(t *testing.T) {
	cases := []struct {
		mono, precise time.Duration
		want          int64
	}{
		{0, 3 * time.Microsecond, 3000},
		{500 * time.Microsecond, 120 * time.Microsecond, 120000},
		{time.Millisecond, -2 * time.Second, int64(time.Millisecond)},
		{time.Millisecond, 5 * time.Second, int64(time.Millisecond)},
	}
	for _, c := range cases {
		if got := epochCost(c.mono, c.precise); got != c.want {
			t.Errorf("epochCost(%v, %v) = %d, want %d", c.mono, c.precise, got, c.want)
		}
	}
}

// TestMeanEpochNs: the epoch cost is reported as a running mean as well as
// the last value; the first epoch seeds the mean and later epochs keep it
// non-negative and no larger than the largest single epoch seen.
func TestMeanEpochNs(t *testing.T) {
	f, err := New(Config{OriginUnixNs: synthOrigin})
	if err != nil {
		t.Fatal(err)
	}
	f.Step(1)
	st := f.Stats()
	if st.Epochs != 1 || st.MeanEpochNs != st.LastEpochNs {
		t.Fatalf("first epoch: epochs %d mean %d last %d", st.Epochs, st.MeanEpochNs, st.LastEpochNs)
	}
	var maxNs int64 = st.LastEpochNs
	for i := 2; i <= 100; i++ {
		f.Step(float64(i))
		st = f.Stats()
		maxNs = max(maxNs, st.LastEpochNs)
		if st.MeanEpochNs < 0 || st.MeanEpochNs > maxNs {
			t.Fatalf("epoch %d: mean %d outside [0, %d]", i, st.MeanEpochNs, maxNs)
		}
	}
}
