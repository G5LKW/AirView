package trust

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"airview/internal/geoid"
	"airview/internal/model"
	"airview/internal/registry"
)

// RegistrySource adapts the station registry to Stations: the timing state
// from registry.Station.Timing and the effective position.
type RegistrySource struct{ R *registry.Registry }

// TrustInfo implements Stations.
func (s RegistrySource) TrustInfo(st model.StationIdx) (StationInfo, bool) {
	r, ok := s.R.Station(st)
	if !ok {
		return StationInfo{}, false
	}
	in := StationInfo{State: Trusted}
	switch r.Timing() {
	case registry.TimingQuarantined:
		in.State = Quarantined
	case registry.TimingExcluded:
		in.State = Excluded
	}
	switch {
	case r.Located:
		in.Located, in.Lat, in.Lon, in.HAE = true, r.Effective.Lat, r.Effective.Lon, r.Effective.HAE
	case r.GNSS.Valid:
		in.Located, in.Lat, in.Lon, in.HAE = true, r.GNSS.Lat, r.GNSS.Lon, r.GNSS.Alt
	case r.Declared.Valid:
		// mlat-client handshakes set only the declared position, which the
		// MLAT engine also uses (internal/engine mlatStationInfo); without
		// this fallback such stations could never be judged and proven.
		d := r.Declared
		hae := d.Alt
		if r.DeclaredAltRef != model.AltRefHAE {
			if h, ok := geoid.MSLToHAE(d.Lat, d.Lon, d.Alt); ok {
				hae = h
				if r.DeclaredAltRef == model.AltRefAmbiguous {
					hae = (h + d.Alt) / 2
				}
			}
		}
		in.Located, in.Lat, in.Lon, in.HAE = true, d.Lat, d.Lon, hae
	}
	return in, true
}

// RegistryPromoter returns the promote callback for New: it ends the
// station's quarantine in the registry (Registry.ProveTiming, audited) on
// its own goroutine.
func RegistryPromoter(reg *registry.Registry, log *slog.Logger) func(model.StationIdx, Report) {
	if log == nil {
		log = slog.Default()
	}
	return func(st model.StationIdx, rep Report) {
		detail := fmt.Sprintf(`{"windows":%d,"good":%d,"refs":%d,"sigmaNs":%.0f}`, rep.Windows, rep.Good, rep.Refs, rep.LastSigmaNs)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := reg.ProveTiming(ctx, st, detail); err != nil {
				log.Warn("timing proof not stored", "station", uint32(st), "err", err)
				return
			}
			log.Info("station timing proven, joins MLAT", "station", uint32(st), "windows", rep.Windows, "refs", rep.Refs)
		}()
	}
}
