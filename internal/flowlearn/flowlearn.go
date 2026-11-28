// Package flowlearn learns how traffic flows through the airspace around
// the station and uses it to predict where an aircraft will go, correcting
// itself against what really happens.
//
// Learning: every aircraft position (about once a second, from the live
// picture) goes into a short per-aircraft history. When an aircraft has
// flown on for 30 s (and 60 s), the change of its track since then is
// recorded as a turn rate in the cell where it was: a 0.1-degree cell, one
// of six altitude bands and one of twelve 30-degree heading sectors. The
// cells are time-decayed averages (half-life 3 h), so they follow what the
// surrounding traffic does now: the runway in use, a standard arrival, an
// airway bend, a holding pattern.
//
// Prediction: from an aircraft's position, track and ground speed the path
// is integrated in 5-second steps; at each step the learned turn rate of the
// cell it is in is applied, scaled by the cell's confidence (how much
// traffic it has seen) and by the learned blend weight. With no traffic
// learned the prediction is the straight line (dead reckoning).
//
// Self-correction: every 15 s per aircraft both a learned and a straight
// prediction are made for 30 and 60 s ahead and scored when the aircraft
// gets there. The blend weight of each horizon moves towards the method
// that was closer (bounded steps, 0..1), and the mean errors of both are
// reported, so the benefit is measured, not assumed.
//
// Memory only (it relearns within hours after a restart); bounded to
// MaxCells cells and MaxAircraft histories.
package flowlearn

import (
	"math"
	"sort"
	"sync"
	"time"

	"airview/internal/geo"
)

// Tunables.
const (
	CellDeg     = 0.1
	Sectors     = 12
	HalfLife    = 3 * time.Hour
	MaxCells    = 400_000
	MaxAircraft = 20_000
	histKeep    = 90_000 // ms of history per aircraft
	histEvery   = 2_000  // ms between kept history samples
	evalEvery   = 15_000 // ms between self-check predictions per aircraft
	stepS       = 5.0
	confK       = 4.0  // cell confidence n/(n+confK)
	maxTurnDegS = 3.5  // standard rate turn is 3 deg/s; anything faster is noise
	weightStep  = 0.01 // blend weight change per scored prediction
	minGSKt     = 60
	idleDrop    = 10 * time.Minute
	cellIdle    = 72 * time.Hour
)

// Horizons are the scored prediction horizons (seconds).
var Horizons = [2]int{30, 60}

// altBand is one of six altitude bands.
func altBand(ft float64) int {
	switch {
	case ft < 3000:
		return 0
	case ft < 10000:
		return 1
	case ft < 20000:
		return 2
	case ft < 30000:
		return 3
	case ft < 40000:
		return 4
	}
	return 5
}

type key uint64

func keyOf(lat, lon, altFt, trk float64) key {
	i := int64(math.Floor((lat + 90) / CellDeg))
	j := int64(math.Floor((geo.NormalizeLon(lon) + 180) / CellDeg))
	s := int64(math.Floor(math.Mod(trk+360, 360)/(360/Sectors))) % Sectors
	return key(uint64(i)<<40 | uint64(j)<<16 | uint64(altBand(altFt))<<8 | uint64(s))
}

// cell is a time-decayed mean turn rate.
type cell struct {
	sum, w float64 // decayed sums of turn rate (deg/s) and weight
	last   int64   // Unix ms of the last update
}

func decay(dtMs int64) float64 {
	if dtMs <= 0 {
		return 1
	}
	return math.Exp(-float64(dtMs) / float64(HalfLife.Milliseconds()) * math.Ln2)
}

func (c *cell) add(v float64, t int64) {
	d := decay(t - c.last)
	c.sum, c.w = c.sum*d+v, c.w*d+1
	c.last = max(c.last, t)
}

// mean returns the turn rate and the effective number of samples at t.
func (c *cell) mean(t int64) (float64, float64) {
	if c.w <= 0 {
		return 0, 0
	}
	return c.sum / c.w, c.w * decay(t-c.last)
}

type sample struct {
	t                  int64
	lat, lon, alt, trk float64
	gs                 float64
}

type pending struct {
	due      int64
	h        int // horizon index
	lat, lon float64
	dLat     float64 // straight-line prediction
	dLon     float64
}

type history struct {
	s    []sample
	pend []pending
	next int64 // next self-check prediction
	seen int64
}

// Skill is the self-check of one horizon.
type Skill struct {
	HorizonS    int     `json:"horizonS"`
	N           int64   `json:"n"`           // scored predictions
	LearnedM    float64 `json:"learnedM"`    // mean error of the learned prediction, m (EWMA)
	StraightM   float64 `json:"straightM"`   // mean error of dead reckoning, m (EWMA)
	Weight      float64 `json:"weight"`      // current blend weight of the learned turn rates (0..1)
	BetterPct   float64 `json:"betterPct"`   // share of predictions where learned was closer
	Improvement float64 `json:"improvement"` // 1 - learned/straight (positive = learned better)
}

// Stats describes the learner.
type Stats struct {
	Cells    int     `json:"cells"`
	Aircraft int     `json:"aircraft"`
	Samples  int64   `json:"samples"` // turn observations learned
	Skill    []Skill `json:"skill"`
}

type skill struct {
	n          int64
	errL, errS float64
	better     float64
	weight     float64
}

// Learner is safe for concurrent use.
type Learner struct {
	mu      sync.Mutex
	cells   map[key]*cell
	ac      map[uint32]*history
	samples int64
	sk      [len(Horizons)]skill
	sweep   int64
}

// New creates an empty Learner.
func New() *Learner {
	l := &Learner{cells: map[key]*cell{}, ac: map[uint32]*history{}}
	for i := range l.sk {
		l.sk[i].weight = 0.5
	}
	return l
}

// Observe adds one real position of an aircraft (tMs Unix ms). Unknown
// values are NaN; positions without track or ground speed only advance the
// self-check.
func (l *Learner) Observe(icao uint32, tMs int64, lat, lon, altFt, trk, gs float64) {
	if !geo.ValidLatLon(lat, lon) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if tMs-l.sweep > 60_000 {
		l.sweepLocked(tMs)
	}
	h := l.ac[icao]
	if h == nil {
		if len(l.ac) >= MaxAircraft {
			return
		}
		h = &history{}
		l.ac[icao] = h
	}
	h.seen = tMs
	l.scoreLocked(h, tMs, lat, lon)
	moving := !math.IsNaN(trk) && !math.IsNaN(gs) && gs >= minGSKt && !math.IsNaN(altFt)
	if !moving {
		return
	}
	if n := len(h.s); n > 0 && tMs-h.s[n-1].t < histEvery {
		return
	}
	cur := sample{t: tMs, lat: lat, lon: lon, alt: altFt, trk: trk, gs: gs}
	// learn the turn since 30 and 60 s ago in the cells the aircraft was in then
	for _, hz := range Horizons {
		if p, ok := h.at(tMs - int64(hz)*1000); ok {
			rate := geo.AngleDiff(trk, p.trk) / float64(hz)
			if math.Abs(rate) <= maxTurnDegS {
				k := keyOf(p.lat, p.lon, p.alt, p.trk)
				c := l.cells[k]
				if c == nil {
					if len(l.cells) >= MaxCells {
						continue
					}
					c = &cell{}
					l.cells[k] = c
				}
				c.add(rate, tMs)
				l.samples++
			}
		}
	}
	h.s = append(h.s, cur)
	for len(h.s) > 0 && tMs-h.s[0].t > histKeep {
		h.s = h.s[1:]
	}
	if tMs >= h.next {
		h.next = tMs + evalEvery
		for i, hz := range Horizons {
			pts := l.predictLocked(lat, lon, altFt, trk, gs, float64(hz), float64(hz), l.sk[i].weight, tMs)
			end := pts[len(pts)-1]
			sLat, sLon := geo.Destination(lat, lon, trk, gs*1.852/3600*float64(hz))
			h.pend = append(h.pend, pending{due: tMs + int64(hz)*1000, h: i, lat: end.Lat, lon: end.Lon, dLat: sLat, dLon: sLon})
		}
	}
}

// at returns the history sample closest to t (within 3 s).
func (h *history) at(t int64) (sample, bool) {
	best, bd := sample{}, int64(math.MaxInt64)
	for _, s := range h.s {
		d := s.t - t
		if d < 0 {
			d = -d
		}
		if d < bd {
			best, bd = s, d
		}
	}
	return best, bd <= 3000
}

// scoreLocked scores the due self-check predictions against a real position.
func (l *Learner) scoreLocked(h *history, tMs int64, lat, lon float64) {
	keep := h.pend[:0]
	for _, p := range h.pend {
		if tMs < p.due {
			keep = append(keep, p)
			continue
		}
		if tMs-p.due > 3000 {
			continue // no position close enough to the due time
		}
		eL := geo.HaversineKm(p.lat, p.lon, lat, lon) * 1000
		eS := geo.HaversineKm(p.dLat, p.dLon, lat, lon) * 1000
		sk := &l.sk[p.h]
		sk.n++
		a := 1 / math.Min(float64(sk.n), 500) // EWMA over the last ~500
		sk.errL += (eL - sk.errL) * a
		sk.errS += (eS - sk.errS) * a
		b := 0.0
		if eL < eS {
			b = 1
		}
		sk.better += (b - sk.better) * a
		// move the blend weight towards the better method
		switch {
		case eL < eS-1:
			sk.weight = math.Min(1, sk.weight+weightStep)
		case eL > eS+1:
			sk.weight = math.Max(0, sk.weight-weightStep)
		}
	}
	h.pend = keep
}

func (l *Learner) sweepLocked(tMs int64) {
	l.sweep = tMs
	for k, h := range l.ac {
		if tMs-h.seen > idleDrop.Milliseconds() {
			delete(l.ac, k)
		}
	}
	if len(l.cells) > MaxCells/2 {
		for k, c := range l.cells {
			if tMs-c.last > cellIdle.Milliseconds() {
				delete(l.cells, k)
			}
		}
	}
}

// Point is one point of a predicted path.
type Point struct {
	DT  float64 `json:"dt"` // seconds after the start
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	Trk float64 `json:"track"`
	// Conf is the confidence of the learned turn applied in this step (0 = straight).
	Conf float64 `json:"conf"`
}

// Path is a learned prediction.
type Path struct {
	Points []Point `json:"points"`
	Weight float64 `json:"weight"` // blend weight used
	// Learned is true when at least one step used learned traffic.
	Learned bool `json:"learned"`
}

// Predict returns the learned path from a state for horizonS seconds, a
// point every outStepS seconds (nowMs is the decay reference).
func (l *Learner) Predict(lat, lon, altFt, trk, gs float64, horizonS, outStepS float64, nowMs int64) Path {
	l.mu.Lock()
	defer l.mu.Unlock()
	hi := 0
	if horizonS > float64(Horizons[0]) {
		hi = 1
	}
	w := l.sk[hi].weight
	pts := l.predictLocked(lat, lon, altFt, trk, gs, horizonS, outStepS, w, nowMs)
	out := Path{Weight: math.Round(w*100) / 100}
	for _, p := range pts {
		out.Learned = out.Learned || p.Conf > 0.05
	}
	out.Points = pts
	return out
}

func (l *Learner) predictLocked(lat, lon, altFt, trk, gs, horizonS, outStepS, w float64, nowMs int64) []Point {
	out := []Point{{DT: 0, Lat: lat, Lon: lon, Trk: trk}}
	if math.IsNaN(trk) || math.IsNaN(gs) || gs <= 0 {
		return out
	}
	if outStepS <= 0 {
		outStepS = stepS
	}
	nextOut := outStepS
	kmPerS := gs * 1.852 / 3600
	for t := stepS; t <= horizonS+1e-9; t += stepS {
		conf := 0.0
		if c := l.cells[keyOf(lat, lon, altFt, trk)]; c != nil {
			rate, n := c.mean(nowMs)
			conf = n / (n + confK)
			trk = math.Mod(trk+rate*conf*w*stepS+360, 360)
		}
		lat, lon = geo.Destination(lat, lon, trk, kmPerS*stepS)
		if t+1e-9 >= nextOut || t+stepS > horizonS+1e-9 {
			out = append(out, Point{DT: t, Lat: round6(lat), Lon: round6(lon), Trk: math.Round(trk*10) / 10, Conf: math.Round(conf*w*100) / 100})
			nextOut += outStepS
		}
	}
	return out
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// Stats returns the learner's state and self-check.
func (l *Learner) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Stats{Cells: len(l.cells), Aircraft: len(l.ac), Samples: l.samples}
	for i, hz := range Horizons {
		sk := l.sk[i]
		k := Skill{HorizonS: hz, N: sk.n, LearnedM: math.Round(sk.errL), StraightM: math.Round(sk.errS), Weight: math.Round(sk.weight*100) / 100,
			BetterPct: math.Round(sk.better*1000) / 10}
		if sk.errS > 0 {
			k.Improvement = math.Round((1-sk.errL/sk.errS)*1000) / 1000
		}
		s.Skill = append(s.Skill, k)
	}
	sort.Slice(s.Skill, func(i, j int) bool { return s.Skill[i].HorizonS < s.Skill[j].HorizonS })
	return s
}

// MinBlendScored is the number of scored self-check predictions a horizon
// needs before its learned turns may move the live coasting positions, and
// MinBlendGain the improvement over dead reckoning it must have shown (where
// traffic flies straight both are equally good, and blending would only pull
// the filter's own turn prediction towards a straight line).
const (
	MinBlendScored = 50
	MinBlendGain   = 0.1   // relative: 1 - learned/straight error
	MinBlendGainM  = 100.0 // absolute: straight minus learned mean error, m
)

// Blend returns where the learned traffic flow takes an aircraft that was at
// (lat, lon, altFt) with track trk and ground speed gs (kt) dtS seconds ago,
// and the weight w (0..1) the live filter's coasting position should give
// it. w is the self-check weight of the nearest horizon times the mean
// confidence of the cells the path used; it is 0 (ok false) unless that
// horizon has at least MinBlendScored scored predictions and the learned
// prediction was measured at least MinBlendGain and MinBlendGainM better
// than the straight one. dtS beyond twice
// the longest horizon is not blended.
func (l *Learner) Blend(lat, lon, altFt, trk, gs, dtS float64, nowMs int64) (lat2, lon2, w float64, ok bool) {
	if !(dtS > 0) || dtS > 2*float64(Horizons[len(Horizons)-1]) || math.IsNaN(trk) || !(gs >= minGSKt) {
		return 0, 0, 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	hi := 0
	if dtS > float64(Horizons[0]) {
		hi = 1
	}
	sk := l.sk[hi]
	if sk.n < MinBlendScored || !(sk.errS > 0) || 1-sk.errL/sk.errS < MinBlendGain || sk.errS-sk.errL < MinBlendGainM || sk.weight <= 0 {
		return 0, 0, 0, false
	}
	pts := l.predictLocked(lat, lon, altFt, trk, gs, dtS, stepS, sk.weight, nowMs)
	if len(pts) < 2 {
		return 0, 0, 0, false
	}
	// Conf of each point is conf*weight of its step; average over the steps
	var sum float64
	for _, p := range pts[1:] {
		sum += p.Conf
	}
	w = sum / float64(len(pts)-1)
	if !(w > 0.01) {
		return 0, 0, 0, false
	}
	last := pts[len(pts)-1]
	return last.Lat, last.Lon, math.Min(w, 1), true
}
