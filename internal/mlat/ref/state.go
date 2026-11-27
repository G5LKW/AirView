package ref

import (
	"math"
	"strings"

	"airview/internal/mlat/geo"
)

// Flags describe a reference track and a queried state.
type Flags uint32

const (
	// FlagConfirmed: ConfirmPositions accepted positions and a velocity.
	FlagConfirmed Flags = 1 << iota
	// FlagT1: positions are UTC-coupled (T=1) and tagged with their 0.2 s
	// epoch (no latency estimate needed).
	FlagT1
	// FlagNUCp: NACp is not known from an operational status / target
	// state message and was derived from the position type code.
	FlagNUCp
	// FlagOnGround: the aircraft reports surface positions.
	FlagOnGround
	// FlagProbation: recently re-initialised after a jump, a T=1 timing
	// fault or velocity inconsistency.
	FlagProbation
	// FlagSuspended: suspended by Suspend (e.g. TDOA verification failed,
	// DESIGN §9.2.4 blacklist) or after instability.
	FlagSuspended
	// FlagUnstable: repeated jumps or interleaved inconsistent positions
	// (duplicate address, spoofer).
	FlagUnstable
	// FlagVelInconsistent: reported velocities contradict the positions.
	FlagVelInconsistent
	// FlagBeyondHorizon: the position is below the radio horizon of a
	// receiver that heard it (CheckHeard).
	FlagBeyondHorizon
	// FlagT1Untrusted: T=1 reports were sent too far from their epoch; the
	// aircraft's positions are tagged as T=0.
	FlagT1Untrusted
	// FlagLatencyEstimated: the T=0 position latency was refined by
	// ObserveFix.
	FlagLatencyEstimated
	// FlagExtrapolated: the queried instant is after the newest
	// measurement (forward prediction).
	FlagExtrapolated
	// FlagBackward: the queried instant is before the oldest measurement
	// kept.
	FlagBackward
	// FlagStale: the nearest position report is more than MaxPosAgeS away.
	FlagStale
	// FlagLowQuality: NACp or NIC below the sync gate, or unknown.
	FlagLowQuality
	// FlagNoAltitude: no usable height (vertical σ above MaxVertSigmaM).
	FlagNoAltitude
)

// blocking are the flags that make a state unusable as a sync reference.
const blocking = FlagOnGround | FlagProbation | FlagSuspended | FlagUnstable |
	FlagVelInconsistent | FlagBeyondHorizon | FlagStale | FlagLowQuality | FlagNoAltitude | FlagBackward

var flagNames = [...]string{"confirmed", "t1", "nucp", "ground", "probation", "suspended", "unstable",
	"vel-inconsistent", "beyond-horizon", "t1-untrusted", "latency-estimated", "extrapolated",
	"backward", "stale", "low-quality", "no-altitude"}

// String lists the set flags.
func (f Flags) String() string {
	var b strings.Builder
	for i, n := range flagNames {
		if f&(1<<uint(i)) != 0 {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(n)
		}
	}
	return b.String()
}

// State is a reference aircraft's state at a queried instant (normally the
// transmit instant of one of its messages).
type State struct {
	ICAO uint32
	TNs  int64 // the queried instant
	// Pos and Vel are ECEF (m, m/s); Lat/Lon/HAE the same position.
	Pos           geo.Vec3
	Vel           geo.Vec3
	Lat, Lon, HAE float64
	// Cov is the total ECEF position covariance (m², row-major 3×3): the
	// smoother's covariance, the NACp/altitude bias of the reports, the
	// latency uncertainty and the queried instant's uncertainty along the
	// velocity. CovENU is the same in the local east-north-up frame.
	Cov    [9]float64
	CovENU [9]float64
	// SigmaFilterH is the horizontal 1σ (m, RMS of east and north) of the
	// smoother alone, SigmaBiasH the NACp bias 1σ per horizontal axis and
	// SigmaV the total vertical 1σ.
	SigmaFilterH float64
	SigmaBiasH   float64
	SigmaV       float64
	// PosAgeS is the time from the instant to the nearest position report.
	PosAgeS float64
	// LatencyS is the position latency applied (T=0), LatencySigmaS its 1σ.
	LatencyS      float64
	LatencySigmaS float64
	NACp, NIC     uint8
	Version       uint8
	AltSrc        AltSource
	Flags         Flags
}

// Usable reports whether the state passes the sync-reference gates of
// DESIGN §9.2.2 (NACp, NIC, position age, track health).
func (s *State) Usable() bool { return s.Flags&FlagConfirmed != 0 && s.Flags&blocking == 0 }

// Blocking returns the flags that make the state unusable (0 = usable,
// apart from confirmation).
func (s *State) Blocking() Flags { return s.Flags & blocking }

// SigmaH returns the total horizontal 1σ (RMS of east and north).
func (s *State) SigmaH() float64 { return math.Sqrt((s.CovENU[0] + s.CovENU[4]) / 2) }

// Info is a diagnostic summary of a track.
type Info struct {
	ICAO          uint32  `json:"icao"`
	Nodes         int     `json:"nodes"`
	Positions     uint64  `json:"positions"`
	Velocities    uint64  `json:"velocities"`
	Rejected      uint64  `json:"rejected"`
	VelRejected   uint64  `json:"velRejected"`
	Jumps         int     `json:"jumps"`
	NACp          uint8   `json:"nacp"`
	NIC           uint8   `json:"nic"`
	Version       uint8   `json:"version"`
	LatencyS      float64 `json:"latencyS"`
	LatencySigmaS float64 `json:"latencySigmaS"`
	LastSeenNs    int64   `json:"lastSeenNs"`
	Flags         Flags   `json:"flags"`
	FlagNames     string  `json:"flagNames"`
}
