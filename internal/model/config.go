package model

// ConfigVersion is bumped when the persisted config layout changes.
const ConfigVersion = 1

// Run modes.
const (
	ModeStandalone = "standalone"  // local feeds -> local engine -> local UI
	ModeRemoteData = "remote-data" // local engine fed from an AirView host's data port (hub)
	ModeRemoteWeb  = "remote-web"  // window simply shows the AirView host's hosted website
	// ModeAggregator runs a multi-station network server: AVF/1, Beast and
	// mlat-client ingest, the merged map and the own MLAT engine
	// (docs/aggregator/DESIGN.md §1; settings in Config.Aggregator).
	ModeAggregator = "aggregator"
	// ModeFeeder is used only by the headless airview-feeder program
	// (FeederConfig.Mode); the main application never runs in it.
	ModeFeeder = "feeder"
)

// Config is the complete persisted configuration (config.json in the data dir).
// GET/PUT /api/config exchange exactly this shape.
type Config struct {
	Version  int           `json:"version"`
	Mode     string        `json:"mode"`
	HTTP     HTTPConfig    `json:"http"`
	Receiver Receiver      `json:"receiver"`
	Antennas []Antenna     `json:"antennas"`
	Feeds    []Feed        `json:"feeds"`
	NTP      NTPConfig     `json:"ntp"`
	Weather  WeatherConfig `json:"weather"`
	Hub      HubConfig     `json:"hub"`
	Remote   RemoteConfig  `json:"remote"`
	Storage  StorageConfig `json:"storage"`
	UI       UIConfig      `json:"ui"`
	Demo     DemoConfig    `json:"demo"`

	Prediction PredictionConfig `json:"prediction"`
	Terrain    TerrainConfig    `json:"terrain"`
	NavData    NavDataConfig    `json:"navData"`
	Alerts     AlertsConfig     `json:"alerts"`
	RFStat     RFStatConfig     `json:"rfStat"`

	// Uplinks feed this station to AirView aggregators over AVF/1
	// (standalone and aggregator modes; omitted from config.json when empty).
	Uplinks []Uplink `json:"uplinks,omitempty"`
	// UplinkTuning holds the global tunables of the uplink client (ring,
	// backfill, backoff, spool segments); they apply to every uplink.
	UplinkTuning UplinkTuning `json:"uplinkTuning"`
	// Aggregator holds the aggregator-mode settings (used only when Mode is
	// ModeAggregator). Stand-alone configs carry the defaults; a document
	// without this key decodes to the defaults when decoded over
	// DefaultConfig(), and a zero block is restored by Aggregator.Normalize.
	Aggregator Aggregator `json:"aggregator"`

	// Update is the signed self-update of this program (opt-in; see
	// internal/selfupdate).
	Update UpdateSettings `json:"update"`
	// Locate controls Locate Me v2: the guess samples recorded to estimate
	// the receiver position from what it hears.
	Locate LocateConfig `json:"locate"`
	// SSR is the experimental bistatic SSR self-location of this receiver
	// (off by default; owner/admin only, never applied automatically).
	SSR SSRConfig `json:"ssr,omitzero"`
	// Process holds the log file and memory settings of the server
	// process (applied at start).
	Process ProcessConfig `json:"process,omitzero"`
}

// IsFactoryDefaultFeed reports whether f is an unchanged factory default
// feed: the generic "beast" feed of DefaultConfig (localhost:30005) or the
// "radarcape" Beast feed (host "radarcape", port 10003) that earlier
// releases shipped and existing config files still carry. The engine
// ignores it in aggregator mode (SCALE-08).
func IsFactoryDefaultFeed(f Feed) bool {
	if f.Type != FeedBeast {
		return false
	}
	return (f.ID == DefaultFeedID && f.Host == DefaultFeedHost && f.Port == DefaultFeedPort) ||
		(f.ID == "radarcape" && f.Host == "radarcape" && f.Port == 10003)
}

// The factory default feed: Beast binary from a local readsb / dump1090
// (their standard Beast output port 30005).
const (
	DefaultFeedID   = "beast"
	DefaultFeedHost = "localhost"
	DefaultFeedPort = 30005
)

// ExampleRadarcapeConfig is DefaultConfig with the two-antenna Radarcape
// EVO 2CH station that earlier releases shipped as factory default: a
// vertical (channel 0) and a 9-element yagi (channel 1), and the
// "radarcape" Beast feed on radarcape:10003. Tests and examples that need a
// directional antenna or a second channel start from it; it is never used
// as a default.
func ExampleRadarcapeConfig() Config {
	c := DefaultConfig()
	c.Receiver.Model = "Radarcape EVO 2CH"
	c.Antennas = append(c.Antennas, Antenna{
		ID: "yagi", Name: "Yagi", Type: AntYagi, Channel: 1,
		GainDBi: 12, HeightAGLM: 8, AzimuthDeg: 0, HBeamwidthDeg: 40, VBeamwidthDeg: 40,
		FrontToBackDB: 18, Elements: 9, Polarization: "vertical",
		Color: "#f59e0b",
	})
	c.Feeds = []Feed{{
		ID: "radarcape", Name: "Radarcape EVO (Beast 10003)", Enabled: true,
		Type: FeedBeast, Host: "radarcape", Port: 10003,
	}}
	return c
}

// PredictionConfig controls the trajectory filter (internal/trajectory):
// coasting aircraft on the live map and predicted paths.
type PredictionConfig struct {
	Enabled bool `json:"enabled"` // run the tracking filter (default true)
	// CoastSec is how long an aircraft without new positions is shown at its
	// predicted position (default 60, 5..120). Predicted positions start
	// PredictAfterSec (4 s) after the last real position.
	CoastSec int `json:"coastSec"`
	// PathSec is the default horizon of predicted paths (default 120, 10..900).
	PathSec int `json:"pathSec"`
	// ShowPaths: the map draws the predicted path of the selected aircraft (default true).
	ShowPaths bool `json:"showPaths"`
}

// TerrainConfig controls the terrain line-of-sight analysis (internal/terrain).
type TerrainConfig struct {
	Enabled bool `json:"enabled"` // default true
	// RadiusKm of the analysis around the receiver (default 450, 50..1000).
	RadiusKm int `json:"radiusKm"`
	// Zoom of the elevation tiles: 0 = automatic (default), else 6..12.
	Zoom int `json:"zoom"`
	// KFactor is the effective earth radius factor for radio line of sight (default 4/3, 0.5..10).
	KFactor float64 `json:"kFactor"`
	// AutoPrefetch downloads the elevation tiles (AWS Terrain Tiles, about
	// 30-40 MB for 450 km) and runs the analysis automatically once the
	// receiver is located (default true). Off = only on request.
	AutoPrefetch bool `json:"autoPrefetch"`
}

// NavDataConfig controls aviation reference data (internal/navdata).
// Nothing is downloaded automatically: imports run only on request.
type NavDataConfig struct {
	AirportsOverlay bool `json:"airportsOverlay"` // airports layer offered on the map (default true)
	// AutoRefresh downloads the reference data (airports, runways, navaids,
	// countries, VRS standing-data routes; CC0 / public domain, about 25 MB,
	// no user data sent) at first start and checks it daily at a per-install
	// random time with a conditional GET (default true; off for air-gapped
	// sites).
	AutoRefresh bool `json:"autoRefresh"`
	// Photos shows aircraft photos from planespotters.net (sends the ICAO
	// address; default false). Requires PhotoContact.
	Photos       bool         `json:"photos"`
	PhotoContact string       `json:"photoContact"` // contact URL or e-mail sent in the User-Agent (required by planespotters.net)
	Routes       RoutesConfig `json:"routes"`       // route look-ups, learning and overrides
}

// RoutesConfig controls route resolution (internal/routes).
type RoutesConfig struct {
	Enabled bool `json:"enabled"` // master switch of the route feature (default true)
	// Online looks callsigns up at adsbdb.com when no offline route (override,
	// learned, standing data) exists (default true). It sends the callsign,
	// never the receiver location; answers are only cached, never merged into
	// AirView's own tables, and shown only to the station's own users.
	Online    bool   `json:"online"`
	DailyCap  int    `json:"dailyCap"`  // online look-ups per 24 h (default 1000)
	StableSec int    `json:"stableSec"` // a callsign must be received this long before it is looked up online (default 45)
	Contact   string `json:"contact"`   // contact URL or e-mail in the User-Agent of online look-ups (default: the project URL)
	Learn     bool   `json:"learn"`     // learn routes from observed take-offs and landings (default true)
	ETA       bool   `json:"eta"`       // estimate the arrival time (default true)
}

// ProjectURL identifies AirView in the User-Agent of online look-ups
// (never a personal address).
const ProjectURL = "https://github.com/G5LKW/AirView"

// AlertsConfig controls the alert rules engine (internal/alerts).
type AlertsConfig struct {
	Enabled bool `json:"enabled"` // evaluate rules (default true)
	Sound   bool `json:"sound"`   // alerts may play a sound in the UI (default true)
	// Webhooks allows rules to POST to their webhook URL (default true; off = no webhook is sent).
	Webhooks bool `json:"webhooks"`
}

// RFStatConfig controls the RF / protocol statistics (internal/rfstat).
type RFStatConfig struct {
	Enabled bool `json:"enabled"` // default true
}

// HTTPConfig configures the built-in web server.
type HTTPConfig struct {
	Port int `json:"port"` // default 80; if busy the server falls back to 8080, 8081.. then an ephemeral port
	// BindAddr "" = all interfaces. "127.0.0.1" = local only.
	BindAddr string `json:"bindAddr"`
	// AdminPasswordHash protects mutating endpoints (config, factory reset) for
	// non-loopback clients when non-empty. Format: "pbkdf2-sha256$<iter>$<salt>$<key>"
	// (the former "sha256:<hex salt>:<hex hash>" is still accepted).
	AdminPasswordHash string `json:"adminPasswordHash"`
	TileCache         bool   `json:"tileCache"` // cache map tiles on disk via /tilecache/
	// AllowInsecureLogin (aggregator mode) accepts passwords, TOTP codes
	// and the setup token from remote clients over plain HTTP. Off by
	// default: credentials are accepted only over HTTPS (a trusted reverse
	// proxy that sets X-Forwarded-Proto) or from the machine itself.
	AllowInsecureLogin bool `json:"allowInsecureLogin,omitempty"`
	// AllowOpenAdmin (standalone mode, no admin password) makes every
	// client admin, also those outside the private networks. Off by
	// default: without a password only this machine and private-network
	// (RFC 1918, IPv6 ULA, link-local) clients are admin.
	AllowOpenAdmin bool `json:"allowOpenAdmin,omitempty"`
	// SSEGzipMax caps the live streams (/api/live) that are gzip-compressed
	// at once; each holds a compressor of about 800 KiB (0..100000; 0 =
	// automatic: 12 in standalone mode, every stream in aggregator mode).
	// Streams above the cap are sent uncompressed.
	SSEGzipMax int `json:"sseGzipMax,omitempty"`
}

// Receiver describes the receiving station.
type Receiver struct {
	Name    string  `json:"name"`
	Model   string  `json:"model"`   // e.g. "Radarcape EVO 2CH"
	Located bool    `json:"located"` // false until the user (or the guesser) sets coordinates
	Lat     float64 `json:"lat"`     // WGS84 degrees
	Lon     float64 `json:"lon"`
	// GroundElevM is the terrain elevation at the site, metres above mean sea level.
	GroundElevM float64 `json:"groundElevM"`
}

// Antenna types.
const (
	AntVertical  = "vertical"  // simple 1/4 or 1/2 wave omni
	AntCollinear = "collinear" // stacked omni with compressed vertical beam
	AntYagi      = "yagi"      // directional
	AntPatch     = "patch"     // directional panel
	AntOther     = "other"
)

// Antenna describes one physical antenna and its RF chain.
type Antenna struct {
	ID         string  `json:"id"` // stable slug, unique
	Name       string  `json:"name"`
	Type       string  `json:"type"`       // Ant* constants
	Channel    int     `json:"channel"`    // receiver RF channel index this antenna is plugged into (0-based), -1 = n/a
	GainDBi    float64 `json:"gainDbi"`    // peak gain
	HeightAGLM float64 `json:"heightAglM"` // feed-point height above ground, metres
	// Directional parameters (yagi/patch). Azimuth is the boresight in degrees true.
	AzimuthDeg    float64 `json:"azimuthDeg"`
	TiltDeg       float64 `json:"tiltDeg"`       // boresight elevation above horizon
	HBeamwidthDeg float64 `json:"hBeamwidthDeg"` // -3 dB horizontal beamwidth
	VBeamwidthDeg float64 `json:"vBeamwidthDeg"` // -3 dB vertical beamwidth
	FrontToBackDB float64 `json:"frontToBackDb"` //
	Elements      int     `json:"elements"`      // yagi element count (informational / used to estimate beamwidth when 0)
	Polarization  string  `json:"polarization"`  // "vertical" (ADS-B) | "horizontal"
	CableLossDB   float64 `json:"cableLossDb"`   // feedline loss before the first amplifier
	LNAGainDB     float64 `json:"lnaGainDb"`     //
	LNANoiseFigDB float64 `json:"lnaNoiseFigDb"` //
	FilterLossDB  float64 `json:"filterLossDb"`  //
	Notes         string  `json:"notes"`         //
	Color         string  `json:"color"`         // UI colour for this antenna's overlays (hex)
}

// Feed types.
const (
	FeedBeast = "beast" // Beast binary over TCP (Radarcape 10003, dump1090/readsb 30005)
	FeedAVR   = "avr"   // AVR / raw hex over TCP (30002, Radarcape 10002)
	FeedSBS   = "sbs"   // BaseStation CSV over TCP (30003)
	FeedJSON  = "json"  // HTTP polling of aircraft.json (readsb/dump1090) or Radarcape aircraftlist.json
	FeedHub   = "hub"   // AirView host data port (remote-data mode)
	FeedDemo  = "demo"  // built-in simulator
	// FeedRTLTCP ("rtltcp", wave3.go) is an RTL-SDR dongle via rtl_tcp.
	// FeedAuto is used only by airview-feeder (FeederReceiver.Input): probe
	// 127.0.0.1:30005 (readsb/dump1090) and a Radarcape on :10003.
	FeedAuto = "auto"
)

// Feed is one data input.
type Feed struct {
	ID      string `json:"id"` // stable slug, unique
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Type    string `json:"type"` // Feed* constants
	Host    string `json:"host"`
	Port    int    `json:"port"`
	URL     string `json:"url"` // for FeedJSON
	// AntennaID is the antenna this feed's frames are attributed to when the
	// protocol does not carry a channel number. "" = unknown/combined.
	AntennaID string `json:"antennaId"`
	// MLATOnly marks a feed that only carries MLAT results.
	MLATOnly bool   `json:"mlatOnly"`
	Notes    string `json:"notes"`
	// NoForward keeps this feed's data off every uplink (DESIGN §1). Feeds
	// with MLATOnly are never forwarded regardless of this flag.
	NoForward bool `json:"noForward,omitempty"`
	// ClockClass overrides the detected receiver clock class for uplinks
	// (ClockClass.String() names; "" = detect).
	ClockClass string `json:"clockClass,omitempty"`
	// SDR tunes a feed of type "rtltcp" (an RTL-SDR dongle through rtl_tcp;
	// ignored for the other types).
	SDR FeedSDR `json:"sdr,omitzero"`
}

// Forwardable reports whether frames of this feed may be sent on an uplink
// (DESIGN §1: never MLAT results, never feeds marked NoForward, never the
// demo simulator and never hub feeds, whose frames belong to another
// station).
func (f Feed) Forwardable() bool {
	return !f.MLATOnly && !f.NoForward && f.Type != FeedDemo && f.Type != FeedHub
}

// NTPConfig configures the SNTP reference used for timing analysis.
type NTPConfig struct {
	Enabled     bool   `json:"enabled"`
	Server      string `json:"server"`      // host or host:port; "" = disabled
	IntervalSec int    `json:"intervalSec"` // sampling interval, default 64
}

// WeatherConfig configures free weather data ingestion (Open-Meteo, no API key).
type WeatherConfig struct {
	Enabled      bool `json:"enabled"`
	IntervalMin  int  `json:"intervalMin"`  // refresh period, default 15
	RadarOverlay bool `json:"radarOverlay"` // RainViewer precipitation radar layer on the map
}

// HubConfig configures the data port that other AirView instances log in to.
type HubConfig struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`     // default 30154
	Password string `json:"password"` // shared secret for the HMAC challenge login; "" = open
}

// RemoteConfig is used by the desktop app in remote-data / remote-web modes.
type RemoteConfig struct {
	Host            string `json:"host"`            // AirView host (e.g. the Raspberry Pi)
	DataPort        int    `json:"dataPort"`        // host's hub port, default 30154
	HTTPPort        int    `json:"httpPort"`        // host's website port, default 80
	Password        string `json:"password"`        // hub password
	UseHostReceiver bool   `json:"useHostReceiver"` // adopt the host's receiver/antenna settings
}

// StorageConfig controls database retention and how the database writes to
// disk (the storage profile, see storage.go).
type StorageConfig struct {
	TrailIntervalSec    int `json:"trailIntervalSec"`    // min seconds between stored trail points per aircraft (default 5)
	TrailRetentionDays  int `json:"trailRetentionDays"`  // stored position trails (default 30)
	FlightRetentionDays int `json:"flightRetentionDays"` // flight/session rows (default 730, 0 = forever)
	MinuteRetentionDays int `json:"minuteRetentionDays"` // per-minute stats (default 90); hourly/daily rollups are kept forever
	// Profile selects how often the database writes to disk and how much
	// memory its caches use: "auto" ("" is the same; server in aggregator
	// mode, low-wear when the data directory is on an SD/MMC card or on a
	// small ARM board, else standard), "standard", "low-wear" (fewest disk
	// writes, for SD cards) or "server". No profile changes what is stored
	// or shown; the power-cut loss windows are listed in docs/CONFIG.md.
	Profile string `json:"profile,omitempty"`
	// BackupBeforeMigrate controls the backup taken before the database
	// is converted to a new storage layout: "auto" ("" is the same; a
	// backup when the disk has room for it, else migrate without one),
	// "always" (no migration without a backup) or "never".
	BackupBeforeMigrate string `json:"backupBeforeMigrate,omitempty"`
	// Tuning overrides single values of the storage profile (advanced;
	// 0 = the profile's value).
	Tuning StorageTuning `json:"tuning,omitzero"`
}

// UIConfig holds shared UI defaults (per-browser preferences live in localStorage).
type UIConfig struct {
	Units         string `json:"units"`         // "aviation" (ft/kt/nm) | "metric" | "imperial"
	TrailMinutes  int    `json:"trailMinutes"`  // live map trail length, default 15
	RangeRingsKm  []int  `json:"rangeRingsKm"`  // e.g. [100,200,300,400]
	AircraftTTLs  int    `json:"aircraftTtlS"`  // seconds until a silent aircraft leaves the live list (default 60)
	MapStyle      string `json:"mapStyle"`      // "airview-dark" default
	ColorTrailsBy string `json:"colorTrailsBy"` // "altitude" | "speed" | "signal" | "source"
}

// DemoConfig enables the built-in traffic simulator (no receiver needed).
type DemoConfig struct {
	Enabled  bool `json:"enabled"`
	Aircraft int  `json:"aircraft"` // simulated aircraft count, default 60
}

// DefaultConfig returns factory defaults. Factory reset restores exactly this.
func DefaultConfig() Config {
	return Config{
		Version:  ConfigVersion,
		Mode:     ModeStandalone,
		HTTP:     HTTPConfig{Port: 80, BindAddr: "", TileCache: true},
		Receiver: Receiver{Name: "My Station"},
		// One omnidirectional antenna and a local readsb / dump1090 Beast
		// feed: the common station. Existing config files keep their own
		// antennas and feeds (lists are never merged with these defaults).
		Antennas: []Antenna{
			{
				ID: "vertical", Name: "Vertical", Type: AntVertical, Channel: 0,
				GainDBi: 5, HeightAGLM: 8, VBeamwidthDeg: 30, Polarization: "vertical",
				Color: "#38bdf8",
			},
		},
		Feeds: []Feed{
			{
				ID: DefaultFeedID, Name: "Local Beast (readsb/dump1090 :30005)", Enabled: true,
				Type: FeedBeast, Host: DefaultFeedHost, Port: DefaultFeedPort, AntennaID: "",
			},
		},
		NTP:     NTPConfig{Enabled: false, Server: "", IntervalSec: 64},
		Weather: WeatherConfig{Enabled: true, IntervalMin: 15, RadarOverlay: false},
		Hub:     HubConfig{Enabled: false, Port: 30154},
		Remote:  RemoteConfig{DataPort: 30154, HTTPPort: 80, UseHostReceiver: true},
		Storage: StorageConfig{
			TrailIntervalSec: 5, TrailRetentionDays: 30,
			FlightRetentionDays: 730, MinuteRetentionDays: 90,
		},
		UI: UIConfig{
			Units: "aviation", TrailMinutes: 15, RangeRingsKm: []int{100, 200, 300, 400},
			AircraftTTLs: 60, MapStyle: "airview-dark", ColorTrailsBy: "altitude",
		},
		Demo: DemoConfig{Enabled: false, Aircraft: 60},

		Prediction: PredictionConfig{Enabled: true, CoastSec: 60, PathSec: 120, ShowPaths: true},
		Terrain:    TerrainConfig{Enabled: true, RadiusKm: 450, Zoom: 0, KFactor: 4.0 / 3.0, AutoPrefetch: true},
		NavData: NavDataConfig{AirportsOverlay: true, AutoRefresh: true,
			Routes: RoutesConfig{Enabled: true, Online: true, DailyCap: 1000, StableSec: 45, Learn: true, ETA: true}},
		Alerts: AlertsConfig{Enabled: true, Sound: true, Webhooks: true},
		RFStat: RFStatConfig{Enabled: true},

		Aggregator:   DefaultAggregator(),
		UplinkTuning: DefaultUplinkTuning(),
		Update:       DefaultUpdateSettings(),
		Locate:       DefaultLocateConfig(),
	}
}
