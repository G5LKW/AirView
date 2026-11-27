package survey

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// synth generates survey observations with a known truth: aircraft on
// straight tracks around the target, the emission time known to the
// peers' accuracy, and a target clock of the given class (offset, drift,
// thermal ramp, jitter, quantisation, optional silent step).
type synth struct {
	seed       uint64
	class      model.ClockClass
	truth      geo.LLH
	aircraft   int
	radiusKm   float64
	durS       float64
	rateHz     float64 // observations per aircraft per second
	tauSigmaNs float64
	jitterNs   float64
	quantNs    float64
	driftPPM   float64
	rampPPMs   float64 // thermal ramp (ppm/s)
	stepAtS    float64 // silent clock step (0 = none)
	stepNs     float64
	posSigmaM  float64 // aircraft position noise per report (white)
	latBiasM   float64 // per-aircraft along-track bias 1σ (latency)
	peerDistKm float64 // distance of the virtual peers (Ebar)
	sector     float64 // aircraft azimuths limited to ±sector/2 around north (0 = all)
	minElev    float64
}

const unixBase = int64(1_780_000_000) * 1e9

func (s synth) gen() []Obs {
	r := rand.New(rand.NewPCG(s.seed, 99))
	u := s.truth.ECEF()
	fr := geo.NewFrame(s.truth)
	hz := s.class.NominalHz()
	type ac struct {
		key      uint32
		p0, v    geo.Vec3 // ENU
		bias     float64
		phase    float64
		peerECEF []geo.Vec3
	}
	acs := make([]ac, s.aircraft)
	for i := range acs {
		az := r.Float64() * 2 * math.Pi
		if s.sector > 0 {
			az = (r.Float64() - 0.5) * s.sector * math.Pi / 180
		}
		d := math.Sqrt(r.Float64()) * s.radiusKm * 1000
		h := 3000 + r.Float64()*9000
		hd := r.Float64() * 2 * math.Pi
		sp := 120 + r.Float64()*130
		a := ac{key: 0x400000 + uint32(i), p0: geo.Vec3{X: d * math.Sin(az), Y: d * math.Cos(az), Z: h},
			v: geo.Vec3{X: sp * math.Sin(hd), Y: sp * math.Cos(hd)}, bias: r.NormFloat64() * s.latBiasM, phase: r.Float64()}
		for j := 0; j < 3; j++ {
			pa := r.Float64() * 2 * math.Pi
			pd := s.peerDistKm * 1000 * (0.5 + r.Float64())
			a.peerECEF = append(a.peerECEF, fr.FromENU(geo.Vec3{X: pd * math.Sin(pa), Y: pd * math.Cos(pa), Z: 0}))
		}
		acs[i] = a
	}
	atm := geo.DefaultAtmosphere()
	var out []Obs
	dt := 1 / s.rateHz
	ticks0 := uint64(1) << 40
	theta0 := 0.0013 // s
	for t := 0.0; t < s.durS; t += dt {
		for i := range acs {
			a := &acs[i]
			tt := t + a.phase*dt
			e := a.p0.Add(a.v.Scale(tt))
			p := fr.FromENU(e)
			if _, el, _ := fr.AzElRange(p); el < math.Max(s.minElev, 0.5) {
				continue
			}
			// target receive time in true time
			tau := float64(unixBase)/1e9 + tt
			rng := atm.Range(u, p)
			trx := tt + rng/cLight
			theta := theta0 + s.driftPPM*1e-6*trx + 0.5*s.rampPPMs*1e-6*trx*trx
			if s.stepAtS > 0 && trx > s.stepAtS {
				theta += s.stepNs * 1e-9
			}
			local := trx + theta + r.NormFloat64()*s.jitterNs*1e-9
			if s.quantNs > 0 {
				q := s.quantNs * 1e-9
				local = math.Round(local/q) * q
			}
			var ticks uint64
			if s.class.GPS() {
				ticks = uint64(unixBase + int64(math.Round(local*1e9)))
			} else {
				ticks = ticks0 + uint64(math.Round(local*hz))
			}
			// what the peers / reference store report
			vu := fr.Unrotate(a.v).Unit()
			perr := vu.Scale(a.bias).Add(geo.Vec3{X: r.NormFloat64(), Y: r.NormFloat64(), Z: r.NormFloat64()}.Scale(s.posSigmaM))
			pRep := p.Add(perr)
			var eb geo.Vec3
			for _, pe := range a.peerECEF {
				eb = eb.Add(pRep.Sub(pe).Unit())
			}
			eb = eb.Scale(1 / float64(len(a.peerECEF))).Unit()
			// the peers' τ: true τ, plus the error the peers make by using
			// the reported position (their ranges), plus clock noise
			var tauErr float64
			for _, pe := range a.peerECEF {
				tauErr += (pRep.Sub(pe).Norm() - p.Sub(pe).Norm()) / cLight
			}
			tauErr = -tauErr/float64(len(a.peerECEF)) + r.NormFloat64()*s.tauSigmaNs*1e-9
			_ = tau
			tauNs := unixBase + int64(math.Round((tt+tauErr)*1e9))
			ps := s.posSigmaM*s.posSigmaM + s.latBiasM*s.latBiasM
			out = append(out, Obs{TauNs: tauNs, Ticks: ticks, Epoch: 1, Class: s.class, Domain: 1, Anchored: true, Peers: 3,
				Key: a.key, P: pRep, CovP: [6]float64{ps, 0, 0, ps, 0, ps}, Ebar: eb,
				VarTauM2: math.Pow(s.tauSigmaNs*1e-9*cLight, 2)})
		}
	}
	return out
}

func gpsSynth(seed uint64) synth {
	return synth{seed: seed, class: model.ClockGPSUTC, truth: geo.LLH{Lat: 51.2, Lon: 9.1, H: 210}, aircraft: 60,
		radiusKm: 250, durS: 300, rateHz: 0.5, tauSigmaNs: 15, jitterNs: 30, quantNs: 1, posSigmaM: 5, latBiasM: 20,
		peerDistKm: 60}
}

func sdrSynth(seed uint64) synth {
	return synth{seed: seed, class: model.ClockSDR12, truth: geo.LLH{Lat: 51.2, Lon: 9.1, H: 210}, aircraft: 60,
		radiusKm: 250, durS: 1800, rateHz: 0.5, tauSigmaNs: 80, jitterNs: 400, quantNs: 416.7, driftPPM: 63,
		rampPPMs: 0.008, posSigmaM: 10, latBiasM: 30, peerDistKm: 60}
}

func horiz(sol Solution, truth geo.LLH) (float64, float64) {
	f := geo.NewFrame(truth)
	d := f.ToENU(sol.ECEF)
	return math.Hypot(d.X, d.Y), d.Z
}

func solveSynth(t testing.TB, s synth, seedOff float64) Solution {
	t.Helper()
	obs := s.gen()
	f := geo.NewFrame(s.truth)
	seed := f.FromENU(geo.Vec3{X: seedOff, Y: -seedOff / 2})
	sol, err := Solve(Input{Obs: obs, Seed: seed, HeightHAE: s.truth.H + 7, HeightSigmaM: 20}, DefaultConfig())
	if err != nil {
		t.Fatalf("solve: %v (n=%d)", err, len(obs))
	}
	return sol
}

func TestSolveSynthGPS(t *testing.T) {
	s := gpsSynth(1)
	sol := solveSynth(t, s, 20e3)
	h, v := horiz(sol, s.truth)
	t.Logf("formal %.1f %.1f indep %.1f %.1f jk %.1f %.1f groups %d", math.Sqrt(sol.FormalENU[0]+sol.FormalENU[4]), math.Sqrt(sol.FormalENU[8]),
		math.Sqrt(sol.IndepENU[0]+sol.IndepENU[4]), math.Sqrt(sol.IndepENU[8]), math.Sqrt(sol.JackknifeENU[0]+sol.JackknifeENU[4]), math.Sqrt(sol.JackknifeENU[8]), sol.JackknifeGroups)
	t.Logf("GPS: err %.2f m (dz %.1f) σh %.2f σv %.1f jitter %.1f ns used %d/%d rej %d ac %d az %.0f° halves %.1f m χ² %.1f iters %d",
		h, v, sol.SigmaHM, sol.SigmaVM, sol.JitterNs, sol.Used, sol.N, sol.Rejected, sol.Aircraft, sol.AzSpreadDeg, sol.HalvesM, sol.HalvesChi2, sol.Iterations)
	if h > 10 || sol.SigmaHM > 25 || h > 4*sol.SigmaHM {
		t.Fatalf("GPS survey error %.1f m σ %.1f m", h, sol.SigmaHM)
	}
}

func TestSolveSynthSDR(t *testing.T) {
	s := sdrSynth(2)
	sol := solveSynth(t, s, 30e3)
	h, v := horiz(sol, s.truth)
	t.Logf("SDR: err %.2f m (dz %.1f) σh %.2f σv %.1f jitter %.1f ns used %d/%d rej %d ac %d seg %d brk %d halves %.1f χ² %.1f",
		h, v, sol.SigmaHM, sol.SigmaVM, sol.JitterNs, sol.Used, sol.N, sol.Rejected, sol.Aircraft, sol.Segments, sol.Breaks, sol.HalvesM, sol.HalvesChi2)
	if h > 100 || h > 4*sol.SigmaHM {
		t.Fatalf("SDR survey error %.1f m σ %.1f m", h, sol.SigmaHM)
	}
}
