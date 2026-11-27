package model

// Position / data source labels used in Aircraft.Source and stats.
const (
	SrcADSB  = "adsb"  // DF17 extended squitter position
	SrcMLAT  = "mlat"  // multilateration result
	SrcTISB  = "tisb"  // TIS-B (DF18)
	SrcADSR  = "adsr"  // ADS-R rebroadcast (DF18)
	SrcModeS = "modes" // Mode S only, no position
	SrcOther = "other" // SBS/JSON feed without further detail
)

// Aircraft is the live state DTO for one tracked aircraft
// (GET /api/aircraft, SSE "aircraft" event). Nil / omitted = unknown.
type Aircraft struct {
	ICAO     string `json:"icao"`               // 6 upper-case hex digits
	AddrType string `json:"addrType,omitempty"` // "icao" | "non-icao" | "tisb-track" | "anon" | "modeac" (Mode A/C MLAT target, ICAO "#xxxxxx")
	Callsign string `json:"callsign,omitempty"`
	Squawk   string `json:"squawk,omitempty"`   // 4 octal digits
	Category string `json:"category,omitempty"` // ADS-B emitter category e.g. "A3"

	// Reference data (from the optional aircraft database / ICAO allocation tables).
	Registration string `json:"reg,omitempty"`
	TypeCode     string `json:"type,omitempty"`     // ICAO type designator e.g. "A20N"
	TypeDesc     string `json:"typeDesc,omitempty"` // e.g. "Airbus A320neo"
	Operator     string `json:"operator,omitempty"`
	Country      string `json:"country,omitempty"`     // state of registration by ICAO address block
	CountryCode  string `json:"countryCode,omitempty"` // ISO 3166-1 alpha-2
	Military     bool   `json:"military,omitempty"`

	// Position.
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	Source   string   `json:"source"`           // Src* constant: best current position/data source
	PosAge   *float64 `json:"posAge,omitempty"` // seconds since last position
	OnGround *bool    `json:"onGround,omitempty"`

	// Vertical.
	AltBaro  *float64 `json:"altBaro,omitempty"`  // ft
	AltGeom  *float64 `json:"altGeom,omitempty"`  // ft (GNSS)
	VertRate *float64 `json:"vertRate,omitempty"` // ft/min
	SelAlt   *float64 `json:"selAlt,omitempty"`   // selected altitude MCP/FCU or FMS, ft
	QNH      *float64 `json:"qnh,omitempty"`      // hPa

	// Horizontal.
	GS         *float64 `json:"gs,omitempty"`         // ground speed kt
	Track      *float64 `json:"track,omitempty"`      // true track deg
	IAS        *float64 `json:"ias,omitempty"`        // kt
	TAS        *float64 `json:"tas,omitempty"`        // kt
	Mach       *float64 `json:"mach,omitempty"`       //
	MagHeading *float64 `json:"magHeading,omitempty"` // deg magnetic
	Roll       *float64 `json:"roll,omitempty"`       // deg, + = right wing down
	TrackRate  *float64 `json:"trackRate,omitempty"`  // deg/s
	SelHeading *float64 `json:"selHeading,omitempty"` // deg

	// Derived meteorology (Mode S EHS / MRAR).
	WindDir *float64 `json:"windDir,omitempty"` // deg true, direction wind blows FROM
	WindSpd *float64 `json:"windSpd,omitempty"` // kt
	OAT     *float64 `json:"oat,omitempty"`     // static air temperature °C

	// Integrity / status.
	ADSBVersion *int     `json:"adsbVersion,omitempty"`
	NIC         *int     `json:"nic,omitempty"`
	NACp        *int     `json:"nacp,omitempty"`
	NACv        *int     `json:"nacv,omitempty"`
	SIL         *int     `json:"sil,omitempty"`
	RcM         *float64 `json:"rcM,omitempty"`       // containment radius metres
	Emergency   string   `json:"emergency,omitempty"` // "", "general", "medical", "minfuel", "nordo", "unlawful", "downed"
	Alert       bool     `json:"alert,omitempty"`
	SPI         bool     `json:"spi,omitempty"`
	NavModes    []string `json:"navModes,omitempty"` // "autopilot","vnav","althold","approach","lnav","tcas"

	// Geometry relative to the receiver (only when receiver is located and aircraft has a position).
	DistKm    *float64 `json:"distKm,omitempty"`    // great-circle ground distance
	Bearing   *float64 `json:"bearing,omitempty"`   // deg true from receiver to aircraft
	Elevation *float64 `json:"elevation,omitempty"` // deg above the receiver's horizon (4/3 earth)
	SlantKm   *float64 `json:"slantKm,omitempty"`

	// Reception.
	RSSI     *float64 `json:"rssi,omitempty"`     // last frame dBFS
	RSSIAvg  *float64 `json:"rssiAvg,omitempty"`  // smoothed dBFS
	Msgs     uint64   `json:"msgs"`               // frames received this session
	MsgRate  float64  `json:"msgRate"`            // frames/s (smoothed)
	Antennas []string `json:"antennas,omitempty"` // antenna IDs that heard it recently
	Feeds    []string `json:"feeds,omitempty"`    // feed IDs that heard it recently

	FirstSeen int64 `json:"firstSeen"` // Unix ms
	LastSeen  int64 `json:"lastSeen"`  // Unix ms
	FlightID  int64 `json:"flightId,omitempty"`

	// Prediction (internal/trajectory). When Predicted is true, Lat/Lon (and
	// DistKm/Bearing/Elevation/SlantKm) are the position the tracking filter
	// predicts for now, because no real position arrived for more than
	// PredictAfterSec seconds; the aircraft is shown like this for at most
	// config.prediction.coastSec seconds after its last real position.
	// Predicted positions are never stored, never enter trails, statistics,
	// coverage or records.
	Predicted  bool     `json:"predicted,omitempty"`
	CoastS     *float64 `json:"coastS,omitempty"`     // seconds since the last real position (only while predicted)
	PredSigmaM *float64 `json:"predSigmaM,omitempty"` // 1-sigma horizontal uncertainty of the shown position, m (only while predicted)
	// PredLearned is the weight (0..1) with which turns learned from the
	// surrounding traffic moved the predicted position (absent = not used).
	PredLearned *float64 `json:"predLearned,omitempty"`
	LastRealLat *float64 `json:"lastRealLat,omitempty"` // last real (received) position, only while predicted
	LastRealLon *float64 `json:"lastRealLon,omitempty"` //
	CPAKm       *float64 `json:"cpaKm,omitempty"`       // predicted closest approach to the receiver, km (within 30 min)
	CPAInS      *float64 `json:"cpaInS,omitempty"`      // seconds until that closest approach (0 = now / moving away)

	// Terrain line of sight from the receiver (internal/terrain), refreshed at 1 Hz.
	TerrainVisible *bool    `json:"terrainVisible,omitempty"` // above the terrain radio horizon of the best antenna; omitted until the terrain analysis exists
	LOSMarginDeg   *float64 `json:"losMarginDeg,omitempty"`   // elevation above (+) or below (-) the terrain skyline, deg

	// Route from the offline standing data (or a cached online lookup).
	Route *RouteShort `json:"route,omitempty"`
	// ACAS is set while the aircraft reports an active ACAS (TCAS)
	// resolution advisory (internal/acas).
	ACAS *ACASMark `json:"acas,omitempty"`
	// Highlight is set while an alert rule with a highlight colour matches
	// this aircraft (the JSON name "alert" is the Mode S alert flag above).
	Highlight *AlertMark `json:"highlight,omitempty"`

	// --- network (aggregator mode) fields; omitted in standalone mode ---

	// MLATGrade is the grade "A".."C" of the current own-MLAT position.
	MLATGrade string `json:"mlatGrade,omitempty"`
	// MLATSigmaM is the 1-sigma horizontal uncertainty of an MLAT position, m.
	MLATSigmaM *float64 `json:"mlatSigmaM,omitempty"`
	// Mine is set for an owner when one of the owner's stations heard the
	// aircraft in the last 60 s (owner/admin scope only).
	Mine bool `json:"mine,omitempty"`
	// Stations lists the SIDs of stations that heard the aircraft recently:
	// only the viewer's own stations for an owner, all for an admin, never
	// in public scope.
	Stations []string `json:"stations,omitempty"`
}

// PredictAfterSec is the age of the last real position from which the live
// picture shows a predicted position instead (ADS-B positions are sent twice
// per second, so gaps up to a few seconds are normal reception jitter).
const PredictAfterSec = 4

// RouteShort is the compact route of a flight in the live picture. The
// airport fields describe the ACTIVE leg of a multi-stop route.
type RouteShort struct {
	From      string `json:"from"`               // departure airport code of the active leg (ICAO, else IATA / ident)
	To        string `json:"to"`                 // destination airport code of the active leg
	FromIATA  string `json:"fromIata,omitempty"` //
	ToIATA    string `json:"toIata,omitempty"`   //
	FromName  string `json:"fromName,omitempty"` // short airport name / city
	ToName    string `json:"toName,omitempty"`   //
	Via       int    `json:"via,omitempty"`      // number of intermediate stops of the whole route
	Plausible bool   `json:"plausible"`          // the current position fits the route (state fits or uncertain)
	Source    string `json:"source"`             // "override" | "learned" | "standing-data" | "adsbdb"
	Checked   bool   `json:"checked,omitempty"`  // plausibility could be checked (position and both airports known)

	State    string   `json:"state,omitempty"`    // fits | uncertain | doubtful | reversed | unchecked
	Conf     float64  `json:"conf,omitempty"`     // fused confidence 0..1 (2 dp)
	Reason   string   `json:"reason,omitempty"`   // one line, no station identifiers
	Stops    []string `json:"stops,omitempty"`    // full ordered route codes when more than 2 airports
	Leg      int      `json:"leg,omitempty"`      // 1-based active leg (multi-stop routes)
	Legs     int      `json:"legs,omitempty"`     // number of legs (multi-stop routes)
	AlongPct float64  `json:"along,omitempty"`    // progress along the active leg, 0..100
	DistRem  float64  `json:"distRem,omitempty"`  // remaining distance on the active leg, km
	ETA      int64    `json:"eta,omitempty"`      // estimated arrival at the active leg's end, Unix s (rounded to 30 s)
	ETASigma int      `json:"etaSigma,omitempty"` // 1-sigma ETA uncertainty, s
	Kind     string   `json:"kind,omitempty"`     // "" | "positioning" | "charter"
	Class    string   `json:"lic,omitempty"`      // licence class: open | own | restricted (restricted never leaves the station's own scopes)
	Flags    []string `json:"flags,omitempty"`    // "variant", "stale-standing"
}

// ACASMark is an active ACAS resolution advisory reported by the aircraft.
type ACASMark struct {
	Advisory string `json:"advisory"`        // e.g. "Climb", "Do not descend"
	Sense    string `json:"sense,omitempty"` // "up" | "down"
	Since    int64  `json:"since"`           // Unix ms of the first report
}

// AlertMark marks an aircraft that matches an alert rule with a highlight.
type AlertMark struct {
	RuleID   string `json:"ruleId"`
	Color    string `json:"color"`    // CSS colour of the highlight
	Label    string `json:"label"`    // rule name
	Severity string `json:"severity"` // "info" | "warning" | "critical"
}

// TrailPoint is one stored/trail position. Serialised as a compact JSON array
// [tMs, lat, lon, altFt, gsKt, rssi, srcCode] by the server (see docs/API.md).
type TrailPoint struct {
	T    int64   // Unix ms
	Lat  float64 //
	Lon  float64 //
	Alt  float64 // ft (baro preferred, geometric fallback); NaN unknown
	GS   float64 // kt; NaN unknown
	RSSI float64 // dBFS; NaN unknown
	Src  uint8   // 0 adsb, 1 mlat, 2 tisb/adsr, 3 other
}

// LiveCounts summarises the live picture.
type LiveCounts struct {
	Aircraft   int     `json:"aircraft"`
	WithPos    int     `json:"withPos"`
	ADSB       int     `json:"adsb"`
	MLAT       int     `json:"mlat"`
	TISB       int     `json:"tisb"`
	ModeS      int     `json:"modes"`
	MsgRate    float64 `json:"msgRate"`    // frames/s all feeds
	PosRate    float64 `json:"posRate"`    // decoded positions/s
	MaxRangeKm float64 `json:"maxRangeKm"` // farthest live aircraft (real positions only)
	Predicted  int     `json:"predicted"`  // aircraft shown at a predicted position (not counted in withPos)
}

// LiveSnapshot is the payload of GET /api/aircraft and the SSE "aircraft" event.
type LiveSnapshot struct {
	Now      int64      `json:"now"` // Unix ms
	Aircraft []Aircraft `json:"aircraft"`
	Counts   LiveCounts `json:"counts"`
}

// FeedStatus is the runtime state of one feed.
type FeedStatus struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Addr      string  `json:"addr"`
	Enabled   bool    `json:"enabled"`
	Connected bool    `json:"connected"`
	Since     int64   `json:"since"` // Unix ms of last state change
	Frames    uint64  `json:"frames"`
	Bytes     uint64  `json:"bytes"`
	MsgRate   float64 `json:"msgRate"`
	LastFrame int64   `json:"lastFrame"` // Unix ms
	LastError string  `json:"lastError,omitempty"`
	// GPS timestamp mode detected (Radarcape) and last receiver status text, if any.
	GPSTime bool   `json:"gpsTime"`
	Info    string `json:"info,omitempty"`
}

// NTPStatus is the latest NTP measurement summary.
type NTPStatus struct {
	Enabled   bool    `json:"enabled"`
	Server    string  `json:"server"`
	Synced    bool    `json:"synced"`
	OffsetMs  float64 `json:"offsetMs"` // true UTC - system clock
	DelayMs   float64 `json:"delayMs"`  // round-trip
	JitterMs  float64 `json:"jitterMs"`
	Stratum   int     `json:"stratum"`
	RefID     string  `json:"refId,omitempty"`
	LastOK    int64   `json:"lastOk"` // Unix ms
	LastError string  `json:"lastError,omitempty"`
}

// HubStatus is the runtime state of the data port.
type HubStatus struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
	Clients int  `json:"clients"`
}

// Status is the payload of GET /api/status and the SSE "status" event.
type Status struct {
	App       string       `json:"app"`
	Version   string       `json:"version"`
	Platform  string       `json:"platform"` // GOOS/GOARCH
	GUI       bool         `json:"gui"`      // running inside the desktop window
	Now       int64        `json:"now"`
	Started   int64        `json:"started"`
	Mode      string       `json:"mode"`
	HTTPPort  int          `json:"httpPort"` // actual bound port
	Located   bool         `json:"located"`
	Demo      bool         `json:"demo"`
	TileCache bool         `json:"tileCache"`
	Feeds     []FeedStatus `json:"feeds"`
	Counts    LiveCounts   `json:"counts"`
	DBBytes   int64        `json:"dbBytes"`
	DataDir   string       `json:"dataDir"`
	NTP       NTPStatus    `json:"ntp"`
	Hub       HubStatus    `json:"hub"`
	// AdminLocked is true when this client must authenticate before changing settings.
	AdminLocked bool `json:"adminLocked"`
	// ReceiverGPS is the receiver's own GPS position when a feed reports one
	// (Radarcape Beast type '5' frames). The UI offers it as "use receiver GPS position".
	ReceiverGPS *ReceiverGPS `json:"receiverGps,omitempty"`
	// Station is the effective station position every analysis uses: the
	// located receiver (source "config"; in demo mode the simulator site),
	// else the receiver's own GPS position ("gps"), else none ("none",
	// lat / lon null).
	Station StationInfo `json:"station"`
	// TileCacheHosts lists the upstream map hosts the /tilecache/ proxy accepts.
	TileCacheHosts []string `json:"tileCacheHosts,omitempty"`
}

// StationInfo is the effective station position reported in Status.
type StationInfo struct {
	Lat    *float64 `json:"lat"`
	Lon    *float64 `json:"lon"`
	Source string   `json:"source"` // "config" | "gps" | "none"
}

// ReceiverGPS is a receiver-reported station position.
type ReceiverGPS struct {
	FeedID string  `json:"feedId"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	AltM   float64 `json:"altM"` // metres (as reported by the receiver)
	T      int64   `json:"t"`    // Unix ms of the report
}
