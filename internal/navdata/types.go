package navdata

import (
	"math"
	"strings"
)

// SizeClass orders airports for map overlays (bigger = more important).
type SizeClass int8

// Size classes derived from the OurAirports "type" column.
const (
	SizeClosed SizeClass = -1 // "closed"
	SizeOther  SizeClass = 0  // heliport, seaplane_base, balloonport, unknown
	SizeSmall  SizeClass = 1  // small_airport
	SizeMedium SizeClass = 2  // medium_airport
	SizeLarge  SizeClass = 3  // large_airport
)

// SizeOf maps an OurAirports airport type to its size class.
func SizeOf(airportType string) SizeClass {
	switch airportType {
	case "large_airport":
		return SizeLarge
	case "medium_airport":
		return SizeMedium
	case "small_airport":
		return SizeSmall
	case "closed":
		return SizeClosed
	}
	return SizeOther
}

// ParseSize parses a size class from "large"|"medium"|"small"|"other"|"closed"
// or the numeric value; ok=false for anything else.
func ParseSize(s string) (SizeClass, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "large", "3", "large_airport":
		return SizeLarge, true
	case "medium", "2", "medium_airport":
		return SizeMedium, true
	case "small", "1", "small_airport":
		return SizeSmall, true
	case "other", "0", "heliport", "seaplane_base", "balloonport":
		return SizeOther, true
	case "closed", "-1", "all":
		return SizeClosed, true
	}
	return 0, false
}

// String returns "large", "medium", "small", "other" or "closed".
func (s SizeClass) String() string {
	switch s {
	case SizeLarge:
		return "large"
	case SizeMedium:
		return "medium"
	case SizeSmall:
		return "small"
	case SizeClosed:
		return "closed"
	}
	return "other"
}

// Airport is one OurAirports airport row (plus its runways when requested).
type Airport struct {
	ID           int64     `json:"id"`
	Ident        string    `json:"ident"`
	Type         string    `json:"type"` // OurAirports type, e.g. "large_airport"
	Size         SizeClass `json:"size"`
	Name         string    `json:"name"`
	Lat          float64   `json:"lat"`
	Lon          float64   `json:"lon"`
	ElevFt       *int      `json:"elevFt,omitempty"`
	Continent    string    `json:"continent,omitempty"`
	Country      string    `json:"country,omitempty"` // ISO 3166-1 alpha-2
	CountryName  string    `json:"countryName,omitempty"`
	Region       string    `json:"region,omitempty"` // ISO 3166-2
	Municipality string    `json:"municipality,omitempty"`
	Scheduled    bool      `json:"scheduled,omitempty"`
	ICAO         string    `json:"icao,omitempty"`
	IATA         string    `json:"iata,omitempty"`
	GPS          string    `json:"gps,omitempty"`
	Local        string    `json:"local,omitempty"`
	DistKm       *float64  `json:"distKm,omitempty"` // set by radius / nearest queries
	Runways      []Runway  `json:"runways,omitempty"`
}

// Code returns the best display code: ICAO, else GPS code, else ident.
func (a *Airport) Code() string {
	switch {
	case a.ICAO != "":
		return a.ICAO
	case a.GPS != "":
		return a.GPS
	}
	return a.Ident
}

// elevFt returns the elevation or 0 when unknown.
func (a *Airport) elevFt() float64 {
	if a.ElevFt == nil {
		return 0
	}
	return float64(*a.ElevFt)
}

// RunwayEnd is one end ("le" = low end, "he" = high end) of a runway.
type RunwayEnd struct {
	Ident       string   `json:"ident,omitempty"` // e.g. "09L"
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	ElevFt      *int     `json:"elevFt,omitempty"`
	HeadingT    *float64 `json:"headingT,omitempty"` // degrees true (as published)
	DisplacedFt *int     `json:"displacedFt,omitempty"`
}

// Runway is one OurAirports runway row.
type Runway struct {
	ID           int64     `json:"id"`
	AirportID    int64     `json:"airportId"`
	AirportIdent string    `json:"airportIdent"`
	LengthFt     *int      `json:"lengthFt,omitempty"`
	WidthFt      *int      `json:"widthFt,omitempty"`
	Surface      string    `json:"surface,omitempty"`
	Lighted      bool      `json:"lighted,omitempty"`
	Closed       bool      `json:"closed,omitempty"`
	LE           RunwayEnd `json:"le"`
	HE           RunwayEnd `json:"he"`
}

// Navaid is one OurAirports navaid row.
type Navaid struct {
	ID         int64    `json:"id"`
	Ident      string   `json:"ident"`
	Name       string   `json:"name"`
	Type       string   `json:"type"` // VOR, VOR-DME, VORTAC, DME, NDB, NDB-DME, TACAN
	FreqKHz    *int     `json:"freqKHz,omitempty"`
	Lat        float64  `json:"lat"`
	Lon        float64  `json:"lon"`
	ElevFt     *int     `json:"elevFt,omitempty"`
	Country    string   `json:"country,omitempty"`
	DMEChannel string   `json:"dmeChannel,omitempty"`
	MagVarDeg  *float64 `json:"magVarDeg,omitempty"`
	Usage      string   `json:"usage,omitempty"` // HI, LO, BOTH, TERMINAL, RNAV
	Power      string   `json:"power,omitempty"`
	Airport    string   `json:"airport,omitempty"` // associated airport ident
}

// Country is one OurAirports countries.csv row.
type Country struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Continent string `json:"continent,omitempty"`
}

// RouteRecord is one stored callsign route (standing data).
type RouteRecord struct {
	Callsign string   `json:"callsign"`
	Airline  string   `json:"airline,omitempty"` // ICAO airline designator
	Number   string   `json:"number,omitempty"`
	Codes    []string `json:"codes"` // airport codes in flight order (2 or more)
}

// fptr returns a pointer to v, or nil when v is not finite.
func fptr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// iptr returns a pointer to v.
func iptr(v int) *int { return &v }

// round rounds v to d decimals.
func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}
