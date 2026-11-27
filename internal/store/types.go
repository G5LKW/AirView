package store

import (
	"encoding/binary"
	"math"

	"airview/internal/model"
)

// Conventions for every row type in this package:
//
//   - Times are Unix SECONDS (as in the database), unlike the HTTP API which
//     uses milliseconds. The row types that have a MarshalJSON method convert
//     to the API form (ms timestamps, 6-digit hex ICAO, unknown values
//     omitted), so they can be handed to encoding/json directly.
//   - An unknown / not-applicable float is NaN and is stored as SQL NULL
//     (and read back as NaN). The zero value 0 means "known to be zero", so
//     build rows with the New* constructors or set optional fields to
//     math.NaN() explicitly.
//   - ICAO is the 24-bit address as a number; 0 means "none".
//   - Ant is the antenna ID, "" = all antennas combined.

// Position source codes stored in positions.src and met_obs.src. They are
// the same numbers as model.TrailPoint.Src.
const (
	SrcADSB  uint8 = 0 // ADS-B (DF17)
	SrcMLAT  uint8 = 1 // multilateration
	SrcTISB  uint8 = 2 // TIS-B / ADS-R (DF18)
	SrcOther uint8 = 3 // SBS/JSON feed without further detail
)

// Bits of aircraft.src_mask / flights.src_mask: 1<<Src* for position
// sources, plus MaskModeS for "heard, Mode S only, never a position".
const (
	MaskADSB  = 1 << SrcADSB
	MaskMLAT  = 1 << SrcMLAT
	MaskTISB  = 1 << SrcTISB
	MaskOther = 1 << SrcOther
	MaskModeS = 1 << 4
)

// Bits of aircraft.flags and acdb.flags (the numbering used by the
// readsb / tar1090 aircraft database "dbFlags").
const (
	FlagMilitary    = 1
	FlagInteresting = 2
	FlagPIA         = 4
	FlagLADD        = 8
)

// AircraftSeen is one accumulated sighting of an airframe, merged into the
// aircraft table by Batch.UpsertAircraft:
//
//	first_seen = min, last_seen = max, msgs/flights/positions are ADDED
//	(pass deltas since the previous upsert), max_dist_km / max_alt = max,
//	src_mask / flags are OR-ed, and a non-empty text field replaces the
//	stored one (an empty string never erases).
type AircraftSeen struct {
	ICAO      uint32
	FirstSeen int64 // 0 = same as LastSeen
	LastSeen  int64
	Callsign  string
	Reg       string
	Type      string // ICAO type designator
	Operator  string
	Country   string
	Category  string  // ADS-B emitter category, e.g. "A3"
	Msgs      int64   // delta
	Flights   int64   // delta (1 when a new flight was opened)
	Positions int64   // delta
	MaxDistKm float64 // 0 / NaN = unknown
	MaxAlt    float64 // ft; NaN = unknown
	SrcMask   int
	Flags     int
}

// AircraftRow is a row of the aircraft table joined with the imported
// aircraft database (acdb): Reg, Type and Operator fall back to the acdb
// values when the tracker never learned them.
type AircraftRow struct {
	ICAO      uint32
	FirstSeen int64
	LastSeen  int64
	Callsign  string
	Reg       string
	Type      string
	TypeDesc  string // acdb "desc"
	Operator  string
	Country   string
	Category  string
	Msgs      int64
	Flights   int64
	Positions int64
	MaxDistKm float64 // 0 = unknown
	MaxAlt    float64 // ft; NaN = unknown
	SrcMask   int
	Flags     int // aircraft.flags | acdb.flags
	Year      int // year built from acdb, 0 = unknown
}

// Flight is one contact session (flights table). Batch.UpsertFlights writes
// the complete row keyed by ID, so "open", "update" and "close" are the same
// operation: allocate the ID once with Store.NextFlightID, keep the Flight in
// memory, and upsert it whenever it changed.
//
// Reg, Type, Operator and Country are filled by the read API (join with
// aircraft / acdb) and ignored on writes.
type Flight struct {
	ID         int64
	ICAO       uint32
	Callsign   string
	Squawk     string
	Start      int64 // first message
	End        int64 // last message so far
	Msgs       int64
	Positions  int64
	SrcMask    int
	MaxDistKm  float64 // NaN = no position
	MaxDistBrg float64 // bearing at max distance, deg
	MinDistKm  float64
	MinAlt     float64 // ft
	MaxAlt     float64 // ft
	MaxGS      float64 // kt
	RSSIMin    float64 // dBFS
	RSSIMax    float64
	RSSIAvg    float64
	FirstLat   float64
	FirstLon   float64
	LastLat    float64
	LastLon    float64
	Emergency  string // "" or the model.Aircraft.Emergency value seen

	Reg      string // read-only
	Type     string // read-only
	Operator string // read-only
	Country  string // read-only
	Dep      string // read-only: departure airport code ("" = unknown or not resolved yet)
	Arr      string // read-only: arrival airport code
}

// NewFlight returns a Flight with every optional numeric field unknown (NaN).
func NewFlight(id int64, icao uint32, start int64) Flight {
	nan := math.NaN()
	return Flight{
		ID: id, ICAO: icao, Start: start, End: start,
		MaxDistKm: nan, MaxDistBrg: nan, MinDistKm: nan, MinAlt: nan, MaxAlt: nan, MaxGS: nan,
		RSSIMin: nan, RSSIMax: nan, RSSIAvg: nan,
		FirstLat: nan, FirstLon: nan, LastLat: nan, LastLon: nan,
	}
}

// Position is one stored trail point (positions table, primary key
// (FlightID, T): a second point in the same second replaces the first).
// Stored resolution: lat/lon 1e-5 deg, alt 1 ft, gs 1 kt, track 1 deg,
// rssi 0.1 dB.
type Position struct {
	FlightID int64
	T        int64
	Lat      float64
	Lon      float64
	Alt      float64 // ft; NaN unknown
	GS       float64 // kt; NaN unknown
	Track    float64 // deg true; NaN unknown
	RSSI     float64 // dBFS; NaN unknown
	Src      uint8   // Src* code
}

// TrailPoint converts p to the API trail type (T in Unix ms).
func (p Position) TrailPoint() model.TrailPoint {
	return model.TrailPoint{T: p.T * 1000, Lat: p.Lat, Lon: p.Lon, Alt: p.Alt, GS: p.GS, RSSI: p.RSSI, Src: p.Src}
}

// PositionFromTrail converts an in-memory trail point to a storable row.
// track may be NaN.
func PositionFromTrail(flightID int64, tp model.TrailPoint, track float64) Position {
	return Position{
		FlightID: flightID, T: tp.T / 1000, Lat: tp.Lat, Lon: tp.Lon,
		Alt: tp.Alt, GS: tp.GS, Track: track, RSSI: tp.RSSI, Src: tp.Src,
	}
}

// StatsRow is one row of stats_minute (written) or stats_hour (rolled up).
// Batch.AddMinuteStats MERGES a row into an existing (T, Ant) row: counters
// are added, the aircraft gauges and MaxKm/RSSIMax take the maximum, RSSIMin
// the minimum and RSSIAvg the frame-weighted mean, so a minute written in
// two parts (e.g. around a restart) ends up correct.
type StatsRow struct {
	T   int64 // start of the minute (hour for stats_hour); rounded down on write
	Ant string

	// Counters.
	Frames        int64 // all frames received (incl. CRCBad)
	DF0           int64
	DF4           int64
	DF5           int64
	DF11          int64
	DF16          int64
	DF17          int64
	DF18          int64
	DF20          int64
	DF21          int64
	DFOther       int64
	CRCBad        int64
	Positions     int64 // decoded positions
	MLATPositions int64

	// Gauges: distinct aircraft during the interval.
	Aircraft    int // heard
	AircraftPos int // with a position
	ADSB        int
	MLAT        int
	ModeS       int // Mode S only

	MaxKm   float64 // farthest position; NaN unknown
	RSSIAvg float64 // dBFS; NaN unknown
	RSSIMin float64
	RSSIMax float64

	// Secs is the part of the interval the receiver was actually counting,
	// in seconds: below 60 for the first minute after a start (or reset)
	// and the last one before a stop; the sum over the minutes for
	// stats_hour. 0 = unknown (rows written before this was recorded):
	// see CoveredSec. Merged rows add up, capped at 60 per minute.
	Secs float64

	// Only set when reading stats_hour.
	AircraftAvg float64 // mean of the per-minute Aircraft gauge
	Minutes     int     // minute rows aggregated (60 = receiver up the whole hour)
}

// CoveredSec is the time base of the row's rates in seconds: Secs when
// known, else a full minute per minute row (60, or Minutes*60 for an hour
// row; hour = true without Minutes means a full hour).
func (r *StatsRow) CoveredSec(hour bool) float64 {
	if r.Secs > 0 {
		return r.Secs
	}
	if !hour {
		return 60
	}
	if r.Minutes > 0 {
		return float64(r.Minutes) * 60
	}
	return 3600
}

// Range histogram geometry for range_hour.
const (
	DistHistBins  = 120 // dist_hist bins
	DistHistBinKm = 5   // bin width, km (last bin also holds everything beyond 600 km)
	SectorBins    = 36  // sector_max bins
	SectorBinDeg  = 10  // bin width, degrees of bearing
)

// RangeHour is one row of range_hour: the hourly range metrics of one
// antenna, the dependent variable for weather correlation and forecasting.
//
// Batch.AddRangeHours merges into an existing (T, Ant) row: histograms are
// added bin by bin, sector maxima and MaxKm take the maximum, Positions and
// FarCount are added, Aircraft takes the maximum, ResidDB the ResidN-weighted
// mean, and the percentiles are recomputed from the merged histogram.
type RangeHour struct {
	T         int64 // start of the hour; rounded down on write
	Ant       string
	MaxKm     float64   // NaN = no positions
	P99Km     float64   //
	P95Km     float64   //
	P90Km     float64   //
	P50Km     float64   //
	Positions int64     //
	Aircraft  int       // distinct aircraft with a position
	FarCount  int64     // positions beyond the caller's "far" threshold
	ResidDB   float64   // mean of rssi + 20*log10(slant_km); NaN unknown
	ResidN    int64     // samples in ResidDB
	DistHist  []uint32  // DistHistBins counts (nil = none)
	SectorMax []float32 // SectorBins max km per 10 deg of bearing (nil = none)
}

// CoverageInc is an increment for the polar coverage table. Merge: MaxKm =
// max, Count / RSSISum / RSSIN added.
type CoverageInc struct {
	Day     int // Unix day number (DayOf)
	Ant     string
	BrgBin  int // 2 deg bins, 0..179 (BrgBin)
	AltBand int // 0..6 (AltBand)
	MaxKm   float64
	Count   int64
	RSSISum float64
	RSSIN   int64
}

// SignalHistInc is an increment for the RSSI-vs-distance histogram.
type SignalHistInc struct {
	Day     int
	Ant     string
	DistBin int // 10 km bins (DistBin)
	RSSIBin int // 1 dB bins: floor(dBFS) in -60..-1, e.g. -23 covers [-23,-22) (RSSIBin)
	Count   int64
}

// ElevHistInc is an increment for the elevation-angle pattern. All three
// numbers are added. Count is the number of samples behind RSSISum and
// ResidSum.
type ElevHistInc struct {
	Day      int
	Ant      string
	ElevBin  int // 0.5 deg bins from -2 deg (ElevBin)
	Count    int64
	RSSISum  float64
	ResidSum float64
}

// HeatInc is an increment for the all-time position grid. Merge: Count /
// RSSISum / RSSIN added, MinAlt = min, MaxAlt = max, LastT = max.
type HeatInc struct {
	Cell    int64 // HeatCell(lat, lon)
	Ant     string
	Count   int64
	RSSISum float64
	RSSIN   int64   // samples in RSSISum (positions without RSSI still count in Count)
	MinAlt  float64 // ft; NaN unknown
	MaxAlt  float64 // ft; NaN unknown
	LastT   int64
}

// Weather is one hourly row of weather_obs, weather_fc or weather_fc_log.
// All measurements are float64 with NaN = missing; use NewWeather.
type Weather struct {
	T      int64 // valid time (start of hour)
	Issued int64 // weather_fc / weather_fc_log: when the forecast was fetched
	LeadH  int   // weather_fc_log: forecast lead time in hours (set on read; derived from T-Issued on write)

	TempC           float64
	DewpointC       float64
	RH              float64 // %
	PressureMSL     float64 // hPa
	SurfacePressure float64 // hPa
	WindKmh         float64
	WindDir         float64 // deg
	GustKmh         float64
	Cloud           float64 // % total
	CloudLow        float64
	CloudMid        float64
	CloudHigh       float64
	PrecipMM        float64
	RainMM          float64
	SnowCM          float64
	VisibilityM     float64
	CAPE            float64 // J/kg
	WeatherCode     float64 // WMO code
	IsDay           float64 // 1 day, 0 night
	NSurface        float64 // surface refractivity, N-units
	DNDH            float64 // refractivity gradient, N/km
	KFactor         float64 // effective earth radius factor
	Duct            float64 // ducting indicator as defined by internal/weather (0 = none)
	ProfileJSON     string  // pressure-level profile (weather_obs / weather_fc only)
}

// NewWeather returns a Weather for time t with every measurement missing.
func NewWeather(t int64) Weather {
	nan := math.NaN()
	return Weather{
		T: t, TempC: nan, DewpointC: nan, RH: nan, PressureMSL: nan, SurfacePressure: nan,
		WindKmh: nan, WindDir: nan, GustKmh: nan, Cloud: nan, CloudLow: nan, CloudMid: nan, CloudHigh: nan,
		PrecipMM: nan, RainMM: nan, SnowCM: nan, VisibilityM: nan, CAPE: nan, WeatherCode: nan, IsDay: nan,
		NSurface: nan, DNDH: nan, KFactor: nan, Duct: nan,
	}
}

// NTPSample is one NTP measurement (primary key T: one sample per second).
type NTPSample struct {
	T        int64
	Server   string
	OffsetMs float64 // true UTC - system clock
	DelayMs  float64 // round trip
	Stratum  int
}

// MetObs is one aircraft-derived meteorological observation.
type MetObs struct {
	T       int64
	ICAO    uint32
	Lat     float64
	Lon     float64
	Alt     float64 // ft
	WindDir float64 // deg true, FROM; NaN unknown
	WindKt  float64 // NaN unknown
	OATC    float64 // static air temperature, deg C; NaN unknown
	Src     uint8   // defined by internal/met (e.g. 0 = computed EHS, 1 = MRAR)
}

// MetProfileInc is an increment for the hourly vertical profile; every
// number is added.
type MetProfileInc struct {
	Hour  int64 // start of the hour, Unix seconds (rounded down on write)
	Level int   // 1000 ft level (MetLevel)
	USum  float64
	VSum  float64
	TSum  float64
	NWind int64
	NTemp int64
}

// RecordMode chooses when Batch.SetRecord replaces an existing record.
type RecordMode uint8

const (
	RecordMax    RecordMode = iota // keep the larger Value
	RecordMin                      // keep the smaller Value
	RecordAlways                   // always overwrite
)

// Record is one all-time record (records table).
type Record struct {
	Key    string // e.g. "max_dist_km"
	Value  float64
	ICAO   uint32
	T      int64
	Detail string // JSON document or ""
}

// Event is one row of the events table.
type Event struct {
	ID     int64 // assigned by the database (read only)
	T      int64
	Kind   string // e.g. "emergency", "record", "feed_up", "feed_down"
	ICAO   uint32
	Detail string // JSON document or ""
}

// ACDBEntry is one row of the imported aircraft database.
type ACDBEntry struct {
	ICAO     uint32
	Reg      string
	Type     string // ICAO type designator
	Desc     string // type description, e.g. "AIRBUS A-320neo"
	Operator string
	Flags    int // Flag* bits
	Year     int // 0 = unknown
}

// ReceiverStatus is one receiver status frame (GPS lock etc.), keyed (T, Feed).
type ReceiverStatus struct {
	T    int64
	Feed string
	JSON string
}

// EncodeDistHist packs a distance histogram as little-endian uint32 (the
// range_hour.dist_hist BLOB). nil / empty input gives nil.
func EncodeDistHist(h []uint32) []byte {
	if len(h) == 0 {
		return nil
	}
	b := make([]byte, 4*len(h))
	for i, v := range h {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	return b
}

// DecodeDistHist is the inverse of EncodeDistHist and of
// EncodeDistHistCompact (stations.db).
func DecodeDistHist(b []byte) []uint32 {
	if isCompact(b) {
		return decodeDistHistCompact(b)
	}
	n := len(b) / 4
	if n == 0 {
		return nil
	}
	h := make([]uint32, n)
	for i := range h {
		h[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return h
}

// EncodeSectorMax packs per-sector maxima as little-endian float32 (the
// range_hour.sector_max BLOB).
func EncodeSectorMax(s []float32) []byte {
	if len(s) == 0 {
		return nil
	}
	b := make([]byte, 4*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// DecodeSectorMax is the inverse of EncodeSectorMax.
func DecodeSectorMax(b []byte) []float32 {
	if isCompact(b) {
		return decodeSectorMaxCompact(b)
	}
	n := len(b) / 4
	if n == 0 {
		return nil
	}
	s := make([]float32, n)
	for i := range s {
		s[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return s
}

// HistPercentile returns the q-quantile (0..1) of a histogram with
// equal-width bins of binWidth starting at 0, interpolating linearly inside
// the bin. It returns NaN for an empty histogram.
func HistPercentile(hist []uint32, binWidth, q float64) float64 {
	var total uint64
	for _, c := range hist {
		total += uint64(c)
	}
	if total == 0 {
		return math.NaN()
	}
	q = math.Max(0, math.Min(1, q))
	target := q * float64(total)
	var cum float64
	for i, c := range hist {
		if c == 0 {
			continue
		}
		next := cum + float64(c)
		if next >= target {
			frac := (target - cum) / float64(c)
			return (float64(i) + frac) * binWidth
		}
		cum = next
	}
	return float64(len(hist)) * binWidth
}
