package survey

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/sim/simnet"
)

// bundleTee feeds the engine first (its reference store learns the
// frame) and then the bundler.
type bundleTee struct {
	e *mlat.Engine
	b *Bundler
}

func (t bundleTee) Observe(g *ingest.Group) {
	t.e.Observe(g)
	t.b.Observe(g)
}

func (t bundleTee) ObserveLate(g *ingest.Group) { t.e.ObserveLate(g) }

// bundleTargets is the acceptance network of the bundle on simnet: the
// peers are anchors (GNSS or confirmed, their true positions), the
// targets honest and deliberately wrong declarations of every receiver
// type, two unlocated stations and a Locate Me hint.
func bundleTargets() []target {
	return []target{
		{dev: simnet.DeviceBeast, eKm: 30, nKm: 10, declared: true, altRef: "hae"},
		{dev: simnet.DeviceSDR, eKm: -40, nKm: 25, declared: true, altRef: "hae"},
		{dev: simnet.DeviceBeast, eKm: 20, nKm: 55, declared: true, declErrM: 2000, altRef: "hae"},
		{dev: simnet.DeviceSDR, eKm: -20, nKm: -45, declared: true, declErrM: 800, altRef: "hae"},
		{dev: simnet.DeviceRadarcape12, eKm: 60, nKm: -30, declared: true, declErrM: 10000, altRef: "msl"},
		{dev: simnet.DeviceAirspy, eKm: -65, nKm: -10, declared: true, declErrM: 350, altRef: "hae"},
		{dev: simnet.DeviceRadarcapeGPS, eKm: 5, nKm: -20},
		{dev: simnet.DeviceSDR, eKm: 45, nKm: 35},
		{dev: simnet.DeviceBeast, eKm: -30, nKm: 60, hint: true},
	}
}

// TestBundleSimnet is the acceptance test of WP-SU2 (DESIGN §10.3 phase
// 4): simnet → ingest → engine (reference store) → Bundler, with
// deliberately wrong declared positions; the adjusted positions are
// compared with simnet's truth. A short run by default; BUNDLE_GATES=1
// runs the full 30-minute version.
func TestBundleSimnet(t *testing.T) {
	if testing.Short() {
		t.Skip("simnet run")
	}
	dur := 8 * time.Minute
	if os.Getenv("BUNDLE_GATES") != "" {
		dur = 30 * time.Minute
	}
	tg := bundleTargets()
	h := newSim(t, simCfg{seed: 31, gps: 6, sdr: 8, aircraft: 250, targets: tg, warmup: 3 * time.Minute, bundle: true})
	h.run(dur)
	res, err := h.bd.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("bundle: %v (stats %+v)", err, h.bd.Stats())
	}
	st := h.bd.Stats()
	t.Logf("bundler: %+v", st)
	t.Logf("bundle: stations %d frames %d/%d recs %d rej %d windows %d blocks %d iters %d released %d vf %.2f jk %d span %.0f s elapsed %v",
		len(res.Stations), res.FramesUsed, res.Frames, res.Receptions, res.Rejected, res.Windows, res.Blocks, res.Iterations,
		res.Released, res.VarianceFactor, res.JackknifeGroups, res.DurationS, res.Elapsed)
	byID := map[int]BundleStationResult{}
	for _, s := range res.Stations {
		byID[int(s.ID)] = s
	}
	var before, after float64
	nDecl := 0
	for k, tt := range tg {
		s, ok := byID[int(h.station(k))]
		if !ok {
			t.Errorf("target %d not in the bundle", k)
			continue
		}
		tr := h.n.Receiver(h.tIdx[k])
		fr := geo.NewFrame(geo.LLH{Lat: tr.Lat, Lon: tr.Lon, H: tr.HAE})
		d := fr.ToENU(s.ECEF)
		eh := math.Hypot(d.X, d.Y)
		b := math.NaN()
		if tt.declared {
			b = tt.declErrM
		}
		t.Logf("target %d %-14s %-8s err %6.1f m (dz %+6.1f) σh %5.1f σv %5.1f | declared err %6.0f | jit %4.0f ns rej %d/%d | tested %v d %.0f lim %.0f released %v outlier %v %v seeded %v",
			k, tt.dev, s.Prior, eh, d.Z, s.SigmaHM, s.SigmaVM, b, s.JitterNs, s.Rejected, s.Receptions+s.Rejected,
			s.PriorTested, s.PriorDistHM, s.PriorLimitM, s.PriorReleased, s.Outlier, s.OutlierWhy, s.Seeded)
		if !s.Determined {
			t.Errorf("target %d not determined (%s)", k, s.Why)
			continue
		}
		wrong := tt.declared && tt.declErrM >= 300
		if wrong && (!s.PriorReleased || !s.Outlier) {
			t.Errorf("target %d: declaration %.0f m off not flagged", k, tt.declErrM)
		}
		if tt.declared && !wrong && s.Outlier {
			t.Errorf("target %d: honest declaration flagged %v", k, s.OutlierWhy)
		}
		lim := 50.0
		if tt.dev == simnet.DeviceSDR {
			lim = 150
		}
		if eh > lim || eh > 4*s.SigmaHM+10 {
			t.Errorf("target %d (%s): error %.1f m, σ_h %.1f m (limit %.0f m)", k, tt.dev, eh, s.SigmaHM, lim)
		}
		if tt.declared {
			before += tt.declErrM
			after += eh
			nDecl++
		}
	}
	t.Logf("declared targets: mean horizontal error %.0f m declared → %.1f m adjusted", before/float64(nDecl), after/float64(nDecl))
	if !(after < before/10) {
		t.Errorf("no improvement: %.0f → %.0f", before, after)
	}
	// the anchors stay where they are and pass their test
	for _, s := range res.Stations {
		if s.Prior == PriorAnchor && (s.Outlier || s.PriorReleased) {
			t.Errorf("anchor %d flagged %v", s.ID, s.OutlierWhy)
		}
	}
}
