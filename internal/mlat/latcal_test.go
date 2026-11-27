package mlat

import (
	"math"
	"testing"

	"airview/internal/mlat/geo"
	"airview/internal/mlat/ref"
	"airview/internal/model"
)

// TestLatencyCalParams: the file block maps onto the quality parameters.
func TestLatencyCalParams(t *testing.T) {
	p := latencyCalParams(model.AggMLATLatencyCal{Enabled: true, MinGain: 0.1, ZEnable: 4, ZDisable: 1.5, TreatFraction: 0.3,
		MinCalSamples: 7, MinArmSamples: 50, MinArmAircraft: 5, WindowH: 2, PriorSigmaS: 0.2, CalibratedSigmaS: 0.05}, 0x1234)
	if p.Salt != 0x1234^0x5a || p.MinGain != 0.1 || p.ZEnable != 4 || p.ZDisable != 1.5 || p.TreatFraction != 0.3 ||
		p.MinCalSamples != 7 || p.MinArmSamples != 50 || p.MinArmAircraft != 5 || p.WindowNs != 2*3600e9 ||
		p.PriorSigmaS != 0.2 || p.CalibratedSigmaS != 0.05 {
		t.Fatalf("params %+v", p)
	}
}

// TestLatencySample: an MLAT position 50 m ahead along track of a 200 m/s
// reference gives AlongM 50 and a latency sample of 0.25 s more than applied.
func TestLatencySample(t *testing.T) {
	llh := geo.LLH{Lat: 52, Lon: 5, H: 10000}
	fr := geo.NewFrame(llh)
	st := ref.State{Pos: fr.Origin, Lat: 52, Lon: 5, HAE: 10000, LatencyS: 0.1, NACp: 9}
	// heading north-east at 200 m/s
	ve := geo.Vec3{X: 200 / math.Sqrt2, Y: 200 / math.Sqrt2}
	st.Vel = fr.Unrotate(ve)
	st.CovENU = [9]float64{9, 0, 0, 0, 9, 0, 0, 0, 25}
	ahead := fr.Unrotate(geo.Vec3{X: 50 / math.Sqrt2, Y: 50 / math.Sqrt2})
	cov := fr.CovToECEF([9]float64{16, 0, 0, 0, 16, 0, 0, 0, 100})
	s, ok := latencySample(0xabc, 1, &st, st.Pos.Add(ahead), cov)
	if !ok {
		t.Fatal("no sample")
	}
	if math.Abs(s.AlongM-50) > 0.01 || math.Abs(s.SpeedMps-200) > 0.01 || math.Abs(s.SigmaAlongM-5) > 0.01 || s.AppliedS != 0.1 || s.NACp != 9 {
		t.Fatalf("sample %+v", s)
	}
	// A stationary reference gives no sample.
	st.Vel = geo.Vec3{}
	if _, ok := latencySample(0xabc, 1, &st, st.Pos, cov); ok {
		t.Fatal("sample without speed")
	}
}
