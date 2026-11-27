package routes

import (
	"math"
	"time"

	"airview/internal/geo"
)

// Progress and ETA (docs/ROUTES-DESIGN.md §3.4).

// arrivalExcessNM is the typical extra track flown beyond the great circle
// into an airport of a size class (vectoring, the approach), by size.
func arrivalExcessNM(size int) float64 {
	switch {
	case size >= 3:
		return 25
	case size == 2:
		return 12
	}
	return 4
}

// approachKt is the final approach speed by ADS-B emitter category.
func approachKt(cat string) float64 {
	switch cat {
	case "A1":
		return 90
	case "A2":
		return 110
	case "A3":
		return 140
	case "A4", "A5":
		return 150
	}
	return 140
}

// ETA estimates the arrival at dest from the observation: cruise at the
// current ground speed to the 3:1 top of descent, the descent above FL100
// at the mean of the ground speed and 290 kt, 230 kt from FL100 to 10 NM out
// and the final 10 NM at the approach speed of the category. sigma is the
// 1-sigma uncertainty. ok=false when the ground speed is unknown or too low,
// or the remaining distance exceeds 1500 NM.
func ETA(dest Stop, o Obs, now time.Time) (eta time.Time, sigma time.Duration, ok bool) {
	if !finite(o.GS) || o.GS < 60 || !dest.Known || o.OnGround {
		return time.Time{}, 0, false
	}
	nm := geo.KmToNM(geo.HaversineKm(o.Lat, o.Lon, dest.Lat, dest.Lon)) + arrivalExcessNM(dest.Size)
	if nm > 1500 {
		return time.Time{}, 0, false
	}
	alt := o.AltFt
	if !finite(alt) {
		alt = 30000
	}
	elev := dest.ElevFt
	if !finite(elev) {
		elev = 0
	}
	vapp := approachKt(o.Category)
	hours := 0.0
	rem := nm
	// final 10 NM
	fin := math.Min(rem, 10)
	hours += fin / math.Min(vapp, o.GS)
	rem -= fin
	// FL100 (or the current altitude) down to 10 NM out
	low := math.Min(rem, math.Max(0, math.Min(alt, 10000)-elev)/1000*3)
	hours += low / math.Min(230, math.Max(o.GS, vapp))
	rem -= low
	// descent above FL100
	high := math.Min(rem, math.Max(0, alt-10000)/1000*3)
	hours += high / ((o.GS + 290) / 2)
	rem -= high
	// cruise
	hours += rem / o.GS
	sec := hours * 3600
	sig := math.Max(90+0.02*sec, 90)
	eta = now.Add(time.Duration(sec * float64(time.Second)))
	return eta, time.Duration(sig * float64(time.Second)), true
}
