package trajectory

import (
	"math"
	"sort"
	"sync"
)

// scoring horizons (seconds)
var horizons = [nHz]float64{5, 15, 30, 60}

const nHz = 4

// predictors compared by the self-verification
const (
	prIMM = iota // this package's full predictor
	prCV         // filtered state, straight line, constant vertical rate
	prDR         // naive dead reckoning from the last raw position / velocity
	nPred
)

var predNames = [nPred]string{"imm", "cv", "dr"}

func hzTol(h float64) float64 { return math.Max(1.5, 0.1*h) }

type errRec struct {
	h, along, cross, v float32 // m, m, m, ft (NaN = no vertical truth)
}

type cell struct {
	ring  []errRec
	head  int
	n     int
	total uint64
	sumH2 float64
	in95  uint64
	n95   uint64
}

func (c *cell) add(r errRec, in95 int8) {
	if len(c.ring) == 0 {
		return
	}
	c.ring[c.head] = r
	c.head = (c.head + 1) % len(c.ring)
	if c.n < len(c.ring) {
		c.n++
	}
	c.total++
	c.sumH2 += float64(r.h) * float64(r.h)
	if in95 >= 0 {
		c.n95++
		if in95 == 1 {
			c.in95++
		}
	}
}

// scorer accumulates rolling prediction-error statistics.
type scorer struct {
	mu    sync.Mutex
	cells [nPred][nHz][nPhase]cell
}

func newScorer(window int) *scorer {
	s := &scorer{}
	for a := range s.cells {
		for b := range s.cells[a] {
			for c := range s.cells[a][b] {
				s.cells[a][b][c].ring = make([]errRec, window)
			}
		}
	}
	return s
}

type scoreSample struct {
	pred, hz, phase uint8
	in95            int8
	r               errRec
}

func (s *scorer) addBatch(b []scoreSample) {
	if len(b) == 0 {
		return
	}
	s.mu.Lock()
	for i := range b {
		x := &b[i]
		s.cells[x.pred][x.hz][phAll].add(x.r, x.in95)
		if x.phase != phAll {
			s.cells[x.pred][x.hz][x.phase].add(x.r, x.in95)
		}
	}
	s.mu.Unlock()
}

// score evaluates matured snapshots against the real position just received.
func (tr *track) score(t float64, o *Obs, e *env) {
	p := &e.p
	var buf [nSnap * nHz * nPred]scoreSample
	n := 0
	vTruth := float32(math.NaN())
	if o.Has&HasAlt != 0 && finite(o.AltFt) {
		vTruth = float32(o.AltFt)
	} else if tr.haveRawAlt && math.Abs(t-tr.rawAltT) < 1.5 {
		vTruth = float32(tr.rawAlt)
	}
	sig := posSigma(o, p)
	for i := range tr.snaps {
		s := &tr.snaps[i]
		if !s.ok {
			continue
		}
		dt := t - s.ps.t
		for k := 0; k < nHz; k++ {
			bit := uint8(1) << k
			if s.done&bit != 0 || dt < horizons[k] {
				continue
			}
			s.done |= bit
			if dt > horizons[k]+hzTol(horizons[k]) {
				continue
			}
			ax, ay := s.ps.fr.toXY(o.Lat, o.Lon)
			// full predictor
			ps := s.ps
			ps.propagate(dt, p, true)
			alt, _, _ := ps.vertical(p)
			r, in95 := errOf(ax, ay, ps.x[ix], ps.x[iy], ps.x[ivx], ps.x[ivy], vTruth, alt, ps.vOK)
			S := [2][2]float64{{ps.P[ix][ix] + sig*sig, ps.P[ix][iy]}, {ps.P[iy][ix], ps.P[iy][iy] + sig*sig}}
			det := S[0][0]*S[1][1] - S[0][1]*S[1][0]
			in95 = -1
			if det > 0 {
				ex, ey := ax-ps.x[ix], ay-ps.x[iy]
				d2 := (ex*ex*S[1][1] - 2*ex*ey*S[0][1] + ey*ey*S[0][0]) / det
				in95 = 0
				if d2 <= 5.991 {
					in95 = 1
				}
			}
			buf[n] = scoreSample{prIMM, uint8(k), s.phase, in95, r}
			n++
			// straight line from the filtered state
			b := &s.ps
			cx, cy := b.x[ix]+b.x[ivx]*dt, b.x[iy]+b.x[ivy]*dt
			calt := b.h + b.hd*(b.vElapsed+dt)
			r, _ = errOf(ax, ay, cx, cy, b.x[ivx], b.x[ivy], vTruth, calt, b.vOK)
			buf[n] = scoreSample{prCV, uint8(k), s.phase, -1, r}
			n++
			// naive dead reckoning
			dd := t - s.drT
			ts, tc := math.Sincos(s.drTrk * deg2rad)
			dx, dy := s.ps.fr.toXY(s.drLat, s.drLon)
			dx += ts * s.drGS * dd
			dy += tc * s.drGS * dd
			dalt := s.drAlt + s.drVR*(t-s.drAltT)
			r, _ = errOf(ax, ay, dx, dy, ts, tc, vTruth, dalt, s.drAltOK)
			buf[n] = scoreSample{prDR, uint8(k), s.phase, -1, r}
			n++
		}
	}
	e.sc.addBatch(buf[:n])
}

func errOf(ax, ay, px, py, vx, vy float64, vTruth float32, alt float64, altOK bool) (errRec, int8) {
	ex, ey := ax-px, ay-py
	var r errRec
	r.h = float32(math.Hypot(ex, ey))
	v := math.Hypot(vx, vy)
	if v > 1e-6 {
		ux, uy := vx/v, vy/v
		r.along = float32(ex*ux + ey*uy)
		r.cross = float32(ex*uy - ey*ux)
	}
	r.v = float32(math.NaN())
	if altOK && !math.IsNaN(float64(vTruth)) && finite(alt) {
		r.v = vTruth - float32(alt)
	}
	return r, -1
}

// HorizonStats is the rolling error summary for one predictor, horizon and flight phase.
type HorizonStats struct {
	HorizonSec    int     `json:"horizon"`
	Predictor     string  `json:"predictor"` // "imm" | "cv" | "dr"
	Phase         string  `json:"phase"`     // "all" | "straight" | "turn" | "vertical"
	N             int     `json:"n"`         // samples in the rolling window
	Total         uint64  `json:"total"`     // samples since start
	MedianKm      float64 `json:"medianKm"`
	P90Km         float64 `json:"p90Km"`
	P95Km         float64 `json:"p95Km"`
	MeanKm        float64 `json:"meanKm"`
	RMSKm         float64 `json:"rmsKm"`         // all-time
	AlongBiasKm   float64 `json:"alongBiasKm"`   // mean along-track error (+ = real aircraft ahead of prediction)
	AlongMedAbsKm float64 `json:"alongMedAbsKm"` //
	CrossBiasKm   float64 `json:"crossBiasKm"`   // mean cross-track error (+ = right of predicted track)
	CrossMedAbsKm float64 `json:"crossMedAbsKm"` //
	VertN         int     `json:"vertN"`
	VertMedAbsFt  float64 `json:"vertMedAbsFt"`
	VertP90Ft     float64 `json:"vertP90Ft"`
	VertBiasFt    float64 `json:"vertBiasFt"` // mean (real - predicted)
	// Calib95 (imm only): fraction of real positions inside the predicted 95 %
	// ellipse (ideal 0.95; lower = over-confident). -1 when unavailable.
	Calib95 float64 `json:"calib95"`
}

// Skill compares the full predictor against naive dead reckoning.
type Skill struct {
	HorizonSec int     `json:"horizon"`
	Phase      string  `json:"phase"`
	N          int     `json:"n"`
	IMMMedKm   float64 `json:"immMedianKm"`
	DRMedKm    float64 `json:"drMedianKm"`
	SkillPct   float64 `json:"skillPct"` // 100*(1 - imm/dr); > 0 = better than dead reckoning
}

// Counters are cumulative ingest counters.
type Counters struct {
	Observations uint64 `json:"observations"`
	Positions    uint64 `json:"positions"`
	Rejected     uint64 `json:"rejected"`    // positions failing the Mahalanobis gate
	VelRejected  uint64 `json:"velRejected"` // velocities failing the gate
	AltRejected  uint64 `json:"altRejected"` // altitudes failing the gate
	OutOfOrder   uint64 `json:"outOfOrder"`  // late observations used (shifted)
	OOODropped   uint64 `json:"oooDropped"`  // too late, dropped
	Duplicates   uint64 `json:"duplicates"`
	Resets       uint64 `json:"resets"` // tracks re-initialised after repeated rejects
	Inits        uint64 `json:"inits"`
	Overflow     uint64 `json:"overflow"` // observations ignored because MaxTracks was reached
}

// Stats is the payload of the statistics endpoint.
type Stats struct {
	Now      int64          `json:"now"`
	Tracks   int            `json:"tracks"`
	Ready    int            `json:"ready"`    // tracks with a usable estimate
	Coasting int            `json:"coasting"` // tracks currently coasting (age > CoastAfter, <= MaxCoast)
	Counters Counters       `json:"counters"`
	Horizons []HorizonStats `json:"horizons"`
	Skill    []Skill        `json:"skill"`
}

func quantSorted(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	h := p * float64(len(s)-1)
	lo := int(math.Floor(h))
	if lo >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[lo] + (h-float64(lo))*(s[lo+1]-s[lo])
}

// summarize computes all cells (allocates; call at UI rate).
func (s *scorer) summarize() ([]HorizonStats, []Skill) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hs []HorizonStats
	var sk []Skill
	var hbuf, abuf, cbuf, vbuf []float64
	med := [nPred][nHz][nPhase]float64{}
	cnt := [nPred][nHz][nPhase]int{}
	for pr := 0; pr < nPred; pr++ {
		for k := 0; k < nHz; k++ {
			for ph := 0; ph < nPhase; ph++ {
				c := &s.cells[pr][k][ph]
				if c.n == 0 {
					continue
				}
				hbuf, abuf, cbuf, vbuf = hbuf[:0], abuf[:0], cbuf[:0], vbuf[:0]
				var sumH, sumA, sumC, sumV float64
				for i := 0; i < c.n; i++ {
					r := c.ring[i]
					hbuf = append(hbuf, float64(r.h))
					abuf = append(abuf, math.Abs(float64(r.along)))
					cbuf = append(cbuf, math.Abs(float64(r.cross)))
					sumH += float64(r.h)
					sumA += float64(r.along)
					sumC += float64(r.cross)
					if !math.IsNaN(float64(r.v)) {
						vbuf = append(vbuf, float64(r.v))
						sumV += float64(r.v)
					}
				}
				sort.Float64s(hbuf)
				sort.Float64s(abuf)
				sort.Float64s(cbuf)
				n := float64(c.n)
				x := HorizonStats{
					HorizonSec:    int(horizons[k]),
					Predictor:     predNames[pr],
					Phase:         phaseNames[ph],
					N:             c.n,
					Total:         c.total,
					MedianKm:      quantSorted(hbuf, 0.5) / 1000,
					P90Km:         quantSorted(hbuf, 0.9) / 1000,
					P95Km:         quantSorted(hbuf, 0.95) / 1000,
					MeanKm:        sumH / n / 1000,
					RMSKm:         math.Sqrt(c.sumH2/float64(c.total)) / 1000,
					AlongBiasKm:   sumA / n / 1000,
					AlongMedAbsKm: quantSorted(abuf, 0.5) / 1000,
					CrossBiasKm:   sumC / n / 1000,
					CrossMedAbsKm: quantSorted(cbuf, 0.5) / 1000,
					VertN:         len(vbuf),
					Calib95:       -1,
				}
				if len(vbuf) > 0 {
					x.VertBiasFt = sumV / float64(len(vbuf))
					for i := range vbuf {
						vbuf[i] = math.Abs(vbuf[i])
					}
					sort.Float64s(vbuf)
					x.VertMedAbsFt = quantSorted(vbuf, 0.5)
					x.VertP90Ft = quantSorted(vbuf, 0.9)
				}
				if c.n95 > 0 {
					x.Calib95 = float64(c.in95) / float64(c.n95)
				}
				sanitizeHS(&x)
				hs = append(hs, x)
				med[pr][k][ph] = x.MedianKm
				cnt[pr][k][ph] = c.n
			}
		}
	}
	for k := 0; k < nHz; k++ {
		for ph := 0; ph < nPhase; ph++ {
			if cnt[prIMM][k][ph] == 0 || cnt[prDR][k][ph] == 0 {
				continue
			}
			d := med[prDR][k][ph]
			sp := 0.0
			if d > 0 {
				sp = 100 * (1 - med[prIMM][k][ph]/d)
			}
			sk = append(sk, Skill{HorizonSec: int(horizons[k]), Phase: phaseNames[ph], N: cnt[prIMM][k][ph],
				IMMMedKm: med[prIMM][k][ph], DRMedKm: d, SkillPct: fin(sp)})
		}
	}
	return hs, sk
}

func sanitizeHS(x *HorizonStats) {
	for _, f := range []*float64{&x.MedianKm, &x.P90Km, &x.P95Km, &x.MeanKm, &x.RMSKm, &x.AlongBiasKm,
		&x.AlongMedAbsKm, &x.CrossBiasKm, &x.CrossMedAbsKm, &x.VertMedAbsFt, &x.VertP90Ft, &x.VertBiasFt, &x.Calib95} {
		*f = fin(*f)
	}
}
