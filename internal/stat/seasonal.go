package stat

import "math"

// Season lengths of the two hourly cycles used throughout AirView.
const (
	HoursPerDay  = 24
	HoursPerWeek = 168
)

// floorDiv returns floor(a/b) for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

// HourOfDay returns the local hour 0..23 of a Unix time (seconds) for a zone
// utcOffsetSec seconds east of UTC.
func HourOfDay(unixSec int64, utcOffsetSec int) int {
	h := floorDiv(unixSec+int64(utcOffsetSec), 3600)
	return int(h - 24*floorDiv(h, 24))
}

// HourOfWeek returns the local hour of the week 0..167, where 0 is Monday
// 00:00-01:00 and 167 is Sunday 23:00-24:00.
func HourOfWeek(unixSec int64, utcOffsetSec int) int {
	// Hour 0 of the Unix epoch was a Thursday 00:00 = slot 72.
	h := floorDiv(unixSec+int64(utcOffsetSec), 3600) + 72
	return int(h - 168*floorDiv(h, 168))
}

// ProfileOptions configures FitSeasonalProfile.
type ProfileOptions struct {
	// HalfLife is the age, in samples, at which an observation's weight has
	// halved (exponential forgetting; the newest sample has age 0).
	// 0 or negative: all observations weigh the same.
	HalfLife float64
	// Mean selects the weighted mean per slot. The default (false) is the
	// weighted median, which ignores outliers such as a feed outage.
	Mean bool
}

// SeasonalProfile is a typical value per phase of a cycle, e.g. per hour of
// the week (Period 168) or hour of the day (Period 24). A slot is defined
// when Weight[slot] > 0. The struct is JSON-safe.
type SeasonalProfile struct {
	Period int       `json:"period"`
	Level  []float64 `json:"level"`  // typical value per slot (0 where undefined)
	Spread []float64 `json:"spread"` // robust sigma per slot (weighted MAD * MADScale, or weighted std with Mean)
	Weight []float64 `json:"weight"` // effective weight per slot (sum of observation weights)
	Count  []int     `json:"count"`  // observations per slot
}

// NewSeasonalProfile returns an empty profile for online use with Update.
// A period < 1 is treated as 1.
func NewSeasonalProfile(period int) *SeasonalProfile {
	if period < 1 {
		period = 1
	}
	return &SeasonalProfile{
		Period: period,
		Level:  make([]float64, period),
		Spread: make([]float64, period),
		Weight: make([]float64, period),
		Count:  make([]int, period),
	}
}

// FitSeasonalProfile builds a profile from a regularly sampled series: y[i]
// belongs to slot (startSlot+i) mod period. NaN values are skipped. Each
// slot's level is the weighted median (or mean) of its observations, with
// weights 0.5^(age/HalfLife). Errors: ErrInvalidArgument (period < 1),
// ErrInsufficientData (no valid value).
func FitSeasonalProfile(y []float64, startSlot, period int, opt *ProfileOptions) (*SeasonalProfile, error) {
	if period < 1 {
		return nil, ErrInvalidArgument
	}
	slots := make([]int, len(y))
	s := ((startSlot % period) + period) % period
	for i := range slots {
		slots[i] = s
		if s++; s == period {
			s = 0
		}
	}
	return FitSeasonalProfileSlots(y, slots, period, opt)
}

// FitSeasonalProfileSlots is FitSeasonalProfile for irregular data: slots[i]
// names the slot of y[i] (values outside 0..period-1 are wrapped). The age
// used for forgetting is the distance from the end of the slice.
func FitSeasonalProfileSlots(y []float64, slots []int, period int, opt *ProfileOptions) (*SeasonalProfile, error) {
	if period < 1 {
		return nil, ErrInvalidArgument
	}
	if len(y) != len(slots) {
		return nil, ErrDimension
	}
	o := ProfileOptions{}
	if opt != nil {
		o = *opt
	}
	vals := make([][]float64, period)
	wts := make([][]float64, period)
	found := false
	for i, v := range y {
		if !IsFinite(v) {
			continue
		}
		s := ((slots[i] % period) + period) % period
		w := 1.0
		if o.HalfLife > 0 {
			w = math.Exp2(-float64(len(y)-1-i) / o.HalfLife)
			if w < 1e-300 {
				continue
			}
		}
		vals[s] = append(vals[s], v)
		wts[s] = append(wts[s], w)
		found = true
	}
	if !found {
		return nil, ErrInsufficientData
	}
	p := NewSeasonalProfile(period)
	for s := 0; s < period; s++ {
		v, w := vals[s], wts[s]
		if len(v) == 0 {
			continue
		}
		p.Count[s] = len(v)
		for _, wi := range w {
			p.Weight[s] += wi
		}
		if o.Mean {
			p.Level[s] = WeightedMean(v, w)
			ss := 0.0
			for i, vi := range v {
				ss += w[i] * sq(vi-p.Level[s])
			}
			p.Spread[s] = math.Sqrt(ss / p.Weight[s])
			continue
		}
		p.Level[s] = WeightedQuantile(v, w, 0.5)
		dev := make([]float64, len(v))
		for i, vi := range v {
			dev[i] = math.Abs(vi - p.Level[s])
		}
		p.Spread[s] = MADScale * WeightedQuantile(dev, w, 0.5)
	}
	return p, nil
}

func (p *SeasonalProfile) slot(s int) int { return ((s % p.Period) + p.Period) % p.Period }

// At returns the typical value of a slot (wrapped into the period); ok is
// false when the slot has no data.
func (p *SeasonalProfile) At(slot int) (v float64, ok bool) {
	if p == nil || p.Period < 1 || len(p.Level) != p.Period || len(p.Weight) != p.Period {
		return 0, false
	}
	s := p.slot(slot)
	return p.Level[s], p.Weight[s] > 0
}

// Update folds one new observation into a slot by exponential forgetting:
// level += alpha*(v-level), the first observation of a slot initialises it.
// Spread is tracked as an exponentially weighted standard deviation. NaN
// values and alpha outside (0, 1] are ignored.
func (p *SeasonalProfile) Update(slot int, v, alpha float64) {
	if p == nil || p.Period < 1 || len(p.Level) != p.Period || len(p.Spread) != p.Period ||
		len(p.Weight) != p.Period || len(p.Count) != p.Period || !IsFinite(v) || !(alpha > 0 && alpha <= 1) {
		return
	}
	s := p.slot(slot)
	if !(p.Weight[s] > 0) {
		p.Level[s], p.Spread[s], p.Weight[s] = v, 0, 1
		p.Count[s]++
		return
	}
	d := v - p.Level[s]
	p.Level[s] += alpha * d
	p.Spread[s] = math.Sqrt((1 - alpha) * (p.Spread[s]*p.Spread[s] + alpha*d*d))
	p.Weight[s] = p.Weight[s]*(1-alpha) + 1
	p.Count[s]++
}

// Filled returns the per-slot levels with undefined slots filled by linear
// interpolation between the nearest defined slots around the cycle. ok is
// false (and the slice nil) when no slot is defined.
func (p *SeasonalProfile) Filled() (levels []float64, ok bool) {
	if p == nil || p.Period < 1 || len(p.Level) != p.Period || len(p.Weight) != p.Period {
		return nil, false
	}
	n := p.Period
	first := -1
	for s := 0; s < n; s++ {
		if p.Weight[s] > 0 {
			first = s
			break
		}
	}
	if first < 0 {
		return nil, false
	}
	levels = make([]float64, n)
	prev := first
	for k := 1; k <= n; k++ {
		s := (first + k) % n
		if !(p.Weight[s] > 0) {
			continue
		}
		gap := k - (prev-first+n)%n
		if prev == first && k == n {
			gap = n // single defined slot: wrap all the way round
		}
		for j := 1; j < gap; j++ {
			t := float64(j) / float64(gap)
			levels[(prev+j)%n] = p.Level[prev] + t*(p.Level[s]-p.Level[prev])
		}
		levels[s] = p.Level[s]
		prev = s
	}
	return levels, true
}

// Forecast returns the profile values for the h slots following startSlot-1,
// i.e. out[k] is the level of slot startSlot+k, with gaps filled as in
// Filled. nil when the profile is empty or h < 1.
func (p *SeasonalProfile) Forecast(startSlot, h int) []float64 {
	levels, ok := p.Filled()
	if !ok || h < 1 {
		return nil
	}
	out := make([]float64, h)
	for k := range out {
		out[k] = levels[p.slot(startSlot+k)]
	}
	return out
}

// Deseasonalize returns y minus the (gap-filled) profile, y[i] being in slot
// startSlot+i. NaN stays NaN. nil when the profile is empty.
func (p *SeasonalProfile) Deseasonalize(y []float64, startSlot int) []float64 {
	levels, ok := p.Filled()
	if !ok {
		return nil
	}
	out := make([]float64, len(y))
	for i, v := range y {
		out[i] = v - levels[p.slot(startSlot+i)]
	}
	return out
}

// SeasonalNaive forecasts the next h values of y by repeating the last
// observed cycle: the forecast for a phase is the most recent finite value
// at that phase, searching back through earlier cycles; if a phase was never
// observed the most recent finite value of the series is used. period < 1
// is treated as 1 (a plain "last value" forecast). Errors:
// ErrInvalidArgument (h < 1), ErrInsufficientData (no valid value).
func SeasonalNaive(y []float64, period, h int) ([]float64, error) {
	if h < 1 {
		return nil, ErrInvalidArgument
	}
	if period < 1 {
		period = 1
	}
	last := math.NaN()
	for i := len(y) - 1; i >= 0; i-- {
		if IsFinite(y[i]) {
			last = y[i]
			break
		}
	}
	if last != last {
		return nil, ErrInsufficientData
	}
	n := len(y)
	out := make([]float64, h)
	for k := range out {
		v := last
		// Index of the same phase one or more cycles before n+k.
		for i := n + k - period*((k/period)+1); i >= 0; i -= period {
			if IsFinite(y[i]) {
				v = y[i]
				break
			}
		}
		out[k] = v
	}
	return out, nil
}
