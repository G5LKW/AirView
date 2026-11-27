package trajectory

import (
	"time"
)

// Source identifies where a position measurement came from. It selects the
// default measurement noise.
type Source uint8

// Position sources.
const (
	SrcUnknown Source = iota
	SrcADSB           // DF17 extended squitter (default 1-sigma from NACp, floor SigmaADSBM)
	SrcMLAT           // multilateration (SigmaMLATM)
	SrcTISB           // TIS-B / ADS-R rebroadcast (SigmaTISBM)
	SrcOther          // SBS / JSON feed without detail (SigmaOtherM)
)

// String returns the model.Src* style label.
func (s Source) String() string {
	switch s {
	case SrcADSB:
		return "adsb"
	case SrcMLAT:
		return "mlat"
	case SrcTISB:
		return "tisb"
	case SrcOther:
		return "other"
	}
	return "unknown"
}

// SourceFromString maps model.Src* labels ("adsb", "mlat", "tisb", "adsr",
// "other") to a Source.
func SourceFromString(s string) Source {
	switch s {
	case "adsb":
		return SrcADSB
	case "mlat":
		return SrcMLAT
	case "tisb", "adsr":
		return SrcTISB
	case "other", "modes":
		return SrcOther
	}
	return SrcUnknown
}

// Field is a bit set telling which fields of an Obs are valid.
type Field uint32

// Obs field bits.
const (
	HasPos        Field = 1 << iota // Lat, Lon, Src (+ NACp / AccuracyM if their bits are set)
	HasVel                          // GSKt + TrackDeg (ground velocity vector)
	HasVRate                        // VRateFpm
	HasAlt                          // AltFt (barometric preferred)
	HasRoll                         // RollDeg
	HasTrackRate                    // TrackRateDps
	HasTAS                          // TASKt
	HasMagHeading                   // MagHeadingDeg
	HasSelAlt                       // SelAltFt
	HasSelHeading                   // SelHeadingDeg (magnetic, as broadcast)
	HasNavModes                     // NavModes
	HasGround                       // OnGround
	HasWind                         // WindDirDeg (FROM) + WindKt
	HasNACp                         // NACp
	HasNACv                         // NACv
	HasAccuracy                     // AccuracyM (explicit 1-sigma horizontal position accuracy)
)

// NavMode is a bit set of autopilot / navigation modes (ADS-B TC29 v2, BDS 4,0).
type NavMode uint8

// Nav mode bits.
const (
	NavAutopilot NavMode = 1 << iota
	NavVNAV
	NavAltHold
	NavApproach
	NavLNAV
	NavTCAS
)

// NavModesFromStrings converts model.Aircraft.NavModes strings
// ("autopilot","vnav","althold","approach","lnav","tcas") to a NavMode set.
func NavModesFromStrings(ss []string) NavMode {
	var m NavMode
	for _, s := range ss {
		switch s {
		case "autopilot":
			m |= NavAutopilot
		case "vnav":
			m |= NavVNAV
		case "althold":
			m |= NavAltHold
		case "approach":
			m |= NavApproach
		case "lnav":
			m |= NavLNAV
		case "tcas":
			m |= NavTCAS
		}
	}
	return m
}

// Obs is one timestamped observation of an aircraft. Only fields whose bit is
// set in Has are read. Units follow the AirView API (ft, kt, ft/min, deg true).
// An Obs is plain data (no pointers): build it on the stack per frame.
type Obs struct {
	T   time.Time // time of applicability (receiver timestamp when known)
	Has Field

	Lat, Lon  float64 // deg
	Src       Source
	NACp      int8    // 0..11
	NACv      int8    // 0..4
	AccuracyM float64 // explicit 1-sigma horizontal accuracy in metres (overrides NACp/Src)

	GSKt     float64 // ground speed
	TrackDeg float64 // true track over ground
	VRateFpm float64 // vertical rate (baro or geometric)
	AltFt    float64 // barometric altitude
	AltQ100  bool    // altitude is Gillham-coded (100 ft steps): larger noise

	RollDeg       float64 // + = right wing down
	TrackRateDps  float64 // + = turning right (clockwise)
	TASKt         float64 // true airspeed
	MagHeadingDeg float64 // magnetic heading
	SelAltFt      float64 // MCP/FCU or FMS selected altitude
	SelHeadingDeg float64 // selected heading (magnetic)
	NavModes      NavMode
	OnGround      bool
	WindDirDeg    float64 // direction the wind blows FROM, deg true
	WindKt        float64
}

// SetPos sets a position measurement.
func (o *Obs) SetPos(lat, lon float64, src Source) {
	o.Lat, o.Lon, o.Src = lat, lon, src
	o.Has |= HasPos
}

// SetVel sets a ground velocity measurement.
func (o *Obs) SetVel(gsKt, trackDeg float64) {
	o.GSKt, o.TrackDeg = gsKt, trackDeg
	o.Has |= HasVel
}

// SetAlt sets a barometric altitude.
func (o *Obs) SetAlt(altFt float64) {
	o.AltFt = altFt
	o.Has |= HasAlt
}

// SetVRate sets a vertical rate.
func (o *Obs) SetVRate(fpm float64) {
	o.VRateFpm = fpm
	o.Has |= HasVRate
}

// Config tunes a Manager. The zero value of any field means "use the default"
// (see DefaultConfig).
type Config struct {
	// MaxCoast is the hard limit for showing a predicted position after the
	// last real position (default 60 s).
	MaxCoast time.Duration
	// CoastAfter marks a prediction as "coasting" (UI glow) when the last real
	// position is older than this (default 4 s).
	CoastAfter time.Duration
	// StaleAfter removes an aircraft that has not been observed for this long (default 180 s).
	StaleAfter time.Duration
	// ReinitGap restarts the filter when positions resume after this gap (default 90 s).
	ReinitGap time.Duration
	// MaxTracks bounds memory (default 2000). New aircraft beyond it are ignored.
	MaxTracks int
	// OOSMaxLag is how much older than the filter state an observation may be
	// and still be used (shifted, with inflated noise). Older ones are dropped (default 2 s).
	OOSMaxLag time.Duration

	RecenterKm float64 // re-centre the tangent plane beyond this distance (default 20)

	SigmaADSBM  float64 // floor of the ADS-B 1-sigma position noise, m (default 15)
	SigmaMLATM  float64 // MLAT 1-sigma, m (default 120)
	SigmaTISBM  float64 // TIS-B/ADS-R 1-sigma, m (default 150)
	SigmaOtherM float64 // unknown-source 1-sigma, m (default 50)
	SigmaVelMS  float64 // ADS-B velocity 1-sigma per axis, m/s, when NACv unknown (default 1.0)
	SigmaAltFt  float64 // 25 ft-coded altitude 1-sigma (default 15); Gillham uses 3x

	GateChi2 float64 // Mahalanobis gate for 2-D measurements (default 13.8 = 99.9 %)
	// ResetAfterRejects re-initialises a track after this many consecutive
	// gated-out positions (default 4).
	ResetAfterRejects int

	QCV    float64 // CV model white-acceleration spectral density, m^2/s^3 (default 0.05)
	QCT    float64 // CT model white-acceleration spectral density, m^2/s^3 (default 0.5)
	QOmega float64 // CT turn-rate random walk, (rad/s)^2/s (default 4e-5)
	QVert  float64 // vertical channel white-acceleration density, ft^2/s^3 (default 9)
	TauCVs float64 // mean sojourn in CV before switching, s (default 60)
	TauCTs float64 // mean sojourn in CT, s (default 20)
	// Prediction-only noise. A straight-flying aircraft gets QOmegaPred (turn
	// onset) only; one that has been manoeuvring (EWMA "activity" of the
	// filtered turn rate over ActivityTau) additionally gets activity*QOmega
	// and an isotropic activity*QManeuver white acceleration, so the predicted
	// 1-sigma stays calibrated in both regimes (see Stats Calib95).
	QOmegaPred  float64 // (rad/s)^2/s (default 1e-7)
	QManeuver   float64 // m^2/s^3 at activity 1 (default 60)
	ActivityTau float64 // s (default 180)
	TurnTau     float64 // predicted turn-rate decay time constant, s (default 20)
	// TurnHoldSec keeps the turn rate constant this long before it starts to decay (default 0).
	TurnHoldSec   float64
	AccelTau      float64 // predicted along-track acceleration decay, s (default 60)
	LevelOffTau   float64 // altitude capture time constant, s (default 10)
	CeilingFt     float64 // plausible climb limit when no selected altitude (default 45000)
	MaxSpeedMS    float64 // plausible speed limit (default 340 m/s)
	SnapshotEvery time.Duration
	// ScoreMaxSigmaM: only real positions at least this accurate (1-sigma) are
	// used to score earlier predictions (default 60 m; excludes MLAT noise).
	ScoreMaxSigmaM float64
	StatsWindow    int // rolling window per statistics cell (default 2048 samples)

	// Manoeuvre floor of the reported prediction uncertainty. The filter
	// covariance alone assumes the aircraft keeps doing what it did (about
	// 10 m after 20 s of straight flight), which is far too confident for an
	// aircraft that may turn while out of coverage. The reported 1-sigma
	// (SigmaHM / Ellipse of Predict, Predicted and PathFor) therefore adds,
	// root-sum-square, the displacement of a turn hypothesis started at the
	// last filter update: a turn at CoastTurnDps, limited to CoastBankDeg of
	// bank at the current speed and to at least CoastMinAccel lateral
	// acceleration. Self-verification scoring (Stats) and smoothing use the
	// raw filter covariance and are not affected.
	CoastTurnDps  float64 // hypothesised turn rate, deg/s (default 3 = standard rate)
	CoastBankDeg  float64 // bank limit of that turn, deg (default 25)
	CoastMinAccel float64 // minimum hypothesised lateral acceleration, m/s^2 (default 1.2)

	// Now is the clock used by Run (default time.Now). It must be the time
	// base of the observation times (Obs.T) and prediction times.
	Now func() time.Time
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		MaxCoast:          60 * time.Second,
		CoastAfter:        4 * time.Second,
		StaleAfter:        180 * time.Second,
		ReinitGap:         90 * time.Second,
		MaxTracks:         2000,
		OOSMaxLag:         2 * time.Second,
		RecenterKm:        20,
		SigmaADSBM:        15,
		SigmaMLATM:        120,
		SigmaTISBM:        150,
		SigmaOtherM:       50,
		SigmaVelMS:        1.0,
		SigmaAltFt:        15,
		GateChi2:          13.8,
		ResetAfterRejects: 4,
		QCV:               0.05,
		QCT:               0.5,
		QOmega:            4e-5,
		QVert:             9,
		TauCVs:            60,
		TauCTs:            20,
		QOmegaPred:        1e-7,
		QManeuver:         60,
		ActivityTau:       180,
		TurnTau:           20,
		TurnHoldSec:       0,
		AccelTau:          60,
		LevelOffTau:       10,
		CeilingFt:         45000,
		MaxSpeedMS:        340,
		SnapshotEvery:     5 * time.Second,
		ScoreMaxSigmaM:    60,
		StatsWindow:       2048,
		CoastTurnDps:      3,
		CoastBankDeg:      25,
		CoastMinAccel:     1.2,
		Now:               time.Now,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxCoast <= 0 {
		c.MaxCoast = d.MaxCoast
	}
	if c.CoastAfter <= 0 {
		c.CoastAfter = d.CoastAfter
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = d.StaleAfter
	}
	if c.StaleAfter < c.MaxCoast {
		c.StaleAfter = c.MaxCoast
	}
	if c.ReinitGap <= 0 {
		c.ReinitGap = d.ReinitGap
	}
	if c.MaxTracks <= 0 {
		c.MaxTracks = d.MaxTracks
	}
	if c.OOSMaxLag <= 0 {
		c.OOSMaxLag = d.OOSMaxLag
	}
	pos := func(v *float64, def float64) {
		if !(*v > 0) {
			*v = def
		}
	}
	pos(&c.RecenterKm, d.RecenterKm)
	pos(&c.SigmaADSBM, d.SigmaADSBM)
	pos(&c.SigmaMLATM, d.SigmaMLATM)
	pos(&c.SigmaTISBM, d.SigmaTISBM)
	pos(&c.SigmaOtherM, d.SigmaOtherM)
	pos(&c.SigmaVelMS, d.SigmaVelMS)
	pos(&c.SigmaAltFt, d.SigmaAltFt)
	pos(&c.GateChi2, d.GateChi2)
	pos(&c.QCV, d.QCV)
	pos(&c.QCT, d.QCT)
	pos(&c.QOmega, d.QOmega)
	pos(&c.QVert, d.QVert)
	pos(&c.TauCVs, d.TauCVs)
	pos(&c.TauCTs, d.TauCTs)
	pos(&c.QOmegaPred, d.QOmegaPred)
	pos(&c.QManeuver, d.QManeuver)
	pos(&c.ActivityTau, d.ActivityTau)
	pos(&c.TurnTau, d.TurnTau)
	pos(&c.AccelTau, d.AccelTau)
	pos(&c.LevelOffTau, d.LevelOffTau)
	pos(&c.CeilingFt, d.CeilingFt)
	pos(&c.MaxSpeedMS, d.MaxSpeedMS)
	pos(&c.ScoreMaxSigmaM, d.ScoreMaxSigmaM)
	pos(&c.CoastTurnDps, d.CoastTurnDps)
	pos(&c.CoastBankDeg, d.CoastBankDeg)
	pos(&c.CoastMinAccel, d.CoastMinAccel)
	if c.CoastBankDeg > 60 {
		c.CoastBankDeg = 60
	}
	if c.TurnHoldSec < 0 || c.TurnHoldSec != c.TurnHoldSec {
		c.TurnHoldSec = 0
	}
	if c.ResetAfterRejects <= 0 {
		c.ResetAfterRejects = d.ResetAfterRejects
	}
	if c.SnapshotEvery <= 0 {
		c.SnapshotEvery = d.SnapshotEvery
	}
	if c.StatsWindow <= 0 {
		c.StatsWindow = d.StatsWindow
	}
	if c.StatsWindow > 65536 {
		c.StatsWindow = 65536
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Ellipse is a 1-sigma horizontal error ellipse.
type Ellipse struct {
	SemiMajorM float64 `json:"a"`   // metres
	SemiMinorM float64 `json:"b"`   // metres
	OrientDeg  float64 `json:"dir"` // direction of the major axis, deg true [0,180)
}

// Prediction is the full predicted state of one aircraft at one instant.
// All floats are finite. Altitude fields are only meaningful when AltOK.
type Prediction struct {
	ICAO       string  `json:"icao"`
	T          int64   `json:"t"`      // Unix ms of the prediction
	Lat        float64 `json:"lat"`    //
	Lon        float64 `json:"lon"`    //
	AltOK      bool    `json:"altOk"`  // altitude known
	AltFt      float64 `json:"alt"`    // ft
	TrackDeg   float64 `json:"track"`  // deg true
	GSKt       float64 `json:"gs"`     // kt
	VRateFpm   float64 `json:"vr"`     // ft/min
	TurnDps    float64 `json:"turn"`   // deg/s, + = right
	SigmaHM    float64 `json:"sigH"`   // 1-sigma horizontal (RMS radial, sqrt of trace), m
	SigmaVFt   float64 `json:"sigV"`   // 1-sigma vertical, ft
	Ellipse    Ellipse `json:"ell"`    //
	AgeSec     float64 `json:"age"`    // seconds since the last real position
	Coasting   bool    `json:"coast"`  // age > CoastAfter: the UI should mark the icon
	Confidence float64 `json:"conf"`   // 0..1
	Model      string  `json:"model"`  // dominant motion model "cv" | "ct"
	PTurn      float64 `json:"pTurn"`  // IMM probability of the turn model
	OnGround   bool    `json:"ground"` //
	Source     string  `json:"src"`    // source of the last position
}

// Predicted is the compact per-aircraft entry of Manager.Predicted.
type Predicted struct {
	ICAO       string  `json:"icao"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	AltFt      float64 `json:"alt,omitempty"` // omitted when unknown
	TrackDeg   float64 `json:"track"`
	GSKt       float64 `json:"gs"`
	VRateFpm   float64 `json:"vr"`
	SigmaHM    float64 `json:"sigH"`
	AgeSec     float64 `json:"age"`
	Coasting   bool    `json:"coast"`
	Confidence float64 `json:"conf"`
}

// PathPoint is one point of a predicted path.
type PathPoint struct {
	DT       float64 `json:"dt"`  // seconds after Path.T0
	Lat      float64 `json:"lat"` //
	Lon      float64 `json:"lon"` //
	AltFt    float64 `json:"alt"` // only meaningful when Path.AltOK
	SigmaHM  float64 `json:"sh"`  // 1-sigma horizontal, m (includes the turn hypothesis: grows to km after a minute)
	SigmaFM  float64 `json:"shf"` // 1-sigma horizontal of the track filter alone, m (no turn hypothesis; for path tick marks)
	SigmaVFt float64 `json:"sv"`  // 1-sigma vertical, ft
	Ellipse  Ellipse `json:"ell"` //
}

// Path is a predicted polyline.
type Path struct {
	ICAO   string      `json:"icao"`
	T0     int64       `json:"t0"`     // Unix ms of the first point
	AltOK  bool        `json:"altOk"`  //
	Plan   string      `json:"plan"`   // "decay" | "capture" | "hold" | "ground"
	Points []PathPoint `json:"points"` //
}

// Runway is a runway threshold for approach detection.
type Runway struct {
	Airport     string  `json:"airport"`
	Ident       string  `json:"ident"`   // e.g. "27L"
	Lat         float64 `json:"lat"`     // threshold
	Lon         float64 `json:"lon"`     //
	HeadingDeg  float64 `json:"heading"` // landing direction, deg true
	ElevationFt float64 `json:"elev"`    // threshold elevation
}

// RunwayProvider supplies runways near a point (optional). dst may be reused;
// implementations must be safe for concurrent use.
type RunwayProvider interface {
	RunwaysNear(lat, lon, radiusKm float64, dst []Runway) []Runway
}

// WindProvider supplies wind (optional; e.g. from internal/met profiles).
// dirFromDeg is the direction the wind blows from. Must be cheap and concurrency-safe.
type WindProvider interface {
	WindAt(lat, lon, altFt float64) (dirFromDeg, speedKt float64, ok bool)
}

// RangeFunc returns the receiver's maximum range in km toward bearingDeg for
// an aircraft at altFt (e.g. from the measured coverage polar).
type RangeFunc func(bearingDeg, altFt float64) float64
