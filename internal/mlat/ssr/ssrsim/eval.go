package ssrsim

import (
	"math"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ssr"
)

// Outcome is one end-to-end run: simulation, estimator, solve, errors.
type Outcome struct {
	Result ssr.Result
	Truth  Truth
	Sim    Stats
	Est    ssr.Stats
	// ErrHM / ErrUM: horizontal / vertical error of the estimate (NaN when
	// the solve failed before producing a position).
	ErrHM, ErrUM float64
	// RadarErrM: per true radar, the horizontal distance to the nearest
	// interrogator in the result (NaN when none within 50 km).
	RadarErrM []float64
}

// Evaluate runs the scenario through an ssr.Estimator (Process once a
// second) and solves at the end.
func Evaluate(sc Scenario, cfg ssr.Config, h ssr.Hint) Outcome {
	e := ssr.New(cfg)
	next := math.Inf(-1)
	tr, st := Run(sc, func(ev Event) {
		if ev.IsPos {
			e.AddPosition(ev.ICAO, ev.Pos)
		} else {
			e.AddReply(ev.Reply)
		}
		if ev.T >= next {
			e.Process(ev.T)
			next = ev.T + 1
		}
	})
	e.Process(math.Inf(1))
	res := e.Solve(h)
	out := Outcome{Result: res, Truth: tr, Sim: st, Est: e.Stats(), ErrHM: math.NaN(), ErrUM: math.NaN()}
	if res.ECEF != (geo.Vec3{}) {
		f := geo.NewFrameECEF(tr.Receiver)
		d := f.ToENU(res.ECEF)
		out.ErrHM, out.ErrUM = math.Hypot(d.X, d.Y), d.Z
	}
	for _, rp := range tr.Radars {
		best := math.NaN()
		f := geo.NewFrameECEF(rp)
		for _, ri := range res.Radars {
			d := f.ToENU(ri.ECEF)
			if h := math.Hypot(d.X, d.Y); h < 50e3 && !(h >= best) {
				best = h
			}
		}
		out.RadarErrM = append(out.RadarErrM, best)
	}
	return out
}

// Offset returns the point at distance m and bearing az (degrees) from p,
// at height h (flat-earth local frame; fine for scenario layout).
func Offset(p geo.LLH, az, m, h float64) geo.LLH {
	f := geo.NewFrame(geo.LLH{Lat: p.Lat, Lon: p.Lon})
	a := az * math.Pi / 180
	l := geo.FromECEF(f.FromENU(geo.Vec3{X: m * math.Sin(a), Y: m * math.Cos(a)}))
	l.H = h
	return l
}

// Baseline returns the reference scenario of SSR-RESEARCH.md: a receiver
// at 52° N 1° W, 30 m HAE, three conventional (Mode A/C) SSRs at 60 km,
// 140 km and 220 km with staggered PRFs, 150 aircraft in a 300 km region,
// a Beast-class receiver, 10 minutes.
func Baseline(seed int64) Scenario {
	rx := geo.LLH{Lat: 52, Lon: -1, H: 30}
	stag := []float64{0.93, 1.05, 0.98, 1.07, 0.95, 1.02}
	return Scenario{
		Seed:          seed,
		Receiver:      rx,
		ClockOffsetS:  1234.5,
		ClockDriftPPM: 25,
		Radars: []Radar{
			{Name: "A", Pos: Offset(rx, 40, 60e3, 80), PeriodS: 4, Az0Deg: 10, PRFHz: 300, Stagger: stag},
			{Name: "B", Pos: Offset(rx, 210, 140e3, 150), PeriodS: 8, Az0Deg: 200, PRFHz: 220, Stagger: stag},
			{Name: "C", Pos: Offset(rx, 290, 220e3, 60), PeriodS: 6, Az0Deg: 99, PRFHz: 250, Stagger: stag},
		},
		Aircraft: 150,
		DurS:     600,
	}
}
