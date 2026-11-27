package terrain

import (
	"fmt"
	"math"
	"sort"

	"airview/internal/geo"
	"airview/internal/radio"
)

// DefaultAdviceExtraM are the extra antenna heights compared by SiteAdvice.
var DefaultAdviceExtraM = []float64{0, 2, 5, 10}

// HeightOption is the coverage for one extra antenna height.
type HeightOption struct {
	ExtraM      float64   `json:"extraM"`
	AntennaAGLM float64   `json:"antennaAglM"`
	AreaKm2     []float64 `json:"areaKm2"`     // per altitude (SiteAdvice.AltitudesFt)
	MeanRangeKm []float64 `json:"meanRangeKm"` // per altitude
	AreaGainPct []float64 `json:"areaGainPct"` // vs. the current height, per altitude
}

// SectorInfo describes the terrain in one 10° sector.
type SectorInfo struct {
	Sector         int     `json:"sector"` // 0..35 (geo.SectorBin)
	FromDeg        float64 `json:"fromDeg"`
	ToDeg          float64 `json:"toDeg"`
	MaxSkylineDeg  float64 `json:"maxSkylineDeg"`
	MeanSkylineDeg float64 `json:"meanSkylineDeg"`
	// LimitDistKm is the distance of the highest skyline point in the sector.
	LimitDistKm float64 `json:"limitDistKm"`
	LimitLat    float64 `json:"limitLat"`
	LimitLon    float64 `json:"limitLon"`
	LimitTerrM  float64 `json:"limitTerrainM"`
	// RangeKm is the mean LOS range at the reference altitude, SmoothKm the
	// smooth-earth range and BlockedPct = 100·(1 − RangeKm/SmoothKm).
	RangeKm    float64 `json:"rangeKm"`
	SmoothKm   float64 `json:"smoothKm"`
	BlockedPct float64 `json:"blockedPct"`
	// Nearest terrain rising above the antenna's horizontal (-1 = none).
	NearestObstacleKm   float64 `json:"nearestObstacleKm"`
	NearestObstacleDeg  float64 `json:"nearestObstacleDeg"`
	NearestObstacleBrg  float64 `json:"nearestObstacleBrg"`
	NearestObstacleLat  float64 `json:"nearestObstacleLat"`
	NearestObstacleLon  float64 `json:"nearestObstacleLon"`
	NearestObstacleTerM float64 `json:"nearestObstacleTerrainM"`
}

// SiteAdvice summarises what the antenna site gains from more height and
// where the terrain blocks it.
type SiteAdvice struct {
	AntennaID   string         `json:"antennaId"`
	AltitudesFt []float64      `json:"altitudesFt"`
	RefAltFt    float64        `json:"refAltFt"` // altitude used for sector blockage
	Heights     []HeightOption `json:"heights"`
	// KM2PerMetre is the coverage area gained per extra metre of antenna
	// height (between the current and the highest option), per altitude.
	Km2PerMetre  []float64    `json:"km2PerMetre"`
	Sectors      []SectorInfo `json:"sectors"`      // 36 × 10°
	WorstSectors []int        `json:"worstSectors"` // indices into Sectors, most blocked first (max 5)
	Summary      []string     `json:"summary"`
	// SummaryItems are the Summary sentences, in the same order, with a
	// stable code and the printed numbers under unit-suffixed names (see
	// AdviceLine).
	SummaryItems []AdviceLine `json:"summaryItems"`
}

// AdviceLine is one site-advice sentence with its numbers, so a UI can
// re-format them in the viewer's units. Text equals the Summary entry.
//
// Codes and Data fields:
//
//	terrain.height-gain     heightM (extra antenna height), pct (coverage area
//	                        gain), altFt, areaKm2 (area gained), km2PerM
//	terrain.sector-blocked  fromDeg, toDeg, elevDeg (max skyline), distKm (of the
//	                        skyline point), altFt (reference altitude), pct (range
//	                        below the smooth-earth horizon); obstacleKm and
//	                        obstacleBrgDeg when terrain rises above the horizontal
//	terrain.no-blockage     (no fields; the altitude is SiteAdvice.RefAltFt)
type AdviceLine struct {
	Code string             `json:"code"`
	Text string             `json:"text"`
	Data map[string]float64 `json:"data"`
}

// computeAdvice evaluates extra antenna heights over the same polar terrain
// and describes the sectors of base.
func computeAdvice(pt *PolarTerrain, base *AntennaCoverage, k, minObs float64, alts, extras []float64) *SiteAdvice {
	if len(extras) == 0 {
		extras = DefaultAdviceExtraM
	}
	adv := &SiteAdvice{AntennaID: base.ID, AltitudesFt: alts, Heights: make([]HeightOption, 0, len(extras))}
	var area0 []float64
	for _, ex := range extras {
		var los []AltRange
		if ex == 0 {
			los = base.LOS
		} else {
			los = NewPanorama(pt, base.AntennaM+ex, k, minObs).LOSRanges(alts)
		}
		h := HeightOption{ExtraM: ex, AntennaAGLM: round(base.HeightAGLM+ex, 2),
			AreaKm2: make([]float64, len(los)), MeanRangeKm: make([]float64, len(los)), AreaGainPct: make([]float64, len(los))}
		for i, r := range los {
			h.AreaKm2[i], h.MeanRangeKm[i] = r.AreaKm2, r.MeanKm
		}
		if ex == 0 || area0 == nil {
			if ex == 0 {
				area0 = h.AreaKm2
			}
		}
		adv.Heights = append(adv.Heights, h)
	}
	if area0 == nil && len(adv.Heights) > 0 {
		area0 = adv.Heights[0].AreaKm2
	}
	maxEx, maxIdx := 0.0, -1
	for hi := range adv.Heights {
		h := &adv.Heights[hi]
		for i := range h.AreaKm2 {
			if area0[i] > 0 {
				h.AreaGainPct[i] = round(100*(h.AreaKm2[i]-area0[i])/area0[i], 2)
			}
		}
		if h.ExtraM > maxEx {
			maxEx, maxIdx = h.ExtraM, hi
		}
	}
	adv.Km2PerMetre = make([]float64, len(alts))
	if maxIdx >= 0 {
		for i := range alts {
			adv.Km2PerMetre[i] = round((adv.Heights[maxIdx].AreaKm2[i]-area0[i])/maxEx, 1)
		}
	}

	// Sectors at the reference altitude (the one closest to 10 000 ft).
	ref := 0
	for i, a := range alts {
		if math.Abs(a-10000) < math.Abs(alts[ref]-10000) {
			ref = i
		}
	}
	if len(alts) > 0 {
		adv.RefAltFt = alts[ref]
	}
	p := base.pano
	o := newOrigin(p.Lat, p.Lon)
	smooth := 0.0
	if len(alts) > 0 {
		smooth = radio.LineOfSightKm(p.AntennaM, alts[ref]*geo.MetresPerFoot, p.K)
	}
	adv.Sectors = make([]SectorInfo, geo.Sectors)
	sums := make([]float64, geo.Sectors)
	rngs := make([]float64, geo.Sectors)
	cnts := make([]int, geo.Sectors)
	for s := range adv.Sectors {
		adv.Sectors[s] = SectorInfo{Sector: s, FromDeg: float64(s) * geo.SectorDeg, ToDeg: float64(s+1) * geo.SectorDeg,
			MaxSkylineDeg: -90, NearestObstacleKm: -1, SmoothKm: round(math.Min(smooth, p.RadiusKm), 3)}
	}
	for i := 0; i < p.rays; i++ {
		brg := float64(i) * p.StepDeg
		s := geo.SectorBin(brg)
		if s < 0 || s >= geo.Sectors {
			continue
		}
		si := &adv.Sectors[s]
		a := float64(p.skyAng[i])
		sums[s] += a
		cnts[s]++
		if len(base.LOS) > ref {
			rngs[s] += base.LOS[ref].RangeKm[i]
		}
		if a > si.MaxSkylineDeg {
			si.MaxSkylineDeg = a
			si.LimitDistKm = float64(p.skyDist[i])
			si.LimitTerrM = float64(p.skyElev[i])
			si.LimitLat, si.LimitLon = o.destKm(brg, si.LimitDistKm)
		}
		if nd := float64(p.nearDist[i]); nd >= 0 && (si.NearestObstacleKm < 0 || nd < si.NearestObstacleKm) {
			si.NearestObstacleKm = nd
			si.NearestObstacleDeg = float64(p.nearAng[i])
			si.NearestObstacleBrg = brg
			si.NearestObstacleTerM = float64(p.nearElev[i])
			si.NearestObstacleLat, si.NearestObstacleLon = o.destKm(brg, nd)
		}
	}
	for s := range adv.Sectors {
		si := &adv.Sectors[s]
		if cnts[s] > 0 {
			si.MeanSkylineDeg = sums[s] / float64(cnts[s])
			si.RangeKm = rngs[s] / float64(cnts[s])
		}
		if si.SmoothKm > 0 {
			si.BlockedPct = math.Max(0, math.Min(100, 100*(1-si.RangeKm/si.SmoothKm)))
		}
		for _, f := range []*float64{&si.MaxSkylineDeg, &si.MeanSkylineDeg, &si.NearestObstacleDeg} {
			*f = round(*f, 3)
		}
		for _, f := range []*float64{&si.LimitDistKm, &si.RangeKm, &si.NearestObstacleKm} {
			*f = round(*f, 3)
		}
		for _, f := range []*float64{&si.LimitLat, &si.LimitLon, &si.NearestObstacleLat, &si.NearestObstacleLon} {
			*f = round(*f, 5)
		}
		si.LimitTerrM = round(si.LimitTerrM, 1)
		si.NearestObstacleTerM = round(si.NearestObstacleTerM, 1)
		si.BlockedPct = round(si.BlockedPct, 1)
	}
	order := make([]int, len(adv.Sectors))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := adv.Sectors[order[a]], adv.Sectors[order[b]]
		if sa.BlockedPct != sb.BlockedPct {
			return sa.BlockedPct > sb.BlockedPct
		}
		return sa.MaxSkylineDeg > sb.MaxSkylineDeg
	})
	adv.WorstSectors = []int{}
	for _, s := range order {
		if len(adv.WorstSectors) >= 5 || adv.Sectors[s].BlockedPct < 1 {
			break
		}
		adv.WorstSectors = append(adv.WorstSectors, s)
	}
	adv.SummaryItems = adviceSummary(adv)
	adv.Summary = make([]string, len(adv.SummaryItems))
	for i, l := range adv.SummaryItems {
		adv.Summary[i] = l.Text
	}
	return adv
}

func adviceSummary(adv *SiteAdvice) []AdviceLine {
	out := []AdviceLine{}
	line := func(code, text string, data map[string]float64) {
		for k, v := range data {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				delete(data, k)
			}
		}
		out = append(out, AdviceLine{Code: code, Text: text, Data: data})
	}
	if len(adv.Heights) > 1 && len(adv.AltitudesFt) > 0 {
		hi := adv.Heights[len(adv.Heights)-1]
		for _, h := range adv.Heights {
			if h.ExtraM > hi.ExtraM {
				hi = h
			}
		}
		// Report the lowest altitude, where height matters most, and the reference altitude.
		idxs := []int{0}
		for i, a := range adv.AltitudesFt {
			if a == adv.RefAltFt && i != 0 {
				idxs = append(idxs, i)
			}
		}
		for _, i := range idxs {
			gain := hi.AreaKm2[i] - adv.Heights[0].AreaKm2[i]
			line("terrain.height-gain",
				fmt.Sprintf("Raising the antenna by %g m adds %.1f %% coverage area at %s ft (%+.0f km², about %.0f km² per metre).",
					hi.ExtraM, hi.AreaGainPct[i], groupThousands(adv.AltitudesFt[i]), gain, adv.Km2PerMetre[i]),
				map[string]float64{"heightM": hi.ExtraM, "pct": hi.AreaGainPct[i], "altFt": adv.AltitudesFt[i],
					"areaKm2": round(gain, 1), "km2PerM": adv.Km2PerMetre[i]})
		}
	}
	for n, s := range adv.WorstSectors {
		if n >= 3 {
			break
		}
		si := adv.Sectors[s]
		msg := fmt.Sprintf("Sector %03.0f°–%03.0f°: skyline up to %.2f° at %.1f km; range at %s ft %.0f %% below the smooth-earth horizon.",
			si.FromDeg, si.ToDeg, si.MaxSkylineDeg, si.LimitDistKm, groupThousands(adv.RefAltFt), si.BlockedPct)
		data := map[string]float64{"fromDeg": si.FromDeg, "toDeg": si.ToDeg, "elevDeg": si.MaxSkylineDeg,
			"distKm": si.LimitDistKm, "altFt": adv.RefAltFt, "pct": si.BlockedPct}
		if si.NearestObstacleKm >= 0 {
			msg += fmt.Sprintf(" Nearest obstacle above the horizontal: %.1f km at %.0f°.", si.NearestObstacleKm, si.NearestObstacleBrg)
			data["obstacleKm"], data["obstacleBrgDeg"] = si.NearestObstacleKm, si.NearestObstacleBrg
		}
		line("terrain.sector-blocked", msg, data)
	}
	if len(adv.WorstSectors) == 0 {
		line("terrain.no-blockage", "No sector is noticeably blocked by terrain at the reference altitude.", map[string]float64{})
	}
	return out
}

func groupThousands(v float64) string {
	n := int64(math.Round(v))
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
