package rfstat

import (
	"fmt"
	"math"
	"sort"
)

// Thresholds of the health heuristics (documented in WIRING.md).
const (
	minHealthFrames  = 1000
	strongWarn       = 0.01 // share of frames within 3 dB of full scale
	strongBad        = 0.05
	marginLowDB      = 6 // p50 - decode floor
	excessWarn       = 0.05
	excessBad        = 0.15
	blockedRatio     = 0.5 // sector Pd below this share of the median sector Pd
	gnssWarnPct      = 5
	gnssBadPct       = 20
	updateTarget     = 0.95 // P(age <= 5 s) within 150 km
	minHealthAircraf = 5
)

func status(score int) string {
	switch {
	case score < 0:
		return "unknown"
	case score >= 80:
		return "ok"
	case score >= 50:
		return "warn"
	}
	return "bad"
}

func clampScore(v float64) int {
	if !finite(v) {
		return -1
	}
	return int(math.Round(math.Max(0, math.Min(100, v))))
}

func pct(v Num) string {
	if !v.Valid() {
		return "n/a"
	}
	return fmt.Sprintf("%.1f %%", float64(v)*100)
}

// pctNum converts a 0..1 fraction to percent (NaN stays NaN).
func pctNum(v Num) Num { return v * 100 }

func healthReport(r *Report) Health {
	h := Health{AdviceItems: []AdviceItem{}}
	add := func(key, code, label string, score int, text string, data Data) {
		if data == nil {
			data = Data{}
		}
		h.Items = append(h.Items, HealthItem{Key: key, Code: code, Label: label, Score: score, Status: status(score), Text: text, Data: data})
	}
	advise := func(code, text string, data Data) {
		if data == nil {
			data = Data{}
		}
		h.Advice = append(h.Advice, text)
		h.AdviceItems = append(h.AdviceItems, AdviceItem{Code: code, Text: text, Data: data})
	}
	sig := &r.Signal

	// 1. gain
	if sig.Frames < minHealthFrames {
		add("gain", "gain.no-data", "Receiver gain", -1, "Not enough frames with signal level yet.", nil)
	} else {
		st := float64(sig.StrongFrac)
		score := 100.0
		code := "gain.ok"
		data := Data{"p50Dbfs": sig.P50, "floorDbfs": sig.DecodeFloor, "strongPct": pctNum(sig.StrongFrac), "nearDb": 3}
		text := fmt.Sprintf("Median %.1f dBFS, decode floor %.1f dBFS, %s of frames within 3 dB of full scale.",
			float64(sig.P50), float64(sig.DecodeFloor), pct(sig.StrongFrac))
		switch {
		case st > strongBad:
			score, code = 30, "gain.too-high"
			text += " Gain is too high: strong nearby aircraft overload the receiver. Reduce gain (or add attenuation) by 3-6 dB."
			data["stepMinDb"], data["stepMaxDb"] = 3, 6
			advise("advice.gain-reduce", "Reduce receiver gain by 3-6 dB: "+pct(sig.StrongFrac)+" of frames are within 3 dB of full scale.",
				Data{"stepMinDb": 3, "stepMaxDb": 6, "strongPct": pctNum(sig.StrongFrac), "nearDb": 3})
		case st > strongWarn:
			score, code = 65, "gain.high"
			text += " Gain is on the high side; consider 2-3 dB less."
			data["stepMinDb"], data["stepMaxDb"] = 2, 3
			advise("advice.gain-slightly-high", "Gain slightly high: consider reducing it by 2-3 dB.", Data{"stepMinDb": 2, "stepMaxDb": 3})
		case sig.SNRMargin.Valid() && float64(sig.SNRMargin) < marginLowDB && st < 0.001:
			score, code = 60, "gain.low"
			text += " Gain may be too low: most signals sit close to the decode floor and none approach full scale."
			advise("advice.gain-low", "Gain may be too low: increase it by 2-3 dB or check the LNA / cable losses.", Data{"stepMinDb": 2, "stepMaxDb": 3})
		}
		add("gain", code, "Receiver gain", clampScore(score), text, data)
	}

	// 2. interference
	ch := &r.Channel
	if ch.Frames < minHealthFrames {
		add("interference", "interference.no-data", "Interference / garble", -1, "Not enough frames yet.", nil)
	} else {
		ex := float64(ch.ExcessFail)
		score := 100 - math.Max(0, ex)*400
		text := fmt.Sprintf("CRC failures %s (%s rejected address/parity), overlap model predicts %s at %.0f frames/s; channel occupancy %.2f %%.",
			pct(ch.FailFrac), pct(ch.RejectedFrac), pct(ch.OverlapProb), float64(ch.FPS), float64(ch.OccupancyPct))
		data := Data{"failPct": pctNum(ch.FailFrac), "rejectedPct": pctNum(ch.RejectedFrac), "overlapPct": pctNum(ch.OverlapProb),
			"rateHz": ch.FPS, "occupancyPct": ch.OccupancyPct}
		code := "interference.ok"
		switch {
		case ex > excessBad:
			code = "interference.likely"
			text += " Far more failures than overlapping replies explain: local interference (LTE/DME/switching supplies) or an unstable receiver likely."
			advise("advice.interference", "Interference likely: CRC failure rate is well above what overlapping replies explain. Check for nearby emitters, add a 1090 MHz bandpass filter, check cabling.",
				Data{"freqMHz": 1090})
		case ex > excessWarn:
			code = "interference.elevated"
			text += " Somewhat more failures than garble explains; possible interference or FRUIT from undecodable replies."
		default:
			text += " Failure rate is consistent with normal reply overlap (FRUIT/garble)."
		}
		if ch.BurstCount > 0 {
			text += fmt.Sprintf(" %d burst second(s) detected.", ch.BurstCount)
			data["bursts"] = Num(ch.BurstCount)
		}
		add("interference", code, "Interference / garble", clampScore(score), text, data)
	}

	// 3. detection
	det := &r.Detection
	pdPos := det.Types[0].Pd
	if !pdPos.Valid() || float64(det.Types[0].Expected) < 2000 {
		add("detection", "detection.no-data", "Reception probability", -1, "Not enough ADS-B squitters yet.", nil)
	} else {
		score := float64(pdPos) * 100 / 0.8 // 80 % overall is excellent for a mixed traffic area
		text := fmt.Sprintf("Position squitters decoded: %s; velocity %s; identification %s. Detection range 90 %%: %s km, 50 %%: %s km.",
			pct(pdPos), pct(det.Types[1].Pd), pct(det.Types[2].Pd), kmStr(det.Range90Km), kmStr(det.Range50Km))
		// The code names the message (measured values); the status comes
		// from the score.
		add("detection", "detection.measured", "Reception probability", clampScore(score), text, Data{
			"posPct": pctNum(pdPos), "velPct": pctNum(det.Types[1].Pd), "identPct": pctNum(det.Types[2].Pd),
			"pd90Pct": 90, "range90Km": det.Range90Km, "pd50Pct": 50, "range50Km": det.Range50Km})
	}

	// 4. sectors
	var pds []float64
	for _, p := range det.VsBearing.Pos {
		if p.Valid() {
			pds = append(pds, float64(p))
		}
	}
	if len(pds) < 12 {
		add("sectors", "sectors.no-data", "Coverage sectors", -1, "Not enough bearing sectors with traffic yet.", nil)
	} else {
		sorted := append([]float64(nil), pds...)
		sort.Float64s(sorted)
		med := qSorted(sorted, 0.5)
		for i, p := range det.VsBearing.Pos {
			if !p.Valid() || float64(p) >= blockedRatio*med {
				continue
			}
			if n := len(h.Blocked); n > 0 && h.Blocked[n-1].ToDeg == i*10 {
				h.Blocked[n-1].ToDeg = i*10 + 10
				h.Blocked[n-1].Pd = Num(math.Min(float64(h.Blocked[n-1].Pd), float64(p)))
				continue
			}
			h.Blocked = append(h.Blocked, Sector{FromDeg: i * 10, ToDeg: i*10 + 10, Pd: p, MedianPd: Num(med)})
		}
		score := 100 - 15*float64(len(h.Blocked))
		text := "No sector with clearly reduced detection probability."
		code, data := "sectors.clear", Data{}
		if len(h.Blocked) > 0 {
			// data carries every printed sector as fromDeg1 / toDeg1,
			// fromDeg2 / toDeg2 ... (Data holds numbers only, and only
			// numbers printed in text: count the keys); fromDeg / toDeg
			// repeat the first one. The full list is also Health.Blocked.
			code = "sectors.blocked"
			data["fromDeg"], data["toDeg"] = Num(h.Blocked[0].FromDeg), Num(h.Blocked[0].ToDeg)
			text = "Reduced detection in"
			for i, b := range h.Blocked {
				text += fmt.Sprintf(" %d-%d°", b.FromDeg, b.ToDeg)
				data[fmt.Sprintf("fromDeg%d", i+1)] = Num(b.FromDeg)
				data[fmt.Sprintf("toDeg%d", i+1)] = Num(b.ToDeg)
			}
			text += ": possible obstruction (buildings, terrain, mast) or antenna pattern null in that direction."
			adv := make(Data, len(data))
			for k, v := range data {
				adv[k] = v
			}
			advise("advice.sectors-blocked", text, adv)
		}
		add("sectors", code, "Coverage sectors", clampScore(score), text, data)
	}

	// 5. update interval
	var gm, gs float64
	var gn int64
	for i := 0; i < 3 && i < len(det.Gaps); i++ { // < 150 km
		g := det.Gaps[i]
		gn += g.N
		if len(g.PUpdate) > 2 && g.PUpdate[2].Valid() {
			gm += float64(g.PUpdate[2]) * float64(g.Mean) * float64(g.N)
			gs += float64(g.Mean) * float64(g.N)
		}
	}
	if gn < 500 || gs <= 0 {
		add("updates", "updates.no-data", "Position update rate", -1, "Not enough position updates within 150 km yet.", Data{"distKm": 150})
	} else {
		p := gm / gs
		score := 100 - math.Max(0, updateTarget-p)*400
		code := "updates.ok"
		if p < updateTarget {
			code = "updates.low"
		}
		add("updates", code, "Position update rate", clampScore(score),
			fmt.Sprintf("Within 150 km a position is at most 5 s old %.1f %% of the time (target %.0f %%).", p*100, updateTarget*100),
			Data{"distKm": 150, "ageS": 5, "freshPct": Num(p * 100), "targetPct": Num(updateTarget * 100)})
	}

	// 6. GNSS
	cen := &r.Census
	if cen.ADSB < minHealthAircraf || !cen.GNSSIndexNow.Valid() {
		add("gnss", "gnss.no-data", "GNSS integrity", -1, "Not enough ADS-B aircraft with integrity data.", nil)
	} else {
		g := float64(cen.GNSSIndexNow)
		score := 100 - g*3
		text := fmt.Sprintf("%.1f %% of ADS-B aircraft report degraded NIC/NACp right now; %d aircraft with position jumps.", g, cen.JumpAircraft)
		code := "gnss.ok"
		if g >= gnssBadPct {
			code = "gnss.interference"
			text += " Clustered integrity loss: GNSS interference (jamming/spoofing) in the area is likely - see the interference map."
			advise("advice.gnss", "GNSS interference likely: many aircraft lost navigation integrity at the same time.", nil)
		} else if g >= gnssWarnPct {
			code = "gnss.elevated"
			text += " Elevated; watch the map for a cluster."
		}
		add("gnss", code, "GNSS integrity", clampScore(score), text, Data{"degradedPct": Num(g), "jumpAircraft": Num(cen.JumpAircraft)})
	}

	// 7. timing
	if !r.Timing.Available {
		add("timing", "timing.unavailable", "Transponder timing", -1, "No GPS timestamps: enable GPS timestamps on the Radarcape to get squitter interval statistics.", nil)
	} else {
		n := len(r.Timing.NonCompliant)
		score := 100 - 10*float64(n)
		text := fmt.Sprintf("Even/odd alternation %s; %d transponder(s) with non-compliant squitter timing.", pct(r.Timing.AltRegularity), n)
		add("timing", "timing.measured", "Transponder timing", clampScore(score), text,
			Data{"alternationPct": pctNum(r.Timing.AltRegularity), "nonCompliant": Num(n)})
	}

	// overall
	var tot, w float64
	worst := 101
	for _, it := range h.Items {
		if it.Score < 0 {
			continue
		}
		wt := 1.0
		if it.Key == "gain" || it.Key == "interference" || it.Key == "detection" {
			wt = 2
		}
		tot += wt * float64(it.Score)
		w += wt
		if it.Score < worst {
			worst = it.Score
		}
	}
	if w == 0 {
		h.Score = -1
	} else {
		// the average, but never more than 20 points above the worst item
		h.Score = clampScore(math.Min(tot/w, float64(worst)+20))
	}
	h.Status = status(h.Score)
	if len(h.Advice) == 0 && h.Score >= 80 {
		advise("advice.healthy", "RF chain looks healthy.", nil)
	}
	return h
}

func kmStr(v Num) string {
	if !v.Valid() {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", float64(v))
}
