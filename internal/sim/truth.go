package sim

import (
	"strings"
	"time"

	"airview/internal/model"
)

// Truth is the ground truth of one simulated aircraft at a virtual time.
// Units follow the model conventions: feet, knots, ft/min, degrees true.
type Truth struct {
	ICAO        uint32
	Callsign    string // trimmed
	Squawk      string // four octal digits
	Category    string // emitter category, e.g. "A3"
	Kind        string // "overflight", "arrival", "departure", "ga", "helicopter", "military"
	Phase       string // "enroute", "climbout", "hold", "final", "flare", "hover", "taxi-out", "line-up", "takeoff", "rollout", "taxi-in", "parked"
	ADSB        bool   // transmits extended squitter (positions off-air)
	MLAT        bool   // Mode S only, positioned by synthetic MLAT results
	GNSSAlt     bool   // airborne positions carry GNSS height (TC 20) instead of baro
	ADSBVersion int    // 0 or 2 (meaningful when ADSB)
	Emergency   string // "", "general", "medical", "minfuel", "nordo", "unlawful"

	Lat, Lon float64
	AltBaro  float64 // pressure altitude, ft
	AltGeom  float64 // GNSS height, ft
	OnGround bool
	GS       float64 // kt
	Track    float64 // deg true
	VertRate float64 // ft/min

	TAS         float64 // kt
	IAS         float64 // kt
	Mach        float64
	TrueHeading float64 // deg
	MagHeading  float64 // deg
	Roll        float64 // deg, + = right wing down
	TrackRate   float64 // deg/s
	SelAlt      float64 // MCP/FCU selected altitude, ft

	WindDir float64 // deg true, blowing from, at the aircraft's level
	WindKt  float64
	OAT     float64 // static air temperature, deg C

	DistKm    float64 // ground distance from the station
	Bearing   float64 // deg true from the station
	Elevation float64 // deg above the horizon of the first antenna (refreshed once per second)
	EIRPdBm   float64 // transmitter EIRP
}

var emergencyNames = [...]string{"", "general", "medical", "minfuel", "nordo", "unlawful", "downed", ""}

// truthOf builds the ground truth of a at dt seconds relative to the current
// world time. For -0.2 <= dt < 0 it is evaluated from the state of the step
// the transmission at that time was generated in (frames are received and
// delivered up to one step after they were sent), so it is exactly what they
// were encoded from; for dt >= 0 it comes from the state of the next step.
// Outside that range it is a linear extrapolation.
func (s *Sim) truthOf(a *aircraft, dt float64) Truth {
	k := &a.kin
	switch {
	case dt < -stepDt && dt >= -2*stepDt:
		k = &a.prev2
		dt += 2 * stepDt
	case dt < 0:
		k = &a.prev
		dt += stepDt
	}
	lat, lon, alt := k.at(dt)
	lat = clamp(lat, -90, 90)
	lon = wrap180(lon)
	ad := s.airData(k)
	d, brg := distBearing(s.st.lat, s.st.lon, lat, lon)
	t := Truth{
		ICAO:        a.icao,
		Callsign:    strings.TrimRight(string(a.callsign[:]), " "),
		Squawk:      squawkString(a.squawk),
		Category:    string([]byte{"DCBA"[(a.catTC-1)&3], '0' + a.cat}),
		Kind:        kindNames[a.kind],
		Phase:       phaseNames[a.phase],
		ADSB:        a.adsb,
		MLAT:        a.mlat,
		GNSSAlt:     a.adsb && a.gnssPos,
		ADSBVersion: int(a.version),
		Emergency:   emergencyNames[a.emerg&7],
		Lat:         lat,
		Lon:         lon,
		AltBaro:     alt,
		AltGeom:     alt + s.gnssMinusBaro(alt),
		OnGround:    k.onGround,
		GS:          k.gs,
		Track:       k.trk,
		VertRate:    k.vrate,
		TAS:         k.tas,
		IAS:         ad.ias,
		Mach:        ad.mach,
		TrueHeading: ad.hdgTrue,
		MagHeading:  ad.hdgMag,
		Roll:        k.roll,
		TrackRate:   k.trkRate,
		SelAlt:      a.selAlt(),
		WindDir:     ad.windDir,
		WindKt:      ad.windKt,
		OAT:         ad.oatC,
		DistKm:      d,
		Bearing:     brg,
		Elevation:   a.stElev,
		EIRPdBm:     a.eirp,
	}
	if k.onGround {
		t.VertRate = 0
	}
	return t
}

// Truth returns the ground truth of every simulated aircraft at the current
// virtual time (Now).
func (s *Sim) Truth() []Truth {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Truth, len(s.ac))
	for i := range s.ac {
		out[i] = s.truthOf(&s.ac[i], 0)
	}
	return out
}

// TruthAt returns the ground truth at virtual time t, which should be close
// to Now. It is exact from two 100 ms steps before Now to one step after: a
// sink that asks for the truth at a frame's transmit time (see TransmitTime)
// while handling that frame gets precisely the state (position, altitude,
// speed, track, vertical rate, roll, airspeeds ...) the frame was encoded
// from. Further out the position is a linear extrapolation. Identity,
// squawk, emergency and flight phase are always the current ones.
//
// Positions with the TIME bit set (T=1, UTC-coupled) apply to a 0.2 s UTC
// epoch instead (see the package documentation).
func (s *Sim) TruthAt(t time.Time) []Truth {
	s.mu.Lock()
	defer s.mu.Unlock()
	dt := float64(t.UnixNano()-(s.epochNs+s.worldNs)) / 1e9
	out := make([]Truth, len(s.ac))
	for i := range s.ac {
		out[i] = s.truthOf(&s.ac[i], dt)
	}
	return out
}

// TransmitTime estimates when a received frame was sent: its GPS time
// stamp (end of frame, on the UTC day of Recv) minus the frame duration. The
// radio propagation time (up to ~2 ms) is not removed. Frames without a GPS
// time stamp (MLAT results) return Recv.
func TransmitTime(f model.RawFrame) time.Time {
	if f.GPSNs < 0 || f.Recv.IsZero() {
		return f.Recv
	}
	day := f.Recv.UTC().Truncate(24 * time.Hour)
	t := day.Add(time.Duration(f.GPSNs))
	if t.After(f.Recv) {
		t = t.Add(-24 * time.Hour)
	}
	switch f.Kind {
	case model.FrameModeSLong:
		t = t.Add(-120 * time.Microsecond)
	case model.FrameModeSShort:
		t = t.Add(-64 * time.Microsecond)
	}
	return t
}

// TruthOf returns the ground truth of one aircraft at virtual time t (see
// TruthAt). It reports false when no such aircraft is currently simulated.
func (s *Sim) TruthOf(icao uint32, t time.Time) (Truth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dt := float64(t.UnixNano()-(s.epochNs+s.worldNs)) / 1e9
	for i := range s.ac {
		if s.ac[i].icao == icao {
			return s.truthOf(&s.ac[i], dt), true
		}
	}
	return Truth{}, false
}

// Airport is one of the synthetic airports of the simulated world.
type Airport struct {
	Lat, Lon      float64
	ElevFt        float64
	RunwayHeading float64 // landing and take-off direction, deg true
}

// Airports returns the synthetic airports traffic arrives at and departs from.
func (s *Sim) Airports() []Airport {
	out := make([]Airport, len(s.apts))
	for i, ap := range s.apts {
		out[i] = Airport{Lat: ap.lat, Lon: ap.lon, ElevFt: ap.elevFt, RunwayHeading: ap.rwyHdg}
	}
	return out
}
