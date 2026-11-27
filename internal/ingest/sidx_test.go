package ingest

import (
	"math/rand/v2"
	"testing"

	"airview/internal/model"
)

// TestStationIndexMatchesScan checks the per-group station look-up (Bloom
// filter + station index, WP-SCALE-tracker) against a plain scan for groups
// of every size up to MaxCopies and beyond, with repeated stations (a
// station's second channel) and clustered and spread station numbers.
func TestStationIndexMatchesScan(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var sh shard
	for round := 0; round < 400; round++ {
		spread := []int{8, 70, 2000, 65000}[round%4]
		n := 1 + rng.IntN(MaxCopies+10)
		i := sh.alloc()
		e := &sh.ents[i]
		*e = entry{next: -1, prev: -1}
		first := model.StationIdx(1 + rng.IntN(spread))
		e.copies = append(sh.getCopies(1), Copy{Station: first})
		e.markStation(first)
		stored := 1
		for k := 1; k < n; k++ {
			st := model.StationIdx(1 + rng.IntN(spread))
			if sh.add(i, &Copy{Station: st, Channel: int8(k & 1)}, 0) {
				stored++
			}
		}
		if stored != min(n, MaxCopies) {
			t.Fatalf("round %d: stored %d copies of %d", round, stored, n)
		}
		if (e.sidx != nil) != (stored > sidxMin) {
			t.Fatalf("round %d: %d copies, index built %v", round, stored, e.sidx != nil)
		}
		for st := model.StationIdx(0); int(st) <= spread+1 && st < 5000; st++ {
			want := -1
			for k := range e.copies {
				if e.copies[k].Station == st {
					want = k
					break
				}
			}
			if got := stationCopy(e, st); got != want {
				t.Fatalf("round %d (%d copies, spread %d): station %d at %d, want %d", round, stored, spread, st, got, want)
			}
		}
		sh.release(i)
	}
	// Released indexes return to the pool cleared.
	for _, s := range sh.sidxPool {
		if len(s) != sidxSize {
			t.Fatalf("pooled index of %d slots", len(s))
		}
		for _, v := range s {
			if v != 0 {
				t.Fatal("pooled index not cleared")
			}
		}
	}
	if len(sh.sidxPool) == 0 {
		t.Fatal("no index was pooled")
	}
}

// TestStationFilterFalsePositives bounds the Bloom filter's false-positive
// rate of a full group (the O(1) expected cost of a new station's copy).
func TestStationFilterFalsePositives(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	fp, probes := 0, 0
	for g := 0; g < 2000; g++ {
		var e entry
		in := map[model.StationIdx]bool{}
		for len(in) < MaxCopies {
			st := model.StationIdx(rng.IntN(5000))
			in[st] = true
			e.markStation(st)
		}
		for st := range in {
			if !e.maybeStation(st) {
				t.Fatalf("station %d missing from the filter", st)
			}
		}
		for k := 0; k < 50; k++ {
			st := model.StationIdx(5000 + rng.IntN(60000))
			probes++
			if e.maybeStation(st) {
				fp++
			}
		}
	}
	rate := float64(fp) / float64(probes)
	t.Logf("full group (%d stations): false-positive rate %.2f %%", MaxCopies, 100*rate)
	if rate > 0.03 {
		t.Fatalf("false-positive rate %.2f %% above 3 %%", 100*rate)
	}
}
