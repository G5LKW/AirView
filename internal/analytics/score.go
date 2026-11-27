package analytics

import (
	"context"
	"fmt"
	"math"

	"airview/internal/stat"
)

// SubScore is one component of the station score.
type SubScore struct {
	Key       string  `json:"key"` // "range" | "uniformity" | "sensitivity" | "decode" | "uptime"
	Label     string  `json:"label"`
	Score     Num     `json:"score"` // 0..100, null when not available
	Weight    float64 `json:"weight"`
	Available bool    `json:"available"`
	Detail    string  `json:"detail"`
}

// Finding is a plain-language observation or piece of advice.
type Finding struct {
	Level string `json:"level"` // "good" | "info" | "warn"
	Code  string `json:"code"`  // stable identifier, e.g. "obstruction", "yagi-pointing"
	Text  string `json:"text"`
}

// StationScore is the result of Analyzer.Score.
type StationScore struct {
	From     int64      `json:"from"`
	To       int64      `json:"to"`
	Ant      string     `json:"ant"`
	Score    Num        `json:"score"` // 0..100, null without any data
	Grade    string     `json:"grade"` // "excellent" | "good" | "fair" | "poor" | "no data"
	Subs     []SubScore `json:"subs"`
	Findings []Finding  `json:"findings"`
}

// Score condenses the coverage, antenna and traffic analyses into a single
// 0-100 station score with sub-scores (range vs radio horizon 35 %, azimuth
// uniformity 20 %, sensitivity 20 %, decode efficiency 10 %, uptime 15 %;
// unavailable parts are left out and the weights renormalised) and
// plain-language findings. Default range: the last 30 days.
func (a *Analyzer) Score(ctx context.Context, r Range, ant string) (*StationScore, error) {
	now, _ := a.clock()
	from, to, d0, d1 := dayRange(r, now.Unix(), 30)
	cfg := a.cfg()
	// The score is shown next to the coverage and antenna-report tiles of
	// the same request window. It is therefore built from (and keyed by)
	// the very Coverage / AntennaReport results those tiles get for this
	// range: the two are fetched from their own caches first and their
	// computation stamps are part of the score key, so when either input is
	// recomputed the score is recomputed too, and the score is never older
	// than the tiles. ttlScore (= ttlCoverage) bounds the uptime / decode
	// figures read from the hourly stats.
	rr := Range{From: from, To: to}
	cov, err := a.Coverage(ctx, rr, ant)
	if err != nil {
		return nil, err
	}
	rep, err := a.Antenna(ctx, rr, ant)
	if err != nil {
		return nil, err
	}
	key := a.key("score", ant, cfg, int64(d0), int64(d1), int64(cov.gen), int64(rep.gen))
	return cached(ctx, a, key, ttlScore, func(ctx context.Context) (*StationScore, error) {
		hours, err := a.hourStats(ctx, ant, from, to)
		if err != nil {
			return nil, err
		}
		res := &StationScore{From: from * 1000, To: to * 1000, Ant: ant, Score: Num(math.NaN()), Grade: "no data", Findings: []Finding{}}
		add := func(level, code, format string, args ...any) {
			res.Findings = append(res.Findings, Finding{Level: level, Code: code, Text: fmt.Sprintf(format, args...)})
		}
		sub := func(key, label string, weight, score float64, detail string) {
			s := SubScore{Key: key, Label: label, Weight: weight, Score: Num(math.NaN()), Detail: detail}
			if isNum(score) {
				s.Score, s.Available = Num(clamp(score, 0, 100)), true
			}
			res.Subs = append(res.Subs, s)
		}

		// Range vs horizon: the 30-40 kft band when it is populated.
		rangeScore, detail := math.NaN(), "no positions yet"
		ref := &cov.Overall
		if cov.Bands[5].Bearings >= 30 {
			ref = &cov.Bands[5]
		}
		if ref.Bearings > 0 {
			rangeScore = float64(ref.MeanPct)
			detail = fmt.Sprintf("on average %.0f %% of the %.0f km radio horizon (%s ft) around the compass", rangeScore, float64(ref.HorizonKm), ref.Label)
			switch {
			case rangeScore >= 85:
				add("good", "range", "Range is excellent: %s.", detail)
			case rangeScore < 50:
				add("warn", "range", "Range is short: %s. A higher or less obstructed antenna position, lower feed-line loss or a filtered LNA at the antenna would help.", detail)
			}
		}
		sub("range", "Range vs radio horizon", 35, rangeScore, detail)

		// Azimuth uniformity.
		uni, detail := math.NaN(), "no positions yet"
		if cov.Overall.Bearings >= 10 {
			top := stat.Quantile(cov.Overall.RobustKm, 0.9)
			var s float64
			for _, v := range cov.Overall.RobustKm {
				if isNum(v) && top > 0 {
					s += math.Min(1, v/top)
				}
			}
			uni = 100 * s / brgBins
			detail = fmt.Sprintf("%d of 180 bearings covered; the average bearing reaches %.0f %% of the best directions", cov.Overall.Bearings, uni)
		}
		sub("uniformity", "Azimuth uniformity", 20, uni, detail)
		for _, o := range cov.Obstructions {
			level := "info"
			if o.Severity != "minor" {
				level = "warn"
			}
			add(level, "obstruction", "%s (%s). Terrain, buildings or trees in that direction, or simply no low traffic there.", o.Text, o.Severity)
		}
		if len(cov.Obstructions) == 0 && cov.Overall.Bearings >= 150 {
			add("good", "obstruction", "No obstructed sectors detected: low-altitude coverage is even all round.")
		}

		// Sensitivity: dynamic range of the decoded signal levels.
		sens, detail := math.NaN(), "no signal statistics yet"
		if sd := rep.Signal; sd != nil && sd.Total >= 500 {
			dr := float64(sd.RSSIP99) - float64(sd.RSSIP01)
			sens = 100 * clamp((dr-8)/22, 0, 1)
			strong := float64(sd.StrongPct)
			if strong > 2 {
				sens -= math.Min(30, 3*(strong-2))
				add("warn", "overload", "%.1f %% of the messages arrive within 3 dB of full scale: the receiver is close to overload. Reduce the gain (or add attenuation) to recover nearby aircraft and dynamic range.", strong)
			}
			detail = fmt.Sprintf("decoded signals span %.0f dB (%.0f to %.0f dBFS)", dr, float64(sd.RSSIP01), float64(sd.RSSIP99))
			if dr < 15 {
				add("info", "dynamic-range", "Decoded signal levels span only %.0f dB: weak signals are lost in noise. Check for interference, cable loss ahead of the first amplifier and the receiver gain setting.", dr)
			}
			if sd.Fit != nil && sd.Fit.Text != "" {
				add("info", "path-loss", "%s", sd.Fit.Text)
			}
		}
		sub("sensitivity", "Sensitivity", 20, sens, detail)

		// Decode efficiency and uptime.
		var frames, bad, minutes int64
		first := int64(0)
		for i := range hours {
			h := &hours[i]
			frames += h.Frames
			bad += h.CRCBad
			m := int64(h.Minutes)
			if m <= 0 {
				m = 60
			}
			minutes += m
			if first == 0 {
				first = h.T
			}
		}
		dec, detail := math.NaN(), "no frames yet"
		if frames > 0 {
			f := float64(bad) / float64(frames)
			dec = 100 * clamp(1-f/0.5, 0, 1)
			detail = fmt.Sprintf("%.1f %% of %d frames failed the CRC", 100*f, frames)
			if f > 0.2 {
				add("warn", "decode", "%.0f %% of the received frames fail the CRC check: strong interference, overload or a marginal signal chain.", 100*f)
			}
		}
		sub("decode", "Decode efficiency", 10, dec, detail)

		up, detail := math.NaN(), "no data yet"
		if first > 0 {
			// The span starts at the first minute with data, not at the start
			// of its hour (a station started at 00:45 has not been "down"
			// from 00:00 to 00:45). Without minute rows (expired) the hour
			// start is used.
			if mins, err := a.st.MinuteStats(ctx, a.antKey(ant), max(first, from), first+3599); err == nil && len(mins) > 0 {
				first = mins[0].T
			}
		}
		if first > 0 {
			end := min(to, now.Unix()+60)
			span := max((end-max(first, from))/60, 1)
			up = clamp(100*float64(minutes)/float64(span), 0, 100)
			detail = fmt.Sprintf("receiving %.1f %% of the time since the first data in the range", up)
			if up < 95 {
				add("warn", "uptime", "The receiver delivered data only %.0f %% of the time. Check the feed connection, power and network of the receiver.", up)
			}
		}
		sub("uptime", "Uptime", 15, up, detail)

		// Antenna findings.
		if y := rep.Yagi; y != nil && y.Text != "" {
			level := "info"
			if y.OK && y.Configured != nil && math.Abs(float64(y.PointingErrorDeg)) >= 8 && float64(y.Confidence) >= 0.4 {
				level = "warn"
			}
			add(level, "yagi-pointing", "%s", y.Text)
			if y.OK && y.Configured != nil && float64(y.Confidence) >= 0.4 {
				if be := float64(y.BeamwidthErrorDeg); math.Abs(be) > 0.5*y.Configured.HBeamwidthDeg {
					add("info", "yagi-beamwidth", "The measured lobe is %.0f° wide but %.0f° is configured: adjust the antenna's beamwidth setting for better predictions.", float64(y.BeamwidthDeg), y.Configured.HBeamwidthDeg)
				}
			}
		}
		for _, row := range rep.LinkBudget {
			if row.Band >= 4 && isNum(float64(row.PctOfPredicted)) && float64(row.PctOfPredicted) < 60 && row.Limit == "horizon" {
				add("info", "link-budget", "At %s ft the link budget predicts %.0f km (horizon-limited) but the best direction reaches %.0f km: obstruction or a much higher noise level than assumed.",
					row.Label, float64(row.PredictedKm), float64(row.AchievedRobustKm))
				break
			}
		}
		if !cov.Located {
			add("info", "location", "The receiver position is not set: skyline, heat-based antenna pattern and map overlays are unavailable. Enter it in the settings or try the location guesser.")
		}
		if cov.Days < 3 {
			add("info", "data", "Only %d day(s) of coverage data so far: scores and findings will firm up after a few days of traffic.", cov.Days)
		}

		var ws, ss float64
		for _, s := range res.Subs {
			if s.Available {
				ws += s.Weight
				ss += s.Weight * float64(s.Score)
			}
		}
		if ws > 0 {
			sc := ss / ws
			res.Score = Num(math.Round(sc*10) / 10)
			switch {
			case sc >= 85:
				res.Grade = "excellent"
			case sc >= 70:
				res.Grade = "good"
			case sc >= 50:
				res.Grade = "fair"
			default:
				res.Grade = "poor"
			}
		}
		return res, nil
	})
}
