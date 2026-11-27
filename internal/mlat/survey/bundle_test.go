package survey

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// bnet generates bundle inputs with a known truth: stations around a
// centre with clocks of their class (offset, drift, thermal ramp, jitter,
// tick quantisation), aircraft on straight tracks whose reported
// positions carry noise and a per-aircraft latency bias, and frames heard
// by every station in line of sight (reception probability pRx).
type bnet struct {
	seed      uint64
	stations  []bnetStation
	aircraft  int
	durS      float64
	everyS    float64 // one frame per aircraft per everyS
	posSigmaM float64
	latBiasM  float64
	pRx       float64
}

type bnetStation struct {
	eKm, nKm float64
	class    model.ClockClass
	prior    PriorKind
	// errM moves the prior position horizontally (azimuth azDeg)
	errM, azDeg float64
	jitterNs    float64 // 0 = class default
}

type bnetTruth struct {
	pos []geo.Vec3
	llh []geo.LLH
}

const bnetBaseNs = int64(1_780_000_000) * 1e9

func (n bnet) gen() (BundleInput, bnetTruth) {
	r := rand.New(rand.NewPCG(n.seed, 1234))
	ctr := geo.LLH{Lat: 51, Lon: 9}
	cf := geo.NewFrame(ctr)
	atm := geo.DefaultAtmosphere()
	var in BundleInput
	var tr bnetTruth
	type clk struct {
		off, drift, ramp, jit, q float64
		hz                       float64
		base                     uint64
	}
	clks := make([]clk, len(n.stations))
	for i, s := range n.stations {
		h := 150 + r.Float64()*300
		p := cf.FromENU(geo.Vec3{X: s.eKm * 1000, Y: s.nKm * 1000})
		l := geo.FromECEF(p)
		l.H = h
		u := l.ECEF()
		tr.pos = append(tr.pos, u)
		tr.llh = append(tr.llh, l)
		bs := BundleStation{ID: model.StationIdx(i + 2), Class: s.class, Prior: s.prior}
		if s.prior != PriorNone {
			f := geo.NewFrame(l)
			az := s.azDeg * math.Pi / 180
			bs.Pos = f.FromENU(geo.Vec3{X: s.errM * math.Sin(az), Y: s.errM * math.Cos(az), Z: 0})
		}
		if s.prior == PriorAnchor {
			bs.SigmaHM, bs.SigmaVM = 3, 5
		}
		if s.prior == PriorHint {
			bs.SigmaHM = 10e3
		}
		in.Stations = append(in.Stations, bs)
		c := clk{hz: s.class.NominalHz(), base: 1 << 40}
		switch {
		case s.class.GPS():
			c.off = r.NormFloat64() * 50e-9
			c.jit = 30e-9
			c.q = 1e-9
		case s.class == model.ClockHW12:
			c.off = r.Float64()
			c.drift = r.NormFloat64() * 1e-6
			c.jit = 50e-9
			c.q = 1 / c.hz
		default:
			c.off = r.Float64()
			c.drift = r.NormFloat64() * 60e-6
			c.ramp = r.NormFloat64() * 0.004e-6
			c.jit = 400e-9
			c.q = 1 / c.hz
		}
		if s.jitterNs > 0 {
			c.jit = s.jitterNs * 1e-9
		}
		clks[i] = c
	}
	type ac struct {
		key   uint32
		p0, v geo.Vec3 // ENU of the centre frame
		bias  float64
		phase float64
	}
	acs := make([]ac, n.aircraft)
	for i := range acs {
		az := r.Float64() * 2 * math.Pi
		d := math.Sqrt(r.Float64()) * 300e3
		hd := r.Float64() * 2 * math.Pi
		sp := 120 + r.Float64()*130
		acs[i] = ac{key: 0x400000 + uint32(i), p0: geo.Vec3{X: d * math.Sin(az), Y: d * math.Cos(az), Z: 2000 + r.Float64()*10000},
			v: geo.Vec3{X: sp * math.Sin(hd), Y: sp * math.Cos(hd)}, bias: r.NormFloat64() * n.latBiasM, phase: r.Float64()}
	}
	for t := 0.0; t < n.durS; t += n.everyS {
		for i := range acs {
			a := &acs[i]
			tt := t + a.phase*n.everyS
			p := cf.FromENU(a.p0.Add(a.v.Scale(tt)))
			r0 := int32(len(in.Recs))
			for si := range n.stations {
				u := tr.pos[si]
				fr := geo.NewFrame(tr.llh[si])
				_, el, rng := fr.AzElRange(p)
				if el < 0.3 || rng > 400e3 || r.Float64() > n.pRx {
					continue
				}
				c := &clks[si]
				arr := tt + atm.Range(u, p)/cLight
				theta := c.off + c.drift*arr + 0.5*c.ramp*arr*arr
				local := arr + theta + r.NormFloat64()*c.jit
				local = math.Round(local/c.q) * c.q
				ticks := c.base + uint64(math.Round(local*c.hz))
				in.Recs = append(in.Recs, BundleRec{St: int32(si), Epoch: 1, Ticks: ticks})
			}
			nr := int32(len(in.Recs)) - r0
			if nr < 2 {
				in.Recs = in.Recs[:r0]
				continue
			}
			vu := cf.Unrotate(a.v).Unit()
			perr := vu.Scale(a.bias).Add(geo.Vec3{X: r.NormFloat64(), Y: r.NormFloat64(), Z: r.NormFloat64()}.Scale(n.posSigmaM))
			ps := float32(n.posSigmaM*n.posSigmaM + n.latBiasM*n.latBiasM)
			in.Frames = append(in.Frames, BundleFrame{Key: a.key, TNs: bnetBaseNs + int64((tt+r.NormFloat64()*1e-3)*1e9),
				P: p.Add(perr), CovP: [6]float32{ps, 0, 0, ps, 0, ps}, R0: r0, NR: nr})
		}
	}
	return in, tr
}

func bnetErr(tr bnetTruth, i int, p geo.Vec3) (h, v float64) {
	f := geo.NewFrame(tr.llh[i])
	d := f.ToENU(p)
	return math.Hypot(d.X, d.Y), d.Z
}

// bnetMixed is the acceptance network: GPS anchors, honest declared
// stations of every class, deliberately wrong declarations (300 m to
// 12 km), unlocated stations and a hint.
func bnetMixed(seed uint64) bnet {
	n := bnet{seed: seed, aircraft: 220, durS: 1200, everyS: 8, posSigmaM: 5, latBiasM: 15, pRx: 0.85}
	add := func(e, nn float64, c model.ClockClass, k PriorKind, errM, az float64) {
		n.stations = append(n.stations, bnetStation{eKm: e, nKm: nn, class: c, prior: k, errM: errM, azDeg: az})
	}
	add(0, 0, model.ClockGPSUTC, PriorAnchor, 0, 0)
	add(60, 40, model.ClockGPSUTC, PriorAnchor, 0, 0)
	add(-70, 30, model.ClockGPSUTC, PriorAnchor, 0, 0)
	add(10, -80, model.ClockGPSUTC, PriorAnchor, 0, 0)
	add(30, 10, model.ClockHW12, PriorDeclared, 0, 0)
	add(-30, -20, model.ClockHW12, PriorDeclared, 0, 0)
	add(50, -50, model.ClockSDR12, PriorDeclared, 0, 0)
	add(-50, 70, model.ClockSDR12, PriorDeclared, 0, 0)
	add(-90, -60, model.ClockSDR12, PriorDeclared, 0, 0)
	add(80, -10, model.ClockSDR12HiRes, PriorDeclared, 0, 0)
	add(20, 60, model.ClockHW12, PriorDeclared, 2000, 45)     // wrong: 2 km
	add(-20, 40, model.ClockSDR12, PriorDeclared, 12000, 200) // wrong: 12 km
	add(-60, -30, model.ClockHW12, PriorDeclared, 300, 120)   // wrong: 300 m
	add(40, 80, model.ClockSDR12, PriorDeclared, 800, 300)    // wrong: 800 m
	add(-10, -40, model.ClockSDR12, PriorNone, 0, 0)          // unlocated
	add(70, 70, model.ClockHW12, PriorNone, 0, 0)             // unlocated
	add(-80, 10, model.ClockSDR12, PriorHint, 9000, 30)       // Locate Me hint 9 km off
	return n
}

func TestBundleSynthWrongDeclared(t *testing.T) {
	n := bnetMixed(1)
	in, tr := n.gen()
	res, err := Bundle(context.Background(), in, BundleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("frames %d/%d recs %d rej %d windows %d blocks %d iters %d released %d vf %.2f jk %d elapsed %v",
		res.FramesUsed, res.Frames, res.Receptions, res.Rejected, res.Windows, res.Blocks, res.Iterations, res.Released,
		res.VarianceFactor, res.JackknifeGroups, res.Elapsed)
	var sumBefore, sumAfter float64
	for i, s := range res.Stations {
		ns := n.stations[i]
		eh, ev := bnetErr(tr, i, s.ECEF)
		before := math.NaN()
		if ns.prior != PriorNone {
			before, _ = bnetErr(tr, i, in.Stations[i].Pos)
		}
		t.Logf("st %2d %-9s %-8v err %7.1f m (dz %+6.1f) σh %6.1f σv %6.1f | before %8.1f | jit %5.0f ns rej %4d/%5d | test %v d %.0f lim %.0f released %v outlier %v %v seeded %v",
			i, s.Prior, s.Class, eh, ev, s.SigmaHM, s.SigmaVM, before, s.JitterNs, s.Rejected, s.Receptions+s.Rejected,
			s.PriorTested, s.PriorDistHM, s.PriorLimitM, s.PriorReleased, s.Outlier, s.OutlierWhy, s.Seeded)
		if !s.Used || !s.Determined {
			t.Errorf("station %d not determined: %s", i, s.Why)
			continue
		}
		wrong := ns.errM >= 300 && ns.prior == PriorDeclared
		if wrong != s.PriorReleased {
			t.Errorf("station %d: declared error %.0f m, released %v", i, ns.errM, s.PriorReleased)
		}
		if wrong && !s.Outlier {
			t.Errorf("station %d: wrong declaration not flagged", i)
		}
		if !wrong && ns.prior != PriorNone && ns.prior != PriorHint && s.Outlier {
			t.Errorf("station %d: honest station flagged %v", i, s.OutlierWhy)
		}
		lim := 60.0
		if ns.class == model.ClockSDR12 {
			lim = 120
		}
		if eh > lim {
			t.Errorf("station %d (%v): error %.1f m > %.0f m", i, ns.class, eh, lim)
		}
		if eh > 4*s.SigmaHM+10 {
			t.Errorf("station %d: error %.1f m not covered by σ_h %.1f m", i, eh, s.SigmaHM)
		}
		if ns.prior == PriorDeclared || ns.prior == PriorHint {
			sumBefore += before
			sumAfter += eh
		}
	}
	t.Logf("declared/hint stations: mean horizontal error before %.0f m, after %.1f m", sumBefore/11, sumAfter/11)
	if sumAfter >= sumBefore/20 {
		t.Errorf("bundle does not improve the declared positions enough: %.0f → %.0f", sumBefore, sumAfter)
	}
}

func TestBundleWrongAnchorAndNoisy(t *testing.T) {
	n := bnetMixed(2)
	n.durS = 900
	n.stations[1].errM, n.stations[1].azDeg = 400, 10 // a GNSS anchor 400 m off
	n.stations[5].jitterNs = 3000                     // a hw12 station with 3 µs timing noise
	in, tr := n.gen()
	res, err := Bundle(context.Background(), in, BundleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Stations[1]
	eh, _ := bnetErr(tr, 1, a.ECEF)
	t.Logf("anchor: err %.1f m released %v why %v d %.0f lim %.0f", eh, a.PriorReleased, a.OutlierWhy, a.PriorDistHM, a.PriorLimitM)
	if !a.PriorReleased || !a.Outlier || a.OutlierWhy[0] != "anchor" || eh > 40 {
		t.Errorf("wrong anchor: released %v outlier %v %v err %.1f", a.PriorReleased, a.Outlier, a.OutlierWhy, eh)
	}
	ns := res.Stations[5]
	t.Logf("noisy: jitter %.0f ns why %v", ns.JitterNs, ns.OutlierWhy)
	if !ns.Outlier || !contains(ns.OutlierWhy, "noisy") {
		t.Errorf("noisy station not flagged: %v (%.0f ns)", ns.OutlierWhy, ns.JitterNs)
	}
	for i, s := range res.Stations {
		if i == 1 || i == 5 || n.stations[i].errM >= 300 || n.stations[i].prior == PriorNone || n.stations[i].prior == PriorHint {
			continue
		}
		if s.Outlier {
			t.Errorf("station %d flagged %v", i, s.OutlierWhy)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestBundleTwoUnlocatedOnly(t *testing.T) {
	// two GPS anchors and two unlocated stations: the unlocated pair is
	// observable (the aircraft fix the frame)
	n := bnet{seed: 5, aircraft: 200, durS: 900, everyS: 8, posSigmaM: 5, latBiasM: 15, pRx: 0.9}
	n.stations = []bnetStation{
		{eKm: 0, nKm: 0, class: model.ClockGPSUTC, prior: PriorAnchor},
		{eKm: 50, nKm: -20, class: model.ClockGPSUTC, prior: PriorAnchor},
		{eKm: -30, nKm: 40, class: model.ClockHW12},
		{eKm: 40, nKm: 50, class: model.ClockSDR12},
	}
	in, tr := n.gen()
	res, err := Bundle(context.Background(), in, BundleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 4; i++ {
		s := res.Stations[i]
		eh, ev := bnetErr(tr, i, s.ECEF)
		t.Logf("unlocated %d: err %.1f m (dz %.1f) σh %.1f seeded %v", i, eh, ev, s.SigmaHM, s.Seeded)
		if !s.Determined || eh > 100 || !s.Seeded {
			t.Errorf("unlocated %d: err %.1f determined %v seeded %v", i, eh, s.Determined, s.Seeded)
		}
	}
}

func TestBundleInputErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Bundle(ctx, BundleInput{}, BundleConfig{}); !errors.Is(err, ErrBundleTooFew) {
		t.Errorf("empty: %v", err)
	}
	in := BundleInput{Stations: make([]BundleStation, 2), Frames: []BundleFrame{{R0: 0, NR: 3}}, Recs: make([]BundleRec, 2)}
	if _, err := Bundle(ctx, in, BundleConfig{}); !errors.Is(err, ErrBundleInput) {
		t.Errorf("range: %v", err)
	}
	in = BundleInput{Stations: make([]BundleStation, 3)}
	if _, err := Bundle(ctx, in, BundleConfig{MaxStations: 2}); !errors.Is(err, ErrBundleTooMany) {
		t.Errorf("too many: %v", err)
	}
	// cancelled
	n := bnetMixed(3)
	n.durS = 200
	in2, _ := n.gen()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Bundle(cctx, in2, BundleConfig{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestBundleConfigNormalize(t *testing.T) {
	c := DefaultBundleConfig()
	if c.WindowS != 10 || c.MinBlockRecs != 6 || c.MaxStations != 64 || c.OutlierMinM != 100 || c.DeclaredSigmaHM != 30 {
		t.Errorf("defaults: %+v", c)
	}
	d := c
	d.Normalize()
	if d != c {
		t.Error("Normalize not idempotent")
	}
	c = BundleConfig{WindowS: 1e9, HuberK: 9, RejectK: 3}
	c.Normalize()
	if c.WindowS != 120 || c.RejectK < c.HuberK {
		t.Errorf("clamp: %+v", c)
	}
}

// BenchmarkBundleMax is one adjustment at MaxStations (64 stations: 16
// GPS anchors, 48 declared or unlocated SDR/hw12) over 30 minutes.
func BenchmarkBundleMax(b *testing.B) {
	r := rand.New(rand.NewPCG(9, 9))
	n := bnet{seed: 9, aircraft: 300, durS: 1800, everyS: 8, posSigmaM: 5, latBiasM: 15, pRx: 0.85}
	for i := range 64 {
		az, d := r.Float64()*2*math.Pi, 140*math.Sqrt(r.Float64())
		s := bnetStation{eKm: d * math.Sin(az), nKm: d * math.Cos(az), class: model.ClockSDR12, prior: PriorDeclared}
		switch {
		case i < 16:
			s.class, s.prior = model.ClockGPSUTC, PriorAnchor
		case i%3 == 0:
			s.class = model.ClockHW12
		case i%7 == 0:
			s.prior = PriorNone
		case i%5 == 0:
			s.errM, s.azDeg = 1500, float64(i*37)
		}
		n.stations = append(n.stations, s)
	}
	in, _ := n.gen()
	b.ResetTimer()
	for range b.N {
		res, err := Bundle(context.Background(), in, BundleConfig{})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(res.FramesUsed), "frames")
		b.ReportMetric(float64(res.Receptions), "recs")
		b.ReportMetric(float64(res.Iterations), "iters")
	}
}

func BenchmarkBundle(b *testing.B) {
	n := bnetMixed(7)
	n.durS = 600
	in, _ := n.gen()
	b.ResetTimer()
	for range b.N {
		if _, err := Bundle(context.Background(), in, BundleConfig{}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(in.Frames)), "frames")
}
