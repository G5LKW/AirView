package tracker

import (
	"airview/internal/acas"
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"airview/internal/geo"
	lchain "airview/internal/latency"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/sim"
	"airview/internal/store"
)

var simStart = time.Date(2026, 3, 10, 12, 0, 30, 0, time.UTC)

// countingSink forwards to a tracker and counts what the simulator delivered.
type countingSink struct {
	t        *Tracker
	modeS    uint64 // Mode S frames
	unique   uint64 // transmissions (first antenna copy or MLAT)
	perChan  [4]uint64
	mlat     uint64
	lastRecv time.Time
}

func (c *countingSink) HandleFrame(f model.RawFrame) {
	if f.Kind == model.FrameModeSShort || f.Kind == model.FrameModeSLong {
		c.modeS++
		if f.Channel >= 0 && f.Channel < len(c.perChan) {
			c.perChan[f.Channel]++
		}
		if f.Channel <= 0 {
			c.unique++
		}
		if f.MLAT {
			c.mlat++
		}
	}
	c.lastRecv = f.Recv
	c.t.HandleFrame(f)
}

func (c *countingSink) HandleUpdate(u model.DecodedUpdate) { c.t.HandleUpdate(u) }

func newSim(t testing.TB, aircraft int, mutate func(*sim.Config)) (*sim.Sim, model.Config) {
	t.Helper()
	cfg := model.ExampleRadarcapeConfig()
	sc := sim.Config{
		Aircraft: aircraft, Seed: 7, Start: simStart, NoDiurnal: true, PerfectReception: true,
		EmergencyPerHour: -1, FeedID: "demo",
	}
	if mutate != nil {
		mutate(&sc)
	}
	s := sim.New(sc)
	cfg.Receiver = s.Station()
	cfg.Receiver.Located = true
	cfg.Feeds = []model.Feed{{ID: "demo", Name: "sim", Enabled: true, Type: model.FeedDemo}}
	return s, cfg
}

func advance(t testing.TB, s *sim.Sim, d time.Duration, sink model.FrameSink) {
	t.Helper()
	if err := s.Advance(context.Background(), d, sink); err != nil {
		t.Fatal(err)
	}
}

func TestSimulatorGroundTruth(t *testing.T) {
	s, cfg := newSim(t, 60, nil)
	tr := New(Options{Config: &cfg})
	sink := &countingSink{t: tr}
	advance(t, s, 4*time.Minute, sink)

	truth := s.Truth()
	if len(truth) < 20 {
		t.Fatalf("simulator has only %d aircraft", len(truth))
	}
	var checked, withPos, mlatPos, callsigns, ehs int
	for _, tru := range truth {
		ac, ok := tr.Aircraft(tru.ICAO)
		if !ok {
			t.Errorf("%06X (%s %s) not tracked", tru.ICAO, tru.Kind, tru.Phase)
			continue
		}
		if ac.Msgs < 100 {
			continue // spawned moments ago
		}
		checked++
		if ac.Country == "" && !ac.Military {
			t.Logf("%s: no country", ac.ICAO)
		}
		if tru.ADSB {
			if ac.Callsign != tru.Callsign {
				t.Errorf("%s callsign %q, truth %q", ac.ICAO, ac.Callsign, tru.Callsign)
			} else {
				callsigns++
			}
			if ac.Category != tru.Category {
				t.Errorf("%s category %q, truth %q", ac.ICAO, ac.Category, tru.Category)
			}
		}
		if ac.Squawk != "" && ac.Squawk != tru.Squawk {
			t.Errorf("%s squawk %s, truth %s", ac.ICAO, ac.Squawk, tru.Squawk)
		}
		if ac.OnGround != nil && *ac.OnGround != tru.OnGround {
			t.Errorf("%s onGround %v, truth %v (%s)", ac.ICAO, *ac.OnGround, tru.OnGround, tru.Phase)
		}
		if ac.AltBaro != nil && !tru.OnGround {
			if d := math.Abs(*ac.AltBaro - tru.AltBaro); d > 250 {
				t.Errorf("%s altBaro %.0f, truth %.0f", ac.ICAO, *ac.AltBaro, tru.AltBaro)
			}
		}
		if tru.GNSSAlt && ac.AltGeom != nil {
			if d := math.Abs(*ac.AltGeom - tru.AltGeom); d > 300 {
				t.Errorf("%s altGeom %.0f, truth %.0f", ac.ICAO, *ac.AltGeom, tru.AltGeom)
			}
		}
		if (tru.ADSB || tru.MLAT) && !tru.OnGround {
			if ac.GS == nil || ac.Track == nil {
				t.Errorf("%s (%s) has no velocity", ac.ICAO, tru.Kind)
			} else {
				tol := 12.0
				if tru.MLAT {
					tol = 30
				}
				if d := math.Abs(*ac.GS - tru.GS); d > tol {
					t.Errorf("%s gs %.0f, truth %.0f", ac.ICAO, *ac.GS, tru.GS)
				}
				if d := math.Abs(geo.AngleDiff(*ac.Track, tru.Track)); d > 6 && tru.GS > 60 {
					t.Errorf("%s track %.1f, truth %.1f", ac.ICAO, *ac.Track, tru.Track)
				}
			}
			if tru.ADSB && ac.VertRate != nil && math.Abs(*ac.VertRate-tru.VertRate) > 400 {
				t.Errorf("%s vertRate %.0f, truth %.0f", ac.ICAO, *ac.VertRate, tru.VertRate)
			}
		}
		if tru.ADSB || (tru.MLAT && !tru.OnGround) {
			if ac.Lat == nil || ac.Lon == nil || ac.PosAge == nil {
				t.Errorf("%s (%s %s adsb=%v mlat=%v) has no position", ac.ICAO, tru.Kind, tru.Phase, tru.ADSB, tru.MLAT)
				continue
			}
			withPos++
			d := geo.HaversineKm(*ac.Lat, *ac.Lon, tru.Lat, tru.Lon)
			// The aircraft kept flying since its last position report.
			tol := 0.3 + (*ac.PosAge+0.2)*tru.GS*ktToKmPerS*1.2
			if tru.MLAT {
				tol += 1.0
				mlatPos++
				if ac.Source != model.SrcMLAT {
					t.Errorf("%s source %q, want mlat", ac.ICAO, ac.Source)
				}
			} else if ac.Source != model.SrcADSB {
				t.Errorf("%s source %q, want adsb", ac.ICAO, ac.Source)
			}
			if d > tol {
				t.Errorf("%s position off by %.2f km (tolerance %.2f, age %.1f s)", ac.ICAO, d, tol, *ac.PosAge)
			}
			// The simulator measures on a sphere, the tracker on the ellipsoid.
			if ac.DistKm == nil || math.Abs(*ac.DistKm-tru.DistKm) > tol+0.5+0.006*tru.DistKm {
				t.Errorf("%s distKm %.2f, truth %.2f", ac.ICAO, *ac.DistKm, tru.DistKm)
			}
			if ac.Bearing != nil && tru.DistKm > 20 && math.Abs(geo.AngleDiff(*ac.Bearing, tru.Bearing)) > 2 {
				t.Errorf("%s bearing %.1f, truth %.1f", ac.ICAO, *ac.Bearing, tru.Bearing)
			}
			if ac.Elevation != nil && tru.DistKm > 20 && !tru.GNSSAlt && math.Abs(*ac.Elevation-tru.Elevation) > 1.0 {
				t.Errorf("%s elevation %.2f, truth %.2f", ac.ICAO, *ac.Elevation, tru.Elevation)
			}
		}
		// Comm-B derived values, when present, must be right.
		if ac.Mach != nil {
			ehs++
			if math.Abs(*ac.Mach-tru.Mach) > 0.03 {
				t.Errorf("%s mach %.3f, truth %.3f", ac.ICAO, *ac.Mach, tru.Mach)
			}
		}
		if ac.IAS != nil && math.Abs(*ac.IAS-tru.IAS) > 12 {
			t.Errorf("%s ias %.0f, truth %.0f", ac.ICAO, *ac.IAS, tru.IAS)
		}
		if ac.TAS != nil && math.Abs(*ac.TAS-tru.TAS) > 12 {
			t.Errorf("%s tas %.0f, truth %.0f", ac.ICAO, *ac.TAS, tru.TAS)
		}
		if ac.MagHeading != nil && math.Abs(geo.AngleDiff(*ac.MagHeading, tru.MagHeading)) > 5 {
			t.Errorf("%s magHeading %.1f, truth %.1f", ac.ICAO, *ac.MagHeading, tru.MagHeading)
		}
		if ac.Roll != nil && math.Abs(*ac.Roll-tru.Roll) > 6 {
			t.Errorf("%s roll %.1f, truth %.1f", ac.ICAO, *ac.Roll, tru.Roll)
		}
	}
	if checked < 20 || withPos < 15 || callsigns < 10 {
		t.Fatalf("too few aircraft verified: checked %d, positions %d, callsigns %d", checked, withPos, callsigns)
	}
	if ehs == 0 {
		t.Error("no aircraft had Comm-B (EHS) data")
	}
	t.Logf("checked %d aircraft, %d with position (%d MLAT), %d with EHS", checked, withPos, mlatPos, ehs)

	// Snapshot and friends.
	snap := tr.Snapshot()
	if snap.Counts.Aircraft < checked || snap.Counts.WithPos < withPos || len(snap.Aircraft) != snap.Counts.Aircraft {
		t.Errorf("snapshot counts %+v, checked %d", snap.Counts, checked)
	}
	for i := 1; i < len(snap.Aircraft); i++ {
		if snap.Aircraft[i-1].ICAO >= snap.Aircraft[i].ICAO {
			t.Fatal("snapshot is not sorted by ICAO")
		}
	}
	if snap.Counts.MsgRate <= 0 || snap.Counts.MaxRangeKm <= 0 {
		t.Errorf("snapshot rates: %+v", snap.Counts)
	}
	for _, v := range []any{snap, tr.Counters(), tr.FeedLatency()} {
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("JSON encoding failed: %v", err)
		}
	}
	trails := tr.Trails(15)
	if len(trails) < withPos/2 {
		t.Errorf("only %d trails for %d positioned aircraft", len(trails), withPos)
	}
	for hex, pts := range trails {
		if len(pts) > 4*60/3+40 {
			t.Errorf("%s trail has %d points in 4 minutes", hex, len(pts))
		}
		for i := 1; i < len(pts); i++ {
			if pts[i].T <= pts[i-1].T {
				t.Fatalf("%s trail not in time order", hex)
			}
			if pts[i].T-pts[i-1].T < trailTurnGapMs {
				t.Fatalf("%s trail points %d ms apart", hex, pts[i].T-pts[i-1].T)
			}
		}
		icao, err := modes.ParseICAO(hex)
		if err != nil {
			t.Fatal(err)
		}
		if one := tr.Trail(icao); len(one) < len(pts) {
			t.Errorf("Trail(%s) has %d points, Trails has %d", hex, len(one), len(pts))
		}
	}

	// Duplicates: every transmission arrived once per antenna but must be
	// counted once in the combined statistics.
	c := tr.Counters()
	if c.Frames != sink.modeS {
		t.Errorf("frames %d, simulator delivered %d", c.Frames, sink.modeS)
	}
	if got := c.Frames - c.Duplicates; got != sink.unique {
		t.Errorf("unique frames %d, simulator transmitted %d (duplicates %d)", got, sink.unique, c.Duplicates)
	}
	if c.Accepted+c.Rejected+c.CRCBad+c.Unsupported != sink.unique {
		t.Errorf("accepted %d + rejected %d + bad %d + unsupported %d != %d",
			c.Accepted, c.Rejected, c.CRCBad, c.Unsupported, sink.unique)
	}
	if c.CRCBad != 0 {
		t.Errorf("%d CRC failures with perfect reception", c.CRCBad)
	}
	if float64(c.Rejected) > 0.02*float64(sink.unique) {
		t.Errorf("%d of %d frames rejected", c.Rejected, sink.unique)
	}
	if len(c.Antennas) != 3 {
		t.Fatalf("antenna counters: %+v", c.Antennas)
	}
	if c.Antennas[0].ID != "" || c.Antennas[0].Frames != sink.unique {
		t.Errorf("combined frames %d, want %d", c.Antennas[0].Frames, sink.unique)
	}
	if c.Antennas[1].Frames != sink.perChan[0] || c.Antennas[2].Frames != sink.perChan[1] {
		t.Errorf("antenna frames %d / %d, want %d / %d",
			c.Antennas[1].Frames, c.Antennas[2].Frames, sink.perChan[0], sink.perChan[1])
	}
	// Each antenna heard every ADS-B position, so it has as many positions
	// as the combined view minus the MLAT ones.
	offAir := c.Positions - c.MLATPositions
	for _, a := range c.Antennas[1:] {
		if a.Positions != offAir {
			t.Errorf("antenna %s positions %d, want %d", a.ID, a.Positions, offAir)
		}
	}
	if c.Antennas[0].Positions != c.Positions {
		t.Errorf("combined positions %d != %d", c.Antennas[0].Positions, c.Positions)
	}
	if c.MLATPositions == 0 && mlatPos > 0 {
		t.Error("no MLAT positions counted")
	}
	if c.DF["df17"] == 0 || c.DF["df11"] == 0 || c.DF["df4"] == 0 {
		t.Errorf("DF counters: %v", c.DF)
	}

	// Timing: the simulator's host reads every frame a few ms after its
	// GPS time of arrival (median ~2.5 ms, rare bursts < 60 ms).
	lat := tr.FeedLatency()
	if len(lat) != 1 || lat[0].FeedID != "demo" || lat[0].Samples < 100 {
		t.Fatalf("feed latency: %+v", lat)
	}
	if lat[0].P50Ms < 1.5 || lat[0].P50Ms > 6 || lat[0].MinMs <= 1.6 || lat[0].MaxMs > 61 {
		t.Errorf("latency min %.3f p50 %.3f max %.3f ms, want ~1.7 / ~2.5 / < 61", lat[0].MinMs, lat[0].P50Ms, lat[0].MaxMs)
	}
	if len(lat[0].History) < 2 {
		t.Errorf("latency history has %d points", len(lat[0].History))
	}
}

func TestAircraftExpire(t *testing.T) {
	s, cfg := newSim(t, 20, nil)
	cfg.UI.AircraftTTLs = 30
	tr := New(Options{Config: &cfg})
	advance(t, s, 40*time.Second, tr)
	n := tr.Snapshot().Counts.Aircraft
	if n < 10 {
		t.Fatalf("only %d aircraft", n)
	}
	// Silence: one lonely frame 45 s later moves the clock.
	last := s.Now()
	f := df11(0x3C6444)
	tr.HandleFrame(model.RawFrame{Recv: last.Add(45 * time.Second), GPSNs: -1, Kind: model.FrameModeSShort, Data: f, Channel: -1})
	if got := tr.Snapshot().Counts.Aircraft; got != 0 {
		t.Errorf("%d aircraft still live after 45 s of silence (TTL 30 s)", got)
	}
	if c := tr.Counters(); c.Tracked < n {
		t.Errorf("dormant aircraft dropped early: tracked %d of %d", c.Tracked, n)
	}
	// After the flight gap everything is gone.
	tr.HandleFrame(model.RawFrame{Recv: last.Add(32 * time.Minute), GPSNs: -1, Kind: model.FrameModeSShort, Data: f, Channel: -1})
	if c := tr.Counters(); c.Tracked > 1 {
		t.Errorf("%d aircraft still tracked after 32 minutes", c.Tracked)
	}
}

// df11 builds an all-call reply / acquisition squitter for icao.
func df11(icao uint32) []byte {
	f := []byte{11<<3 | 5, byte(icao >> 16), byte(icao >> 8), byte(icao), 0, 0, 0}
	modes.SetParity(f, 0)
	return f
}

// df4 builds a surveillance altitude reply with the address in the parity.
func df4(icao uint32, ac13 uint16) []byte {
	f := []byte{4 << 3, 0, byte(ac13 >> 8), byte(ac13), 0, 0, 0}
	modes.SetParity(f, icao)
	return f
}

func TestParityAddressNoiseRejected(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	tr := New(Options{Config: &cfg})
	t0 := simStart
	send := func(dt time.Duration, data []byte) {
		kind := model.FrameModeSShort
		if len(data) == 14 {
			kind = model.FrameModeSLong
		}
		tr.HandleFrame(model.RawFrame{Recv: t0.Add(dt), GPSNs: -1, Signal: -20, Kind: kind, Data: data, FeedID: "x", Channel: -1})
	}
	alt, _ := modes.EncodeAC13(35000)

	// Replies whose address only exists in the parity field must not create
	// aircraft.
	for i := 0; i < 500; i++ {
		send(time.Duration(i)*time.Millisecond, df4(0x400000+uint32(i)*7919, alt))
	}
	c := tr.Counters()
	if c.Tracked != 0 || c.Rejected != 500 || c.Accepted != 0 {
		t.Fatalf("after noise: tracked %d rejected %d accepted %d", c.Tracked, c.Rejected, c.Accepted)
	}
	// Random long frames fail the CRC.
	noise := []byte{0x8D, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
	send(600*time.Millisecond, noise)
	if c = tr.Counters(); c.CRCBad != 1 || c.Tracked != 0 {
		t.Fatalf("CRC noise: bad %d tracked %d", c.CRCBad, c.Tracked)
	}

	// One all-call reply creates a candidate that is not shown yet.
	const icao = 0x4840D6
	send(time.Second, df11(icao))
	if _, ok := tr.Aircraft(icao); ok {
		t.Error("aircraft shown after a single frame")
	}
	if n := tr.Snapshot().Counts.Aircraft; n != 0 {
		t.Errorf("%d aircraft in the snapshot after a single frame", n)
	}
	send(2*time.Second, df11(icao))
	// Now its surveillance replies are accepted ...
	send(2100*time.Millisecond, df4(icao, alt))
	ac, ok := tr.Aircraft(icao)
	if !ok || ac.AltBaro == nil || *ac.AltBaro != 35000 {
		t.Fatalf("known aircraft: ok=%v %+v", ok, ac.AltBaro)
	}
	if ac.Source != model.SrcModeS || ac.Msgs != 3 || ac.FlightID == 0 {
		t.Errorf("source %q msgs %d flight %d", ac.Source, ac.Msgs, ac.FlightID)
	}
	// ... but not once the address has not been confirmed for a while.
	before := tr.Counters().Rejected
	send(2*time.Second+confirmWindowMs*time.Millisecond+time.Second, df4(icao, alt))
	if got := tr.Counters().Rejected; got != before+1 {
		t.Errorf("stale address accepted (rejected %d -> %d)", before, got)
	}
}

func TestDuplicateWindow(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	tr := New(Options{Config: &cfg})
	const icao = 0x3C6444
	send := func(dt time.Duration, ch int, data []byte) {
		tr.HandleFrame(model.RawFrame{Recv: simStart.Add(dt), GPSNs: -1, Signal: -10 - float64(ch), Kind: model.FrameModeSShort,
			Data: data, FeedID: "rc", Channel: ch})
	}
	f := df11(icao)
	send(0, 0, f)
	send(2*time.Millisecond, 1, f)              // same transmission on the other antenna
	send(time.Second, 0, f)                     // next squitter
	send(time.Second+50*time.Millisecond, 0, f) // same antenna again within 100 ms: a new transmission
	send(time.Second+60*time.Millisecond, 1, f) // its copy
	send(3*time.Second, 1, f)
	c := tr.Counters()
	if c.Frames != 6 || c.Duplicates != 2 || c.Accepted != 4 {
		t.Fatalf("frames %d duplicates %d accepted %d", c.Frames, c.Duplicates, c.Accepted)
	}
	ac, ok := tr.Aircraft(icao)
	if !ok || ac.Msgs != 4 {
		t.Fatalf("msgs %d, want 4", ac.Msgs)
	}
	if len(ac.Antennas) != 2 || ac.Antennas[0] != "vertical" || ac.Antennas[1] != "yagi" {
		t.Errorf("antennas %v", ac.Antennas)
	}
	if c.Antennas[0].Frames != 4 || c.Antennas[1].Frames != 3 || c.Antennas[2].Frames != 3 {
		t.Errorf("antenna frames %+v", c.Antennas)
	}
}

func TestFeedAntennaAttribution(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	cfg.Feeds = []model.Feed{
		{ID: "a", Enabled: true, Type: model.FeedBeast, AntennaID: "yagi"},
		{ID: "b", Enabled: true, Type: model.FeedBeast, AntennaID: "vertical"},
		{ID: "c", Enabled: true, Type: model.FeedBeast},
	}
	tr := New(Options{Config: &cfg})
	f := df11(0x3C6444)
	for i, feed := range []string{"a", "b", "c", "a"} {
		tr.HandleFrame(model.RawFrame{Recv: simStart.Add(time.Duration(i) * time.Second), GPSNs: -1, Signal: math.NaN(),
			Kind: model.FrameModeSShort, Data: f, FeedID: feed, Channel: -1})
	}
	c := tr.Counters()
	if c.Antennas[0].Frames != 4 || c.Antennas[1].Frames != 1 || c.Antennas[2].Frames != 2 {
		t.Errorf("antenna frames %+v", c.Antennas)
	}
	if len(c.Feeds) != 3 || c.Feeds[0].Frames != 2 || c.Feeds[0].AntennaID != "yagi" {
		t.Errorf("feeds %+v", c.Feeds)
	}
	// Live reconfiguration: feed c now belongs to the yagi.
	cfg.Feeds[2].AntennaID = "yagi"
	tr.SetConfig(cfg)
	tr.HandleFrame(model.RawFrame{Recv: simStart.Add(10 * time.Second), GPSNs: -1, Signal: math.NaN(),
		Kind: model.FrameModeSShort, Data: f, FeedID: "c", Channel: -1})
	if c = tr.Counters(); c.Antennas[2].Frames != 3 {
		t.Errorf("yagi frames %d after reconfiguration, want 3", c.Antennas[2].Frames)
	}
	ac, _ := tr.Aircraft(0x3C6444)
	if len(ac.Feeds) != 3 {
		t.Errorf("feeds that heard it: %v", ac.Feeds)
	}
}

func TestStoreRows(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	s, cfg := newSim(t, 40, func(c *sim.Config) { c.MilitaryFraction = 0.3 })
	var mu sync.Mutex
	var notices []Notice
	tr := New(Options{Store: st, Config: &cfg, OnNotice: func(n Notice) {
		mu.Lock()
		notices = append(notices, n)
		mu.Unlock()
	}})
	advance(t, s, 150*time.Second, tr)
	icaoEm, ok := s.TriggerEmergency()
	if !ok {
		t.Fatal("simulator could not trigger an emergency")
	}
	advance(t, s, 100*time.Second, tr)
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	from, to := simStart.Unix()-3600, s.Now().Unix()+3600

	// Flights.
	flights, err := st.RecentFlights(ctx, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	snap := tr.Snapshot()
	if len(flights) < snap.Counts.Aircraft || len(flights) == 0 {
		t.Fatalf("%d flights stored for %d live aircraft", len(flights), snap.Counts.Aircraft)
	}
	var withPos, emergencies int
	var posFlight store.Flight
	for _, f := range flights {
		if f.Msgs <= 0 || f.End < f.Start || f.ICAO == 0 {
			t.Errorf("bad flight row %+v", f)
		}
		if f.Positions > 20 && !math.IsNaN(f.MaxDistKm) && f.SrcMask&store.MaskADSB != 0 {
			withPos++
			posFlight = f
			if math.IsNaN(f.FirstLat) || math.IsNaN(f.LastLon) || math.IsNaN(f.RSSIAvg) || f.MinDistKm > f.MaxDistKm {
				t.Errorf("flight aggregates incomplete: %+v", f)
			}
		}
		if f.Emergency != "" {
			emergencies++
			if f.ICAO != icaoEm || f.Emergency != "general" {
				t.Errorf("emergency flight %06X %q, want %06X general", f.ICAO, f.Emergency, icaoEm)
			}
		}
	}
	if withPos == 0 {
		t.Fatal("no flight with positions")
	}
	if emergencies != 1 {
		t.Errorf("%d flights with an emergency, want 1", emergencies)
	}
	// Flight ids match the live table.
	for _, a := range snap.Aircraft {
		if a.FlightID == 0 {
			t.Errorf("%s has no flight id", a.ICAO)
		}
	}

	// Positions: down-sampled to the trail interval.
	ps, err := st.Positions(ctx, posFlight.ID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) < 5 || len(ps) > 250/5+2 {
		t.Errorf("%d stored positions for flight %d", len(ps), posFlight.ID)
	}
	for i := 1; i < len(ps); i++ {
		if ps[i].T-ps[i-1].T < int64(cfg.Storage.TrailIntervalSec) {
			t.Errorf("stored positions %d s apart", ps[i].T-ps[i-1].T)
		}
	}

	// Aircraft table.
	_, total, err := st.ListAircraft(ctx, store.AircraftQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total < snap.Counts.Aircraft {
		t.Errorf("%d aircraft rows for %d live aircraft", total, snap.Counts.Aircraft)
	}
	row, ok, err := st.AircraftByICAO(ctx, posFlight.ICAO)
	if err != nil || !ok {
		t.Fatalf("aircraft row: %v %v", ok, err)
	}
	if row.Msgs < posFlight.Msgs-50 || row.Msgs > posFlight.Msgs+50 || row.Flights != 1 || row.Positions != posFlight.Positions {
		t.Errorf("aircraft row %+v vs flight %+v", row, posFlight)
	}

	// Minute statistics, combined and per antenna.
	c := tr.Counters()
	for i, ant := range []string{"", "vertical", "yagi"} {
		rows, err := st.MinuteStats(ctx, ant, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) < 4 {
			t.Fatalf("ant %q: %d minute rows", ant, len(rows))
		}
		var frames, positions, df17, mlatPos int64
		maxAircraft := 0
		for _, r := range rows {
			frames += r.Frames
			positions += r.Positions
			df17 += r.DF17
			mlatPos += r.MLATPositions
			maxAircraft = max(maxAircraft, r.Aircraft)
			if r.Frames > 0 && (math.IsNaN(r.RSSIAvg) || r.RSSIMin > r.RSSIMax) {
				t.Errorf("ant %q minute %d: rssi %v %v %v", ant, r.T, r.RSSIAvg, r.RSSIMin, r.RSSIMax)
			}
		}
		if uint64(frames) != c.Antennas[i].Frames {
			t.Errorf("ant %q: stored frames %d, counted %d", ant, frames, c.Antennas[i].Frames)
		}
		if uint64(positions) != c.Antennas[i].Positions {
			t.Errorf("ant %q: stored positions %d, counted %d", ant, positions, c.Antennas[i].Positions)
		}
		if i == 0 && (uint64(df17) != c.DF["df17"] || uint64(mlatPos) != c.MLATPositions) {
			t.Errorf("combined df17 %d (want %d), mlat positions %d (want %d)", df17, c.DF["df17"], mlatPos, c.MLATPositions)
		}
		if maxAircraft < snap.Counts.Aircraft/2 || maxAircraft > len(flights)+5 {
			t.Errorf("ant %q: aircraft gauge %d (live %d)", ant, maxAircraft, snap.Counts.Aircraft)
		}

		// Hourly range metrics.
		hours, err := st.RangeHours(ctx, ant, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if len(hours) != 1 {
			t.Fatalf("ant %q: %d range_hour rows", ant, len(hours))
		}
		h := hours[0]
		var histN uint64
		for _, n := range h.DistHist {
			histN += uint64(n)
		}
		if h.Positions == 0 || histN != uint64(h.Positions) || uint64(h.Positions) != c.Positions-c.MLATPositions {
			t.Errorf("ant %q: range positions %d, histogram %d, off-air positions %d", ant, h.Positions, histN, c.Positions-c.MLATPositions)
		}
		if !(h.MaxKm > 50) || !(h.P50Km > 0) || !(h.P50Km <= h.P95Km) || !(h.P95Km <= h.MaxKm+5) || h.Aircraft == 0 {
			t.Errorf("ant %q: range row %+v", ant, h)
		}
		if math.IsNaN(h.ResidDB) || h.ResidN == 0 || len(h.SectorMax) != geo.Sectors {
			t.Errorf("ant %q: resid %v n %d sectors %d", ant, h.ResidDB, h.ResidN, len(h.SectorMax))
		}
	}

	// Day tables.
	for _, q := range []string{
		`SELECT COALESCE(SUM(count),0) FROM coverage WHERE ant=''`,
		`SELECT COALESCE(SUM(count),0) FROM signal_hist WHERE ant=''`,
		`SELECT COALESCE(SUM(count),0) FROM elev_hist WHERE ant='yagi'`,
		`SELECT COALESCE(SUM(count),0) FROM heat WHERE ant='vertical'`,
	} {
		var n int64
		if err := st.DB().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if uint64(n) != c.Positions-c.MLATPositions {
			t.Errorf("%s = %d, want %d", q, n, c.Positions-c.MLATPositions)
		}
	}

	// Records and events.
	recs, err := st.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Record{}
	for _, r := range recs {
		got[r.Key] = r
	}
	for _, key := range []string{RecordMaxDistKm, RecordMaxAltFt, RecordMaxGSKt, RecordMinRSSIPos, RecordMaxAircraft, RecordMaxMsgRate, RecordMaxFlightS} {
		r, ok := got[key]
		if !ok {
			t.Errorf("record %s missing", key)
			continue
		}
		if key == RecordMaxDistKm {
			var d struct {
				Bearing *float64 `json:"bearing"`
				AltFt   *float64 `json:"altFt"`
				Lat     *float64 `json:"lat"`
			}
			if err := json.Unmarshal([]byte(r.Detail), &d); err != nil || d.Bearing == nil || d.Lat == nil || r.ICAO == 0 {
				t.Errorf("farthest record detail %q (%v)", r.Detail, err)
			}
			if math.Abs(r.Value-snapMax(st, ctx, t)) > 0.01 {
				t.Errorf("farthest record %.2f km differs from the flights table", r.Value)
			}
		}
	}
	evs, err := st.Events(ctx, store.EventQuery{Kind: "emergency"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ICAO != icaoEm {
		t.Errorf("emergency events: %+v", evs)
	}
	mil, err := st.Events(ctx, store.EventQuery{Kind: "military"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	var alerts, milNotes int
	for _, n := range notices {
		switch n.Kind {
		case "emergency":
			alerts++
			if n.Level != "alert" || n.Text == "" || n.T == 0 {
				t.Errorf("emergency notice %+v", n)
			}
		case "military":
			milNotes++
		}
	}
	mu.Unlock()
	if alerts != 1 {
		t.Errorf("%d emergency notices, want 1", alerts)
	}
	if len(mil) == 0 || milNotes != len(mil) {
		t.Errorf("%d military events, %d notices", len(mil), milNotes)
	}
	if ac, ok := tr.Aircraft(icaoEm); !ok || ac.Emergency != "general" || ac.Squawk != "7700" {
		t.Errorf("emergency aircraft: %+v", ac)
	}

	// A second flush must not write anything twice.
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.MinuteStats(ctx, "", from, to)
	var frames int64
	for _, r := range rows {
		frames += r.Frames
	}
	if uint64(frames) != c.Antennas[0].Frames {
		t.Errorf("frames after second flush %d, want %d", frames, c.Antennas[0].Frames)
	}

	// Reset drops everything.
	tr.Reset()
	if c := tr.Counters(); c.Frames != 0 || c.Tracked != 0 || len(tr.Snapshot().Aircraft) != 0 {
		t.Errorf("state after Reset: %+v", c)
	}
}

func TestHourRollover(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	s, cfg := newSim(t, 25, func(c *sim.Config) { c.PerfectReception = false })
	tr := New(Options{Store: st, Config: &cfg})
	sink := &countingSink{t: tr}
	// 12:00:30 -> 13:36:30 crosses one hour boundary and lets early
	// aircraft leave, go dormant and have their flights closed.
	advance(t, s, 96*time.Minute, sink)
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	from, to := simStart.Unix()-3600, s.Now().Unix()+3600
	c := tr.Counters()

	rows, err := st.MinuteStats(ctx, "", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 97 {
		t.Errorf("%d minute rows, want 97", len(rows))
	}
	var frames, bad int64
	for i, r := range rows {
		frames += r.Frames
		bad += r.CRCBad
		if i > 0 && r.T != rows[i-1].T+60 {
			t.Errorf("minute rows not contiguous at %d", r.T)
		}
		if r.Frames == 0 || r.Aircraft == 0 || r.Aircraft > 80 || r.AircraftPos > r.Aircraft || r.ADSB+r.MLAT+r.ModeS > r.Aircraft {
			t.Errorf("minute row %+v", r)
		}
	}
	if uint64(frames) != c.Frames-c.Duplicates {
		t.Errorf("stored frames %d, want %d", frames, c.Frames-c.Duplicates)
	}
	if uint64(bad) != c.CRCBad+c.Rejected {
		t.Errorf("stored crc_bad %d, want %d", bad, c.CRCBad+c.Rejected)
	}
	hours, err := st.RangeHours(ctx, "", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 2 || hours[1].T != hours[0].T+3600 {
		t.Fatalf("range_hour rows: %d", len(hours))
	}
	var pos int64
	for _, h := range hours {
		pos += h.Positions
		if h.Aircraft == 0 || h.Aircraft > 200 || !(h.MaxKm > 0) {
			t.Errorf("range row %+v", h)
		}
	}
	if uint64(pos) != c.Positions-c.MLATPositions {
		t.Errorf("range positions %d, want %d", pos, c.Positions-c.MLATPositions)
	}
	if n, err := st.RollupHours(ctx); err != nil || n == 0 {
		t.Errorf("RollupHours: %d %v", n, err)
	}

	// Flights of aircraft that left more than 30 minutes ago are closed
	// and their aircraft dropped from memory.
	flights, err := st.RecentFlights(ctx, 5000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(flights)) != c.Flights {
		t.Errorf("%d flight rows, %d flights opened", len(flights), c.Flights)
	}
	if c.Tracked >= len(flights) {
		t.Errorf("tracked %d aircraft, %d flights ever: nothing was dropped", c.Tracked, len(flights))
	}
	ids := map[int64]bool{}
	var msgs int64
	for _, f := range flights {
		if ids[f.ID] {
			t.Fatalf("duplicate flight id %d", f.ID)
		}
		ids[f.ID] = true
		msgs += f.Msgs
	}
	// Nearly every accepted frame belongs to a flight (all but the first
	// frame of aircraft that were never confirmed).
	if float64(msgs) < 0.99*float64(c.Accepted) || uint64(msgs) > c.Accepted {
		t.Errorf("flight msgs %d, accepted frames %d", msgs, c.Accepted)
	}
	var seenMsgs, seenFlights int64
	if err := st.DB().QueryRowContext(ctx, `SELECT SUM(msgs), SUM(flights) FROM aircraft`).Scan(&seenMsgs, &seenFlights); err != nil {
		t.Fatal(err)
	}
	if seenMsgs != msgs || seenFlights != int64(len(flights)) {
		t.Errorf("aircraft table: msgs %d flights %d, flights table: %d / %d", seenMsgs, seenFlights, msgs, len(flights))
	}
	if c.PosJumps > 3 {
		t.Errorf("%d track restarts in clean simulator data", c.PosJumps)
	}
	t.Logf("frames %d dup %d rejected %d crcBad %d positions %d (rejected %d, pending %d, jumps %d) flights %d tracked %d",
		c.Frames, c.Duplicates, c.Rejected, c.CRCBad, c.Positions, c.PosRejected, c.PosPending, c.PosJumps, c.Flights, c.Tracked)
}

func TestAircraftDatabase(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const icao = 0x3C6444
	b := store.NewBatch()
	b.AddACDB([]store.ACDBEntry{{ICAO: icao, Reg: "D-AIBD", Type: "A319", Desc: "AIRBUS A-319", Operator: "Lufthansa", Flags: store.FlagInteresting}})
	if err := st.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	cfg := model.ExampleRadarcapeConfig()
	var notes []Notice
	tr := New(Options{Store: st, Config: &cfg, OnNotice: func(n Notice) { notes = append(notes, n) }})
	for i := 0; i < 3; i++ {
		for _, a := range []uint32{icao, 0x406B75} {
			tr.HandleFrame(model.RawFrame{Recv: simStart.Add(time.Duration(i) * time.Second), GPSNs: -1, Signal: -20,
				Kind: model.FrameModeSShort, Data: df11(a), FeedID: "x", Channel: -1})
		}
	}
	ac, _ := tr.Aircraft(icao)
	if ac.Country != "Germany" || ac.CountryCode != "DE" {
		t.Errorf("allocation data: %q %q", ac.Country, ac.CountryCode)
	}
	if err := tr.Flush(ctx); err != nil { // resolves the queued look-ups
		t.Fatal(err)
	}
	ac, _ = tr.Aircraft(icao)
	if ac.Registration != "D-AIBD" || ac.TypeCode != "A319" || ac.TypeDesc == "" || ac.Operator != "Lufthansa" {
		t.Errorf("aircraft database not applied: %+v", ac)
	}
	if other, ok := tr.Aircraft(0x406B75); !ok || other.TypeCode != "" || other.Country != "United Kingdom" {
		t.Errorf("aircraft without a database entry: %+v", other)
	}
	if len(notes) != 1 || notes[0].Kind != "interesting" {
		t.Errorf("notices %+v", notes)
	}
	row, ok, err := st.AircraftByICAO(ctx, icao)
	if err != nil || !ok || row.Reg != "D-AIBD" || row.Flags&store.FlagInteresting == 0 || row.Msgs != 3 {
		t.Errorf("aircraft row %+v (%v, %v)", row, ok, err)
	}
	// A re-import is picked up after InvalidateRefdata.
	b.AddACDB([]store.ACDBEntry{{ICAO: icao, Reg: "D-ABCD", Type: "A320"}})
	if err := st.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	tr.InvalidateRefdata()
	if err := tr.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if ac, _ = tr.Aircraft(icao); ac.Registration != "D-ABCD" || ac.TypeCode != "A320" {
		t.Errorf("after re-import: %q %q", ac.Registration, ac.TypeCode)
	}
}

func snapMax(st *store.Store, ctx context.Context, t *testing.T) float64 {
	var v float64
	if err := st.DB().QueryRowContext(ctx, `SELECT MAX(max_dist_km) FROM flights WHERE src_mask & 1`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEHSHook(t *testing.T) {
	s, cfg := newSim(t, 60, nil)
	tr := New(Options{Config: &cfg})
	var mu sync.Mutex
	calls := map[uint32][]EHSEvent{}
	truths := map[uint32][]sim.Truth{}
	tr.SetEHSHook(func(ev EHSEvent) EHSResult {
		// Inside HandleFrame the simulator's truth is exactly the state the
		// frame was encoded from.
		tru, _ := s.TruthOf(ev.ICAO, ev.T)
		mu.Lock()
		calls[ev.ICAO] = append(calls[ev.ICAO], ev)
		truths[ev.ICAO] = append(truths[ev.ICAO], tru)
		mu.Unlock()
		// Must be callable without deadlock.
		_ = tr.Counters()
		return EHSResult{WindDir: 270, WindSpd: 42, OAT: -51.5, HasWind: true, HasOAT: true}
	})
	advance(t, s, 3*time.Minute, tr)
	if len(calls) == 0 {
		t.Fatal("EHS hook never called")
	}
	var full, mrar int
	for icao, evs := range calls {
		for i, ev := range evs {
			if math.IsNaN(ev.Lat) || math.IsNaN(ev.Lon) {
				t.Fatalf("%06X: event without position", icao)
			}
			if i > 0 && ev.T.Sub(evs[i-1].T) < hookIntervalMs*time.Millisecond {
				t.Fatalf("%06X: hook called %v apart", icao, ev.T.Sub(evs[i-1].T))
			}
			tru := truths[icao][i]
			if tru.ICAO != icao {
				continue
			}
			if !math.IsNaN(ev.TAS) && !math.IsNaN(ev.MagHeading) && !math.IsNaN(ev.GS) {
				full++
				if math.Abs(ev.TAS-tru.TAS) > 15 || math.Abs(geo.AngleDiff(ev.MagHeading, tru.MagHeading)) > 8 ||
					math.Abs(ev.GS-tru.GS) > 15 || math.Abs(ev.AltFt-tru.AltBaro) > 400 {
					t.Errorf("%06X: event %+v vs truth tas %.0f hdg %.1f gs %.0f alt %.0f", icao, ev, tru.TAS, tru.MagHeading, tru.GS, tru.AltBaro)
				}
			}
			if !math.IsNaN(ev.MRARWindSpd) {
				mrar++
				if math.Abs(ev.MRARWindSpd-tru.WindKt) > 3 || math.Abs(geo.AngleDiff(ev.MRARWindDir, tru.WindDir)) > 4 ||
					math.Abs(ev.MRARTemp-tru.OAT) > 1.5 {
					t.Errorf("%06X: MRAR wind %.0f/%.0f temp %.1f, truth %.0f/%.0f %.1f", icao,
						ev.MRARWindDir, ev.MRARWindSpd, ev.MRARTemp, tru.WindDir, tru.WindKt, tru.OAT)
				}
			}
		}
		ac, ok := tr.Aircraft(icao)
		if ok && time.UnixMilli(ac.LastSeen).Sub(evs[len(evs)-1].T) < 30*time.Second {
			if ac.WindSpd == nil || *ac.WindSpd != 42 || ac.OAT == nil || *ac.OAT != -51.5 || ac.WindDir == nil {
				t.Errorf("%06X: hook result not stored: %+v %+v", icao, ac.WindSpd, ac.OAT)
			}
		}
	}
	if full == 0 {
		t.Error("no event carried TAS + heading + ground speed")
	}
	t.Logf("%d aircraft, %d complete events, %d with MRAR", len(calls), full, mrar)
}

func TestHandleUpdate(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon = true, 50.0, 8.5
	cfg.Feeds = []model.Feed{{ID: "sbs", Enabled: true, Type: model.FeedSBS, AntennaID: "yagi"}}
	tr := New(Options{Config: &cfg})
	const icao = 0x3C4B26
	lat, lon := 50.5, 9.0
	for i := 0; i < 20; i++ {
		u := model.DecodedUpdate{
			Recv: simStart.Add(time.Duration(i) * time.Second), FeedID: "sbs", ICAO: icao,
			Callsign: model.S("dlh4ab "), Squawk: model.S("1000"),
			Lat: model.F(lat + 0.001*float64(i)), Lon: model.F(lon),
			AltBaro: model.F(36000), GS: model.F(450), Track: model.F(0), VertRate: model.F(0),
			OnGround: model.B(false), Signal: model.F(-18.5), Msgs: 3,
		}
		tr.HandleUpdate(u)
	}
	// An impossible jump is ignored.
	tr.HandleUpdate(model.DecodedUpdate{Recv: simStart.Add(20 * time.Second), FeedID: "sbs", ICAO: icao,
		Lat: model.F(53), Lon: model.F(12), Msgs: 1})
	// So is a position beyond the range limit, for a new aircraft.
	tr.HandleUpdate(model.DecodedUpdate{Recv: simStart.Add(20 * time.Second), FeedID: "sbs", ICAO: 0x111111,
		Lat: model.F(10), Lon: model.F(100), Msgs: 1})

	ac, ok := tr.Aircraft(icao)
	if !ok {
		t.Fatal("aircraft not tracked")
	}
	if ac.Callsign != "DLH4AB" || ac.Squawk != "1000" || ac.Source != model.SrcOther || ac.Msgs != 61 {
		t.Errorf("callsign %q squawk %q source %q msgs %d", ac.Callsign, ac.Squawk, ac.Source, ac.Msgs)
	}
	if ac.Operator == "" || ac.Country != "Germany" {
		t.Errorf("reference data: operator %q country %q", ac.Operator, ac.Country)
	}
	if ac.Lat == nil || math.Abs(*ac.Lat-(lat+0.019)) > 1e-9 || ac.DistKm == nil || ac.Elevation == nil || ac.SlantKm == nil {
		t.Fatalf("position / geometry: %+v %+v", ac.Lat, ac.DistKm)
	}
	want := geo.HaversineKm(50.0, 8.5, *ac.Lat, *ac.Lon)
	if math.Abs(*ac.DistKm-want) > 0.3 {
		t.Errorf("distance %.2f, want %.2f", *ac.DistKm, want)
	}
	if *ac.SlantKm < *ac.DistKm || *ac.Elevation < 5 || *ac.Elevation > 12 {
		t.Errorf("slant %.2f elevation %.2f", *ac.SlantKm, *ac.Elevation)
	}
	if len(ac.Antennas) != 1 || ac.Antennas[0] != "yagi" {
		t.Errorf("antennas %v", ac.Antennas)
	}
	if o, ok := tr.Aircraft(0x111111); !ok || o.Lat != nil {
		t.Errorf("out-of-range position accepted: %+v", o.Lat)
	}
	c := tr.Counters()
	if c.Updates != 22 || c.UpdateMsgs != 62 || c.Positions != 20 || c.PosRejected != 2 {
		t.Errorf("counters %+v", c)
	}
	if _, err := json.Marshal(tr.Snapshot()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotNeverNaN(t *testing.T) {
	// Frames without signal level, receiver not located.
	s, cfg := newSim(t, 30, nil)
	cfg.Receiver.Located = false
	tr := New(Options{Config: &cfg})
	nan := sinkFunc(func(f model.RawFrame) {
		f.Signal = math.NaN()
		f.GPSNs = -1
		tr.HandleFrame(f)
	})
	advance(t, s, 40*time.Second, nan)
	snap := tr.Snapshot()
	if snap.Counts.WithPos == 0 {
		t.Fatal("no positions without a receiver location")
	}
	for _, a := range snap.Aircraft {
		if a.DistKm != nil || a.RSSI != nil {
			t.Fatalf("%s has geometry / rssi without location / signal", a.ICAO)
		}
	}
	if _, err := json.Marshal(snap); err != nil {
		t.Fatal(err)
	}
	if lat := tr.FeedLatency(); len(lat) != 0 {
		t.Errorf("latency without GPS timestamps: %+v", lat)
	}
	if _, err := json.Marshal(tr.Counters()); err != nil {
		t.Fatal(err)
	}
}

type sinkFunc func(model.RawFrame)

func (f sinkFunc) HandleFrame(fr model.RawFrame)    { f(fr) }
func (f sinkFunc) HandleUpdate(model.DecodedUpdate) {}

type fixedClock time.Duration

func (c fixedClock) Offset() time.Duration { return time.Duration(c) }
func (c fixedClock) Synced() bool          { return true }

func TestLatencyMidnightWrap(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	tr := New(Options{Config: &cfg, Clock: fixedClock(-30 * time.Millisecond)})
	f := df11(0x3C6444)
	// Frames stamped by the receiver just before midnight arrive 80 ms later
	// on a host clock that runs 30 ms fast: latency 50 ms after the GPS stamp
	// (end of frame), across midnight.
	base := time.Date(2026, 3, 10, 23, 59, 59, 950_000_000, time.UTC)
	for i := 0; i < 50; i++ {
		gps := base.Add(time.Duration(i) * 50 * time.Millisecond)
		ns := int64(gps.Hour())*3600e9 + int64(gps.Minute())*60e9 + int64(gps.Second())*1e9 + int64(gps.Nanosecond())
		tr.HandleFrame(model.RawFrame{Recv: gps.Add(80 * time.Millisecond), GPSNs: ns, Signal: -20,
			Kind: model.FrameModeSShort, Data: f, FeedID: "rc", Channel: -1})
	}
	lat := tr.FeedLatency()
	if len(lat) != 1 || lat[0].Samples != 50 {
		t.Fatalf("latency %+v", lat)
	}
	l := lat[0]
	// + 64 us: measured from the start of the (short) frame
	if math.Abs(l.MinMs-50.064) > 0.001 || math.Abs(l.MaxMs-50.064) > 0.001 || math.Abs(l.P50Ms-50.064) > 0.001 || !l.ClockSynced || l.ClockOffsetMs != -30 {
		t.Errorf("latency min %.3f max %.3f p50 %.3f offset %.3f", l.MinMs, l.MaxMs, l.P50Ms, l.ClockOffsetMs)
	}
}

func TestRunIdleClock(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	tr := New(Options{Config: &cfg})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx) }()
	f := df11(0x3C6444)
	now := time.Now()
	tr.HandleFrame(model.RawFrame{Recv: now, GPSNs: -1, Kind: model.FrameModeSShort, Data: f, Channel: -1})
	tr.HandleFrame(model.RawFrame{Recv: now.Add(10 * time.Millisecond), GPSNs: -1, Kind: model.FrameModeSShort, Data: f, Channel: -1})
	t0 := tr.Counters().Now
	deadline := time.Now().Add(5 * time.Second)
	for tr.Counters().Now < t0+1000 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := tr.Counters().Now; got < t0+1000 {
		t.Errorf("data clock did not advance while idle: %d -> %d", t0, got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}
}

// recordFrames captures simulator output for the benchmark.
type recorder struct{ frames []model.RawFrame }

func (r *recorder) HandleFrame(f model.RawFrame)     { r.frames = append(r.frames, f) }
func (r *recorder) HandleUpdate(model.DecodedUpdate) {}

// BenchmarkHandleFrame measures the frame path alone ("base") and with the
// latency-chain analysis attached ("latency"); both report ns/frame and must
// stay at 0 allocs/op.
func BenchmarkHandleFrame(b *testing.B) {
	b.Run("base", func(b *testing.B) { benchHandleFrame(b, nil) })
	b.Run("latency", func(b *testing.B) {
		benchHandleFrame(b, func(tr *Tracker, cfg *model.Config) { tr.SetLatency(newLatency(cfg)) })
	})
}

// newLatency returns a latency analyzer set up for the receiver of cfg.
func newLatency(cfg *model.Config) *lchain.Analyzer {
	a := lchain.New(lchain.Options{})
	if cfg.Receiver.Located {
		a.SetReceiver(cfg.Receiver.Lat, cfg.Receiver.Lon, cfg.Receiver.GroundElevM+10)
	}
	return a
}

func benchHandleFrame(b *testing.B, setup func(tr *Tracker, cfg *model.Config)) {
	s, cfg := newSim(b, 150, nil)
	rec := &recorder{}
	const span = 20 * time.Second
	advance(b, s, span, rec)
	tr := New(Options{Config: &cfg})
	if setup != nil {
		setup(tr, &cfg)
	}
	// Warm up: aircraft, trails and maps reach their steady-state size.
	for pass := 0; pass < 3; pass++ {
		for _, f := range rec.frames {
			f.Recv = f.Recv.Add(time.Duration(pass) * span)
			tr.HandleFrame(f)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	pass, i := 3, 0
	shift := time.Duration(pass) * span
	for n := 0; n < b.N; n++ {
		f := rec.frames[i]
		f.Recv = f.Recv.Add(shift)
		tr.HandleFrame(f)
		if i++; i == len(rec.frames) {
			i = 0
			pass++
			shift = time.Duration(pass) * span
		}
	}
	b.StopTimer()
	c := tr.Counters()
	b.ReportMetric(float64(c.Positions)/float64(c.Frames), "pos/frame")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/frame")
}

// TestLatencyHook checks that the latency analysis sees the tracker's
// frames, positions decoded from the message itself, and that the frame
// path stays allocation-free with it attached.
func TestLatencyHook(t *testing.T) {
	s, cfg := newSim(t, 40, nil)
	rec := &recorder{}
	advance(t, s, 30*time.Second, rec)
	tr := New(Options{Config: &cfg})
	lat := newLatency(&cfg)
	tr.SetLatency(lat)
	for _, f := range rec.frames {
		tr.HandleFrame(f)
	}
	sum := lat.Summary()
	c := tr.Counters()
	if sum.Counts.Frames == 0 || sum.Counts.Frames > int64(c.Accepted) {
		t.Fatalf("latency frames %d, tracker accepted %d", sum.Counts.Frames, c.Accepted)
	}
	if sum.Counts.Positions == 0 {
		t.Error("no positions reached the latency analysis")
	}
	// Only positions decoded from the message itself carry a range.
	if sum.Counts.PositionsRanged == 0 || sum.Counts.PositionsRanged > int64(c.Positions) {
		t.Errorf("latency ranged positions %d, tracker positions %d", sum.Counts.PositionsRanged, c.Positions)
	}
	if len(lat.Aircraft()) == 0 {
		t.Error("no per-aircraft latency rows")
	}
	// Steady state: no allocations per frame.
	shift := 31 * time.Second
	i := 0
	allocs := testing.AllocsPerRun(2000, func() {
		f := rec.frames[i%len(rec.frames)]
		f.Recv = f.Recv.Add(shift * time.Duration(1+i/len(rec.frames)))
		tr.HandleFrame(f)
		i++
	})
	if allocs > 0.05 {
		t.Errorf("HandleFrame with latency: %.3f allocs/frame", allocs)
	}
}

// df16RA builds a DF16 air-air reply whose MV carries an RA (VDS 3,0).
func df16RA(icao uint32, ac13 uint16) []byte {
	f := make([]byte, 14)
	f[0] = 16 << 3
	f[2], f[3] = byte(ac13>>8), byte(ac13)
	f[4] = 0x30
	f[5] = 0xC2 // MV bits 9 (RA in force), 10 (corrective), 15 (positive); upward sense -> "Climb"
	modes.SetParity(f, icao)
	return f
}

func TestACASHook(t *testing.T) {
	cfg := model.ExampleRadarcapeConfig()
	tr := New(Options{Config: &cfg})
	var got []acas.Report
	tr.SetACASHook(func(r acas.Report) { got = append(got, r) })
	send := func(dt time.Duration, data []byte) {
		kind := model.FrameModeSShort
		if len(data) == 14 {
			kind = model.FrameModeSLong
		}
		tr.HandleFrame(model.RawFrame{Recv: simStart.Add(dt), GPSNs: -1, Signal: -20, Kind: kind, Data: data, FeedID: "x", Channel: -1})
	}
	const icao = 0x4840D6
	alt, _ := modes.EncodeAC13(11000)
	send(0, df11(icao))
	send(time.Second, df11(icao))
	send(1100*time.Millisecond, df16RA(icao, alt))
	if len(got) != 1 || got[0].ICAO != icao || got[0].DF != 16 || !got[0].RA.Active() || !got[0].AltOK || got[0].AltFt != 11000 {
		t.Fatalf("%+v", got)
	}
	text, sense, _ := acas.Describe(&got[0].RA)
	if text != "Climb" || sense != "up" {
		t.Fatalf("%q %q (ARA %014b)", text, sense, got[0].RA.ARA)
	}
}
