// Command ssrsim runs the bistatic SSR self-location simulator
// (airview/internal/mlat/ssr/ssrsim) for a set of scenarios and prints the
// feasibility table of docs/aggregator/SSR-RESEARCH.md.
//
//	go run ./internal/mlat/ssr/cmd/ssrsim -seeds 5 -scenario all
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
	"airview/internal/mlat/ssr/ssrsim"
)

type variant struct {
	name string
	desc string
	mod  func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint)
}

func variants() []variant {
	return []variant{
		{"ideal", "no noise, no biases, sites configured", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.Ideal = true
			setSites(sc, cfg)
		}},
		{"baseline-sites", "Beast-class rx, 3 Mode A/C SSRs configured, 10 min", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			setSites(sc, cfg)
		}},
		{"baseline", "as above, interrogators learned from the data", nil},
		{"rtlsdr", "learned, RTL-SDR-class ToA (150 ns)", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.RxJitterACs, sc.RxJitterSs = 150, 100
			cfg.TimingSigmaACNs = 180
		}},
		{"latency", "learned, 40% of airframes with 0-0.6 s ADS-B position latency", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.LatencyFrac, sc.LatencyMaxS = 0.4, 0.6
		}},
		{"30min", "learned, 30 minutes", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.DurS = 1800
		}},
		{"3min", "learned, 3 minutes", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.DurS = 180
		}},
		{"one-radar", "learned, only the 140 km SSR", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.Radars = sc.Radars[1:2]
		}},
		{"light-traffic", "learned, 40 aircraft", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.Aircraft = 40
		}},
		{"const-prf", "learned, no PRF stagger", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			for i := range sc.Radars {
				sc.Radars[i].Stagger = nil
			}
		}},
		{"modes-lockout", "learned, Mode S SSRs only: all-call 60 Hz, 90% locked out, no Mode A/C", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			for i := range sc.Radars {
				sc.Radars[i].Modes = []ssrsim.Mode{ssrsim.ModeSAllCall}
				sc.Radars[i].PRFHz = 60
				sc.Radars[i].LockoutFrac = 0.9
				sc.Radars[i].IID = uint8(i + 1)
			}
		}},
		{"modes-30min", "as modes-lockout, 30 minutes", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			for i := range sc.Radars {
				sc.Radars[i].Modes = []ssrsim.Mode{ssrsim.ModeSAllCall}
				sc.Radars[i].PRFHz = 60
				sc.Radars[i].LockoutFrac = 0.9
				sc.Radars[i].IID = uint8(i + 1)
			}
			sc.DurS = 1800
		}},
		{"modes-nolockout", "learned, Mode S SSRs only: all-call 100 Hz, no lockout, no Mode A/C", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			for i := range sc.Radars {
				sc.Radars[i].Modes = []ssrsim.Mode{ssrsim.ModeSAllCall}
				sc.Radars[i].PRFHz = 100
				sc.Radars[i].IID = uint8(i + 1)
			}
		}},
		{"sites-1km-off", "sites configured 1 km wrong (claimed 50 m)", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			setSites(sc, cfg)
			for i := range cfg.Sites {
				p := ssrsim.Offset(geo.LLH{Lat: cfg.Sites[i].Lat, Lon: cfg.Sites[i].Lon}, float64(70+130*i), 1000, cfg.Sites[i].HeightM)
				cfg.Sites[i].Lat, cfg.Sites[i].Lon = p.Lat, p.Lon
			}
		}},
		{"radar-height-300m", "sites configured, true heights 300 m above the assumed", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			setSites(sc, cfg)
			for i := range sc.Radars {
				sc.Radars[i].Pos.H += 300
			}
		}},
		{"heavy-fruit", "learned, 3000 FRUIT/s", func(sc *ssrsim.Scenario, cfg *ssr.Config, h *ssr.Hint) {
			sc.FruitRate = 3000
		}},
	}
}

func setSites(sc *ssrsim.Scenario, cfg *ssr.Config) {
	cfg.Sites = nil
	for _, r := range sc.Radars {
		cfg.Sites = append(cfg.Sites, ssr.Site{Name: r.Name, Lat: r.Pos.Lat, Lon: r.Pos.Lon, HeightM: r.Pos.H})
	}
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Round(p * float64(len(s)-1)))
	return s[i]
}

func main() {
	seeds := flag.Int("seeds", 5, "seeds per scenario")
	which := flag.String("scenario", "all", "comma-separated scenario names or all")
	verbose := flag.Bool("v", false, "print every run")
	flag.Parse()
	want := map[string]bool{}
	for _, w := range strings.Split(*which, ",") {
		want[strings.TrimSpace(w)] = true
	}
	fmt.Println("| scenario | OK runs | median err_h m | max err_h m | median σ_h m | median err_h/σ_h | radar err m (median) | obs used | rms ns | s/run |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
	for _, v := range variants() {
		if !want["all"] && !want[v.name] {
			continue
		}
		var errs, sig, ratio, rerr, used, rms []float64
		ok := 0
		var took time.Duration
		for s := 0; s < *seeds; s++ {
			sc := ssrsim.Baseline(int64(1000 + s))
			cfg := ssr.DefaultConfig()
			cfg.Enabled = true
			h := ssr.Hint{}
			if v.mod != nil {
				v.mod(&sc, &cfg, &h)
			}
			t0 := time.Now()
			o := ssrsim.Evaluate(sc, cfg, h)
			took += time.Since(t0)
			if *verbose {
				fmt.Fprintf(os.Stderr, "%s seed %d: ok=%v reason=%q err_h=%.1f err_u=%.1f σ_h=%.1f used=%d/%d ac=%d radars=%d vf=%.2f rms=%.1fns radarErr=%v est=%+v sim=%+v\n",
					v.name, s, o.Result.OK, o.Result.Reason, o.ErrHM, o.ErrUM, o.Result.SigmaHM, o.Result.NUsed, o.Result.NObs,
					o.Result.NAircraft, o.Result.NRadars, o.Result.VarFactor, o.Result.RMSNs, fmtR(o.RadarErrM), o.Est, o.Sim)
			}
			if *verbose {
				for _, ri := range o.Result.Radars {
					best := math.Inf(1)
					for _, tp := range o.Truth.Radars {
						best = math.Min(best, tp.Dist(ri.ECEF))
					}
					fmt.Fprintf(os.Stderr, "    radar lines=%d obs=%d spread=%.0f σh=%.0f dTrue=%.0f\n", ri.Lines, ri.Obs, ri.SpreadDeg, ri.SigmaHM(), best)
				}
			}
			if o.Result.OK {
				ok++
			}
			if !math.IsNaN(o.ErrHM) && o.Result.SigmaHM > 0 {
				errs = append(errs, o.ErrHM)
				sig = append(sig, o.Result.SigmaHM)
				ratio = append(ratio, o.ErrHM/o.Result.SigmaHM)
				used = append(used, float64(o.Result.NUsed))
				rms = append(rms, o.Result.RMSNs)
			}
			for _, r := range o.RadarErrM {
				if !math.IsNaN(r) {
					rerr = append(rerr, r)
				}
			}
		}
		fmt.Printf("| %s | %d/%d | %.0f | %.0f | %.0f | %.2f | %.0f | %.0f | %.0f | %.1f |\n", v.name, ok, *seeds,
			pct(errs, 0.5), pct(errs, 1), pct(sig, 0.5), pct(ratio, 0.5), pct(rerr, 0.5), pct(used, 0.5), pct(rms, 0.5),
			took.Seconds()/float64(*seeds))
	}
}

func fmtR(r []float64) string {
	var s []string
	for _, x := range r {
		s = append(s, fmt.Sprintf("%.0f", x))
	}
	return "[" + strings.Join(s, " ") + "]"
}
