package met

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"airview/internal/radio"
	"airview/internal/store"
)

// Level is one 1000 ft layer of a vertical profile. Absent values are nil
// (never NaN), so the type marshals safely.
type Level struct {
	Level       int      `json:"level"`                // layer index: covers [level*1000, level*1000+1000) ft
	AltFt       float64  `json:"altFt"`                // mean pressure altitude of the samples (layer centre when unknown)
	PressureHPa float64  `json:"hpa"`                  // standard-atmosphere pressure at AltFt
	WindDir     *float64 `json:"windDir,omitempty"`    // vector-mean wind, degrees true FROM
	WindKt      *float64 `json:"windKt,omitempty"`     //
	U           *float64 `json:"u,omitempty"`          // eastward component, kt
	V           *float64 `json:"v,omitempty"`          // northward component, kt
	TempC       *float64 `json:"tempC,omitempty"`      // mean static air temperature
	ISADevC     *float64 `json:"isaDev,omitempty"`     // TempC minus the standard atmosphere
	NWind       int      `json:"nWind"`                //
	NTemp       int      `json:"nTemp"`                //
	WindSpread  *float64 `json:"windSpread,omitempty"` // RMS vector deviation from the mean, kt
	TempSpread  *float64 `json:"tempSpread,omitempty"` // standard deviation, degC
}

// Profile is a vertical profile above the station with its analysis.
type Profile struct {
	T         int64   `json:"t"`         // Unix ms the profile is valid for (end of the window)
	From      int64   `json:"from"`      // Unix ms, start of the averaging window
	WindowMin float64 `json:"windowMin"` // length of the window, minutes
	NWind     int     `json:"nWind"`     // observations in the window (all levels)
	NTemp     int     `json:"nTemp"`
	Levels    []Level `json:"levels"` // ascending, only levels with data
	Analysis
}

// Analysis is everything derived from the levels of one profile.
type Analysis struct {
	FreezingLevelFt *float64     `json:"freezingLevelFt,omitempty"` // lowest 0 degC crossing, pressure altitude
	Tropopause      *Tropopause  `json:"tropopause,omitempty"`
	MaxWind         *MaxWind     `json:"maxWind,omitempty"`  // jet-stream maximum
	MaxShear        *Shear       `json:"maxShear,omitempty"` // strongest vertical wind shear
	Lapse           []LapseLayer `json:"lapse"`              // layer by layer
	Inversions      []Inversion  `json:"inversions"`         // temperature inversions below the tropopause
	WindsAloft      []WindAloft  `json:"windsAloft"`         // at the standard flight levels
}

// Tropopause is the thermal (WMO lapse-rate) tropopause estimate.
type Tropopause struct {
	AltFt float64 `json:"altFt"`
	TempC float64 `json:"tempC"`
	// Confirmed: the profile reaches 2 km above, so the WMO criterion
	// (mean lapse rate <= 2 K/km over the next 2 km) was checked in full.
	// Otherwise it held over the available depth only.
	Confirmed bool    `json:"confirmed"`
	DepthFt   float64 `json:"depthFt"` // depth of data above that the criterion was checked over
}

// MaxWind is the level of the strongest vector-mean wind.
type MaxWind struct {
	AltFt   float64 `json:"altFt"`
	WindKt  float64 `json:"windKt"`
	WindDir float64 `json:"windDir"`
	Jet     bool    `json:"jet"` // >= 60 kt at or above 20 000 ft: a jet stream
}

// Shear is a vertical wind shear between two adjacent levels.
type Shear struct {
	BaseFt      float64 `json:"baseFt"`
	TopFt       float64 `json:"topFt"`
	KtPer1000Ft float64 `json:"ktPer1000Ft"` // magnitude of the vector difference per 1000 ft
}

// Lapse-rate classes.
const (
	LapseInversion   = "inversion"   // temperature rises with height
	LapseIsothermal  = "isothermal"  // within +-0.5 K/km
	LapseStable      = "stable"      // below the moist adiabat
	LapseConditional = "conditional" // between the moist and the dry adiabat
	LapseUnstable    = "unstable"    // steeper than the dry adiabat (9.8 K/km)
)

// LapseLayer is the temperature gradient between two adjacent levels.
type LapseLayer struct {
	BaseFt      float64 `json:"baseFt"`
	TopFt       float64 `json:"topFt"`
	LapseKPerKm float64 `json:"lapse"` // positive = cooling with height; geometric height
	Class       string  `json:"class"`
}

// Inversion is a layer in which temperature rises with height.
type Inversion struct {
	BaseFt     float64 `json:"baseFt"` // pressure altitude
	TopFt      float64 `json:"topFt"`
	BaseTempC  float64 `json:"baseTempC"`
	TopTempC   float64 `json:"topTempC"`
	StrengthC  float64 `json:"strengthC"`  // TopTempC - BaseTempC
	ThicknessM float64 `json:"thicknessM"` // geometric
	GradKPerKm float64 `json:"grad"`       // dT/dz, positive
}

// WindAloft is one row of the winds-aloft table.
type WindAloft struct {
	FL      int      `json:"fl"`
	AltFt   float64  `json:"altFt"`
	WindDir *float64 `json:"windDir,omitempty"`
	WindKt  *float64 `json:"windKt,omitempty"`
	TempC   *float64 `json:"tempC,omitempty"`
	ISADevC *float64 `json:"isaDev,omitempty"`
	// Coded is the classic winds-aloft (FD/FB) group "DDSS+TT", e.g.
	// "2745-32" = from 270 degrees at 45 kt, -32 degC ("9900" = light and
	// variable; speeds of 100 kt and more add 50 to the direction digits).
	Coded string `json:"coded,omitempty"`
}

// StandardFlightLevels are the levels of the winds-aloft table.
var StandardFlightLevels = []int{30, 60, 90, 120, 180, 240, 300, 340, 390, 450}

// point is the working form of a Level: NaN = missing.
type point struct {
	level       int
	alt         float64
	u, v, t     float64
	nw, nt      int
	wsd, tsd    float64 // spreads, NaN unknown
	isaT, press float64
}

func fp(v float64) *float64 {
	if !finite(v) {
		return nil
	}
	return &v
}

func fp1(v float64) *float64 {
	if !finite(v) {
		return nil
	}
	v = round1(v)
	return &v
}

func pv(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func (p *point) level1() Level {
	l := Level{Level: p.level, AltFt: math.Round(p.alt), PressureHPa: round1(p.press), NWind: p.nw, NTemp: p.nt}
	if finite(p.u) && finite(p.v) {
		spd := math.Hypot(p.u, p.v)
		dir := 0.0
		if spd > 0 {
			dir = norm360(math.Atan2(-p.u, -p.v) * 180 / math.Pi)
		}
		l.WindDir, l.WindKt = fp1(dir), fp1(spd)
		l.U, l.V = fp(round2(p.u)), fp(round2(p.v))
		l.WindSpread = fp1(p.wsd)
	}
	if finite(p.t) {
		l.TempC = fp(round2(p.t))
		l.ISADevC = fp(round2(p.t - p.isaT))
		l.TempSpread = fp(round2(p.tsd))
	}
	return l
}

func newPoint(level int, alt float64) point {
	nan := math.NaN()
	isa := radio.ISAFt(alt)
	return point{level: level, alt: alt, u: nan, v: nan, t: nan, wsd: nan, tsd: nan, isaT: isa.TempC, press: isa.PressureHPa}
}

// pointsOf converts public levels back to working points (ascending).
func pointsOf(levels []Level) []point {
	pts := make([]point, 0, len(levels))
	for i := range levels {
		l := &levels[i]
		if !finite(l.AltFt) {
			continue
		}
		p := newPoint(l.Level, l.AltFt)
		p.nw, p.nt = l.NWind, l.NTemp
		p.u, p.v = pv(l.U), pv(l.V)
		if !finite(p.u) || !finite(p.v) {
			p.u, p.v = math.NaN(), math.NaN()
			if l.WindDir != nil && l.WindKt != nil {
				s, c := math.Sincos(*l.WindDir * math.Pi / 180)
				p.u, p.v = -*l.WindKt*s, -*l.WindKt*c
			}
		}
		p.t = pv(l.TempC)
		p.wsd, p.tsd = pv(l.WindSpread), pv(l.TempSpread)
		pts = append(pts, p)
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].alt < pts[j].alt })
	return pts
}

// Current returns the profile of the last Options.Window ending at now
// (zero = the time of the newest observation, or the wall clock when there
// is none).
func (c *Collector) Current(now time.Time) Profile {
	c.mu.Lock()
	if now.IsZero() {
		if c.newest != 0 {
			now = time.UnixMilli(c.newest)
		} else {
			now = time.Now()
		}
	}
	nowSlot := now.Unix() / slotSec
	lo := c.windowSlots(nowSlot)
	var sum [nLevels]cell
	for i := range c.slots {
		s := &c.slots[i]
		if s.id < lo || s.id > nowSlot {
			continue
		}
		for l := range s.cells {
			a, b := &sum[l], &s.cells[l]
			if b.na == 0 {
				continue
			}
			a.u += b.u
			a.v += b.v
			a.t += b.t
			a.uu += b.uu
			a.vv += b.vv
			a.tt += b.tt
			a.alt += b.alt
			a.nw += b.nw
			a.nt += b.nt
			a.na += b.na
		}
	}
	minN := int32(c.opt.MinSamples)
	c.mu.Unlock()

	p := Profile{T: now.UnixMilli(), From: lo * slotSec * 1000}
	p.WindowMin = math.Round(float64(p.T-p.From)/6000) / 10
	pts := make([]point, 0, 48)
	for l := range sum {
		s := &sum[l]
		p.NWind += int(s.nw)
		p.NTemp += int(s.nt)
		if s.na == 0 || (s.nw < minN && s.nt < minN) {
			continue
		}
		pt := newPoint(l+minLevel, s.alt/float64(s.na))
		pt.nw, pt.nt = int(s.nw), int(s.nt)
		if s.nw >= minN {
			n := float64(s.nw)
			pt.u, pt.v = s.u/n, s.v/n
			if s.nw >= 2 {
				// RMS vector deviation = sqrt(var u + var v).
				vr := (s.uu-n*pt.u*pt.u)/(n-1) + (s.vv-n*pt.v*pt.v)/(n-1)
				pt.wsd = math.Sqrt(math.Max(vr, 0))
			}
		}
		if s.nt >= minN {
			n := float64(s.nt)
			pt.t = s.t / n
			if s.nt >= 2 {
				pt.tsd = math.Sqrt(math.Max((s.tt-n*pt.t*pt.t)/(n-1), 0))
			}
		}
		pts = append(pts, pt)
	}
	p.Levels = make([]Level, len(pts))
	for i := range pts {
		p.Levels[i] = pts[i].level1()
	}
	p.Analysis = analyze(pts)
	return p
}

// Analyze derives freezing level, tropopause, maximum wind, lapse rates,
// inversions and the winds-aloft table from profile levels (any order).
func Analyze(levels []Level) Analysis { return analyze(pointsOf(levels)) }

// geomFactor converts a pressure-altitude difference into a geometric
// thickness: the real layer is thicker than the standard one in proportion
// to its absolute temperature (hypsometric equation).
func geomFactor(a, b *point) float64 {
	ta, tb := a.t, b.t
	if !finite(ta) || !finite(tb) {
		return 1
	}
	return ((ta+tb)/2 + 273.15) / ((a.isaT+b.isaT)/2 + 273.15)
}

func classifyLapse(g float64) string {
	switch {
	case g < -0.5:
		return LapseInversion
	case g <= 0.5:
		return LapseIsothermal
	case g < 5.5:
		return LapseStable
	case g <= 9.8:
		return LapseConditional
	}
	return LapseUnstable
}

// stderrT is the standard error of a level's mean temperature. A single
// Mach/TAS temperature is uncertain by about 1.5 K (quantisation).
func stderrT(p *point) float64 {
	sd := p.tsd
	if !finite(sd) || sd < 0.5 {
		sd = 1.5
	}
	return sd / math.Sqrt(float64(max(p.nt, 1)))
}

func analyze(pts []point) Analysis {
	a := Analysis{Lapse: []LapseLayer{}, Inversions: []Inversion{}, WindsAloft: []WindAloft{}}

	var tp []*point
	for i := range pts {
		if finite(pts[i].t) {
			tp = append(tp, &pts[i])
		}
	}

	// Lapse rates between adjacent levels.
	for i := 0; i+1 < len(tp); i++ {
		lo, hi := tp[i], tp[i+1]
		dz := (hi.alt - lo.alt) * 0.3048 * geomFactor(lo, hi) / 1000 // km
		if dz < 0.1 || hi.alt-lo.alt > 4500 {
			continue
		}
		g := (lo.t - hi.t) / dz
		a.Lapse = append(a.Lapse, LapseLayer{BaseFt: math.Round(lo.alt), TopFt: math.Round(hi.alt),
			LapseKPerKm: round2(g), Class: classifyLapse(g)})
	}

	// Freezing level: lowest crossing from above 0 degC to 0 or below.
	for i := 0; i+1 < len(tp); i++ {
		lo, hi := tp[i], tp[i+1]
		if lo.t > 0 && hi.t <= 0 && hi.alt-lo.alt <= 4500 {
			f := lo.alt + (hi.alt-lo.alt)*lo.t/(lo.t-hi.t)
			a.FreezingLevelFt = fp(math.Round(f))
			break
		}
	}

	// Tropopause (WMO): the lowest level at which the lapse rate falls to
	// 2 K/km or less, provided the mean lapse rate between it and every
	// level within 2 km above does not exceed 2 K/km. The second condition
	// is what keeps single-level noise from faking a tropopause.
	tropoAlt := math.Inf(1)
	for i := 0; i+2 < len(tp); i++ {
		p := tp[i]
		if p.alt < 20000 {
			continue
		}
		ok, n := true, 0
		depth := 0.0
		for j := i + 1; j < len(tp); j++ {
			q := tp[j]
			dz := (q.alt - p.alt) * 0.3048 * geomFactor(p, q) / 1000
			if dz > 2.05 {
				break
			}
			if j == i+1 && q.alt-p.alt > 2500 {
				ok = false
				break
			}
			if (p.t-q.t)/dz > 2 {
				ok = false
				break
			}
			n++
			depth = q.alt - p.alt
		}
		if !ok || n < 2 {
			continue
		}
		tropoAlt = p.alt
		a.Tropopause = &Tropopause{AltFt: math.Round(p.alt), TempC: round1(p.t), Confirmed: depth >= 6000, DepthFt: math.Round(depth)}
		break
	}

	// Inversions below the tropopause: runs of layers in which the
	// temperature rises, kept when the rise is statistically significant.
	for i := 0; i+1 < len(tp); {
		lo := tp[i]
		j := i
		for j+1 < len(tp) && tp[j+1].t > tp[j].t && tp[j+1].alt-tp[j].alt <= 2500 && tp[j+1].alt <= tropoAlt {
			j++
		}
		if j == i {
			i++
			continue
		}
		hi := tp[j]
		rise := hi.t - lo.t
		if rise >= math.Max(0.8, 2*math.Hypot(stderrT(lo), stderrT(hi))) {
			dz := (hi.alt - lo.alt) * 0.3048 * geomFactor(lo, hi)
			a.Inversions = append(a.Inversions, Inversion{BaseFt: math.Round(lo.alt), TopFt: math.Round(hi.alt),
				BaseTempC: round1(lo.t), TopTempC: round1(hi.t), StrengthC: round1(rise),
				ThicknessM: math.Round(dz), GradKPerKm: round2(rise / dz * 1000)})
		}
		i = j
	}

	// Maximum wind and strongest shear.
	var wp []*point
	for i := range pts {
		if finite(pts[i].u) && finite(pts[i].v) {
			wp = append(wp, &pts[i])
		}
	}
	best := -1.0
	for _, p := range wp {
		if s := math.Hypot(p.u, p.v); s > best {
			best = s
			dir := 0.0
			if s > 0 {
				dir = norm360(math.Atan2(-p.u, -p.v) * 180 / math.Pi)
			}
			a.MaxWind = &MaxWind{AltFt: math.Round(p.alt), WindKt: round1(s), WindDir: round1(dir), Jet: s >= 60 && p.alt >= 20000}
		}
	}
	bestShear := 0.0
	for i := 0; i+1 < len(wp); i++ {
		lo, hi := wp[i], wp[i+1]
		dh := hi.alt - lo.alt
		if dh < 500 || dh > 2500 || lo.nw < 5 || hi.nw < 5 {
			continue
		}
		if s := math.Hypot(hi.u-lo.u, hi.v-lo.v) / dh * 1000; s > bestShear {
			bestShear = s
			a.MaxShear = &Shear{BaseFt: math.Round(lo.alt), TopFt: math.Round(hi.alt), KtPer1000Ft: round1(s)}
		}
	}

	// Winds aloft at the standard levels.
	for _, fl := range StandardFlightLevels {
		alt := float64(fl) * 100
		u, v, okW := sampleWind(pts, alt)
		t, okT := sampleTemp(pts, alt)
		if !okW && !okT {
			continue
		}
		w := WindAloft{FL: fl, AltFt: alt}
		spd, dir := math.NaN(), math.NaN()
		if okW {
			spd = math.Hypot(u, v)
			dir = 0
			if spd > 0 {
				dir = norm360(math.Atan2(-u, -v) * 180 / math.Pi)
			}
			w.WindDir, w.WindKt = fp(math.Round(dir)), fp1(spd)
		}
		if okT {
			w.TempC = fp1(t)
			w.ISADevC = fp1(t - radio.ISAFt(alt).TempC)
		} else {
			t = math.NaN()
		}
		w.Coded = codeWindAloft(dir, spd, t, fl)
		a.WindsAloft = append(a.WindsAloft, w)
	}
	return a
}

// codeWindAloft formats an FD/FB winds-aloft group.
func codeWindAloft(dir, spd, t float64, fl int) string {
	s := "////"
	if finite(dir) && finite(spd) {
		dd := int(math.Round(dir/10)) % 36
		if dd == 0 {
			dd = 36
		}
		kt := int(math.Round(spd))
		switch {
		case kt < 5:
			s = "9900"
		case kt >= 199:
			s = fmt.Sprintf("%02d99", dd+50)
		case kt >= 100:
			s = fmt.Sprintf("%02d%02d", dd+50, kt-100)
		default:
			s = fmt.Sprintf("%02d%02d", dd, kt)
		}
	}
	if finite(t) {
		ti := int(math.Round(t))
		if fl > 240 && ti <= 0 {
			s += fmt.Sprintf("%02d", -ti) // always negative up there: sign omitted
		} else {
			s += fmt.Sprintf("%+03d", ti)
		}
	}
	return s
}

// sample interpolates a quantity linearly in pressure altitude between the
// nearest levels that have it. It never extrapolates beyond 700 ft and
// never bridges gaps wider than 4500 ft.
func sample(pts []point, alt float64, val func(*point) float64) (float64, bool) {
	var lo, hi *point
	for i := range pts {
		p := &pts[i]
		if !finite(val(p)) {
			continue
		}
		if p.alt <= alt {
			lo = p
		} else {
			hi = p
			break
		}
	}
	switch {
	case lo != nil && hi != nil && hi.alt-lo.alt <= 4500:
		f := (alt - lo.alt) / (hi.alt - lo.alt)
		return val(lo) + f*(val(hi)-val(lo)), true
	case lo != nil && alt-lo.alt <= 700:
		return val(lo), true
	case hi != nil && hi.alt-alt <= 700:
		return val(hi), true
	}
	return 0, false
}

func sampleTemp(pts []point, alt float64) (float64, bool) {
	return sample(pts, alt, func(p *point) float64 { return p.t })
}

func sampleWind(pts []point, alt float64) (u, v float64, ok bool) {
	u, ok1 := sample(pts, alt, func(p *point) float64 {
		if !finite(p.v) {
			return math.NaN()
		}
		return p.u
	})
	v, ok2 := sample(pts, alt, func(p *point) float64 {
		if !finite(p.u) {
			return math.NaN()
		}
		return p.v
	})
	return u, v, ok1 && ok2
}

// ---------------------------------------------------------------- history

// HistCell is one hour x level cell of the time-height history.
type HistCell struct {
	T       int64    `json:"t"`     // Unix ms, start of the hour
	Level   int      `json:"level"` // 1000 ft layer
	AltFt   float64  `json:"altFt"` // layer centre
	WindDir *float64 `json:"windDir,omitempty"`
	WindKt  *float64 `json:"windKt,omitempty"`
	TempC   *float64 `json:"tempC,omitempty"`
	ISADevC *float64 `json:"isaDev,omitempty"`
	NWind   int      `json:"nWind"`
	NTemp   int      `json:"nTemp"`
}

// TimeHeight is the hourly time-height history of the profiles.
type TimeHeight struct {
	From  int64      `json:"from"`  // Unix ms
	To    int64      `json:"to"`    // Unix ms (exclusive)
	Hours []int64    `json:"hours"` // Unix ms of the hours that have data, ascending
	Cells []HistCell `json:"cells"` // ordered by hour, then level
}

type histRow struct {
	hour    int64
	idx     int
	u, v, t float64
	nw, nt  int64
}

// historyRows collects the hourly sums for [from, to): the in-memory hours
// (which include increments not yet on disk) and, for older hours, the
// met_profile table.
func (c *Collector) historyRows(ctx context.Context, from, to int64) ([]histRow, error) {
	from = from / hourSec * hourSec
	var rows []histRow
	c.mu.Lock()
	memMin := int64(math.MaxInt64)
	for h := range c.hours {
		memMin = min(memMin, h)
	}
	for h, hp := range c.hours {
		if h < from || h >= to {
			continue
		}
		for i := range hp {
			if hp[i].nw != 0 || hp[i].nt != 0 {
				rows = append(rows, histRow{h, i, hp[i].u, hp[i].v, hp[i].t, int64(hp[i].nw), int64(hp[i].nt)})
			}
		}
	}
	loaded := c.loaded
	c.mu.Unlock()

	if c.st != nil && (from < memMin || !loaded) {
		// Without Load the memory holds only this run's increments, so the
		// table is authoritative for everything already flushed; with it,
		// only hours older than the in-memory range come from disk.
		upper := min(to, memMin)
		if !loaded {
			upper = to
			rows = rows[:0]
			// The increments not handed to the store yet (the open hour:
			// they are written once the hour has closed) complete the
			// table.
			c.mu.Lock()
			for _, p := range c.pend {
				idx := p.Level - minLevel
				if p.Hour < from || p.Hour >= to || idx < 0 || idx >= nLevels {
					continue
				}
				rows = append(rows, histRow{p.Hour, idx, p.USum, p.VSum, p.TSum, p.NWind, p.NTemp})
			}
			c.mu.Unlock()
		}
		rs, err := c.st.DB().QueryContext(ctx,
			`SELECT hour, level, u_sum, v_sum, t_sum, n_wind, n_temp FROM met_profile WHERE hour >= ? AND hour < ?`, from, upper)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		for rs.Next() {
			var r histRow
			var level int
			if err := rs.Scan(&r.hour, &level, &r.u, &r.v, &r.t, &r.nw, &r.nt); err != nil {
				return nil, err
			}
			r.idx = level - minLevel
			if r.idx < 0 || r.idx >= nLevels {
				continue
			}
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			return nil, err
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].hour != rows[j].hour {
			return rows[i].hour < rows[j].hour
		}
		return rows[i].idx < rows[j].idx
	})
	// A (hour, level) can come both from the table and from the pending
	// increments of the open hour: one row each.
	out := rows[:0]
	for _, r := range rows {
		if n := len(out); n > 0 && out[n-1].hour == r.hour && out[n-1].idx == r.idx {
			p := &out[n-1]
			p.u, p.v, p.t, p.nw, p.nt = p.u+r.u, p.v+r.v, p.t+r.t, p.nw+r.nw, p.nt+r.nt
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (r *histRow) point(minN int) (point, bool) {
	// Hourly sums carry no altitude: use the layer centre.
	p := newPoint(r.idx+minLevel, float64(r.idx+minLevel)*LevelFt+LevelFt/2)
	p.nw, p.nt = int(r.nw), int(r.nt)
	if p.nw >= minN {
		p.u, p.v = r.u/float64(r.nw), r.v/float64(r.nw)
	}
	if p.nt >= minN {
		p.t = r.t / float64(r.nt)
	}
	return p, p.nw >= minN || p.nt >= minN
}

// History returns the hourly time-height cross-section for [from, to).
// Hours still in memory include observations not yet flushed to disk.
func (c *Collector) History(ctx context.Context, from, to time.Time) (TimeHeight, error) {
	th := TimeHeight{From: from.UnixMilli(), To: to.UnixMilli(), Hours: []int64{}, Cells: []HistCell{}}
	rows, err := c.historyRows(ctx, from.Unix(), to.Unix())
	if err != nil {
		return th, err
	}
	last := int64(-1)
	for i := range rows {
		r := &rows[i]
		p, ok := r.point(c.opt.MinSamples)
		if !ok {
			continue
		}
		if r.hour != last {
			last = r.hour
			th.Hours = append(th.Hours, r.hour*1000)
		}
		l := p.level1()
		th.Cells = append(th.Cells, HistCell{T: r.hour * 1000, Level: l.Level, AltFt: l.AltFt,
			WindDir: l.WindDir, WindKt: l.WindKt, TempC: l.TempC, ISADevC: l.ISADevC, NWind: l.NWind, NTemp: l.NTemp})
	}
	return th, nil
}

// HourProfile returns the analysed profile of one clock hour (the hour
// containing t) from the hourly history. Level altitudes are layer centres.
func (c *Collector) HourProfile(ctx context.Context, t time.Time) (Profile, error) {
	hour := t.Unix() / hourSec * hourSec
	p := Profile{T: (hour + hourSec) * 1000, From: hour * 1000, WindowMin: 60, Levels: []Level{}}
	rows, err := c.historyRows(ctx, hour, hour+hourSec)
	if err != nil {
		return p, err
	}
	pts := make([]point, 0, len(rows))
	for i := range rows {
		p.NWind += int(rows[i].nw)
		p.NTemp += int(rows[i].nt)
		if pt, ok := rows[i].point(c.opt.MinSamples); ok {
			pts = append(pts, pt)
		}
	}
	for i := range pts {
		p.Levels = append(p.Levels, pts[i].level1())
	}
	p.Analysis = analyze(pts)
	return p, nil
}

// ------------------------------------------------------------ map layer

// Obs is one recent observation for the map layer.
type Obs struct {
	T       int64    `json:"t"` // Unix ms
	ICAO    string   `json:"icao"`
	Lat     float64  `json:"lat"`
	Lon     float64  `json:"lon"`
	AltFt   float64  `json:"altFt"`
	WindDir *float64 `json:"windDir,omitempty"`
	WindKt  *float64 `json:"windKt,omitempty"`
	OAT     *float64 `json:"oat,omitempty"`
	Quality float64  `json:"q"`
	MRAR    bool     `json:"mrar,omitempty"` // contains a direct BDS 4,4 value
}

// Latest returns the observations of the last maxAge (default 15 min)
// before now (zero = newest observation), newest first, at most limit
// (default 1000). With perAircraft only the newest of each aircraft is
// returned.
func (c *Collector) Latest(now time.Time, maxAge time.Duration, limit int, perAircraft bool) []Obs {
	if maxAge <= 0 {
		maxAge = 15 * time.Minute
	}
	if limit <= 0 {
		limit = 1000
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	nowMs := now.UnixMilli()
	if now.IsZero() {
		nowMs = c.newest
	}
	cut := nowMs - maxAge.Milliseconds()
	out := []Obs{}
	var seen map[uint32]struct{}
	if perAircraft {
		seen = make(map[uint32]struct{}, 128)
	}
	n := min(c.ringN, ringSize)
	for k := 1; k <= n && len(out) < limit; k++ {
		r := &c.ring[(c.ringN-k)%ringSize]
		if r.t < cut || r.t > nowMs+60000 {
			continue
		}
		if perAircraft {
			if _, dup := seen[r.icao]; dup {
				continue
			}
			seen[r.icao] = struct{}{}
		}
		out = append(out, Obs{T: r.t, ICAO: store.HexICAO(r.icao),
			Lat: math.Round(float64(r.lat)*1e4) / 1e4, Lon: math.Round(float64(r.lon)*1e4) / 1e4,
			AltFt:   math.Round(float64(r.alt)),
			WindDir: fp1(float64(r.dir)), WindKt: fp1(float64(r.spd)), OAT: fp1(float64(r.oat)),
			Quality: round2(float64(r.q)), MRAR: r.src != 0})
	}
	return out
}
