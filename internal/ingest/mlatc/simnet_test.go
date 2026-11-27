package mlatc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// --------------------------------------------------------- recording

// chunk is one write of an emitter: bytes and the feeder host time.
type chunk struct {
	rx     int
	hostNs int64
	b      []byte
}

type chunkRec struct {
	rx     int
	mu     sync.Mutex
	chunks []chunk
}

func (r *chunkRec) Write(h int64, p []byte) {
	r.mu.Lock()
	r.chunks = append(r.chunks, chunk{rx: r.rx, hostNs: h, b: append([]byte(nil), p...)})
	r.mu.Unlock()
}

// sentFrame is a frame a client put on the wire, with its truth.
type sentFrame struct {
	obs simnet.Observation
	t   uint64 // the value sent
	// sync: sent as half of a sync pair (or as ssync)
	sync bool
}

// obsRec keeps every observation of a receiver (Data copied).
type obsRec struct{ obs []simnet.Observation }

func (r *obsRec) Observe(o *simnet.Observation) {
	c := *o
	c.Tx = nil
	c.Data = append([]byte(nil), o.Data...)
	r.obs = append(r.obs, c)
}
func (r *obsRec) Advance(float64) {}

// ------------------------------------------------- real-client emulator

// realClient produces what the reference mlat-client sends for one
// receiver, from the receiver's raw Beast timestamps (MLATC.md §5): GPS
// clocks as nanoseconds of the UTC day at the start of the frame
// (64/120/20.3 µs subtracted, wrapped into the previous day), 12 MHz
// counters minus 768 (Mode S) / 244 (Mode A/C) ticks, SBS 24-bit counters
// plus 1120 (short) / 1920 (A/C) ticks, widened by borrowing the previous
// value's upper bits and adding 2^24 whenever the result would go down.
// DF17 airborne positions go out as even/odd sync pairs within 5 s, all
// other frames as mlat. Epoch changes become clock_reset.
type realClient struct {
	n      *simnet.Network
	rx     int
	dev    simnet.Device
	out    simnet.ByteSink
	epoch  uint32
	cls    model.ClockClass
	start  bool
	sbsW   uint64
	sbsSet bool
	pend   map[uint32]*[2]*sentFrame // ICAO -> waiting even/odd
	sent   []sentFrame               // in wire order
	glitch int                       // SBS widening wraps that were not real
	// split: the server negotiated split_sync; each sync candidate is sent
	// on its own (ssync) when it arrives, instead of as a delayed pair.
	split bool
}

func newRealClient(n *simnet.Network, rx int, out simnet.ByteSink) *realClient {
	c := &realClient{n: n, rx: rx, dev: n.Receiver(rx).Device, out: out, pend: map[uint32]*[2]*sentFrame{}}
	n.Attach(rx, c)
	return c
}

func (c *realClient) hello() []byte {
	r := c.n.Receiver(c.rx)
	h := map[string]any{
		"version": 3, "client_version": "0.2.13-emul", "compress": []string{"none", "zlib", "zlib2"},
		"selective_traffic": true, "heartbeat": true, "return_results": true, "return_stats": true,
		"udp_transport": false, "return_result_format": "ecef", "uuid": r.UUID, "user": r.Name,
		"lat": r.DeclaredLat, "lon": r.DeclaredLon, "alt": r.DeclaredAlt, "clock_type": c.dev.MLATClientType(),
		"privacy": true,
	}
	b, _ := json.Marshal(h)
	b = append(b, strings.Repeat(" ", 128)...)
	return append(b, '\n')
}

func (c *realClient) value(o *simnet.Observation) (uint64, bool) {
	ts := o.BeastTS
	switch {
	case o.Class.GPS():
		v := int64(ts>>30)*1e9 + int64(ts&(1<<30-1))
		switch len(o.Data) {
		case 2:
			v -= offGPSAC
		case 7:
			v -= offGPSShort
		default:
			v -= offGPSLong
		}
		if v < 0 {
			v += model.NsPerDay
		}
		return uint64(v), true
	case c.dev == simnet.DeviceSBS:
		off := uint64(0)
		switch len(o.Data) {
		case 2:
			off = offSBSAC
		case 7:
			off = offSBSShort
		}
		// A real SBS-1 outputs frames in stamp order (frames cannot be
		// stamped closer than their 64 µs length), so its widened counter
		// only ever wraps forward. simnet delivers frames in true arrival
		// order with independent stamp noise, so a stamp may be a few
		// ticks below the previous one; that is not a wrap.
		lo := (ts + off) & (1<<24 - 1)
		w := c.sbsW&^(1<<24-1) | lo
		if c.sbsSet && w < c.sbsW {
			if c.sbsW-w < 1<<23 {
				c.glitch++ // a reordering, not a wrap
			} else {
				w += 1 << 24
			}
		}
		if !c.sbsSet || w > c.sbsW {
			c.sbsW = w
		}
		c.sbsSet = true
		return w, true
	}
	off := uint64(off12ModeS)
	if len(o.Data) == 2 {
		off = off12ModeAC
	}
	if ts < off {
		return 0, false
	}
	return ts - off, true
}

func (c *realClient) Observe(o *simnet.Observation) {
	var lines bytes.Buffer
	if !c.start {
		c.start = true
		c.epoch, c.cls = o.Epoch, o.Class
		lines.Write(c.hello())
		lines.WriteString(`{"input_connected":"connected"}` + "\n")
		lines.WriteString(`{"rate_report":{}}` + "\n")
	}
	defer func() { c.cls = o.Class }()
	if o.Epoch != c.epoch {
		c.epoch = o.Epoch
		reason := "restart"
		for _, ev := range c.n.ClockEvents(c.rx) {
			if ev.NewEpoch == o.Epoch {
				reason = string(ev.Reason)
			}
		}
		switch {
		case c.dev == simnet.DeviceRadarcapeGPS && o.Class == model.ClockGPSEmul && c.cls != model.ClockGPSEmul:
			// the reference client's decoder switches to the emulated mode
			// when the Radarcape reports "timestamp not OK"
			lines.WriteString(`{"clock_reset":{"reason":"Decoder mode changed to RADARCAPE_EMULATED","frequency":1000000000,"epoch":null,"mode":"RADARCAPE_EMULATED"}}` + "\n")
		case c.dev == simnet.DeviceRadarcapeGPS && o.Class != model.ClockGPSEmul && c.cls == model.ClockGPSEmul:
			lines.WriteString(`{"clock_reset":{"reason":"Decoder mode changed to RADARCAPE","frequency":1000000000,"epoch":"utc_midnight","mode":"RADARCAPE"}}` + "\n")
		default:
			fmt.Fprintf(&lines, `{"clock_reset":{"reason":%q}}`+"\n", reason)
		}
		clear(c.pend)
	}
	v, ok := c.value(o)
	if ok {
		f := sentFrame{obs: *o, t: v}
		f.obs.Tx = nil
		f.obs.Data = append([]byte(nil), o.Data...)
		d := o.Data
		if d[0]>>3 == 17 && len(d) == 14 {
			if tc := d[4] >> 3; (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) {
				odd := int(d[6]>>2) & 1
				p := c.pend[o.ICAO]
				if p == nil {
					p = &[2]*sentFrame{}
					c.pend[o.ICAO] = p
				}
				f.sync = true
				p[odd] = &f
				if q := p[1-odd]; c.split && q != nil && o.ArrivalS-q.obs.ArrivalS <= 5 {
					fmt.Fprintf(&lines, `{"ssync":{"t":%d,"m":"%s"}}`+"\n", f.t, hex.EncodeToString(f.obs.Data))
					c.sent = append(c.sent, f)
					goto out
				}
				if q := p[1-odd]; q != nil && o.ArrivalS-q.obs.ArrivalS <= 5 {
					fmt.Fprintf(&lines, `{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n",
						p[0].t, hex.EncodeToString(p[0].obs.Data), p[1].t, hex.EncodeToString(p[1].obs.Data))
					c.sent = append(c.sent, *p[0], *p[1])
					p[0], p[1] = nil, nil
				}
				goto out
			}
		}
		fmt.Fprintf(&lines, `{"mlat":{"t":%d,"m":"%s"}}`+"\n", v, hex.EncodeToString(d))
		c.sent = append(c.sent, f)
	}
out:
	if lines.Len() > 0 {
		c.out.Write(o.HostNs, lines.Bytes())
	}
}

func (c *realClient) Advance(float64) {}

// ----------------------------------------------------------- replay

// replay runs recorded client streams against a server in virtual time:
// slice by slice, the server clock is set to the end of the slice, every
// chunk of the slice is written, and the next slice starts once the
// server has consumed all frames sent so far (so host times, rate
// accounting and GPS day resolution see consistent time).
type replay struct {
	t        *testing.T
	env      *testEnv
	clk      *testClock
	clients  map[int]*scriptClient
	expected uint64
	after    func(nowNs int64) // called after each slice (ingest sweep)
}

func frameCount(b []byte) uint64 {
	var n uint64
	for _, l := range bytes.Split(b, []byte("\n")) {
		switch {
		case bytes.HasPrefix(l, []byte(`{"sync":`)):
			n += 2
		case bytes.HasPrefix(l, []byte(`{"mlat":`)), bytes.HasPrefix(l, []byte(`{"ssync":`)):
			n++
		}
	}
	return n
}

func (r *replay) run(chunks []chunk, sliceNs int64) {
	t := r.t
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].hostNs < chunks[j].hostNs })
	if len(chunks) == 0 {
		t.Fatal("nothing to replay")
	}
	end := chunks[0].hostNs + sliceNs
	i := 0
	for i < len(chunks) {
		r.clk.SetNs(end)
		for ; i < len(chunks) && chunks[i].hostNs <= end; i++ {
			ch := chunks[i]
			b := ch.b
			c := r.clients[ch.rx]
			if c == nil {
				nl := bytes.IndexByte(b, '\n')
				c = dialHello(t, r.env.addr, b[:nl+1])
				if c.denied() != nil {
					t.Fatalf("rx %d denied: %v", ch.rx, c.denied())
				}
				r.clients[ch.rx] = c
				b = b[nl+1:]
			}
			r.expected += frameCount(b)
			if len(b) > 0 {
				if err := c.sendRaw(b); err != nil {
					t.Fatal(err)
				}
			}
		}
		waitFor(t, "server to consume the slice", 10*time.Second, func() bool {
			s := r.env.srv.Stats()
			return s.Frames+s.DroppedDup+s.DroppedBad+s.DroppedRate >= r.expected
		})
		if r.after != nil {
			r.after(end)
		}
		end += sliceNs
	}
}

func simNet(t *testing.T, seed uint64, rx, aircraft int, mix map[simnet.Device]float64) *simnet.Network {
	t.Helper()
	specs := simnet.RandomReceivers(seed, rx, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 200, Mix: mix})
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// simNetAllDevices is simNet with at least one receiver of every device
// (the first seed from seed on that has them), clock events on the first
// SDR (a declared sample drop at 20 s, a restart at 40 s) and a GPS lock
// loss on the first Radarcape GPS (25..35 s).
func simNetAllDevices(t *testing.T, seed uint64, rx, aircraft int) *simnet.Network {
	t.Helper()
	var specs []simnet.ReceiverSpec
	for ; ; seed++ {
		specs = simnet.RandomReceivers(seed, rx, simnet.Placement{CenterLat: 51, CenterLon: 9, RadiusKm: 200, Mix: allDevices})
		have := map[simnet.Device]bool{}
		for _, s := range specs {
			have[s.Clock.Device] = true
		}
		if len(have) == len(allDevices) {
			break
		}
	}
	sdr, gps := false, false
	for i := range specs {
		c := &specs[i].Clock
		switch {
		case c.Device == simnet.DeviceSDR && !sdr:
			sdr = true
			c.Drops = []simnet.SampleDrop{{AtS: 20, Samples: 5000, Declared: true}}
			c.Restarts = []float64{40}
		case c.Device == simnet.DeviceRadarcapeGPS && !gps:
			gps = true
			c.LockLoss = []simnet.Interval{{FromS: 25, ToS: 35}}
		}
	}
	n, err := simnet.New(simnet.Config{Seed: seed, Aircraft: aircraft, Receivers: specs})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var allDevices = map[simnet.Device]float64{
	simnet.DeviceRadarcapeGPS: 1, simnet.DeviceRadarcape12: 1, simnet.DeviceBeast: 1,
	simnet.DeviceSDR: 1, simnet.DeviceAirspy: 1, simnet.DeviceSBS: 1,
}

// stationFrames groups the sink's frames by station, in delivery order.
func stationFrames(fs *fakeSink) map[model.StationIdx][]model.RawFrame {
	frames, _ := fs.snapshot()
	m := map[model.StationIdx][]model.RawFrame{}
	for _, f := range frames {
		m[f.Station] = append(m[f.Station], f)
	}
	return m
}

// TestSimnetRealClientConvention replays the reference client's timestamp
// conventions (generated from simnet's raw receiver stamps) for every
// device type and checks that the server recovers the receivers' own ticks
// (the AirView convention used on AVF) exactly, with the clock epochs of
// the truth.
func TestSimnetRealClientConvention(t *testing.T) {
	const nrx = 12
	n := simNetAllDevices(t, 21, nrx, 150)
	clients := make([]*realClient, nrx)
	var recs []*chunkRec
	for i := 0; i < nrx; i++ {
		rec := &chunkRec{rx: i}
		recs = append(recs, rec)
		clients[i] = newRealClient(n, i, rec)
	}
	if err := n.Advance(context.Background(), 70*time.Second); err != nil {
		t.Fatal(err)
	}
	clk := newTestClock()
	env := startServer(t, Options{Now: clk.Now, Policy: loopbackPolicy}, nil, nil)
	r := &replay{t: t, env: env, clk: clk, clients: map[int]*scriptClient{}}
	var all []chunk
	for _, rec := range recs {
		all = append(all, rec.chunks...)
	}
	r.run(all, int64(200*time.Millisecond))

	byStation := stationFrames(env.sink)
	devCount := map[simnet.Device]int{}
	total := 0
	for i, c := range clients {
		tr := n.Receiver(i)
		st, id, ok := env.reg.Lookup("uuid:" + mustUUID(t, tr.UUID))
		if !ok {
			t.Fatalf("rx %d not registered", i)
		}
		if id.ClockType != tr.Device.MLATClientType() {
			t.Fatalf("rx %d clock type %q", i, id.ClockType)
		}
		got := byStation[st]
		if len(got) != len(c.sent) {
			t.Fatalf("rx %d (%s): %d frames delivered, %d sent", i, tr.Device, len(got), len(c.sent))
		}
		sbsOff := map[uint64]int{}
		emul := 0
		for k, f := range got {
			o := &c.sent[k].obs
			if !bytes.Equal(f.Data, o.Data) {
				t.Fatalf("rx %d frame %d: payload differs", i, k)
			}
			switch {
			case o.Class.GPS():
				if f.Ticks != o.Ticks {
					t.Fatalf("rx %d (%s) frame %d: ticks %d, truth %d (diff %d ns)", i, tr.Device, k, f.Ticks, o.Ticks, int64(f.Ticks-o.Ticks))
				}
			case tr.Device == simnet.DeviceSBS:
				d := f.Ticks - o.Ticks
				if d%(1<<24) != 0 {
					t.Fatalf("rx %d SBS frame %d: ticks %d vs truth %d not congruent mod 2^24", i, k, f.Ticks, o.Ticks)
				}
				sbsOff[d]++
			default:
				if f.Ticks&mask48 != o.Ticks&mask48 {
					t.Fatalf("rx %d (%s) frame %d: ticks %d, truth %d", i, tr.Device, k, f.Ticks, o.Ticks)
				}
			}
			// epochs: a server epoch change exactly where the truth epoch changes
			if k > 0 {
				if (f.Epoch != got[k-1].Epoch) != (o.Epoch != c.sent[k-1].obs.Epoch) {
					t.Fatalf("rx %d (%s) frame %d: epoch %d->%d, truth %d->%d; ticks %d -> %d (truth %d -> %d), arrival %.3f -> %.3f; events %v", i, tr.Device, k, got[k-1].Epoch, f.Epoch, c.sent[k-1].obs.Epoch, o.Epoch,
						got[k-1].Ticks, f.Ticks, c.sent[k-1].obs.Ticks, o.Ticks, c.sent[k-1].obs.ArrivalS, o.ArrivalS, evs(env.sink, st))
				}
			}
			cls, _ := classifyClock(tr.Device.MLATClientType())
			if o.Class == model.ClockGPSEmul {
				cls = model.ClockGPSEmul
				emul++
			}
			if f.ClockCls != cls {
				t.Fatalf("rx %d class %v want %v", i, f.ClockCls, cls)
			}
		}
		if emul > 0 {
			t.Logf("rx %d GPS: %d frames in emulated (lock lost) mode", i, emul)
		}
		if tr.Device == simnet.DeviceSBS {
			t.Logf("rx %d SBS: %d frames (%d stamped below their predecessor), %d distinct 2^24 offsets", i, len(got), c.glitch, len(sbsOff))
		}
		devCount[tr.Device] += len(got)
		total += len(got)
	}
	for d := range allDevices {
		if devCount[d] == 0 {
			t.Errorf("no frames from a %s receiver", d)
		}
	}
	s := env.srv.Stats()
	t.Logf("%d receivers, %d frames (%v), %d syncs, %d mlat, %d epochs, dups %d, bad %d, rate drops %d, %d wire bytes (zlib2)",
		nrx, total, devCount, s.Syncs, s.MLATs, s.Epochs, s.DroppedDup, s.DroppedBad, s.DroppedRate, s.WireBytes)
	if s.DroppedBad != 0 || s.DroppedRate != 0 || s.BadLines != 0 {
		t.Fatalf("drops: %+v", s)
	}
}

func mustUUID(t *testing.T, u string) string {
	t.Helper()
	n, ok := model.NormalizeUUID(u)
	if !ok {
		t.Fatalf("bad uuid %q", u)
	}
	return n
}

// TestSimnetEmitterReplay replays simnet's own mlat-client-like emitter
// (AirView tick convention, Options.RawTicks) and checks every delivered
// frame against the truth observation with the same ticks.
func TestSimnetEmitterReplay(t *testing.T) {
	const nrx = 8
	n := simNet(t, 5, nrx, 200, allDevices)
	var recs []*chunkRec
	obs := make([]*obsRec, nrx)
	for i := 0; i < nrx; i++ {
		rec := &chunkRec{rx: i}
		recs = append(recs, rec)
		obs[i] = &obsRec{}
		n.Attach(i, obs[i])
		n.NewMLATClientEmitter(i, rec, simnet.MLATClientOptions{SeenEveryS: 5, RateEveryS: 10, HeartbeatS: 20})
	}
	if err := n.Advance(context.Background(), 120*time.Second); err != nil {
		t.Fatal(err)
	}
	clk := newTestClock()
	env := startServer(t, Options{Now: clk.Now, RawTicks: true, FlushEvery: 10 * time.Millisecond, Policy: loopbackPolicy}, nil, nil)
	r := &replay{t: t, env: env, clk: clk, clients: map[int]*scriptClient{}}
	var all []chunk
	for _, rec := range recs {
		all = append(all, rec.chunks...)
	}
	r.run(all, int64(250*time.Millisecond))
	byStation := stationFrames(env.sink)
	total := 0
	for i := 0; i < nrx; i++ {
		tr := n.Receiver(i)
		st, _, _ := env.reg.Lookup("uuid:" + mustUUID(t, tr.UUID))
		type key struct {
			ticks uint64
			data  string
		}
		byTicks := map[key]*simnet.Observation{}
		for k := range obs[i].obs {
			o := &obs[i].obs[k]
			byTicks[key{o.Ticks, string(o.Data)}] = o
		}
		got := byStation[st]
		var prev *simnet.Observation
		for k, f := range got {
			o := byTicks[key{f.Ticks, string(f.Data)}]
			if o == nil {
				var near []string
				for _, x := range obs[i].obs {
					if bytes.Equal(x.Data, f.Data) {
						near = append(near, fmt.Sprintf("%d/e%d/%v", x.Ticks, x.Epoch, x.Class))
					}
				}
				t.Fatalf("rx %d (%s) frame %d: no truth observation with ticks %d (epoch %d class %v) and this payload; same payload: %v", i, tr.Device, k, f.Ticks, f.Epoch, f.ClockCls, near)
			}
			if prev != nil && (f.Epoch != got[k-1].Epoch) != (o.Epoch != prev.Epoch) {
				t.Fatalf("rx %d frame %d: epoch change mismatch", i, k)
			}
			prev = o
		}
		total += len(got)
	}
	s := env.srv.Stats()
	if uint64(total) != r.expected || s.DroppedBad+s.DroppedDup+s.DroppedRate != 0 {
		t.Fatalf("%d frames delivered, %d sent; %+v", total, r.expected, s)
	}
	// the server asked every client for the aircraft it reported
	for _, l := range env.srv.Links() {
		if l.Seen == 0 || l.Requested == 0 {
			t.Fatalf("link %+v saw nothing", l)
		}
	}
	t.Logf("%d receivers, %d frames, %d syncs, %d mlat, %d epochs", nrx, total, s.Syncs, s.MLATs, s.Epochs)
}

func evs(fs *fakeSink, st model.StationIdx) []string {
	_, e := fs.snapshot()
	var out []string
	for _, x := range e {
		if x.st == st {
			out = append(out, fmt.Sprintf("%d:%s", x.ev.NewEpoch, x.ev.Reason))
		}
	}
	return out
}

// loopbackPolicy exempts the test clients (all on 127.0.0.1) from the per-address limits.
var loopbackPolicy = Policy{AllowCIDRs: []string{"127.0.0.0/8"}}
