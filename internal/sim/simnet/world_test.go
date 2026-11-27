package simnet

import (
	"context"
	"math"
	"testing"

	"airview/internal/model"
)

// TestWorldRecovery checks the transmissions recovered from the world
// against internal/sim's own truth and timing: the transmit position is the
// sim's aircraft position at TxS, and TxS plus the light time to the world
// station plus the frame duration is the GPS time stamp the sim gave the
// frame there.
func TestWorldRecovery(t *testing.T) {
	cfg := Config{Seed: 11, Aircraft: 120, Receivers: []ReceiverSpec{{Lat: 49, Lon: 10}}, PosLatencyMaxMs: -1, PosErrorScale: -1}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	w := newWorld(&cfg, cfg.CenterLat, cfg.CenterLon, cfg.RangeKm)
	var seq uint64
	var txs []Transmission
	var maxPos, maxT float64
	n := 0
	for step := 0; step < 30; step++ {
		var err error
		txs, err = w.step(context.Background(), &seq, txs[:0])
		if err != nil {
			t.Fatal(err)
		}
		j := 0
		for i := range w.col.frames {
			f := &w.col.frames[i]
			if f.MLAT || f.Kind == model.FrameStatus || f.GPSNs < 0 {
				continue
			}
			if _, ok := frameICAO(f.Data); !ok {
				continue
			}
			tx := &txs[j]
			j++
			if string(tx.Data) != string(f.Data) {
				t.Fatal("transmission order does not follow the frames")
			}
			// timing at the world station
			dur := 120e-6
			if f.Kind == model.FrameModeSShort {
				dur = 64e-6
			}
			end := tx.TxS + tx.Pos.Sub(w.st).Norm()/SpeedOfLight + dur
			endNs := math.Mod(end*1e9+float64(w.startNs%model.NsPerDay), float64(model.NsPerDay))
			maxT = math.Max(maxT, math.Abs(endNs-float64(f.GPSNs)))
			// position against the sim truth (exact within two steps of Now)
			tr, ok := w.s.TruthOf(tx.ICAO, w.timeOf(tx.TxS))
			if !ok {
				continue
			}
			p := ECEF(tr.Lat, tr.Lon, tx.HAE)
			maxPos = math.Max(maxPos, p.Sub(tx.Pos).Norm())
			n++
		}
		if j != len(txs) {
			t.Fatalf("%d transmissions for %d frames", len(txs), j)
		}
	}
	if n < 500 {
		t.Fatalf("only %d transmissions checked", n)
	}
	// The sim's own slant range is spherical; a few ns of model difference
	// are expected, far below any receiver jitter.
	if maxT > 50 {
		t.Fatalf("recovered transmit times are off by up to %.1f ns", maxT)
	}
	if maxPos > 2 {
		t.Fatalf("recovered positions are off by up to %.2f m", maxPos)
	}
	t.Logf("%d transmissions: timing within %.1f ns, position within %.3f m", n, maxT, maxPos)
}

func TestFrameICAO(t *testing.T) {
	long := []byte{0x8D, 0x48, 0x40, 0xD6, 0x20, 0x2C, 0xC3, 0x71, 0xC3, 0x2C, 0xE0, 0x57, 0x60, 0x98}
	if a, ok := frameICAO(long); !ok || a != 0x4840D6 {
		t.Fatalf("DF17: %06x %v", a, ok)
	}
	// DF4 with address/parity: build one for 0x3C6586
	short := []byte{0x20, 0x00, 0x17, 0x18, 0, 0, 0}
	setAP(short, 0x3C6586)
	if a, ok := frameICAO(short); !ok || a != 0x3C6586 {
		t.Fatalf("DF4: %06x %v", a, ok)
	}
	if _, ok := frameICAO([]byte{0xC0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("DF24 accepted")
	}
}
