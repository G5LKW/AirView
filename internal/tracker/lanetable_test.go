package tracker

import (
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/modes"
)

// TestRouteKey checks that the routing key of every decodable simulator
// frame is the address the decoder finds.
func TestRouteKey(t *testing.T) {
	frames, _ := uniqueFrames(t, 40, 20*time.Second)
	var m modes.Message
	n := 0
	for _, f := range frames {
		if len(f.Data) != 7 && len(f.Data) != 14 {
			continue
		}
		if err := modes.DecodeOpts(f.Data, &m, modes.Options{}); err != nil {
			continue
		}
		if m.CRC != modes.CRCOK && m.CRC != modes.CRCAddressParity {
			continue
		}
		got, ok := RouteKey(f.Data)
		if !ok || got != m.AddrKey()&0xFFFFFF {
			t.Fatalf("DF%d %x: RouteKey %06X %v, decoder %06X", m.DF, f.Data, got, ok, m.AddrKey())
		}
		n++
	}
	if n < 100 {
		t.Fatalf("only %d frames checked", n)
	}
	if _, ok := RouteKey([]byte{24 << 3, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("DF24 routed")
	}
	if _, ok := RouteKey(make([]byte, 7)); !ok {
		t.Error("DF0 not routed")
	}
	if _, ok := RouteKey([]byte{20 << 3, 1, 2, 3, 4, 5, 6}); ok {
		t.Error("short DF20 routed")
	}
}

func TestShardOf(t *testing.T) {
	const shards = 8
	var cnt [shards]int
	rng := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 80_000; i++ {
		// Real allocations come in blocks: 0x400000-0x43FFFF (UK) etc.
		addr := 0x400000 + uint32(rng.IntN(0x40000))
		s := ShardOf(addr, shards)
		if s < 0 || s >= shards || s != ShardOf(addr|0x1000000, shards) {
			t.Fatalf("ShardOf(%06X) = %d", addr, s)
		}
		cnt[s]++
	}
	for i, c := range cnt {
		if c < 9000 || c > 11000 {
			t.Errorf("shard %d: %d of 80000", i, c)
		}
	}
	if ShardOf(123, 1) != 0 || ShardOf(123, 0) != 0 {
		t.Error("single shard")
	}
}

func TestLockStats(t *testing.T) {
	fx := newNetFixture(t, 30, 60*time.Second)
	s := fx.tr.LockStats()
	if s.Samples == 0 || !(s.HoldNs > 0) || !(s.Utilization >= 0) {
		t.Errorf("lock stats %+v", s)
	}
}

// TestACTableAgainstMap drives the open-addressing table and a Go map with
// the same random operations (adds, look-ups, deletes, growth, clustered
// keys) and compares them after every step.
func TestACTableAgainstMap(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var tab acTable
	ref := map[uint32]int64{}
	// A small key space forces collisions and long probe runs.
	key := func() uint32 {
		if rng.IntN(2) == 0 {
			return uint32(rng.IntN(300))
		}
		return 0x400000 + uint32(rng.IntN(300))<<12
	}
	for step := 0; step < 200_000; step++ {
		k := key()
		switch op := rng.IntN(10); {
		case op < 5:
			e := tab.getOrAdd(k)
			e.lastMs++
			ref[k]++
		case op < 8:
			tab.del(k)
			delete(ref, k)
		default:
			e := tab.get(k)
			v, ok := ref[k]
			if (e != nil) != ok || (ok && e.lastMs != v) {
				t.Fatalf("step %d: get(%#x) = %v, map has %v %v", step, k, e, v, ok)
			}
		}
		if tab.len() != len(ref) {
			t.Fatalf("step %d: len %d, map %d", step, tab.len(), len(ref))
		}
	}
	n := 0
	tab.each(func(k uint32, e *laneAC) {
		n++
		if ref[k] != e.lastMs {
			t.Fatalf("each: %#x = %d, map %d", k, e.lastMs, ref[k])
		}
	})
	if n != len(ref) {
		t.Fatalf("each visited %d of %d", n, len(ref))
	}
	for k := range ref {
		tab.del(k)
	}
	if tab.len() != 0 || tab.get(1) != nil {
		t.Fatal("table not empty")
	}
	var empty acTable
	empty.del(5)
	if empty.get(5) != nil || empty.len() != 0 {
		t.Fatal("zero table")
	}
}
