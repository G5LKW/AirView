package rfstat

import (
	"log/slog"
	"math"
	"sync"
	"time"
)

// Nominal ADS-B extended-squitter rates (per second) while airborne:
// airborne position 2/s, airborne velocity 2/s, identification 0.2/s.
var nominalRate = [nPdTypes]float64{2, 2, 0.2}

// Config configures Stats. Zero values select the defaults.
type Config struct {
	// Antennas are the antenna IDs; Frame.Antenna indexes this slice (max 8).
	Antennas []string
	// MaxAircraft bounds the per-aircraft table (default 4096).
	MaxAircraft int
	// AircraftTimeout evicts aircraft not heard for this long (default 5 min).
	AircraftTimeout time.Duration
	// PdWindow is the reception-probability counting window (default 10 s).
	PdWindow time.Duration
	// PdMaxGap: silent gaps longer than this end a track for Pd purposes (default 30 s).
	PdMaxGap time.Duration
	// FadeWindow is the short window for fading statistics (default 10 s).
	FadeWindow time.Duration
	// StationLat/Lon (deg) give cell centres in the GNSS interference map; NaN/0 = unknown.
	StationLat, StationLon float64
	// DecodeSNRdB is the assumed SNR at the weakest decodable level, used to
	// turn the decode floor into a noise-floor estimate (default 10 dB).
	DecodeSNRdB float64
	// History is the number of hourly summaries kept in memory (default 48).
	History int
	Logger  *slog.Logger
}

func (c *Config) defaults() {
	if c.MaxAircraft <= 0 {
		c.MaxAircraft = 4096
	}
	if c.AircraftTimeout <= 0 {
		c.AircraftTimeout = 5 * time.Minute
	}
	if c.PdWindow <= 0 {
		c.PdWindow = 10 * time.Second
	}
	if c.PdMaxGap <= 0 {
		c.PdMaxGap = 30 * time.Second
	}
	if c.FadeWindow <= 0 {
		c.FadeWindow = 10 * time.Second
	}
	if c.DecodeSNRdB <= 0 {
		c.DecodeSNRdB = 10
	}
	if c.History <= 0 {
		c.History = 48
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if len(c.Antennas) > MaxAntennas {
		c.Antennas = c.Antennas[:MaxAntennas]
	}
}

// secRec is one second of channel activity.
type secRec struct {
	sec                         int64
	frames, bad, fixed, rejectd uint16
	airUs                       float32
}

const secRing = 600 // 10 minutes of per-second records

// Burst is a second with an abnormally high frame or error rate.
type Burst struct {
	T        int64 `json:"t"` // Unix ms
	Frames   int   `json:"frames"`
	Expected Num   `json:"expected"` // EWMA rate before the burst
	BadFrac  Num   `json:"badFrac"`
}

const maxBursts = 32

type gnssMin struct {
	min           int64
	tot, deg, jmp uint16
}

const gnssRing = 1440 // 24 h of minutes

// Stats is the online RF statistics engine. It is safe for concurrent use.
type Stats struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	tab   *table
	cur   accum // current clock hour
	prev  accum // previous clock hour
	hour  int64 // Unix hour of cur
	sec   int64 // last processed Unix second
	start int64 // first second seen

	secs    [secRing]secRec
	rateAvg float64 // EWMA of frames/s
	rateVar float64
	rateN   int
	bursts  [maxBursts]Burst
	nBurst  int

	icMin  [60][nIID]uint16 // per-minute DF11 replies per IID
	icMinT [60]int64        // minute number of each row
	minute int64

	gnss [gnssRing]gnssMin

	lastEvict int64
	dropped   uint64 // frames whose aircraft did not fit into the table

	history []HourSummary
	pending chan HourSummary

	scratch accum // snapshot work area (guarded by snapMu)
	snapMu  sync.Mutex
}

// New returns a Stats engine.
func New(cfg Config) *Stats {
	cfg.defaults()
	s := &Stats{
		cfg:     cfg,
		log:     cfg.Logger,
		tab:     newTable(cfg.MaxAircraft),
		history: make([]HourSummary, 0, cfg.History),
		pending: make(chan HourSummary, 4),
	}
	return s
}

// AntennaIndex returns the Frame.Antenna index of an antenna ID, or -1.
// Call it once per feed when (re)configuring, not per frame.
func (s *Stats) AntennaIndex(id string) int8 {
	for i, a := range s.cfg.Antennas {
		if a == id {
			return int8(i)
		}
	}
	return -1
}

// Observe accumulates one frame. It does not allocate.
func (s *Stats) Observe(f Frame) {
	t := f.Recv.UnixNano()
	sec := t / 1e9
	s.mu.Lock()
	s.advance(sec)
	s.observe(&f, t, sec)
	s.mu.Unlock()
}

// Tick advances the clocks (second/minute/hour rolls, aircraft eviction).
// Call it about once per second, also when no frames arrive.
func (s *Stats) Tick(now time.Time) {
	s.mu.Lock()
	s.advance(now.Unix())
	s.mu.Unlock()
}

func (s *Stats) observe(f *Frame, t, sec int64) {
	a := &s.cur
	df := int(f.DF)
	if df > nDF-1 {
		df = nDF - 1
	}
	a[oFrames]++
	a[oDF+df]++
	crc := f.CRC
	if crc >= numCRC {
		crc = CRCUnknown
	}
	a[oCRC+int(crc)]++
	kind := f.Kind
	if kind >= numKind {
		kind = KindOther
	}
	a[oKind+int(kind)]++
	air := airShortUs
	if f.DF == DFModeAC || kind == KindModeAC {
		air = airModeAC
	} else if longDF(f.DF) {
		air = airLongUs
	}
	a[oAirUs] += air
	if sec < s.sec {
		sec = s.sec // late frame: count it in the current second
	}
	r := &s.secs[sec%secRing]
	r.frames++
	r.airUs += float32(air)
	switch crc {
	case CRCBad:
		r.bad++
		return
	case CRCAddrRejected:
		r.rejectd++
		return
	case CRCFixed:
		r.fixed++
	}

	ant := int(f.Antenna)
	if ant >= MaxAntennas {
		ant = -1
	}
	if ant >= 0 {
		a[oAntFrames+ant]++
	}
	sig := f.Signal
	hasSig := finite(sig)
	dist := f.DistKm
	hasDist := finite(dist) && dist >= 0
	elev := f.ElevDeg
	hasElev := finite(elev)
	if hasSig {
		b := rssiBin(sig)
		a[oRSSIAll+b]++
		if ant >= 0 {
			a[oRSSIAnt+ant*nRSSI+b]++
		}
		if hasDist {
			a[oRSSIRing+clampInt(int(dist/ringKm), 0, nRing-1)*nRSSI+b]++
		}
		if hasElev {
			a[oRSSIElev+elevBand(elev)*nRSSI+b]++
		}
	}

	switch kind {
	case KindAllCall:
		if f.IID < nIID {
			a[oIID+int(f.IID)]++
			s.icMin[s.minute%60][f.IID]++
		}
	case KindCommBAlt, KindCommBIdent:
		a[oBDS+bdsIndex(f.BDS)]++
	}
	if f.RA {
		a[oRA]++
	}

	if f.ICAO == 0 {
		return
	}
	ac, isNew := s.tab.get(f.ICAO)
	if ac == nil {
		s.dropped++
		return
	}
	if isNew {
		s.initAircraft(ac, t)
	}
	s.updateAircraft(ac, f, t, kind, hasSig, hasDist, hasElev)
}

func (s *Stats) initAircraft(ac *acState, t int64) {
	ac.first = t
	ac.version, ac.nic, ac.nacp, ac.sil = -1, -1, -1, -1
	ac.maxNIC, ac.maxNACp = -1, -1
	ac.gpsFeed = -2
	ac.lastLat, ac.lastLon = nan, nan
	ac.dist, ac.brg, ac.elev, ac.alt, ac.track = float32(nan), float32(nan), float32(nan), float32(nan), float32(nan)
	ac.rssiEWMA, ac.residEWMA = float32(nan), float32(nan)
	for i := range ac.lastGPS {
		ac.lastGPS[i] = -1
	}
}

// pdType maps a kind to its reception-probability type (-1 = none).
func pdType(k Kind) int {
	switch k {
	case KindPosEven, KindPosOdd:
		return 0
	case KindVelocity:
		return 1
	case KindIdent:
		return 2
	}
	return -1
}

func (s *Stats) updateAircraft(ac *acState, f *Frame, t int64, kind Kind, hasSig, hasDist, hasElev bool) {
	a := &s.cur
	ac.last = t
	ac.frames++
	if f.OnGround {
		ac.flags |= fGround
	} else {
		ac.flags &^= fGround
	}
	switch f.DF {
	case 17:
		ac.flags |= fADSB
		ac.lastADSB = t
	case 18:
		ac.flags |= fDF18
	case 4, 5, 20, 21:
		ac.flags |= fSurv
		ac.survN++
		ac.lastSurv = t
	case 0, 16:
		ac.flags |= fACAS
		ac.acasN++
	case 11:
		ac.df11N++
	}
	if f.RA {
		ac.raN++
	}
	switch kind {
	case KindAircraftStatus:
		ac.flags |= fTC28
	case KindTargetState:
		ac.flags |= fTC29
	case KindOpStatus:
		ac.flags |= fTC31
	case KindAllCall:
		if f.IID < nIID {
			ac.noteCode(f.IID, t)
		}
	}
	if f.Version >= 0 {
		ac.version = f.Version
	}
	if f.SIL >= 0 {
		ac.sil = f.SIL
	}
	if f.Category != 0 {
		ac.category = f.Category
	}
	if f.NIC >= 0 {
		ac.nic = f.NIC
		if f.NIC > ac.maxNIC {
			ac.maxNIC = f.NIC
		}
	}
	if f.NACp >= 0 {
		ac.nacp = f.NACp
		if f.NACp > ac.maxNACp {
			ac.maxNACp = f.NACp
		}
	}
	ac.degraded = (ac.nacp >= 0 && ac.nacp < 5 && ac.maxNACp >= 8) || (ac.nic >= 0 && ac.nic < 5 && ac.maxNIC >= 7)
	if f.GNSSAlt {
		ac.flags |= fGNSSAlt
	}
	if finite(f.GNSSDeltaFt) {
		ac.flags |= fGNSSAlt
		a[oGDHist+clampInt(int((f.GNSSDeltaFt-gdLo)/gdStep), 0, nGD-1)]++
		ab := altBand(f.AltFt)
		a[oGDSum+ab] += f.GNSSDeltaFt
		a[oGDSq+ab] += f.GNSSDeltaFt * f.GNSSDeltaFt
		a[oGDN+ab]++
	}
	if hasDist {
		ac.dist = float32(f.DistKm)
	}
	if finite(f.BearingDeg) {
		ac.brg = float32(f.BearingDeg)
	}
	if hasElev {
		ac.elev = float32(f.ElevDeg)
	}
	if finite(f.AltFt) {
		ac.alt = float32(f.AltFt)
	}
	if finite(f.TrackDeg) {
		ac.track = float32(f.TrackDeg)
	}

	pt := pdType(kind)
	isADSB := f.DF == 17

	// --- signal: fading, aspect, multipath
	if hasSig {
		if ac.rssiEWMA != ac.rssiEWMA {
			ac.rssiEWMA = float32(f.Signal)
		} else {
			ac.rssiEWMA += 0.1 * (float32(f.Signal) - ac.rssiEWMA)
		}
		if hasDist {
			d := f.DistKm
			if d < 0.5 {
				d = 0.5
			}
			resid := f.Signal + 20*math.Log10(d)
			s.signalAircraft(ac, f, t, resid, d, hasElev)
		}
	}

	// --- reception probability (DF17 airborne squitters)
	if isADSB {
		s.pdAdvance(ac, t)
		if hasSig {
			ac.pdRSum += float32(f.Signal)
			ac.pdRN++
		}
		if pt >= 0 && !f.OnGround {
			// Duplicates of the same squitter (several feeds) arrive within
			// milliseconds; real squitters of one type are >= 0.4 s apart.
			if t-ac.lastTypeT[pt] > 50e6 {
				ac.pdSeen |= 1 << pt
				ac.pdCnt[pt]++
			}
			ac.lastTypeT[pt] = t
		}
	}

	// --- position update gaps, jumps
	if kind == KindPosEven || kind == KindPosOdd {
		ac.flags |= fPos
		if ac.lastPosT > 0 {
			gap := float64(t-ac.lastPosT) / 1e9
			if gap > 0.05 && gap < 120 {
				b := gapBand(f.DistKm)
				if gap < float64(nGapBins)*gapStep {
					a[oGapHist+b*nGapBins+int(gap/gapStep)]++
				}
				a[oGapN+b]++
				a[oGapSum+b] += gap
				for i, T := range gapT {
					a[oGapMin+b*nGapT+i] += math.Min(gap, T)
				}
			}
		}
		if t-ac.lastPosT > 50e6 {
			ac.lastPosT = t
		}
		ac.posTMinute = t
		if finite(f.Lat) && finite(f.Lon) {
			s.checkJump(ac, f, t)
		}
	}
	if f.Implausible && t-ac.lastJumpT > 5e9 {
		ac.jumps++
		ac.jumpsMin++
		ac.lastJumpT = t
	}

	// --- GPS timing
	if f.GPSNs >= 0 && isADSB && pt >= 0 && !f.OnGround {
		s.timing(ac, f, pt, kind)
	}
}

func (ac *acState) noteCode(iid uint8, t int64) {
	c := iid + 1
	oldest := 0
	for i, v := range ac.codes {
		if v == c || v == 0 {
			ac.codes[i] = c
			ac.codeLast[i] = t
			return
		}
		if ac.codeLast[i] < ac.codeLast[oldest] {
			oldest = i
		}
	}
	ac.codes[oldest] = c
	ac.codeLast[oldest] = t
}

// checkJump flags positions implying more than 1200 kt over more than 3 km.
func (s *Stats) checkJump(ac *acState, f *Frame, t int64) {
	if finite(ac.lastLat) {
		dLat := (f.Lat - ac.lastLat) * 111.195
		dLon := (f.Lon - ac.lastLon) * 111.195 * math.Cos(f.Lat*math.Pi/180)
		dk := math.Sqrt(dLat*dLat + dLon*dLon)
		dts := float64(t-ac.lastLatT) / 1e9
		if dts < 0.1 {
			dts = 0.1
		}
		speedKt := dk / dts * 3600 / 1.852
		if dk > 3 && speedKt > 1200 {
			if t-ac.lastJumpT > 5e9 {
				ac.jumps++
				ac.jumpsMin++
				ac.lastJumpT = t
			}
			// Keep the old reference: a single bad position must not make
			// the next good one look like a jump back.
			if t-ac.lastLatT < 30e9 {
				return
			}
		}
	}
	ac.lastLat, ac.lastLon = f.Lat, f.Lon
	ac.lastLatT = t
}
