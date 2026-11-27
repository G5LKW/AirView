package analytics

import (
	"context"
	"fmt"
	"math"
	"time"

	"airview/internal/astro"
	"airview/internal/stat"
)

// PhaseStats are the mean hourly metrics of one sun phase or hour of day.
type PhaseStats struct {
	Phase     string `json:"phase,omitempty"` // "day" | "twilight" | "night"
	Hour      int    `json:"hour"`            // local hour of day (ByHour only)
	Hours     int    `json:"hours"`           // hours of data
	P95Km     Num    `json:"p95Km"`
	MaxKm     Num    `json:"maxKm"`
	ResidDB   Num    `json:"residDb"`
	Aircraft  Num    `json:"aircraft"`
	Positions Num    `json:"positions"`
	// P95NormKm is the 95th percentile range with the traffic-volume effect
	// removed (what the range would be at the average traffic level).
	P95NormKm Num `json:"p95NormKm"`
}

// TrafficFit is the dependence of the hourly range on traffic volume:
// p95_km = a + SlopeKm * ln(1 + aircraft).
type TrafficFit struct {
	N       int `json:"n"`
	SlopeKm Num `json:"slopeKm"` // km per e-fold of traffic
	R2      Num `json:"r2"`
	// KmPerDoubling is the range gained when traffic doubles.
	KmPerDoubling Num `json:"kmPerDoubling"`
}

// Contrast is a two-sample comparison (Welch t-test) of the
// traffic-normalised range.
type Contrast struct {
	NA          int  `json:"nA"`
	NB          int  `json:"nB"`
	DiffKm      Num  `json:"diffKm"`    // mean(B) - mean(A), traffic-normalised
	RawDiffKm   Num  `json:"rawDiffKm"` // without normalisation
	DiffResidDB Num  `json:"diffResidDb"`
	T           Num  `json:"t"`
	P           Num  `json:"p"`
	Significant bool `json:"significant"` // p < 0.01 with at least 24 hours on each side
}

// DayNight is the result of Analyzer.DayNight.
type DayNight struct {
	From         int64        `json:"from"`
	To           int64        `json:"to"`
	Ant          string       `json:"ant"`
	Located      bool         `json:"located"` // sun phases need the receiver position
	UTCOffsetSec int          `json:"utcOffsetSec"`
	Hours        int          `json:"hours"`
	Phases       []PhaseStats `json:"phases"`
	ByHour       []PhaseStats `json:"byHour"`
	TrafficFit   *TrafficFit  `json:"trafficFit,omitempty"`
	// NightVsDay compares night against day (positive = better at night).
	NightVsDay  *Contrast `json:"nightVsDay,omitempty"`
	Conclusions []string  `json:"conclusions"`
}

type phaseAcc struct {
	n                                int
	p95, maxKm, resid, ac, pos, norm stat.Accumulator
	normVals, rawVals, residVals     []float64
}

func (p *phaseAcc) stats() PhaseStats {
	m := func(a *stat.Accumulator) Num {
		if a.N == 0 {
			return Num(math.NaN())
		}
		return Num(a.Mean())
	}
	return PhaseStats{Hours: p.n, P95Km: m(&p.p95), MaxKm: m(&p.maxKm), ResidDB: m(&p.resid), Aircraft: m(&p.ac),
		Positions: m(&p.pos), P95NormKm: m(&p.norm)}
}

// DayNight compares range, signal and traffic by day / twilight / night
// (sun elevation at the receiver) and by local hour of day, with a
// traffic-normalised range so that the effect is not just traffic volume.
// Default range: the last 30 days.
func (a *Analyzer) DayNight(ctx context.Context, r Range, ant string) (*DayNight, error) {
	now, off := a.clock()
	from, to := r.resolve(now.Unix(), 30*86400, 3600)
	cfg := a.cfg()
	return cached(ctx, a, a.key("daynight", ant, cfg, from, to, off), ttlWeather, func(ctx context.Context) (*DayNight, error) {
		rows, err := a.RangeHours(ctx, ant, from, to-1)
		if err != nil {
			return nil, err
		}
		s := siteOf(cfg, ant)
		res := &DayNight{From: from * 1000, To: to * 1000, Ant: ant, Located: s.located, UTCOffsetSec: int(off),
			Phases: []PhaseStats{}, Conclusions: []string{}}

		// Traffic normalisation.
		x := make([]float64, 0, len(rows))
		y := make([]float64, 0, len(rows))
		for i := range rows {
			if isNum(rows[i].P95Km) && rows[i].Positions > 0 {
				x = append(x, math.Log1p(float64(rows[i].Aircraft)))
				y = append(y, rows[i].P95Km)
			}
		}
		res.Hours = len(y)
		slope, xMean := 0.0, stat.Mean(x)
		if fit, err := stat.LinearRegression(x, y); err == nil && fit.N >= 12 {
			slope = fit.Slope
			res.TrafficFit = &TrafficFit{N: fit.N, SlopeKm: Num(fit.Slope), R2: Num(fit.R2), KmPerDoubling: Num(fit.Slope * math.Ln2)}
		}

		var phases [3]phaseAcc // day, twilight, night
		var hours [24]phaseAcc
		for i := range rows {
			row := &rows[i]
			if !isNum(row.P95Km) || row.Positions <= 0 {
				continue
			}
			norm := row.P95Km - slope*(math.Log1p(float64(row.Aircraft))-xMean)
			accs := []*phaseAcc{&hours[stat.HourOfDay(row.T, int(off))]}
			if s.located {
				k := 1
				switch astro.PhaseAt(time.Unix(row.T+1800, 0), s.lat, s.lon) {
				case astro.Day:
					k = 0
				case astro.Night:
					k = 2
				}
				accs = append(accs, &phases[k])
			}
			for _, p := range accs {
				p.n++
				p.p95.Add(row.P95Km)
				p.maxKm.Add(row.MaxKm)
				p.resid.Add(row.ResidDB)
				p.ac.Add(float64(row.Aircraft))
				p.pos.Add(float64(row.Positions))
				p.norm.Add(norm)
				p.normVals = append(p.normVals, norm)
				p.rawVals = append(p.rawVals, row.P95Km)
				if isNum(row.ResidDB) {
					p.residVals = append(p.residVals, row.ResidDB)
				}
			}
		}
		res.ByHour = make([]PhaseStats, 24)
		for h := range res.ByHour {
			res.ByHour[h] = hours[h].stats()
			res.ByHour[h].Hour = h
		}
		if s.located {
			for k, name := range [3]string{"day", "twilight", "night"} {
				ps := phases[k].stats()
				ps.Phase = name
				res.Phases = append(res.Phases, ps)
			}
			res.NightVsDay = welch(&phases[0], &phases[2])
		}
		res.Conclusions = dayNightConclusions(res)
		return res, nil
	})
}

// welch compares b against a.
func welch(a, b *phaseAcc) *Contrast {
	c := &Contrast{NA: a.n, NB: b.n, DiffKm: Num(math.NaN()), RawDiffKm: Num(math.NaN()), DiffResidDB: Num(math.NaN()),
		T: Num(math.NaN()), P: Num(math.NaN())}
	if a.n == 0 || b.n == 0 {
		return c
	}
	c.DiffKm = Num(stat.Mean(b.normVals) - stat.Mean(a.normVals))
	c.RawDiffKm = Num(stat.Mean(b.rawVals) - stat.Mean(a.rawVals))
	if len(a.residVals) > 0 && len(b.residVals) > 0 {
		c.DiffResidDB = Num(stat.Mean(b.residVals) - stat.Mean(a.residVals))
	}
	if a.n < 3 || b.n < 3 {
		return c
	}
	va, vb := stat.Variance(a.normVals)/float64(a.n), stat.Variance(b.normVals)/float64(b.n)
	if !(va+vb > 0) {
		return c
	}
	t := float64(c.DiffKm) / math.Sqrt(va+vb)
	df := (va + vb) * (va + vb) / (va*va/float64(a.n-1) + vb*vb/float64(b.n-1))
	c.T, c.P = Num(t), Num(stat.StudentTTwoSidedP(t, df))
	c.Significant = float64(c.P) < 0.01 && a.n >= 24 && b.n >= 24
	return c
}

func dayNightConclusions(d *DayNight) []string {
	var out []string
	if d.Hours < 48 {
		return []string{fmt.Sprintf("Not enough data yet: %d hours with range statistics (at least 48 are needed for a day/night comparison).", d.Hours)}
	}
	if f := d.TrafficFit; f != nil {
		out = append(out, fmt.Sprintf("Traffic volume explains %.0f %% of the hour-to-hour variation in range (%+.0f km per doubling of the aircraft count); the comparisons below are corrected for it.",
			100*float64(f.R2), float64(f.KmPerDoubling)))
	}
	if !d.Located {
		return append(out, "Set the receiver position to compare day, twilight and night by sun elevation; the hour-of-day table is available meanwhile.")
	}
	c := d.NightVsDay
	if c == nil || c.NA < 12 || c.NB < 12 {
		return append(out, "Not enough hours in both daylight and darkness yet for a day/night comparison.")
	}
	diff := float64(c.DiffKm)
	switch {
	case c.Significant && diff > 0:
		out = append(out, fmt.Sprintf("At equal traffic the 95th percentile range is %.0f km longer at night than by day (%d night hours vs %d day hours, p = %.3g): typical of night-time cooling and stronger refraction near the ground.",
			diff, c.NB, c.NA, float64(c.P)))
	case c.Significant:
		out = append(out, fmt.Sprintf("At equal traffic the 95th percentile range is %.0f km shorter at night than by day (%d night hours vs %d day hours, p = %.3g).",
			-diff, c.NB, c.NA, float64(c.P)))
	default:
		out = append(out, fmt.Sprintf("No reliable day/night difference in range once traffic is accounted for (%+.0f km at night, %d night vs %d day hours, p = %.2g).",
			diff, c.NB, c.NA, float64(c.P)))
	}
	if raw := float64(c.RawDiffKm); math.Abs(raw-diff) > 10 {
		out = append(out, fmt.Sprintf("Without the traffic correction the difference would read %+.0f km: most of that is simply fewer (or more) aircraft in the air at night.", raw))
	}
	out = append(out, "Hourly values are correlated from one hour to the next, so the p-values are optimistic.")
	return out
}
