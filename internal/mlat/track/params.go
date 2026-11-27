package track

import (
	"math"

	"airview/internal/mlat/geo"
)

// Params configures a Tracker. The zero value of every field selects the
// documented default (see DefaultParams); Normalize clamps the rest.
type Params struct {
	// Process noise: power spectral densities of the white-noise
	// accelerations. QCV and QCT are the horizontal acceleration PSDs of the
	// constant-velocity and coordinated-turn models (m²/s³), QVert the
	// vertical one shared by both (m²/s³), QOmega the turn-rate
	// acceleration PSD (rad²/s³). OmegaTauS is the correlation time of the
	// turn rate (s): ω is an Ornstein-Uhlenbeck process with stationary
	// σ² = QOmega·OmegaTauS/2 (defaults: 20 s, σ ≈ 1°/s).
	QCV, QCT, QVert, QOmega float64
	OmegaTauS               float64

	// SojournCVS / SojournCTS are the mean times the aircraft stays in the
	// CV / CT mode (s); they set the Markov switching probabilities
	// π_ij(Δt) = 1 − exp(−Δt/τ_i). InitPCT is the CT probability of a new
	// track.
	SojournCVS, SojournCTS float64
	InitPCT                float64

	// Prior of a new track's velocity and turn rate: 1σ horizontal speed
	// (per axis, m/s), vertical rate (m/s) and turn rate (rad/s).
	InitSigmaVelH, InitSigmaVelU, InitSigmaOmega float64

	// GateProb is the probability mass of the Mahalanobis gate (§9.5:
	// 0.999). Iterations is the number of iterated-EKF relinearisations per
	// update (1 = plain EKF).
	GateProb   float64
	Iterations int

	// Lifecycle (§9.5). ConfirmWindowS: a tentative hypothesis must collect
	// ConfirmUpdates position-determining updates (a fix, or a TOA cluster
	// with at least three rows) within this window of its creation, or it is
	// dropped. CoastS: a track without an accepted update for this long is
	// deleted. MaxSigmaHM: a track whose horizontal 1σ exceeds this is
	// deleted.
	ConfirmWindowS float64
	ConfirmUpdates int
	CoastS         float64
	MaxSigmaHM     float64

	// MaxSpeedMS is the horizontal speed a hypothesis may not exceed by
	// more than 3σ (≈ 700 kt default: Mach-aware ceiling for civil traffic
	// at cruise altitude plus wind).
	MaxSpeedMS float64
	// HypLR is the likelihood ratio at which a competing hypothesis of the
	// same ICAO is dropped (3+alt ambiguity, re-acquisition). MaxHyp is the
	// number of hypotheses kept per ICAO. RespawnMisses is the number of
	// consecutive fixes that gate against no hypothesis after which the
	// latest fix starts a new competing hypothesis.
	HypLR         float64
	MaxHyp        int
	RespawnMisses int
	// LOSMarginM: a hypothesis whose position is hidden below the 4/3-earth
	// radio horizon of a station that heard it by more than this many
	// metres is penalised (default 3000 m, lenient against ducting).
	LOSMarginM float64

	// MaxMembers caps the stations used from one cluster (the ones with the
	// smallest σ are kept); at most MaxClusterMembers.
	MaxMembers int
	// MaxExclude is the number of cluster members that may be excluded as
	// outliers when a cluster fails the gate (only while at least three
	// members remain; 0 = default 2, negative = none).
	MaxExclude int
	// MaxLagS: measurements older than the track by more than this are
	// dropped; younger ones are applied as out-of-sequence measurements
	// (DESIGN §9.3: late copies up to 2 s still update the track).
	MaxLagS float64
	// ExcessRefreshM: the cached refraction excess of a station is
	// recomputed when the aircraft moved more than this from where it was
	// computed (its gradient carries it in between).
	ExcessRefreshM float64
	// RecentreM: the local frame is moved under the aircraft when its
	// horizontal offset from the frame origin exceeds this.
	RecentreM float64

	// SyncTauS is the correlation time (s) of the clock-sync errors given in
	// TOA.SyncSigma (default 15 s, about the clock filter's memory).
	SyncTauS float64

	// Atmosphere is the propagation model of the optical range (zero value
	// = geo.DefaultAtmosphere()). The engine passes the network's k_atm/N0.
	Atmosphere geo.Atmosphere
}

// MaxClusterMembers is the hard cap on stations used from one cluster.
const MaxClusterMembers = 32

// DefaultParams returns the default parameters.
func DefaultParams() Params {
	return Params{
		QCV: 0.3, QCT: 0.5, QVert: 1, QOmega: 3e-5, OmegaTauS: 20,
		SojournCVS: 300, SojournCTS: 20, InitPCT: 0.1,
		InitSigmaVelH: 150, InitSigmaVelU: 15, InitSigmaOmega: 0.03,
		GateProb: 0.999, Iterations: 3,
		ConfirmWindowS: 10, ConfirmUpdates: 2, CoastS: 60, MaxSigmaHM: 5000,
		MaxSpeedMS: 360, HypLR: 100, MaxHyp: 4, RespawnMisses: 3, LOSMarginM: 3000,
		MaxMembers: 24, MaxExclude: 2, MaxLagS: 2.5, ExcessRefreshM: 2000, RecentreM: 5000, SyncTauS: 15,
		Atmosphere: geo.DefaultAtmosphere(),
	}
}

// Normalize fills zero fields with defaults and clamps out-of-range values.
func (p Params) Normalize() Params {
	d := DefaultParams()
	pos := func(v *float64, def, lo, hi float64) {
		if !(*v > 0) || math.IsInf(*v, 0) {
			*v = def
		}
		*v = math.Min(math.Max(*v, lo), hi)
	}
	pos(&p.QCV, d.QCV, 1e-4, 100)
	pos(&p.QCT, d.QCT, 1e-4, 1000)
	pos(&p.QVert, d.QVert, 1e-4, 100)
	pos(&p.QOmega, d.QOmega, 1e-8, 1e-1)
	pos(&p.OmegaTauS, d.OmegaTauS, 1, 3600)
	pos(&p.SojournCVS, d.SojournCVS, 1, 3600)
	pos(&p.SojournCTS, d.SojournCTS, 1, 3600)
	pos(&p.InitPCT, d.InitPCT, 0.01, 0.99)
	pos(&p.InitSigmaVelH, d.InitSigmaVelH, 1, 1000)
	pos(&p.InitSigmaVelU, d.InitSigmaVelU, 0.1, 200)
	pos(&p.InitSigmaOmega, d.InitSigmaOmega, 1e-4, 1)
	pos(&p.GateProb, d.GateProb, 0.5, 0.999999)
	if p.Iterations <= 0 {
		p.Iterations = d.Iterations
	}
	p.Iterations = min(p.Iterations, 10)
	pos(&p.ConfirmWindowS, d.ConfirmWindowS, 0.5, 600)
	if p.ConfirmUpdates <= 0 {
		p.ConfirmUpdates = d.ConfirmUpdates
	}
	pos(&p.CoastS, d.CoastS, 1, 3600)
	pos(&p.MaxSigmaHM, d.MaxSigmaHM, 100, 1e6)
	pos(&p.MaxSpeedMS, d.MaxSpeedMS, 50, 2000)
	pos(&p.HypLR, d.HypLR, 2, 1e9)
	if p.MaxHyp <= 0 {
		p.MaxHyp = d.MaxHyp
	}
	p.MaxHyp = min(p.MaxHyp, maxHyp)
	if p.RespawnMisses <= 0 {
		p.RespawnMisses = d.RespawnMisses
	}
	pos(&p.LOSMarginM, d.LOSMarginM, 0, 1e5)
	if p.MaxMembers <= 0 {
		p.MaxMembers = d.MaxMembers
	}
	p.MaxMembers = min(max(p.MaxMembers, 2), MaxClusterMembers)
	switch {
	case p.MaxExclude == 0:
		p.MaxExclude = d.MaxExclude
	case p.MaxExclude < 0:
		p.MaxExclude = 0
	}
	p.MaxExclude = min(p.MaxExclude, 4)
	pos(&p.MaxLagS, d.MaxLagS, 0.01, 30)
	pos(&p.ExcessRefreshM, d.ExcessRefreshM, 10, 1e5)
	pos(&p.RecentreM, d.RecentreM, 100, 1e5)
	pos(&p.SyncTauS, d.SyncTauS, 0.1, 3600)
	if p.Atmosphere == (geo.Atmosphere{}) {
		p.Atmosphere = geo.DefaultAtmosphere()
	}
	p.Atmosphere = p.Atmosphere.Normalize()
	return p
}
