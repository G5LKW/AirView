package federation

import (
	"strings"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/privacy"
	"airview/internal/privacy/publicdto"
)

// FeedPrefix starts the FeedID of every update this package delivers to a
// model.FrameSink. Aircraft whose Feeds all carry it were heard only through
// federation and are never exported again (loop prevention when the engine
// injects remote aircraft into its tracker).
const FeedPrefix = "fed:"

// SrcMLATFed is the DecodedUpdate.Src of MLAT results received from a
// federated network (beside model.SrcMLATAV and model.SrcMLATExt).
const SrcMLATFed = "mlat-fed"

// local is one aircraft of the local picture in public form.
type local struct {
	a      publicdto.Aircraft
	key    uint32 // addr | nonICAO<<24
	island bool   // the island guard flags it
}

// aircraftKey returns the store key of an ICAO string.
func aircraftKey(icao string) (uint32, bool) {
	addr, non, ok := avf.ParseICAO(icao)
	if !ok {
		return 0, false
	}
	if non {
		addr |= 1 << 24
	}
	return addr, true
}

// fedOnly reports whether every feed that heard a is a federation feed.
func fedOnly(a *model.Aircraft) bool {
	if len(a.Feeds) == 0 {
		return false
	}
	for _, f := range a.Feeds {
		if !strings.HasPrefix(f, FeedPrefix) {
			return false
		}
	}
	return true
}

// prepare converts the local picture to its public form once per export:
// publicdto.FromAircraft is a whitelist, so no receiver field (feeds,
// stations, distance, RSSI, ...) survives. Predicted (coasting) positions
// are replaced by the last real one; aircraft heard only through
// federation are left out.
func prepare(s *model.LiveSnapshot, guard publicdto.Guard, dst []local) []local {
	dst = dst[:0]
	if s == nil {
		return dst
	}
	for i := range s.Aircraft {
		m := &s.Aircraft[i]
		if fedOnly(m) {
			continue
		}
		key, ok := aircraftKey(m.ICAO)
		if !ok {
			continue
		}
		l := local{a: publicdto.FromAircraft(m), key: key}
		if guard != nil && guard(m) {
			l.island = true
		}
		a := &l.a
		if a.Predicted {
			a.Lat, a.Lon = a.LastRealLat, a.LastRealLon
			if a.CoastS != nil {
				a.PosAge = a.CoastS
			}
		}
		a.Predicted, a.CoastS, a.PredSigmaM, a.LastRealLat, a.LastRealLon = false, nil, nil, nil, nil
		// Reference data and routes are not exchanged: the receiver
		// enriches from its own tables.
		a.Registration, a.TypeCode, a.TypeDesc, a.Operator, a.Country, a.CountryCode, a.Route = "", "", "", "", "", "", nil
		dst = append(dst, l)
	}
	return dst
}

// apply filters and shapes one aircraft for a destination with policy p.
// island is the guard verdict of the local picture (false for remote data,
// which the origin network already processed). It returns false when the
// aircraft must not be sent; otherwise dst holds the shaped copy.
func (p *Policy) apply(src *publicdto.Aircraft, island bool, dst *publicdto.Aircraft) bool {
	if p.Aircraft == ShareNone {
		return false
	}
	if p.NoMilitary && src.Military {
		return false
	}
	if island && p.Island == IslandDrop {
		return false
	}
	ground := src.OnGround != nil && *src.OnGround
	if p.NoGround && ground {
		return false
	}
	if p.MinAltFt > 0 {
		alt := src.AltBaro
		if alt == nil {
			alt = src.AltGeom
		}
		if ground || alt == nil || *alt < float64(p.MinAltFt) {
			return false
		}
	}
	hasPos := src.Lat != nil && src.Lon != nil
	if len(p.Boxes) > 0 {
		if !hasPos {
			return false
		}
		in := false
		for _, b := range p.Boxes {
			if b.Contains(*src.Lat, *src.Lon) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	if hasPos {
		for _, b := range p.ExcludeBoxes {
			if b.Contains(*src.Lat, *src.Lon) {
				return false
			}
		}
	}
	*dst = *src
	if p.Aircraft == ShareNoMLAT && dst.Source == model.SrcMLAT {
		dst.Lat, dst.Lon, dst.PosAge = nil, nil, nil
		dst.MLATGrade, dst.MLATSigmaM = "", nil
		dst.Source = model.SrcModeS
	}
	if island && !dst.Coarse {
		dst.Coarsen(privacy.IslandGridDeg)
	}
	return true
}

// accept applies a receive policy to a decoded aircraft.
func (r *ReceivePolicy) accept(a *publicdto.Aircraft) bool {
	if r.Aircraft == ShareNone {
		return false
	}
	if len(r.Boxes) == 0 {
		return true
	}
	if a.Lat == nil || a.Lon == nil {
		return false
	}
	for _, b := range r.Boxes {
		if b.Contains(*a.Lat, *a.Lon) {
			return true
		}
	}
	return false
}

// fixAllowed applies the position filters of p to an MLAT fix.
func (p *Policy) fixAllowed(f *Fix) bool {
	if p.Aircraft == ShareNone || p.Aircraft == ShareNoMLAT {
		return false
	}
	if p.MinAltFt > 0 && f.AltM*3.28084 < float64(p.MinAltFt) {
		return false
	}
	if len(p.Boxes) > 0 {
		in := false
		for _, b := range p.Boxes {
			if b.Contains(f.Lat, f.Lon) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	for _, b := range p.ExcludeBoxes {
		if b.Contains(f.Lat, f.Lon) {
			return false
		}
	}
	return true
}

func (r *ReceivePolicy) acceptFix(f *Fix) bool {
	if r.MLAT == ShareNone {
		return false
	}
	if len(r.Boxes) == 0 {
		return true
	}
	for _, b := range r.Boxes {
		if b.Contains(f.Lat, f.Lon) {
			return true
		}
	}
	return false
}
