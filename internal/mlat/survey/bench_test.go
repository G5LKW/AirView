package survey

import (
	"testing"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// BenchmarkObserveNoTarget is the tap's cost for a group without a survey
// target while surveys run (every group of the network pays it). Budget:
// ≤ 50 ns, 0 allocations.
func BenchmarkObserveNoTarget(b *testing.B) {
	s, _, _ := newAPI(b, Config{})
	for st := model.StationIdx(1000); st < 1064; st++ {
		s.SetStation(st, Station{Class: model.ClockSDR12, Timestamps: true})
	}
	s.Tick()
	g := &ingest.Group{Kind: model.FrameModeSLong, Key: 0x4840d6}
	for i := 0; i < 8; i++ {
		g.Copies = append(g.Copies, ingest.Copy{Station: model.StationIdx(2 + i), Timed: true})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Observe(g)
	}
}

// BenchmarkSolve is one evaluation of a full job (15 000 observations, the
// MaxObs bound). Budget: ≤ 2 s on one core (evaluations run once a minute
// per job on Workers background goroutines).
func BenchmarkSolve(b *testing.B) {
	sy := sdrSynth(9)
	sy.durS = 500
	obs := sy.gen()
	if len(obs) > 15000 {
		obs = obs[:15000]
	}
	f := geo.NewFrame(sy.truth)
	in := Input{Obs: obs, Seed: f.FromENU(geo.Vec3{X: 20e3, Y: -10e3}), HeightHAE: sy.truth.H, HeightSigmaM: 20}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Solve(in, DefaultConfig()); err != nil {
			b.Fatal(err)
		}
	}
}
