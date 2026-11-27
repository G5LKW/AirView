package mlat

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/group"
	"airview/internal/mlat/track"
	"airview/internal/model"
)

func TestModeACReplyFromFrame(t *testing.T) {
	sq := track.ACFromSquawk(0o7500)
	f := model.RawFrame{Kind: model.FrameModeAC, Data: []byte{byte(sq >> 8), byte(sq)}, Station: 5, Epoch: 2,
		ClockCls: model.ClockClass(5), Ticks: 123}
	r, ok := ModeACReplyFromFrame(&f, "")
	if !ok || r.Code != sq || r.Station != 5 || r.Epoch != 2 || r.Ticks != 123 {
		t.Fatalf("pulses: %+v %v", r, ok)
	}
	// nibble format: 0x7500
	f.Data = []byte{0x75, 0x00}
	if r, ok := ModeACReplyFromFrame(&f, ModeACNibbles); !ok || r.Code != sq {
		t.Fatalf("nibbles: %+v %v", r, ok)
	}
	// 0x7500 is not a 13-bit pulse word; 0x0800 nibbles has a digit 8
	if _, ok := ModeACReplyFromFrame(&f, ModeACPulses); ok {
		t.Fatal("16-bit value accepted as pulses")
	}
	f.Data = []byte{0x08, 0x00}
	if _, ok := ModeACReplyFromFrame(&f, ModeACNibbles); ok {
		t.Fatal("digit 8 accepted")
	}
	bad := []model.RawFrame{
		{Kind: model.FrameModeAC, Data: []byte{0, 0}, ClockCls: 5, Ticks: 1},                   // empty reply
		{Kind: model.FrameModeAC, Data: []byte{0, 0x40}, ClockCls: 5, Ticks: 1},                // X bit only
		{Kind: model.FrameModeAC, Data: []byte{0x0A, 0x55}, Ticks: 1},                          // untimed
		{Kind: model.FrameModeAC, Data: []byte{0x0A, 0x55}, ClockCls: 5},                       // no ticks
		{Kind: model.FrameModeSShort, Data: make([]byte, 7), ClockCls: 5, Ticks: 1},            // Mode S
		{Kind: model.FrameModeAC, Data: []byte{0x0A}, ClockCls: 5, Ticks: 1},                   // short
		{Kind: model.FrameModeAC, Data: []byte{0x0A, 0x55}, ClockCls: 5, Ticks: 1, MLAT: true}, // synthetic
	}
	for i := range bad {
		if _, ok := ModeACReplyFromFrame(&bad[i], ""); ok {
			t.Errorf("frame %d accepted", i)
		}
	}
}

// Off by default: no state, no targets, every entry point a no-op.
func TestModeACDisabled(t *testing.T) {
	e := testEngine(t, nil)
	if e.modeAC.Load() != nil || e.ModeACTargets() != nil || e.ModeACAircraft() != nil || e.ModeACStats().Enabled {
		t.Fatal("Mode A/C active while disabled")
	}
	e.ObserveModeAC([]ModeACReply{{Station: 2, Code: 0x1234}})
	e.ObserveModeACFrames([]model.RawFrame{{Kind: model.FrameModeAC, Data: []byte{0x0A, 0x55}, ClockCls: 5, Ticks: 9}})
	e.Advance(1.8e18 + 2e9)
	if e.Stats().ModeAC != nil {
		t.Fatal("stats block while disabled")
	}
	// enabled by configuration alone (no callback needed)
	e = testEngine(t, func(o *Options) { o.Config.ModeAC = true })
	if e.modeAC.Load() == nil || e.ModeACTargets() == nil || !e.ModeACStats().Enabled || e.Stats().ModeAC == nil {
		t.Fatal("Mode A/C not active with modeAc")
	}
	b, err := json.Marshal(e.Stats())
	if err != nil || !strings.Contains(string(b), `"modeAc":{"enabled":true`) {
		t.Fatalf("stats JSON: %v %s", err, b)
	}
}

// Fixes and 2-3 station clusters reach the tracker; a target confirmed
// over several sweeps is published (target and aircraft DTO).
func TestModeACEnginePipeline(t *testing.T) {
	var fixes []ModeACFix
	e := testEngine(t, func(o *Options) { o.Config.ModeAC = true })
	if err := e.EnableModeAC(func(f ModeACFix) { fixes = append(fixes, f) }); err != nil {
		t.Fatal(err)
	}
	st := e.modeAC.Load()
	atm := e.Atmosphere()
	var sites []geo.Vec3
	for i := 0; i < 6; i++ {
		a := float64(i) / 6 * 2 * math.Pi
		sites = append(sites, geo.ToECEF(51+0.5*math.Cos(a), 9+0.8*math.Sin(a), 150))
	}
	fr := geo.NewFrame(geo.LLH{Lat: 51, Lon: 9})
	sqA := track.ACFromSquawk(0o4321)
	sqC, _ := track.ACFromAltitudeFt(15000)
	t0 := int64(1.8e18)
	pos := func(tS float64) geo.Vec3 {
		return fr.FromENU(geo.Vec3{X: -20e3 + 150*tS, Y: 5e3, Z: 15000*0.3048 + 48})
	}
	cluster := func(tS float64, code uint16, n int) *group.ACCluster {
		p := pos(tS)
		tx := group.MakeTime(t0+int64(tS*1e9), 0)
		c := &group.ACCluster{Code: code}
		for i := 0; i < n; i++ {
			c.Members = append(c.Members, group.ACMember{Station: model.StationIdx(i + 2), Pos: sites[i], Var: 30 * 30, JVar: 20 * 20,
				T: tx.Add(atm.PropagationNs(sites[i], p))})
		}
		return c
	}
	for k := 0; k < 8; k++ { // eight sweeps 4 s apart, dwell of 6 replies
		for j := 0; j < 6; j++ {
			tS := float64(k)*4 + float64(j)*0.004
			code := sqA
			if j%2 == 1 {
				code = sqC
			}
			n := 6
			if j >= 4 {
				n = 3 // a small cluster: TDOA update of the target
			}
			e.modeACCluster(st, cluster(tS, code, n))
		}
		e.publishModeAC(st, t0+int64(float64(k)*4e9)+1e8)
	}
	if len(fixes) != 32 || fixes[len(fixes)-1].Target == 0 {
		t.Fatalf("%d fixes, last %+v", len(fixes), fixes[len(fixes)-1])
	}
	ts := e.ModeACTargets()
	if len(ts) != 1 {
		t.Fatalf("%d targets", len(ts))
	}
	tg := ts[0]
	if tg.Squawk != "4321" || tg.AltFt == nil || *tg.AltFt != 15000 || tg.ID[0] != '#' || !track.IsACID(tg.Key) {
		t.Fatalf("target %+v", tg)
	}
	if d := horizDist(geo.ToECEF(tg.Lat, tg.Lon, tg.AltGeoM), pos(float64(tg.TUnixNs-t0)/1e9)); d > 50 {
		t.Fatalf("target %.0f m off", d)
	}
	if math.Abs(tg.GS-150/0.514444) > 10 || math.Abs(tg.Track-90) > 5 {
		t.Fatalf("velocity gs %.0f track %.0f", tg.GS, tg.Track)
	}
	a := tg.Aircraft()
	if a.ICAO != tg.ID || a.AddrType != "modeac" || a.Squawk != "4321" || a.AltBaro == nil || *a.AltBaro != 15000 || a.Source != model.SrcMLAT {
		t.Fatalf("aircraft %+v", a)
	}
	s := e.ModeACStats()
	if s.Clusters != 48 || s.Solves != 32 || s.SolveOK != 32 || s.Small != 16 || s.Track.ClusterApplied == 0 || s.Shown != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func horizDist(a, b geo.Vec3) float64 {
	lf := geo.NewFrame(geo.FromECEF(b))
	d := lf.ToENU(a)
	return math.Hypot(d.X, d.Y)
}
