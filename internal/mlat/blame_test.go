package mlat

import (
	"testing"
	"time"

	"airview/internal/model"
)

// TestBlameTable: blameLimit charges within the window ask for one survey;
// the next request waits blameSurveyEvery; old charges expire.
func TestBlameTable(t *testing.T) {
	var b blameTable
	const st = model.StationIdx(7)
	now := int64(1e18)
	asked := 0
	for i := 0; i < 3*blameLimit; i++ {
		if b.charge(st, now+int64(i)*int64(time.Second)) {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times within an hour", asked)
	}
	later := now + int64(blameSurveyEvery) + int64(time.Minute)
	for i := 0; i < blameLimit; i++ {
		if b.charge(st, later) && i != blameLimit-1 {
			t.Fatal("asked early")
		}
	}
	// charges spread wider than the window never add up
	var c blameTable
	for i := 0; i < 3*blameLimit; i++ {
		if c.charge(st, now+int64(i)*int64(blameWindow)/2) {
			t.Fatal("charges outside the window counted")
		}
	}
	c.expire(now + 100*int64(blameWindow))
	if len(c.m) != 0 {
		t.Fatal("not expired")
	}
}
