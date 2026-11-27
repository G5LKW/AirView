package store

import (
	"database/sql"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// TestKBNMatchesSQLiteSum: the Go summation equals SQLite's SUM() bit for
// bit (same values, same order).
func TestKBNMatchesSQLiteSum(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(1, 2))
	if err := s.Write(bg, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE kbn(g INTEGER, i INTEGER, v REAL NOT NULL, PRIMARY KEY(g, i)) WITHOUT ROWID`); err != nil {
			return err
		}
		for g := 0; g < 20; g++ {
			for i := 0; i < 500; i++ {
				v := math.Round(rng.NormFloat64()*300) / 10 * float64(1+rng.IntN(1000))
				if rng.IntN(7) == 0 {
					v = rng.Float64() * 1e9
				}
				if _, err := tx.Exec(`INSERT INTO kbn VALUES(?, ?, ?)`, g, i, v); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 20; g++ {
		want := scalar[float64](t, s, `SELECT SUM(v) FROM kbn WHERE g = ?`, g)
		var k kbnSum
		rs, err := s.DB().Query(`SELECT v FROM kbn WHERE g = ? ORDER BY i`, g)
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			var v float64
			if err := rs.Scan(&v); err != nil {
				t.Fatal(err)
			}
			k.add(v)
		}
		rs.Close()
		if got := k.sum(); math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("group %d: kbn %v, SQLite %v", g, got, want)
		}
	}
}

func TestHeatTile(t *testing.T) {
	for _, c := range [][3]float64{{47.3, 8.5, 0}, {-89.99, -179.99, 0}, {89.99, 179.99, 0}} {
		cell := HeatCell(c[0], c[1])
		li, lo := cell>>16, cell&0xffff
		if got, want := HeatTile(cell), (li>>2)<<14|lo>>2; got != want {
			t.Fatalf("tile(%d) = %d, want %d", cell, got, want)
		}
	}
	// Cells of one 4x4 block share a tile, all antennas included (the key is (tile, cell, ant)).
	a, b := HeatCell(47.0+HeatCellDeg/2, 8.0+HeatCellDeg/2), HeatCell(47.0+3.5*HeatCellDeg, 8.0+3.5*HeatCellDeg)
	if HeatTile(a) != HeatTile(b) {
		t.Fatalf("tiles %d %d", HeatTile(a), HeatTile(b))
	}
}

// randomIncs returns increments for three antennas over two days, with
// repeated keys.
func randomIncs(rng *rand.Rand, day int, n int) PendingIncs {
	ants := []string{"", "yagi", "vertical"}
	var p PendingIncs
	for i := 0; i < n; i++ {
		ant := ants[rng.IntN(3)]
		d := day + rng.IntN(2)
		cnt := int64(1 + rng.IntN(50))
		rssi := -float64(rng.IntN(300)) / 10 * float64(cnt)
		alt := math.NaN()
		if rng.IntN(4) > 0 {
			alt = float64(rng.IntN(40000)) + 0.4
		}
		p.Coverage = append(p.Coverage, CoverageInc{Day: d, Ant: ant, BrgBin: rng.IntN(BrgBins), AltBand: rng.IntN(AltBands),
			MaxKm: rng.Float64() * 400, Count: cnt, RSSISum: rssi, RSSIN: cnt - int64(rng.IntN(2))})
		p.Signal = append(p.Signal, SignalHistInc{Day: d, Ant: ant, DistBin: rng.IntN(40), RSSIBin: -1 - rng.IntN(40), Count: cnt})
		p.Elev = append(p.Elev, ElevHistInc{Day: d, Ant: ant, ElevBin: rng.IntN(ElevBins), Count: cnt, RSSISum: rssi, ResidSum: rng.Float64() * 1000})
		p.Heat = append(p.Heat, HeatInc{Cell: HeatCell(46+rng.Float64()*2, 7+rng.Float64()*2), Ant: ant, Count: cnt,
			RSSISum: rssi, RSSIN: cnt, MinAlt: alt, MaxAlt: alt + float64(rng.IntN(3000)), LastT: int64(d)*86400 + int64(rng.IntN(86400))})
	}
	return p
}

func writeIncs(t *testing.T, s *Store, p PendingIncs) {
	t.Helper()
	s.MergeCoverage(p.Coverage)
	s.MergeSignalHist(p.Signal)
	s.MergeElevHist(p.Elev)
	s.MergeHeat(p.Heat)
	if len(p.Range) > 0 {
		s.AddRangeHours(p.Range)
	}
	mustFlush(t, s)
}

// TestOverlayEqualsWrite: stored rows + the overlay of pending increments
// equal the rows read after the increments were written.
func TestOverlayEqualsWrite(t *testing.T) {
	s := openTest(t, manual)
	rng := rand.New(rand.NewPCG(3, 4))
	const day = 20000
	writeIncs(t, s, randomIncs(rng, day, 3000))
	pend := randomIncs(rng, day, 800)
	nan := math.NaN()
	h := int64(day)*86400 + 7200
	pend.Range = []RangeHour{
		{T: h + 60, Ant: "yagi", MaxKm: 210, P99Km: nan, P95Km: nan, P90Km: nan, P50Km: nan, Positions: 10, Aircraft: 2, ResidDB: 3, ResidN: 10,
			DistHist: make([]uint32, DistHistBins), SectorMax: make([]float32, SectorBins)},
		{T: h + 3600, Ant: "yagi", MaxKm: 99, P99Km: 90, P95Km: 80, P90Km: 70, P50Km: 30, Positions: 3, Aircraft: 1, ResidDB: nan},
	}
	pend.Range[0].DistHist[7] = 5
	pend.Range[0].SectorMax[3] = 210
	s.AddRangeHours([]RangeHour{{T: h, Ant: "yagi", MaxKm: 150, P99Km: 140, P95Km: 130, P90Km: 120, P50Km: 50, Positions: 20, Aircraft: 4,
		ResidDB: 1, ResidN: 20, DistHist: make([]uint32, DistHistBins), SectorMax: make([]float32, SectorBins)}})
	mustFlush(t, s)

	type snap struct {
		cov   map[string][]CoverageRow
		bands map[string][]CoverageBand
		sig   map[string][]SignalRow
		elev  map[string][]ElevRow
		heat  map[string][]HeatRow
		rng   []RangeHour
	}
	read := func(p PendingIncs) snap {
		sn := snap{map[string][]CoverageRow{}, map[string][]CoverageBand{}, map[string][]SignalRow{}, map[string][]ElevRow{}, map[string][]HeatRow{}, nil}
		for _, ant := range []string{"", "yagi", "vertical"} {
			var err error
			if sn.cov[ant], err = s.CoverageRows(bg, ant, day, day+1, p.Coverage); err != nil {
				t.Fatal(err)
			}
			if sn.bands[ant], err = s.CoverageBands(bg, ant, day, day+1, p.Coverage); err != nil {
				t.Fatal(err)
			}
			if sn.sig[ant], err = s.SignalTotals(bg, ant, day, day+1, p.Signal); err != nil {
				t.Fatal(err)
			}
			if sn.elev[ant], err = s.ElevTotals(bg, ant, day, day+1, p.Elev); err != nil {
				t.Fatal(err)
			}
			cells, err := s.HeatCells(bg, ant)
			if err != nil {
				t.Fatal(err)
			}
			sn.heat[ant] = OverlayHeat(cells, ant, p.Heat)
		}
		rows, err := s.RangeHours(bg, "yagi", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		sn.rng = OverlayRangeHours(rows, "yagi", 0, 0, p.Range)
		return sn
	}
	over := read(pend)
	writeIncs(t, s, pend)
	written := read(PendingIncs{})
	for _, ant := range []string{"", "yagi", "vertical"} {
		if !reflect.DeepEqual(over.cov[ant], written.cov[ant]) {
			t.Errorf("coverage %q differs", ant)
		}
		if !reflect.DeepEqual(over.bands[ant], written.bands[ant]) {
			t.Errorf("coverage bands %q: %+v vs %+v", ant, over.bands[ant], written.bands[ant])
		}
		if !reflect.DeepEqual(over.sig[ant], written.sig[ant]) {
			t.Errorf("signal %q differs", ant)
		}
		if !elevNear(over.elev[ant], written.elev[ant]) {
			t.Errorf("elevation %q differs", ant)
		}
		if !heatEqual(over.heat[ant], written.heat[ant]) {
			t.Errorf("heat %q differs (%d vs %d cells)", ant, len(over.heat[ant]), len(written.heat[ant]))
		}
	}
	if !reflect.DeepEqual(rangeJSON(over.rng), rangeJSON(written.rng)) {
		t.Errorf("range rows:\n%+v\n%+v", over.rng, written.rng)
	}
}

// elevNear: counts equal, REAL sums equal up to summation order.
func elevNear(a, b []ElevRow) bool {
	if len(a) != len(b) {
		return false
	}
	rel := func(x, y float64) bool { return math.Abs(x-y) <= 1e-12*math.Max(1, math.Abs(y)) }
	for i := range a {
		if a[i].ElevBin != b[i].ElevBin || a[i].Count != b[i].Count || !rel(a[i].RSSISum, b[i].RSSISum) || !rel(a[i].ResidSum, b[i].ResidSum) {
			return false
		}
	}
	return true
}

func heatEqual(a, b []HeatRow) bool {
	if len(a) != len(b) {
		return false
	}
	same := func(x, y float64) bool { return x == y || (math.IsNaN(x) && math.IsNaN(y)) }
	for i := range a {
		x, y := a[i], b[i]
		if x.Cell != y.Cell || x.Count != y.Count || x.RSSISum != y.RSSISum || x.RSSIN != y.RSSIN || !same(x.MinAlt, y.MinAlt) ||
			!same(x.MaxAlt, y.MaxAlt) || x.LastT != y.LastT {
			return false
		}
	}
	return true
}

// rangeJSON maps NaN to a sentinel so rows compare with DeepEqual.
func rangeJSON(rows []RangeHour) []RangeHour {
	out := make([]RangeHour, len(rows))
	fix := func(v float64) float64 {
		if math.IsNaN(v) {
			return -12345
		}
		return v
	}
	for i, r := range rows {
		r.MaxKm, r.P99Km, r.P95Km, r.P90Km, r.P50Km, r.ResidDB = fix(r.MaxKm), fix(r.P99Km), fix(r.P95Km), fix(r.P90Km), fix(r.P50Km), fix(r.ResidDB)
		if len(r.DistHist) == 0 {
			r.DistHist = nil
		}
		if len(r.SectorMax) == 0 {
			r.SectorMax = nil
		}
		out[i] = r
	}
	return out
}
