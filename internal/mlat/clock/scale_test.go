package clock

import (
	"math"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
)

// scaleRun is the result of one simnet scaling run.
type scaleRun struct {
	n, parts, synced, domains int
	medMs, p95Ms, maxMs       float64
	res                       map[string]float64
}

// runScale runs n simnet receivers (default device mix) at the density of
// TestSimnetLarge (200 receivers within 300 km) for d of simulated time with
// partitions of at most maxSt stations, and measures every epoch's cost,
// the per-class clock error against truth and the synced count.
func runScale(t *testing.T, n int, fc Config, d time.Duration) scaleRun {
	radius := 300 * math.Sqrt(float64(n)/200)
	world := radius + 350
	aircraft := int(700 * (world * world) / (650 * 650))
	specs := simnet.RandomReceivers(41, n, simnet.Placement{CenterLat: 50, CenterLon: 9, RadiusKm: radius})
	h := newSimHarness(t, simnet.Config{Seed: 41, Aircraft: aircraft, Receivers: specs},
		fc)
	h.register()
	cfg := h.f.Config()
	maxSt, workers := cfg.MaxComponentStations, cfg.Workers
	if n > maxSt {
		maxSt = cfg.PartitionStations
	}
	gps := gpsIdx(h)
	var costs []float64
	var st classStats
	type partAcc struct {
		sum, sq, nees float64
		n             int
		cmSq, wSq     float64 // per sample: partition mean², spread about it
		cmN           int
		cur           []float64
	}
	byPart := map[uint32]*partAcc{}
	tEnd := d.Seconds()
	trace := os.Getenv("AIRVIEW_CLOCK_SCALE_TRACE") != ""
	h.onEpoch = func(tE float64) {
		if tE > tEnd/2 {
			costs = append(costs, float64(h.f.Stats().LastEpochNs)/1e6)
		}
		if trace && math.Mod(tE, 30) == 0 {
			// AIRVIEW_CLOCK_SCALE_TRACE: mean GPS error per partition over
			// the whole run (convergence of the partitions' common modes)
			if cm, ok := h.commonMode(gps, tE); ok {
				sum, cnt := map[uint32]float64{}, map[uint32]int{}
				for i, r := range h.n.Receivers() {
					if e, _, sc, ok := h.errNs(i, tE); ok && sc.Synced && r.Class.GPS() {
						sum[sc.Comp] += e - cm
						cnt[sc.Comp]++
					}
				}
				ids := make([]int, 0, len(sum))
				for id := range sum {
					ids = append(ids, int(id))
				}
				sort.Ints(ids)
				line := ""
				for _, id := range ids {
					line += " " + strconv.Itoa(id) + ":" + strconv.FormatFloat(sum[uint32(id)]/float64(cnt[uint32(id)]), 'f', 1, 64)
				}
				t.Logf("trace t=%.0f s partition mean GPS error (ns):%s", tE, line)
			}
		}
		if tE < tEnd/2 || math.Mod(tE, 10) != 0 {
			return
		}
		cm, ok := h.commonMode(gps, tE)
		if !ok {
			return
		}
		for i, r := range h.n.Receivers() {
			if e, sg, sc, ok := h.errNs(i, tE); ok && sc.Synced {
				st.add(r.Class.String(), e-cm, sg)
				if r.Class.GPS() {
					pa := byPart[sc.Comp]
					if pa == nil {
						pa = &partAcc{}
						byPart[sc.Comp] = pa
					}
					pa.sum += e - cm
					pa.sq += (e - cm) * (e - cm)
					pa.nees += (e - cm) * (e - cm) / (sg * sg)
					pa.n++
					pa.cur = append(pa.cur, e-cm)
				}
			}
		}
		for _, pa := range byPart {
			if len(pa.cur) == 0 {
				continue
			}
			m := 0.0
			for _, v := range pa.cur {
				m += v
			}
			m /= float64(len(pa.cur))
			pa.cmSq += m * m
			pa.cmN++
			for _, v := range pa.cur {
				pa.wSq += (v - m) * (v - m)
			}
			pa.cur = pa.cur[:0]
		}
	}
	h.run(d)
	for _, c := range h.f.Snapshot().Components {
		if pa := byPart[c.ID]; pa != nil && pa.n > 0 {
			k := float64(pa.n)
			t.Logf("partition %d: %d stations, %d anchors: GPS error mean %+.1f ns, RMS %.1f ns, e²/σ² %.2f (n=%d); per sample: partition mean RMS %.1f ns, spread about it %.1f ns",
				c.ID, len(c.Stations), c.Anchors, pa.sum/k, math.Sqrt(pa.sq/k), pa.nees/k, pa.n,
				math.Sqrt(pa.cmSq/float64(max(pa.cmN, 1))), math.Sqrt(pa.wSq/k))
		}
		// spatial compactness: RMS distance of the members from their
		// centroid (km, flat-earth)
		var cx, cy float64
		var xs, ys []float64
		for _, st := range c.Stations {
			if i := int(st) - 2; i >= 0 && i < len(specs) {
				x := (specs[i].Lon - 9) * 111.32 * math.Cos(50*math.Pi/180)
				y := (specs[i].Lat - 50) * 111.32
				xs, ys = append(xs, x), append(ys, y)
				cx, cy = cx+x, cy+y
			}
		}
		if len(xs) > 0 {
			cx, cy = cx/float64(len(xs)), cy/float64(len(xs))
			r2 := 0.0
			for i := range xs {
				r2 += (xs[i]-cx)*(xs[i]-cx) + (ys[i]-cy)*(ys[i]-cy)
			}
			t.Logf("partition %d: centroid (%+.0f, %+.0f) km, RMS radius %.0f km", c.ID, cx, cy, math.Sqrt(r2/float64(len(xs))))
		}
	}
	out := scaleRun{n: n, res: st.report(t)}
	sort.Float64s(costs)
	if len(costs) > 0 {
		out.medMs, out.p95Ms, out.maxMs = costs[len(costs)/2], costs[len(costs)*95/100], costs[len(costs)-1]
	}
	snap := h.f.Snapshot()
	doms := map[uint32]bool{}
	for _, c := range snap.Components {
		doms[c.Domain] = true
		if len(c.Stations) > maxSt {
			t.Errorf("component %d has %d stations > %d", c.ID, len(c.Stations), maxSt)
		}
	}
	out.parts, out.domains = len(snap.Components), len(doms)
	for i := range specs {
		if sc := snap.Station(model.StationIdx(i + 2)); sc != nil && sc.Synced {
			out.synced++
		}
	}
	t.Logf("%4d receivers, %d aircraft, %d partitions in %d domains: epoch median %.1f ms, p95 %.1f ms, max %.1f ms (workers %d); synced %d/%d; %+v",
		n, aircraft, out.parts, out.domains, out.medMs, out.p95Ms, out.maxMs, workers, out.synced, n, h.f.Stats())
	return out
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// envFloat returns a positive float from the environment (0 = unset).
func envFloat(name string) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return 0
}

// envExt: AIRVIEW_CLOCK_SCALE_EXT=off switches the external states off, a
// number sets the cap.
func envExt() int {
	if os.Getenv("AIRVIEW_CLOCK_SCALE_EXT") == "off" {
		return -1
	}
	return envInt("AIRVIEW_CLOCK_SCALE_EXT", 0)
}

// envDrift: AIRVIEW_CLOCK_SCALE_DRIFT=off switches the partition
// common-mode drift off, a number sets it (ns/√s).
func envDrift() float64 {
	if os.Getenv("AIRVIEW_CLOCK_SCALE_DRIFT") == "off" {
		return -1
	}
	return envFloat("AIRVIEW_CLOCK_SCALE_DRIFT")
}

// TestSimnetScaleExplore: AIRVIEW_CLOCK_SCALE_N receivers (exploration).
func TestSimnetScaleExplore(t *testing.T) {
	n := envInt("AIRVIEW_CLOCK_SCALE_N", 0)
	if n == 0 {
		t.Skip("set AIRVIEW_CLOCK_SCALE_N")
	}
	runScale(t, n, Config{ProbationS: float64(envInt("AIRVIEW_CLOCK_SCALE_PROB", 60)), MaxComponentStations: envInt("AIRVIEW_CLOCK_SCALE_MAX", 200),
		PartitionStations: envInt("AIRVIEW_CLOCK_SCALE_PART", 0), Workers: envInt("AIRVIEW_CLOCK_SCALE_WORKERS", 1),
		ExternalStates: envExt(), ExternalInflate: envFloat("AIRVIEW_CLOCK_SCALE_INFL"), PartitionDriftNs: envDrift()},
		time.Duration(envInt("AIRVIEW_CLOCK_SCALE_S", 240))*time.Second)
}

// TestSimnetScaleCurve is the phase-5 acceptance run (DESIGN §14, §20.2):
// 200, 500 and 1,000 simnet receivers (default device mix, constant
// density) for three simulated minutes each on one core (Workers 1), with
// the default partitioning (MaxComponentStations 200, PartitionStations
// 100). It reports the epoch cost curve, per-class clock error against
// truth and the synced count, and checks that every receiver is synced,
// that the accuracy stays within the class limits and that the
// 1,000-station epoch fits the budget (median ≤ 250 ms on the development
// core: half the 0.5 s epoch, leaving room for a VPS core about 1.5×
// slower). Several minutes; run with AIRVIEW_CLOCK_SCALE=1.
func TestSimnetScaleCurve(t *testing.T) {
	if testing.Short() || os.Getenv("AIRVIEW_CLOCK_SCALE") == "" {
		t.Skip("set AIRVIEW_CLOCK_SCALE=1 (several minutes)")
	}
	sizes := []int{200, 500, 1000}
	if v := envInt("AIRVIEW_CLOCK_SCALE_N", 0); v > 0 {
		sizes = []int{v}
	}
	var runs []scaleRun
	for _, n := range sizes {
		r := runScale(t, n, Config{ProbationS: 60, Workers: 1}, 3*time.Minute)
		runs = append(runs, r)
		if r.synced < n*99/100 {
			t.Errorf("%d receivers: synced %d", n, r.synced)
		}
		if r.res["sdr12"] > 120 || r.res["hw12"] > 30 || r.res["gps_utc"] > 30 || r.res["sdr12_hires"] > 50 {
			t.Errorf("%d receivers: accuracy %v", n, r.res)
		}
		if n >= 1000 && r.medMs > 250 {
			t.Errorf("%d receivers: epoch median %.1f ms > 250 ms", n, r.medMs)
		}
	}
	for _, r := range runs {
		t.Logf("curve: %4d stations, %2d partitions: epoch median %6.1f ms p95 %6.1f ms; synced %d; RMS gps %.1f hw12 %.1f hires %.1f sdr12 %.1f sbs20 %.1f ns; e²/σ² %.2f",
			r.n, r.parts, r.medMs, r.p95Ms, r.synced, r.res["gps_utc"], r.res["hw12"], r.res["sdr12_hires"], r.res["sdr12"], r.res["sbs20"], r.res["nees"])
	}
}
