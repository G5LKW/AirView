package store

import (
	"math"

	"airview/internal/geo"
)

// The functions in this file define the bin encodings used by the
// statistics tables, so that the tracker (which writes) and the analytics
// (which read) agree on them.

// DayOf returns the Unix day number of Unix time t (seconds) - the "day"
// column of coverage, signal_hist and elev_hist. DayStart is its inverse.
func DayOf(t int64) int { return int(floorDiv(t, 86400)) }

// DayStart returns the Unix time (seconds, UTC midnight) at which day begins.
func DayStart(day int) int64 { return int64(day) * 86400 }

// AltBands is the number of altitude bands.
const AltBands = 7

// AltBand returns the altitude band of an altitude in feet:
//
//	0: <1000   1: 1-5k   2: 5-10k   3: 10-20k   4: 20-30k   5: 30-40k   6: >=40k
func AltBand(altFt float64) int {
	switch {
	case !(altFt >= 1000): // also NaN
		return 0
	case altFt < 5000:
		return 1
	case altFt < 10000:
		return 2
	case altFt < 20000:
		return 3
	case altFt < 30000:
		return 4
	case altFt < 40000:
		return 5
	}
	return 6
}

// AltBandRange returns the [lo, hi) altitude range in feet of a band; hi is
// +Inf for the top band and lo is -Inf for band 0.
func AltBandRange(band int) (lo, hi float64) {
	edges := [...]float64{math.Inf(-1), 1000, 5000, 10000, 20000, 30000, 40000, math.Inf(1)}
	if band < 0 {
		band = 0
	}
	if band >= AltBands {
		band = AltBands - 1
	}
	return edges[band], edges[band+1]
}

// BrgBins is the number of bearing bins in the coverage table.
const BrgBins = 180

// BrgBin returns the 2 degree bearing bin (0..179) of a bearing in degrees.
func BrgBin(deg float64) int {
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return 0
	}
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	b := int(deg / 2)
	if b >= BrgBins {
		b = 0
	}
	return b
}

// DistBinKm is the width of a signal_hist distance bin.
const DistBinKm = 10

// DistBin returns the 10 km distance bin of a distance in km (0..99; the
// last bin holds everything from 990 km).
func DistBin(km float64) int {
	if !(km > 0) {
		return 0
	}
	b := int(km / DistBinKm)
	if b > 99 {
		b = 99
	}
	return b
}

// RSSIBin returns the on-disk 1 dB signal bin (signal_hist.rssi_bin) of a
// level in dBFS: floor(dBFS) clamped to -60..-1, so bin b covers [b, b+1)
// dBFS; levels >= 0 go to -1, levels below -60 (and NaN) to -60. It is
// defined through geo.RSSIBin, the index the tracker accumulates in memory,
// so writers and readers agree: RSSIBin(x) == geo.RSSIBinStored(geo.RSSIBin(x)).
func RSSIBin(dbfs float64) int {
	if math.IsNaN(dbfs) {
		return geo.RSSIBinStored(geo.RSSIBins - 1)
	}
	return geo.RSSIBinStored(geo.RSSIBin(dbfs))
}

// Elevation histogram geometry: 0.5 degree bins from -2 to +90 degrees.
const (
	ElevBins   = 184
	ElevBinDeg = 0.5
	ElevMinDeg = -2.0
)

// ElevBin returns the elevation bin (0..183) of an elevation angle in degrees.
func ElevBin(deg float64) int {
	if math.IsNaN(deg) {
		return 0
	}
	b := int(math.Floor((deg - ElevMinDeg) / ElevBinDeg))
	if b < 0 {
		b = 0
	}
	if b >= ElevBins {
		b = ElevBins - 1
	}
	return b
}

// ElevBinLow returns the lower edge, in degrees, of an elevation bin.
func ElevBinLow(bin int) float64 { return ElevMinDeg + float64(bin)*ElevBinDeg }

// HeatCellDeg is the size of a heat-map cell in degrees.
const HeatCellDeg = 0.05

// HeatCell returns the heat-grid cell of a position:
// (latIdx << 16) | lonIdx with latIdx = floor((lat+90)/0.05) (0..3599) and
// lonIdx = floor((lon+180)/0.05) (0..7199).
func HeatCell(lat, lon float64) int64 {
	if math.IsNaN(lat) || math.IsNaN(lon) {
		return 0
	}
	li := int64(math.Floor((lat + 90) / HeatCellDeg))
	if li < 0 {
		li = 0
	}
	if li > 3599 {
		li = 3599
	}
	lon = math.Mod(lon+180, 360)
	if lon < 0 {
		lon += 360
	}
	lo := int64(math.Floor(lon / HeatCellDeg))
	if lo < 0 {
		lo = 0
	}
	if lo > 7199 {
		lo = 7199
	}
	return li<<16 | lo
}

// HeatCellCenter returns the centre of a heat-grid cell.
func HeatCellCenter(cell int64) (lat, lon float64) {
	li := cell >> 16
	lo := cell & 0xffff
	return (float64(li)+0.5)*HeatCellDeg - 90, (float64(lo)+0.5)*HeatCellDeg - 180
}

// MetLevel returns the 1000 ft level of an altitude in feet for
// met_profile: floor(alt/1000), so level L covers [L*1000, (L+1)*1000) ft.
// It is clamped to -1..60.
func MetLevel(altFt float64) int {
	if math.IsNaN(altFt) {
		return 0
	}
	l := math.Floor(altFt / 1000)
	if l < -1 {
		l = -1
	}
	if l > 60 {
		l = 60
	}
	return int(l)
}

// floorDiv divides rounding towards negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// floorTo rounds t down to a multiple of step (step > 0).
func floorTo(t, step int64) int64 { return floorDiv(t, step) * step }
