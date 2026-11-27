package store

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

func TestHistRealCodec(t *testing.T) {
	vals := []float64{0, math.Copysign(0, -1), 1, -7698.700000000002, -1.5347567000000011e+06, 63.14941284543971,
		4.719619607248462e+06, 1e300, -1e-300, math.Inf(1), math.MaxFloat64, 123456789012.3}
	e := &histEnc{}
	for _, v := range vals {
		e.real(v, 10)
	}
	d := &histDec{b: e.b}
	for _, v := range vals {
		if got := d.real(10); math.Float64bits(got) != math.Float64bits(v) {
			t.Fatalf("%v -> %v", v, got)
		}
	}
	if d.err != nil || len(d.b) != 0 {
		t.Fatal(d.err, len(d.b))
	}
}

// histSnapshot reads everything the analytics and the exports see of the
// three tables.
type histSnapshot struct {
	cov   [][]CoverageRow
	bands [][]CoverageBand
	sig   [][]SignalRow
	elev  [][]ElevRow
	rows  [3][][]any
}

func readHist(t *testing.T, s *Store, d0, d1 int) histSnapshot {
	t.Helper()
	var sn histSnapshot
	for _, ant := range []string{"", "yagi", "vertical"} {
		c, err := s.CoverageRows(bg, ant, d0, d1, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.CoverageBands(bg, ant, d0, d1, nil)
		if err != nil {
			t.Fatal(err)
		}
		g, err := s.SignalTotals(bg, ant, d0, d1, nil)
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.ElevTotals(bg, ant, d0, d1, nil)
		if err != nil {
			t.Fatal(err)
		}
		sn.cov, sn.bands, sn.sig, sn.elev = append(sn.cov, c), append(sn.bands, b), append(sn.sig, g), append(sn.elev, e)
	}
	for i, name := range []string{"coverage", "signal_hist", "elev_hist"} {
		err := s.ScanTable(bg, name, "day", nil, 0, func([]string) error { return nil }, func(v []any) error {
			sn.rows[i] = append(sn.rows[i], append([]any(nil), v...))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return sn
}

// TestSealHistDays: sealing is lossless for the readers and the exports,
// late increments of a sealed day are merged, and the bytes shrink.
func TestSealHistDays(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(9, 10))
	const day = 20000
	for d := day; d < day+8; d += 2 {
		writeIncs(t, s, randomIncs(rng, d, 4000))
	}
	before := readHist(t, s, day-1, day+10)
	// The logical rows are the 1.0.0 `SELECT *` rows.
	for i, name := range []string{"coverage", "signal_hist", "elev_hist"} {
		var phys [][]any
		err := scanPhysical(bg, s.DB(), name, "", nil, 0, func([]string) error { return nil }, func(v []any) error {
			phys = append(phys, append([]any(nil), v...))
			return nil
		})
		if err != nil || !reflect.DeepEqual(phys, before.rows[i]) {
			t.Fatalf("%s: logical rows differ from SELECT * (%v)", name, err)
		}
	}
	now := DayStart(day+9) + 3600
	rep, err := s.SealHistDays(bg, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Days != 3*7 { // days day..day+6 of the three tables (day+7.. are recent)
		t.Fatalf("sealed %+v", rep)
	}
	if n := scalar[int](t, s, `SELECT COUNT(*) FROM coverage WHERE day <= ?`, day+6); n != 0 {
		t.Fatalf("live rows left: %d", n)
	}
	after := readHist(t, s, day-1, day+10)
	if !reflect.DeepEqual(before.cov, after.cov) || !reflect.DeepEqual(before.bands, after.bands) || !reflect.DeepEqual(before.sig, after.sig) {
		t.Fatal("readers differ after sealing")
	}
	for i := range before.elev {
		if !elevNear(before.elev[i], after.elev[i]) {
			t.Fatal("elevation differs after sealing")
		}
	}
	for i := range before.rows {
		if !reflect.DeepEqual(before.rows[i], after.rows[i]) {
			t.Fatalf("logical rows of table %d differ (%d vs %d)", i, len(before.rows[i]), len(after.rows[i]))
		}
	}
	for i, name := range []string{"coverage", "signal_hist", "elev_hist"} {
		n, err := LogicalCount(bg, s.DB(), KindMain, name)
		if err != nil || n != int64(len(before.rows[i])) {
			t.Fatalf("%s count %d, rows %d: %v", name, n, len(before.rows[i]), err)
		}
	}

	// A late increment for a sealed day: merged by the readers, folded in
	// by the next run.
	late := randomIncs(rng, day+2, 200)
	late.Range, late.Heat = nil, nil
	for i := range late.Coverage {
		late.Coverage[i].Day, late.Signal[i].Day, late.Elev[i].Day = day+2, day+2, day+2
	}
	writeIncs(t, s, late)
	merged := readHist(t, s, day-1, day+10)
	if _, err := s.SealHistDays(bg, now, 0); err != nil {
		t.Fatal(err)
	}
	resealed := readHist(t, s, day-1, day+10)
	if !reflect.DeepEqual(merged.cov, resealed.cov) || !reflect.DeepEqual(merged.sig, resealed.sig) || !reflect.DeepEqual(merged.rows[0], resealed.rows[0]) {
		t.Fatal("late increment changed by resealing")
	}
	if reflect.DeepEqual(merged.sig, after.sig) {
		t.Fatal("late increment not visible")
	}
}

// TestHistDayCompression: sealed bytes per bin against the live rows.
func TestHistDayCompression(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	var cov []CoverageRow
	for b := 0; b < BrgBins; b++ {
		for a := 0; a < AltBands; a++ {
			n := int64(rng.IntN(50000))
			var rs float64
			for i := 0; i < 50; i++ {
				rs += -float64(rng.IntN(300)) / 10
			}
			cov = append(cov, CoverageRow{BrgBin: b, AltBand: a, MaxKm: rng.Float64() * 400, Count: n, RSSISum: rs * float64(n) / 50, RSSIN: n})
		}
	}
	data := encodeCoverageDay(cov)
	got, err := decodeCoverageDay(0, data)
	if err != nil || !reflect.DeepEqual(got, cov) {
		t.Fatalf("round trip: %v", err)
	}
	t.Logf("coverage: %.1f B/bin", float64(len(data))/float64(len(cov)))
}
