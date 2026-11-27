// Package publicdto holds the data-transfer types of the public surfaces of
// aggregator mode (/api/public/*, the public live stream, /data/*.json;
// docs/aggregator/DESIGN.md §8.4).
//
// The types deliberately have NO fields for station coordinates, distance,
// bearing, elevation, receiver lists, signal levels, IP addresses, UUIDs,
// feeder IDs or raw timestamps. A public handler builds its answer only
// from these types, so it cannot serialise such data by accident: a field
// that does not exist cannot leak. The From* functions copy the allowed
// fields from the model types (a whitelist, never a blacklist).
//
// Changing a type here is a privacy decision: the golden tests in
// internal/server crawl every public route over fixtures and fail on any
// leaked coordinate, IP, hidden SID, ns timestamp or forbidden key
// (privacy.ForbiddenPublicKeys).
package publicdto

import (
	"math"
	"strconv"

	"airview/internal/model"
)

// Aircraft is one aircraft of the public merged picture.
type Aircraft struct {
	ICAO     string `json:"icao"`
	AddrType string `json:"addrType,omitempty"`
	Callsign string `json:"callsign,omitempty"`
	Squawk   string `json:"squawk,omitempty"`
	Category string `json:"category,omitempty"`

	Registration string `json:"reg,omitempty"`
	TypeCode     string `json:"type,omitempty"`
	TypeDesc     string `json:"typeDesc,omitempty"`
	Operator     string `json:"operator,omitempty"`
	Country      string `json:"country,omitempty"`
	CountryCode  string `json:"countryCode,omitempty"`
	Military     bool   `json:"military,omitempty"`

	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	Source   string   `json:"source"`
	PosAge   *float64 `json:"posAge,omitempty"`
	OnGround *bool    `json:"onGround,omitempty"`
	// Coarse is set when the position was rounded by the island guard
	// (DESIGN §8.6).
	Coarse bool `json:"coarse,omitempty"`

	AltBaro  *float64 `json:"altBaro,omitempty"`
	AltGeom  *float64 `json:"altGeom,omitempty"`
	VertRate *float64 `json:"vertRate,omitempty"`
	SelAlt   *float64 `json:"selAlt,omitempty"`
	QNH      *float64 `json:"qnh,omitempty"`

	GS         *float64 `json:"gs,omitempty"`
	Track      *float64 `json:"track,omitempty"`
	IAS        *float64 `json:"ias,omitempty"`
	TAS        *float64 `json:"tas,omitempty"`
	Mach       *float64 `json:"mach,omitempty"`
	MagHeading *float64 `json:"magHeading,omitempty"`
	Roll       *float64 `json:"roll,omitempty"`
	TrackRate  *float64 `json:"trackRate,omitempty"`
	SelHeading *float64 `json:"selHeading,omitempty"`

	WindDir *float64 `json:"windDir,omitempty"`
	WindSpd *float64 `json:"windSpd,omitempty"`
	OAT     *float64 `json:"oat,omitempty"`

	ADSBVersion *int     `json:"adsbVersion,omitempty"`
	NIC         *int     `json:"nic,omitempty"`
	NACp        *int     `json:"nacp,omitempty"`
	NACv        *int     `json:"nacv,omitempty"`
	SIL         *int     `json:"sil,omitempty"`
	RcM         *float64 `json:"rcM,omitempty"`
	Emergency   string   `json:"emergency,omitempty"`
	Alert       bool     `json:"alert,omitempty"`
	SPI         bool     `json:"spi,omitempty"`
	NavModes    []string `json:"navModes,omitempty"`

	FirstSeen int64 `json:"firstSeen"` // Unix ms
	LastSeen  int64 `json:"lastSeen"`  // Unix ms

	Predicted   bool     `json:"predicted,omitempty"`
	CoastS      *float64 `json:"coastS,omitempty"`
	PredSigmaM  *float64 `json:"predSigmaM,omitempty"`
	LastRealLat *float64 `json:"lastRealLat,omitempty"`
	LastRealLon *float64 `json:"lastRealLon,omitempty"`

	Route *model.RouteShort `json:"route,omitempty"`

	MLATGrade  string   `json:"mlatGrade,omitempty"`
	MLATSigmaM *float64 `json:"mlatSigmaM,omitempty"`
}

// FromAircraft copies the public fields of a.
func FromAircraft(a *model.Aircraft) Aircraft {
	return Aircraft{
		ICAO: a.ICAO, AddrType: a.AddrType, Callsign: a.Callsign, Squawk: a.Squawk, Category: a.Category,
		Registration: a.Registration, TypeCode: a.TypeCode, TypeDesc: a.TypeDesc, Operator: a.Operator,
		Country: a.Country, CountryCode: a.CountryCode, Military: a.Military,
		Lat: a.Lat, Lon: a.Lon, Source: a.Source, PosAge: a.PosAge, OnGround: a.OnGround,
		AltBaro: a.AltBaro, AltGeom: a.AltGeom, VertRate: a.VertRate, SelAlt: a.SelAlt, QNH: a.QNH,
		GS: a.GS, Track: a.Track, IAS: a.IAS, TAS: a.TAS, Mach: a.Mach, MagHeading: a.MagHeading,
		Roll: a.Roll, TrackRate: a.TrackRate, SelHeading: a.SelHeading,
		WindDir: a.WindDir, WindSpd: a.WindSpd, OAT: a.OAT,
		ADSBVersion: a.ADSBVersion, NIC: a.NIC, NACp: a.NACp, NACv: a.NACv, SIL: a.SIL, RcM: a.RcM,
		Emergency: a.Emergency, Alert: a.Alert, SPI: a.SPI, NavModes: a.NavModes,
		FirstSeen: a.FirstSeen, LastSeen: a.LastSeen,
		Predicted: a.Predicted, CoastS: a.CoastS, PredSigmaM: a.PredSigmaM,
		LastRealLat: a.LastRealLat, LastRealLon: a.LastRealLon,
		Route: a.Route, MLATGrade: a.MLATGrade, MLATSigmaM: a.MLATSigmaM,
	}
}

// Coarsen rounds the aircraft's positions to grid degrees and marks it
// Coarse (island guard).
func (a *Aircraft) Coarsen(grid float64) {
	r := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := math.Round(math.Round(*p/grid)*grid*1e4) / 1e4
		return &v
	}
	a.Lat, a.Lon = r(a.Lat), r(a.Lon)
	a.LastRealLat, a.LastRealLon = r(a.LastRealLat), r(a.LastRealLon)
	a.Coarse = true
}

// Counts summarises the public picture. It has no maximum range (a
// receiver product).
type Counts struct {
	Aircraft  int     `json:"aircraft"`
	WithPos   int     `json:"withPos"`
	ADSB      int     `json:"adsb"`
	MLAT      int     `json:"mlat"`
	TISB      int     `json:"tisb"`
	ModeS     int     `json:"modes"`
	MsgRate   float64 `json:"msgRate"` // merged network frames/s
	PosRate   float64 `json:"posRate"`
	Predicted int     `json:"predicted"`
}

// FromCounts copies the public counters of c.
func FromCounts(c model.LiveCounts) Counts {
	return Counts{Aircraft: c.Aircraft, WithPos: c.WithPos, ADSB: c.ADSB, MLAT: c.MLAT, TISB: c.TISB,
		ModeS: c.ModeS, MsgRate: c.MsgRate, PosRate: c.PosRate, Predicted: c.Predicted}
}

// Snapshot is the public live picture (GET /api/public/aircraft, the
// public "aircraft" stream event, and GET /api/aircraft for public viewers
// in aggregator mode).
type Snapshot struct {
	Now      int64      `json:"now"` // Unix ms
	Aircraft []Aircraft `json:"aircraft"`
	Counts   Counts     `json:"counts"`
}

// Guard decides per aircraft whether the island guard coarsens it. nil =
// guard off.
type Guard func(a *model.Aircraft) bool

// FromSnapshot builds the public picture from a (possibly unprojected)
// snapshot. guard may be nil.
func FromSnapshot(s *model.LiveSnapshot, guard Guard, grid float64) Snapshot {
	out := Snapshot{Now: s.Now, Counts: FromCounts(s.Counts), Aircraft: make([]Aircraft, 0, len(s.Aircraft))}
	for i := range s.Aircraft {
		a := &s.Aircraft[i]
		p := FromAircraft(a)
		if guard != nil && guard(a) {
			p.Coarsen(grid)
		}
		out.Aircraft = append(out.Aircraft, p)
	}
	return out
}

// TrailPoint is one public trail point. It encodes as the compact array
// [tMs, lat, lon, altFt|null, gsKt|null, null, src] - the layout of the
// standalone trails with the signal column always null, so existing
// clients parse it unchanged.
type TrailPoint struct {
	T        int64
	Lat, Lon float64
	Alt, GS  float64 // NaN = unknown
	Src      uint8
}

// FromTrailPoint copies a trail point without its signal level.
func FromTrailPoint(p model.TrailPoint) TrailPoint {
	return TrailPoint{T: p.T, Lat: p.Lat, Lon: p.Lon, Alt: p.Alt, GS: p.GS, Src: p.Src}
}

// MarshalJSON encodes the compact array form.
func (p TrailPoint) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 64)
	b = append(b, '[')
	b = appendInt(b, p.T)
	b = append(b, ',')
	b = appendFloat(b, p.Lat, 5)
	b = append(b, ',')
	b = appendFloat(b, p.Lon, 5)
	b = append(b, ',')
	b = appendFloat(b, p.Alt, 0)
	b = append(b, ',')
	b = appendFloat(b, p.GS, 0)
	b = append(b, ",null,"...)
	b = appendInt(b, int64(p.Src))
	b = append(b, ']')
	return b, nil
}

// Trails is the public trails reply.
type Trails struct {
	Now     int64                   `json:"now"`
	Minutes int                     `json:"minutes"`
	Trails  map[string][]TrailPoint `json:"trails"`
}

// Status is the public application status (GET /api/public/status and GET
// /api/status for public viewers in aggregator mode). It carries no feed,
// receiver, data-port, NTP or path information.
type Status struct {
	App       string `json:"app"`
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	Network   string `json:"network,omitempty"` // aggregator display name
	Now       int64  `json:"now"`
	Started   int64  `json:"started"`
	TileCache bool   `json:"tileCache"`
	Counts    Counts `json:"counts"`
	// AdminLocked is always true for a public viewer.
	AdminLocked bool `json:"adminLocked"`
	// Located is always false: the public scope has no station position.
	Located bool `json:"located"`
	// PublicMap / PublicStats / TarJSON tell the UI which public surfaces
	// exist.
	PublicMap   bool `json:"publicMap"`
	PublicStats bool `json:"publicStats"`
	TarJSON     bool `json:"tarJson"`
	// TileCacheHosts lists the map hosts of the tile proxy.
	TileCacheHosts []string `json:"tileCacheHosts,omitempty"`
}

// Notice is a public live notice: only the kinds that describe an aircraft
// (emergency, military, interesting). Record, feed and alert notices refer
// to receivers and are never public.
type Notice struct {
	Level string `json:"level"`
	Text  string `json:"text"`
	T     int64  `json:"t"`
	Kind  string `json:"kind"`
	ICAO  string `json:"icao,omitempty"`
}

// PublicNoticeKind reports whether notices of kind may be shown publicly.
func PublicNoticeKind(kind string) bool {
	switch kind {
	case "emergency", "military", "interesting":
		return true
	}
	return false
}

// Pin is a station's stored public pin point (DESIGN §8.3): never the exact
// position, always the keyed point inside its cell.
type Pin struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Precision string  `json:"precision"` // "region" | "cell01" (after the rural guard)
}

// StationStats are the opt-in, non-locating statistics of a station.
type StationStats struct {
	// MsgRate is the recent frame rate (frames/s, 1 decimal).
	MsgRate float64 `json:"msgRate"`
	// PosRate is the recent position rate (positions/s, 1 decimal).
	PosRate float64 `json:"posRate"`
	// Aircraft heard in the current minute.
	Aircraft int `json:"aircraft"`
	// Positions decoded since the station's lane started (0 for entries
	// without SID, see Unlinkable).
	Positions uint64 `json:"positions"`
	// UptimePct is the share of the last 24 h with data (when known).
	UptimePct *float64 `json:"uptimePct,omitempty"`
	// AircraftDay is the number of aircraft heard today (when known).
	AircraftDay *int `json:"aircraftDay,omitempty"`
	// MLATContrib is the number of MLAT fixes the station contributed to
	// in the last hour (when the MLAT engine runs).
	MLATContrib *int `json:"mlatContrib,omitempty"`
}

// Unlinkable makes the statistics of a station listed without SID (hidden
// pin) hard to follow across polls (PRIV-A7): the lifetime position counter
// only grows, so it would tag the entry as surely as the omitted SID. It is
// dropped (0), and the rates are rounded to one significant figure.
func (s *StationStats) Unlinkable() {
	if s == nil {
		return
	}
	s.Positions = 0
	s.MsgRate = roundSig1(s.MsgRate)
	s.PosRate = roundSig1(s.PosRate)
	s.MLATContrib = nil
	if s.UptimePct != nil {
		v := math.Round(*s.UptimePct/10) * 10
		s.UptimePct = &v
	}
	if s.AircraftDay != nil {
		v := int(roundSig1(float64(*s.AircraftDay)))
		s.AircraftDay = &v
	}
}

// roundSig1 rounds v to one significant figure (0.5 → 0.5, 347 → 300).
func roundSig1(v float64) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	return math.Round(v/p) * p
}

// StationEntry is one entry of the public station list. Stations with a
// hidden pin are listed only when their owner opted in to public
// statistics, and then without ID and pin (their SID would link them
// across views).
type StationEntry struct {
	SID    string        `json:"sid,omitempty"`  // only for stations with a public pin
	Name   string        `json:"name,omitempty"` // only when the owner opted in
	Online bool          `json:"online"`
	Pin    *Pin          `json:"pin"` // null = no public pin
	Stats  *StationStats `json:"stats,omitempty"`
}

// StationList is GET /api/public/stations.
type StationList struct {
	Stations []StationEntry `json:"stations"`
	// Approved and Online count every approved station (not only listed).
	Approved int `json:"approved"`
	Online   int `json:"online"`
}

// Summary is GET /api/public/summary: the network landing page.
type Summary struct {
	Name           string        `json:"name,omitempty"`
	StationCounts  StationCounts `json:"stationCounts"`
	Aircraft       int           `json:"aircraft"`
	MLATAircraft   int           `json:"mlatAircraft"`
	MessagesPerSec float64       `json:"messagesPerSec"`
	// MLAT is the empirical network MLAT quality (null until the MLAT
	// engine publishes it).
	MLAT    *MLATQuality `json:"mlat"`
	Updated int64        `json:"updated"` // Unix ms
}

// StationCounts counts approved stations: all, online and with a public pin.
type StationCounts struct {
	Total  int `json:"total"`
	Online int `json:"online"`
	Public int `json:"public"`
}

// MLATQuality is the network-wide empirical MLAT accuracy (DESIGN §8.2).
type MLATQuality struct {
	// P50M/P95M/P99M: horizontal error of the hold-out tracks against the
	// ADS-B reference; it includes the reference's airframe position
	// latency, so it overstates the MLAT error.
	P50M float64 `json:"p50M"`
	P95M float64 `json:"p95M"`
	P99M float64 `json:"p99M"`
	// EstP50M/EstP95M: the horizontal error estimated from the cross-track
	// errors alone (free of that latency): the headline figure (0 = not
	// available).
	EstP50M     float64 `json:"estP50M"`
	EstP95M     float64 `json:"estP95M"`
	FixesPerMin float64 `json:"fixesPerMin"`
}

// MLATCoverage is GET /api/public/mlat: the network's empirical MLAT
// accuracy over the last hour and its 1° cells (DESIGN §8.2). A cell is
// listed only with at least 20 hold-out samples from at least 5 different
// contributing stations; no station identity or count is published.
type MLATCoverage struct {
	// Quality is null while the last hour has fewer than 20 hold-out
	// samples.
	Quality *MLATQuality `json:"quality"`
	Samples uint64       `json:"samples"`
	Cells   []MLATCell   `json:"cells"`
	Updated int64        `json:"updated"` // Unix ms
}

// MLATCell is one 1° cell of MLATCoverage (south-west corner; Band is the
// altitude band of the engine's quality map).
type MLATCell struct {
	Lat  int     `json:"lat"`
	Lon  int     `json:"lon"`
	Band uint8   `json:"band"`
	N    uint64  `json:"n"`
	P50M float64 `json:"p50M"`
	P95M float64 `json:"p95M"`
}

// NetworkStats is GET /api/public/stats: the merged network in numbers.
type NetworkStats struct {
	Now      int64  `json:"now"`
	Name     string `json:"name,omitempty"`
	Approved int    `json:"approved"` // approved stations
	Online   int    `json:"online"`   // approved stations with data in the last 2 minutes
	Counts   Counts `json:"counts"`
}

func appendInt(b []byte, v int64) []byte { return strconv.AppendInt(b, v, 10) }

func appendFloat(b []byte, v float64, decimals int) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, v, 'f', decimals, 64)
}
