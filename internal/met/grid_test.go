package met

import (
	"math"
	"testing"

	"airview/internal/store"
)

func TestBuildGrid(t *testing.T) {
	nan := math.NaN()
	t0 := int64(1_800_000_000)
	obs := []store.MetObs{
		// one cell at 50.2N 8.3E: two aircraft, wind from 270 at 40 and 60 kt -> 270/50
		{T: t0, ICAO: 1, Lat: 50.2, Lon: 8.3, Alt: 30000, WindDir: 270, WindKt: 40, OATC: -45},
		{T: t0 + 60, ICAO: 2, Lat: 50.3, Lon: 8.4, Alt: 31000, WindDir: 270, WindKt: 60, OATC: -47},
		// opposite winds cancel (vector mean), temperature only from one
		{T: t0, ICAO: 3, Lat: 52.2, Lon: 8.3, Alt: 30000, WindDir: 90, WindKt: 50, OATC: nan},
		{T: t0, ICAO: 4, Lat: 52.2, Lon: 8.3, Alt: 30000, WindDir: 270, WindKt: 50, OATC: -44},
		// outside the band / window: ignored
		{T: t0, ICAO: 5, Lat: 50.2, Lon: 8.3, Alt: 20000, WindDir: 90, WindKt: 90, OATC: -20},
		{T: t0 - 7200, ICAO: 6, Lat: 50.2, Lon: 8.3, Alt: 30000, WindDir: 90, WindKt: 90, OATC: -20},
		// a lone observation does not make a cell
		{T: t0, ICAO: 7, Lat: 40.2, Lon: 2.3, Alt: 30000, WindDir: 0, WindKt: 20, OATC: -40},
	}
	g := BuildGrid(obs, GridQuery{LevelFt: 30000}, (t0-3600)*1000, (t0+3600)*1000)
	if g.Samples != 5 || len(g.Cells) != 2 {
		t.Fatalf("samples %d cells %+v", g.Samples, g.Cells)
	}
	c := g.Cells[0]
	if c.NAc != 2 || c.NWind != 2 || *c.WindDir != 270 || math.Abs(*c.WindKt-50) > 0.1 {
		t.Fatalf("%+v dir %v kt %v", c, *c.WindDir, *c.WindKt)
	}
	// -45 at FL300 and -47 at FL310 reduced to FL300 (+1.98): mean -45.0
	if math.Abs(*c.TempC-(-45.0)) > 0.06 || math.Abs(*c.ISADevC-(-45.0-ISATempC(30000))) > 0.11 {
		t.Fatalf("temp %v isa dev %v", *c.TempC, *c.ISADevC)
	}
	c = g.Cells[1]
	if *c.WindKt > 0.1 || c.TempC != nil || c.NTemp != 1 {
		t.Fatalf("opposite winds: %+v", c)
	}
	if ISATempC(0) != 15 || ISATempC(40000) != -56.5 || math.Abs(ISATempC(10000)-(-4.8)) > 1e-9 {
		t.Fatal("ISA")
	}
}

func TestHazardLog(t *testing.T) {
	l := NewHazardLog()
	sev := uint8(2)
	l.Add(Hazard{T: 1000, ICAO: "400001", Turbulence: &sev})
	l.Add(Hazard{T: 30000, ICAO: "400001"}) // thinned: same aircraft within a minute
	l.Add(Hazard{T: 70000, ICAO: "400001"})
	l.Add(Hazard{T: 2000, ICAO: "400002"})
	got := l.Since(0)
	if len(got) != 3 || got[0].T != 2000 || l.Count() != 3 {
		t.Fatalf("%+v", got)
	}
	if !got[2].Severe() || got[1].Severe() {
		t.Fatal("Severe")
	}
	if len(l.Since(5000)) != 1 {
		t.Fatal("Since")
	}
}
