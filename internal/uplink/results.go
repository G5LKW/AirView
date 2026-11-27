package uplink

import (
	"math"
	"strings"
	"time"

	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// FeedPrefix is the FeedID prefix of MLAT results injected into the local
// tracker ("uplink/<id>"). Such updates are never forwarded upstream.
const FeedPrefix = "uplink/"

// IsInjectedFeed reports whether feedID names injected uplink results.
func IsInjectedFeed(feedID string) bool { return strings.HasPrefix(feedID, FeedPrefix) }

const (
	mToFt      = 3.280839895
	mpsToKt    = 1.943844492
	mpsToFtMin = 196.8503937
)

// FixUpdate converts an MLAT result into a DecodedUpdate for the local
// tracker. sigma is the horizontal DRMS (sqrt(var_e + var_n)) of the fix
// covariance. Recv is the transmit time unless the fix is in relative
// (unanchored) network time, when now is used.
func FixUpdate(f *model.MLATFix, feedID string, now time.Time) model.DecodedUpdate {
	p := geo.Vec3{X: f.ECEF[0], Y: f.ECEF[1], Z: f.ECEF[2]}
	llh := geo.FromECEF(p)
	fr := geo.NewFrame(llh)
	c := f.Cov
	full := [9]float64{
		float64(c[0]), float64(c[1]), float64(c[2]),
		float64(c[1]), float64(c[3]), float64(c[4]),
		float64(c[2]), float64(c[4]), float64(c[5]),
	}
	enu := fr.CovToENU(full)
	sigma := math.Sqrt(math.Max(0, enu[0]+enu[4]))
	u := model.DecodedUpdate{
		Recv:    now,
		FeedID:  feedID,
		ICAO:    f.ICAO & 0xFFFFFF,
		MLAT:    true,
		Lat:     model.F(llh.Lat),
		Lon:     model.F(llh.Lon),
		AltGeom: model.F(math.Round(llh.H * mToFt)),
		Msgs:    1,
		Src:     model.SrcMLATAV,
		SigmaM:  model.F(sigma),
		Grade:   f.Grade,
	}
	if f.Flags&model.FixFlagRelativeTime == 0 && f.TUnixNs > 0 {
		u.Recv = time.Unix(0, f.TUnixNs)
	}
	if f.Vel != nil {
		e, n, up := float64(f.Vel[0]), float64(f.Vel[1]), float64(f.Vel[2])
		u.GS = model.F(math.Hypot(e, n) * mpsToKt)
		trk := math.Atan2(e, n) * 180 / math.Pi
		if trk < 0 {
			trk += 360
		}
		u.Track = model.F(trk)
		u.VertRate = model.F(math.Round(up * mpsToFtMin))
	}
	return u
}
