package mlat

import "testing"

// The late-copy rings cost about 1 kB per slot and worker. Their total
// must not scale with the CPU count (release soak: 32 workers x 16384
// slots = 515 MB of a 0.9 GB live heap).
func TestLateSlotsPerWorker(t *testing.T) {
	const def = 16384
	for _, n := range []int{-1, 1, 2, 4, 8, 16, 32, 64} {
		per := lateSlotsPerWorker(def, n)
		if n <= 4 && per != def {
			t.Errorf("%d workers: %d slots, want the configured %d", n, per, def)
		}
		if per < 2048 && def >= 2048 {
			t.Errorf("%d workers: %d slots, below the floor", n, per)
		}
		if total := per * max(n, 1); total > 4*def && per > 2048 {
			t.Errorf("%d workers: %d slots in total, more than 4x the configured ring", n, total)
		}
	}
	if got := lateSlotsPerWorker(1024, 32); got != 1024 {
		t.Errorf("a small configured ring must not grow: %d", got)
	}
}
