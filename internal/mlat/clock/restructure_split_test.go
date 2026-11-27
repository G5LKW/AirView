package clock

import (
	"math"
	"math/rand/v2"
	"testing"

	"airview/internal/model"
)

// TestRestructureSplitOldComponentBehind is the regression test of finding
// SCALE-01: a dense component larger than MaxComponentStations, one epoch
// behind the restructure time, is split into several parts. The first part
// takes the old component over and rewrites its stations' offsets; the
// later parts have no owner. Before the fix the later parts predicted the
// old component to tE after that rewrite, so predictBlock walked the old
// station list with the new offsets: a panic (slice out of range) at 1,000
// feeders, or silently wrong covariances. Every copied state must now equal
// the old component predicted to tE with its own layout.
func TestRestructureSplitOldComponentBehind(t *testing.T) {
	for _, n := range []int{41, 97, 160} {
		f, err := New(Config{OriginUnixNs: 1, MaxComponentStations: 20, PartitionStations: 12, Workers: 1})
		if err != nil {
			t.Fatal(err)
		}
		const tE = 50.0
		set := gridStations(n)
		for i, s := range set {
			for len(f.st) <= int(s.idx) {
				f.st = append(f.st, nil)
			}
			f.st[s.idx] = s
			cls := model.ClockSDR12
			if i%5 == 0 {
				cls = model.ClockHW12
			}
			s.setInfo(StationInfo{Class: cls, Pos: s.info.Pos, Located: true})
			s.hasEpoch = true
			s.lastObsS = tE - 0.5
		}
		// a chain of co-observations keeps the whole set one domain
		for i := 1; i < len(set); i++ {
			a, b := set[i-1].idx, set[i].idx
			f.edges[uint64(a)<<32|uint64(b)] = tE - 1
		}
		old := buildComponent(1, 1, set, tE-1, f.hostVar())
		f.nextID = 1
		f.comps = []*component{old}
		// a full, correlated covariance and a non-zero state
		r := rand.New(rand.NewPCG(uint64(n), 7))
		m := old.n
		M := make([]float64, m*m)
		for i := range M {
			M[i] = r.NormFloat64()
		}
		for i := 0; i < m; i++ {
			old.x[i] = r.NormFloat64() * 10
			for j := 0; j < m; j++ {
				v := 0.0
				for k := 0; k < m; k++ {
					v += M[i*m+k] * M[j*m+k]
				}
				old.P[i*m+j] = v / float64(m)
			}
			old.P[i*m+i] += 1
		}
		// reference: a deep copy predicted to tE with the original layout
		ref := &component{sts: old.sts, n: m, x: append([]float64(nil), old.x...), P: append([]float64(nil), old.P...), tE: old.tE}
		refOff := map[*station]int{}
		refLay := map[*station][numKinds]int8{}
		for _, s := range set {
			refOff[s], refLay[s] = s.off, s.layout
		}
		ref.predict(tE)

		f.restructure(tE)

		if len(f.comps) < 2 {
			t.Fatalf("n=%d: domain not partitioned (%d components)", n, len(f.comps))
		}
		seen := 0
		for _, c := range f.comps {
			if c.tE != tE {
				t.Errorf("n=%d: component %d at %v, want %v", n, c.id, c.tE, tE)
			}
			for _, s := range c.sts {
				seen++
				if s.comp != c {
					t.Fatalf("n=%d: station %d not owned by its component", n, s.idx)
				}
				for k := stateKind(0); k < numKinds; k++ {
					lo, ln := refLay[s][k], s.layout[k]
					if lo < 0 || ln < 0 {
						continue
					}
					ri := refOff[s] + int(lo)
					ni := s.off + int(ln)
					if d := math.Abs(c.x[ni] - ref.x[ri]); d > 1e-9*math.Max(1, math.Abs(ref.x[ri])) {
						t.Fatalf("n=%d: station %d state %d mean %v, want %v", n, s.idx, k, c.x[ni], ref.x[ri])
					}
					want := ref.P[ri*m+ri]
					if d := math.Abs(c.P[ni*c.n+ni] - want); d > 1e-9*math.Max(1, want) {
						t.Fatalf("n=%d: station %d state %d variance %v, want %v", n, s.idx, k, c.P[ni*c.n+ni], want)
					}
				}
			}
		}
		if seen != n {
			t.Errorf("n=%d: %d stations in the components", n, seen)
		}
	}
}

// TestStepRecoversPanic: a panic inside Step (here a corrupt component) is
// recovered: OnPanic is told, every clock state is reset, and the next
// Steps run normally (finding SCALE-01: the panic killed the process).
func TestStepRecoversPanic(t *testing.T) {
	var got any
	f, err := New(Config{OriginUnixNs: 1, Workers: 1, OnPanic: func(v any, _ []byte) { got = v }})
	if err != nil {
		t.Fatal(err)
	}
	f.Step(1)
	s := f.station(5, true)
	s.setInfo(StationInfo{Class: model.ClockGPSUTC, Located: true})
	s.hasEpoch = true
	s.mode = modeAnchor
	bad := &component{id: 9, domain: 9, sts: []*station{s}, n: 4, x: make([]float64, 1), P: make([]float64, 1), tE: 0.5}
	s.comp, s.off, s.layout[kTheta], s.layout[kPhi] = bad, 3, 0, 1
	f.comps = []*component{bad}
	f.Step(2)
	if got == nil {
		t.Fatal("OnPanic not called")
	}
	if st := f.Stats(); st.PanicResets != 1 {
		t.Errorf("PanicResets %d", st.PanicResets)
	}
	if s.comp != nil || !s.fresh || s.hasEpoch || s.mode != modeFree {
		t.Errorf("station not reset: comp %v fresh %v epoch %v mode %v", s.comp != nil, s.fresh, s.hasEpoch, s.mode)
	}
	for i := 3; i < 10; i++ {
		f.Step(float64(i))
	}
	if st := f.Stats(); st.PanicResets != 1 || st.Epochs < 9 {
		t.Errorf("after recovery: %+v", st)
	}
}
