package simnet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// perRx records one receiver's observations (an RxSink).
type perRx struct{ obs []Observation }

func (p *perRx) Observe(o *Observation) { c := *o; c.Tx = nil; p.obs = append(p.obs, c) }
func (p *perRx) Advance(float64)        {}

func deviceNet(t *testing.T, seed uint64, devs ...Device) *Network {
	t.Helper()
	var specs []ReceiverSpec
	for i, d := range devs {
		r := ReceiverSpec{Lat: 50 + 0.3*float64(i), Lon: 8 + 0.2*float64(i), GroundHAE: 150, AntennaAGL: 15,
			Clock: ClockSpec{Device: d, JitterNs: 50, InitialPPM: 10, CableBiasNs: 100, PPSJitterNs: 20,
				FrameRefShortNs: 64000, FrameRefLongNs: 120000}}
		specs = append(specs, r)
	}
	n, err := New(Config{Seed: seed, Aircraft: 150, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBeastEmitterParses(t *testing.T) {
	devs := []Device{DeviceRadarcapeGPS, DeviceRadarcape12, DeviceBeast, DeviceSDR, DeviceAirspy, DeviceSBS}
	n := deviceNet(t, 21, devs...)
	bufs := make([]*bytes.Buffer, len(devs))
	recs := make([]*perRx, len(devs))
	ems := make([]*BeastEmitter, len(devs))
	for i := range devs {
		bufs[i] = &bytes.Buffer{}
		recs[i] = &perRx{}
		n.Attach(i, recs[i])
		ems[i] = n.NewBeastEmitter(i, &WriterSink{W: bufs[i]}, BeastOptions{PositionEveryS: 10})
	}
	if err := n.Advance(context.Background(), 12*time.Second); err != nil {
		t.Fatal(err)
	}
	for i, d := range devs {
		var frames []feed.BeastFrame
		var status, pos int
		p := feed.NewBeastParser(func(f *feed.BeastFrame) {
			switch f.Type {
			case feed.BeastStatusType:
				status++
				s, ok := feed.ParseBeastStatus(f.Payload)
				if !ok || (d == DeviceRadarcapeGPS) != s.GPSTimestamps {
					t.Fatalf("%s: status %+v", d, s)
				}
				if d == DeviceRadarcapeGPS && !s.GPSLocked() {
					t.Fatalf("%s: GPS not locked", d)
				}
				return
			case feed.BeastPosition:
				pos++
				rp, ok := feed.ParseBeastPosition(f.Payload)
				if !ok || rp.Lat < 49 || rp.Lat > 52 {
					t.Fatalf("position frame %+v", rp)
				}
				return
			}
			c := *f
			c.Payload = append([]byte(nil), f.Payload...)
			frames = append(frames, c)
		})
		// feed in odd-sized chunks
		b := bufs[i].Bytes()
		for len(b) > 0 {
			k := min(len(b), 997)
			p.Write(b[:k])
			b = b[k:]
		}
		if p.Garbage != 0 || p.Truncated != 0 {
			t.Fatalf("%s: garbage %d truncated %d", d, p.Garbage, p.Truncated)
		}
		obs := recs[i].obs
		if len(frames) != len(obs) || uint64(len(obs)) != ems[i].Frames || len(obs) < 100 {
			t.Fatalf("%s: %d frames parsed, %d observed, %d written", d, len(frames), len(obs), ems[i].Frames)
		}
		for j, f := range frames {
			o := &obs[j]
			if f.Timestamp != o.BeastTS || f.Signal != o.SignalByte || !bytes.Equal(f.Payload, o.Data) {
				t.Fatalf("%s frame %d: ts %x/%x sig %d/%d", d, j, f.Timestamp, o.BeastTS, f.Signal, o.SignalByte)
			}
			if d == DeviceSBS && f.Timestamp >= 1<<24 {
				t.Fatal("SBS timestamp wider than 24 bits")
			}
		}
		radar := d == DeviceRadarcapeGPS || d == DeviceRadarcape12
		if radar && (status < 10 || pos < 1) || !radar && (status != 0 || pos != 0) {
			t.Fatalf("%s: %d status, %d position frames", d, status, pos)
		}
		// GPS timestamps decode with the feed Timestamper conventions
		if d == DeviceRadarcapeGPS {
			o := &obs[len(obs)/2]
			ts := frames[len(obs)/2].Timestamp
			nsOfDay := int64(ts>>30)*1e9 + int64(ts&0x3FFFFFFF)
			if got, _ := model.GPSTicks(nsOfDay, o.HostNs); got != o.Ticks {
				t.Fatalf("GPS ticks %d != %d", got, o.Ticks)
			}
		}
	}
}

func TestBeastReducePlus(t *testing.T) {
	n := deviceNet(t, 22, DeviceSDR)
	var buf bytes.Buffer
	rec := &perRx{}
	n.Attach(0, rec)
	e := n.NewBeastEmitter(0, &WriterSink{W: &buf}, BeastOptions{Format: BeastReducePlus, ReduceInterval: time.Second, ReceiverID: 0x1A2B3C4D5E6F7081})
	if err := n.Advance(context.Background(), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	uuid := n.Receiver(0).UUID
	if !bytes.HasPrefix(b, append([]byte{0x1A, 0xE4}, uuid...)) || string(b[38:41]) != "\x1aWO" {
		t.Fatalf("preamble %q", b[:48])
	}
	if b[41] != 0x1A || b[42] != 0xE3 || b[43] != 0x1A || b[44] != 0x1A {
		t.Fatalf("receiver id escape % x", b[41:52])
	}
	var frames int
	p := feed.NewBeastParser(func(f *feed.BeastFrame) { frames++ })
	p.Write(b)
	if uint64(frames) != e.Frames || e.Reduced == 0 || e.Frames+e.Reduced != uint64(len(rec.obs)) {
		t.Fatalf("frames %d written %d reduced %d observed %d", frames, e.Frames, e.Reduced, len(rec.obs))
	}
	// The current feed parser does not know the 0xE4 escape yet (WP-F adds
	// the callback); it may skip the 38 preamble bytes and 'W','O' as
	// garbage, but nothing else.
	if p.Garbage > 41 {
		t.Fatalf("garbage %d", p.Garbage)
	}
	if e.Frames > uint64(len(rec.obs))*3/4 {
		t.Fatalf("reduce kept %d of %d", e.Frames, len(rec.obs))
	}
}

func TestMLATClientStream(t *testing.T) {
	n := deviceNet(t, 23, DeviceBeast)
	var buf bytes.Buffer
	rec := &perRx{}
	n.Attach(0, rec)
	e := n.NewMLATClientEmitter(0, &WriterSink{W: &buf}, MLATClientOptions{SeenEveryS: 5, RateEveryS: 10, HeartbeatS: 10})
	if err := n.Advance(context.Background(), 31*time.Second); err != nil {
		t.Fatal(err)
	}
	byTicks := map[uint64]*Observation{}
	for i := range rec.obs {
		byTicks[rec.obs[i].Ticks] = &rec.obs[i]
	}
	sc := bufio.NewScanner(&buf)
	sc.Buffer(nil, 1<<20)
	kinds := map[string]int{}
	first := true
	for sc.Scan() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		if first {
			first = false
			var hs struct {
				Version   int     `json:"version"`
				UUID      string  `json:"uuid"`
				ClockType string  `json:"clock_type"`
				Lat       float64 `json:"lat"`
			}
			json.Unmarshal(sc.Bytes(), &hs)
			if hs.Version != 3 || hs.UUID != n.Receiver(0).UUID || hs.ClockType != "beast" || hs.Lat == 0 {
				t.Fatalf("handshake %s", sc.Text())
			}
			continue
		}
		for k, v := range m {
			kinds[k]++
			switch k {
			case "sync":
				var s struct {
					ET, OT uint64
					EM, OM string
				}
				json.Unmarshal(v, &s)
				eo, oo := byTicks[s.ET], byTicks[s.OT]
				if eo == nil || oo == nil || hex.EncodeToString(eo.Data) != s.EM || hex.EncodeToString(oo.Data) != s.OM || eo.ICAO != oo.ICAO {
					t.Fatalf("sync does not match observations: %s", v)
				}
				if (eo.Data[6]>>2)&1 != 0 || (oo.Data[6]>>2)&1 != 1 {
					t.Fatal("sync parity order")
				}
			case "mlat":
				var s struct {
					T uint64
					M string
				}
				json.Unmarshal(v, &s)
				if o := byTicks[s.T]; o == nil || hex.EncodeToString(o.Data) != s.M {
					t.Fatalf("mlat does not match: %s", v)
				}
			}
		}
	}
	for _, k := range []string{"input_connected", "sync", "mlat", "seen", "rate_report", "heartbeat"} {
		if kinds[k] == 0 {
			t.Fatalf("no %s message (%v)", k, kinds)
		}
	}
	if uint64(kinds["sync"]) != e.Syncs || uint64(kinds["mlat"]) != e.MLATs {
		t.Fatal("counters")
	}
	t.Logf("messages: %v", kinds)
}

func TestMLATClientClockReset(t *testing.T) {
	spec := ReceiverSpec{Lat: 50, Lon: 8, Clock: ClockSpec{Device: DeviceSDR, Drops: []SampleDrop{{AtS: 5, Samples: 1000, Declared: true}}}}
	n, err := New(Config{Seed: 3, Aircraft: 150, Receivers: []ReceiverSpec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n.NewMLATClientEmitter(0, &WriterSink{W: &buf}, MLATClientOptions{})
	if err := n.Advance(context.Background(), 8*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `{"clock_reset":{"epoch":`) || !strings.Contains(buf.String(), `"reason":"sample_drop"`) {
		t.Fatal("no clock_reset for the declared sample drop")
	}
}

func TestAVFEmitter(t *testing.T) {
	spec := ReceiverSpec{Lat: 50, Lon: 8, GroundHAE: 100, Clock: ClockSpec{Device: DeviceRadarcapeGPS, CableBiasNs: 100, BootGPSTimeS: 3},
		Link: LinkSpec{Outages: []Interval{{8, 12}}}}
	spec2 := ReceiverSpec{Lat: 50.2, Lon: 8.1, Clock: ClockSpec{Device: DeviceSDR, JitterNs: 400}}
	n, err := New(Config{Seed: 4, Aircraft: 150, Receivers: []ReceiverSpec{spec, spec2}})
	if err != nil {
		t.Fatal(err)
	}
	link := n.NewLink(0, nil)
	var r0, r1 AVFRecorder
	n.NewAVFEmitter(0, &r0, AVFOptions{Conn: link, SendLocation: true})
	n.NewAVFEmitter(1, &r1, AVFOptions{})
	if err := n.Advance(context.Background(), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	check := func(name string, r *AVFRecorder) map[byte]int {
		cnt := map[byte]int{}
		epochs := map[uint32]bool{}
		var cur uint32
		for _, x := range r.Records {
			cnt[x.Type]++
			switch x.Type {
			case 0x01:
				cur = x.Receiver.EpochID
				epochs[cur] = true
				if x.Receiver.NominalHz != x.Receiver.ClockClass.NominalHz() {
					t.Fatalf("%s: receiver %+v", name, x.Receiver)
				}
			case 0x02:
				f := x.Frames
				if !epochs[f.EpochID] {
					t.Fatalf("%s: FRAMES of epoch %d before its RECEIVER/CLOCK_EVENT", name, f.EpochID)
				}
				if len(f.Frames) == 0 || f.BaseTicks != f.Frames[0].Ticks || len(f.Frames) > 4096 {
					t.Fatalf("%s: bad batch", name)
				}
				for i := 1; i < len(f.Frames); i++ {
					if f.Frames[i].Ticks < f.Frames[i-1].Ticks {
						t.Fatalf("%s: frames not sorted by ticks", name)
					}
				}
			case 0x05:
				epochs[x.Event.NewEpoch] = true
				if !x.Event.Reason.Valid() {
					t.Fatalf("%s: event %+v", name, x.Event)
				}
			}
		}
		return cnt
	}
	c0 := check("gps", &r0)
	c1 := check("sdr", &r1)
	if c0[0x03] < 10 || c0[0x04] < 2 || c0[0x05] < 1 || c0[0x0B] != 2 || c0[0] != 2 {
		t.Fatalf("gps record counts %v", c0)
	}
	if c1[0x03] != 0 || c1[0x04] < 3 || c1[0x02] < 100 {
		t.Fatalf("sdr record counts %v", c1)
	}
	// backfilled frames carry the late bit, live ones do not
	var late, live int
	for _, x := range r0.Records {
		if x.Type == 0x02 {
			for _, f := range x.Frames.Frames {
				if f.Late {
					late++
					if float64(f.HostNs-n.startNs)/1e9 < 7.9 || float64(f.HostNs-n.startNs)/1e9 > 12.1 {
						t.Fatalf("late frame from %v s, outside the outage", float64(f.HostNs-n.startNs)/1e9)
					}
				} else {
					live++
				}
			}
		}
	}
	if late == 0 || live == 0 {
		t.Fatalf("late %d live %d", late, live)
	}
	// location sent for rx 0
	for _, x := range r0.Records {
		if x.Type == 0x01 && (x.Receiver.Location == nil || x.Receiver.Location.AltRef != "msl") {
			t.Fatal("location missing")
		}
	}
}

func TestLink(t *testing.T) {
	n := deviceNet(t, 24, DeviceBeast)
	var got []Delivery
	l := n.NewLink(0, func(d Delivery) { got = append(got, d) })
	l.Spec = LinkSpec{LatencyMs: 30, JitterMs: 20, Outages: []Interval{{5, 7}}}
	e := n.NewBeastEmitter(0, l, BeastOptions{Format: BeastReducePlus})
	if err := n.Advance(context.Background(), 12*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(got) < 10 || l.Lost != 0 && e.Dropped == 0 {
		t.Fatalf("deliveries %d lost %d dropped %d", len(got), l.Lost, e.Dropped)
	}
	sessions := map[int]bool{}
	var prev int64
	pre := 0
	for _, d := range got {
		if d.ArrivalNs < prev || d.ArrivalNs-d.HostNs < 30e6 {
			t.Fatal("arrivals out of order or faster than the latency")
		}
		prev = d.ArrivalNs
		s := float64(d.HostNs-n.startNs) / 1e9
		if s >= 5 && s < 7 {
			t.Fatalf("data written during the outage at %v s", s)
		}
		if !sessions[d.Session] && bytes.HasPrefix(d.Data, []byte{0x1A, 0xE4}) {
			pre++
		}
		sessions[d.Session] = true
	}
	if len(sessions) != 2 || pre != 2 {
		t.Fatalf("sessions %v, preambles %d", sessions, pre)
	}
}

func TestTruthLog(t *testing.T) {
	n := deviceNet(t, 25, DeviceBeast, DeviceRadarcapeGPS)
	var buf bytes.Buffer
	tl := &TruthLog{N: n, W: &buf, Observations: true}
	n.AddSink(tl)
	if err := n.Advance(context.Background(), 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if tl.Err != nil {
		t.Fatal(tl.Err)
	}
	cnt := map[string]int{}
	sc := bufio.NewScanner(&buf)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var l struct{ Type string }
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		cnt[l.Type]++
	}
	if cnt["receivers"] != 1 || cnt["aircraft"] < 100 || cnt["clock"] != 8 || cnt["obs"] < 100 {
		t.Fatalf("truth log %v", cnt)
	}
}
