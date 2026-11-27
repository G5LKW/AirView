package clock

import (
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

// Member is one station's timestamp of a sync transmission.
type Member struct {
	Station model.StationIdx
	Epoch   uint32
	Ticks   uint64 // unwrapped receiver ticks in units of the class's NominalHz
}

// SyncObs is one transmission of a reference aircraft heard by several
// stations: the input of the clock filter (DESIGN §9.2.2). The engine builds
// it from an ingest group and the reference track (mlat/ref); ObserveGroup
// does that for a RefSource.
type SyncObs struct {
	// Key is the aircraft address key (for the per-aircraft blacklist).
	Key uint32
	// Kind selects the frame reference and the β_short term
	// (FrameModeSShort or FrameModeSLong).
	Kind model.FrameKind
	// HostNs is the server-clock time of the transmission (Unix ns, e.g.
	// the group's FirstNs): it orders the observation into an epoch and
	// initialises new clocks; milliseconds of error are fine.
	HostNs int64
	// Pos is the transmitter position (ECEF, m) at the transmit instant and
	// PosCov its covariance (upper triangle xx, xy, xz, yy, yz, zz in m²;
	// zero = exact). Both come from the reference track.
	Pos    geo.Vec3
	PosCov [6]float64
	// Members are the stations' timestamps (one per station; at most
	// Config.MaxMembers are used).
	Members []Member
}

// StationInfo describes a station to the filter.
type StationInfo struct {
	// Class is the station's declared clock class (the server may only
	// downgrade it, DESIGN §4.3).
	Class model.ClockClass
	// Pos is the antenna position (ECEF, m; height above the ellipsoid) and
	// Located whether it is known well enough for sync.
	Pos     geo.Vec3
	Located bool
	// NoStatus marks a GPS station that never sends Radarcape status frames
	// (mlat-client radarcape_gps): anchor condition (a) is skipped.
	NoStatus bool
	// Flagged is the admin flag: the station never becomes an anchor
	// (condition (c)), and an anchor is demoted.
	Flagged bool
	// JitterNs overrides the class's prior timestamp jitter (0 = prior);
	// mlat/quality feeds its learned value back here.
	JitterNs float64
	// PosSigmaM is the 1σ of Pos (m; 0 = exact). The part above
	// Config.PosSigmaFreeM is added, as (σ/c)², to the variance of every
	// observation of the station, so a station whose position is doubtful
	// (a declared position the survey contradicted: σ inflated by the
	// discrepancy) no longer pulls the clocks of honest stations and anchors
	// (DESIGN §10.1; finding MLAT-R4).
	PosSigmaM float64
}

// Event is a new receiver clock epoch (AVF CLOCK_EVENT, Beast epoch change,
// mlat-client clock_reset).
type Event struct {
	OldEpoch, NewEpoch uint32
	Reason             model.ClockEventReason
	SamplesLost        int64
	HostNs             int64
}

// GPSStatus is the GPS state of a Radarcape-like station from its status
// frames.
type GPSStatus struct {
	HostNs int64
	// Locked: GPS timestamps valid (antenna OK, ≥ 3 satellites, FPGA
	// synchronised, timestamp not bad). UTC: the time base is UTC.
	Locked, UTC bool
}

// RefSource supplies reference-track positions for sync observations
// (implemented by mlat/ref). It must be safe for concurrent use.
type RefSource interface {
	// RefPosition returns the transmitter position (ECEF) and its
	// covariance (upper triangle, m²) of aircraft key for a transmission of
	// downlink format df at server time tUnixNs; ok is false when the
	// aircraft is not usable for sync (DESIGN §9.2.2: NACp ≥ 8, NIC ≥ 7,
	// valid CRC, position age ≤ 1 s once propagated).
	RefPosition(key uint32, df uint8, tUnixNs int64) (pos geo.Vec3, cov [6]float64, ok bool)
	// Usable is the cheap form of the same test for the grouper (no time:
	// "has a good reference track now").
	Usable(key uint32) bool
}

// SyncDF reports whether frames of downlink format df may be used for clock
// sync: DF17/18 squitters and the DF0/4/5/11 replies of ADS-B aircraft
// (which make β_short observable), DESIGN §9.2.2.
func SyncDF(df uint8) bool {
	switch df {
	case 0, 4, 5, 11, 17, 18:
		return true
	}
	return false
}
