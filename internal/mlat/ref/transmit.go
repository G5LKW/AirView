package ref

import (
	"math"

	"airview/internal/mlat/geo"
)

// Arrival is one receiver's time of arrival of a message, for
// TransmitInstant.
type Arrival struct {
	// Station is the receiver antenna (ECEF, m).
	Station geo.Vec3
	// DtNs is the arrival time of the frame reference point (start of the
	// preamble) at the antenna in network time, as an offset from the
	// caller's base (ns). Absolute Unix ns do not fit a float64 at ns
	// resolution; the base keeps them exact.
	DtNs float64
	// SigmaNs is its 1σ (clock and timestamp noise; for an unsynced
	// receiver the host-time uncertainty).
	SigmaNs float64
}

// TransmitInstant estimates the transmit instant of a message of reference
// aircraft key from its arrivals (doc.go "Transmit instant"): the weighted
// mean over receivers of t_i − ρ_i(p(τ))/c, iterated because the position
// depends on τ (the fixed point converges by a factor |v|/c ≈ 1e-6 per
// step). atm (may be nil) adds the tropospheric excess path. It returns τ
// as baseNs + tauDtNs, its 1σ, and fills st with the reference state at τ.
func (s *Store) TransmitInstant(key uint32, baseNs int64, arr []Arrival, atm *geo.Atmosphere, st *State) (tauDtNs, sigmaNs float64, ok bool) {
	if len(arr) == 0 {
		return 0, 0, false
	}
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil {
		return 0, 0, false
	}
	// start from the earliest arrival minus a typical 1 ms of flight
	t := math.Inf(1)
	var sw float64
	for i := range arr {
		t = math.Min(t, arr[i].DtNs)
		sg := arr[i].SigmaNs
		if !(sg > 0) {
			sg = 1
		}
		sw += 1 / (sg * sg)
	}
	tau := t - 1e6
	sigT := math.Sqrt(1 / sw)
	// One state evaluation; the iterations move the position along the
	// velocity (over the sub-millisecond corrections the acceleration term
	// is below 1e-5 m), then a final evaluation at the converged τ.
	if !s.state(tr, baseNs+int64(math.Round(tau)), sigT, st) {
		return 0, 0, false
	}
	p0, v, tau0 := st.Pos, st.Vel, tau
	// The refraction excess changes by far less than a millimetre over the
	// metres the aircraft moves during the iterations: evaluate it once.
	var exBuf [16]float64
	ex := exBuf[:0]
	if len(arr) > len(exBuf) {
		ex = make([]float64, 0, len(arr))
	}
	for i := range arr {
		e := 0.0
		if atm != nil {
			e = atm.Excess(arr[i].Station, p0)
		}
		ex = append(ex, e)
	}
	for it := 0; it < 6; it++ {
		p := p0.Add(v.Scale((tau - tau0) * 1e-9))
		var num float64
		for i := range arr {
			a := &arr[i]
			rho := a.Station.Dist(p) + ex[i]
			sg := a.SigmaNs
			if !(sg > 0) {
				sg = 1
			}
			num += (a.DtNs - rho/geo.C*1e9) / (sg * sg)
		}
		nt := num / sw
		done := math.Abs(nt-tau) < 0.01
		tau = nt
		if done {
			break
		}
	}
	if !s.state(tr, baseNs+int64(math.Round(tau)), sigT, st) {
		return 0, 0, false
	}
	// position uncertainty projected on the mean line of sight
	var u geo.Vec3
	for i := range arr {
		u = u.Add(st.Pos.Sub(arr[i].Station).Unit())
	}
	u = u.Unit()
	var pv float64
	c := &st.Cov
	uv := [3]float64{u.X, u.Y, u.Z}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			pv += uv[i] * c[i*3+j] * uv[j]
		}
	}
	sigmaNs = math.Sqrt(sigT*sigT + pv/(geo.C*geo.C)*1e18)
	s.state(tr, baseNs+int64(math.Round(tau)), sigmaNs, st)
	return tau, sigmaNs, true
}

// ObserveFix feeds an independent position of aircraft key at transmit
// instant tNs — an MLAT fix of a hold-out aircraft, or the TDOA solution of
// its ADS-B message from GPS-timed receivers — to the position latency
// estimator of a T=0 transmitter (doc.go "Position latency"). cov is the
// fix's ECEF covariance (m², row-major). It returns false when the fix was
// not used (no track, T=1, too slow, outlier).
func (s *Store) ObserveFix(key uint32, tNs int64, pos geo.Vec3, cov [9]float64) bool {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil || tr.n == 0 {
		return false
	}
	if tr.info.t1 && !tr.q.t1Untrusted {
		return false
	}
	var st State
	if !s.state(tr, tNs, 0, &st) || st.PosAgeS > s.opt.MaxPosAgeS {
		return false
	}
	speed := st.Vel.Norm()
	if speed < 50 {
		return false
	}
	u := st.Vel.Scale(1 / speed)
	uv := [3]float64{u.X, u.Y, u.Z}
	// along-track variance of the fix plus the track's own (filter + bias,
	// without the latency term, which is what is being estimated)
	var vf, vt float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			vf += uv[i] * cov[i*3+j] * uv[j]
		}
	}
	vt = st.SigmaFilterH*st.SigmaFilterH + st.SigmaBiasH*st.SigmaBiasH
	r := pos.Sub(st.Pos).Dot(u)
	lMeas := tr.lBuilt + r/speed
	vMeas := (vf + vt) / (speed * speed)

	// random walk of the latency estimate between fixes: it also absorbs
	// the slowly varying along-track part of the reported position error
	ts := tr.sec(tNs)
	if dt := ts - tr.lastRetag; dt > 0 && tr.lObs > 0 {
		tr.lVar += latencyRW * dt
	}
	d := lMeas - tr.lHat
	if tr.lObs >= 5 && d*d > 25*(tr.lVar+vMeas) && math.Abs(d) > 0.02 {
		return false
	}
	k := tr.lVar / (tr.lVar + vMeas)
	tr.lHat += k * d
	tr.lVar *= 1 - k
	if tr.lVar < 1e-8 {
		tr.lVar = 1e-8
	}
	tr.lObs++
	tr.lastRetag = ts
	if math.Abs(tr.lHat-tr.lBuilt) > s.opt.RetagS {
		tr.retag(s.opt.QHorizontal, s.opt.QVertical)
	}
	return true
}

// latencyRW is the random-walk rate of the latency estimate, s²/s
// (≈ 1.6 ms/√s).
const latencyRW = 2.5e-7

// SetLatency sets the position latency estimate of aircraft key (s) and
// its 1σ, e.g. restored from a previous run. The position nodes are
// re-tagged when it moves by more than RetagS.
func (s *Store) SetLatency(key uint32, latencyS, sigmaS float64) {
	sh := s.shard(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	tr := sh.m[key]
	if tr == nil || math.IsNaN(latencyS) {
		return
	}
	tr.lHat = latencyS
	if sigmaS > 0 {
		tr.lVar = sigmaS * sigmaS
	}
	if math.Abs(tr.lHat-tr.lBuilt) > s.opt.RetagS {
		tr.retag(s.opt.QHorizontal, s.opt.QVertical)
	}
}
