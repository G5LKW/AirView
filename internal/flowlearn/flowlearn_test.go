package flowlearn

import (
	"math"
	"testing"

	"airview/internal/geo"
)

// fly simulates one aircraft at 1 Hz: west along 50N at 300 kt from 8.6E,
// turning right at 1.5 deg/s once west of 8.0E until it heads north. It
// returns the next start time.
func fly(l *Learner, icao uint32, t0 int64, turn bool) int64 {
	lat, lon, trk, gs := 50.0, 8.6, 270.0, 300.0
	t := t0
	for i := 0; i < 500; i++ {
		l.Observe(icao, t, lat, lon, 10000, trk, gs)
		if turn && lon <= 8.0 && trk != 0 {
			trk = math.Mod(trk+1.5, 360)
			if trk > 358.5 || trk < 1 {
				trk = 0
			}
		}
		lat, lon = geo.Destination(lat, lon, trk, gs*1.852/3600)
		t += 1000
	}
	return t
}

func TestLearnsTurnAndCorrectsItself(t *testing.T) {
	l := New()
	t0 := int64(1_800_000_000_000)
	for i := 0; i < 60; i++ {
		t0 = fly(l, uint32(0x400000+i), t0, true)
	}
	st := l.Stats()
	if st.Cells == 0 || st.Samples < 1000 {
		t.Fatalf("%+v", st)
	}
	for _, sk := range st.Skill {
		if sk.N < 100 || sk.LearnedM >= sk.StraightM || sk.Improvement <= 0.2 || sk.Weight <= 0.5 {
			t.Errorf("horizon %d s: %+v", sk.HorizonS, sk)
		}
	}
	// A new aircraft just before the turn: the learned path bends north, dead reckoning does not.
	lat, lon := 50.0, 8.05
	p := l.Predict(lat, lon, 10000, 270, 300, 60, 10, t0)
	end := p.Points[len(p.Points)-1]
	if !p.Learned || end.Trk < 300 && end.Trk > 30 {
		t.Fatalf("learned path did not turn: %+v", p)
	}
	// truth: fly it
	tl, tn, tt := lat, lon, 270.0
	for s := 0; s < 60; s++ {
		if tn <= 8.0 && tt != 0 {
			tt = math.Mod(tt+1.5, 360)
		}
		tl, tn = geo.Destination(tl, tn, tt, 300*1.852/3600)
	}
	sl, sn := geo.Destination(lat, lon, 270, 300*1.852/3600*60)
	eL := geo.HaversineKm(end.Lat, end.Lon, tl, tn)
	eS := geo.HaversineKm(sl, sn, tl, tn)
	t.Logf("60 s: learned off by %.2f km, straight by %.2f km; skill %+v", eL, eS, st.Skill)
	if eL >= eS*0.6 {
		t.Errorf("learned %.2f km vs straight %.2f km", eL, eS)
	}
}

// Where traffic flies straight, the learner must not invent turns and its
// errors stay those of dead reckoning.
func TestStraightTrafficStaysStraight(t *testing.T) {
	l := New()
	t0 := int64(1_800_000_000_000)
	for i := 0; i < 20; i++ {
		t0 = fly(l, uint32(0x500000+i), t0, false)
	}
	p := l.Predict(50, 8.4, 10000, 270, 300, 120, 30, t0)
	end := p.Points[len(p.Points)-1]
	if math.Abs(geo.AngleDiff(end.Trk, 270)) > 1 {
		t.Fatalf("invented a turn: %+v", end)
	}
	for _, sk := range l.Stats().Skill {
		if sk.LearnedM > 50 || sk.StraightM > 50 {
			t.Errorf("%+v", sk)
		}
	}
	// No traffic at all: a straight line, nothing learned.
	if p := New().Predict(10, 10, 5000, 90, 200, 60, 10, t0); p.Learned || len(p.Points) != 7 {
		t.Fatalf("%+v", p)
	}
}

func BenchmarkObserve(b *testing.B) {
	l := New()
	t := int64(1_800_000_000_000)
	for i := 0; i < b.N; i++ {
		l.Observe(uint32(i%2000), t+int64(i/2000)*1000, 50+float64(i%2000)*0.001, 8, 30000, 270, 450)
	}
}
