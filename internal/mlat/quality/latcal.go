package quality

import (
	"math"
	"sync"
)

// Airframe-latency calibration behind a hold-out A/B test (DESIGN §9.6,
// phase 4).
//
// An ADS-B transmitter with NACp-good positions still reports them late:
// the position in a squitter is the airframe's position L seconds before
// transmission (DO-260B allows up to a few hundred ms; simnet draws
// 0..300 ms). The reference store places T=0 positions at τ − L̂ with the
// prior L̂ = 0 ± 150 ms, so a reference lags the truth by v·(L − L̂) along
// track — tens of metres. Those references feed clock sync and score the
// hold-out, so a better L̂ per airframe improves both, if it is right.
//
// Learning. For aircraft NOT in the hold-out, every accepted MLAT fix gives
// a sample of the airframe's latency from the along-track difference
// between the MLAT position and the reference the engine used:
//
//	Lₛ = L̂_applied + a / |v|,  a = (MLAT − reference)·v̂,  σₛ = σ_a / |v|
//
// combined per airframe by a scalar Kalman update of a constant (prior
// PriorS ± PriorSigmaS, posterior σ floored at FloorSigmaS, estimate
// bounded to MinS…MaxS) with a 4σ gate after 5 samples. Only precise
// samples teach: NACp >= MinNACp (a NACp-7 position wanders by a 75 m 1σ
// over minutes, i.e. 0.3 s at 250 m/s, which no averaging removes) and
// σₛ <= MaxSampleSigmaS (0.15 s: fixes inside good geometry; poorly
// conditioned fixes far outside the network carry per-airframe biases that
// persist along the track). An airframe is calibrated once its posterior
// σ <= CalibratedSigmaS and it has MinCalSamples samples. Hold-out aircraft
// never teach (their fixes score the network, so they must not be fitted
// to it); because the hold-out rotates, most airframes are calibrated
// while outside it and keep that estimate, frozen, while inside.
//
// A/B test. Every calibrated airframe is assigned to an arm by a keyed hash
// (TreatFraction to "treated", independent of the hold-out hash). While in
// the hold-out, a treated airframe's reference uses its calibrated latency
// and a control airframe's the prior (ForReference). Score adds each
// precise hold-out comparison (σ_a <= MaxScoreSigmaM, NACp >= MinNACp) to
// its arm with weight w = 1/σ_a², winsorised at ClipM, in hourly slices;
// Decide compares the weighted mean squared along-track error of the arms
// over the last WindowNs with a cluster-robust (per-airframe) standard
// error:
//
//	MSE = Σ w e² / Σ w,   Var(MSE) = Σ_airframes (Σ w e² − MSE·Σ w)² / (Σ w)²
//	gain = 1 − MSE_T / MSE_C,   z = (MSE_C − MSE_T) / sqrt(Var_T + Var_C)
//
// The MLAT error enters both arms equally, so the difference is the
// reference error the calibration removed. Calibration is enabled for the
// network's references (non-hold-out aircraft) only when each arm has
// MinArmSamples samples from MinArmAircraft airframes, gain >= MinGain and
// z >= ZEnable; once enabled it is disabled again when the treated arm is
// significantly worse (gain < 0 and z <= −ZDisable). The hold-out arms keep
// running either way, so the decision follows the data.
//
// The comparison is honest about one circularity: a treated airframe's
// latency was learned from MLAT fixes of the same network, so a systematic
// along-track MLAT bias would be learned and would look like an
// improvement. MLAT errors have no preferred along-track sign (the
// direction of flight is unrelated to the station geometry), so this
// averages out over airframes; the simnet validation
// (latcal_simnet_test.go) checks the learned latencies, and the
// improvement the A/B claims, against the truth.

// LatencyCalParams tune LatencyCal. Zero fields take the defaults.
type LatencyCalParams struct {
	PriorS, PriorSigmaS float64 // prior latency and 1σ (0, 0.15 s)
	MinS, MaxS          float64 // bounds of the estimate (−0.2, 0.4 s)
	FloorSigmaS         float64 // smallest posterior 1σ (0.01 s)
	MinSpeedMps         float64 // samples below this speed are ignored (60 m/s)
	MinNACp             uint8   // samples with a lower NACp are ignored (8)
	MaxSampleSigmaS     float64 // learning samples with a larger σₛ are ignored (0.15 s)
	MaxScoreSigmaM      float64 // A/B samples with a larger σ_a are ignored (50 m)
	CalibratedSigmaS    float64 // calibrated below this posterior 1σ (0.04 s)
	MinCalSamples       int     // and with at least this many samples (20)
	TreatFraction       float64 // share of calibrated airframes treated (0.5)
	Salt                uint64  // arm hash key
	WindowNs            int64   // A/B window (6 h, hourly slices)
	MinArmSamples       int     // per arm (500)
	MinArmAircraft      int     // per arm (20)
	MinGain             float64 // relative MSE reduction to enable (0.05)
	ZEnable, ZDisable   float64 // significance to enable / disable (3, 2)
	ClipM               float64 // winsorisation of |along error| (300 m)
	TTLNs               int64   // airframes unseen this long are forgotten (24 h)
}

func (p *LatencyCalParams) defaults() {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&p.PriorSigmaS, 0.15)
	def(&p.MinS, -0.2)
	def(&p.MaxS, 0.4)
	def(&p.FloorSigmaS, 0.01)
	def(&p.MinSpeedMps, 60)
	def(&p.MaxSampleSigmaS, 0.15)
	def(&p.MaxScoreSigmaM, 50)
	def(&p.CalibratedSigmaS, 0.04)
	def(&p.TreatFraction, 0.5)
	def(&p.MinGain, 0.05)
	def(&p.ZEnable, 3)
	def(&p.ZDisable, 2)
	def(&p.ClipM, 300)
	if p.MinNACp == 0 {
		p.MinNACp = 8
	}
	if p.MinCalSamples <= 0 {
		p.MinCalSamples = 20
	}
	if p.WindowNs <= 0 {
		p.WindowNs = 6 * hourNs
	}
	if p.MinArmSamples <= 0 {
		p.MinArmSamples = 500
	}
	if p.MinArmAircraft <= 0 {
		p.MinArmAircraft = 20
	}
	if p.TTLNs <= 0 {
		p.TTLNs = 24 * hourNs
	}
}

// LatencySample is one along-track comparison of an MLAT fix with the
// reference built from an airframe's ADS-B position for the same instant.
type LatencySample struct {
	Key   uint32 // airframe (ICAO address)
	NowNs int64
	// AlongM = (MLAT − reference)·v̂ (m), v̂ the horizontal unit velocity;
	// SigmaAlongM its 1σ: the MLAT fix's along-track σ ⊕ the reference
	// position σ (NACp), m.
	AlongM, SigmaAlongM float64
	SpeedMps            float64 // ground speed
	// AppliedS is the latency the reference had applied (ForReference).
	AppliedS float64
	NACp     uint8 // announced NACp of the airframe
}

// airframeLat is one airframe's latency estimate.
type airframeLat struct {
	x, p     float64 // estimate (s) and its variance (s²)
	n        int32   // accepted samples
	rejected int32
	lastNs   int64
}

type latShard struct {
	mu sync.Mutex
	m  map[uint32]*airframeLat
}

// abArm accumulates one arm in one slice (weighted).
type abArm struct {
	n             int
	sw, swe2, swe float64
	per           map[uint32][2]float64 // Σw, Σw e² per airframe
}

func (a *abArm) add(key uint32, e, w float64) {
	a.n++
	a.sw += w
	a.swe2 += w * e * e
	a.swe += w * e
	if a.per == nil {
		a.per = map[uint32][2]float64{}
	}
	v := a.per[key]
	v[0] += w
	v[1] += w * e * e
	a.per[key] = v
}

type abSlice struct {
	start int64
	arm   [2]abArm // 0 control, 1 treated
}

// LatencyCal learns airframe latencies and runs the A/B test. Safe for
// concurrent use; Learn, Latency and ForReference take one shard lock.
type LatencyCal struct {
	p      LatencyCalParams
	thresh uint64
	sh     [64]latShard

	mu      sync.Mutex
	slices  []abSlice
	enabled bool
	last    ABReport
	learned uint64
	gated   uint64
}

// NewLatencyCal returns a calibrator with p (zero fields = defaults).
func NewLatencyCal(p LatencyCalParams) *LatencyCal {
	p.defaults()
	c := &LatencyCal{p: p}
	c.thresh = uint64(min(max(p.TreatFraction, 0), 1) * (1 << 63) * 2)
	if p.TreatFraction >= 1 {
		c.thresh = math.MaxUint64
	}
	for i := range c.sh {
		c.sh[i].m = map[uint32]*airframeLat{}
	}
	return c
}

// Params returns the effective parameters.
func (c *LatencyCal) Params() LatencyCalParams { return c.p }

// Learn adds one comparison of an aircraft that is NOT in the hold-out. It
// reports whether the sample was used.
func (c *LatencyCal) Learn(s LatencySample) bool {
	if s.NACp < c.p.MinNACp || !(s.SpeedMps >= c.p.MinSpeedMps) || !(s.SigmaAlongM > 0) ||
		math.IsNaN(s.AlongM) || math.IsNaN(s.AppliedS) {
		return false
	}
	ls := s.AppliedS + s.AlongM/s.SpeedMps
	sv := s.SigmaAlongM / s.SpeedMps
	if !(sv <= c.p.MaxSampleSigmaS) {
		return false
	}
	vs := sv * sv
	sh := &c.sh[s.Key&63]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	a := sh.m[s.Key]
	if a == nil {
		a = &airframeLat{x: c.p.PriorS, p: c.p.PriorSigmaS * c.p.PriorSigmaS}
		sh.m[s.Key] = a
	}
	a.lastNs = s.NowNs
	if a.n >= 5 && math.Abs(ls-a.x) > 4*math.Sqrt(a.p+vs) {
		a.rejected++
		c.count(&c.gated)
		return false
	}
	k := a.p / (a.p + vs)
	a.x += k * (ls - a.x)
	a.p = math.Max(a.p*(1-k), c.p.FloorSigmaS*c.p.FloorSigmaS)
	a.x = min(max(a.x, c.p.MinS), c.p.MaxS)
	a.n++
	c.count(&c.learned)
	return true
}

func (c *LatencyCal) count(v *uint64) {
	c.mu.Lock()
	*v++
	c.mu.Unlock()
}

// Latency returns an airframe's estimate (s), its 1σ, the samples behind
// it and whether it is calibrated.
func (c *LatencyCal) Latency(key uint32) (latS, sigmaS float64, n int, calibrated bool) {
	s := &c.sh[key&63]
	s.mu.Lock()
	a := s.m[key]
	if a == nil {
		s.mu.Unlock()
		return c.p.PriorS, c.p.PriorSigmaS, 0, false
	}
	x, p, nn := a.x, a.p, int(a.n)
	s.mu.Unlock()
	sg := math.Sqrt(p)
	return x, sg, nn, nn >= c.p.MinCalSamples && sg <= c.p.CalibratedSigmaS
}

// Treated reports the A/B arm of an airframe (true = calibration applied
// while in the hold-out). It is a pure function of the key and Salt.
func (c *LatencyCal) Treated(key uint32) bool {
	return mix64(uint64(key)^c.p.Salt^0x6c61746e63616c31) < c.thresh
}

// ForReference returns the latency the engine should apply to the
// reference of airframe key and its 1σ: the calibrated estimate when the
// airframe is calibrated and either in the hold-out and treated, or outside
// the hold-out while calibration is enabled; otherwise the prior.
// applied says which.
func (c *LatencyCal) ForReference(key uint32, inHoldOut bool) (latS, sigmaS float64, applied bool) {
	l, sg, _, cal := c.Latency(key)
	if cal {
		if inHoldOut && c.Treated(key) {
			return l, sg, true
		}
		if !inHoldOut && c.Enabled() {
			return l, sg, true
		}
	}
	return c.p.PriorS, c.p.PriorSigmaS, false
}

// Score adds one hold-out comparison, the reference built with
// ForReference(key, true) (s.AppliedS is not used). Only calibrated
// airframes take part (both arms), so the arms compare like with like,
// and only precise samples (σ_a <= MaxScoreSigmaM, NACp >= MinNACp). It
// reports whether the sample was counted.
func (c *LatencyCal) Score(s LatencySample) bool {
	if math.IsNaN(s.AlongM) || s.NACp < c.p.MinNACp || !(s.SigmaAlongM > 0 && s.SigmaAlongM <= c.p.MaxScoreSigmaM) {
		return false
	}
	if _, _, _, cal := c.Latency(s.Key); !cal {
		return false
	}
	e := min(max(s.AlongM, -c.p.ClipM), c.p.ClipM)
	w := 1 / (s.SigmaAlongM * s.SigmaAlongM)
	arm := 0
	if c.Treated(s.Key) {
		arm = 1
	}
	start := s.NowNs - s.NowNs%hourNs
	c.mu.Lock()
	defer c.mu.Unlock()
	var sl *abSlice
	for i := range c.slices {
		if c.slices[i].start == start {
			sl = &c.slices[i]
			break
		}
	}
	if sl == nil {
		c.pruneLocked(s.NowNs)
		c.slices = append(c.slices, abSlice{start: start})
		sl = &c.slices[len(c.slices)-1]
	}
	sl.arm[arm].add(s.Key, e, w)
	return true
}

func (c *LatencyCal) pruneLocked(nowNs int64) {
	keep := c.slices[:0]
	for _, s := range c.slices {
		if s.start+hourNs > nowNs-c.p.WindowNs {
			keep = append(keep, s)
		}
	}
	c.slices = keep
}

// ABArm summarises one arm.
type ABArm struct {
	N        int     `json:"n"`
	Aircraft int     `json:"aircraft"`
	RMSM     float64 `json:"rmsM"`  // weighted RMS along-track error, m
	MeanM    float64 `json:"meanM"` // weighted mean along-track error, m
}

// ABReport is the A/B state.
type ABReport struct {
	Control ABArm `json:"control"`
	Treated ABArm `json:"treated"`
	// Gain = 1 − MSE_treated / MSE_control; Z its significance.
	Gain float64 `json:"gain"`
	Z    float64 `json:"z"`
	// Enabled: calibration is applied to the network's references.
	Enabled bool `json:"enabled"`
	// Decision is "enable", "disable", "keep" or "insufficient".
	Decision string `json:"decision"`
	// Airframes known, calibrated; samples learned and gated (rejected).
	Airframes  int    `json:"airframes"`
	Calibrated int    `json:"calibrated"`
	Learned    uint64 `json:"learned"`
	Gated      uint64 `json:"gated"`
}

// Decide evaluates the A/B test over the last WindowNs before nowNs,
// updates the enabled state and returns the report. Call it about once a
// minute; it also forgets airframes unseen for TTLNs.
func (c *LatencyCal) Decide(nowNs int64) ABReport {
	var r ABReport
	for i := range c.sh {
		s := &c.sh[i]
		s.mu.Lock()
		for k, a := range s.m {
			if nowNs-a.lastNs > c.p.TTLNs {
				delete(s.m, k)
				continue
			}
			r.Airframes++
			if int(a.n) >= c.p.MinCalSamples && math.Sqrt(a.p) <= c.p.CalibratedSigmaS {
				r.Calibrated++
			}
		}
		s.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(nowNs)
	var mse, vr [2]float64
	for arm := 0; arm < 2; arm++ {
		per := map[uint32][2]float64{}
		var n int
		var sw, swe2, swe float64
		for i := range c.slices {
			a := &c.slices[i].arm[arm]
			n += a.n
			sw += a.sw
			swe2 += a.swe2
			swe += a.swe
			for k, v := range a.per {
				w := per[k]
				w[0] += v[0]
				w[1] += v[1]
				per[k] = w
			}
		}
		out := ABArm{N: n, Aircraft: len(per)}
		if sw > 0 {
			mse[arm] = swe2 / sw
			out.RMSM = math.Sqrt(mse[arm])
			out.MeanM = swe / sw
			var v float64
			for _, w := range per {
				d := w[1] - w[0]*mse[arm]
				v += d * d
			}
			vr[arm] = v / (sw * sw)
		}
		if arm == 0 {
			r.Control = out
		} else {
			r.Treated = out
		}
	}
	r.Learned, r.Gated = c.learned, c.gated
	enough := r.Control.N >= c.p.MinArmSamples && r.Treated.N >= c.p.MinArmSamples &&
		r.Control.Aircraft >= c.p.MinArmAircraft && r.Treated.Aircraft >= c.p.MinArmAircraft && mse[0] > 0
	r.Decision = "insufficient"
	if enough {
		r.Gain = 1 - mse[1]/mse[0]
		if s := math.Sqrt(vr[0] + vr[1]); s > 0 {
			r.Z = (mse[0] - mse[1]) / s
		}
		r.Decision = "keep"
		switch {
		case !c.enabled && r.Gain >= c.p.MinGain && r.Z >= c.p.ZEnable:
			c.enabled = true
			r.Decision = "enable"
		case c.enabled && r.Gain < 0 && r.Z <= -c.p.ZDisable:
			c.enabled = false
			r.Decision = "disable"
		}
	}
	r.Enabled = c.enabled
	c.last = r
	return r
}

// Enabled reports whether calibration is applied outside the hold-out.
func (c *LatencyCal) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// Last returns the report of the last Decide.
func (c *LatencyCal) Last() ABReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}
