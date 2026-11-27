package simnet

import (
	"context"
	"testing"
	"time"
)

// TestTenThousandAircraftOneWorld is the regression test of finding
// SCALE-11: the DESIGN §20.2 load target (1,000 feeders, 10,000 aircraft)
// must fit in one simulated world, so the feeders can co-observe (two
// disjoint 5,000-aircraft processes cannot exercise one MLAT network).
func TestTenThousandAircraftOneWorld(t *testing.T) {
	if testing.Short() {
		t.Skip("10k-aircraft world")
	}
	if MaxAircraft < 10000 {
		t.Fatalf("MaxAircraft %d below the 10k target", MaxAircraft)
	}
	specs := RandomReceivers(3, 1000, Placement{CenterLat: 50, CenterLon: 10, RadiusKm: 1000})
	n, err := New(Config{Seed: 3, Aircraft: 10000, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(n.Aircraft()); got < 9000 {
		t.Errorf("%d aircraft in the world, want about 10,000", got)
	}
	if err := n.Advance(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Aircraft: MaxAircraft + 1, Receivers: specs[:1]}); err == nil {
		t.Error("aircraft above MaxAircraft accepted")
	}
}
