package publicdto

import (
	"math"
	"strings"

	"airview/internal/model"
)

// tar1090-compatible JSON (GET /data/aircraft.json and /data/receiver.json),
// so tar1090-style map front-ends and scripts can read the merged picture.
// The shape follows the widely documented readsb/dump1090 aircraft.json
// field names; it is produced from our own model, field by field.
//
// Privacy (DESIGN §8.4): no receiver fields. There is no rssi, no per
// aircraft message count (a reception measure) and no receiver position.

// TarAircraftFile is /data/aircraft.json.
type TarAircraftFile struct {
	Now      float64       `json:"now"`      // Unix seconds
	Messages uint64        `json:"messages"` // merged frames since start (network total)
	Aircraft []TarAircraft `json:"aircraft"`
}

// TarAircraft is one entry of aircraft.json.
type TarAircraft struct {
	Hex      string   `json:"hex"`
	Type     string   `json:"type"`
	Flight   string   `json:"flight,omitempty"`
	Reg      string   `json:"r,omitempty"`
	TypeCode string   `json:"t,omitempty"`
	Desc     string   `json:"desc,omitempty"`
	AltBaro  any      `json:"alt_baro,omitempty"` // ft, or "ground"
	AltGeom  *int     `json:"alt_geom,omitempty"`
	GS       *float64 `json:"gs,omitempty"`
	IAS      *int     `json:"ias,omitempty"`
	TAS      *int     `json:"tas,omitempty"`
	Mach     *float64 `json:"mach,omitempty"`
	Track    *float64 `json:"track,omitempty"`
	TrackRt  *float64 `json:"track_rate,omitempty"`
	Roll     *float64 `json:"roll,omitempty"`
	MagHdg   *float64 `json:"mag_heading,omitempty"`
	BaroRate *int     `json:"baro_rate,omitempty"`
	Squawk   string   `json:"squawk,omitempty"`
	Emerg    string   `json:"emergency,omitempty"`
	Category string   `json:"category,omitempty"`
	NavQNH   *float64 `json:"nav_qnh,omitempty"`
	NavAlt   *int     `json:"nav_altitude_mcp,omitempty"`
	NavHdg   *float64 `json:"nav_heading,omitempty"`
	NavModes []string `json:"nav_modes,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	NIC      *int     `json:"nic,omitempty"`
	RC       *int     `json:"rc,omitempty"`
	SeenPos  *float64 `json:"seen_pos,omitempty"`
	Version  *int     `json:"version,omitempty"`
	NACp     *int     `json:"nac_p,omitempty"`
	NACv     *int     `json:"nac_v,omitempty"`
	SIL      *int     `json:"sil,omitempty"`
	Alert    int      `json:"alert"`
	SPI      int      `json:"spi"`
	MLAT     []string `json:"mlat"`
	TISB     []string `json:"tisb"`
	Seen     float64  `json:"seen"`
	DBFlags  int      `json:"dbFlags,omitempty"` // 1 = military
}

func tarType(a *Aircraft) string {
	nonICAO := a.AddrType != "" && a.AddrType != "icao"
	switch a.Source {
	case model.SrcADSB:
		if nonICAO {
			return "adsb_other"
		}
		return "adsb_icao"
	case model.SrcADSR:
		if nonICAO {
			return "adsr_other"
		}
		return "adsr_icao"
	case model.SrcTISB:
		if nonICAO {
			return "tisb_other"
		}
		return "tisb_icao"
	case model.SrcMLAT:
		return "mlat"
	case model.SrcModeS:
		return "mode_s"
	}
	return "other"
}

func tarEmergency(e string) string {
	switch e {
	case "":
		return "none"
	case "medical":
		return "lifeguard"
	}
	return e
}

func iptr(p *float64) *int {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	v := int(math.Round(*p))
	return &v
}

func r1(p *float64, decimals float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	v := math.Round(*p*decimals) / decimals
	return &v
}

// TarFromPublic converts one public aircraft to the tar1090 shape. nowMs is
// the snapshot time.
func TarFromPublic(a *Aircraft, nowMs int64) TarAircraft {
	t := TarAircraft{
		Hex: strings.ToLower(a.ICAO), Type: tarType(a), Reg: a.Registration, TypeCode: a.TypeCode,
		Desc: a.TypeDesc, Squawk: a.Squawk, Emerg: tarEmergency(a.Emergency), Category: a.Category,
		Lat: r1(a.Lat, 1e6), Lon: r1(a.Lon, 1e6), GS: r1(a.GS, 10), Track: r1(a.Track, 100),
		TrackRt: r1(a.TrackRate, 100), Roll: r1(a.Roll, 100), MagHdg: r1(a.MagHeading, 100),
		Mach: r1(a.Mach, 1000), NavQNH: r1(a.QNH, 10), NavHdg: r1(a.SelHeading, 100),
		AltGeom: iptr(a.AltGeom), IAS: iptr(a.IAS), TAS: iptr(a.TAS), BaroRate: iptr(a.VertRate),
		NavAlt: iptr(a.SelAlt), NavModes: a.NavModes, SeenPos: r1(a.PosAge, 10),
		Version: a.ADSBVersion, NIC: a.NIC, NACp: a.NACp, NACv: a.NACv, SIL: a.SIL,
		MLAT: []string{}, TISB: []string{},
	}
	if a.Callsign != "" {
		t.Flight = (a.Callsign + "        ")[:max(8, len(a.Callsign))]
	}
	switch {
	case a.OnGround != nil && *a.OnGround:
		t.AltBaro = "ground"
	case a.AltBaro != nil:
		t.AltBaro = iptr(a.AltBaro)
	}
	if a.RcM != nil && !math.IsNaN(*a.RcM) {
		t.RC = iptr(a.RcM)
	}
	if a.Alert {
		t.Alert = 1
	}
	if a.SPI {
		t.SPI = 1
	}
	if a.Military {
		t.DBFlags = 1
	}
	if a.LastSeen > 0 && nowMs >= a.LastSeen {
		t.Seen = math.Round(float64(nowMs-a.LastSeen)/100) / 10
	}
	switch a.Source {
	case model.SrcMLAT:
		t.MLAT = []string{"lat", "lon"}
		if a.GS != nil {
			t.MLAT = append(t.MLAT, "gs")
		}
		if a.Track != nil {
			t.MLAT = append(t.MLAT, "track")
		}
		if a.VertRate != nil {
			t.MLAT = append(t.MLAT, "baro_rate")
		}
	case model.SrcTISB:
		t.TISB = []string{"lat", "lon"}
	}
	return t
}

// TarFromSnapshot converts a public snapshot. Predicted (coasting)
// positions are not exported: tar1090 clients extrapolate themselves.
func TarFromSnapshot(s *Snapshot, messages uint64) TarAircraftFile {
	out := TarAircraftFile{Now: math.Round(float64(s.Now)/100) / 10, Messages: messages, Aircraft: make([]TarAircraft, 0, len(s.Aircraft))}
	for i := range s.Aircraft {
		a := s.Aircraft[i]
		if a.Predicted {
			a.Lat, a.Lon = a.LastRealLat, a.LastRealLon
		}
		out.Aircraft = append(out.Aircraft, TarFromPublic(&a, s.Now))
	}
	return out
}

// TarReceiver is /data/receiver.json. Lat/Lon are present only in
// standalone mode (the local receiver, for its own operator) or, in
// aggregator mode, as the rounded centroid of the public pins.
type TarReceiver struct {
	Version string   `json:"version"`
	Refresh int      `json:"refresh"` // ms between aircraft.json updates
	History int      `json:"history"` // number of history files (none)
	Lat     *float64 `json:"lat,omitempty"`
	Lon     *float64 `json:"lon,omitempty"`
}
