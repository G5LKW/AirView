package radio

import (
	"math"
	"strconv"
	"strings"

	"airview/internal/geo"
	"airview/internal/model"
)

// ---------------------------------------------------------------------------
// Path loss

// FSPLDB is the free-space path loss in dB between isotropic antennas
// (Friis; ITU-R P.525): L = (4π·d/λ)² = 32.45 + 20·log10(d_km) + 20·log10(f_MHz).
// Distances below 1 m are treated as 1 m.
func FSPLDB(distKm, freqMHz float64) float64 {
	if !(distKm > 1e-3) {
		distKm = 1e-3
	}
	return 20 * math.Log10(4*math.Pi*distKm*1000*freqMHz*1e6/SpeedOfLightMPerS)
}

// FSPL1090DB is the free-space path loss at 1090 MHz:
// 93.20 + 20·log10(d_km) dB (145.2 dB at 400 km).
func FSPL1090DB(distKm float64) float64 { return FSPLDB(distKm, FreqMHz) }

// SpecificAttenuation estimates the specific attenuation of the atmospheric
// gases in dB/km at one point: the oxygen (dry air) and water vapour parts,
// from pressure (hPa), temperature (°C) and water vapour density (g/m³).
//
// It implements the simplified formulae of Recommendation ITU-R P.676-10,
// Annex 2: equation (22a) for dry air, valid for f <= 54 GHz, and equation
// (23a) for water vapour, with rp = p/1013 and rt = 288/(273 + t):
//
//	γo = [ 7.2·rt^2.8 / (f² + 0.34·rp²·rt^1.6)
//	       + 0.62·ξ3 / ((54 − f)^(1.16·ξ1) + 0.83·ξ2) ] · f²·rp²·1e-3
//
// (f in GHz). At 1090 MHz and sea level this gives γo ≈ 0.0056 dB/km and a
// water vapour contribution about a hundred times smaller.
func SpecificAttenuation(freqMHz, pHPa, tC, vapourDensityGm3 float64) (oxygenDBPerKm, waterDBPerKm float64) {
	f := freqMHz / 1000
	if !(f > 0) || f > 54 {
		return math.NaN(), math.NaN()
	}
	rp := pHPa / 1013
	rt := 288 / (273 + tC)
	if !(rp > 0) || !(rt > 0) {
		return 0, 0
	}
	phi := func(a, b, c, d float64) float64 {
		return math.Pow(rp, a) * math.Pow(rt, b) * math.Exp(c*(1-rp)+d*(1-rt))
	}
	xi1 := phi(0.0717, -1.8132, 0.0156, -1.6515)
	xi2 := phi(0.5146, -4.6368, -0.1921, -5.7416)
	xi3 := phi(0.3414, -6.5851, 0.2130, -8.5854)
	f2 := f * f
	oxygenDBPerKm = (7.2*math.Pow(rt, 2.8)/(f2+0.34*rp*rp*math.Pow(rt, 1.6)) +
		0.62*xi3/(math.Pow(54-f, 1.16*xi1)+0.83*xi2)) * f2 * rp * rp * 1e-3

	rho := math.Max(0, vapourDensityGm3)
	eta1 := 0.955*rp*math.Pow(rt, 0.68) + 0.006*rho
	eta2 := 0.735*rp*math.Sqrt(rt) + 0.0353*math.Pow(rt, 4)*rho
	g := func(fi float64) float64 {
		r := (f - fi) / (f + fi)
		return 1 + r*r
	}
	sq := func(x float64) float64 { return x * x }
	e := func(c float64) float64 { return math.Exp(c * (1 - rt)) }
	sum := 3.98*eta1*e(2.23)/(sq(f-22.235)+9.42*eta1*eta1)*g(22) +
		11.96*eta1*e(0.7)/(sq(f-183.31)+11.14*eta1*eta1) +
		0.081*eta1*e(6.44)/(sq(f-321.226)+6.29*eta1*eta1) +
		3.66*eta1*e(1.6)/(sq(f-325.153)+9.22*eta1*eta1) +
		25.37*eta1*e(1.09)/sq(f-380) +
		17.4*eta1*e(1.46)/sq(f-448) +
		844.6*eta1*e(0.17)/sq(f-557)*g(557) +
		290*eta1*e(0.41)/sq(f-752)*g(752) +
		8.3328e4*eta2*e(0.99)/sq(f-1780)*g(1780)
	waterDBPerKm = sum * f2 * math.Pow(rt, 2.5) * rho * 1e-4
	return oxygenDBPerKm, waterDBPerKm
}

// Equivalent heights at 1090 MHz and sea-level pressure from ITU-R P.676-10
// Annex 2 equations (25) and (26): ho = 6.1/(1 + 0.17·rp^−1.1)·(1 + t1 + t2 +
// t3) = 5.21 km (the t terms vanish far below the 60 GHz complex) and
// hw = 1.66·(1 + …) = 1.66 km. They are used as the scale heights of an
// exponential decrease of the specific attenuation with altitude.
const (
	oxygenScaleKm = 5.21
	waterScaleKm  = 1.66
)

// GasModel describes the gaseous absorption of the atmosphere at 1090 MHz
// as specific attenuations at a reference height that decay exponentially
// with altitude: γ(h) = γref·exp(−(h − href)/H).
type GasModel struct {
	OxygenDBPerKm float64 `json:"oxygenDbPerKm"` // at RefHeightM
	WaterDBPerKm  float64 `json:"waterDbPerKm"`  // at RefHeightM
	OxygenScaleKm float64 `json:"oxygenScaleKm"` // equivalent height of dry air
	WaterScaleKm  float64 `json:"waterScaleKm"`  // equivalent height of water vapour
	RefHeightM    float64 `json:"refHeightM"`    // height the specific attenuations apply to
}

// NewGasModel builds a GasModel from surface weather: pressure (hPa),
// temperature (°C) and relative humidity (%) measured at heightM metres AMSL.
func NewGasModel(pHPa, tC, rhPct, heightM float64) GasModel {
	rho := WaterVapourDensity(VapourPressureFromRH(tC, rhPct, pHPa), tC)
	o, w := SpecificAttenuation(FreqMHz, pHPa, tC, rho)
	return GasModel{OxygenDBPerKm: o, WaterDBPerKm: w, OxygenScaleKm: oxygenScaleKm, WaterScaleKm: waterScaleKm, RefHeightM: heightM}
}

// StandardGas is the gas model of the ITU-R mean annual global reference
// atmosphere at sea level: 1013.25 hPa, 15 °C, 7.5 g/m³ of water vapour.
func StandardGas() GasModel {
	o, w := SpecificAttenuation(FreqMHz, 1013.25, 15, 7.5)
	return GasModel{OxygenDBPerKm: o, WaterDBPerKm: w, OxygenScaleKm: oxygenScaleKm, WaterScaleKm: waterScaleKm}
}

var standardGas = StandardGas()

// PathLossDB integrates the gaseous absorption in dB along the straight
// (effective-earth) ray from an antenna at antennaM metres AMSL to an
// aircraft at aircraftM metres AMSL at ground distance groundKm, for
// effective-earth factor k (0 = 4/3). Unlike the cosecant law this stays
// correct at the grazing angles of distant aircraft, where the ray spends
// hundreds of kilometres in the dense lower atmosphere: about 0.25 dB at
// 100 km and 1.25 dB at 400 km for an airliner at cruise level (the zenith
// value is only 0.03 dB).
func (g GasModel) PathLossDB(groundKm, antennaM, aircraftM, k float64) float64 {
	if g.OxygenDBPerKm <= 0 && g.WaterDBPerKm <= 0 {
		return 0
	}
	ho, hw := g.OxygenScaleKm, g.WaterScaleKm
	if !(ho > 0) {
		ho = oxygenScaleKm
	}
	if !(hw > 0) {
		hw = waterScaleKm
	}
	if !(groundKm > 0) {
		groundKm = 0
	}
	re := geo.EffectiveK(k) * geo.EarthRadiusKm
	h1, h2 := antennaM/1000, aircraftM/1000
	r1, r2 := re+h1, re+h2
	sh := math.Sin(groundKm / (2 * re))
	chord2 := 4 * r1 * r2 * sh * sh
	slant := math.Sqrt((h2-h1)*(h2-h1) + chord2)
	if slant == 0 {
		return 0
	}
	ref := g.RefHeightM / 1000
	gamma := func(u float64) float64 {
		// Height above the smooth earth of the point at fraction u of the
		// chord: |P(u)| − Re with |P|² = A² − u(1−u)·chord², A = Re + (1−u)h1 + u·h2,
		// arranged to avoid subtracting two ~8500 km numbers.
		a := (1-u)*h1 + u*h2 // A − Re
		b := u * (1 - u) * chord2
		p := math.Sqrt((re+a)*(re+a) - b)
		h := (a*(2*re+a) - b) / (p + re)
		if h < 0 {
			h = 0
		}
		h -= ref
		return g.OxygenDBPerKm*math.Exp(-h/ho) + g.WaterDBPerKm*math.Exp(-h/hw)
	}
	// Composite Simpson rule; the integrand is smooth and nearly exponential.
	const n = 16
	sum := gamma(0) + gamma(1)
	for i := 1; i < n; i++ {
		w := 2.0
		if i&1 == 1 {
			w = 4
		}
		sum += w * gamma(float64(i)/n)
	}
	return slant * sum / (3 * n)
}

// GaseousLossDB is the gaseous absorption in dB at 1090 MHz along the path
// antenna → aircraft for the standard atmosphere (see GasModel.PathLossDB).
func GaseousLossDB(groundKm, antennaM, aircraftM, k float64) float64 {
	return standardGas.PathLossDB(groundKm, antennaM, aircraftM, k)
}

// ---------------------------------------------------------------------------
// Transponders

// Transponder transmitter power classes: peak pulse power at the antenna end
// of the transponder's feed line. ICAO Annex 10 Vol. IV (3.1.1.7.11 /
// 3.1.2.10.2) and RTCA DO-181/DO-260 require at least 18.5 dBW (70 W) for
// aircraft that do not operate above 15 000 ft, at least 21 dBW (125 W) for
// aircraft that do (or cruise faster than 175 kt), and at most 27 dBW (500 W).
const (
	TxPowerLowW     = 70.0  // 18.5 dBW: minimum, aircraft confined below 15 000 ft (light GA, gliders)
	TxPowerMinW     = 125.0 // 21 dBW: minimum for aircraft operating above 15 000 ft
	TxPowerTypicalW = 250.0 // 24 dBW: typical air-transport transponder
	TxPowerMaxW     = 500.0 // 27 dBW: maximum permitted
)

// TransponderClass is one transmitter power class.
type TransponderClass struct {
	Name     string  `json:"name"`
	PowerW   float64 `json:"powerW"`
	PowerDBm float64 `json:"powerDbm"`
	Note     string  `json:"note"`
}

// TransponderClasses returns the four standard power classes, weakest first.
func TransponderClasses() []TransponderClass {
	mk := func(w float64, note string) TransponderClass {
		return TransponderClass{Name: formatWatts(w), PowerW: w, PowerDBm: WattsToDBm(w), Note: note}
	}
	return []TransponderClass{
		mk(TxPowerLowW, "minimum for aircraft that stay below 15 000 ft (18.5 dBW): light aircraft, gliders"),
		mk(TxPowerMinW, "minimum for aircraft operating above 15 000 ft or faster than 175 kt (21 dBW)"),
		mk(TxPowerTypicalW, "typical air-transport transponder (24 dBW)"),
		mk(TxPowerMaxW, "maximum permitted peak power (27 dBW)"),
	}
}

func formatWatts(w float64) string {
	return strconv.Itoa(int(math.Round(w))) + " W"
}

// ---------------------------------------------------------------------------
// Link budget

// CrossPolarLossDB is the polarisation mismatch applied when a horizontally
// polarised antenna receives the vertically polarised 1090 MHz signal. The
// theoretical loss is infinite; scattering and antenna cross-polar response
// limit it to about 20 dB in practice.
const CrossPolarLossDB = 20.0

// Link-budget defaults (see LinkParams).
const (
	DefaultBandwidthMHz   = 2.0   // Mode S replies are 0.5 µs pulses at 1 Mbit/s
	DefaultReceiverNFDB   = 6.0   // typical Mode S receiver / SDR front end without an LNA
	DefaultMinSNRDB       = 10.0  // SNR needed for reliable Mode S pulse-position decoding
	DefaultAntennaTempK   = 150.0 // L-band antenna looking at the horizon: half cold sky, half warm ground
	DefaultLNANoiseFigDB  = 1.0   // used when an LNA gain is configured without a noise figure
	maxLinkKm             = 2000.0
	linkProfileMaxSamples = 800
)

// Range limits reported by Link.MaxRangeKm.
const (
	LimitHorizon = "horizon" // enough signal all the way to the radio horizon
	LimitPower   = "power"   // the link budget runs out before the horizon
	LimitNone    = "none"    // no reception at any distance
)

// LinkParams are the link-budget inputs that are not properties of the
// receive antenna. Zero values select the documented defaults.
type LinkParams struct {
	// TxPowerW is the transponder peak pulse power at the aircraft antenna
	// terminals in watts (see the TxPower* constants). Default 250.
	TxPowerW float64 `json:"txPowerW"`
	// TxAntennaGainDBi is the gain of the aircraft's blade antenna towards
	// the station. Default 0 dBi, the usual planning value for directions
	// near the aircraft's horizon.
	TxAntennaGainDBi float64 `json:"txAntennaGainDbi"`
	// BandwidthMHz is the receiver noise bandwidth. Default 2.
	BandwidthMHz float64 `json:"bandwidthMHz"`
	// ReceiverNFDB is the noise figure of the receiver itself (without any
	// external LNA). Default 6.
	ReceiverNFDB float64 `json:"receiverNfDb"`
	// ReceiverSensitivityDBm, when negative, is the receiver's own minimum
	// decodable signal level at its input (e.g. −90). It replaces
	// ReceiverNFDB: NF = sensitivity − MinSNR − 10·log10(k·T0·B).
	ReceiverSensitivityDBm float64 `json:"receiverSensitivityDbm"`
	// MinSNRDB is the signal-to-noise ratio required for decoding. Default 10.
	MinSNRDB float64 `json:"minSnrDb"`
	// AntennaTempK is the external noise temperature seen by the antenna.
	// Default 150.
	AntennaTempK float64 `json:"antennaTempK"`
	// MiscLossDB is any further loss to budget for (fading allowance,
	// obstruction, implementation loss). Default 0.
	MiscLossDB float64 `json:"miscLossDb"`
	// K is the effective-earth-radius factor. Default 4/3.
	K float64 `json:"k"`
	// Gas is the gaseous absorption model; the zero value selects StandardGas().
	Gas GasModel `json:"gas"`
}

// WithDefaults returns the parameters with every unset (zero or invalid)
// field replaced by its default.
func (p LinkParams) WithDefaults() LinkParams {
	if !(p.TxPowerW > 0) {
		p.TxPowerW = TxPowerTypicalW
	}
	if p.TxAntennaGainDBi != p.TxAntennaGainDBi {
		p.TxAntennaGainDBi = 0
	}
	if !(p.BandwidthMHz > 0) {
		p.BandwidthMHz = DefaultBandwidthMHz
	}
	if !(p.MinSNRDB > 0) {
		p.MinSNRDB = DefaultMinSNRDB
	}
	if p.ReceiverSensitivityDBm < 0 {
		nf := p.ReceiverSensitivityDBm - p.MinSNRDB - NoisePowerDBm(T0K, p.BandwidthMHz*1e6)
		p.ReceiverNFDB = math.Max(0.1, nf)
	} else {
		p.ReceiverSensitivityDBm = 0
	}
	if !(p.ReceiverNFDB > 0) {
		p.ReceiverNFDB = DefaultReceiverNFDB
	}
	if !(p.AntennaTempK > 0) {
		p.AntennaTempK = DefaultAntennaTempK
	}
	if !(p.MiscLossDB > 0) {
		p.MiscLossDB = 0
	}
	p.K = geo.EffectiveK(p.K)
	if p.Gas == (GasModel{}) {
		p.Gas = standardGas
	}
	return p
}

// AntennaHeightAMSL returns the feed-point height of an antenna above mean
// sea level: site ground elevation + height above ground.
func AntennaHeightAMSL(rx model.Receiver, a model.Antenna) float64 {
	return rx.GroundElevM + a.HeightAGLM
}

// Link is a prepared 1090 MHz downlink budget aircraft → one receive antenna
// and its RF chain. It is immutable after NewLink and safe for concurrent use.
//
// Signal chain and reference planes:
//
//	aircraft: TxPowerW → aircraft antenna → EIRP
//	path:     free-space loss + gaseous absorption + polarisation + misc loss
//	station:  antenna pattern gain ──► [antenna terminals] ──► cable + filter
//	          loss ──► LNA (if any) ──► receiver
//
// Noise is referred to the antenna terminals (Friis cascade, losses at
// T0 = 290 K):
//
//	Tsys = Ta + T0·(L − 1) + L·Tlna + L·Trx/Glna     with an LNA
//	Tsys = Ta + T0·(L − 1) + L·Trx                    without
//
// where L is the cable + filter loss ahead of the first amplifier and
// T = T0·(F − 1) for each noise figure. The noise floor is k·Tsys·B and the
// station decodes when the signal at the antenna terminals exceeds it by
// MinSNRDB and the aircraft is above the radio horizon.
type Link struct {
	Info     PatternInfo // resolved antenna pattern parameters
	Pattern  Pattern     // receive antenna pattern
	Params   LinkParams  // inputs with defaults applied
	AntennaM float64     // antenna height above the smooth earth (AMSL), metres

	TxPowerDBm         float64 // transponder power
	EIRPDBm            float64 // TxPowerDBm + aircraft antenna gain
	PolarizationLossDB float64 // CrossPolarLossDB for a horizontally polarised antenna, else 0
	FrontEndLossDB     float64 // cable + filter loss ahead of the LNA / receiver
	LNAGainDB          float64 // 0 = no LNA
	LNANoiseFigDB      float64 //
	SystemTempK        float64 // Tsys at the antenna terminals
	SystemNFDB         float64 // noise figure of everything behind the antenna terminals
	NoiseFloorDBm      float64 // k·Tsys·B
	SensitivityDBm     float64 // minimum decodable signal at the antenna terminals: noise floor + MinSNRDB
}

// NewLink prepares the link budget for a configured antenna. antennaM is the
// antenna height in metres above the smooth earth used for the horizon —
// normally AntennaHeightAMSL(receiver, antenna).
func NewLink(a model.Antenna, antennaM float64, p LinkParams) *Link {
	p = p.WithDefaults()
	info := Resolve(a)
	l := &Link{Info: info, Pattern: info.Pattern(), Params: p, AntennaM: antennaM}
	l.TxPowerDBm = WattsToDBm(p.TxPowerW)
	l.EIRPDBm = l.TxPowerDBm + p.TxAntennaGainDBi
	if strings.EqualFold(strings.TrimSpace(a.Polarization), "horizontal") {
		l.PolarizationLossDB = CrossPolarLossDB
	}
	l.FrontEndLossDB = math.Max(0, a.CableLossDB) + math.Max(0, a.FilterLossDB)
	loss := FromDB(l.FrontEndLossDB)
	tRx := NoiseTempFromNF(p.ReceiverNFDB)
	te := T0K * (loss - 1) // receiver-chain noise referred to the antenna terminals
	if a.LNAGainDB > 0 {
		l.LNAGainDB = a.LNAGainDB
		l.LNANoiseFigDB = a.LNANoiseFigDB
		if !(l.LNANoiseFigDB > 0) {
			l.LNANoiseFigDB = DefaultLNANoiseFigDB
		}
		te += loss * (NoiseTempFromNF(l.LNANoiseFigDB) + tRx/FromDB(l.LNAGainDB))
	} else {
		te += loss * tRx
	}
	l.SystemTempK = p.AntennaTempK + te
	l.SystemNFDB = NFFromNoiseTemp(te)
	l.NoiseFloorDBm = NoisePowerDBm(l.SystemTempK, p.BandwidthMHz*1e6)
	l.SensitivityDBm = l.NoiseFloorDBm + p.MinSNRDB
	return l
}

// Budget is the complete link budget for one aircraft position.
type Budget struct {
	AzimuthDeg   float64 `json:"azimuthDeg"`   // bearing from the station
	GroundKm     float64 `json:"groundKm"`     // ground distance
	AltM         float64 `json:"altM"`         // aircraft altitude AMSL
	SlantKm      float64 `json:"slantKm"`      // straight-line path length
	ElevationDeg float64 `json:"elevationDeg"` // elevation of the aircraft at the antenna
	HorizonKm    float64 `json:"horizonKm"`    // line-of-sight limit for this altitude
	LineOfSight  bool    `json:"lineOfSight"`  // GroundKm <= HorizonKm

	TxPowerDBm         float64 `json:"txPowerDbm"`
	EIRPDBm            float64 `json:"eirpDbm"`
	FSPLDB             float64 `json:"fsplDb"`
	GasLossDB          float64 `json:"gasLossDb"`
	PolarizationLossDB float64 `json:"polarizationLossDb"`
	MiscLossDB         float64 `json:"miscLossDb"`
	RxAntennaGainDBi   float64 `json:"rxAntennaGainDbi"` // pattern gain towards the aircraft
	ReceivedDBm        float64 `json:"receivedDbm"`      // signal at the antenna terminals
	FrontEndLossDB     float64 `json:"frontEndLossDb"`   // cable + filter
	LNAGainDB          float64 `json:"lnaGainDb"`
	RxInputDBm         float64 `json:"rxInputDbm"` // signal at the receiver input

	SystemTempK    float64 `json:"systemTempK"`
	SystemNFDB     float64 `json:"systemNfDb"`
	NoiseFloorDBm  float64 `json:"noiseFloorDbm"`  // at the antenna terminals
	SensitivityDBm float64 `json:"sensitivityDbm"` // noise floor + required SNR
	SNRDB          float64 `json:"snrDb"`
	RequiredSNRDB  float64 `json:"requiredSnrDb"`
	MarginDB       float64 `json:"marginDb"`  // SNR − required SNR
	Decodable      bool    `json:"decodable"` // LineOfSight && MarginDB >= 0
}

// pathDB returns the path geometry and the total propagation loss in dB
// (free space + gases) for a ground distance and aircraft altitude.
func (l *Link) pathDB(groundKm, altM float64) (slantKm, elevDeg, fspl, gas float64) {
	slantKm = geo.SlantRangeKm(groundKm, l.AntennaM, altM)
	elevDeg = geo.ElevationDeg(groundKm, l.AntennaM, altM, l.Params.K)
	fspl = FSPL1090DB(slantKm)
	gas = l.Params.Gas.PathLossDB(groundKm, l.AntennaM, altM, l.Params.K)
	return
}

// Budget evaluates the link for an aircraft at bearing azimuthDeg, ground
// distance groundKm and altitude aircraftAltM metres AMSL. Beyond the radio
// horizon the free-space figures are still filled in, but LineOfSight and
// Decodable are false (diffraction loss rises so steeply behind the horizon
// that it is not modelled).
func (l *Link) Budget(azimuthDeg, groundKm, aircraftAltM float64) Budget {
	if !(groundKm > 0) {
		groundKm = 0
	}
	b := Budget{
		AzimuthDeg: azimuthDeg, GroundKm: groundKm, AltM: aircraftAltM,
		TxPowerDBm: l.TxPowerDBm, EIRPDBm: l.EIRPDBm,
		PolarizationLossDB: l.PolarizationLossDB, MiscLossDB: l.Params.MiscLossDB,
		FrontEndLossDB: l.FrontEndLossDB, LNAGainDB: l.LNAGainDB,
		SystemTempK: l.SystemTempK, SystemNFDB: l.SystemNFDB,
		NoiseFloorDBm: l.NoiseFloorDBm, SensitivityDBm: l.SensitivityDBm,
		RequiredSNRDB: l.Params.MinSNRDB,
	}
	b.SlantKm, b.ElevationDeg, b.FSPLDB, b.GasLossDB = l.pathDB(groundKm, aircraftAltM)
	b.HorizonKm = LineOfSightKm(l.AntennaM, aircraftAltM, l.Params.K)
	b.LineOfSight = groundKm <= b.HorizonKm
	b.RxAntennaGainDBi = l.Pattern.GainDBi(azimuthDeg, b.ElevationDeg)
	b.ReceivedDBm = l.EIRPDBm - b.FSPLDB - b.GasLossDB - b.PolarizationLossDB - b.MiscLossDB + b.RxAntennaGainDBi
	b.RxInputDBm = b.ReceivedDBm - b.FrontEndLossDB + b.LNAGainDB
	b.SNRDB = b.ReceivedDBm - b.NoiseFloorDBm
	b.MarginDB = b.SNRDB - b.RequiredSNRDB
	b.Decodable = b.LineOfSight && b.MarginDB >= 0
	return b
}

// ReceivedDBm is the predicted signal level at the antenna terminals for an
// aircraft position (a cheaper Budget when only the level is needed).
func (l *Link) ReceivedDBm(azimuthDeg, groundKm, aircraftAltM float64) float64 {
	if !(groundKm > 0) {
		groundKm = 0
	}
	_, el, fspl, gas := l.pathDB(groundKm, aircraftAltM)
	return l.EIRPDBm - fspl - gas - l.PolarizationLossDB - l.Params.MiscLossDB + l.Pattern.GainDBi(azimuthDeg, el)
}

// rangeProfile holds everything about the path at one altitude that does not
// depend on the azimuth, sampled from the station out to the radio horizon.
type rangeProfile struct {
	horizonKm float64   // true line-of-sight limit
	capped    bool      // the horizon lies beyond maxLinkKm (trapping geometry)
	distKm    []float64 // sample distances, ascending, last = min(horizon, maxLinkKm)
	elevDeg   []float64 // elevation of the aircraft at each distance
	baseDB    []float64 // link margin without the receive antenna gain
}

func (l *Link) profile(altM float64) rangeProfile {
	p := rangeProfile{horizonKm: LineOfSightKm(l.AntennaM, altM, l.Params.K)}
	dMax := p.horizonKm
	if dMax > maxLinkKm {
		dMax, p.capped = maxLinkKm, true
	}
	if !(dMax > 0) {
		dMax = 0
	}
	n := int(math.Ceil(dMax)) // ~1 km steps
	if n < 16 {
		n = 16
	} else if n > linkProfileMaxSamples {
		n = linkProfileMaxSamples
	}
	p.distKm = make([]float64, n+1)
	p.elevDeg = make([]float64, n+1)
	p.baseDB = make([]float64, n+1)
	fixed := l.EIRPDBm - l.PolarizationLossDB - l.Params.MiscLossDB - l.SensitivityDBm
	for i := 0; i <= n; i++ {
		d := dMax * float64(i) / float64(n)
		if i == n {
			d = dMax // exactly, whatever the rounding of the division
		}
		_, el, fspl, gas := l.pathDB(d, altM)
		p.distKm[i], p.elevDeg[i], p.baseDB[i] = d, el, fixed-fspl-gas
	}
	return p
}

// maxRange finds the largest distance with a non-negative margin.
func (p *rangeProfile) maxRange(pat Pattern, azimuthDeg float64) (km float64, limit string, marginDB float64) {
	n := len(p.distKm) - 1
	margin := func(i int) float64 { return p.baseDB[i] + pat.GainDBi(azimuthDeg, p.elevDeg[i]) }
	mOuter := margin(n)
	if mOuter >= 0 {
		if p.capped {
			return p.distKm[n], LimitPower, mOuter // capped far beyond any real range (trapping geometry)
		}
		return p.distKm[n], LimitHorizon, mOuter
	}
	for i := n - 1; i >= 0; i-- {
		m := margin(i)
		if m >= 0 {
			// Margin crosses zero between samples i and i+1.
			f := m / (m - mOuter)
			return p.distKm[i] + f*(p.distKm[i+1]-p.distKm[i]), LimitPower, 0
		}
		mOuter = m
	}
	return 0, LimitNone, mOuter
}

// MaxRangeKm predicts the greatest ground distance at which an aircraft at
// aircraftAltM metres AMSL on bearing azimuthDeg can be decoded, and what
// limits it: LimitHorizon when the signal is still sufficient at the radio
// horizon, LimitPower when the link budget runs out first, LimitNone when
// there is no reception at any distance (km = 0).
func (l *Link) MaxRangeKm(azimuthDeg, aircraftAltM float64) (km float64, limit string) {
	p := l.profile(aircraftAltM)
	km, limit, _ = p.maxRange(l.Pattern, azimuthDeg)
	return km, limit
}

// BandPrediction is the predicted maximum range for one altitude band.
type BandPrediction struct {
	Band      int     `json:"band"`      // 0..6 (geo.AltBand)
	Label     string  `json:"label"`     //
	AltFt     float64 `json:"altFt"`     // representative altitude of the band
	HorizonKm float64 `json:"horizonKm"` // smooth-earth line-of-sight limit
	RangeKm   float64 `json:"rangeKm"`   // predicted maximum decoding range
	Limit     string  `json:"limit"`     // Limit* constant
	// MarginDB is the link margin left at RangeKm: the spare signal at the
	// horizon when horizon-limited, 0 when power-limited.
	MarginDB float64 `json:"marginDb"`
}

// RangeTable predicts the maximum range on bearing azimuthDeg for each of
// the seven standard altitude bands (at the band's representative altitude).
func (l *Link) RangeTable(azimuthDeg float64) []BandPrediction {
	out := make([]BandPrediction, geo.AltBands)
	for b := range out {
		altFt := geo.AltBandTypicalFt(b)
		p := l.profile(altFt * geo.MetresPerFoot)
		km, limit, margin := p.maxRange(l.Pattern, azimuthDeg)
		out[b] = BandPrediction{
			Band: b, Label: geo.AltBandLabel(b), AltFt: altFt,
			HorizonKm: p.horizonKm, RangeKm: km, Limit: limit, MarginDB: margin,
		}
	}
	return out
}

// RangeCut is the predicted maximum range around the compass at one altitude.
type RangeCut struct {
	AltM      float64   `json:"altM"`
	HorizonKm float64   `json:"horizonKm"` // smooth-earth line-of-sight limit
	AngleDeg  []float64 `json:"angleDeg"`  // azimuths 0, step, … < 360
	RangeKm   []float64 `json:"rangeKm"`
}

// RangeByAzimuth predicts the maximum range for an aircraft at aircraftAltM
// metres AMSL on every azimuth 0, step, 2·step, … < 360 (stepDeg <= 0
// selects 2°, the coverage bearing-bin width): the model counterpart of the
// measured polar coverage plot.
func (l *Link) RangeByAzimuth(aircraftAltM, stepDeg float64) RangeCut {
	if !(stepDeg > 0) {
		stepDeg = geo.BearingBinDeg
	}
	n := cutCount(360, stepDeg)
	p := l.profile(aircraftAltM)
	c := RangeCut{AltM: aircraftAltM, HorizonKm: p.horizonKm, AngleDeg: make([]float64, n), RangeKm: make([]float64, n)}
	_, omni := l.Pattern.(Omni)
	for i := 0; i < n; i++ {
		az := 360 * float64(i) / float64(n)
		c.AngleDeg[i] = az
		if omni && i > 0 {
			c.RangeKm[i] = c.RangeKm[0]
			continue
		}
		c.RangeKm[i], _, _ = p.maxRange(l.Pattern, az)
	}
	return c
}
