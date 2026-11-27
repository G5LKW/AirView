package geo

import "math"

// Binning conventions shared by the tracker (writer), the store schema and
// the analytics (reader). They mirror docs/ARCHITECTURE.md "Database":
//
//	heat.cell            0.05° cells, cell = (latIdx<<16)|lonIdx
//	coverage.brg_bin     2° bearing bins, 0..179
//	*.alt_band           7 altitude bands (ft): <1000, 1–5k, 5–10k, 10–20k, 20–30k, 30–40k, >40k
//	signal_hist          10 km distance bins × 1 dB RSSI bins
//	elev_hist.elev_bin   0.5° elevation bins from −2° to +90°
//	range_hour.dist_hist 5 km bins × 120
//	range_hour.sector_max 36 sectors of 10°
//
// All bin functions return -1 for NaN input (the bearing bins also for ±Inf,
// which has no direction) and clamp out-of-range input into the first/last
// bin. HeatCell, which cannot signal an error, maps NaN to index 0.

// Heat grid.
const (
	HeatCellDeg  = 0.05 // cell size in degrees
	HeatLatCells = 3600 // 180 / 0.05
	HeatLonCells = 7200 // 360 / 0.05
	heatPerDeg   = 20   // 1 / HeatCellDeg
)

// HeatCell returns the heat-grid cell id (latIdx<<16)|lonIdx containing the
// position. latIdx = floor((lat+90)·20) in 0..3599, lonIdx =
// floor((lon+180)·20) in 0..7199. Latitude is clamped, longitude wrapped.
func HeatCell(lat, lon float64) uint32 {
	latIdx := heatIndex((ClampLat(lat)+90)*heatPerDeg, HeatLatCells)
	lonIdx := heatIndex((NormalizeLon(lon)+180)*heatPerDeg, HeatLonCells)
	return uint32(latIdx)<<16 | uint32(lonIdx)
}

// heatIndex truncates a non-negative grid coordinate to an index in 0..n-1.
// NaN gives 0; the float is range-checked before the conversion because
// converting NaN or an out-of-range float to int is platform dependent in Go
// (amd64 and arm64 disagree).
func heatIndex(v float64, n int) int {
	if !(v > 0) {
		return 0
	}
	if v >= float64(n) {
		return n - 1
	}
	return int(v)
}

// HeatCellFromIndices builds a cell id from grid indices (clamped/wrapped
// into range).
func HeatCellFromIndices(latIdx, lonIdx int) uint32 {
	if latIdx < 0 {
		latIdx = 0
	} else if latIdx >= HeatLatCells {
		latIdx = HeatLatCells - 1
	}
	lonIdx %= HeatLonCells
	if lonIdx < 0 {
		lonIdx += HeatLonCells
	}
	return uint32(latIdx)<<16 | uint32(lonIdx)
}

// HeatCellIndices splits a cell id into its latitude and longitude indices.
func HeatCellIndices(cell uint32) (latIdx, lonIdx int) {
	return int(cell >> 16), int(cell & 0xFFFF)
}

// HeatCellBounds returns the south-west and north-east corners of a cell.
func HeatCellBounds(cell uint32) (south, west, north, east float64) {
	latIdx, lonIdx := HeatCellIndices(cell)
	south = float64(latIdx)/heatPerDeg - 90
	west = float64(lonIdx)/heatPerDeg - 180
	return south, west, south + HeatCellDeg, west + HeatCellDeg
}

// HeatCellCenter returns the centre of a cell.
func HeatCellCenter(cell uint32) (lat, lon float64) {
	latIdx, lonIdx := HeatCellIndices(cell)
	return (float64(latIdx)+0.5)/heatPerDeg - 90, (float64(lonIdx)+0.5)/heatPerDeg - 180
}

// Bearing bins (coverage table) and 10° sectors (range_hour.sector_max).
const (
	BearingBinDeg = 2.0
	BearingBins   = 180
	SectorDeg     = 10.0
	Sectors       = 36
)

// BearingBin returns the 2° bearing bin 0..179 (bin 0 = [0°,2°)).
func BearingBin(bearingDeg float64) int {
	if bearingDeg-bearingDeg != 0 { // NaN or ±Inf
		return -1
	}
	b := int(NormalizeDeg(bearingDeg) / BearingBinDeg)
	if b >= BearingBins {
		b = BearingBins - 1
	}
	return b
}

// BearingBinCenter returns the centre bearing of a 2° bin in degrees.
func BearingBinCenter(bin int) float64 { return (float64(bin) + 0.5) * BearingBinDeg }

// SectorBin returns the 10° sector 0..35 (sector 0 = [0°,10°)).
func SectorBin(bearingDeg float64) int {
	if bearingDeg-bearingDeg != 0 { // NaN or ±Inf
		return -1
	}
	b := int(NormalizeDeg(bearingDeg) / SectorDeg)
	if b >= Sectors {
		b = Sectors - 1
	}
	return b
}

// SectorCenter returns the centre bearing of a 10° sector in degrees.
func SectorCenter(sector int) float64 { return (float64(sector) + 0.5) * SectorDeg }

// AltBands is the number of altitude bands.
const AltBands = 7

// altBandUpperFt holds the exclusive upper bound in feet of bands 0..5.
var altBandUpperFt = [AltBands - 1]float64{1000, 5000, 10000, 20000, 30000, 40000}

var altBandLabels = [AltBands]string{"<1k", "1-5k", "5-10k", "10-20k", "20-30k", "30-40k", ">40k"}

// altBandTypicalFt is a representative altitude for each band, used when a
// model needs one number per band (e.g. predicted range tables).
var altBandTypicalFt = [AltBands]float64{500, 3000, 7500, 15000, 25000, 35000, 43000}

// AltBand returns the altitude band 0..6 for an altitude in feet:
// 0:<1000 1:1–5k 2:5–10k 3:10–20k 4:20–30k 5:30–40k 6:>=40k. NaN gives -1.
func AltBand(altFt float64) int {
	if altFt != altFt {
		return -1
	}
	for i, up := range altBandUpperFt {
		if altFt < up {
			return i
		}
	}
	return AltBands - 1
}

// AltBandBoundsFt returns the [lo,hi) bounds in feet of a band. The lowest
// band starts at -Inf and the highest ends at +Inf.
func AltBandBoundsFt(band int) (loFt, hiFt float64) {
	if band <= 0 {
		return math.Inf(-1), altBandUpperFt[0]
	}
	if band >= AltBands-1 {
		return altBandUpperFt[AltBands-2], math.Inf(1)
	}
	return altBandUpperFt[band-1], altBandUpperFt[band]
}

// AltBandLabel returns a short label ("<1k", "1-5k", … ">40k") in feet.
func AltBandLabel(band int) string {
	if band < 0 || band >= AltBands {
		return ""
	}
	return altBandLabels[band]
}

// AltBandTypicalFt returns a representative altitude in feet for a band
// (500, 3000, 7500, 15000, 25000, 35000, 43000).
func AltBandTypicalFt(band int) float64 {
	if band < 0 {
		band = 0
	} else if band >= AltBands {
		band = AltBands - 1
	}
	return altBandTypicalFt[band]
}

// Signal histogram (signal_hist): 10 km distance bins × 1 dB RSSI bins.
const (
	DistBinKm = 10.0
	DistBins  = 80 // 0..800 km; the last bin also collects anything farther
	RSSIBinDB = 1.0
	RSSIBins  = 60 // 0 .. -60 dBFS; the last bin also collects anything weaker
)

// DistBin returns the 10 km distance bin: floor(km/10) clamped to
// 0..DistBins-1.
func DistBin(km float64) int { return clampBin(km/DistBinKm, DistBins) }

// DistBinCenterKm returns the centre distance of a 10 km bin.
func DistBinCenterKm(bin int) float64 { return (float64(bin) + 0.5) * DistBinKm }

// RSSIBin returns the 1 dB signal bin index for a level in dBFS (<= 0):
// bin i covers [-(i+1), -i) dBFS, so -23.4 dBFS falls in bin 23 and -23.0
// in bin 22. Levels >= 0 clamp to bin 0, levels below -60 to the last bin,
// NaN gives -1.
//
// The index is for in-memory arrays. On disk (signal_hist.rssi_bin) a bin
// is stored as floor(dBFS) = RSSIBinStored(i) = -(i+1), which is exactly
// store.RSSIBin of the same level: the two helpers agree on every level.
func RSSIBin(rssiDBFS float64) int {
	if rssiDBFS != rssiDBFS {
		return -1
	}
	return clampBin(-math.Floor(rssiDBFS/RSSIBinDB)-1, RSSIBins)
}

// RSSIBinStored returns the on-disk signal_hist.rssi_bin of bin index i:
// floor(dBFS) of the levels in the bin, -(i+1).
func RSSIBinStored(i int) int { return -(i + 1) }

// RSSIBinCenter returns the centre level in dBFS of an RSSI bin.
func RSSIBinCenter(bin int) float64 { return -(float64(bin) + 0.5) * RSSIBinDB }

// Elevation histogram (elev_hist): 0.5° bins from −2° to +90°.
const (
	ElevBinDeg = 0.5
	ElevMinDeg = -2.0
	ElevMaxDeg = 90.0
	ElevBins   = 184 // (90 − (−2)) / 0.5
)

// ElevBin returns the 0.5° elevation bin: floor((elev+2)/0.5) clamped to
// 0..183 (bin 4 = [0°,0.5°)).
func ElevBin(elevDeg float64) int { return clampBin((elevDeg-ElevMinDeg)/ElevBinDeg, ElevBins) }

// ElevBinCenter returns the centre elevation in degrees of a bin.
func ElevBinCenter(bin int) float64 { return ElevMinDeg + (float64(bin)+0.5)*ElevBinDeg }

// Hourly range histogram (range_hour.dist_hist): 5 km bins × 120.
const (
	RangeBinKm = 5.0
	RangeBins  = 120
)

// RangeBin returns the 5 km range-histogram bin clamped to 0..119.
func RangeBin(km float64) int { return clampBin(km/RangeBinKm, RangeBins) }

// RangeBinCenterKm returns the centre distance of a 5 km range bin.
func RangeBinCenterKm(bin int) float64 { return (float64(bin) + 0.5) * RangeBinKm }

func clampBin(v float64, n int) int {
	if v != v {
		return -1
	}
	if v <= 0 {
		return 0
	}
	if v >= float64(n) {
		return n - 1
	}
	return int(v)
}
