package store

import (
	"encoding/json"
	"fmt"
	"math"
)

// JSON (HTTP API) forms of the row types. encoding/json cannot encode NaN,
// which this package uses for "unknown", so every row type the read API
// returns has a MarshalJSON method. The API conventions of internal/model
// apply: timestamps in Unix milliseconds, ICAO addresses as 6 upper-case hex
// digits, unknown values omitted.

// HexICAO formats an ICAO address the way the API does ("4CA2D1"); 0 gives "".
func HexICAO(icao uint32) string {
	if icao == 0 {
		return ""
	}
	return fmt.Sprintf("%06X", icao)
}

// fp returns nil for an unknown (NaN / Inf) value.
func fp(v float64) *float64 {
	if !isNum(v) {
		return nil
	}
	return &v
}

// rawJSON embeds a stored JSON document; text that is not valid JSON is
// encoded as a JSON string, "" is omitted.
func rawJSON(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// MarshalJSON implements json.Marshaler (API form).
func (r AircraftRow) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ICAO      string   `json:"icao"`
		FirstSeen int64    `json:"firstSeen"`
		LastSeen  int64    `json:"lastSeen"`
		Callsign  string   `json:"callsign,omitempty"`
		Reg       string   `json:"reg,omitempty"`
		Type      string   `json:"type,omitempty"`
		TypeDesc  string   `json:"typeDesc,omitempty"`
		Operator  string   `json:"operator,omitempty"`
		Country   string   `json:"country,omitempty"`
		Category  string   `json:"category,omitempty"`
		Msgs      int64    `json:"msgs"`
		Flights   int64    `json:"flights"`
		Positions int64    `json:"positions"`
		MaxDistKm *float64 `json:"maxDistKm,omitempty"`
		MaxAlt    *float64 `json:"maxAlt,omitempty"`
		SrcMask   int      `json:"srcMask"`
		Flags     int      `json:"flags"`
		Military  bool     `json:"military,omitempty"`
		Year      int      `json:"year,omitempty"`
	}{
		ICAO: HexICAO(r.ICAO), FirstSeen: r.FirstSeen * 1000, LastSeen: r.LastSeen * 1000,
		Callsign: r.Callsign, Reg: r.Reg, Type: r.Type, TypeDesc: r.TypeDesc, Operator: r.Operator,
		Country: r.Country, Category: r.Category, Msgs: r.Msgs, Flights: r.Flights, Positions: r.Positions,
		MaxDistKm: positive(r.MaxDistKm), MaxAlt: fp(r.MaxAlt), SrcMask: r.SrcMask, Flags: r.Flags,
		Military: r.Flags&FlagMilitary != 0, Year: r.Year,
	})
}

// positive returns nil for unknown or non-positive values (max_dist_km is
// stored as 0 when no position was ever received).
func positive(v float64) *float64 {
	if !isNum(v) || v <= 0 {
		return nil
	}
	return &v
}

// MarshalJSON implements json.Marshaler (API form).
func (f Flight) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID         int64    `json:"id"`
		ICAO       string   `json:"icao"`
		Callsign   string   `json:"callsign,omitempty"`
		Squawk     string   `json:"squawk,omitempty"`
		Start      int64    `json:"start"`
		End        int64    `json:"end"`
		Msgs       int64    `json:"msgs"`
		Positions  int64    `json:"positions"`
		SrcMask    int      `json:"srcMask"`
		MaxDistKm  *float64 `json:"maxDistKm,omitempty"`
		MaxDistBrg *float64 `json:"maxDistBrg,omitempty"`
		MinDistKm  *float64 `json:"minDistKm,omitempty"`
		MinAlt     *float64 `json:"minAlt,omitempty"`
		MaxAlt     *float64 `json:"maxAlt,omitempty"`
		MaxGS      *float64 `json:"maxGs,omitempty"`
		RSSIMin    *float64 `json:"rssiMin,omitempty"`
		RSSIMax    *float64 `json:"rssiMax,omitempty"`
		RSSIAvg    *float64 `json:"rssiAvg,omitempty"`
		FirstLat   *float64 `json:"firstLat,omitempty"`
		FirstLon   *float64 `json:"firstLon,omitempty"`
		LastLat    *float64 `json:"lastLat,omitempty"`
		LastLon    *float64 `json:"lastLon,omitempty"`
		Emergency  string   `json:"emergency,omitempty"`
		Reg        string   `json:"reg,omitempty"`
		Type       string   `json:"type,omitempty"`
		Operator   string   `json:"operator,omitempty"`
		Country    string   `json:"country,omitempty"`
		Dep        string   `json:"dep,omitempty"`
		Arr        string   `json:"arr,omitempty"`
	}{
		ID: f.ID, ICAO: HexICAO(f.ICAO), Callsign: f.Callsign, Squawk: f.Squawk,
		Start: f.Start * 1000, End: f.End * 1000, Msgs: f.Msgs, Positions: f.Positions, SrcMask: f.SrcMask,
		MaxDistKm: fp(f.MaxDistKm), MaxDistBrg: fp(f.MaxDistBrg), MinDistKm: fp(f.MinDistKm),
		MinAlt: fp(f.MinAlt), MaxAlt: fp(f.MaxAlt), MaxGS: fp(f.MaxGS),
		RSSIMin: fp(f.RSSIMin), RSSIMax: fp(f.RSSIMax), RSSIAvg: fp(f.RSSIAvg),
		FirstLat: fp(f.FirstLat), FirstLon: fp(f.FirstLon), LastLat: fp(f.LastLat), LastLon: fp(f.LastLon),
		Emergency: f.Emergency, Reg: f.Reg, Type: f.Type, Operator: f.Operator, Country: f.Country,
		Dep: f.Dep, Arr: f.Arr,
	})
}

// MarshalJSON implements json.Marshaler (API form). The stats_hour-only
// fields aircraftAvg and minutes are omitted for minute rows.
func (r StatsRow) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		T             int64    `json:"t"`
		Ant           string   `json:"ant"`
		Frames        int64    `json:"frames"`
		DF0           int64    `json:"df0"`
		DF4           int64    `json:"df4"`
		DF5           int64    `json:"df5"`
		DF11          int64    `json:"df11"`
		DF16          int64    `json:"df16"`
		DF17          int64    `json:"df17"`
		DF18          int64    `json:"df18"`
		DF20          int64    `json:"df20"`
		DF21          int64    `json:"df21"`
		DFOther       int64    `json:"dfOther"`
		CRCBad        int64    `json:"crcBad"`
		Positions     int64    `json:"positions"`
		MLATPositions int64    `json:"mlatPositions"`
		Aircraft      int      `json:"aircraft"`
		AircraftPos   int      `json:"aircraftPos"`
		ADSB          int      `json:"adsb"`
		MLAT          int      `json:"mlat"`
		ModeS         int      `json:"modes"`
		MaxKm         *float64 `json:"maxKm,omitempty"`
		RSSIAvg       *float64 `json:"rssiAvg,omitempty"`
		RSSIMin       *float64 `json:"rssiMin,omitempty"`
		RSSIMax       *float64 `json:"rssiMax,omitempty"`
		Secs          float64  `json:"secs,omitempty"` // covered seconds (omitted when unknown)
		AircraftAvg   *float64 `json:"aircraftAvg,omitempty"`
		Minutes       int      `json:"minutes,omitempty"`
	}{
		T: r.T * 1000, Ant: r.Ant, Frames: r.Frames,
		DF0: r.DF0, DF4: r.DF4, DF5: r.DF5, DF11: r.DF11, DF16: r.DF16, DF17: r.DF17, DF18: r.DF18,
		DF20: r.DF20, DF21: r.DF21, DFOther: r.DFOther, CRCBad: r.CRCBad,
		Positions: r.Positions, MLATPositions: r.MLATPositions,
		Aircraft: r.Aircraft, AircraftPos: r.AircraftPos, ADSB: r.ADSB, MLAT: r.MLAT, ModeS: r.ModeS,
		MaxKm: fp(r.MaxKm), RSSIAvg: fp(r.RSSIAvg), RSSIMin: fp(r.RSSIMin), RSSIMax: fp(r.RSSIMax),
		Secs: r.Secs, AircraftAvg: hourOnly(r), Minutes: r.Minutes,
	})
}

func hourOnly(r StatsRow) *float64 {
	if r.Minutes == 0 {
		return nil
	}
	return fp(r.AircraftAvg)
}

// MarshalJSON implements json.Marshaler (API form). issued and leadH appear
// only for forecast rows; profile is the stored pressure-level document.
func (w Weather) MarshalJSON() ([]byte, error) {
	out := struct {
		T               int64           `json:"t"`
		Issued          int64           `json:"issued,omitempty"`
		LeadH           *int            `json:"leadH,omitempty"`
		TempC           *float64        `json:"tempC,omitempty"`
		DewpointC       *float64        `json:"dewpointC,omitempty"`
		RH              *float64        `json:"rh,omitempty"`
		PressureMSL     *float64        `json:"pressureMsl,omitempty"`
		SurfacePressure *float64        `json:"surfacePressure,omitempty"`
		WindKmh         *float64        `json:"windKmh,omitempty"`
		WindDir         *float64        `json:"windDir,omitempty"`
		GustKmh         *float64        `json:"gustKmh,omitempty"`
		Cloud           *float64        `json:"cloud,omitempty"`
		CloudLow        *float64        `json:"cloudLow,omitempty"`
		CloudMid        *float64        `json:"cloudMid,omitempty"`
		CloudHigh       *float64        `json:"cloudHigh,omitempty"`
		PrecipMM        *float64        `json:"precipMm,omitempty"`
		RainMM          *float64        `json:"rainMm,omitempty"`
		SnowCM          *float64        `json:"snowCm,omitempty"`
		VisibilityM     *float64        `json:"visibilityM,omitempty"`
		CAPE            *float64        `json:"cape,omitempty"`
		WeatherCode     *float64        `json:"weatherCode,omitempty"`
		IsDay           *float64        `json:"isDay,omitempty"`
		NSurface        *float64        `json:"nSurface,omitempty"`
		DNDH            *float64        `json:"dnDh,omitempty"`
		KFactor         *float64        `json:"kFactor,omitempty"`
		Duct            *float64        `json:"duct,omitempty"`
		Profile         json.RawMessage `json:"profile,omitempty"`
	}{
		T: w.T * 1000, Issued: w.Issued * 1000,
		TempC: fp(w.TempC), DewpointC: fp(w.DewpointC), RH: fp(w.RH), PressureMSL: fp(w.PressureMSL),
		SurfacePressure: fp(w.SurfacePressure), WindKmh: fp(w.WindKmh), WindDir: fp(w.WindDir),
		GustKmh: fp(w.GustKmh), Cloud: fp(w.Cloud), CloudLow: fp(w.CloudLow), CloudMid: fp(w.CloudMid),
		CloudHigh: fp(w.CloudHigh), PrecipMM: fp(w.PrecipMM), RainMM: fp(w.RainMM), SnowCM: fp(w.SnowCM),
		VisibilityM: fp(w.VisibilityM), CAPE: fp(w.CAPE), WeatherCode: fp(w.WeatherCode), IsDay: fp(w.IsDay),
		NSurface: fp(w.NSurface), DNDH: fp(w.DNDH), KFactor: fp(w.KFactor), Duct: fp(w.Duct),
		Profile: rawJSON(w.ProfileJSON),
	}
	if w.Issued > 0 {
		lead := w.LeadH
		if lead == 0 { // weather_fc rows carry no lead column
			lead = int(floorDiv(w.T-w.Issued+1800, 3600))
		}
		if lead >= 0 {
			out.LeadH = &lead
		}
	}
	return json.Marshal(out)
}

// MarshalJSON implements json.Marshaler (API form). distHist is the array
// of 5 km bin counts, sectorMax the array of per-10-degree maxima in km.
func (r RangeHour) MarshalJSON() ([]byte, error) {
	sect := make([]float64, len(r.SectorMax))
	for i, v := range r.SectorMax {
		if f := float64(v); isNum(f) {
			sect[i] = math.Round(f*100) / 100 // float32 noise
		}
	}
	return json.Marshal(struct {
		T         int64     `json:"t"`
		Ant       string    `json:"ant"`
		MaxKm     *float64  `json:"maxKm,omitempty"`
		P99Km     *float64  `json:"p99Km,omitempty"`
		P95Km     *float64  `json:"p95Km,omitempty"`
		P90Km     *float64  `json:"p90Km,omitempty"`
		P50Km     *float64  `json:"p50Km,omitempty"`
		Positions int64     `json:"positions"`
		Aircraft  int       `json:"aircraft"`
		FarCount  int64     `json:"farCount"`
		ResidDB   *float64  `json:"residDb,omitempty"`
		ResidN    int64     `json:"residN"`
		DistHist  []uint32  `json:"distHist,omitempty"`
		SectorMax []float64 `json:"sectorMax,omitempty"`
	}{
		T: r.T * 1000, Ant: r.Ant, MaxKm: fp(r.MaxKm), P99Km: fp(r.P99Km), P95Km: fp(r.P95Km), P90Km: fp(r.P90Km),
		P50Km: fp(r.P50Km), Positions: r.Positions, Aircraft: r.Aircraft, FarCount: r.FarCount,
		ResidDB: fp(r.ResidDB), ResidN: r.ResidN, DistHist: r.DistHist, SectorMax: sect,
	})
}

// MarshalJSON implements json.Marshaler (API form).
func (n NTPSample) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		T        int64    `json:"t"`
		Server   string   `json:"server,omitempty"`
		OffsetMs *float64 `json:"offsetMs,omitempty"`
		DelayMs  *float64 `json:"delayMs,omitempty"`
		Stratum  int      `json:"stratum"`
	}{n.T * 1000, n.Server, fp(n.OffsetMs), fp(n.DelayMs), n.Stratum})
}

// MarshalJSON implements json.Marshaler (API form).
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID     int64           `json:"id"`
		T      int64           `json:"t"`
		Kind   string          `json:"kind"`
		ICAO   string          `json:"icao,omitempty"`
		Detail json.RawMessage `json:"detail,omitempty"`
	}{e.ID, e.T * 1000, e.Kind, HexICAO(e.ICAO), rawJSON(e.Detail)})
}

// MarshalJSON implements json.Marshaler (API form).
func (r Record) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Key    string          `json:"key"`
		Value  *float64        `json:"value"`
		ICAO   string          `json:"icao,omitempty"`
		T      int64           `json:"t"`
		Detail json.RawMessage `json:"detail,omitempty"`
	}{r.Key, fp(r.Value), HexICAO(r.ICAO), r.T * 1000, rawJSON(r.Detail)})
}

// MarshalJSON implements json.Marshaler (API form).
func (e ACDBEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ICAO     string `json:"icao"`
		Reg      string `json:"reg,omitempty"`
		Type     string `json:"type,omitempty"`
		Desc     string `json:"typeDesc,omitempty"`
		Operator string `json:"operator,omitempty"`
		Flags    int    `json:"flags"`
		Military bool   `json:"military,omitempty"`
		Year     int    `json:"year,omitempty"`
	}{HexICAO(e.ICAO), e.Reg, e.Type, e.Desc, e.Operator, e.Flags, e.Flags&FlagMilitary != 0, e.Year})
}
