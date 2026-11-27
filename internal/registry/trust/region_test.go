package trust

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/geoid"
	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
)

// df17Pos builds a DF17 airborne position message (TC 11, barometric
// altitude) for icao at lat/lon/altFt.
func df17Pos(icao uint32, lat, lon float64, altFt int32, odd bool) []byte {
	f := make([]byte, 14)
	f[0] = 17<<3 | 5
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	ac, ok := modes.EncodeAC12(altFt)
	if !ok {
		panic("altitude")
	}
	cl, cn := modes.CPREncode(lat, lon, odd, false)
	fl := uint64(0)
	if odd {
		fl = 1
	}
	me := uint64(11)<<51 | uint64(ac)<<36 | fl<<34 | uint64(cl)<<17 | uint64(cn)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (8 * (6 - i)))
	}
	modes.SetParity(f, 0)
	return f
}

// TestRegionalClusterBootstrap is the regression test of finding SCALE-06:
// region A has trusted stations; region B, 1,200 km away, is a new cluster
// of quarantined stations that never shares an aircraft with region A.
// The network-wide bootstrap rule never applied there (A had enough active
// trusted references), so B was never judged. Now an isolated station is
// judged against its quarantined co-observers: the honest B stations are
// proven, the hostile one (±50 µs timing) is not, and the report says that
// no trusted reference is in range while it waits.
func TestRegionalClusterBootstrap(t *testing.T) {
	type stn struct {
		idx      model.StationIdx
		lat, lon float64
		state    State
		offTicks float64
		noiseNs  float64
	}
	sts := []stn{
		{2, 50.0, 8.0, Trusted, 1e9, 0}, {3, 50.4, 8.6, Trusted, 3e9, 0}, {4, 49.7, 8.9, Trusted, 7e9, 0},
		{10, 50.0, 25.0, Quarantined, 2e9, 0}, {11, 50.4, 25.6, Quarantined, 5e9, 0},
		{12, 49.7, 25.9, Quarantined, 4e9, 0}, {13, 50.2, 25.3, Quarantined, 6e9, 50_000},
	}
	s := src{}
	for _, x := range sts {
		s[x.idx] = StationInfo{State: x.state, Located: true, Lat: x.lat, Lon: x.lon, HAE: 100}
	}
	r := &rec{}
	var promoted []model.StationIdx
	g := New(r, s, Config{}, func(st model.StationIdx, _ Report) {
		promoted = append(promoted, st)
		in := s[st]
		in.State = Trusted
		s[st] = in
	})
	rng := rand.New(rand.NewPCG(5, 6))
	const altFt = 35000
	t0 := int64(1.7e18)
	sawNoRef := false
	for k := 0; k < 8*60*4; k++ { // 8 minutes, 4 groups per second
		ts := float64(k) / 4
		region := k % 2 // A, B alternately
		cLat, cLon := 50.0, 8.5
		if region == 1 {
			cLon = 25.5
		}
		lat := cLat + (rng.Float64()-0.5)*1.2
		lon := cLon + (rng.Float64()-0.5)*1.8
		odd := k%4 >= 2
		icao := uint32(0x400000 + region*16 + k%7)
		msg := df17Pos(icao, lat, lon, altFt, odd)
		// the position the gate will decode (CPR quantisation)
		var m modes.Message
		if err := modes.Decode(msg, &m); err != nil {
			t.Fatal(err)
		}
		dl, do, ok := modes.CPRAirborneLocal(cLat, cLon, m.CPRLat, m.CPRLon, m.CPROdd)
		if !ok {
			t.Fatal("CPR decode")
		}
		h := float64(altFt) * 0.3048
		if n, ok := geoid.Undulation(dl, do); ok {
			h += n
		}
		P := geo.ToECEF(dl, do, h)
		grp := &ingest.Group{FirstNs: t0 + int64(ts*1e9), Data: msg}
		for _, x := range sts {
			if (region == 0) != (x.lon < 15) {
				continue
			}
			d := P.Sub(geo.ToECEF(x.lat, x.lon, 100)).Norm()
			tt := ts + d/cAir + rng.NormFloat64()*30e-9 + (rng.Float64()*2-1)*x.noiseNs*1e-9
			grp.Copies = append(grp.Copies, ingest.Copy{Station: x.idx, ClockCls: model.ClockHW12, Epoch: 1,
				Ticks: uint64(math.Round(x.offTicks + tt*12e6)), Timed: true})
		}
		grp.Timed = len(grp.Copies)
		g.Observe(grp)
		if rep, ok := g.Status(10); ok && rep.NoTrustedRef && !rep.Proven {
			sawNoRef = true
		}
	}
	got := map[model.StationIdx]bool{}
	for _, st := range promoted {
		got[st] = true
	}
	for _, st := range []model.StationIdx{10, 11, 12} {
		if !got[st] {
			rep, _ := g.Status(st)
			t.Errorf("honest station %d of the new region not proven: %+v", st, rep)
		}
	}
	if got[13] {
		t.Error("the hostile station of the new region was proven")
	}
	if !sawNoRef {
		t.Error("the report never said that no trusted reference is in range")
	}
}

// TestRegionalBootstrapNotWithTrustedCoObservers: a quarantined station
// that hears aircraft together with trusted stations is judged against the
// trusted ones only (no peer judging outside the bootstrap), so it cannot
// be proven by new peers alone.
func TestRegionalBootstrapNotWithTrustedCoObservers(t *testing.T) {
	g := New(&rec{}, src{}, Config{}, nil)
	now := int64(1e18)
	s := newEvalState(now)
	s.lastTrusted = now
	if g.isolated(s, now+int64(g.cfg.RefActiveFor)) {
		t.Error("a station with a trusted co-observer RefActiveFor ago is isolated")
	}
	if !g.isolated(s, now+int64(g.cfg.RefActiveFor)+1) {
		t.Error("a station without a trusted co-observer for longer than RefActiveFor is not isolated")
	}
	fresh := newEvalState(now)
	if g.isolated(fresh, now+int64(g.cfg.RefActiveFor)/2) {
		t.Error("a station is isolated before it was judged RefActiveFor long")
	}
}
