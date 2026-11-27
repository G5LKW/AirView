package rfstat

// The hourly accumulator is one flat []float64 with named offsets. That
// makes the per-frame path a handful of indexed increments, and merging,
// scaling (sliding-hour blend) and resetting simple loops over one array.

// Limits and bin geometry.
const (
	MaxAntennas = 8 // Frame.Antenna 0..7
	MaxFeeds    = 16

	// RSSI histograms: 0.5 dB bins from -100 dBFS to 0 dBFS.
	rssiLo   = -100.0
	rssiStep = 0.5
	nRSSI    = 200

	// Distance rings for RSSI: 25 km, last ring open.
	ringKm = 25.0
	nRing  = 16

	// Multipath: residual vs elevation, 0.05 deg bins from -1 to 14 deg.
	mpLo   = -1.0
	mpStep = 0.05
	nMP    = 300

	nAspect   = 18 // 10 deg aspect bins 0..180
	nKBins    = 40 // Rician K in dB, 1 dB bins from -10 dB
	kLo       = -10.0
	nStdBins  = 40 // short-window RSSI std dev, 0.25 dB bins
	stdStep   = 0.25
	nPdTypes  = 3 // position, velocity, identification
	pdDistKm  = 10.0
	nPdDist   = 50
	nPdBrg    = 36
	pdRSSILo  = -50.0
	pdRSSIW   = 2.0
	nPdRSSI   = 25
	nGapBands = 6
	gapStep   = 0.1 // s
	nGapBins  = 600 // 0..60 s
	nGapT     = 5   // P(age <= T) thresholds
	nDF       = 34  // 0..31, 32 = Mode A/C, 33 = other
	nLoad     = 30  // load buckets of 100 frames/s
	loadStep  = 100.0
	nBDS      = 11
	nIID      = 80
	nTimeBins = 600
	nTimeKind = 3
	nGD       = 160 // GNSS-baro delta, 25 ft bins from -2000 ft
	gdLo      = -2000.0
	gdStep    = 25.0
	nAltBands = 7
	nCellSec  = 36
	nCellRing = 10 // 50 km
	cellKm    = 50.0
)

// Elevation bands (deg) for RSSI and fading statistics.
var elevEdges = [...]float64{0, 0.5, 1, 2, 3, 5, 10, 20, 45}

const nElev = len(elevEdges) + 1 // band 0 = below 0 deg

// Elevation bins (deg) for probability of detection.
var pdElevEdges = [...]float64{-1, -0.5, 0, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4, 5, 7, 10, 15, 20, 30, 45}

const nPdElev = len(pdElevEdges) + 1

// Position-update gap bands (km) and the thresholds T (s) for P(age <= T).
var (
	gapBandEdges = [...]float64{50, 100, 150, 200, 300}
	gapT         = [nGapT]float64{2, 4, 5, 8, 12}
)

// Comm-B registers tracked individually; index nBDS-2 = other, nBDS-1 = unknown.
var bdsList = [...]uint8{0x10, 0x17, 0x20, 0x30, 0x40, 0x44, 0x45, 0x50, 0x60}

// Timing histogram bin width per squitter type (s): 5 ms for position and
// velocity (0..3 s), 20 ms for identification (0..12 s).
var timeBinW = [nTimeKind]float64{0.005, 0.005, 0.02}

// Offsets into the flat accumulator.
const (
	oSecs       = 0
	oFrames     = 1
	oAirUs      = 2
	oRA         = 3
	oBursts     = 4
	oDF         = 5
	oCRC        = oDF + nDF
	oKind       = oCRC + int(numCRC)
	oAntFrames  = oKind + int(numKind)
	oRSSIAll    = oAntFrames + MaxAntennas
	oRSSIAnt    = oRSSIAll + nRSSI
	oRSSIRing   = oRSSIAnt + MaxAntennas*nRSSI
	oRSSIElev   = oRSSIRing + nRing*nRSSI
	oMPSum      = oRSSIElev + nElev*nRSSI
	oMPN        = oMPSum + nMP
	oAspSum     = oMPN + nMP
	oAspSq      = oAspSum + nAspect
	oAspN       = oAspSq + nAspect
	oKHist      = oAspN + nAspect
	oStdHist    = oKHist + nKBins
	oKElevSum   = oStdHist + nStdBins
	oKElevN     = oKElevSum + nElev
	oPdTotExp   = oKElevN + nElev
	oPdTotGot   = oPdTotExp + nPdTypes
	oPdDistExp  = oPdTotGot + nPdTypes
	oPdDistGot  = oPdDistExp + nPdTypes*nPdDist
	oPdElevExp  = oPdDistGot + nPdTypes*nPdDist
	oPdElevGot  = oPdElevExp + nPdTypes*nPdElev
	oPdBrgExp   = oPdElevGot + nPdTypes*nPdElev
	oPdBrgGot   = oPdBrgExp + nPdTypes*nPdBrg
	oPdRSSIExp  = oPdBrgGot + nPdTypes*nPdBrg
	oPdRSSIGot  = oPdRSSIExp + nPdTypes*nPdRSSI
	oGapHist    = oPdRSSIGot + nPdTypes*nPdRSSI
	oGapN       = oGapHist + nGapBands*nGapBins
	oGapSum     = oGapN + nGapBands
	oGapMin     = oGapSum + nGapBands
	oLoadSecs   = oGapMin + nGapBands*nGapT
	oLoadFrames = oLoadSecs + nLoad
	oLoadBad    = oLoadFrames + nLoad
	oBDS        = oLoadBad + nLoad
	oIID        = oBDS + nBDS
	oTimeHist   = oIID + nIID
	oTimeN      = oTimeHist + nTimeKind*nTimeBins
	oTimeBad    = oTimeN + nTimeKind
	oAltK1      = oTimeBad + nTimeKind
	oAltK1OK    = oAltK1 + 1
	oAltK2      = oAltK1OK + 1
	oAltK2OK    = oAltK2 + 1
	oGDHist     = oAltK2OK + 1
	oGDSum      = oGDHist + nGD
	oGDSq       = oGDSum + nAltBands
	oGDN        = oGDSq + nAltBands
	oCellTot    = oGDN + nAltBands
	oCellDeg    = oCellTot + nCellSec*nCellRing
	accLen      = oCellDeg + nCellSec*nCellRing
)

type accum [accLen]float64

func (a *accum) reset() { *a = accum{} }

// blend sets a = cur + w*prev.
func (a *accum) blend(cur, prev *accum, w float64) {
	for i := range a {
		a[i] = cur[i] + w*prev[i]
	}
}

func clampInt(i, lo, hi int) int {
	if i < lo {
		return lo
	}
	if i > hi {
		return hi
	}
	return i
}

func rssiBin(s float64) int { return clampInt(int((s-rssiLo)/rssiStep), 0, nRSSI-1) }

func elevBand(e float64) int {
	i := 0
	for i < len(elevEdges) && e >= elevEdges[i] {
		i++
	}
	return i
}

func pdElevBin(e float64) int {
	i := 0
	for i < len(pdElevEdges) && e >= pdElevEdges[i] {
		i++
	}
	return i
}

func gapBand(d float64) int {
	if !finite(d) {
		return nGapBands - 1
	}
	i := 0
	for i < len(gapBandEdges) && d >= gapBandEdges[i] {
		i++
	}
	return i
}

func bdsIndex(b uint8) int {
	if b == 0 {
		return nBDS - 1
	}
	for i, v := range bdsList {
		if v == b {
			return i
		}
	}
	return nBDS - 2
}

func altBand(ft float64) int {
	switch {
	case !finite(ft) || ft < 1000:
		return 0
	case ft < 5000:
		return 1
	case ft < 10000:
		return 2
	case ft < 20000:
		return 3
	case ft < 30000:
		return 4
	case ft < 40000:
		return 5
	}
	return 6
}

// brgBin returns the 10-degree sector of a bearing.
func brgBin(b float64) int {
	i := int(b / 10)
	if i < 0 {
		i += 36
	}
	return clampInt(i, 0, 35)
}
