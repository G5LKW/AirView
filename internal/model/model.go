// Package model holds the shared contract types for AirView: configuration,
// raw receiver frames, and the JSON DTOs served by the HTTP API.
//
// This package is the single source of truth for cross-package data shapes.
// It must stay dependency-free (stdlib only) and must not import any other
// airview package.
//
// API unit conventions (the web UI converts for display):
//   - altitude: feet, speed: knots, vertical rate: ft/min
//   - distance: kilometres, angles: degrees (true north, clockwise)
//   - signal: dBFS (negative numbers, 0 = full scale)
//   - timestamps: Unix milliseconds (UTC)
//   - weather: °C, hPa, km/h, mm, %
package model

import "time"

// Version is the application version shown in the UI and /api/status.
const Version = "1.0.0"

// AppName is the product name.
const AppName = "AirView"

// FrameKind identifies the type of a raw receiver frame.
type FrameKind uint8

const (
	FrameModeAC     FrameKind = 1 // 2 bytes Mode A/C
	FrameModeSShort FrameKind = 2 // 7 bytes (56 bit)
	FrameModeSLong  FrameKind = 3 // 14 bytes (112 bit)
	FrameStatus     FrameKind = 4 // receiver status / config frame (Radarcape Beast type '4')
)

// RawFrame is one frame as delivered by a feed (Beast / AVR / hub) before
// Mode S decoding. Feeds produce these; the tracker consumes them.
type RawFrame struct {
	Recv time.Time // host system-clock time the frame was read from the socket
	// GPSNs is the receiver's GPS timestamp as nanoseconds since UTC midnight
	// (Radarcape GPS-timestamp mode). -1 when not available.
	GPSNs int64
	// Clock12MHz is the legacy 48-bit 12 MHz free-running counter timestamp
	// (classic Beast / dump1090). 0 when not available.
	Clock12MHz uint64
	Signal     float64   // RSSI in dBFS (<= 0). NaN when unknown.
	Kind       FrameKind //
	Data       []byte    // 2, 7 or 14 bytes; for FrameStatus the raw status payload
	FeedID     string    // Config.Feeds[].ID that produced this frame
	// Channel is the receiver RF channel index (0-based) if the protocol carries
	// it, else -1. NOTE: no documented Radarcape output carries a channel flag
	// (the EVO 2CH is antenna diversity into one data stream), so Beast/AVR/SBS
	// feeds always report -1. Attribute frames to an antenna via Feed.AntennaID;
	// only the simulator and hub relays of simulator data set Channel.
	Channel int
	MLAT    bool // frame is a synthetic MLAT result (not received off-air by us)

	// --- network fields (docs/aggregator/DESIGN.md §4.3, §6.1). In
	// standalone mode they stay zero (Station = LocalStation) and the legacy
	// GPSNs / Clock12MHz fields above remain authoritative. ---

	// Station is the receiving station (0 = local / standalone).
	Station StationIdx
	// Epoch is the receiver clock epoch Ticks belong to. Ticks of different
	// epochs of one station are not comparable.
	Epoch uint32
	// ClockCls is the class of the receiver clock (ClockNone = untimed).
	ClockCls ClockClass
	// Ticks is the unwrapped, monotonic receiver timestamp of this epoch in
	// units of ClockCls.NominalHz(): for GPS classes nanoseconds as
	// dayIndex*NsPerDay + nsOfDay (see GPSTicks), else the unwrapped counter.
	// Only meaningful when ClockCls.Timed().
	Ticks uint64
	// HostNs is the host UTC time of reception in Unix ns as stamped by the
	// receiving host (feeder), uncorrected for the host offset. 0 = unknown
	// (use Recv).
	HostNs int64
	// Late marks backfilled data (sent after a reconnect). Late frames feed
	// lanes, stats, flight_station and history only, never the live map,
	// the clock filter or live MLAT.
	Late bool
}

// DecodedUpdate is a pre-decoded aircraft update from feeds that do not
// deliver raw Mode S (SBS/BaseStation port 30003, aircraft.json polling).
// Nil pointer fields mean "not present in this update".
type DecodedUpdate struct {
	Recv   time.Time
	FeedID string
	// ICAO is the 24-bit address. Bit 24 (1<<24, feed.NonICAOFlag) marks a
	// non-ICAO address (readsb "~" prefix: TIS-B track files, anonymous).
	ICAO     uint32
	MLAT     bool
	Callsign *string
	Squawk   *string
	Lat, Lon *float64
	AltBaro  *float64 // ft
	AltGeom  *float64 // ft
	GS       *float64 // kt
	Track    *float64 // deg
	VertRate *float64 // ft/min
	OnGround *bool
	// Signal is dBFS for readsb/dump1090 sources. The Radarcape aircraftlist.json
	// only offers approximate dBm at the antenna connector, passed through as is.
	Signal *float64
	Msgs   int // number of underlying messages this update represents (>=1)

	// --- network fields (docs/aggregator/DESIGN.md §6.1, §9.5) ---

	// Station is the station that delivered the update (0 = local /
	// standalone; MLAT results of the own engine use 0 as well).
	Station StationIdx
	// Late marks backfilled data (see RawFrame.Late).
	Late bool
	// Src refines the origin of an MLAT position: SrcMLATAV (own engine),
	// SrcMLATExt (external mlat-server bridge) or "" (anything else).
	Src string
	// SigmaM is the 1-sigma horizontal uncertainty of Lat/Lon in metres
	// (MLAT results; nil = unknown).
	SigmaM *float64
	// Grade is the MLAT quality grade (Grade* constants; GradeNone = none).
	Grade uint8
}

// FrameSink receives data from feeds. Implemented by the tracker.
// Implementations must be safe for concurrent use and must not block for long.
type FrameSink interface {
	HandleFrame(f RawFrame)
	HandleUpdate(u DecodedUpdate)
}

// Clock supplies the best-known correction between the host system clock and
// true UTC (from the configured NTP server). true UTC ≈ time.Now().Add(Offset()).
type Clock interface {
	Offset() time.Duration // 0 when unknown
	Synced() bool          // true when a recent NTP sample exists
}

// F returns a pointer to v (helper for optional DTO fields).
func F(v float64) *float64 { return &v }

// I returns a pointer to v.
func I(v int) *int { return &v }

// S returns a pointer to v.
func S(v string) *string { return &v }

// B returns a pointer to v.
func B(v bool) *bool { return &v }
