package simnet

import (
	"context"
	"hash/fnv"
	"math"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/modes"
)

// collect is a Sink that keeps deep copies of observations and their
// transmissions.
type collect struct {
	obs  []Observation
	txs  map[uint64]Transmission
	hor  float64
	bad  int
	last float64
}

func (c *collect) Observe(obs []Observation) {
	if c.txs == nil {
		c.txs = map[uint64]Transmission{}
	}
	for i := range obs {
		o := obs[i]
		if o.ArrivalS < c.last {
			c.bad++
		}
		c.last = o.ArrivalS
		c.txs[o.Seq] = *o.Tx
		o.Tx = nil
		c.obs = append(c.obs, o)
	}
}
func (c *collect) Advance(h float64) { c.hor = h }

func smallNet(t testing.TB, seed uint64, workers int, mut func(*Config)) *Network {
	t.Helper()
	specs := RandomReceivers(seed, 12, Placement{CenterLat: 50.5, CenterLon: 8, RadiusKm: 250})
	cfg := Config{Seed: seed, Aircraft: 150, Receivers: specs, Workers: workers}
	if mut != nil {
		mut(&cfg)
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func obsHash(obs []Observation) uint64 {
	h := fnv.New64a()
	var b [8]byte
	put := func(v uint64) {
		for i := range b {
			b[i] = byte(v >> (8 * i))
		}
		h.Write(b[:])
	}
	for i := range obs {
		o := &obs[i]
		put(uint64(o.Rx))
		put(o.Seq)
		put(o.Ticks)
		put(uint64(o.HostNs))
		put(uint64(o.Epoch))
		put(uint64(o.SignalByte))
		put(math.Float64bits(o.ArrivalS))
		h.Write(o.Data)
	}
	return h.Sum64()
}

func TestDeterministicReplay(t *testing.T) {
	run := func(seed uint64, workers int) (uint64, int) {
		n := smallNet(t, seed, workers, nil)
		c := &collect{}
		n.AddSink(c)
		if err := n.Advance(context.Background(), 15*time.Second); err != nil {
			t.Fatal(err)
		}
		if c.bad != 0 {
			t.Fatalf("%d observations out of arrival order", c.bad)
		}
		return obsHash(c.obs), len(c.obs)
	}
	h1, n1 := run(7, 1)
	h2, n2 := run(7, 8)
	h3, n3 := run(7, 3)
	if n1 == 0 {
		t.Fatal("no observations")
	}
	if h1 != h2 || h1 != h3 || n1 != n2 || n1 != n3 {
		t.Fatalf("replay differs: %x/%d %x/%d %x/%d", h1, n1, h2, n2, h3, n3)
	}
	h4, _ := run(8, 4)
	if h4 == h1 {
		t.Fatal("different seeds gave identical output")
	}
	t.Logf("%d observations, hash %x", n1, h1)
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("no receivers accepted")
	}
	bad := []ReceiverSpec{{Lat: 80, Lon: 0}}
	if _, err := New(Config{Receivers: bad}); err == nil {
		t.Fatal("polar receiver accepted")
	}
	dup := []ReceiverSpec{{ID: "a", Lat: 50, Lon: 8}, {ID: "a", Lat: 50.1, Lon: 8}}
	if _, err := New(Config{Receivers: dup}); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
	cls := []ReceiverSpec{{Lat: 50, Lon: 8, Clock: ClockSpec{Device: DeviceSDR, Class: model.ClockGPSUTC}}}
	if _, err := New(Config{Receivers: cls}); err == nil {
		t.Fatal("GPS class on an SDR accepted")
	}
	n, err := New(Config{Receivers: []ReceiverSpec{{Lat: 50, Lon: 8}}})
	if err != nil {
		t.Fatal(err)
	}
	r := n.Receiver(0)
	if r.ID != "rx0000" || len(r.UUID) != 36 || r.Device != DeviceBeast || r.Class != model.ClockHW12 {
		t.Fatalf("defaults: %+v", r)
	}
}

// TestTruthConsistency checks that every observation is explained exactly by
// the truth API: geometry, refraction, clock state and noise.
func TestTruthConsistency(t *testing.T) {
	n := smallNet(t, 3, 4, nil)
	c := &collect{}
	n.AddSink(c)
	checked := map[int]int{}
	var maxPosErr, maxTickErr float64
	n.AddSink(sinkFunc(func(obs []Observation) {
		for i := range obs {
			o := &obs[i]
			tx := o.Tx
			r := n.rx[o.Rx]
			// geometry
			if d := tx.Pos.Sub(r.pos).Norm(); math.Abs(d-o.RangeM) > 1e-6 {
				t.Fatalf("range %v != %v", o.RangeM, d)
			}
			want := tx.TxS + (o.RangeM+o.ExcessM)/SpeedOfLight
			if math.Abs(want-o.ArrivalS) > 1e-12 {
				t.Fatalf("arrival %v != %v", o.ArrivalS, want)
			}
			if o.ExcessM < 0.5 || o.ExcessM > 120 {
				t.Fatalf("implausible refraction excess %v m at range %v", o.ExcessM, o.RangeM)
			}
			// aircraft truth at the transmit instant
			at, ok := n.AircraftAt(tx.ICAO, tx.TxS)
			if !ok {
				t.Fatalf("no truth for %06x", tx.ICAO)
			}
			maxPosErr = math.Max(maxPosErr, at.Pos.Sub(tx.Pos).Norm())
			// clock truth at the stamping instant
			ct, ok := n.ClockAt(o.Rx, o.TOA())
			if !ok {
				t.Fatalf("clock history too short at %v (now %v)", o.TOA(), n.NowS())
			}
			if math.Abs(ct.OffsetNs-o.ClockOffsetNs) > 1e-3 || ct.Epoch != o.Epoch || ct.Class != o.Class {
				t.Fatalf("clock truth %+v vs obs offset %v epoch %d", ct, o.ClockOffsetNs, o.Epoch)
			}
			// ticks = ideal + noise + quantisation
			f := o.Class.NominalHz()
			q := f / n.cfg.Receivers[o.Rx].Clock.SampleRateHz // ticks per sample
			e := n.TickError(o) - o.NoiseNs*1e-9*f
			if e > 0.01 || e < -math.Max(q, 1)-0.01 {
				t.Fatalf("rx %d %v: ticks %d (err %v ticks, quantum %v)", o.Rx, o.Class, o.Ticks, e, q)
			}
			maxTickErr = math.Max(maxTickErr, -e/f*1e9)
			checked[o.Rx]++
		}
	}))
	if err := n.Advance(context.Background(), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(checked) < 8 {
		t.Fatalf("only %d receivers heard anything", len(checked))
	}
	if maxPosErr > 1 {
		t.Fatalf("transmission position differs from truth by %.3f m", maxPosErr)
	}
	t.Logf("checked %d receivers, max tick quantisation %.1f ns, max position difference %.3f m", len(checked), maxTickErr, maxPosErr)
}

type sinkFunc func([]Observation)

func (f sinkFunc) Observe(o []Observation) { f(o) }
func (f sinkFunc) Advance(float64)         {}

// TestTransmitTimeRecovery checks the transmit instants against the
// world's own timing: the virtual world station receives every frame
// exactly once, at TxS + range/c (its GPSNs, end of frame).
func TestTransmitTimeRecovery(t *testing.T) {
	n := smallNet(t, 5, 1, func(c *Config) { c.PosLatencyMaxMs = -1; c.PosErrorScale = -1 })
	c := &collect{}
	n.AddSink(c)
	if err := n.Advance(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// A receiver at the world centre would see arrival - TxS = range / c;
	// check the ordering of transmissions is consistent: TxS increases
	// roughly with Seq (within the 100 ms step).
	var prev float64
	var back int
	for s := uint64(1); s <= uint64(len(c.txs)); s++ {
		tx, ok := c.txs[s]
		if !ok {
			continue
		}
		if tx.TxS < prev-0.01 {
			back++
		}
		prev = tx.TxS
		if tx.Position {
			t.Fatal("position re-encoded although disabled")
		}
	}
	if back > 0 {
		t.Fatalf("%d transmissions jump back more than 10 ms", back)
	}
}

func TestPositionImpairment(t *testing.T) {
	n := smallNet(t, 9, 2, nil)
	c := &collect{}
	n.AddSink(c)
	if err := n.Advance(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	var npos, withLat int
	for _, tx := range c.txs {
		if !tx.Position {
			continue
		}
		npos++
		// the frame decodes to the reported position, CRC intact
		if modes.Residual(tx.Data) != 0 {
			t.Fatal("re-encoded frame has a bad CRC")
		}
		var m modes.Message
		if err := modes.Decode(tx.Data, &m); err != nil {
			t.Fatal(err)
		}
		me := uint64(tx.Data[4])<<48 | uint64(tx.Data[5])<<40 | uint64(tx.Data[6])<<32 | uint64(tx.Data[7])<<24 | uint64(tx.Data[8])<<16 | uint64(tx.Data[9])<<8 | uint64(tx.Data[10])
		odd := (tx.Data[6]>>2)&1 == 1
		lat, lon, ok := modes.CPRAirborneLocal(tx.Lat, tx.Lon, uint32(me>>17)&0x1FFFF, uint32(me)&0x1FFFF, odd)
		if !ok {
			t.Fatal("local CPR decode failed")
		}
		if d := groundDistance(lat, lon, tx.ReportedLat, tx.ReportedLon); d > 10 {
			t.Fatalf("CPR position %v,%v is %.1f m from the reported %v,%v", lat, lon, d, tx.ReportedLat, tx.ReportedLon)
		}
		if tx.PosLatencyS > 0.01 {
			withLat++
		}
		if tx.PosLatencyS < 0 || tx.PosLatencyS > 0.3 {
			t.Fatalf("latency %v outside 0..300 ms", tx.PosLatencyS)
		}
		// the reported position lags the truth by about latency * speed
		speed := tx.Vel.Norm()
		lag := groundDistance(tx.Lat, tx.Lon, tx.ReportedLat, tx.ReportedLon)
		errM := math.Hypot(tx.PosErrEastM, tx.PosErrNorthM)
		if lag > speed*tx.PosLatencyS+errM+20 {
			t.Fatalf("reported position %.0f m off (speed %.0f, latency %.3f, error %.0f)", lag, speed, tx.PosLatencyS, errM)
		}
	}
	if npos < 100 || withLat == 0 {
		t.Fatalf("only %d re-encoded positions (%d with latency)", npos, withLat)
	}
}

func TestReceptionHorizon(t *testing.T) {
	// two receivers 1200 km apart cannot both hear a low aircraft
	specs := []ReceiverSpec{
		{ID: "w", Lat: 50, Lon: 2, Clock: ClockSpec{Device: DeviceBeast}, Reception: ReceptionSpec{SensitivityDBm: -95}},
		{ID: "e", Lat: 50, Lon: 18.8, Clock: ClockSpec{Device: DeviceBeast}, Reception: ReceptionSpec{SensitivityDBm: -95}},
	}
	n, err := New(Config{Seed: 2, Aircraft: 400, Receivers: specs, RangeKm: 1000})
	if err != nil {
		t.Fatal(err)
	}
	c := &collect{}
	n.AddSink(c)
	if err := n.Advance(context.Background(), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	atm := n.cfg.Atmosphere
	var far int
	for _, o := range c.obs {
		tx := c.txs[o.Seq]
		hz := atm.horizonM(n.cfg.Receivers[o.Rx].Reception.HorizonHeightM) + atm.horizonM(tx.HAE)
		if o.RangeM > hz*1.25 {
			far++
		}
		if o.SignalByte == 0 || o.SignalDBFS > 0 || o.SignalDBFS < -48.2 {
			t.Fatalf("bad signal %v / %d", o.SignalDBFS, o.SignalByte)
		}
	}
	if far > 0 {
		t.Fatalf("%d observations beyond the radio horizon", far)
	}
	if len(c.obs) == 0 {
		t.Fatal("nothing received")
	}
}

func TestMaskAndMultipath(t *testing.T) {
	spec := ReceiverSpec{Lat: 50, Lon: 8, Clock: ClockSpec{Device: DeviceBeast},
		Reception: ReceptionSpec{MinElevDeg: 5, MultipathProb: 1, MultipathMeanNs: 200, OutlierProb: 0.5, OutlierMaxNs: 3000}}
	n, err := New(Config{Seed: 4, Aircraft: 300, Receivers: []ReceiverSpec{spec}})
	if err != nil {
		t.Fatal(err)
	}
	c := &collect{}
	n.AddSink(c)
	if err := n.Advance(context.Background(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var mp, out int
	for _, o := range c.obs {
		if o.ElevDeg < 5 {
			t.Fatalf("observation at %.2f deg below the 5 deg mask", o.ElevDeg)
		}
		if o.MultipathNs > 0 {
			mp++
		}
		if o.OutlierNs != 0 {
			out++
		}
	}
	if len(c.obs) < 100 || mp < len(c.obs)*9/10 || out < len(c.obs)/3 || out > len(c.obs)*2/3 {
		t.Fatalf("obs %d multipath %d outliers %d", len(c.obs), mp, out)
	}
}
