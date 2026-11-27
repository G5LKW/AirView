package tracker

import (
	"math"
	"math/bits"
	"math/rand"
	"testing"

	"airview/internal/model"
	"airview/internal/store"
)

// scanGauges recomputes a lane's current-minute gauges the slow way (a scan
// of the aircraft table): the reference for the incremental lane.gauge.
func scanGauges(l *lane) (g [1 + laneChannels][5]int32) {
	for i, k := range l.ac.keys {
		e := &l.ac.vals[i]
		if k == 0 || e.min != l.minStart || e.minMask == 0 {
			continue
		}
		for m := e.minMask; m != 0; m &= m - 1 {
			b := bits.TrailingZeros16(m)
			bit := uint16(1) << uint(b)
			c := &g[b]
			c[0]++
			if e.posMask&bit != 0 {
				c[1]++
			}
			switch e.cls {
			case classADSB:
				c[2]++
			case classMLAT:
				c[3]++
			case classModeS:
				c[4]++
			}
		}
	}
	return g
}

// TestLaneGaugesIncremental credits random copies (channels, classes that
// change within a minute, shown / not shown, positions, minute rolls,
// sweeps) and checks after every copy that the incremental gauges equal a
// full recount, and that the emitted stats_minute rows carry them.
func TestLaneGaugesIncremental(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	res := newFakeRes()
	res.set(3, "s3", 52.0, -1.0, true)
	ls := NewLanes(LaneOptions{Resolver: res})
	var rows []store.StatsRow
	ls.tapRows = func(min []store.StatsRow, _ []store.RangeHour) { rows = append(rows, min...) }
	classes := []uint8{classNone, classADSB, classMLAT, classTISB, classModeS, classOther}
	var hb [40]heardBy
	tMs := int64(1_700_000_000_000)
	for n := 0; n < 20000; n++ {
		tMs += int64(rng.Intn(40))
		k := rng.Intn(len(hb))
		r := Result{
			TMs: tMs, Key: uint32(0x400000 + k), Status: ResultOK, DFIdx: 5,
			Class: classes[(k+n/500)%len(classes)], Shown: k%7 != 0,
			FlightID: int64(k + 1), heard: &hb[k],
		}
		if rng.Intn(4) == 0 {
			r.Pos, r.Lat, r.Lon, r.AltFt, r.PosSrc = true, 52.2, -1.1, 30000, store.SrcADSB
		}
		ch := rng.Intn(laneChannels+2) - 1 // -1 .. laneChannels (out of range = none)
		ls.Credit(3, ch, -20-rng.Float64()*10, &r)
		l := ls.shards[3%laneShards].lanes[3/laneShards]
		if got, want := l.gauge, scanGauges(l); got != want {
			t.Fatalf("copy %d: gauges %v, recount %v", n, got, want)
		}
		if n%997 == 0 {
			ls.Tick(tMs) // flushes (minute rows) and occasionally sweeps
		}
	}
	ls.FlushAll(tMs)
	if len(rows) == 0 {
		t.Fatal("no stats_minute rows")
	}
	seen := false
	for _, r := range rows {
		if r.Ant == "s3" && r.Aircraft > 0 {
			seen = true
			if r.Aircraft < r.ADSB+r.MLAT+r.ModeS || r.AircraftPos > r.Aircraft {
				t.Errorf("inconsistent gauges in %+v", r)
			}
		}
	}
	if !seen {
		t.Error("no station row with aircraft")
	}
}

// TestHeardByHint checks the slot hint of the lane's heardBy marks: a correct
// hint is used, a stale one (slot taken by another station) falls back to
// the scan, and the set stays de-duplicated.
func TestHeardByHint(t *testing.T) {
	var h heardBy
	s1 := h.markHint(5, 100, -1)
	if s1 < 0 || model.StationIdx(h.s[s1].Load()>>32) != 5 {
		t.Fatalf("slot %d", s1)
	}
	if got := h.markHint(5, 101, s1); got != s1 || uint32(h.s[s1].Load()) != 101 {
		t.Fatalf("hinted re-mark: slot %d ts %d", got, uint32(h.s[s1].Load()))
	}
	// Stale hint: the slot now holds another station.
	s2 := h.markHint(9, 101, -1)
	if got := h.markHint(9, 102, s1); got != s2 || uint32(h.s[s2].Load()) != 102 {
		t.Fatalf("stale hint: slot %d, want %d", got, s2)
	}
	if uint32(h.s[s1].Load()) != 101 || model.StationIdx(h.s[s1].Load()>>32) != 5 {
		t.Fatal("stale hint overwrote another station")
	}
	// Out-of-range hints are ignored.
	if got := h.markHint(5, 103, 99); got != s1 {
		t.Fatalf("bad hint: slot %d want %d", got, s1)
	}
	// Fill beyond capacity: the least recently heard is replaced and every
	// returned slot really holds its station.
	for st := model.StationIdx(20); st < 60; st++ {
		sec := int64(200 + st)
		if i := h.markHint(st, sec, -1); i >= 0 && model.StationIdx(h.s[i].Load()>>32) != st {
			t.Fatalf("station %d: slot %d holds %d", st, i, h.s[i].Load()>>32)
		}
	}
	l := h.list(259, nil)
	if len(l) != heardMax {
		t.Fatalf("list %v", l)
	}
	for _, st := range l {
		if st < 44 {
			t.Errorf("old station %d kept over newer ones: %v", st, l)
		}
	}
	// Invalid input.
	var nilSet *heardBy
	if nilSet.markHint(1, 1, 0) != -1 || h.markHint(1, 0, -1) != -1 || h.markHint(1, math.MaxUint32+1, -1) != -1 {
		t.Error("invalid marks stored")
	}
}

// TestLaneHeardByMarkedOncePerSecond checks that a lane marks the
// aircraft's heardBy at most once per data second and that a backfilled
// copy (no heardBy pointer) does not suppress the next live mark.
func TestLaneHeardByMarkedOncePerSecond(t *testing.T) {
	ls := NewLanes(LaneOptions{})
	var hb heardBy
	base := int64(1_700_000_000_000)
	r := Result{TMs: base, Key: 0x4840d6, Status: ResultOK, DFIdx: 5}
	ls.Credit(2, -1, -30, &r) // no heardBy pointer (as a backfilled copy)
	r.heard = &hb
	r.TMs = base + 10
	ls.Credit(2, -1, -30, &r)
	if got := hb.list(base/1000, nil); len(got) != 1 || got[0] != 2 {
		t.Fatalf("heardBy %v after the first live copy of the second", got)
	}
	// Same second: no re-mark (the stored second does not change).
	for i := range hb.s {
		hb.s[i].Store(0)
	}
	r.TMs = base + 500
	ls.Credit(2, -1, -30, &r)
	if got := hb.list(base/1000, nil); len(got) != 0 {
		t.Fatalf("re-marked within the same second: %v", got)
	}
	r.TMs = base + 1000
	ls.Credit(2, -1, -30, &r)
	if got := hb.list(base/1000+1, nil); len(got) != 1 {
		t.Fatalf("next second not marked: %v", got)
	}
}
