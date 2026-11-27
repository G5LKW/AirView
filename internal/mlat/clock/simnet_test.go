package clock

import (
	"context"
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// simHarness drives the clock filter from a simnet network: it groups the
// observations of each transmission, adds a reference position (truth at
// the transmit instant plus the airframe's NACp-consistent position error,
// i.e. what mlat/ref delivers after its latency correction), feeds the
// filter, steps it every epoch and compares its clocks with the truth.
type simHarness struct {
	tb      testing.TB
	n       *simnet.Network
	f       *Filter
	origin  int64
	lagS    float64
	nextE   float64
	groups  map[uint64]*simGroup
	acErr   map[uint32]geo.Vec3
	fault   map[int]func(t float64) float64 // extra receiver offset (ns) by rx
	class   map[int]model.ClockClass        // class reported to the filter
	gpsOK   map[int]func(t float64) bool    // status override for GPS receivers
	noGPS   bool                            // do not send GPS status
	bias    []float64                       // mean tick error (quantisation) by rx, ticks
	biasN   []float64
	nextGPS float64
	onEpoch func(tE float64)
	stats   struct{ groups, admitted int }
}

type simGroup struct {
	icao   uint32
	kind   model.FrameKind
	txS    float64
	pos    geo.Vec3
	sig    float64
	hostNs int64
	ms     []Member
}

func toGeo(v simnet.Vec3) geo.Vec3 { return geo.Vec3{X: v[0], Y: v[1], Z: v[2]} }

func newSimHarness(tb testing.TB, cfg simnet.Config, fc Config) *simHarness {
	n, err := simnet.New(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	fc.OriginUnixNs = n.UnixNs(0)
	if fc.Atmosphere == (geo.Atmosphere{}) {
		a := n.Config().Atmosphere
		fc.Atmosphere = geo.Atmosphere{N0: a.N0, H: a.ScaleHeightM, K: a.KAtm}
	}
	f, err := New(fc)
	if err != nil {
		tb.Fatal(err)
	}
	h := &simHarness{tb: tb, n: n, f: f, origin: fc.OriginUnixNs, lagS: 0.5, nextE: 0.5,
		groups: map[uint64]*simGroup{}, acErr: map[uint32]geo.Vec3{}, fault: map[int]func(float64) float64{},
		class: map[int]model.ClockClass{}, gpsOK: map[int]func(float64) bool{},
		bias: make([]float64, n.NumReceivers()), biasN: make([]float64, n.NumReceivers())}
	n.AddSink(h)
	return h
}

func (h *simHarness) register() {
	for i, r := range h.n.Receivers() {
		cls := r.Class
		if c, ok := h.class[i]; ok {
			cls = c
		}
		h.f.SetStation(model.StationIdx(i+2), StationInfo{Class: cls, Pos: toGeo(r.Pos), Located: true})
	}
}

// nacpSigma is a 1-sigma horizontal position error for a NACp (EPU bound
// at 95 % / 2.45).
func nacpSigma(nacp uint8) float64 {
	switch {
	case nacp >= 11:
		return 3 / 2.45
	case nacp == 10:
		return 10 / 2.45
	case nacp == 9:
		return 30 / 2.45
	case nacp == 8:
		return 92.6 / 2.45
	}
	return 185.2 / 2.45
}

// Observe implements simnet.Sink.
func (h *simHarness) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		if o.Kind != model.FrameModeSShort && o.Kind != model.FrameModeSLong {
			continue
		}
		// quantisation bias of the receiver (for the truth comparison)
		if !o.Class.GPS() || true {
			h.bias[o.Rx] += h.n.TickError(o)
			h.biasN[o.Rx]++
		}
		g := h.groups[o.Seq]
		if g == nil {
			tx := o.Tx
			if tx.NACp < 8 || len(o.Data) == 0 || !SyncDF(o.Data[0]>>3) {
				h.groups[o.Seq] = &simGroup{} // not usable; remember to skip
				continue
			}
			sig := nacpSigma(tx.NACp)
			if tx.Position {
				up := simnet.Up(tx.Lat, tx.Lon)
				fr := geo.NewFrame(geo.LLH{Lat: tx.Lat, Lon: tx.Lon})
				_ = up
				h.acErr[tx.ICAO] = fr.Unrotate(geo.Vec3{X: tx.PosErrEastM, Y: tx.PosErrNorthM})
			}
			e := h.acErr[tx.ICAO]
			g = &simGroup{icao: tx.ICAO, kind: o.Kind, txS: tx.TxS, pos: toGeo(tx.Pos).Add(e), sig: sig, hostNs: o.HostNs}
			h.groups[o.Seq] = g
		}
		if g.icao == 0 && g.sig == 0 {
			continue
		}
		ticks := o.Ticks
		if fn := h.fault[o.Rx]; fn != nil {
			d := fn(o.ArrivalS)
			if o.Class.GPS() {
				ticks = uint64(int64(ticks) + int64(math.Round(d)))
			} else {
				ticks = uint64(int64(ticks) + int64(math.Round(d*1e-9*o.Class.NominalHz())))
			}
		}
		g.ms = append(g.ms, Member{Station: model.StationIdx(o.Rx + 2), Epoch: o.Epoch, Ticks: ticks})
		if o.HostNs < g.hostNs {
			g.hostNs = o.HostNs
		}
	}
}

// Advance implements simnet.Sink.
func (h *simHarness) Advance(hz float64) {
	var ready []uint64
	for seq, g := range h.groups {
		if g.txS > hz-0.05 && g.sig != 0 {
			continue
		}
		ready = append(ready, seq)
	}
	slices.Sort(ready)
	for _, seq := range ready {
		g := h.groups[seq]
		delete(h.groups, seq)
		if g.sig == 0 || len(g.ms) < 2 {
			continue
		}
		h.stats.groups++
		v := g.sig * g.sig
		vz := 15.0 * 15
		if h.f.AddSync(&SyncObs{Key: g.icao, Kind: g.kind, HostNs: g.hostNs, Pos: g.pos,
			PosCov: [6]float64{v, 0, 0, v, 0, math.Max(v, vz)}, Members: g.ms}) {
			h.stats.admitted++
		}
	}
	// GPS status once a second
	if !h.noGPS {
		for ; h.nextGPS <= hz; h.nextGPS++ {
			for i, r := range h.n.Receivers() {
				if r.Device != simnet.DeviceRadarcapeGPS {
					continue
				}
				ct, _ := h.n.ClockAt(i, h.nextGPS)
				ok := ct.Locked
				utc := !ct.GPSTime
				if fn := h.gpsOK[i]; fn != nil {
					ok, utc = fn(h.nextGPS), true
				}
				h.f.GPS(model.StationIdx(i+2), GPSStatus{HostNs: h.n.UnixNs(h.nextGPS), Locked: ok, UTC: utc})
			}
		}
	}
	for h.nextE <= hz-h.lagS {
		h.f.Step(h.nextE)
		if h.onEpoch != nil {
			h.onEpoch(h.nextE)
		}
		h.nextE += h.f.cfg.EpochS
	}
}

func (h *simHarness) run(d time.Duration) {
	if err := h.n.Advance(context.Background(), d); err != nil {
		h.tb.Fatal(err)
	}
}

// errNs returns the filter's network time minus the true time for receiver
// rx at true time t (ns), with the filter's sigma; ok false when the
// station is not published or not in its truth epoch.
func (h *simHarness) errNs(rx int, t float64) (e, sig float64, sc *StationClock, ok bool) {
	sc = h.f.Snapshot().Station(model.StationIdx(rx + 2))
	if sc == nil || sc.Hz == 0 {
		return 0, 0, sc, false
	}
	ct, good := h.n.ClockAt(rx, t)
	if !good || ct.Epoch != sc.Epoch {
		return 0, 0, sc, false
	}
	meanBias := 0.0
	if h.biasN[rx] > 0 {
		meanBias = h.bias[rx] / h.biasN[rx]
	}
	extra := 0.0
	if fn := h.fault[rx]; fn != nil {
		extra = fn(t)
	}
	if ct.Class.GPS() && ct.Locked && h.fault[rx] == nil {
		// the per-second 1PPS discipline error is white timing noise, not
		// a clock offset the filter can follow
		extra -= ct.PPSErrNs
	}
	var y float64
	if sc.Hz == 1e9 {
		start := h.n.UnixNs(0)
		y = sc.BaseS + (float64(start-int64(sc.BaseTicks))+ct.LocalS*1e9+meanBias+extra)/1e9
	} else {
		y = (ct.Ticks+meanBias-float64(sc.BaseTicks))/sc.Hz + sc.BaseS + extra*1e-9
	}
	tn, v := sc.NetFromLocal(y, false)
	return (tn - t) * 1e9, math.Sqrt(v), sc, true
}

// commonMode returns the median error of the given receivers (the network
// time offset against true time).
func (h *simHarness) commonMode(rxs []int, t float64) (float64, bool) {
	var es []float64
	for _, rx := range rxs {
		if e, _, sc, ok := h.errNs(rx, t); ok && sc.Synced {
			es = append(es, e)
		}
	}
	if len(es) == 0 {
		return 0, false
	}
	sort.Float64s(es)
	return es[len(es)/2], true
}

// classStats accumulates clock errors per class.
type classStats struct {
	errs  map[string][]float64
	nees  []float64
	neesC map[string][]float64
}

func (c *classStats) add(cls string, e, sig float64) {
	if c.errs == nil {
		c.errs = map[string][]float64{}
	}
	c.errs[cls] = append(c.errs[cls], e)
	if sig > 0 {
		c.nees = append(c.nees, e*e/(sig*sig))
		if c.neesC == nil {
			c.neesC = map[string][]float64{}
		}
		c.neesC[cls] = append(c.neesC[cls], e*e/(sig*sig))
	}
}

func rms(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s / float64(max(len(v), 1)))
}

func p95abs(v []float64) float64 {
	a := make([]float64, len(v))
	for i, x := range v {
		a[i] = math.Abs(x)
	}
	slices.Sort(a)
	if len(a) == 0 {
		return 0
	}
	return a[int(0.95*float64(len(a)-1))]
}

func (c *classStats) report(tb testing.TB) map[string]float64 {
	out := map[string]float64{}
	keys := make([]string, 0, len(c.errs))
	for k := range c.errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := c.errs[k]
		out[k] = rms(v)
		nm := 0.0
		for _, x := range c.neesC[k] {
			nm += x
		}
		nm /= float64(max(len(c.neesC[k]), 1))
		tb.Logf("clock offset error %-12s n=%5d  RMS %7.1f ns  P95 %7.1f ns  mean e²/σ² %.2f", k, len(v), rms(v), p95abs(v), nm)
	}
	if len(c.nees) > 0 {
		m := 0.0
		for _, x := range c.nees {
			m += x
		}
		tb.Logf("normalised error (e²/σ²) mean %.2f over %d samples", m/float64(len(c.nees)), len(c.nees))
		out["nees"] = m / float64(len(c.nees))
	}
	return out
}

// mixedReceivers places nGPS Radarcape GPS receivers on a ring and nOther
// receivers of the given devices at random within radiusKm.
func mixedReceivers(seed uint64, nGPS int, others []simnet.Device, radiusKm float64) []simnet.ReceiverSpec {
	mix := map[simnet.Device]float64{}
	for _, d := range others {
		mix[d]++
	}
	var out []simnet.ReceiverSpec
	if nGPS > 0 {
		g := simnet.RandomReceivers(seed, nGPS, simnet.Placement{CenterLat: 50, CenterLon: 9, RadiusKm: radiusKm,
			Mix: map[simnet.Device]float64{simnet.DeviceRadarcapeGPS: 1}})
		out = append(out, g...)
	}
	if len(others) > 0 {
		o := simnet.RandomReceivers(seed+100, len(others), simnet.Placement{CenterLat: 50, CenterLon: 9, RadiusKm: radiusKm, Mix: mix})
		out = append(out, o...)
	}
	for i := range out {
		out[i].ID = ""
	}
	return out
}

var allClockDevices = []simnet.Device{simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceSDR, simnet.DeviceBeast,
	simnet.DeviceRadarcape12, simnet.DeviceAirspy, simnet.DeviceSBS}

// TestSimnetClockAccuracy reports the clock offset error against simnet
// truth for every class in a mixed network (8 GPS anchors + 24 others).
func TestSimnetClockAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	var others []simnet.Device
	for i := 0; i < 24; i++ {
		others = append(others, allClockDevices[i%len(allClockDevices)])
	}
	rx := mixedReceivers(7, 8, others, 150)
	h := newSimHarness(t, simnet.Config{Seed: 7, Aircraft: 250, Receivers: rx},
		Config{ProbationS: 120})
	h.register()
	gps := []int{0, 1, 2, 3, 4, 5, 6, 7}
	var st classStats
	h.onEpoch = func(tE float64) {
		if tE < 420 || math.Mod(tE, 5) != 0 {
			return
		}
		cm, ok := h.commonMode(gps, tE)
		if !ok {
			return
		}
		for i, r := range h.n.Receivers() {
			e, sg, sc, ok := h.errNs(i, tE)
			if !ok || !sc.Synced {
				continue
			}
			st.add(r.Class.String(), e-cm, sg)
		}
	}
	h.run(15 * time.Minute)
	res := st.report(t)
	snap := h.f.Snapshot()
	anchors, synced := 0, 0
	for i := range h.n.Receivers() {
		sc := snap.Station(model.StationIdx(i + 2))
		if sc == nil {
			continue
		}
		if sc.Anchor {
			anchors++
		}
		if sc.Synced {
			synced++
		}
	}
	h.dump(t)
	for i := range rx {
		if sc := snap.Station(model.StationIdx(i + 2)); sc != nil {
			for _, e := range sc.Events {
				t.Logf("rx %d event %.1f %s %s", i, e.T, e.Reason, e.Detail)
			}
		}
	}
	fs := h.f.Stats()
	t.Logf("anchors %d, synced %d/%d, groups %d admitted %d, filter %+v", anchors, synced, len(rx), h.stats.groups, h.stats.admitted, fs)
	if anchors != 8 {
		t.Errorf("anchors = %d, want 8", anchors)
	}
	if synced < len(rx)-1 {
		t.Errorf("synced = %d of %d", synced, len(rx))
	}
	limits := map[string]float64{"gps_utc": 15, "hw12": 40, "sdr12": 250, "sdr12_hires": 120, "sbs20": 250}
	for k, lim := range limits {
		if v, ok := res[k]; ok && v > lim {
			t.Errorf("%s RMS %.1f ns > %.0f ns", k, v, lim)
		}
	}
	if fs.Steps > 3 {
		t.Errorf("%d spurious steps", fs.Steps)
	}
}

func (h *simHarness) dump(tb testing.TB) {
	snap := h.f.Snapshot()
	gps := []int{}
	for i, r := range h.n.Receivers() {
		if r.Device == simnet.DeviceRadarcapeGPS {
			gps = append(gps, i)
		}
	}
	cm, _ := h.commonMode(gps, snap.T)
	for i, r := range h.n.Receivers() {
		e, sg, sc, ok := h.errNs(i, snap.T)
		if sc == nil {
			tb.Logf("rx %2d %-14s not published", i, r.Device)
			continue
		}
		ct, _ := h.n.ClockAt(i, snap.T)
		tb.Logf("rx %2d %-14s %-12s ok=%v synced=%v anchor=%-9s err=%8.1f sig=%7.1f phi=%10.1f (true %10.1f) beta=%7.1f acc=%3d q=%.2f j=%.2f out=%4.1f%% steps=%d peers=%d",
			i, r.Device, sc.Class, ok, sc.Synced, sc.AnchorState, e-cm, sg, sc.Phi, ct.FreqPPM*1000, sc.Beta, sc.Accepted30s, sc.QScale, sc.JitterScale, sc.OutlierPct, sc.Steps, len(sc.Peers))
	}
}
