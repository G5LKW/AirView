package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"airview/internal/config"
	"airview/internal/engine"
	"airview/internal/hub"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/predict"
	"airview/internal/refdata"
	"airview/internal/store"
	"airview/internal/tracker"
	"airview/internal/trajectory"
)

func apiStatus(s *Server, r *http.Request) (any, error) {
	return s.viewStatus(r), nil
}

func apiGetConfig(s *Server, r *http.Request) (any, error) {
	if s.netRestricted(r) {
		return s.publicConfig(), nil // aggregator mode below admin: display settings only
	}
	if !s.isAdmin(r) {
		return config.RedactPublic(s.eng.Config().Get()), nil
	}
	cfg := config.Redact(s.eng.Config().Get())
	if locked := lockedSettings(s.eng.Layers().Overrides()); len(locked) > 0 {
		return configReply{Config: cfg, Locked: locked}, nil
	}
	return cfg, nil
}

// lockedSetting is a setting fixed by the operator config file, an
// AIRVIEW_* environment variable or a -set flag (DESIGN §20.3). Only where
// it comes from is shown, never its value.
type lockedSetting struct {
	Path   string `json:"path"`   // e.g. "http.port", "uplinks[0].enrolToken"
	Origin string `json:"origin"` // e.g. "/etc/airview/airview.jsonc:14:5", "env AIRVIEW_HTTP_PORT"
}

// configReply is the admin GET /api/config reply when settings are locked.
type configReply struct {
	model.Config
	Locked []lockedSetting `json:"locked"`
}

func lockedSettings(ov []config.Override) []lockedSetting {
	if len(ov) == 0 {
		return nil
	}
	out := make([]lockedSetting, len(ov))
	for i, o := range ov {
		out[i] = lockedSetting{Path: o.Path, Origin: o.Origin}
	}
	return out
}

// apiPutConfig is PUT /api/config. Top-level sections missing from the body
// keep their stored value, so a client may send only what it edits; a
// section that is present replaces the stored one completely.
func apiPutConfig(s *Server, r *http.Request) (any, error) {
	raw, err := readBody(r, 1<<20)
	if err != nil {
		return nil, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
		return nil, badRequest("invalid configuration: a JSON object is required")
	}
	var in model.Config
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, badRequest("invalid configuration: %v", err)
	}
	cur := s.eng.Config().Get()
	has := func(k string) bool { _, ok := keys[k]; return ok }
	if !has("version") {
		in.Version = cur.Version
	}
	if !has("mode") {
		in.Mode = cur.Mode
	}
	if !has("http") {
		in.HTTP = cur.HTTP
	}
	if !has("receiver") {
		in.Receiver = cur.Receiver
	}
	if !has("antennas") {
		in.Antennas = cur.Antennas
	}
	if !has("feeds") {
		in.Feeds = cur.Feeds
	}
	if !has("ntp") {
		in.NTP = cur.NTP
	}
	if !has("weather") {
		in.Weather = cur.Weather
	}
	if !has("hub") {
		in.Hub = cur.Hub
	}
	if !has("remote") {
		in.Remote = cur.Remote
	}
	if !has("storage") {
		in.Storage = cur.Storage
	}
	if !has("ui") {
		in.UI = cur.UI
	}
	if !has("demo") {
		in.Demo = cur.Demo
	}
	if !has("prediction") {
		in.Prediction = cur.Prediction
	}
	if !has("terrain") {
		in.Terrain = cur.Terrain
	}
	if !has("navData") {
		in.NavData = cur.NavData
	}
	if !has("alerts") {
		in.Alerts = cur.Alerts
	}
	if !has("rfStat") {
		in.RFStat = cur.RFStat
	}
	if !has("uplinks") {
		in.Uplinks = cur.Uplinks
	}
	if !has("aggregator") {
		in.Aggregator = cur.Aggregator
	}
	if !has("uplinkTuning") {
		in.UplinkTuning = cur.UplinkTuning
	}
	if !has("update") {
		in.Update = cur.Update
	}
	// SEC-R4: the release manifest URL and the release keys decide which
	// binaries "airview update" installs (as root on a server). They are
	// trust anchors: set in the operator configuration file, an
	// AIRVIEW_UPDATE_* variable or the stored configuration by hand, never
	// through the web API, so a web compromise cannot become persistent
	// code execution.
	if in.Update.ManifestURL != cur.Update.ManifestURL || !sameStrings(in.Update.Keys, cur.Update.Keys) {
		return nil, errStatus(http.StatusForbidden, "update.manifestUrl and update.keys cannot be changed through the web interface: "+
			"set them in the operator configuration file (airview.jsonc) or with AIRVIEW_UPDATE_MANIFEST_URL / AIRVIEW_UPDATE_KEYS")
	}
	in.Update.ManifestURL, in.Update.Keys = cur.Update.ManifestURL, cur.Update.Keys
	if !has("locate") {
		in.Locate = cur.Locate
	}
	if !has("ssr") {
		in.SSR = cur.SSR
	}
	if !has("process") {
		in.Process = cur.Process
	}
	if !has("acars") {
		in.ACARS = cur.ACARS
	}
	next, notes, err := s.eng.ApplyConfig(in)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		s.eng.Notify("warn", "Configuration: "+n)
	}
	s.cache.invalidate()
	s.live.pushStatus()
	return config.Redact(next), nil
}

// sameStrings reports whether a and b hold the same strings in the same
// order (nil and empty are equal).
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	b := make([]byte, 0, 4096)
	buf := make([]byte, 8192)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if int64(len(b)) > limit {
			return nil, errStatus(http.StatusRequestEntityTooLarge, "request body too large")
		}
		if err != nil {
			break
		}
	}
	return b, nil
}

func apiFactoryReset(s *Server, r *http.Request) (any, error) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSON(nil, r, &in, 4<<10); err != nil {
		return nil, err
	}
	if in.Confirm != "RESET" {
		return nil, badRequest("confirmation text must be RESET")
	}
	s.log.Warn("factory reset requested", "client", s.logClient(r))
	if err := s.eng.FactoryReset(); err != nil {
		return nil, err
	}
	s.auth.dropAll()
	s.cache.invalidate()
	s.live.pushStatus()
	return map[string]any{"ok": true}, nil
}

func apiAircraft(s *Server, r *http.Request) (any, error) {
	// PRIV-A1: aggregator.publicMap=false closes the viewer routes too.
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	return s.viewSnapshot(r), nil
}

// trailList encodes trail points as the compact arrays
// [tMs, lat, lon, altFt|null, gsKt|null, rssi|null, src].
type trailList []model.TrailPoint

func appendOpt(b []byte, v float64, decimals int) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, v, 'f', decimals, 64)
}

func (t trailList) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 2+len(t)*56)
	b = append(b, '[')
	for i, p := range t {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		b = strconv.AppendInt(b, p.T, 10)
		b = append(b, ',')
		b = appendOpt(b, p.Lat, 5)
		b = append(b, ',')
		b = appendOpt(b, p.Lon, 5)
		b = append(b, ',')
		b = appendOpt(b, p.Alt, 0)
		b = append(b, ',')
		b = appendOpt(b, p.GS, 1)
		b = append(b, ',')
		b = appendOpt(b, p.RSSI, 1)
		b = append(b, ',')
		b = strconv.AppendInt(b, int64(p.Src), 10)
		b = append(b, ']')
	}
	return append(b, ']'), nil
}

// aircraftInfo is the reference data of one address: allocation tables,
// aircraft database, type table and the interpretation of the callsign.
type aircraftInfo struct {
	ICAO string `json:"icao"`
	refdata.Info
	TypeCode     string                `json:"type,omitempty"`      // ICAO type designator
	TypeDesc     string                `json:"typeDesc,omitempty"`  // e.g. "Airbus A320neo"
	TypeClass    string                `json:"typeClass,omitempty"` // e.g. "Landplane, 2 jet engines"
	TypeInfo     *refdata.AircraftType `json:"typeInfo,omitempty"`
	WTC          string                `json:"wtc,omitempty"` // wake turbulence category text
	Operator     string                `json:"operator,omitempty"`
	Year         int                   `json:"year,omitempty"`         // year built
	CategoryDesc string                `json:"categoryDesc,omitempty"` // ADS-B emitter category in words
	Callsign     *refdata.CallsignInfo `json:"callsign,omitempty"`
	Spoken       string                `json:"spoken,omitempty"` // radiotelephony form of the callsign
	Squawk       *refdata.SquawkInfo   `json:"squawk,omitempty"`
	Interesting  bool                  `json:"interesting,omitempty"` // aircraft database flag
	PIA          bool                  `json:"pia,omitempty"`
	LADD         bool                  `json:"ladd,omitempty"`
	// RegSource says where Registration came from: "acdb" (the imported
	// aircraft database), "derived" (calculated from the ICAO address),
	// "callsign" (the aircraft flies under its registration) or "history"
	// (stored with an earlier sighting); empty when there is none.
	RegSource string `json:"regSource,omitempty"`
	// TypeSource says where the type designator came from: "acdb" or
	// "history"; empty when there is none.
	TypeSource string `json:"typeSource,omitempty"`
	// ACDBAttribution is the installed aircraft database's attribution
	// text, sent with the aircraft detail when a value came from it.
	ACDBAttribution string `json:"acdbAttribution,omitempty"`
}

// fromACDB reports whether the registration or the type came from the
// aircraft database.
func (i *aircraftInfo) fromACDB() bool { return i.RegSource == "acdb" || i.TypeSource == "acdb" }

// refSources classifies the registration and type designator buildInfo
// settled on. The live tracker fills its registration from the address
// tables, the aircraft database and the callsign (tracker.rebuildRef) and
// its type only from the database, so a live value that is neither derived
// nor the callsign's is the database's even when db is nil (flight detail).
func refSources(info *aircraftInfo, key uint32, callsign string, live *model.Aircraft, row *store.AircraftRow, db *store.ACDBEntry) (reg, typ string) {
	icao := key &^ modes.NonICAOFlag
	if r := info.Registration; r != "" {
		switch {
		case db != nil && r == db.Reg:
			reg = "acdb"
		case key&modes.NonICAOFlag == 0 && r == refdata.Registration(icao):
			reg = "derived"
		case callsign != "" && callsignReg(callsign, key) == r:
			reg = "callsign"
		case live != nil && r == live.Registration:
			reg = "acdb"
		case row != nil && r == row.Reg:
			reg = "history"
		}
	}
	if t := info.TypeCode; t != "" {
		switch {
		case db != nil && t == db.Type, live != nil && t == live.TypeCode:
			typ = "acdb"
		case row != nil && t == row.Type:
			typ = "history"
		}
	}
	return reg, typ
}

// callsignReg returns the registration a callsign spells, or "".
func callsignReg(callsign string, key uint32) string {
	var ci refdata.CallsignInfo
	if key&modes.NonICAOFlag == 0 {
		ci = refdata.LookupCallsignFor(callsign, key&^modes.NonICAOFlag)
	} else {
		ci = refdata.LookupCallsign(callsign)
	}
	if ci.Kind != refdata.CallsignRegistration {
		return ""
	}
	return ci.Registration
}

// buildInfo merges every reference source for one address. live and row may
// be nil.
func buildInfo(key uint32, live *model.Aircraft, row *store.AircraftRow, db *store.ACDBEntry) aircraftInfo {
	icao := key &^ modes.NonICAOFlag
	info := aircraftInfo{ICAO: modes.FormatICAO(key), Info: refdata.Lookup(icao)}
	if key&modes.NonICAOFlag != 0 {
		// A non-ICAO address (TIS-B track file, anonymous) says nothing
		// about a state of registry.
		info.Info = refdata.Info{ICAO: icao}
	}
	callsign, category, squawk := "", "", ""
	if row != nil {
		info.TypeCode, info.TypeDesc, info.Operator, info.Year = row.Type, row.TypeDesc, row.Operator, row.Year
		if row.Reg != "" {
			info.Registration = row.Reg
		}
		callsign, category = row.Callsign, row.Category
		info.Military = info.Military || row.Flags&store.FlagMilitary != 0
		info.Interesting = row.Flags&store.FlagInteresting != 0
		info.PIA = row.Flags&store.FlagPIA != 0
		info.LADD = row.Flags&store.FlagLADD != 0
	}
	if db != nil {
		if db.Reg != "" {
			info.Registration = db.Reg
		}
		if db.Type != "" {
			info.TypeCode = db.Type
		}
		if db.Desc != "" {
			info.TypeDesc = db.Desc
		}
		if db.Operator != "" {
			info.Operator = db.Operator
		}
		if db.Year != 0 {
			info.Year = db.Year
		}
		info.Military = info.Military || db.Flags&store.FlagMilitary != 0
		info.Interesting = info.Interesting || db.Flags&store.FlagInteresting != 0
		info.PIA = info.PIA || db.Flags&store.FlagPIA != 0
		info.LADD = info.LADD || db.Flags&store.FlagLADD != 0
	}
	if live != nil {
		if live.Registration != "" {
			info.Registration = live.Registration
		}
		if live.TypeCode != "" {
			info.TypeCode = live.TypeCode
		}
		if live.TypeDesc != "" {
			info.TypeDesc = live.TypeDesc
		}
		if live.Operator != "" {
			info.Operator = live.Operator
		}
		if live.Callsign != "" {
			callsign = live.Callsign
		}
		if live.Category != "" {
			category = live.Category
		}
		squawk = live.Squawk
		info.Military = info.Military || live.Military
	}
	info.RegSource, info.TypeSource = refSources(&info, key, callsign, live, row, db)
	if info.TypeCode != "" {
		if t, ok := refdata.LookupType(info.TypeCode); ok {
			info.TypeInfo = &t
			info.TypeClass = t.ClassText()
			info.WTC = t.WTCText()
			if info.TypeDesc == "" {
				info.TypeDesc = t.Name
			}
		}
	}
	if category != "" {
		info.CategoryDesc = refdata.EmitterCategory(category)
	}
	if callsign != "" {
		ci := refdata.LookupCallsignFor(callsign, icao)
		info.Callsign = &ci
		info.Spoken = ci.Spoken()
		if info.Operator == "" {
			info.Operator = ci.Operator
		}
	}
	if squawk != "" {
		if sq, ok := refdata.Squawk(squawk); ok {
			info.Squawk = &sq
		}
	}
	return info
}

func parseICAOParam(v string) (uint32, error) {
	key, err := modes.ParseICAO(strings.ToUpper(strings.TrimSpace(v)))
	if err != nil {
		return 0, badRequest("invalid ICAO address: 6 hexadecimal digits expected")
	}
	return key, nil
}

type aircraftDetail struct {
	Aircraft *model.Aircraft    `json:"aircraft"` // live state, null when not currently tracked
	Trail    trailList          `json:"trail"`    // live in-memory trail
	Info     aircraftInfo       `json:"info"`
	ACDB     *store.ACDBEntry   `json:"acdb"`    // record of the imported aircraft database, null = none
	Seen     *store.AircraftRow `json:"seen"`    // all-time totals of this airframe, null = never stored
	Flights  []store.Flight     `json:"flights"` // newest first
	Watch    *watchInfo         `json:"watch"`   // alert watch-list membership, null = none
}

// watchInfo tells on which alert watch-list an address is.
type watchInfo struct {
	List string `json:"list"`
	Note string `json:"note,omitempty"`
}

func (s *Server) aircraftDetail(r *http.Request, key uint32, flights int) (*aircraftDetail, error) {
	ctx := r.Context()
	st := s.eng.Store()
	out := &aircraftDetail{Trail: trailList{}, Flights: []store.Flight{}}
	if ac, ok := s.eng.Tracker().Aircraft(key); ok {
		out.Aircraft = &ac
		if tr := s.eng.Tracker().Trail(key); tr != nil {
			out.Trail = tr
		}
	}
	s.freshAircraftRows(ctx)
	if row, ok, err := st.AircraftByICAO(ctx, key); err != nil {
		return nil, err
	} else if ok {
		out.Seen = &row
	}
	if e, ok, err := st.ACDBLookup(ctx, key&^modes.NonICAOFlag); err != nil {
		return nil, err
	} else if ok && key&modes.NonICAOFlag == 0 {
		out.ACDB = &e
	}
	fl, err := st.FlightsForAircraft(ctx, key, flights, 0)
	if err != nil {
		return nil, err
	}
	if fl != nil {
		out.Flights = fl
	}
	out.Info = buildInfo(key, out.Aircraft, out.Seen, out.ACDB)
	s.attributeACDB(r.Context(), &out.Info)
	if al := s.eng.Alerts(); al != nil && key&modes.NonICAOFlag == 0 {
		if list, note, ok := al.WatchNote(key); ok {
			out.Watch = &watchInfo{List: list, Note: note}
		}
	}
	return out, nil
}

func apiAircraftDetail(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	if s.netRestricted(r) {
		return s.publicDetail(r, key)
	}
	d, err := s.aircraftDetail(r, key, 20)
	if err == nil && qBool(r, "smooth", false) && len(d.Trail) > 2 {
		d.Trail = trajectory.SmoothTrail(d.Trail, trajectory.SmoothOptions{})
	}
	return d, err
}

type trailsReply struct {
	Now     int64                `json:"now"`
	Minutes int                  `json:"minutes"`
	Trails  map[string]trailList `json:"trails"`
}

func apiTrails(s *Server, r *http.Request) (any, error) {
	if err := s.publicGate(r, mapOn, http.StatusForbidden); err != nil {
		return nil, err
	}
	def := s.eng.EffectiveConfig().UI.TrailMinutes
	if def <= 0 {
		def = 15
	}
	minutes, err := qInt(r, "minutes", def, 1, 240)
	if err != nil {
		return nil, err
	}
	if s.netRestricted(r) {
		bb, err := parseTrailBox(r)
		if err != nil {
			return nil, err
		}
		return s.publicTrails(minutes, bb), nil
	}
	src := s.eng.Tracker().Trails(minutes)
	out := trailsReply{Now: time.Now().UnixMilli(), Minutes: minutes, Trails: make(map[string]trailList, len(src))}
	for k, v := range src {
		out.Trails[k] = v
	}
	return out, nil
}

func apiNotices(s *Server, r *http.Request) (any, error) {
	n := s.eng.RecentNotices()
	if s.netRestricted(r) {
		return map[string]any{"notices": publicNotices(n)}, nil
	}
	if n == nil {
		n = []engine.Notice{}
	}
	return map[string]any{"notices": n}, nil
}

type feedsReply struct {
	Mode        string                    `json:"mode"` // effective run mode
	Demo        bool                      `json:"demo"`
	Feeds       []model.FeedStatus        `json:"feeds"`    // connection state per feed
	Counters    []tracker.FeedCounters    `json:"counters"` // decoded totals per feed
	Antennas    []tracker.AntennaCounters `json:"antennas"` // totals per antenna, index 0 = "" (combined)
	Hub         hubReply                  `json:"hub"`
	ReceiverGPS *model.ReceiverGPS        `json:"receiverGps"`
}

type hubReply struct {
	model.HubStatus
	Error          string           `json:"error,omitempty"` // why the data port could not start
	ClientList     []hub.ClientInfo `json:"clientList"`
	DroppedClients uint64           `json:"droppedClients"` // clients cut off for being too slow
}

func (s *Server) hubReply() hubReply {
	out := hubReply{HubStatus: s.eng.HubStatus(), Error: s.eng.HubError(), ClientList: []hub.ClientInfo{}}
	if h := s.eng.Hub(); h != nil {
		if c := h.Clients(); c != nil {
			out.ClientList = c
		}
		out.DroppedClients = h.DroppedClients()
	}
	return out
}

func apiFeeds(s *Server, r *http.Request) (any, error) {
	c := s.eng.Tracker().Counters()
	return feedsReply{
		Mode: s.eng.Mode(), Demo: s.eng.Demo(),
		Feeds: s.eng.FeedStatus(), Counters: c.Feeds, Antennas: c.Antennas,
		Hub: s.hubReply(), ReceiverGPS: s.eng.ReceiverGPS(),
	}, nil
}

type memoryInfo struct {
	AllocBytes     uint64  `json:"allocBytes"`     // live heap objects
	HeapInuseBytes uint64  `json:"heapInuseBytes"` //
	SysBytes       uint64  `json:"sysBytes"`       // obtained from the OS
	StackBytes     uint64  `json:"stackBytes"`
	NumGC          uint32  `json:"numGc"`
	GCPauseMs      float64 `json:"gcPauseMs"` // most recent pause
}

type systemReply struct {
	App             string         `json:"app"`
	Version         string         `json:"version"`
	Platform        string         `json:"platform"` // GOOS/GOARCH
	GoVersion       string         `json:"goVersion"`
	Hostname        string         `json:"hostname"`
	PID             int            `json:"pid"`
	CPUs            int            `json:"cpus"`
	Goroutines      int            `json:"goroutines"`
	Memory          memoryInfo     `json:"memory"`
	Now             int64          `json:"now"`
	Started         int64          `json:"started"`
	UptimeS         int64          `json:"uptimeS"`
	TimeZone        string         `json:"timeZone"`     // of the host
	UTCOffsetSec    int            `json:"utcOffsetSec"` // of the host, now
	DataDir         string         `json:"dataDir"`
	DBBytes         int64          `json:"dbBytes"`
	GUI             bool           `json:"gui"`
	Mode            string         `json:"mode"`
	Demo            bool           `json:"demo"`
	Located         bool           `json:"located"`
	HTTPAddr        string         `json:"httpAddr"`
	HTTPPort        int            `json:"httpPort"`
	RestartRequired bool           `json:"restartRequired"` // a saved setting (HTTP port / bind address) needs a restart
	LiveClients     int            `json:"liveClients"`     // open /api/events streams
	Sessions        int            `json:"sessions"`        // admin logins
	CacheEntries    int            `json:"cacheEntries"`    // cached API responses
	Forecaster      predict.Status `json:"forecaster"`
	Tracker         trackerBrief   `json:"tracker"`
}

type trackerBrief struct {
	Frames   uint64  `json:"frames"`
	MsgRate  float64 `json:"msgRate"`
	Aircraft int     `json:"aircraft"`
	Tracked  int     `json:"tracked"`
}

func apiSystem(s *Server, r *http.Request) (any, error) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := s.eng.Status()
	now := time.Now()
	zone, off := now.Zone()
	host, _ := os.Hostname()
	c := s.eng.Tracker().Counters()
	out := systemReply{
		App: model.AppName, Version: s.opts.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion: runtime.Version(), Hostname: host, PID: os.Getpid(), CPUs: runtime.NumCPU(),
		Goroutines: runtime.NumGoroutine(),
		Memory: memoryInfo{
			AllocBytes: ms.Alloc, HeapInuseBytes: ms.HeapInuse, SysBytes: ms.Sys, StackBytes: ms.StackInuse,
			NumGC: ms.NumGC, GCPauseMs: float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e6,
		},
		Now: now.UnixMilli(), Started: st.Started, UptimeS: (now.UnixMilli() - st.Started) / 1000,
		TimeZone: zone, UTCOffsetSec: off,
		DataDir: s.eng.DataDir(), DBBytes: st.DBBytes, GUI: s.opts.GUI, Mode: st.Mode, Demo: st.Demo,
		Located: st.Located, HTTPAddr: s.Addr(), HTTPPort: s.Port(),
		RestartRequired: s.eng.RestartRequired(),
		LiveClients:     s.live.count(), Sessions: s.auth.count(), CacheEntries: s.cache.size(),
		Forecaster: s.eng.Forecaster().Status(),
		Tracker:    trackerBrief{Frames: c.Frames, MsgRate: c.MsgRate, Aircraft: c.Aircraft, Tracked: c.Tracked},
	}
	if !s.isAdmin(r) {
		// Host details (the data path contains the OS user name) are for admins.
		out.Hostname, out.PID, out.DataDir = "", 0, ""
	}
	return out, nil
}

func apiTileCacheStats(s *Server, r *http.Request) (any, error) {
	st := s.tiles.stats()
	if !s.isAdmin(r) {
		st.Dir = ""
	}
	return st, nil
}

func apiTileCacheClear(s *Server, r *http.Request) (any, error) {
	if err := s.tiles.clear(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// attributeACDB sets info.ACDBAttribution when a shown value came from the
// aircraft database. A failed read only leaves the attribution out.
func (s *Server) attributeACDB(ctx context.Context, info *aircraftInfo) {
	if !info.fromACDB() {
		return
	}
	if a, err := s.eng.AircraftDBAttribution(ctx); err == nil {
		info.ACDBAttribution = a
	}
}
