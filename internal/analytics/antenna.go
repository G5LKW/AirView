package analytics

import (
	"context"
	"fmt"
	"math"

	"airview/internal/geo"
	"airview/internal/model"
	"airview/internal/radio"
	"airview/internal/stat"
)

// azimuthRefElevDeg is the elevation at which modelled azimuth patterns are
// evaluated (typical elevation of en-route traffic 50-250 km out).
const azimuthRefElevDeg = 3.0

// ------------------------------------------------------ RSSI vs distance

// PathLossFit is the fitted signal-vs-distance law
// rssi = InterceptDB - SlopeDBPerDecade * log10(d_km).
type PathLossFit struct {
	Bins             int     `json:"bins"`             // distance bins used
	MinKm            float64 `json:"minKm"`            // fitted distance range
	MaxKm            float64 `json:"maxKm"`            //
	SlopeDBPerDecade Num     `json:"slopeDbPerDecade"` // measured loss per decade of distance
	FreeSpaceSlope   float64 `json:"freeSpaceSlope"`   // 20 dB / decade
	InterceptDB      Num     `json:"interceptDb"`      // extrapolated level at 1 km, dBFS
	R2               Num     `json:"r2"`               //
	// SystemOffsetDB is the mean of rssi + 20*log10(d): the level a free-space
	// link would give at 1 km (transmit power + antenna and receiver gains in
	// dBFS terms). It moves with receiver gain, cable loss and antenna gain.
	SystemOffsetDB Num    `json:"systemOffsetDb"`
	LineKm         Nums   `json:"lineKm"`      // sample distances for plotting
	MeasuredDB     Nums   `json:"measuredDb"`  // fitted line
	FreeSpaceDB    Nums   `json:"freeSpaceDb"` // 20 dB / decade line through SystemOffsetDB
	Text           string `json:"text"`
}

// SignalDistance is the RSSI-vs-distance 2-D histogram.
type SignalDistance struct {
	DistBinKm int `json:"distBinKm"`
	// Cells are the non-empty bins as [distance bin, RSSI dB (floor), count].
	Cells    [][3]int64 `json:"cells"`
	Total    int64      `json:"total"`
	MaxCount int64      `json:"maxCount"`
	// Per distance bin (index = bin, centre = (i + 0.5) * DistBinKm).
	DistKm Nums    `json:"distKm"`
	N      []int64 `json:"n"`
	Median Nums    `json:"median"`
	P10    Nums    `json:"p10"`
	P90    Nums    `json:"p90"`
	// Overall distribution.
	RSSIP01 Num `json:"rssiP01"`
	RSSIP50 Num `json:"rssiP50"`
	RSSIP99 Num `json:"rssiP99"`
	// StrongPct is the share of samples above -3 dBFS (overload indicator).
	StrongPct Num          `json:"strongPct"`
	Fit       *PathLossFit `json:"fit,omitempty"`
}

// rssiOfBin maps a stored signal_hist rssi_bin to dBFS (bin centre). Both
// encodings in use are understood: floor(dBFS) <= 0 (store.RSSIBin) and the
// positive index of geo.RSSIBin.
func rssiOfBin(b int) float64 {
	switch {
	case b < 0:
		return float64(b) + 0.5
	case b == 0:
		return -0.5
	}
	return -(float64(b) + 0.5)
}

const rssiHistLo, rssiHistBins = -90.0, 91 // 1 dB bins [-90, 1)

func (a *Analyzer) signalDistance(ctx context.Context, ant string, d0, d1 int) (*SignalDistance, error) {
	res := &SignalDistance{
		DistBinKm: geo.DistBinKm, Cells: [][3]int64{},
		RSSIP01: Num(math.NaN()), RSSIP50: Num(math.NaN()), RSSIP99: Num(math.NaN()), StrongPct: Num(math.NaN()),
	}
	hist := map[int]*[rssiHistBins]int64{}
	var all [rssiHistBins]int64
	maxBin := -1
	var strong int64
	rows, err := a.st.SignalTotals(ctx, a.antKey(ant), d0, d1, a.pending().Signal)
	for _, r := range rows {
		func() {
			db, rb, n := r.DistBin, r.RSSIBin, r.Count
			if n <= 0 || db < 0 || db > 200 {
				return
			}
			v := rssiOfBin(rb)
			i := int(math.Floor(v - rssiHistLo))
			i = min(max(i, 0), rssiHistBins-1)
			h := hist[db]
			if h == nil {
				h = &[rssiHistBins]int64{}
				hist[db] = h
			}
			h[i] += n
			all[i] += n
			res.Cells = append(res.Cells, [3]int64{int64(db), int64(math.Floor(v)), n})
			res.Total += n
			res.MaxCount = max(res.MaxCount, n)
			maxBin = max(maxBin, db)
			if v > -3 {
				strong += n
			}
		}()
	}
	if err != nil {
		return nil, err
	}
	nb := maxBin + 1
	res.DistKm, res.N = make(Nums, nb), make([]int64, nb)
	res.Median, res.P10, res.P90 = nanSlice(nb), nanSlice(nb), nanSlice(nb)
	for b := 0; b < nb; b++ {
		res.DistKm[b] = (float64(b) + 0.5) * geo.DistBinKm
		h := hist[b]
		if h == nil {
			continue
		}
		for _, c := range h {
			res.N[b] += c
		}
		res.Median[b] = stat.HistQuantile(h[:], rssiHistLo, 1, 0.5)
		res.P10[b] = stat.HistQuantile(h[:], rssiHistLo, 1, 0.1)
		res.P90[b] = stat.HistQuantile(h[:], rssiHistLo, 1, 0.9)
	}
	if res.Total > 0 {
		res.RSSIP01 = Num(stat.HistQuantile(all[:], rssiHistLo, 1, 0.01))
		res.RSSIP50 = Num(stat.HistQuantile(all[:], rssiHistLo, 1, 0.5))
		res.RSSIP99 = Num(stat.HistQuantile(all[:], rssiHistLo, 1, 0.99))
		res.StrongPct = Num(pct(float64(strong), float64(res.Total)))
	}
	res.Fit = fitPathLoss(res)
	return res, nil
}

// fitPathLoss fits the median RSSI per distance bin against log10(distance).
// Bins nearer than 20 km (elevation-pattern effects, overload) and the far
// tail beyond 90 % of all samples (only the loudest aircraft are still
// decoded there, which flattens the curve) are left out.
func fitPathLoss(sd *SignalDistance) *PathLossFit {
	if sd.Total < 200 {
		return nil
	}
	minN := max(int64(20), sd.Total/500)
	var cum int64
	var X [][]float64
	var y, w []float64
	lo, hi := math.Inf(1), 0.0
	var offS, offW float64
	for b := range sd.N {
		cum += sd.N[b]
		d := sd.DistKm[b]
		tail := float64(cum-sd.N[b]) > 0.9*float64(sd.Total)
		if d < 20 || sd.N[b] < minN || tail || !isNum(sd.Median[b]) {
			continue
		}
		X = append(X, []float64{math.Log10(d)})
		y = append(y, sd.Median[b])
		wt := math.Sqrt(float64(sd.N[b]))
		w = append(w, wt)
		offS += wt * (sd.Median[b] + 20*math.Log10(d))
		offW += wt
		lo, hi = math.Min(lo, d), math.Max(hi, d)
	}
	if len(y) < 4 || hi < 2*lo {
		return nil
	}
	fit, err := stat.WLS(X, y, w, true)
	if err != nil || len(fit.Coef) < 2 {
		return nil
	}
	f := &PathLossFit{
		Bins: len(y), MinKm: lo, MaxKm: hi, FreeSpaceSlope: 20,
		SlopeDBPerDecade: Num(-fit.Coef[1]), InterceptDB: Num(fit.Coef[0]), R2: Num(fit.R2),
		SystemOffsetDB: Num(offS / offW),
	}
	for i := 0; i <= 24; i++ {
		d := lo * math.Pow(hi/lo, float64(i)/24)
		f.LineKm = append(f.LineKm, d)
		f.MeasuredDB = append(f.MeasuredDB, fit.Coef[0]+fit.Coef[1]*math.Log10(d))
		f.FreeSpaceDB = append(f.FreeSpaceDB, offS/offW-20*math.Log10(d))
	}
	s := -fit.Coef[1]
	switch {
	case s > 26:
		f.Text = fmt.Sprintf("Signal falls by %.0f dB per decade of distance, faster than free space (20): distant aircraft are low on the horizon and lose signal to the antenna's elevation pattern, terrain or the atmosphere.", s)
	case s < 14:
		f.Text = fmt.Sprintf("Signal falls by only %.0f dB per decade of distance (free space: 20): weak distant signals are missing from the statistics, so the receiver is sensitivity-limited rather than horizon-limited.", s)
	default:
		f.Text = fmt.Sprintf("Signal falls by %.0f dB per decade of distance, close to the free-space law (20).", s)
	}
	return f
}

// -------------------------------------------------------- azimuth pattern

// AzimuthPattern is the measured azimuth gain pattern.
type AzimuthPattern struct {
	// Method: "heat-residual" (mean of rssi + 20*log10(slant) per bearing
	// over the all-time heat grid, 30-300 km, elevation 0.3-12 degrees, with
	// the common distance trend removed) or "coverage-rssi" (mean RSSI per
	// bearing at 10-40 kft relative to the all-round mean; used while the
	// receiver position is unknown; less accurate).
	Method     string `json:"method"`
	BearingDeg Nums   `json:"bearingDeg"`
	RelDB      Nums   `json:"relDb"` // smoothed, relative to the all-round median; null = no data
	RawDB      Nums   `json:"rawDb"` // unsmoothed
	Cells      []int  `json:"cells"` // samples (heat cells / coverage rows) per bearing
	Bearings   int    `json:"bearings"`
	Samples    int64  `json:"samples"`
	PeakDB     Num    `json:"peakDb"`
	PeakBrg    Num    `json:"peakBrg"`
	MinDB      Num    `json:"minDb"`
	MinBrg     Num    `json:"minBrg"`
	SpreadDB   Num    `json:"spreadDb"` // 95th - 5th percentile over bearings
	// DistSlopeDB is the residual distance trend that was removed, dB per
	// decade (0 = free-space law holds exactly).
	DistSlopeDB Num `json:"distSlopeDb"`

	w []float64 // weight per bearing bin
}

type azSample struct {
	bin     int
	x, y, w float64
}

// measureAzimuthHeat derives the azimuth pattern from heat-grid residuals.
func measureAzimuthHeat(s site, cells []heatCell) *AzimuthPattern {
	if !s.located || len(cells) == 0 {
		return nil
	}
	st := geo.NewStation(s.lat, s.lon, s.antM, 0)
	samples := make([]azSample, 0, len(cells)/2)
	var total int64
	for i := range cells {
		c := &cells[i]
		if c.rssiN < 3 || !isNum(c.rssi) {
			continue
		}
		km, brg := st.DistanceBearing(c.lat, c.lon)
		if km < 30 || km > 300 {
			continue
		}
		altFt := 33000.0
		switch {
		case isNum(c.minAlt) && isNum(c.maxAlt):
			altFt = (c.minAlt + c.maxAlt) / 2
		case isNum(c.maxAlt):
			altFt = c.maxAlt
		case isNum(c.minAlt):
			altFt = c.minAlt
		}
		altM := geo.FeetToMetres(altFt)
		if el := geo.ElevationDeg(km, s.antM, altM, 0); el < 0.3 || el > 12 {
			continue
		}
		slant := geo.SlantRangeKm(km, s.antM, altM)
		if !(slant > 1) {
			continue
		}
		samples = append(samples, azSample{
			bin: geo.BearingBin(brg), x: math.Log10(km), y: c.rssi + 20*math.Log10(slant),
			w: math.Min(float64(c.rssiN), 200),
		})
		total += c.rssiN
	}
	if len(samples) < 20 {
		return nil
	}
	// Two-way model y = g[bin] + b * x: b is the "within-bin" slope.
	var sw, sx, sy [brgBins]float64
	var n [brgBins]int
	var allW, allX float64
	for _, q := range samples {
		sw[q.bin] += q.w
		sx[q.bin] += q.w * q.x
		sy[q.bin] += q.w * q.y
		n[q.bin]++
		allW += q.w
		allX += q.w * q.x
	}
	var num, den float64
	for _, q := range samples {
		dx := q.x - sx[q.bin]/sw[q.bin]
		num += q.w * dx * (q.y - sy[q.bin]/sw[q.bin])
		den += q.w * dx * dx
	}
	slope := 0.0
	if den > 0.5 {
		slope = clamp(num/den, -15, 15)
	}
	p := &AzimuthPattern{Method: "heat-residual", Samples: total, DistSlopeDB: Num(slope)}
	raw := nanSlice(brgBins)
	p.Cells = make([]int, brgBins)
	p.w = make([]float64, brgBins)
	xAll := allX / allW
	for b := 0; b < brgBins; b++ {
		p.Cells[b] = n[b]
		if n[b] < 2 && sw[b] < 6 {
			continue
		}
		raw[b] = sy[b]/sw[b] - slope*(sx[b]/sw[b]-xAll)
		p.w[b] = sw[b]
	}
	finishAzimuth(p, raw)
	return p
}

// measureAzimuthCoverage is the fallback without a receiver position.
func measureAzimuthCoverage(cd *covData) *AzimuthPattern {
	p := &AzimuthPattern{Method: "coverage-rssi", DistSlopeDB: Num(math.NaN())}
	raw := nanSlice(brgBins)
	p.Cells = make([]int, brgBins)
	p.w = make([]float64, brgBins)
	var sum, wsum [brgBins]float64
	for k := 3; k <= 5; k++ {
		var bs, bn float64
		for b := 0; b < brgBins; b++ {
			bs += cd.rssiS[k][b]
			bn += float64(cd.rssiN[k][b])
		}
		if bn < 200 {
			continue
		}
		mean := bs / bn
		for b := 0; b < brgBins; b++ {
			if cd.rssiN[k][b] < 5 {
				continue
			}
			w := math.Min(float64(cd.rssiN[k][b]), 2000)
			sum[b] += w * (cd.rssiS[k][b]/float64(cd.rssiN[k][b]) - mean)
			wsum[b] += w
			p.Cells[b]++
			p.Samples += cd.rssiN[k][b]
		}
	}
	nb := 0
	for b := 0; b < brgBins; b++ {
		if wsum[b] > 0 {
			raw[b] = sum[b] / wsum[b]
			p.w[b] = wsum[b]
			nb++
		}
	}
	if nb < 10 {
		return nil
	}
	finishAzimuth(p, raw)
	return p
}

// finishAzimuth smooths, normalises and summarises a raw pattern.
func finishAzimuth(p *AzimuthPattern, raw Nums) {
	p.BearingDeg = make(Nums, brgBins)
	for b := range p.BearingDeg {
		p.BearingDeg[b] = geo.BearingBinCenter(b)
	}
	sm := nanSlice(brgBins)
	for b := 0; b < brgBins; b++ {
		if !isNum(raw[b]) {
			continue
		}
		var s, w float64
		for o, k := range [3]float64{1, 2, 1} {
			j := (b + o - 1 + brgBins) % brgBins
			if isNum(raw[j]) {
				s += k * p.w[j] * raw[j]
				w += k * p.w[j]
			}
		}
		sm[b] = s / w
		p.Bearings++
	}
	ref := stat.WeightedQuantile(sm, p.w, 0.5)
	if !isNum(ref) {
		ref = stat.Median(sm)
	}
	p.RelDB, p.RawDB = nanSlice(brgBins), nanSlice(brgBins)
	p.PeakDB, p.PeakBrg, p.MinDB, p.MinBrg = Num(math.NaN()), Num(math.NaN()), Num(math.NaN()), Num(math.NaN())
	for b := 0; b < brgBins; b++ {
		if !isNum(sm[b]) {
			continue
		}
		p.RelDB[b] = sm[b] - ref
		p.RawDB[b] = raw[b] - ref
		if !(float64(p.PeakDB) >= p.RelDB[b]) {
			p.PeakDB, p.PeakBrg = Num(p.RelDB[b]), Num(p.BearingDeg[b])
		}
		if !(float64(p.MinDB) <= p.RelDB[b]) {
			p.MinDB, p.MinBrg = Num(p.RelDB[b]), Num(p.BearingDeg[b])
		}
	}
	p.SpreadDB = Num(stat.Quantile(p.RelDB, 0.95) - stat.Quantile(p.RelDB, 0.05))
}

// -------------------------------------------------------------- lobe fit

// ConfiguredYagi is the directional antenna from the configuration that the
// measured lobe is compared with.
type ConfiguredYagi struct {
	AntennaID      string  `json:"antennaId"`
	Name           string  `json:"name"`
	AzimuthDeg     float64 `json:"azimuthDeg"`
	HBeamwidthDeg  float64 `json:"hBeamwidthDeg"`
	GainOverOmniDB Num     `json:"gainOverOmniDb"` // modelled advantage over the omni at low elevation, net of feed losses
}

// YagiFit is the directional lobe inferred from the measured azimuth
// pattern by fitting  level + max(omni, yagi lobe)  (a diversity receiver
// uses whichever antenna hears the frame better):
//
//	m(az) = C + max(0, G + 10*n*log10(cos(az - az0))),  cos^n(BW/2) = 1/2
type YagiFit struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`

	AzimuthDeg     Num `json:"azimuthDeg"`     // measured boresight
	BeamwidthDeg   Num `json:"beamwidthDeg"`   // measured -3 dB beamwidth of the lobe
	GainOverOmniDB Num `json:"gainOverOmniDb"` // lobe peak above the omni level
	// Confidence 0..1: how much of the azimuth variation the lobe explains,
	// how far it stands above the omni level and how complete the data is.
	Confidence      Num    `json:"confidence"`
	ConfidenceLabel string `json:"confidenceLabel"` // "low" | "medium" | "high"
	ExplainedPct    Num    `json:"explainedPct"`
	RMSDB           Num    `json:"rmsDb"`
	FitDB           Nums   `json:"fitDb"` // fitted curve per bearing bin, same reference as AzimuthPattern.RelDB

	Configured        *ConfiguredYagi `json:"configured,omitempty"`
	PointingErrorDeg  Num             `json:"pointingErrorDeg"`  // measured - configured; positive = clockwise (right)
	BeamwidthErrorDeg Num             `json:"beamwidthErrorDeg"` // measured - configured
	GainErrorDB       Num             `json:"gainErrorDb"`       // measured - modelled advantage
	Text              string          `json:"text"`
}

func lobeExponent(bwDeg float64) float64 {
	c := math.Cos(bwDeg / 2 * math.Pi / 180)
	return -3.0103 / (10 * math.Log10(c))
}

// lobeDB is the main-lobe level in dB relative to its peak, delta degrees
// off boresight.
func lobeDB(delta, n float64) float64 {
	c := math.Cos(delta * math.Pi / 180)
	if c <= 1e-6 {
		return -200
	}
	return 10 * n * math.Log10(c)
}

// lobeSSE evaluates the model for one parameter set; it returns the weighted
// SSE and the optimal level C.
func lobeSSE(y, w []float64, az0, bw, gain float64) (sse, level float64) {
	n := lobeExponent(bw)
	var sw, sr, srr float64
	for b := range y {
		if !(w[b] > 0) {
			continue
		}
		l := gain + lobeDB(geo.AngleDiff((float64(b)+0.5)*360/float64(len(y)), az0), n)
		if l < 0 {
			l = 0
		}
		r := y[b] - l
		sw += w[b]
		sr += w[b] * r
		srr += w[b] * r * r
	}
	if sw <= 0 {
		return math.Inf(1), 0
	}
	return srr - sr*sr/sw, sr / sw
}

// fitLobe fits the omni + lobe model to a measured pattern.
func fitLobe(p *AzimuthPattern) *YagiFit {
	nan := Num(math.NaN())
	f := &YagiFit{AzimuthDeg: nan, BeamwidthDeg: nan, GainOverOmniDB: nan, Confidence: 0, ConfidenceLabel: "low",
		ExplainedPct: nan, RMSDB: nan, PointingErrorDeg: nan, BeamwidthErrorDeg: nan, GainErrorDB: nan}
	if p == nil || p.Bearings < 45 {
		f.Reason = "not enough bearings with data for a pattern fit"
		return f
	}
	y := make([]float64, brgBins)
	w := make([]float64, brgBins)
	var wmax float64
	for b := 0; b < brgBins; b++ {
		if isNum(p.RelDB[b]) && p.w[b] > 0 {
			y[b] = p.RelDB[b]
			w[b] = math.Sqrt(p.w[b])
			wmax = math.Max(wmax, w[b])
		}
	}
	// Null model: a constant.
	var sw, sy, syy float64
	for b := range y {
		sw += w[b]
		sy += w[b] * y[b]
		syy += w[b] * y[b] * y[b]
	}
	sse0 := syy - sy*sy/sw
	if !(sse0 > 0) {
		f.Reason = "the measured pattern is flat"
		return f
	}
	// Coarse grid, then Nelder-Mead polish.
	best, bAz, bBW, bG := math.Inf(1), 0.0, 40.0, 3.0
	for bw := 12.0; bw <= 120; bw += 6 {
		n := lobeExponent(bw)
		var tab [brgBins/2 + 1]float64 // lobe level by |offset| in bins
		for k := range tab {
			tab[k] = lobeDB(float64(k)*brgBinDeg, n)
		}
		for g := 1.0; g <= 24; g += 1 {
			reach := 0
			for reach < len(tab)-1 && g+tab[reach+1] > 0 {
				reach++
			}
			for c := 0; c < brgBins; c++ {
				// SSE = sum w (y-l)^2 - (sum w (y-l))^2 / sum w, with l != 0
				// only within +-reach bins of the candidate boresight.
				sr, srr := sy, syy
				for o := -reach; o <= reach; o++ {
					b := (c + o + brgBins) % brgBins
					if w[b] == 0 {
						continue
					}
					k := o
					if k < 0 {
						k = -k
					}
					l := g + tab[k]
					sr -= w[b] * l
					srr += w[b] * (l*l - 2*l*y[b])
				}
				if sse := srr - sr*sr/sw; sse < best {
					best, bAz, bBW, bG = sse, (float64(c)+0.5)*brgBinDeg, bw, g
				}
			}
		}
	}
	obj := func(x []float64) float64 {
		s, _ := lobeSSE(y, w, x[0], x[1], x[2])
		return s
	}
	x0 := []float64{bAz, bBW, bG}
	if r, err := stat.NelderMead(obj, x0, &stat.NMOptions{
		Step: []float64{2, 5, 0.7}, Lower: []float64{bAz - 20, 8, 0.2}, Upper: []float64{bAz + 20, 150, 30}, MaxIter: 600,
	}); err == nil && len(r.X) == 3 && r.F <= best {
		best, x0 = r.F, r.X
	}
	az, bw, g := geo.NormalizeDeg(x0[0]), x0[1], x0[2]
	_, level := lobeSSE(y, w, az, bw, g)
	f.FitDB = make(Nums, brgBins)
	n := lobeExponent(bw)
	for b := range f.FitDB {
		f.FitDB[b] = level + math.Max(0, g+lobeDB(geo.AngleDiff((float64(b)+0.5)*brgBinDeg, az), n))
	}
	explained := clamp(1-best/sse0, 0, 1)
	f.AzimuthDeg, f.BeamwidthDeg, f.GainOverOmniDB = Num(az), Num(bw), Num(g)
	f.ExplainedPct = Num(100 * explained)
	f.RMSDB = Num(math.Sqrt(math.Max(best, 0) / sw))
	conf := math.Sqrt(clamp(explained/0.6, 0, 1)) * clamp((g-0.5)/3, 0, 1) * clamp(float64(p.Bearings)/120, 0, 1)
	if p.Method != "heat-residual" {
		conf *= 0.6
	}
	f.Confidence = Num(conf)
	switch {
	case conf >= 0.7:
		f.ConfidenceLabel = "high"
	case conf >= 0.4:
		f.ConfidenceLabel = "medium"
	}
	f.OK = conf >= 0.2
	if !f.OK {
		f.Reason = "no clear directional lobe in the measured azimuth pattern"
	}
	return f
}

// compareYagi fills the configured-vs-measured part of a fit.
func compareYagi(f *YagiFit, ants []model.Antenna) {
	var yagi, omni *model.Antenna
	for i := range ants {
		if radio.Resolve(ants[i]).Directional {
			if yagi == nil {
				yagi = &ants[i]
			}
		} else if omni == nil {
			omni = &ants[i]
		}
	}
	if yagi == nil {
		if f.OK {
			f.Text = fmt.Sprintf("The data shows a directional lobe towards %s (%s), about %.0f° wide and %.1f dB above the all-round level, but no directional antenna is configured.",
				fmtDeg(float64(f.AzimuthDeg)), compass(float64(f.AzimuthDeg)), float64(f.BeamwidthDeg), float64(f.GainOverOmniDB))
		}
		return
	}
	info := radio.Resolve(*yagi)
	cy := &ConfiguredYagi{AntennaID: yagi.ID, Name: yagi.Name, AzimuthDeg: info.AzimuthDeg,
		HBeamwidthDeg: info.HBeamwidthDeg, GainOverOmniDB: Num(math.NaN())}
	if omni != nil {
		gy := info.Pattern().GainDBi(info.AzimuthDeg, azimuthRefElevDeg) - feedLoss(*yagi)
		go_ := radio.FromAntenna(*omni).GainDBi(0, azimuthRefElevDeg) - feedLoss(*omni)
		cy.GainOverOmniDB = Num(gy - go_)
	}
	f.Configured = cy
	if !f.OK {
		f.Text = fmt.Sprintf("No clear lobe of the %s could be found in the combined data yet (configured azimuth %s).", antName(*yagi), fmtDeg(cy.AzimuthDeg))
		return
	}
	perr := geo.AngleDiff(float64(f.AzimuthDeg), cy.AzimuthDeg)
	f.PointingErrorDeg = Num(perr)
	f.BeamwidthErrorDeg = Num(float64(f.BeamwidthDeg) - cy.HBeamwidthDeg)
	if isNum(float64(cy.GainOverOmniDB)) {
		f.GainErrorDB = Num(float64(f.GainOverOmniDB) - float64(cy.GainOverOmniDB))
	}
	side := "right (clockwise)"
	if perr < 0 {
		side = "left (anticlockwise)"
	}
	switch {
	case math.Abs(perr) < 6:
		f.Text = fmt.Sprintf("The %s points where it is configured: measured boresight %s, configured %s.",
			antName(*yagi), fmtDeg(float64(f.AzimuthDeg)), fmtDeg(cy.AzimuthDeg))
	default:
		f.Text = fmt.Sprintf("The %s appears to point %.0f° to the %s of its configured azimuth (measured %s, configured %s).",
			antName(*yagi), math.Abs(perr), side, fmtDeg(float64(f.AzimuthDeg)), fmtDeg(cy.AzimuthDeg))
	}
	f.Text += fmt.Sprintf(" Measured beamwidth %.0f° (configured %.0f°), %.1f dB above the omni level; confidence %s.",
		float64(f.BeamwidthDeg), cy.HBeamwidthDeg, float64(f.GainOverOmniDB), f.ConfidenceLabel)
}

func antName(a model.Antenna) string {
	if a.Name != "" {
		return a.Name
	}
	if a.ID != "" {
		return a.ID
	}
	return "antenna"
}

func feedLoss(a model.Antenna) float64 {
	l := 0.0
	if isNum(a.CableLossDB) && a.CableLossDB > 0 {
		l += a.CableLossDB
	}
	if isNum(a.FilterLossDB) && a.FilterLossDB > 0 {
		l += a.FilterLossDB
	}
	return l
}

// ------------------------------------------------------ elevation pattern

// ElevationPattern is the measured elevation pattern from elev_hist.
type ElevationPattern struct {
	ElevDeg Nums    `json:"elevDeg"` // bin centres, 0.5 degree bins from -2
	Count   []int64 `json:"count"`
	RSSI    Nums    `json:"rssi"`    // mean dBFS
	ResidDB Nums    `json:"residDb"` // mean of rssi + 20*log10(slant)
	// RelDB is the smoothed residual relative to its maximum: the measured
	// elevation gain pattern (null where there are too few samples). Near
	// the horizon it reads high because only strong signals are decoded.
	RelDB   Nums  `json:"relDb"`
	Total   int64 `json:"total"`
	PeakDeg Num   `json:"peakDeg"`
	// LowestDeg is the lowest elevation with regular reception (0.5 % of
	// the samples lie below).
	LowestDeg Num `json:"lowestDeg"`
}

func (a *Analyzer) elevationPattern(ctx context.Context, ant string, d0, d1 int) (*ElevationPattern, error) {
	const nb = geo.ElevBins
	p := &ElevationPattern{
		ElevDeg: make(Nums, nb), Count: make([]int64, nb), RSSI: nanSlice(nb), ResidDB: nanSlice(nb), RelDB: nanSlice(nb),
		PeakDeg: Num(math.NaN()), LowestDeg: Num(math.NaN()),
	}
	for i := range p.ElevDeg {
		p.ElevDeg[i] = geo.ElevBinCenter(i)
	}
	rows, err := a.st.ElevTotals(ctx, a.antKey(ant), d0, d1, a.pending().Elev)
	for _, r := range rows {
		b, n := r.ElevBin, r.Count
		if b < 0 || b >= nb || n <= 0 {
			continue
		}
		p.Count[b] = n
		p.RSSI[b] = r.RSSISum / float64(n)
		p.ResidDB[b] = r.ResidSum / float64(n)
		p.Total += n
	}
	if err != nil || p.Total == 0 {
		return p, err
	}
	minN := max(int64(20), p.Total/2000)
	sm := nanSlice(nb)
	peak := math.Inf(-1)
	for b := 0; b < nb; b++ {
		if p.Count[b] < minN {
			continue
		}
		var s, w float64
		for o := -1; o <= 1; o++ {
			if j := b + o; j >= 0 && j < nb && p.Count[j] >= minN {
				k := float64(p.Count[j]) * float64(2-o*o)
				s += k * p.ResidDB[j]
				w += k
			}
		}
		sm[b] = s / w
		if sm[b] > peak {
			peak, p.PeakDeg = sm[b], Num(p.ElevDeg[b])
		}
	}
	for b := range sm {
		if isNum(sm[b]) {
			p.RelDB[b] = sm[b] - peak
		}
	}
	p.LowestDeg = Num(stat.HistQuantile(p.Count, geo.ElevMinDeg, geo.ElevBinDeg, 0.005))
	return p, nil
}

// -------------------------------------------------------- antenna models

// PatternCompare compares a measured pattern with a modelled one (both
// centred on their means over the bearings with data).
type PatternCompare struct {
	RMSDB Num `json:"rmsDb"`
	R     Num `json:"r"` // correlation of the shapes
	N     int `json:"n"`
}

// AntennaModel is one configured antenna: its modelled patterns and, when
// antenna-specific statistics exist (one feed per antenna), its measured
// azimuth pattern.
type AntennaModel struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Type  string            `json:"type"`
	Color string            `json:"color,omitempty"`
	Info  radio.PatternInfo `json:"info"`
	// AzGainDBi is the modelled gain per bearing bin at 3 degrees elevation;
	// AzRelDB the same relative to its peak.
	AzGainDBi Nums `json:"azGainDbi"`
	AzRelDB   Nums `json:"azRelDb"`
	// ElGainDBi is the modelled gain per elevation bin (on boresight for a
	// directional antenna); ElRelDB relative to its peak.
	ElGainDBi Nums `json:"elGainDbi"`
	ElRelDB   Nums `json:"elRelDb"`

	Measured *AzimuthPattern `json:"measured,omitempty"`
	Compare  *PatternCompare `json:"compare,omitempty"`
	Fit      *YagiFit        `json:"fit,omitempty"` // directional antennas with their own data
}

func modelAntenna(an model.Antenna) AntennaModel {
	info := radio.Resolve(an)
	pat := info.Pattern()
	m := AntennaModel{ID: an.ID, Name: an.Name, Type: an.Type, Color: an.Color, Info: info,
		AzGainDBi: make(Nums, brgBins), AzRelDB: make(Nums, brgBins),
		ElGainDBi: make(Nums, geo.ElevBins), ElRelDB: make(Nums, geo.ElevBins)}
	if m.Info.Estimated == nil {
		m.Info.Estimated = []string{}
	}
	sanitize := func(v float64) float64 {
		if !isNum(v) {
			return -60
		}
		return math.Max(v, -60)
	}
	peak := math.Inf(-1)
	for b := range m.AzGainDBi {
		m.AzGainDBi[b] = sanitize(pat.GainDBi(geo.BearingBinCenter(b), azimuthRefElevDeg))
		peak = math.Max(peak, m.AzGainDBi[b])
	}
	for b := range m.AzRelDB {
		m.AzRelDB[b] = m.AzGainDBi[b] - peak
	}
	peak = math.Inf(-1)
	for b := range m.ElGainDBi {
		m.ElGainDBi[b] = sanitize(pat.GainDBi(info.AzimuthDeg, geo.ElevBinCenter(b)))
		peak = math.Max(peak, m.ElGainDBi[b])
	}
	for b := range m.ElRelDB {
		m.ElRelDB[b] = m.ElGainDBi[b] - peak
	}
	return m
}

func comparePattern(meas *AzimuthPattern, modelDB []float64) *PatternCompare {
	var x, y, w []float64
	for b := 0; b < brgBins; b++ {
		if isNum(meas.RelDB[b]) {
			x = append(x, meas.RelDB[b])
			y = append(y, modelDB[b])
			w = append(w, 1)
		}
	}
	if len(x) < 10 {
		return nil
	}
	mx, my := stat.WeightedMean(x, w), stat.WeightedMean(y, w)
	var ss float64
	for i := range x {
		d := (x[i] - mx) - (y[i] - my)
		ss += d * d
	}
	c := &PatternCompare{RMSDB: Num(math.Sqrt(ss / float64(len(x)))), R: Num(math.NaN()), N: len(x)}
	if r, err := stat.Pearson(x, y); err == nil {
		c.R = Num(r.R)
	}
	return c
}

// ------------------------------------------------------------ link budget

// LinkRow compares the predicted and the achieved range in one altitude band.
type LinkRow struct {
	Band        int     `json:"band"`
	Label       string  `json:"label"`
	AltFt       float64 `json:"altFt"`
	HorizonKm   Num     `json:"horizonKm"`   // smooth-earth line of sight at AltFt
	PredictedKm Num     `json:"predictedKm"` // link-budget maximum range (best antenna, on boresight)
	Limit       string  `json:"limit"`       // "horizon" | "power" | "none"
	MarginDB    Num     `json:"marginDb"`
	Antenna     string  `json:"antenna"` // antenna giving the prediction
	// Achieved: the absolute maximum, the best bearing of the robust
	// envelope, and the median over all bearings of the robust envelope.
	AchievedMaxKm     Num `json:"achievedMaxKm"`
	AchievedRobustKm  Num `json:"achievedRobustKm"`
	AchievedTypicalKm Num `json:"achievedTypicalKm"`
	PctOfPredicted    Num `json:"pctOfPredicted"` // AchievedRobustKm / PredictedKm
}

func linkBudget(cfg model.Config, s site, bands []PolarBand) []LinkRow {
	rows := make([]LinkRow, altBands)
	ants := s.ants
	if s.ant != nil {
		ants = []model.Antenna{*s.ant}
	}
	for b := range rows {
		rows[b] = LinkRow{Band: b, Label: geo.AltBandLabel(b), AltFt: geo.AltBandTypicalFt(b), Limit: radio.LimitNone,
			HorizonKm:   Num(radio.LineOfSightFtKm(s.antM, geo.AltBandTypicalFt(b), 0)),
			PredictedKm: Num(math.NaN()), MarginDB: Num(math.NaN()),
			AchievedMaxKm: Num(math.NaN()), AchievedRobustKm: Num(math.NaN()), AchievedTypicalKm: Num(math.NaN()),
			PctOfPredicted: Num(math.NaN())}
	}
	for _, an := range ants {
		l := radio.NewLink(an, radio.AntennaHeightAMSL(cfg.Receiver, an), radio.LinkParams{})
		tab := l.RangeTable(l.Info.AzimuthDeg)
		for b := range rows {
			if b < len(tab) && isNum(tab[b].RangeKm) && !(float64(rows[b].PredictedKm) >= tab[b].RangeKm) {
				rows[b].PredictedKm, rows[b].Limit, rows[b].Antenna = Num(tab[b].RangeKm), tab[b].Limit, an.ID
				rows[b].MarginDB = Num(tab[b].MarginDB)
				rows[b].HorizonKm = Num(tab[b].HorizonKm)
			}
		}
	}
	for b := range rows {
		if b >= len(bands) || bands[b].Bearings == 0 {
			continue
		}
		rows[b].AchievedMaxKm = bands[b].BestKm
		rows[b].AchievedRobustKm = Num(stat.Max(bands[b].RobustKm))
		rows[b].AchievedTypicalKm = Num(stat.Median(bands[b].RobustKm))
		if p := float64(rows[b].PredictedKm); p > 0 {
			rows[b].PctOfPredicted = Num(100 * float64(rows[b].AchievedRobustKm) / p)
		}
	}
	return rows
}

// ----------------------------------------------------------------- report

// AntennaReport is the result of Analyzer.Antenna.
type AntennaReport struct {
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Ant     string `json:"ant"`
	Located bool   `json:"located"`
	// Signal is the RSSI-vs-distance histogram with the path-loss fit.
	Signal *SignalDistance `json:"signal"`
	// Azimuth is the measured azimuth gain pattern of the selected antenna
	// ("" = everything the receiver heard); nil until there is enough data.
	Azimuth   *AzimuthPattern   `json:"azimuth"`
	Elevation *ElevationPattern `json:"elevation"`
	// Antennas are the configured antennas with their modelled patterns.
	Antennas []AntennaModel `json:"antennas"`
	// Yagi is the directional lobe inferred from Azimuth.
	Yagi       *YagiFit  `json:"yagi"`
	LinkBudget []LinkRow `json:"linkBudget"`
	Notes      []string  `json:"notes"`

	gen uint64 // computation stamp (see Analyzer.nextGen)
}

// Antenna returns the antenna performance analysis: RSSI vs distance with
// the fitted path-loss law, the measured azimuth and elevation patterns
// next to the modelled patterns of every configured antenna, the yagi lobe
// inferred from the combined data and the link-budget table. The histogram
// tables honour the range (default 30 days); the azimuth pattern uses the
// all-time heat grid.
func (a *Analyzer) Antenna(ctx context.Context, r Range, ant string) (*AntennaReport, error) {
	now, _ := a.clock()
	from, to, d0, d1 := dayRange(r, now.Unix(), 30)
	cfg := a.cfg()
	return cached(ctx, a, a.key("antenna", ant, cfg, int64(d0), int64(d1)), ttlAntenna, func(ctx context.Context) (*AntennaReport, error) {
		s := siteOf(cfg, ant)
		res := &AntennaReport{From: from * 1000, To: to * 1000, Ant: ant, Located: s.located, Antennas: []AntennaModel{}, Notes: []string{}}
		var err error
		if res.Signal, err = a.signalDistance(ctx, ant, d0, d1); err != nil {
			return nil, err
		}
		if res.Elevation, err = a.elevationPattern(ctx, ant, d0, d1); err != nil {
			return nil, err
		}
		cd, err := a.loadCoverage(ctx, ant, d0, d1)
		if err != nil {
			return nil, err
		}
		res.Azimuth, err = a.measuredAzimuth(ctx, s, ant, cd)
		if err != nil {
			return nil, err
		}
		if res.Azimuth == nil {
			res.Notes = append(res.Notes, "Not enough positions with signal levels yet for a measured azimuth pattern.")
		} else if res.Azimuth.Method != "heat-residual" {
			res.Notes = append(res.Notes, "The receiver position is not set: the azimuth pattern is a rough estimate from mean signal levels. Set the position for a proper path-loss-corrected pattern.")
		}
		res.Yagi = fitLobe(res.Azimuth)
		if ant == "" {
			compareYagi(res.Yagi, cfg.Antennas)
		} else if s.ant != nil {
			compareYagi(res.Yagi, []model.Antenna{*s.ant})
		}
		for _, an := range cfg.Antennas {
			m := modelAntenna(an)
			if an.ID != "" && an.ID != ant {
				// Antenna-specific rows exist when a feed is attributed to it.
				if cells, err := a.loadHeat(ctx, an.ID); err != nil {
					return nil, err
				} else if len(cells) > 0 {
					m.Measured = measureAzimuthHeat(siteOf(cfg, an.ID), cells)
				}
			} else if an.ID == ant {
				m.Measured = res.Azimuth
			}
			if m.Measured != nil {
				m.Compare = comparePattern(m.Measured, m.AzGainDBi)
				if m.Info.Directional {
					m.Fit = fitLobe(m.Measured)
					compareYagi(m.Fit, []model.Antenna{an})
				}
			}
			res.Antennas = append(res.Antennas, m)
		}
		table := radio.HorizonTable(s.antM, 0)
		bands := make([]PolarBand, altBands)
		for b := range bands {
			bands[b] = polarBand(cd, b, geo.AltBandLabel(b), geo.AltBandTypicalFt(b), table[b].LOSHiKm, table[b].LOSKm)
		}
		res.LinkBudget = linkBudget(cfg, s, bands)
		res.gen = a.nextGen()
		return res, nil
	})
}

// measuredAzimuth picks the best available method.
func (a *Analyzer) measuredAzimuth(ctx context.Context, s site, ant string, cd *covData) (*AzimuthPattern, error) {
	if s.located {
		cells, err := a.loadHeat(ctx, ant)
		if err != nil {
			return nil, err
		}
		if p := measureAzimuthHeat(s, cells); p != nil && p.Bearings >= 20 {
			return p, nil
		}
	}
	return measureAzimuthCoverage(cd), nil
}
