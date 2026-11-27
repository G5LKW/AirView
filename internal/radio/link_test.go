package radio

import (
	"encoding/json"
	"math"
	"testing"

	"airview/internal/geo"
	"airview/internal/model"
)

func TestFSPL(t *testing.T) {
	// L = 32.45 + 20·log10(d_km) + 20·log10(f_MHz).
	near(t, "1 km @ 1090", FSPL1090DB(1), 32.45+20*math.Log10(1090), 0.01)
	near(t, "1 km @ 1090 abs", FSPL1090DB(1), 93.20, 0.01)
	near(t, "100 km", FSPL1090DB(100), 133.20, 0.01)
	near(t, "400 km", FSPL1090DB(400), 145.24, 0.01)
	near(t, "1 km @ 1000 MHz", FSPLDB(1, 1000), 92.45, 0.01)
	near(t, "doubling distance", FSPL1090DB(200)-FSPL1090DB(100), 6.0206, 1e-4)
	near(t, "Friis form", FSPL1090DB(37), 20*math.Log10(4*math.Pi*37000/WavelengthM), 1e-9)
	// Distances below 1 m are clamped; no −Inf or NaN.
	near(t, "zero distance", FSPL1090DB(0), FSPL1090DB(0.001), 0)
	near(t, "negative distance", FSPL1090DB(-5), FSPL1090DB(0.001), 0)
	near(t, "NaN distance", FSPL1090DB(math.NaN()), FSPL1090DB(0.001), 0)
}

func TestGaseousAbsorption(t *testing.T) {
	// ITU-R P.676-10 Annex 2 at 1.09 GHz, rp = rt = 1 (1013 hPa, 15 °C),
	// hand-computed:
	//   γo = [7.2/(1.1881 + 0.34) + 0.62/(52.91^1.16 + 0.83)]·1.1881e-3 = 0.005605 dB/km
	//   γw (7.5 g/m³) = 0.07558·1.1881·7.5e-4 = 6.73e-5 dB/km
	o, w := SpecificAttenuation(FreqMHz, 1013, 15, 7.5)
	near(t, "γ oxygen", o, 0.005605, 0.00001)
	near(t, "γ water", w, 6.73e-5, 0.05e-5)
	// Dry air: no water term. Thin air: much less oxygen absorption (∝ p²,
	// partly offset by the cold): about a fifth at 10 km, which is what the
	// exponential model with ho = 5.2 km assumes (e^(−10/5.21) = 0.15).
	_, w0 := SpecificAttenuation(FreqMHz, 1013, 15, 0)
	near(t, "dry", w0, 0, 0)
	oHigh, _ := SpecificAttenuation(FreqMHz, 264, -56.5, 0) // ~10 km
	if !(oHigh > o/8 && oHigh < o/4) {
		t.Errorf("oxygen attenuation at 264 hPa = %v, sea level %v", oHigh, o)
	}
	if a, b := SpecificAttenuation(60000, 1013, 15, 7.5); !math.IsNaN(a) || !math.IsNaN(b) {
		t.Error("outside the validity range (f > 54 GHz) the result must be NaN")
	}
	if a, b := SpecificAttenuation(FreqMHz, 0, 15, 7.5); a != 0 || b != 0 {
		t.Error("vacuum does not absorb")
	}

	g := StandardGas()
	near(t, "standard γo", g.OxygenDBPerKm, 0.005605, 0.00002)
	near(t, "ho", g.OxygenScaleKm, 6.1/1.17, 0.005) // P.676-10 eq. (25a) at rp = 1
	near(t, "hw", g.WaterScaleKm, 1.66, 0)

	// Short horizontal path at sea level: γ·d.
	near(t, "10 km at sea level", GaseousLossDB(10, 0, 0, 0), 10*(g.OxygenDBPerKm+g.WaterDBPerKm), 0.0005)
	// Vertical path through the whole atmosphere: γo·ho + γw·hw = 0.0293 dB
	// (the zenith attenuation of P.676 eq. 24); to 12 km it is
	// γo·ho·(1 − e^(−12/5.21)) + γw·hw = 0.0264 dB.
	zen := g.OxygenDBPerKm*g.OxygenScaleKm + g.WaterDBPerKm*g.WaterScaleKm
	near(t, "zenith total", GaseousLossDB(0, 0, 80000, 0), zen, 0.0015)
	near(t, "zenith to 12 km", GaseousLossDB(0, 0, 12000, 0), 0.0264, 0.0005)
	// Long grazing paths to an airliner at 11 km: a few tenths of a dB at
	// 100 km, about 1.2 dB at 400 km — far more than the cosecant law's
	// zenith value because the ray stays in dense air.
	l100 := GaseousLossDB(100, 10, 11000, 0)
	l400 := GaseousLossDB(400, 10, 11000, 0)
	if l100 < 0.18 || l100 > 0.30 {
		t.Errorf("gas loss at 100 km = %.3f dB", l100)
	}
	if l400 < 1.0 || l400 > 1.5 {
		t.Errorf("gas loss at 400 km = %.3f dB", l400)
	}
	// Monotonic in distance, symmetric in the end points, zero for no path.
	prev := 0.0
	for d := 10.0; d <= 420; d += 10 {
		l := GaseousLossDB(d, 10, 11000, 0)
		if l <= prev {
			t.Fatalf("gas loss not increasing at %v km: %v after %v", d, l, prev)
		}
		prev = l
	}
	near(t, "reciprocal", GaseousLossDB(250, 11000, 10, 0), GaseousLossDB(250, 10, 11000, 0), 1e-9)
	near(t, "no path", GaseousLossDB(0, 50, 50, 0), 0, 0)
	near(t, "negative distance", GaseousLossDB(-10, 0, 3000, 0), GaseousLossDB(0, 0, 3000, 0), 0)

	// A model built from surface weather: hot humid air absorbs more water
	// vapour energy, and a station on a mountain sits above part of the gas.
	humid := NewGasModel(1010, 30, 80, 0)
	if !(humid.WaterDBPerKm > 3*g.WaterDBPerKm) {
		t.Errorf("humid water attenuation %v vs standard %v", humid.WaterDBPerKm, g.WaterDBPerKm)
	}
	std := NewGasModel(1013.25, 15, 58.5, 0) // ≈ 7.5 g/m³
	near(t, "weather model ≈ standard", std.PathLossDB(300, 10, 11000, 0), g.PathLossDB(300, 10, 11000, 0), 0.01)
	mtn := NewGasModel(700, 0, 50, 3000)
	near(t, "reference height", mtn.RefHeightM, 3000, 0)
	near(t, "at the reference height", mtn.PathLossDB(5, 3000, 3000, 0), 5*(mtn.OxygenDBPerKm+mtn.WaterDBPerKm), 0.0003)
	if (GasModel{}).PathLossDB(300, 10, 11000, 0) != 0 {
		t.Error("the zero GasModel absorbs nothing")
	}
	if _, err := json.Marshal(humid); err != nil {
		t.Errorf("json: %v", err)
	}
}

func TestTransponderClasses(t *testing.T) {
	c := TransponderClasses()
	if len(c) != 4 {
		t.Fatalf("len = %d", len(c))
	}
	want := []struct {
		w, dbm float64
		name   string
	}{{70, 48.45, "70 W"}, {125, 50.97, "125 W"}, {250, 53.98, "250 W"}, {500, 56.99, "500 W"}}
	for i, x := range want {
		if c[i].PowerW != x.w || c[i].Name != x.name || c[i].Note == "" {
			t.Errorf("class %d: %+v", i, c[i])
		}
		near(t, "class dBm", c[i].PowerDBm, x.dbm, 0.01)
	}
	// 18.5, 21, 24 and 27 dBW.
	near(t, "70 W", WattsToDBm(TxPowerLowW)-30, 18.5, 0.05)
	near(t, "125 W", WattsToDBm(TxPowerMinW)-30, 21, 0.05)
	near(t, "250 W", WattsToDBm(TxPowerTypicalW)-30, 24, 0.05)
	near(t, "500 W", WattsToDBm(TxPowerMaxW)-30, 27, 0.05)
}

func TestLinkNoise(t *testing.T) {
	// No losses, no LNA, 6 dB receiver: Trx = 290·(10^0.6 − 1) = 864.5 K,
	// Tsys = 150 + 864.5 = 1014.5 K, N = kTB = −105.53 dBm in 2 MHz.
	l := NewLink(model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30}, 20, LinkParams{})
	near(t, "Tsys", l.SystemTempK, 1014.5, 0.1)
	near(t, "system NF", l.SystemNFDB, 6, 1e-9)
	near(t, "noise floor", l.NoiseFloorDBm, -105.53, 0.01)
	near(t, "sensitivity", l.SensitivityDBm, -95.53, 0.01)
	near(t, "tx power", l.TxPowerDBm, 53.98, 0.01)
	near(t, "EIRP", l.EIRPDBm, 53.98, 0.01)
	if l.PolarizationLossDB != 0 || l.FrontEndLossDB != 0 || l.LNAGainDB != 0 || l.Params.K != geo.DefaultK ||
		l.Params.BandwidthMHz != 2 || l.Params.MinSNRDB != 10 || l.Params.Gas != StandardGas() || l.AntennaM != 20 {
		t.Errorf("defaults: %+v", l)
	}

	// 3 dB of cable in front of the receiver costs 3 dB of noise figure
	// (Friis: an attenuator at T0 has F = L).
	lossy := NewLink(model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30, CableLossDB: 2, FilterLossDB: 1}, 20, LinkParams{})
	near(t, "front-end loss", lossy.FrontEndLossDB, 3, 0)
	near(t, "NF with cable", lossy.SystemNFDB, 9, 1e-9)

	// Mast-head LNA (20 dB, NF 1 dB) behind 1 dB of loss:
	// Te = 290·0.2589 + 1.2589·(75.09 + 864.5/100) = 180.5 K → Tsys = 330.5 K.
	lna := NewLink(model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30,
		CableLossDB: 1, LNAGainDB: 20, LNANoiseFigDB: 1}, 20, LinkParams{})
	near(t, "Tsys with LNA", lna.SystemTempK, 330.5, 0.2)
	near(t, "NF with LNA", lna.SystemNFDB, NFFromNoiseTemp(180.5), 0.01)
	near(t, "LNA improvement", l.NoiseFloorDBm-lna.NoiseFloorDBm, DB(1014.5/330.5), 0.01)
	// LNA gain without a noise figure assumes 1 dB.
	lna2 := NewLink(model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30, CableLossDB: 1, LNAGainDB: 20}, 20, LinkParams{})
	near(t, "default LNA NF", lna2.LNANoiseFigDB, DefaultLNANoiseFigDB, 0)
	near(t, "default LNA NF Tsys", lna2.SystemTempK, lna.SystemTempK, 1e-9)

	// A stated receiver sensitivity replaces the noise figure:
	// NF = −90 − 10 − (−110.97) = 10.97 dB.
	s := NewLink(model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30}, 20, LinkParams{ReceiverSensitivityDBm: -90})
	near(t, "NF from sensitivity", s.Params.ReceiverNFDB, 10.965, 0.005)
	// With a 290 K antenna the system sensitivity is then exactly the stated one.
	s = NewLink(model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30}, 20,
		LinkParams{ReceiverSensitivityDBm: -90, AntennaTempK: 290})
	near(t, "sensitivity reproduced", s.SensitivityDBm, -90, 1e-6)

	// Horizontal polarisation is penalised.
	h := NewLink(model.Antenna{Type: model.AntYagi, GainDBi: 12, Polarization: " Horizontal "}, 20, LinkParams{})
	near(t, "cross-polar loss", h.PolarizationLossDB, CrossPolarLossDB, 0)

	near(t, "antenna AMSL", AntennaHeightAMSL(model.Receiver{GroundElevM: 120}, model.Antenna{HeightAGLM: 8}), 128, 0)
}

func TestLinkBudget(t *testing.T) {
	ant := model.Antenna{Type: model.AntVertical, GainDBi: 5, VBeamwidthDeg: 30, CableLossDB: 1.5, LNAGainDB: 18, LNANoiseFigDB: 0.8}
	l := NewLink(ant, 30, LinkParams{})

	const d, alt = 200.0, 11000.0
	b := l.Budget(77, d, alt)
	slant := geo.SlantRangeKm(d, 30, alt)
	elev := geo.ElevationDeg(d, 30, alt, 0)
	near(t, "slant", b.SlantKm, slant, 1e-9)
	near(t, "elevation", b.ElevationDeg, elev, 1e-9)
	if elev < 2 || elev > 3 {
		t.Errorf("elevation at 200 km / 11 km = %v, expected ~2.4°", elev)
	}
	near(t, "fspl", b.FSPLDB, 93.197+20*math.Log10(slant), 0.01)
	near(t, "gas", b.GasLossDB, GaseousLossDB(d, 30, alt, 0), 1e-12)
	near(t, "rx gain", b.RxAntennaGainDBi, l.Pattern.GainDBi(77, elev), 1e-12)
	// 53.98 − 139.2 − 0.5 + 4.8 ≈ −81 dBm at the antenna terminals.
	near(t, "received", b.ReceivedDBm, b.EIRPDBm-b.FSPLDB-b.GasLossDB+b.RxAntennaGainDBi, 1e-9)
	if b.ReceivedDBm < -83 || b.ReceivedDBm > -79 {
		t.Errorf("received level %v dBm, expected about −81", b.ReceivedDBm)
	}
	near(t, "rx input", b.RxInputDBm, b.ReceivedDBm-1.5+18, 1e-9)
	near(t, "snr", b.SNRDB, b.ReceivedDBm-l.NoiseFloorDBm, 1e-9)
	near(t, "margin", b.MarginDB, b.SNRDB-10, 1e-9)
	near(t, "margin vs sensitivity", b.MarginDB, b.ReceivedDBm-b.SensitivityDBm, 1e-9)
	near(t, "horizon", b.HorizonKm, LineOfSightKm(30, alt, 0), 1e-9)
	if !b.LineOfSight || !b.Decodable || b.MarginDB < 10 {
		t.Errorf("200 km airliner must be decodable with margin: %+v", b)
	}
	near(t, "ReceivedDBm shortcut", l.ReceivedDBm(77, d, alt), b.ReceivedDBm, 1e-9)
	if _, err := json.Marshal(b); err != nil {
		t.Errorf("json: %v", err)
	}

	// Beyond the radio horizon: figures are filled in but nothing decodes.
	far := l.Budget(77, b.HorizonKm+20, alt)
	if far.LineOfSight || far.Decodable {
		t.Errorf("beyond the horizon: %+v", far)
	}
	// Overhead: the collinear-style null hurts, the short path more than
	// makes up for it.
	over := l.Budget(0, 0, alt)
	near(t, "overhead elevation", over.ElevationDeg, 90, 1e-9)
	near(t, "overhead slant", over.SlantKm, (alt-30)/1000, 1e-9)
	if !over.Decodable {
		t.Errorf("overhead aircraft must be decodable: %+v", over)
	}
	near(t, "negative distance = overhead", l.Budget(0, -5, alt).ReceivedDBm, over.ReceivedDBm, 0)

	// Each dB of transmitter power, aircraft antenna gain or misc loss moves
	// the margin by exactly one dB.
	hi := NewLink(ant, 30, LinkParams{TxPowerW: TxPowerMaxW}).Budget(77, d, alt)
	near(t, "500 W vs 250 W", hi.MarginDB-b.MarginDB, 3.0103, 1e-6)
	lo := NewLink(ant, 30, LinkParams{TxPowerW: TxPowerLowW, TxAntennaGainDBi: -2, MiscLossDB: 3}).Budget(77, d, alt)
	near(t, "70 W, −2 dBi, 3 dB misc", lo.MarginDB-b.MarginDB, DB(70.0/250)-2-3, 1e-6)
	near(t, "EIRP", lo.EIRPDBm, WattsToDBm(70)-2, 1e-9)
}

func TestMaxRange(t *testing.T) {
	// A good station hears airliners to the radio horizon.
	ant := model.Antenna{Type: model.AntCollinear, GainDBi: 6, CableLossDB: 1, LNAGainDB: 20, LNANoiseFigDB: 1}
	l := NewLink(ant, 25, LinkParams{})
	alt := 38000 * geo.MetresPerFoot
	km, limit := l.MaxRangeKm(0, alt)
	near(t, "horizon-limited range", km, LineOfSightKm(25, alt, 0), 1e-6)
	if limit != LimitHorizon {
		t.Errorf("limit = %q", limit)
	}
	if !l.Budget(0, km-0.01, alt).Decodable {
		t.Error("budget must agree just inside the horizon")
	}

	// 20 dB of extra loss and a 70 W transponder: the budget runs out first,
	// and the budget at the reported range has zero margin.
	weak := NewLink(ant, 25, LinkParams{MiscLossDB: 20, TxPowerW: TxPowerLowW})
	km, limit = weak.MaxRangeKm(0, alt)
	if limit != LimitPower || km < 20 || km > 300 {
		t.Fatalf("power-limited range %v km (%s)", km, limit)
	}
	near(t, "zero margin at max range", weak.Budget(0, km, alt).MarginDB, 0, 0.05)
	if weak.Budget(0, km+5, alt).Decodable || !weak.Budget(0, km-5, alt).Decodable {
		t.Error("budget and max range disagree around the limit")
	}

	// Hopeless link.
	none := NewLink(ant, 25, LinkParams{MiscLossDB: 150})
	if km, limit = none.MaxRangeKm(0, alt); km != 0 || limit != LimitNone {
		t.Errorf("no reception: %v km (%s)", km, limit)
	}

	// A k-factor above standard pushes the horizon-limited range out.
	super := NewLink(ant, 25, LinkParams{K: 2})
	kmSuper, _ := super.MaxRangeKm(0, alt)
	kmStd, _ := l.MaxRangeKm(0, alt)
	if !(kmSuper > kmStd*1.15) {
		t.Errorf("k = 2 range %v vs standard %v", kmSuper, kmStd)
	}
	// Trapping geometry stays finite.
	duct := NewLink(ant, 25, LinkParams{K: KMax})
	km, limit = duct.MaxRangeKm(0, alt)
	if math.IsInf(km, 0) || math.IsNaN(km) || km <= kmStd || km > 2000 || limit != LimitPower {
		t.Errorf("trapping range %v km (%s)", km, limit)
	}
}

func TestRangeTableAndAzimuth(t *testing.T) {
	def := model.ExampleRadarcapeConfig()
	rx := model.Receiver{GroundElevM: 40}
	vert := NewLink(def.Antennas[0], AntennaHeightAMSL(rx, def.Antennas[0]), LinkParams{})
	yagi := NewLink(def.Antennas[1], AntennaHeightAMSL(rx, def.Antennas[1]), LinkParams{})
	near(t, "antenna height", vert.AntennaM, 48, 0)

	tab := vert.RangeTable(0)
	if len(tab) != geo.AltBands {
		t.Fatalf("len = %d", len(tab))
	}
	for i, r := range tab {
		if r.Band != i || r.Label != geo.AltBandLabel(i) || r.AltFt != geo.AltBandTypicalFt(i) {
			t.Errorf("row %d: %+v", i, r)
		}
		near(t, "row horizon", r.HorizonKm, LineOfSightFtKm(48, r.AltFt, 0), 1e-9)
		if r.RangeKm > r.HorizonKm+1e-9 || r.RangeKm <= 0 {
			t.Errorf("row %d range %v vs horizon %v", i, r.RangeKm, r.HorizonKm)
		}
		switch r.Limit {
		case LimitHorizon:
			near(t, "horizon-limited row", r.RangeKm, r.HorizonKm, 1e-6)
			if r.MarginDB < 0 {
				t.Errorf("row %d: negative margin at the horizon", i)
			}
		case LimitPower:
			near(t, "power-limited margin", r.MarginDB, 0, 0)
		default:
			t.Errorf("row %d: limit %q", i, r.Limit)
		}
		if i > 0 && r.RangeKm < tab[i-1].RangeKm {
			t.Errorf("row %d: range should not shrink with altitude", i)
		}
		km, limit := vert.MaxRangeKm(0, r.AltFt*geo.MetresPerFoot)
		if km != r.RangeKm || limit != r.Limit {
			t.Errorf("row %d disagrees with MaxRangeKm: %v/%s vs %v/%s", i, r.RangeKm, r.Limit, km, limit)
		}
	}
	if _, err := json.Marshal(tab); err != nil {
		t.Errorf("json: %v", err)
	}

	// Without an LNA and with a deaf receiver the yagi's pattern shows up in
	// the predicted range: far on boresight (north), short behind.
	deaf := LinkParams{ReceiverNFDB: 12, MiscLossDB: 12, TxPowerW: TxPowerMinW}
	yagi = NewLink(def.Antennas[1], 48, deaf)
	alt := 35000 * geo.MetresPerFoot
	cut := yagi.RangeByAzimuth(alt, 0)
	if len(cut.AngleDeg) != 180 || len(cut.RangeKm) != 180 || cut.AngleDeg[1] != 2 || cut.AltM != alt {
		t.Fatalf("range cut shape: %d", len(cut.AngleDeg))
	}
	near(t, "cut horizon", cut.HorizonKm, LineOfSightKm(48, alt, 0), 1e-9)
	front, back := cut.RangeKm[0], cut.RangeKm[90]
	if !(front > 1.5*back) || back <= 0 {
		t.Errorf("yagi range front %v km, back %v km", front, back)
	}
	for i, r := range cut.RangeKm {
		if r < back-1e-6 || r > front+1e-6 || r > cut.HorizonKm+1e-9 {
			t.Fatalf("azimuth %v: range %v outside [%v, %v]", cut.AngleDeg[i], r, back, front)
		}
		// Symmetric about boresight.
		if j := (180 - i) % 180; math.Abs(cut.RangeKm[j]-r) > 1e-6 {
			t.Fatalf("asymmetric: %v° %v km vs %v° %v km", cut.AngleDeg[i], r, cut.AngleDeg[j], cut.RangeKm[j])
		}
		km, _ := yagi.MaxRangeKm(cut.AngleDeg[i], alt)
		if km != r {
			t.Fatalf("cut and MaxRangeKm disagree at %v°", cut.AngleDeg[i])
		}
	}
	// The same deaf chain on the omni gives a circle.
	omni := NewLink(def.Antennas[0], 48, deaf).RangeByAzimuth(alt, 10)
	if len(omni.RangeKm) != 36 {
		t.Fatalf("10° cut length %d", len(omni.RangeKm))
	}
	for _, r := range omni.RangeKm {
		if r != omni.RangeKm[0] {
			t.Fatal("omni range must not depend on azimuth")
		}
	}
	if _, err := json.Marshal(cut); err != nil {
		t.Errorf("json: %v", err)
	}
}

func TestLinkParamsDefaults(t *testing.T) {
	p := LinkParams{}.WithDefaults()
	if p.TxPowerW != 250 || p.TxAntennaGainDBi != 0 || p.BandwidthMHz != 2 || p.ReceiverNFDB != 6 ||
		p.ReceiverSensitivityDBm != 0 || p.MinSNRDB != 10 || p.AntennaTempK != 150 || p.MiscLossDB != 0 ||
		p.K != geo.DefaultK || p.Gas != StandardGas() {
		t.Errorf("defaults: %+v", p)
	}
	// Garbage in, defaults out.
	p = LinkParams{TxPowerW: -1, TxAntennaGainDBi: math.NaN(), BandwidthMHz: math.NaN(), ReceiverNFDB: -3,
		ReceiverSensitivityDBm: 5, MinSNRDB: math.NaN(), AntennaTempK: -10, MiscLossDB: -4, K: math.NaN()}.WithDefaults()
	if p.TxPowerW != 250 || p.TxAntennaGainDBi != 0 || p.BandwidthMHz != 2 || p.ReceiverNFDB != 6 ||
		p.ReceiverSensitivityDBm != 0 || p.MinSNRDB != 10 || p.AntennaTempK != 150 || p.MiscLossDB != 0 || p.K != geo.DefaultK {
		t.Errorf("sanitised: %+v", p)
	}
	// Explicit values survive, and WithDefaults is idempotent.
	in := LinkParams{TxPowerW: 125, TxAntennaGainDBi: -1, BandwidthMHz: 2.6, ReceiverNFDB: 4, MinSNRDB: 8,
		AntennaTempK: 200, MiscLossDB: 2, K: 1.5, Gas: NewGasModel(1000, 25, 70, 100)}
	p = in.WithDefaults()
	if p != in || p.WithDefaults() != p {
		t.Errorf("explicit values changed: %+v", p)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Errorf("json: %v", err)
	}
}
