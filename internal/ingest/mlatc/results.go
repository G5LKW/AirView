package mlatc

import (
	"math"
	"strconv"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Unit conversions of the result message (MLATC.md §6): velocities in
// knots, vertical rate in ft/min, legacy altitude in feet.
const (
	mpsToKt     = 3600.0 / 1852.0
	mpsToFtMin  = 60.0 / 0.3048
	mToFt       = 1 / 0.3048
	gdopMetres  = 300.0 // the client turns gdop into an error estimate of gdop × 300 m
	resultPrecM = 1     // decimals of ECEF metres
)

// stationsFor returns the station count reported for a result.
func stationsFor(r *Result) int {
	if r.NStations > 0 {
		return r.NStations
	}
	switch r.NBucket {
	case model.NBucket4:
		return 4
	case model.NBucket5to8:
		return 5
	case model.NBucket9Plus:
		return 9
	}
	return 3
}

// appendResult appends one result line in the client's requested format.
//
// ecef format: {"result":{"@":T,"addr":"abcdef","ecef":[x,y,z],
// "cov":[xx,xy,xz,yy,yz,zz],"nd":N[,"ewvel":E,"nsvel":N,"vrate":V]
// [,"callsign":C][,"squawk":S]}} — T in Unix seconds, ECEF in metres,
// covariance in m² (upper triangle, row major, as model.MLATFix.Cov),
// velocities in knots and ft/min.
//
// legacy format: the same with "lat","lon" (degrees), "alt" (feet above the
// WGS-84 ellipsoid: what the ECEF path yields in the client too),
// "gdop" (= sqrt(trace cov) / 300, so the client's gdop·300 error estimate
// equals the 3-D 1-sigma) and "nstations" instead of ecef/cov/nd.
func appendResult(dst []byte, r *Result, ecef bool) []byte {
	dst = append(dst, `{"result":{"@":`...)
	dst = strconv.AppendFloat(dst, float64(r.TUnixNs)/1e9, 'f', 3, 64)
	dst = append(dst, `,"addr":"`...)
	dst = appendHex6(dst, r.ICAO)
	dst = append(dst, '"')
	if ecef {
		dst = append(dst, `,"ecef":[`...)
		for i, v := range r.ECEF {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendNum(dst, v, resultPrecM)
		}
		dst = append(dst, `],"cov":[`...)
		for i, v := range r.Cov {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendNum(dst, float64(v), 2)
		}
		dst = append(dst, `],"nd":`...)
		dst = strconv.AppendInt(dst, int64(stationsFor(r)), 10)
	} else {
		llh := geo.FromECEF(geo.Vec3{X: r.ECEF[0], Y: r.ECEF[1], Z: r.ECEF[2]})
		dst = append(dst, `,"lat":`...)
		dst = appendNum(dst, llh.Lat, 6)
		dst = append(dst, `,"lon":`...)
		dst = appendNum(dst, llh.Lon, 6)
		dst = append(dst, `,"alt":`...)
		dst = appendNum(dst, llh.H*mToFt, 0)
		tr := float64(r.Cov[0]) + float64(r.Cov[3]) + float64(r.Cov[5])
		g := -1.0
		if tr >= 0 && !math.IsInf(tr, 0) && !math.IsNaN(tr) {
			g = math.Sqrt(tr) / gdopMetres
		}
		dst = append(dst, `,"gdop":`...)
		dst = appendNum(dst, g, 3)
		dst = append(dst, `,"nstations":`...)
		dst = strconv.AppendInt(dst, int64(stationsFor(r)), 10)
	}
	if v := r.Vel; v != nil {
		dst = append(dst, `,"ewvel":`...)
		dst = appendNum(dst, float64(v[0])*mpsToKt, 1)
		dst = append(dst, `,"nsvel":`...)
		dst = appendNum(dst, float64(v[1])*mpsToKt, 1)
		dst = append(dst, `,"vrate":`...)
		dst = appendNum(dst, float64(v[2])*mpsToFtMin, 0)
	}
	dst = appendIdent(dst, `,"callsign":"`, r.Callsign, 8)
	dst = appendIdent(dst, `,"squawk":"`, r.Squawk, 4)
	return append(dst, "}}\n"...)
}

// appendNum appends a finite number with prec decimals (non-finite values
// become 0: JSON has no NaN).
func appendNum(dst []byte, v float64, prec int) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		v = 0
	}
	return strconv.AppendFloat(dst, v, 'f', prec, 64)
}

const hexDigits = "0123456789abcdef"

func appendHex6(dst []byte, a uint32) []byte {
	for s := 20; s >= 0; s -= 4 {
		dst = append(dst, hexDigits[(a>>uint(s))&15])
	}
	return dst
}

// appendIdent appends prefix + cleanIdent(s) + '"' when the cleaned text
// is not empty.
func appendIdent(dst []byte, prefix, s string, max int) []byte {
	var b [16]byte
	v := cleanInto(&b, s, max)
	if len(v) == 0 {
		return dst
	}
	dst = append(dst, prefix...)
	dst = append(dst, v...)
	return append(dst, '"')
}

// cleanIdent keeps A-Z, 0-9 and spaces (callsigns, squawks), trimmed and
// cut to max, so the text never needs JSON escaping.
func cleanIdent(s string, max int) string {
	var b [16]byte
	return string(cleanInto(&b, s, max))
}

func cleanInto(b *[16]byte, s string, max int) []byte {
	n := 0
	for i := 0; i < len(s) && n < max && n < len(b); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ' ':
		default:
			continue
		}
		b[n] = c
		n++
	}
	for n > 0 && b[n-1] == ' ' {
		n--
	}
	st := 0
	for st < n && b[st] == ' ' {
		st++
	}
	return b[st:n]
}

// appendAddrList appends {"<key>":["abcdef",...]}\n.
func appendAddrList(dst []byte, key string, addrs []uint32) []byte {
	dst = append(dst, `{"`...)
	dst = append(dst, key...)
	dst = append(dst, `":[`...)
	for i, a := range addrs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = appendHex6(dst, a)
		dst = append(dst, '"')
	}
	return append(dst, "]}\n"...)
}

// appendStats appends a stats message.
func appendStats(dst []byte, s ClientStats) []byte {
	dst = append(dst, `{"stats":{"peer_count":`...)
	dst = strconv.AppendInt(dst, int64(max(s.PeerCount, 0)), 10)
	dst = append(dst, `,"outlier_percent":`...)
	dst = appendNum(dst, math.Max(0, math.Min(100, s.OutlierPercent)), 1)
	dst = append(dst, `,"bad_sync_timeout":`...)
	dst = appendNum(dst, math.Max(0, s.BadSyncTimeout), 0)
	return append(dst, "}}\n"...)
}

// appendHeartbeat appends a server heartbeat.
func appendHeartbeat(dst []byte, unixNs int64) []byte {
	dst = append(dst, `{"heartbeat":{"server_time":`...)
	dst = strconv.AppendFloat(dst, float64(unixNs)/1e9, 'f', 3, 64)
	return append(dst, "}}\n"...)
}
