package predict

import (
	"context"
	"math"
	"time"

	"airview/internal/stat"
	"airview/internal/store"
)

const (
	hourSec = 3600
	daySec  = 86400

	// HorizonHours is the length of the traffic and reception forecasts.
	HorizonHours = 72
	// maxHistoryDays bounds the history that is loaded and learned from.
	maxHistoryDays = 180
	// minMinutes is the number of minute rows an hour needs before its
	// aircraft gauge counts as a complete observation.
	minMinutes = 30
	// minPositions is the number of positions an hour needs before its
	// range percentiles are trusted.
	minPositions = 10
)

func floorHour(t int64) int64 { return t - ((t%hourSec)+hourSec)%hourSec }

// history is the station's hourly record on a regular grid: index i is the
// hour starting at t0 + i*3600; the last index is the last COMPLETE hour.
// Missing values are NaN.
type history struct {
	t0  int64
	n   int
	cur int64 // start of the current (incomplete) hour = t0 + n*3600

	aircraft []float64 // peak concurrently tracked aircraft
	msgRate  []float64 // frames per second
	p95      []float64 // km
	maxKm    []float64 // km

	wx  []store.Weather // weather_obs per hour (ProfileJSON dropped once analysed)
	has []bool          // wx[i] present
	atm []Atmos         // propagation state per hour (Valid = false when unknown)

	wxNow *store.Weather // observation row of the current hour, if any
	atNow Atmos

	// Hour-of-week / hour-of-day slot of every grid hour and of the
	// HorizonHours hours that follow.
	sW, sD []int
}

func (h *history) t(i int) int64 { return h.t0 + int64(i)*hourSec }

func (h *history) idx(t int64) int {
	if t < h.t0 {
		return -1
	}
	i := int((t - h.t0) / hourSec)
	if i >= h.n {
		return -1
	}
	return i
}

func nanSlice(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = nan
	}
	return s
}

// loadHistory reads the hourly history up to now. With prev (an earlier
// result) only the last two days are re-read and the rest is carried over,
// which keeps the periodic refresh cheap on an SD card.
func (f *Forecaster) loadHistory(ctx context.Context, now time.Time, prev *history, elevM float64) (*history, error) {
	cur := floorHour(now.Unix())
	end := cur - hourSec // last complete hour
	from := cur - maxHistoryDays*daySec
	incremental := prev != nil && prev.n > 72 && prev.cur <= cur && cur-prev.cur <= 6*hourSec
	qFrom := from
	if incremental {
		qFrom = max(prev.cur-48*hourSec, prev.t0, from)
	}

	stats, err := f.st.HourStats(ctx, f.ant, qFrom, end)
	if err != nil {
		return nil, err
	}
	ranges, err := f.st.RangeHours(ctx, f.ant, qFrom, end)
	if err != nil {
		return nil, err
	}
	obs, err := f.st.WeatherObs(ctx, qFrom, cur)
	if err != nil {
		return nil, err
	}

	t0 := cur
	if incremental {
		t0 = max(prev.t0, from)
	} else {
		if len(stats) > 0 {
			t0 = min(t0, floorHour(stats[0].T))
		}
		if len(ranges) > 0 {
			t0 = min(t0, floorHour(ranges[0].T))
		}
		if len(obs) > 0 {
			t0 = min(t0, floorHour(obs[0].T))
		}
		t0 = max(t0, from)
	}
	n := int((cur - t0) / hourSec)
	h := &history{
		t0: t0, n: n, cur: cur,
		aircraft: nanSlice(n), msgRate: nanSlice(n), p95: nanSlice(n), maxKm: nanSlice(n),
		wx: make([]store.Weather, n), has: make([]bool, n), atm: make([]Atmos, n),
	}
	if incremental {
		// Everything before qFrom is final; carry it over.
		s := int((t0 - prev.t0) / hourSec) // hours that slid out of the window
		k := max(min(int((qFrom-t0)/hourSec), prev.n-s, n), 0)
		copy(h.aircraft, prev.aircraft[s:s+k])
		copy(h.msgRate, prev.msgRate[s:s+k])
		copy(h.p95, prev.p95[s:s+k])
		copy(h.maxKm, prev.maxKm[s:s+k])
		copy(h.wx, prev.wx[s:s+k])
		copy(h.has, prev.has[s:s+k])
		copy(h.atm, prev.atm[s:s+k])
	}
	for i := range stats {
		r := &stats[i]
		j := h.idx(floorHour(r.T))
		if j < 0 {
			continue
		}
		if r.Minutes >= minMinutes {
			h.aircraft[j] = float64(r.Aircraft)
			h.msgRate[j] = float64(r.Frames) / r.CoveredSec(true)
		} else if r.Minutes == 0 && r.Frames > 0 {
			// A row without minute bookkeeping (imported / legacy data).
			h.aircraft[j] = float64(r.Aircraft)
			h.msgRate[j] = float64(r.Frames) / hourSec
		}
	}
	for i := range ranges {
		r := &ranges[i]
		j := h.idx(floorHour(r.T))
		if j < 0 || r.Positions < minPositions {
			continue
		}
		if fin(r.P95Km) && r.P95Km > 0 {
			h.p95[j] = r.P95Km
		}
		if fin(r.MaxKm) && r.MaxKm > 0 {
			h.maxKm[j] = r.MaxKm
		}
	}
	for i := range obs {
		w := &obs[i]
		t := floorHour(w.T)
		if t == cur {
			h.wxNow = w
			h.atNow = AnalyzeWeather(w, elevM)
			continue
		}
		j := h.idx(t)
		if j < 0 {
			continue
		}
		h.atm[j] = AnalyzeWeather(w, elevM)
		h.wx[j] = *w
		h.wx[j].ProfileJSON = "" // analysed; do not keep megabytes of JSON alive
		h.has[j] = true
	}

	m := n + HorizonHours
	h.sW, h.sD = make([]int, m), make([]int, m)
	for i := 0; i < m; i++ {
		t := h.t(i)
		off := f.utcOffset(t)
		h.sW[i] = stat.HourOfWeek(t, off)
		h.sD[i] = stat.HourOfDay(t, off)
	}
	return h, nil
}

// latestObs returns the newest observation (the current hour's row, else the
// newest grid hour not older than maxAgeH hours) and its propagation state.
func (h *history) latestObs(maxAgeH int) (*store.Weather, Atmos, bool) {
	if h.wxNow != nil {
		return h.wxNow, h.atNow, true
	}
	for i := h.n - 1; i >= 0 && i >= h.n-maxAgeH; i-- {
		if h.has[i] {
			return &h.wx[i], h.atm[i], true
		}
	}
	return nil, Atmos{}, false
}

// obsSeries extracts one weather variable over the last k grid hours plus the
// current hour's observation (if present) as the final sample.
func (h *history) obsSeries(k int, get func(w *store.Weather) float64) []float64 {
	start := max(h.n-k, 0)
	out := make([]float64, 0, h.n-start+1)
	for i := start; i < h.n; i++ {
		if h.has[i] {
			out = append(out, get(&h.wx[i]))
		} else {
			out = append(out, nan)
		}
	}
	if h.wxNow != nil {
		out = append(out, get(h.wxNow))
	}
	return out
}

// solarHour returns the local mean solar time of day in hours for Unix time
// t at longitude lon (degrees east).
func solarHour(t int64, lon float64) float64 {
	hh := math.Mod(float64(((t%daySec)+daySec)%daySec)/hourSec+lon/15, 24)
	if hh < 0 {
		hh += 24
	}
	return hh
}
