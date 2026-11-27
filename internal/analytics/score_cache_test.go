package analytics

import (
	"context"
	"strings"
	"testing"
)

// expire drops the cache entries whose key starts with one of prefixes
// (what happens when their ttl runs out).
func expire(a *Analyzer, prefixes ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.cache {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				delete(a.cache, k)
			}
		}
	}
}

func scoreSub(s *StationScore, key string) SubScore {
	for _, x := range s.Subs {
		if x.Key == key {
			return x
		}
	}
	return SubScore{}
}

// The score always agrees with the coverage tile of the same window: once
// the coverage is recomputed, the score is recomputed from that result,
// even though the score's own entry has not expired.
func TestScoreFollowsCoverageTile(t *testing.T) {
	a, st := newTest(t, locatedConfig(50, 8))
	ctx := context.Background()
	plantCoverage(t, st, 2, func(band int, brg float64) float64 {
		if brg < 90 {
			return 100
		}
		return 0
	})
	cov1, _ := a.Coverage(ctx, Range{}, "")
	sc1, err := a.Score(ctx, Range{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if sc2, _ := a.Score(ctx, Range{}, ""); sc2 != sc1 {
		t.Fatal("unchanged inputs should serve the cached score")
	}
	u1 := scoreSub(sc1, "uniformity")
	if !strings.Contains(u1.Detail, "45 of 180") || cov1.Overall.Bearings != 45 {
		t.Fatalf("first window: %q / %d bearings", u1.Detail, cov1.Overall.Bearings)
	}
	// More coverage arrives; the coverage tile's entry expires (5 min) while
	// the score's would not have under the old 10 min ttl.
	plantCoverage(t, st, 2, func(band int, brg float64) float64 { return 150 })
	expire(a, "coverage|", "covraw|")
	cov2, _ := a.Coverage(ctx, Range{}, "")
	sc3, _ := a.Score(ctx, Range{}, "")
	u3 := scoreSub(sc3, "uniformity")
	if cov2.Overall.Bearings != 180 || !strings.Contains(u3.Detail, "180 of 180") {
		t.Errorf("score disagrees with the coverage tile: tile %d bearings, score %q", cov2.Overall.Bearings, u3.Detail)
	}
	if ttlScore > ttlCoverage {
		t.Errorf("ttlScore %v outlives ttlCoverage %v", ttlScore, ttlCoverage)
	}
}
