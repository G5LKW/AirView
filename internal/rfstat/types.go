package rfstat

// JSON result types. All floats are Num (NaN/Inf marshal as null); times are
// Unix milliseconds; ICAO addresses are 6-hex strings ("~" prefix = non-ICAO).

// Report is the full snapshot served by GET /api/rf/report.
type Report struct {
	T         int64 `json:"t"`         // snapshot time (ms)
	WindowSec Num   `json:"windowSec"` // effective length of the sliding window (s)
	Since     int64 `json:"since"`     // engine start (ms)
	Live      bool  `json:"live"`      // sliding ~1 h window (false = one closed hour)

	Signal        SignalStats        `json:"signal"`
	Detection     DetectionStats     `json:"detection"`
	Channel       ChannelStats       `json:"channel"`
	Interrogation InterrogationStats `json:"interrogation"`
	Timing        TimingStats        `json:"timing"`
	Census        CensusStats        `json:"census"`
	Health        Health             `json:"health"`
}

// Hist is a histogram with equal bins: bin i covers [Lo+i*Step, Lo+(i+1)*Step).
type Hist struct {
	Lo     Num   `json:"lo"`
	Step   Num   `json:"step"`
	Counts []Num `json:"counts"`
}

// SignalBand summarises the RSSI distribution of one antenna/ring/elevation band.
type SignalBand struct {
	Label string `json:"label"`
	Lo    Num    `json:"lo"` // band lower bound (km or deg), null if open
	Hi    Num    `json:"hi"`
	N     int64  `json:"n"`
	P05   Num    `json:"p05"`
	P50   Num    `json:"p50"`
	P95   Num    `json:"p95"`
	Mean  Num    `json:"mean"`
	// StrongFrac = share within 3 dB of full scale.
	StrongFrac Num   `json:"strongFrac"`
	Hist       *Hist `json:"hist,omitempty"`
}

// SignalStats is section 1.
type SignalStats struct {
	Frames     int64        `json:"frames"` // frames with a signal level
	All        SignalBand   `json:"all"`
	PerAntenna []SignalBand `json:"perAntenna"`
	PerRing    []SignalBand `json:"perRing"` // 25 km rings
	PerElev    []SignalBand `json:"perElev"`

	DecodeFloor   Num `json:"decodeFloorDbfs"`   // 1st percentile = weakest reliably decodable level
	NoiseFloorEst Num `json:"noiseFloorEstDbfs"` // DecodeFloor - assumed decode SNR
	AssumedSNR    Num `json:"assumedDecodeSnrDb"`
	P50           Num `json:"p50Dbfs"`
	P999          Num `json:"p999Dbfs"`
	DynamicRange  Num `json:"dynamicRangeDb"` // p99.9 - p0.1
	SNRMargin     Num `json:"snrMarginDb"`    // p50 - DecodeFloor
	SNREst        Num `json:"snrEstDb"`       // p50 - NoiseFloorEst
	StrongFrac    Num `json:"strongFrac"`     // >= -3 dBFS
	NearClipFrac  Num `json:"nearClipFrac"`   // >= -1 dBFS

	Fading    FadingStats    `json:"fading"`
	Multipath MultipathStats `json:"multipath"`
	Aspect    AspectStats    `json:"aspect"`
}

// FadingStats: short-window (FadeWindow) fading of range-normalised RSSI.
type FadingStats struct {
	Windows     int64         `json:"windows"`
	KMedianDB   Num           `json:"kMedianDb"` // Rician K (dB), moment method
	StdMedianDB Num           `json:"stdMedianDb"`
	KHist       Hist          `json:"kHist"`   // K in dB
	StdHist     Hist          `json:"stdHist"` // RSSI std dev in dB
	KByElev     []ElevK       `json:"kByElev"`
	Worst       []AircraftFad `json:"worst"` // strongest fading among active aircraft
}

// ElevK is the mean K factor in one elevation band.
type ElevK struct {
	Label string `json:"label"`
	KDB   Num    `json:"kDb"`
	N     int64  `json:"n"`
}

// AircraftFad is the fading state of one aircraft.
type AircraftFad struct {
	ICAO    string `json:"icao"`
	KDB     Num    `json:"kDb"`
	StdDB   Num    `json:"stdDb"`
	Windows int    `json:"windows"`
	DistKm  Num    `json:"distKm"`
	ElevDeg Num    `json:"elevDeg"`
}

// MultipathStats: periodic RSSI ripple versus elevation angle (two-ray /
// Lloyd's mirror lobing from a ground reflection).
type MultipathStats struct {
	ElevDeg  []Num `json:"elevDeg"`  // bin centres with data
	DevDB    []Num `json:"devDb"`    // detrended mean deviation per bin
	RippleDB Num   `json:"rippleDb"` // RMS of the detrended deviation
	// ReflectorHeightM = lambda / (2 * period in sin(elev)): height of the
	// antenna above the dominant reflecting surface.
	ReflectorHeightM Num  `json:"reflectorHeightM"`
	PeriodDeg        Num  `json:"periodDeg"` // lobe spacing near 2 deg elevation
	Strength         Num  `json:"strength"`  // 0..1 periodogram peak (1 = pure sinusoid)
	Detected         bool `json:"detected"`
	Bins             int  `json:"bins"`
}

// AspectStats: RSSI versus aspect (0 = nose-on, 180 = tail-on), relative to
// each aircraft's own mean, i.e. airframe shadowing of the transponder antenna.
type AspectStats struct {
	FromDeg    []Num   `json:"fromDeg"`
	MeanDB     []Num   `json:"meanDb"`
	StdDB      []Num   `json:"stdDb"`
	N          []int64 `json:"n"`
	NoseDB     Num     `json:"noseDb"` // 0-30
	BeamDB     Num     `json:"beamDb"` // 60-120
	TailDB     Num     `json:"tailDb"` // 150-180
	NoseTailDB Num     `json:"noseTailDb"`
}

// PdAxis is probability of detection along one axis. Index i of every slice
// is the same bin; Pos/Vel/Ident are null where too few squitters were expected.
type PdAxis struct {
	Lo       []Num `json:"lo"`
	Hi       []Num `json:"hi"`
	Pos      []Num `json:"pos"`
	Vel      []Num `json:"vel"`
	Ident    []Num `json:"ident"`
	Expected []Num `json:"expected"` // expected position squitters per bin
}

// PdType is the total reception probability of one squitter type.
type PdType struct {
	Type        string `json:"type"`
	NominalRate Num    `json:"nominalRate"` // per second
	Expected    Num    `json:"expected"`
	Decoded     Num    `json:"decoded"`
	Pd          Num    `json:"pd"`
}

// GapBand: position update interval statistics in one range band.
type GapBand struct {
	LoKm Num   `json:"loKm"`
	HiKm Num   `json:"hiKm"` // null = open
	N    int64 `json:"n"`
	P50  Num   `json:"p50"`
	P90  Num   `json:"p90"`
	P95  Num   `json:"p95"`
	P99  Num   `json:"p99"`
	Mean Num   `json:"mean"`
	// PUpdate[i] = time-weighted probability that the latest position is not
	// older than TSec[i] seconds (sum(min(gap,T))/sum(gap)).
	TSec    []Num `json:"tSec"`
	PUpdate []Num `json:"pUpdate"`
}

// AircraftPd is the lifetime reception probability of one active aircraft.
type AircraftPd struct {
	ICAO   string `json:"icao"`
	DistKm Num    `json:"distKm"`
	PdPos  Num    `json:"pdPos"`
	PdVel  Num    `json:"pdVel"`
	PdID   Num    `json:"pdIdent"`
	RSSI   Num    `json:"rssi"`
}

// DetectionStats is section 2.
type DetectionStats struct {
	Types      []PdType     `json:"types"`
	VsDistance PdAxis       `json:"vsDistance"`
	VsElev     PdAxis       `json:"vsElev"`
	VsBearing  PdAxis       `json:"vsBearing"`
	VsRSSI     PdAxis       `json:"vsRssi"`
	Range50Km  Num          `json:"range50Km"` // position Pd crosses 50 %
	Range90Km  Num          `json:"range90Km"` // position Pd crosses 90 %
	Gaps       []GapBand    `json:"gaps"`
	Aircraft   []AircraftPd `json:"aircraft"`
}

// DFRate is the count/rate of one downlink format.
type DFRate struct {
	DF    int    `json:"df"`
	Label string `json:"label"`
	Count int64  `json:"count"`
	Rate  Num    `json:"rate"` // per second
}

// LoadBin: failure rate versus the per-second frame rate.
type LoadBin struct {
	LoFPS     Num   `json:"loFps"`
	HiFPS     Num   `json:"hiFps"`
	Seconds   int64 `json:"seconds"`
	MeanFPS   Num   `json:"meanFps"`
	FailFrac  Num   `json:"failFrac"`
	ModelFail Num   `json:"modelFailFrac"` // Poisson overlap prediction
}

// Series is a per-second time series (index i = T0 + i seconds).
type Series struct {
	T0     int64 `json:"t0"`
	Frames []int `json:"frames"`
	Bad    []int `json:"bad"`
	OccPct []Num `json:"occPct"`
}

// ChannelStats is section 3.
type ChannelStats struct {
	Seconds      Num              `json:"seconds"`
	Frames       int64            `json:"frames"`
	FPS          Num              `json:"fps"`
	ByDF         []DFRate         `json:"byDf"`
	ByCRC        map[string]int64 `json:"byCrc"`
	OccupancyPct Num              `json:"occupancyPct"`     // air time of received replies / time
	PeakOccPct   Num              `json:"peakOccupancyPct"` // max over the last 10 min (1 s)
	MeanReplyUs  Num              `json:"meanReplyUs"`
	BadFrac      Num              `json:"crcBadFrac"`
	FixedFrac    Num              `json:"fixedFrac"`
	RejectedFrac Num              `json:"rejectedFrac"` // rejected / all address-parity frames
	FailFrac     Num              `json:"failFrac"`     // (bad + rejected) / frames
	// Poisson model: probability that another reply overlaps a given one at
	// the measured rate: 1 - exp(-2 * lambda * tau).
	OverlapProb Num `json:"overlapProb"`
	// ImpliedRate: channel rate (replies/s) that would explain the observed
	// failure fraction by overlaps alone. >> FPS means many undecoded replies (FRUIT).
	ImpliedRate Num `json:"impliedRate"`
	// ExcessFail = FailFrac - OverlapProb (> 0: noise/interference beyond garble).
	ExcessFail Num       `json:"excessFail"`
	VsLoad     []LoadBin `json:"vsLoad"`
	Recent     Series    `json:"recent"`
	Bursts     []Burst   `json:"bursts"`
	BurstCount int64     `json:"burstCount"`
}

// Interrogator is one interrogator code seen in DF11 replies.
type Interrogator struct {
	Code     string `json:"code"` // "II 3" or "SI 12"
	IID      int    `json:"iid"`
	Replies  int64  `json:"replies"`
	Rate     Num    `json:"rate"`     // per second over the window
	Last10m  int64  `json:"last10m"`  // replies in the last 10 minutes
	Aircraft int    `json:"aircraft"` // aircraft that replied within 2 min
	// ReplyingFrac = Aircraft / aircraft under Mode S surveillance (2 min).
	ReplyingFrac Num  `json:"replyingFrac"`
	Lockout      bool `json:"lockoutLikely"`
	// Timeline: replies per minute, oldest first, last 60 minutes.
	Timeline []int `json:"timeline"`
	// AcqSquitter: II 0 also contains spontaneous DF11 acquisition squitters.
	AcqSquitter bool `json:"acqSquitter,omitempty"`
}

// BDSCount is the Comm-B register mix.
type BDSCount struct {
	BDS   string `json:"bds"`
	Count int64  `json:"count"`
	Frac  Num    `json:"frac"`
}

// AircraftRate is a per-aircraft surveillance reply rate.
type AircraftRate struct {
	ICAO    string `json:"icao"`
	Rate    Num    `json:"rate"`
	Replies int64  `json:"replies"`
}

// InterrogationStats is section 4.
type InterrogationStats struct {
	Codes        []Interrogator `json:"codes"`
	ActiveRadars int            `json:"activeRadars"` // codes != II0 with >= 10 replies in 10 min
	SurvRateP50  Num            `json:"survRateP50"`
	SurvRateP90  Num            `json:"survRateP90"`
	SurvRateMax  Num            `json:"survRateMax"`
	TopSurv      []AircraftRate `json:"topSurv"`
	CommB        []BDSCount     `json:"commB"`
	DF0Rate      Num            `json:"df0Rate"`
	DF16Rate     Num            `json:"df16Rate"`
	RAReports    int64          `json:"raReports"`
	ACASAircraft int            `json:"acasAircraft"`
	Active       int            `json:"activeAircraft"` // heard within 2 min
	ADSBAndModeS int            `json:"adsbAndModeS"`
	ADSBOnly     int            `json:"adsbOnly"`
	ModeSOnly    int            `json:"modeSOnly"`
	RadarShare   Num            `json:"radarShare"` // share of ADS-B aircraft also under Mode S surveillance
}

// TimingType: GPS-timestamp interval statistics for one squitter type.
type TimingType struct {
	Type      string `json:"type"`
	Intervals int64  `json:"intervals"`
	BadFrac   Num    `json:"badFrac"` // intervals impossible for a compliant transmitter
	Hist      Hist   `json:"hist"`
	// Single-interval (no loss) statistics inside the nominal [lo,hi] range.
	NominalLo Num  `json:"nominalLo"`
	NominalHi Num  `json:"nominalHi"`
	Mean      Num  `json:"mean"`
	Std       Num  `json:"std"`
	Chi2      Num  `json:"chi2"` // 10 sub-bins vs uniform, df = 9
	UniformOK bool `json:"uniformOk"`
}

// NonCompliant is an aircraft whose squitter timing looks non-compliant.
type NonCompliant struct {
	ICAO      string `json:"icao"`
	Intervals int    `json:"intervals"`
	BadFrac   Num    `json:"badFrac"`
	Mean      Num    `json:"mean"`
	Std       Num    `json:"std"`
	Reason    string `json:"reason"`
}

// TimingStats is section 5.
type TimingStats struct {
	Available     bool           `json:"available"`
	Types         []TimingType   `json:"types"`
	AltPairs      int64          `json:"altPairs"`      // consecutive position pairs (no loss)
	AltRegularity Num            `json:"altRegularity"` // share that alternate even/odd
	Alt2Pairs     int64          `json:"alt2Pairs"`     // pairs with one lost squitter
	Alt2Same      Num            `json:"alt2Same"`      // share with equal parity
	NonCompliant  []NonCompliant `json:"nonCompliant"`
	Note          string         `json:"note"`
}

// Count is a labelled count with share.
type Count struct {
	Label string `json:"label"`
	Count int    `json:"count"`
	Share Num    `json:"share"`
}

// AltBandDelta is the GNSS-minus-baro difference in one altitude band.
type AltBandDelta struct {
	Band   string `json:"band"`
	MeanFt Num    `json:"meanFt"`
	StdFt  Num    `json:"stdFt"`
	N      int64  `json:"n"`
}

// GNSSPoint is one minute of the GNSS interference index.
type GNSSPoint struct {
	T     int64 `json:"t"`
	Index Num   `json:"index"` // % of ADS-B aircraft with degraded integrity
	Tot   int   `json:"tot"`
	Deg   int   `json:"deg"`
	Jumps int   `json:"jumps"`
}

// GNSSCell is one polar map cell (10 deg x 50 km) of the interference map.
type GNSSCell struct {
	Sector    int   `json:"sector"` // 0..35 (x10 deg)
	Ring      int   `json:"ring"`   // 0..9 (x50 km)
	CenterLat Num   `json:"lat"`
	CenterLon Num   `json:"lon"`
	Tot       int64 `json:"tot"` // aircraft-minutes
	Deg       int64 `json:"deg"`
	Index     Num   `json:"index"`
}

// CensusStats is section 6.
type CensusStats struct {
	Aircraft       int            `json:"aircraft"`
	ADSB           int            `json:"adsb"`
	Versions       []Count        `json:"versions"`
	NIC            []Count        `json:"nic"`
	NACp           []Count        `json:"nacp"`
	SIL            []Count        `json:"sil"`
	PoorIntegrity  Num            `json:"poorIntegrityShare"`
	Categories     []Count        `json:"categories"`
	TC28Share      Num            `json:"tc28Share"`
	TC29Share      Num            `json:"tc29Share"`
	TC31Share      Num            `json:"tc31Share"`
	GNSSAltShare   Num            `json:"gnssAltShare"`
	GNSSDelta      Hist           `json:"gnssDelta"`
	GNSSDeltaMean  Num            `json:"gnssDeltaMeanFt"`
	GNSSDeltaByAlt []AltBandDelta `json:"gnssDeltaByAlt"`
	JumpAircraft   int            `json:"jumpAircraft"`
	Degraded       int            `json:"degradedNow"`
	DegradedList   []string       `json:"degradedList"`
	GNSSIndexNow   Num            `json:"gnssIndexNow"`
	GNSSSeries     []GNSSPoint    `json:"gnssSeries"`
	GNSSCells      []GNSSCell     `json:"gnssCells"`
	DroppedFrames  uint64         `json:"droppedFrames"` // aircraft table full
}

// HealthItem is one scored aspect of RF health.
type HealthItem struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Score  int    `json:"score"`  // 0..100, -1 = not enough data
	Status string `json:"status"` // ok | warn | bad | unknown
	Text   string `json:"text"`
	// Code is the stable id of the message kind ("gain.too-high", ...) and
	// Data the numbers printed in Text under unit-suffixed names, so a UI
	// can re-format them in the viewer's units (codes and fields: WIRING.md
	// section 6). Text is unchanged.
	Code string `json:"code"`
	Data Data   `json:"data"`
}

// Data holds the numbers behind a prose message, keyed by unit-suffixed
// names (distKm, rateHz, strongPct, p50Dbfs, ...); unknown values are null.
type Data map[string]Num

// AdviceItem is one piece of advice with its stable code and numbers.
type AdviceItem struct {
	Code string `json:"code"`
	Text string `json:"text"` // the same sentence as in Health.Advice
	Data Data   `json:"data"`
}

// Sector is a bearing sector with low detection probability.
type Sector struct {
	FromDeg  int `json:"fromDeg"`
	ToDeg    int `json:"toDeg"`
	Pd       Num `json:"pd"`
	MedianPd Num `json:"medianPd"`
}

// Health is section 7: headline scores with plain-language advice.
type Health struct {
	Score   int          `json:"score"` // 0..100 overall, -1 = no data
	Status  string       `json:"status"`
	Items   []HealthItem `json:"items"`
	Blocked []Sector     `json:"blockedSectors"`
	Advice  []string     `json:"advice"`
	// AdviceItems are the Advice sentences, in the same order, with their
	// codes and numbers.
	AdviceItems []AdviceItem `json:"adviceItems"`
}

// HourSummary is the compact per-hour record (kept in memory and persisted).
type HourSummary struct {
	T            int64            `json:"t"` // hour start (ms)
	Seconds      Num              `json:"seconds"`
	Frames       int64            `json:"frames"`
	FPS          Num              `json:"fps"`
	OccupancyPct Num              `json:"occupancyPct"`
	BadFrac      Num              `json:"crcBadFrac"`
	FixedFrac    Num              `json:"fixedFrac"`
	RejectedFrac Num              `json:"rejectedFrac"`
	OverlapProb  Num              `json:"overlapProb"`
	DecodeFloor  Num              `json:"decodeFloorDbfs"`
	P50          Num              `json:"p50Dbfs"`
	StrongFrac   Num              `json:"strongFrac"`
	KMedianDB    Num              `json:"kMedianDb"`
	PdPos        Num              `json:"pdPos"`
	PdVel        Num              `json:"pdVel"`
	PdIdent      Num              `json:"pdIdent"`
	Range50Km    Num              `json:"range50Km"`
	Range90Km    Num              `json:"range90Km"`
	PUpdate5s    Num              `json:"pUpdate5s"`
	GapP95       Num              `json:"gapP95"`
	Radars       int              `json:"radars"`
	DF11ByIID    map[string]int64 `json:"df11ByIid,omitempty"`
	ACASRate     Num              `json:"acasRate"`
	RAReports    int64            `json:"raReports"`
	Bursts       int64            `json:"bursts"`
	TimingBad    Num              `json:"timingBadFrac"`
	GNSSIndexMax Num              `json:"gnssIndexMax"`
	Health       int              `json:"health"`
}
