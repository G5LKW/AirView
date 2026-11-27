package sim

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"airview/internal/model"
)

var testStart = time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)

type cprHalf struct {
	yz, xz  uint64
	t       time.Time
	surface bool
	valid   bool
}

// streamChecker is a FrameSink that decodes every frame with the independent
// reference decoder and compares it with the simulator's ground truth.
type streamChecker struct {
	t *testing.T
	s *Sim

	prev  map[uint32]Truth // truth at the start of the step being delivered
	prev2 map[uint32]Truth // ... and of the step before (frames received one step late)
	prevT time.Time        // virtual time of the prev snapshot
	known map[uint32]bool  // every address that has ever been simulated

	halves     map[uint32]*[2]cprHalf
	mlatHalves map[uint32]*[2]cprHalf
	lastRecv   time.Time
	lastGPS    int64
	t1Pos      int // airborne positions with T=1
	lastData   []byte
	stRef      model.Receiver

	frames, status, mlatFrames int
	byDF, byTC                 [32]int
	bds                        map[string]int
	posPairs, surfPairs        int
	mlatPairs, cprSkipped      int
	worstPos, worstSurf        float64
	worstMLAT                  float64
	emergencyFrames            int
	alertPositions             int
	errs                       int
}

func newStreamChecker(t *testing.T, s *Sim) *streamChecker {
	c := &streamChecker{
		t: t, s: s,
		prev:       map[uint32]Truth{},
		prev2:      map[uint32]Truth{},
		known:      map[uint32]bool{},
		halves:     map[uint32]*[2]cprHalf{},
		mlatHalves: map[uint32]*[2]cprHalf{},
		bds:        map[string]int{},
		stRef:      s.Station(),
	}
	c.snapshot()
	return c
}

// snapshot records the truth at the current virtual time, i.e. the state the
// frames of the next step are generated from.
func (c *streamChecker) snapshot() {
	c.prev, c.prev2 = c.prev2, c.prev
	clear(c.prev)
	c.prevT = c.s.Now()
	for _, tr := range c.s.Truth() {
		c.prev[tr.ICAO] = tr
		c.known[tr.ICAO] = true
	}
}

func (c *streamChecker) fail(f model.RawFrame, format string, args ...any) {
	c.errs++
	if c.errs <= 12 {
		c.t.Errorf("%s %X: %s", f.Recv.Format("15:04:05.000000"), f.Data, fmt.Sprintf(format, args...))
	}
}

// truthTime is the time a frame's content applies to: the transmit time
// (TransmitTime minus the propagation delay over the slant range, computed
// from the truth like the simulator does), or for an airborne position with
// T=1 the 0.2 s UTC epoch of its CPR format parity nearest to it.
func (c *streamChecker) truthTime(f model.RawFrame, icao uint32) (tx, epoch time.Time) {
	t := TransmitTime(f)
	if f.GPSNs >= 0 {
		if tr, ok := c.s.TruthOf(icao, t); ok {
			st := c.s.Station()
			ant := c.s.Config().Antennas[0]
			ha := math.Max(tr.AltBaro*0.3048-st.GroundElevM, 2) / 1000
			if tr.OnGround {
				ha = 0.004
			}
			hs := math.Max(ant.HeightAGLM, 1) / 1000
			ae := 4.0 / 3 * 6371.0
			sh := math.Sin(tr.DistKm / ae / 2)
			r := math.Sqrt((ha-hs)*(ha-hs) + 4*(ae+hs)*(ae+ha)*sh*sh)
			t = t.Add(-time.Duration(r * 1e9 / radioKmPerS))
		}
	}
	if len(f.Data) != 14 || (refDF(f.Data) != 17 && refDF(f.Data) != 18) {
		return t, time.Time{}
	}
	me := f.Data[4:11]
	tc := rb(me, 1, 5)
	if !(tc >= 9 && tc <= 18 || tc >= 20 && tc <= 22) || rb(me, 21, 21) == 0 {
		return t, time.Time{}
	}
	odd := rb(me, 22, 22) == 1
	const ep = int64(200 * time.Millisecond)
	ns := t.UnixNano()
	k := (ns + ep/2) / ep
	if (k&1 == 1) != odd {
		if ns >= k*ep {
			k++
		} else {
			k--
		}
	}
	return t, time.Unix(0, k*ep).UTC()
}

// rxDelayOK: the simulated receiver -> host delay (Recv minus the GPS time
// stamp) is positive and well below 100 ms.
func rxDelayOK(d int64) bool { return d > 0 && d < int64(100*time.Millisecond) }

func (c *streamChecker) HandleUpdate(model.DecodedUpdate) {
	c.t.Error("the simulator must not produce DecodedUpdates")
}

func (c *streamChecker) HandleFrame(f model.RawFrame) {
	c.frames++
	if f.FeedID != "test" {
		c.fail(f, "FeedID %q", f.FeedID)
	}
	if f.Recv.Before(c.lastRecv) {
		c.fail(f, "frames out of order: %v after %v", f.Recv, c.lastRecv)
	}
	c.lastRecv = f.Recv
	if now := c.s.Now(); f.Recv.After(now) || now.Sub(f.Recv) > 100*time.Millisecond {
		c.fail(f, "Recv %v is not within the step before Now %v", f.Recv, now)
	}
	if f.Clock12MHz != 0 {
		c.fail(f, "Clock12MHz set")
	}
	sod := int64(f.Recv.Sub(f.Recv.Truncate(24 * time.Hour)))

	switch f.Kind {
	case model.FrameStatus:
		c.status++
		if len(f.Data) != 14 || !rxDelayOK(sod-f.GPSNs) || f.GPSNs%1e9 != 0 || f.Channel != -1 || f.MLAT {
			c.fail(f, "status frame: len %d GPSNs %d channel %d", len(f.Data), f.GPSNs, f.Channel)
		}
		if f.Data[0]&0x10 == 0 || f.Data[2]&0x80 == 0 {
			c.fail(f, "status frame does not announce GPS timestamps")
		}
		return
	case model.FrameModeSShort:
		if len(f.Data) != 7 {
			c.fail(f, "short frame with %d bytes", len(f.Data))
			return
		}
	case model.FrameModeSLong:
		if len(f.Data) != 14 {
			c.fail(f, "long frame with %d bytes", len(f.Data))
			return
		}
	default:
		c.fail(f, "unexpected frame kind %d", f.Kind)
		return
	}
	df := refDF(f.Data)
	if (df >= 16) != (len(f.Data) == 14) {
		c.fail(f, "DF%d with %d bytes", df, len(f.Data))
		return
	}

	if f.MLAT {
		c.mlat(f, df)
		return
	}
	if f.Channel != 0 && f.Channel != 1 {
		c.fail(f, "channel %d", f.Channel)
	}
	if math.IsNaN(f.Signal) || f.Signal > 0 || f.Signal < -60 {
		c.fail(f, "signal %v", f.Signal)
	}
	// GPS time of arrival (end of frame) before the host receive time by a
	// realistic receiver -> host delay; arrival order = stream order
	if !rxDelayOK(sod - f.GPSNs) {
		c.fail(f, "GPSNs %d, Recv time of day %d: receiver delay %v", f.GPSNs, sod, time.Duration(sod-f.GPSNs))
	}
	if f.GPSNs < c.lastGPS {
		c.fail(f, "GPS timestamps out of order: %d after %d", f.GPSNs, c.lastGPS)
	}
	c.lastGPS = f.GPSNs
	if f.Channel == 1 {
		// perfect reception: the copy heard by the second antenna follows
		// the first one
		if !bytes.Equal(f.Data, c.lastData) {
			c.fail(f, "channel 1 copy differs from the channel 0 frame")
		}
		return
	}
	c.lastData = f.Data
	c.byDF[df]++

	var icao uint32
	switch df {
	case 11, 17:
		if r := refRemainder(f.Data); r != 0 {
			c.fail(f, "DF%d parity remainder %06X", df, r)
			return
		}
		icao = refICAO(f.Data)
	case 0, 4, 5, 20, 21:
		icao = refRemainder(f.Data) // address overlaid on the parity
	default:
		c.fail(f, "unexpected DF%d", df)
		return
	}
	tx, epoch := c.truthTime(f, icao)
	tr, ok := c.s.TruthOf(icao, tx)
	if ok && !epoch.IsZero() {
		// T=1: the transponder extrapolated the position to the epoch
		dt := epoch.Sub(tx).Seconds()
		v := tr.GS * 1852 / 3600 * dt
		sn, cs := math.Sincos(tr.Track * math.Pi / 180)
		tr.Lat += v * cs / 111195.08
		tr.Lon += v * sn / (111195.08 * math.Cos(tr.Lat*math.Pi/180))
		tr.AltBaro += tr.VertRate / 60 * dt
		tr.AltGeom += tr.VertRate / 60 * dt
		c.t1Pos++
	}
	if !ok {
		if !c.known[icao] {
			c.fail(f, "DF%d from address %06X which was never simulated", df, icao)
		}
		return // left the simulation during this step
	}
	// the non-kinematic state (squawk, emergency, selected altitude) the
	// frame was generated from
	p, hasPrev := c.prev[icao]
	if tx.Before(c.prevT) {
		p, hasPrev = c.prev2[icao]
	}

	switch df {
	case 11:
		c.checkCA(f, tr)
	case 17:
		c.es(f, tr, p, hasPrev)
	case 0:
		if (rb(f.Data, 6, 6) == 1) != tr.OnGround {
			c.fail(f, "DF0 VS bit, on ground = %v", tr.OnGround)
		}
		c.checkAlt13(f, tr)
	case 4, 20:
		c.checkFS(f, tr, p, hasPrev)
		c.checkAlt13(f, tr)
	case 5, 21:
		c.checkFS(f, tr, p, hasPrev)
		if sq := refSquawk(rb(f.Data, 20, 32)); hasPrev && sq != p.Squawk {
			c.fail(f, "DF%d squawk %s, truth %s", df, sq, p.Squawk)
		} else if sq == "7700" {
			c.emergencyFrames++
		}
	}
	if df == 20 || df == 21 {
		c.commB(f, tr, p, hasPrev)
	}
}

func (c *streamChecker) checkCA(f model.RawFrame, tr Truth) {
	want := uint64(5)
	if tr.OnGround {
		want = 4
	}
	if ca := rb(f.Data, 6, 8); ca != want {
		c.fail(f, "CA %d, on ground = %v", ca, tr.OnGround)
	}
}

func (c *streamChecker) checkFS(f model.RawFrame, tr, p Truth, hasPrev bool) {
	fs := rb(f.Data, 6, 8)
	if fs > 3 || (fs&1 == 1) != tr.OnGround {
		c.fail(f, "FS %d, on ground = %v", fs, tr.OnGround)
	}
	if hasPrev && p.Emergency != "" && fs&2 == 0 {
		c.fail(f, "FS %d without alert during an emergency", fs)
	}
	if rb(f.Data, 9, 19) != 0 {
		c.fail(f, "DR / UM not zero")
	}
}

func (c *streamChecker) checkAlt13(f model.RawFrame, tr Truth) {
	alt, ok := refAlt13(rb(f.Data, 20, 32))
	if !ok || math.Abs(alt-tr.AltBaro) > 12.5+1e-6 {
		c.fail(f, "altitude code %v ft (%v), truth %.1f ft", alt, ok, tr.AltBaro)
	}
}

// es checks a DF17 extended squitter.
func (c *streamChecker) es(f model.RawFrame, tr, p Truth, hasPrev bool) {
	if !tr.ADSB || tr.MLAT {
		c.fail(f, "DF17 from an aircraft without ADS-B")
		return
	}
	me := f.Data[4:11]
	tc := int(rb(me, 1, 5))
	c.byTC[tc]++
	icao := tr.ICAO
	switch {
	case tc >= 1 && tc <= 4:
		c.checkCA(f, tr)
		if cs := refCallsign(me, 9); cs != tr.Callsign {
			c.fail(f, "callsign %q, truth %q", cs, tr.Callsign)
		}
		if cat := fmt.Sprintf("%c%d", "DCBA"[tc-1], rb(me, 6, 8)); cat != tr.Category {
			c.fail(f, "category %s, truth %s", cat, tr.Category)
		}

	case tc >= 5 && tc <= 8:
		c.checkCA(f, tr)
		if !tr.OnGround {
			c.fail(f, "surface position from an airborne aircraft")
			return
		}
		lo, hi, ok := refMovement(rb(me, 6, 12))
		if !ok || tr.GS < lo-1e-9 || tr.GS >= hi+1e-9 {
			c.fail(f, "movement [%v, %v) kt, truth %.3f kt", lo, hi, tr.GS)
		}
		if trk := float64(rb(me, 14, 20)) * 360 / 128; rb(me, 13, 13) != 1 || angDiff(trk, tr.Track) > 360.0/256+1e-6 {
			c.fail(f, "ground track %.2f, truth %.2f", trk, tr.Track)
		}
		c.position(f, tr, c.halves, me, true)

	case tc >= 9 && tc <= 18, tc >= 20 && tc <= 22:
		c.checkCA(f, tr)
		if tr.OnGround {
			c.fail(f, "airborne position from an aircraft on the ground")
			return
		}
		want := tr.AltBaro
		if tc >= 20 {
			want = tr.AltGeom
		}
		if (tc >= 20) != tr.GNSSAlt {
			c.fail(f, "TC %d but GNSSAlt = %v", tc, tr.GNSSAlt)
		}
		if alt, ok := refAlt12(rb(me, 9, 20)); !ok || math.Abs(alt-want) > 12.5+1e-6 {
			c.fail(f, "TC %d altitude %v ft, truth %.1f ft", tc, alt, want)
		}
		ss := rb(me, 6, 7)
		if hasPrev && (p.Emergency != "") != (ss == 1) {
			c.fail(f, "surveillance status %d, emergency %q", ss, p.Emergency)
		}
		if ss == 1 {
			c.alertPositions++
		}
		c.position(f, tr, c.halves, me, false)

	case tc == 19:
		v, ok := refVelocity(me)
		if !ok {
			c.fail(f, "not a subtype 1 velocity with valid components")
			return
		}
		sn, cs := math.Sincos(tr.Track * math.Pi / 180)
		if math.Abs(v.ew-tr.GS*sn) > 0.5+1e-6 || math.Abs(v.ns-tr.GS*cs) > 0.5+1e-6 {
			c.fail(f, "velocity %+.0f E %+.0f N, truth %.2f kt on %.2f deg", v.ew, v.ns, tr.GS, tr.Track)
		}
		if math.Abs(v.vr-tr.VertRate) > 32+1e-6 {
			c.fail(f, "vertical rate %v, truth %.1f", v.vr, tr.VertRate)
		}
		if !v.hasDelta || math.Abs(v.delta-(tr.AltGeom-tr.AltBaro)) > 12.5+1 {
			c.fail(f, "GNSS-baro difference %v, truth %.1f", v.delta, tr.AltGeom-tr.AltBaro)
		}

	case tc == 28:
		if rb(me, 6, 8) != 1 {
			c.fail(f, "TC28 subtype %d", rb(me, 6, 8))
		}
		if hasPrev {
			if sq := refSquawk(rb(me, 12, 24)); sq != p.Squawk {
				c.fail(f, "TC28 squawk %s, truth %s", sq, p.Squawk)
			}
			if e := emergencyNames[rb(me, 9, 11)]; e != p.Emergency {
				c.fail(f, "TC28 emergency %q, truth %q", e, p.Emergency)
			} else if e != "" {
				c.emergencyFrames++
			}
		}

	case tc == 29:
		if tr.ADSBVersion != 2 || tr.OnGround {
			c.fail(f, "TC29 from version %d, on ground = %v", tr.ADSBVersion, tr.OnGround)
		}
		if rb(me, 6, 7) != 1 {
			c.fail(f, "TC29 subtype %d", rb(me, 6, 7))
		}
		if n := rb(me, 10, 20); hasPrev && (n == 0 || math.Abs(float64(n-1)*32-p.SelAlt) > 16) {
			c.fail(f, "selected altitude %v, truth %v", float64(n-1)*32, p.SelAlt)
		}
		if n := rb(me, 21, 29); n == 0 || 800+float64(n-1)*0.8 < 940 || 800+float64(n-1)*0.8 > 1060 {
			c.fail(f, "baro setting code %d", n)
		}

	case tc == 31:
		c.checkCA(f, tr)
		st := rb(me, 6, 8)
		if tr.ADSBVersion != 2 || rb(me, 41, 43) != 2 || st > 1 || (st == 1) != tr.OnGround {
			c.fail(f, "TC31 subtype %d version %d, truth version %d on ground %v", st, rb(me, 41, 43), tr.ADSBVersion, tr.OnGround)
		}

	default:
		c.fail(f, "unexpected type code %d from %06X", tc, icao)
	}
}

// position pairs the CPR halves of an aircraft and checks the globally
// decoded position against the truth at the time of the newer frame.
func (c *streamChecker) position(f model.RawFrame, tr Truth, store map[uint32]*[2]cprHalf, me []byte, surface bool) {
	h := store[tr.ICAO]
	if h == nil {
		h = new([2]cprHalf)
		store[tr.ICAO] = h
	}
	odd := int(rb(me, 22, 22))
	h[odd] = cprHalf{yz: rb(me, 23, 39), xz: rb(me, 40, 56), t: f.Recv, surface: surface, valid: true}
	o := h[1-odd]
	if !o.valid || o.surface != surface || f.Recv.Sub(o.t) > 10*time.Second {
		return
	}
	var lat, lon float64
	var ok bool
	if surface {
		lat, lon, ok = refCPRSurface(h[0].yz, h[0].xz, h[1].yz, h[1].xz, odd == 1, c.stRef.Lat, c.stRef.Lon)
	} else {
		lat, lon, ok = refCPRAirborne(h[0].yz, h[0].xz, h[1].yz, h[1].xz, odd == 1)
	}
	if !ok {
		c.cprSkipped++
		return
	}
	d := refDistM(lat, lon, tr.Lat, tr.Lon)
	switch {
	case f.MLAT:
		c.mlatPairs++
		c.worstMLAT = math.Max(c.worstMLAT, d)
		if d > 1500 {
			c.fail(f, "MLAT position %.0f m from the truth", d)
		}
	case surface:
		c.surfPairs++
		c.worstSurf = math.Max(c.worstSurf, d)
		if d > 10 {
			c.fail(f, "surface position %.6f %.6f is %.1f m from the truth %.6f %.6f", lat, lon, d, tr.Lat, tr.Lon)
		}
	default:
		c.posPairs++
		c.worstPos = math.Max(c.worstPos, d)
		if d > 10 {
			c.fail(f, "position %.6f %.6f is %.1f m from the truth %.6f %.6f", lat, lon, d, tr.Lat, tr.Lon)
		}
	}
}

// commB checks that the MB field of a DF20/21 is one of the supported
// registers and that its content matches the aircraft state.
func (c *streamChecker) commB(f model.RawFrame, tr, p Truth, hasPrev bool) {
	mb := f.Data[4:11]
	if mb[0] == 0x20 {
		if cs := refCallsign(mb, 9); cs != tr.Callsign {
			c.fail(f, "BDS 2,0 callsign %q, truth %q", cs, tr.Callsign)
		}
		c.bds["2,0"]++
		return
	}
	if tr.OnGround {
		c.fail(f, "EHS register from an aircraft on the ground")
		return
	}
	if r, ok := refDecode44(mb); ok {
		if math.Abs(r.windKt-tr.WindKt) > 0.5+1e-3 || (tr.WindKt > 2 && angDiff(r.windDir, tr.WindDir) > 180.0/512+1e-3) ||
			math.Abs(r.sat-tr.OAT) > 0.125+1e-3 || r.press < 100 || r.press > 1050 {
			c.fail(f, "BDS 4,4 %+v, truth wind %.1f/%.1f OAT %.2f", r, tr.WindDir, tr.WindKt, tr.OAT)
		}
		c.bds["4,4"]++
		return
	}
	if r, ok := refDecode50(mb); ok &&
		math.Abs(r.roll-tr.Roll) <= 45.0/512+1e-6 && angDiff(r.track, tr.Track) <= 90.0/1024+1e-6 &&
		math.Abs(r.gs-tr.GS) <= 1+1e-6 && math.Abs(r.tas-tr.TAS) <= 1+1e-6 &&
		math.Abs(r.trackRate-tr.TrackRate) <= 8.0/512+1e-6 {
		c.bds["5,0"]++
		return
	}
	if r, ok := refDecode60(mb); ok &&
		angDiff(r.hdg, tr.MagHeading) <= 90.0/1024+1e-3 && math.Abs(r.ias-tr.IAS) <= 0.5+1e-3 &&
		math.Abs(r.mach-tr.Mach) <= 0.002+1e-5 &&
		math.Abs(r.baroRate-tr.VertRate) <= 16+1e-6 && math.Abs(r.inertialRate-tr.VertRate) <= 16+1e-6 {
		c.bds["6,0"]++
		return
	}
	if r, ok := refDecode40(mb); ok && r.source == 2 && r.baro > 940 && r.baro < 1060 &&
		(!hasPrev || math.Abs(r.mcpAlt-p.SelAlt) <= 8) {
		c.bds["4,0"]++
		return
	}
	c.fail(f, "Comm-B MB %X matches no register for roll %.2f trk %.2f gs %.1f tas %.1f rate %.3f | hdg %.2f ias %.1f M %.4f vr %.0f | sel %.0f",
		mb, tr.Roll, tr.Track, tr.GS, tr.TAS, tr.TrackRate, tr.MagHeading, tr.IAS, tr.Mach, tr.VertRate, p.SelAlt)
}

// mlat checks a synthetic MLAT result frame.
func (c *streamChecker) mlat(f model.RawFrame, df int) {
	c.mlatFrames++
	if df != 18 || rb(f.Data, 6, 8) != 2 || refRemainder(f.Data) != 0 {
		c.fail(f, "MLAT result is not a valid DF18 CF=2 frame")
		return
	}
	if f.GPSNs != -1 || f.Channel != -1 || !math.IsNaN(f.Signal) {
		c.fail(f, "MLAT result with GPSNs %d channel %d signal %v", f.GPSNs, f.Channel, f.Signal)
	}
	tx, _ := c.truthTime(f, refICAO(f.Data))
	tr, ok := c.s.TruthOf(refICAO(f.Data), tx)
	if !ok {
		return
	}
	if !tr.MLAT || tr.ADSB {
		c.fail(f, "MLAT result for an aircraft that is not an MLAT target")
	}
	me := f.Data[4:11]
	switch tc := rb(me, 1, 5); tc {
	case 18:
		// An MLAT result is stamped when the client injects it; behind a
		// delayed receiver frame that can be up to 60 ms after its time.
		if alt, ok := refAlt12(rb(me, 9, 20)); !ok || math.Abs(alt-tr.AltBaro) > 12.5+math.Abs(tr.VertRate)/60*0.06+1e-6 {
			c.fail(f, "MLAT altitude %v, truth %.1f", alt, tr.AltBaro)
		}
		c.position(f, tr, c.mlatHalves, me, false)
	case 19:
		v, ok := refVelocity(me)
		sn, cs := math.Sincos(tr.Track * math.Pi / 180)
		if !ok || math.Abs(v.ew-tr.GS*sn) > 30 || math.Abs(v.ns-tr.GS*cs) > 30 {
			c.fail(f, "MLAT velocity %+v, truth %.1f kt on %.1f deg", v, tr.GS, tr.Track)
		}
	default:
		c.fail(f, "MLAT result with type code %d", tc)
	}
}

// TestStreamMatchesGroundTruth is the main end-to-end check of the encoder:
// with perfect reception every transmitted frame is delivered, decoded by the
// reference decoder and compared with the truth it was generated from.
func TestStreamMatchesGroundTruth(t *testing.T) {
	minutes := 20
	if testing.Short() {
		minutes = 2
	}
	s := New(Config{
		Aircraft: 120, Seed: 20260601, Start: testStart, FeedID: "test",
		NoDiurnal: true, PerfectReception: true,
		MLATFraction: 0.12, GNSSAltFraction: 0.08, MilitaryFraction: 0.08,
	})
	c := newStreamChecker(t, s)
	ctx := context.Background()

	var adsbAirSec, adsbSec, allSec float64
	for i := 0; i < minutes*600; i++ {
		for _, tr := range c.prev {
			allSec += stepDt
			if tr.ADSB {
				adsbSec += stepDt
				if !tr.OnGround {
					adsbAirSec += stepDt
				}
			}
		}
		if i == 600 {
			if _, ok := s.TriggerEmergency(); !ok {
				t.Fatal("TriggerEmergency found no aircraft")
			}
			c.snapshot()
		}
		if err := s.Advance(ctx, 100*time.Millisecond, c); err != nil {
			t.Fatal(err)
		}
		c.snapshot()
	}
	if c.errs > 0 {
		t.Fatalf("%d frames disagree with the ground truth", c.errs)
	}

	t.Logf("%d frames, DF0 %d DF4 %d DF5 %d DF11 %d DF17 %d DF20 %d DF21 %d, MLAT %d, status %d",
		c.frames, c.byDF[0], c.byDF[4], c.byDF[5], c.byDF[11], c.byDF[17], c.byDF[20], c.byDF[21], c.mlatFrames, c.status)
	t.Logf("type codes: %v", c.byTC[:])
	t.Logf("Comm-B registers: %v", c.bds)
	t.Logf("positions: %d airborne pairs (worst %.2f m), %d surface pairs (worst %.2f m), %d MLAT pairs (worst %.0f m), %d pairs across a zone boundary",
		c.posPairs, c.worstPos, c.surfPairs, c.worstSurf, c.mlatPairs, c.worstMLAT, c.cprSkipped)

	for _, df := range []int{0, 4, 5, 11, 17, 20, 21} {
		if c.byDF[df] == 0 {
			t.Errorf("no DF%d generated", df)
		}
	}
	ident := c.byTC[1] + c.byTC[2] + c.byTC[3] + c.byTC[4]
	surf := c.byTC[5] + c.byTC[6] + c.byTC[7] + c.byTC[8]
	var baro, gnss int
	for tc := 9; tc <= 18; tc++ {
		baro += c.byTC[tc]
	}
	for tc := 20; tc <= 22; tc++ {
		gnss += c.byTC[tc]
	}
	for name, n := range map[string]int{
		"identification": ident, "surface position": surf, "baro position": baro, "GNSS position": gnss,
		"velocity": c.byTC[19], "emergency status": c.byTC[28], "target state": c.byTC[29], "operational status": c.byTC[31],
	} {
		if n == 0 {
			t.Errorf("no %s messages generated", name)
		}
	}
	for _, reg := range []string{"2,0", "4,0", "5,0", "6,0", "4,4"} {
		if c.bds[reg] == 0 {
			t.Errorf("no BDS %s replies generated", reg)
		}
	}
	if c.bds["4,4"] > c.bds["6,0"]/4 {
		t.Errorf("BDS 4,4 should be occasional: %v", c.bds)
	}
	if c.posPairs < 1000*minutes || c.surfPairs < 10 || c.mlatPairs < 10 {
		t.Errorf("too few position pairs checked: %d airborne, %d surface, %d MLAT", c.posPairs, c.surfPairs, c.mlatPairs)
	}
	if c.cprSkipped > c.posPairs/200 {
		t.Errorf("%d CPR pairs could not be decoded", c.cprSkipped)
	}
	if c.status < minutes*60-1 || c.status > minutes*60+1 {
		t.Errorf("%d status frames in %d minutes", c.status, minutes)
	}
	if minutes > 1 && (c.emergencyFrames == 0 || c.alertPositions == 0) {
		t.Errorf("triggered emergency not visible: %d emergency frames, %d alert positions", c.emergencyFrames, c.alertPositions)
	}

	// squitter rates per aircraft
	rate := func(name string, n int, sec, lo, hi float64) {
		r := float64(n) / sec
		t.Logf("%-22s %.3f /s per aircraft", name, r)
		if r < lo || r > hi {
			t.Errorf("%s rate %.3f /s, want %.2f..%.2f", name, r, lo, hi)
		}
	}
	rate("airborne position", baro+gnss, adsbAirSec, 1.85, 2.15)
	rate("airborne velocity", c.byTC[19], adsbAirSec, 1.85, 2.15)
	rate("identification", ident, adsbSec, 0.15, 0.22)
	rate("DF11 all-call", c.byDF[11], allSec, 0.9, 1.1)
	rate("DF4/5/20/21", c.byDF[4]+c.byDF[5]+c.byDF[20]+c.byDF[21], allSec, 0.5, 3)
	rate("DF0", c.byDF[0], allSec, 0.2, 1.2)
}

// TestMLATTargets: with every civil aircraft an MLAT target there is no
// extended squitter at all, only Mode S replies plus flagged MLAT results.
func TestMLATTargets(t *testing.T) {
	s := New(Config{
		Aircraft: 40, Seed: 99, Start: testStart, FeedID: "test",
		NoDiurnal: true, PerfectReception: true, MLATFraction: 1, MilitaryFraction: 0.2,
	})
	c := newStreamChecker(t, s)
	for i := 0; i < 900; i++ {
		if err := s.Advance(context.Background(), 100*time.Millisecond, c); err != nil {
			t.Fatal(err)
		}
		c.snapshot()
	}
	if c.errs > 0 {
		t.Fatalf("%d frames disagree with the ground truth", c.errs)
	}
	if c.byDF[17] != 0 {
		t.Errorf("%d DF17 frames although no aircraft has ADS-B", c.byDF[17])
	}
	if c.mlatFrames == 0 || c.mlatPairs == 0 {
		t.Errorf("no MLAT results: %d frames, %d pairs", c.mlatFrames, c.mlatPairs)
	}
	var mil, mlat int
	for _, tr := range s.Truth() {
		switch {
		case tr.Kind == "military":
			mil++
			if tr.ADSB || tr.MLAT {
				t.Errorf("military %06X has a position source", tr.ICAO)
			}
		case tr.MLAT:
			mlat++
		default:
			t.Errorf("%06X (%s) is neither military nor MLAT", tr.ICAO, tr.Kind)
		}
	}
	if mil == 0 || mlat == 0 {
		t.Errorf("%d military, %d MLAT aircraft", mil, mlat)
	}
	t.Logf("%d MLAT frames, %d pairs, worst %.0f m; %d military (no position), %d MLAT aircraft", c.mlatFrames, c.mlatPairs, c.worstMLAT, mil, mlat)
}
