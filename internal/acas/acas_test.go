package acas

import (
	"context"
	"testing"
	"time"

	"airview/internal/modes"
	"airview/internal/store"
)

// ra builds an RA from ARA bits (a1 = bit 13 ... a7 = bit 7).
func ra(ara uint16, mte, rat bool) modes.ACASRA {
	return modes.ACASRA{ARA: ara, MultipleThreat: mte, Terminated: rat}
}

const (
	a1 = 1 << 13 // RA in force
	a2 = 1 << 12 // corrective
	a3 = 1 << 11 // downward sense
	a4 = 1 << 10 // increased rate
	a5 = 1 << 9  // sense reversal
	a6 = 1 << 8  // altitude crossing
	a7 = 1 << 7  // positive
)

func TestDescribe(t *testing.T) {
	for _, c := range []struct {
		ara   uint16
		mte   bool
		text  string
		sense string
		corr  bool
	}{
		{a1 | a2 | a7, false, "Climb", "up", true},
		{a1 | a2 | a3 | a7, false, "Descend", "down", true},
		{a1 | a2 | a4 | a7, false, "Increase climb", "up", true},
		{a1 | a2 | a3 | a4 | a7, false, "Increase descent", "down", true},
		{a1 | a2 | a5 | a7, false, "Reversal: climb", "up", true},
		{a1 | a2 | a6 | a7, false, "Climb, crossing", "up", true},
		{a1 | a2, false, "Level off", "up", true},
		{a1, false, "Do not descend", "up", false},
		{a1 | a3, false, "Do not climb", "down", false},
		{a1 | a2 | a7, true, "Climb (multiple threats)", "up", true},
		{0, true, "Multiple threats", "", true},
		{0, false, "", "", false},
	} {
		r := ra(c.ara, c.mte, false)
		text, sense, corr := Describe(&r)
		if text != c.text || sense != c.sense || corr != c.corr {
			t.Errorf("ARA %014b mte %v: %q %q %v, want %q %q %v", c.ara, c.mte, text, sense, corr, c.text, c.sense, c.corr)
		}
	}
	if Complement(0b1010) != "do not pass below, do not turn left" || Complement(0) != "" {
		t.Error("Complement")
	}
}

func TestEventGrouping(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := New(st, nil)
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC).UnixMilli()
	rep := func(dt int64, icao uint32, x modes.ACASRA, df uint8) Report {
		return Report{T: t0 + dt, ICAO: icao, Callsign: "BAW117", Lat: 51.5, Lon: -0.5, PosOK: true, AltFt: 9000, AltOK: true, DF: df, RA: x}
	}
	// No advisory: ignored.
	r.Observe(rep(0, 0x400001, ra(0, false, false), 20))
	if r.Stats().Events != 0 {
		t.Fatal("event from an empty report")
	}
	// Climb, then increase climb, then terminated; reported via three message types.
	up := ra(a1|a2|a7, false, false)
	up.ThreatType, up.ThreatICAO = 1, 0x4CA123
	r.Observe(rep(0, 0x400001, up, 16))
	r.Observe(rep(1000, 0x400001, up, 20))
	r.Observe(rep(5000, 0x400001, ra(a1|a2|a4|a7, false, false), 17))
	end := ra(0, false, true)
	r.Observe(rep(9000, 0x400001, end, 20))
	if m := r.LiveMarks(t0 + 10000); m[0x400001].Advisory != "Increase climb" || m[0x400001].Sense != "up" {
		t.Fatalf("live: %+v", m)
	}
	if m := r.LiveMarks(t0 + 9000 + LiveMs + 1); len(m) != 0 {
		t.Fatalf("still live: %+v", m)
	}
	// A second aircraft; then a new event for the first after the gap.
	r.Observe(rep(2000, 0x400002, ra(a1|a3, false, false), 16))
	r.Observe(rep(9000+GapMs+1, 0x400001, ra(a1|a2|a3|a7, false, false), 16))
	if s := r.Stats(); s.Events != 3 || s.Open != 2 {
		t.Fatalf("%+v", s)
	}
	ctx := context.Background()
	evs, err := r.Events(ctx, Query{From: t0 - 1000})
	if err != nil || len(evs) != 3 {
		t.Fatalf("%d events %v", len(evs), err)
	}
	r.closeIdle(t0 + 9000 + 2*GapMs + 10)
	r.flush(ctx)
	evs, err = r.Events(ctx, Query{From: t0 - 1000, ICAO: 0x400001})
	if err != nil || len(evs) != 2 {
		t.Fatalf("%d events %v", len(evs), err)
	}
	e := evs[1] // oldest
	if e.ID == 0 || e.Callsign != "BAW117" || len(e.Advisories) != 2 || e.Advisories[1] != "Increase climb" || !e.Terminated || e.Reports != 4 ||
		e.ThreatICAO != "4CA123" || len(e.Sources) != 3 || e.Lat == nil || *e.AltFt != 9000 || e.Sense != "up" || !e.Corrective {
		t.Fatalf("%+v", e)
	}
	if evs[0].Advisories[0] != "Descend" {
		t.Fatalf("%+v", evs[0])
	}
}
