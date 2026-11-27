package engine

// Network bundle adjustment (internal/mlat/survey Bundler, DESIGN §10.3;
// survey WIRING.md §6). Beside the surveyor: it observes the same groups
// (in front of the timing quarantine), learns the stations from the same
// registry sync loop and, every aggregator.mlat.survey.bundle.everyMin,
// refines up to maxStations stations' positions and clocks together.
// Results: determined stations without an anchor get an estimate (method
// "bundle", owner/admin only); contradicted declarations are flagged
// suspect through the survey's registry adapter; noisy / rejecting
// stations raise an admin notice. Logs carry counts and σ only, never
// positions (DESIGN §8).

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat"
	"airview/internal/mlat/geo"
	"airview/internal/mlat/survey"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/registry"
)

// newBundler builds the bundler (nil when the survey or the bundle is
// disabled).
func (e *Engine) newBundler(nc *netCore, cfg model.Config, eng *mlat.Engine) (*survey.Bundler, error) {
	sc := cfg.Aggregator.MLAT.Survey
	if sc.Disabled || sc.Bundle.Disabled || nc.survReg == nil {
		return nil, nil
	}
	return survey.NewBundler(survey.BundlerOptions{
		Config:     survey.BundleConfigFromModel(sc.Bundle),
		Refs:       eng.Refs(),
		Atmosphere: eng.Atmosphere,
		Declared:   nc.survReg,
		Notifier:   &bundleNotifier{e: e, nc: nc},
		Now:        ntp.Now,
		Logger:     e.log.With("component", "mlat.bundle"),
	})
}

// bundleTee feeds the bundler and passes everything on (late copies
// included).
type bundleTee struct {
	Next ingest.MLATSink
	B    *survey.Bundler
}

func (t bundleTee) Observe(g *ingest.Group) {
	t.B.Observe(g)
	t.Next.Observe(g)
}

func (t bundleTee) ObserveLate(g *ingest.Group) {
	if lo, ok := t.Next.(ingest.LateObserver); ok {
		lo.ObserveLate(g)
	}
}

// bundleAnchor is a station's GNSS or owner-confirmed position (ECEF, HAE)
// for the bundle, nil when it has none.
func bundleAnchor(st *registry.Station) (*geo.Vec3, float64) {
	switch {
	case st.GNSS.Valid:
		p := geo.ToECEF(st.GNSS.Lat, st.GNSS.Lon, st.GNSS.Alt)
		return &p, 0
	case st.LocStatus == model.LocConfirmed && st.Located:
		p := geo.ToECEF(st.Effective.Lat, st.Effective.Lon, st.Effective.HAE)
		return &p, st.Effective.SigmaM
	}
	return nil, 0
}

// syncBundleStations pushes changed stations to the bundler and removes
// the ones that are gone.
func syncBundleStations(list []registry.Station, bd *survey.Bundler, last map[model.StationIdx]string, hints *surveyHints) {
	seen := make(map[model.StationIdx]struct{}, len(list))
	for i := range list {
		st := &list[i]
		seen[st.Idx] = struct{}{}
		in := surveyStation(st)
		if in.Declared == nil && !in.GNSS && !in.Confirmed {
			if h, ok := hints.get(st.Idx); ok {
				in.Hint = &h
			}
		}
		anchor, sigma := bundleAnchor(st)
		m := survey.MemberOf(in, anchor, sigma)
		k := fmt.Sprintf("%+v|%v|%v|%v", m, deref(m.Declared), derefLLH(m.Hint), anchor != nil)
		if last[st.Idx] == k {
			continue
		}
		bd.SetStation(st.Idx, m)
		last[st.Idx] = k
	}
	for idx := range last {
		if _, ok := seen[idx]; !ok {
			bd.RemoveStation(idx)
			delete(last, idx)
		}
	}
}

func deref(d *survey.Declared) survey.Declared {
	if d == nil {
		return survey.Declared{}
	}
	return *d
}

func derefLLH(h *geo.LLH) geo.LLH {
	if h == nil {
		return geo.LLH{}
	}
	return *h
}

// bundleNotifier stores the results (survey.BundleNotifier).
type bundleNotifier struct {
	e  *Engine
	nc *netCore
}

func (b *bundleNotifier) BundleFinished(r survey.BundleResult) {
	nc := b.nc
	used, det, outl := 0, 0, 0
	var noisy []string
	for i := range r.Stations {
		s := &r.Stations[i]
		if !s.Used {
			continue
		}
		used++
		if s.Outlier {
			outl++
			for _, w := range s.OutlierWhy {
				if w == "noisy" || w == "rejects" {
					noisy = append(noisy, fmt.Sprintf("%d (%s)", int(s.ID), strings.Join(s.OutlierWhy, ", ")))
					break
				}
			}
		}
		if !s.Determined || s.Prior == survey.PriorAnchor {
			continue
		}
		det++
		sig := func(k int) float64 { return math.Sqrt(math.Max(s.CovENU[k], 0)) }
		status := "unstable"
		switch {
		case s.Outlier:
			status = "suspect"
		case s.SigmaHM <= 50:
			status = "stable"
		}
		est := registry.Estimate{Idx: s.ID, T: ntp.Now(), Method: "bundle", Lat: s.LLH.Lat, Lon: s.LLH.Lon, HAE: s.LLH.H,
			SigmaE: sig(0), SigmaN: sig(4), SigmaU: sig(8), N: s.Receptions, Status: status}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := nc.reg.AddEstimate(ctx, est); err != nil {
				nc.log.Warn("bundle: estimate not stored", "station", int(est.Idx), "err", err)
			}
		}()
	}
	nc.log.Info("bundle adjustment finished", "stations", len(r.Stations), "used", used, "estimates", det,
		"outliers", outl, "released", r.Released, "varianceFactor", math.Round(r.VarianceFactor*100)/100,
		"elapsed", r.Elapsed.Round(time.Millisecond))
	if len(noisy) > 0 {
		b.e.Notify("warn", "Bundle adjustment: stations with noisy timing or many rejected receptions: "+strings.Join(noisy, "; "))
	}
}

// Bundler returns the network bundle adjustment (nil when off).
func (e *Engine) Bundler() *survey.Bundler {
	if e.net == nil {
		return nil
	}
	return e.net.bundler
}
