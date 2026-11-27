package simnet

import (
	"context"
	"testing"
	"time"
)

// TestAircraftAtAcrossSnapshots queries every aircraft at instants between
// all kept world states while aircraft enter and leave the world (the truth
// index of an aircraft differs between snapshots). Regression for an index
// out of range: the interpolated state's truth index was used on the
// nearest snapshot's truth table.
func TestAircraftAtAcrossSnapshots(t *testing.T) {
	n := smallNet(t, 12, 2, func(c *Config) { c.Aircraft = 300 })
	ctx := context.Background()
	for step := 0; step < 30; step++ {
		if err := n.Advance(ctx, time.Second); err != nil {
			t.Fatal(err)
		}
		now := n.NowS()
		for _, a := range n.Aircraft() {
			for dt := -2.0; dt <= 0.2; dt += 0.05 {
				at, ok := n.AircraftAt(a.ICAO, now+dt)
				if ok && at.ICAO != a.ICAO {
					t.Fatalf("truth of %06x answered for %06x", at.ICAO, a.ICAO)
				}
			}
		}
	}
}
