package latency

import (
	"math"

	"airview/internal/geo"
	mgeo "airview/internal/mlat/geo"
	"airview/internal/model"
)

const (
	nsPerSec = int64(1_000_000_000)
	nsPerDay = 86400 * nsPerSec

	// EpochNs is the ADS-B UTC time-of-applicability grid (0.2 s).
	EpochNs = int64(200_000_000)

	// SpeedOfLight in vacuum, m/s (exact, SI).
	SpeedOfLight = 299792458.0

	// Frame durations from the start of the preamble to the end of the last
	// data bit: 8 us preamble + 56 or 112 us of data. A Radarcape GPS
	// timestamp marks the END of the frame (mlat-client modes_reader.c
	// subtracts exactly these values to get the start of the frame).
	shortFrameNs = int64(64_000)
	longFrameNs  = int64(120_000)
	modeACNs     = int64(20_300)
)

// floorDiv is integer division rounding towards minus infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// frameDurationNs returns the preamble+data length of a Mode S frame of
// downlink format df (DF0..15 are 56 bit, DF16+ 112 bit).
func frameDurationNs(df uint8) int64 {
	if df >= 16 {
		return longFrameNs
	}
	return shortFrameNs
}

// Receiver-delay reference instants (the "ref" field of the JSON outputs).
// Every receiver delay AirView reports - this package's receiver stage and
// the tracker's per-feed latency - is measured from the START of the frame
// (start of the preamble at the antenna) to the host socket read, unless
// Options.NoFrameEndCorrection is set (then from the Radarcape stamp, the
// end of the frame).
const (
	RefStartOfFrame = "startOfFrame"
	RefEndOfFrame   = "endOfFrame"
)

// FrameStampOffsetNs is the time from the start of a frame to the instant
// its GPS timestamp marks (Mode S: end of frame, 64 / 120 us; Mode A/C:
// 20.3 us; status frames: 0). Subtract it from a Radarcape timestamp to get
// the start of the frame. kind 0 falls back to the downlink format df.
func FrameStampOffsetNs(kind model.FrameKind, df uint8) int64 { return frameAdjustNs(kind, df) }

// frameAdjustNs is the time from the start of the frame to the instant the
// Radarcape stamps, by frame kind (mlat-client modes_reader.c: Mode A/C is
// stamped at F2, 20.3 us after F1; Mode S at the end of the frame; anything
// else, e.g. status frames, is taken as is). Kind 0 (unknown) falls back to
// the downlink format.
func frameAdjustNs(kind model.FrameKind, df uint8) int64 {
	switch kind {
	case model.FrameModeAC:
		return modeACNs
	case model.FrameModeSShort:
		return shortFrameNs
	case model.FrameModeSLong:
		return longFrameNs
	case model.FrameStatus:
		return 0
	}
	return frameDurationNs(df)
}

// GPSToUnixNs converts a receiver timestamp in nanoseconds since UTC midnight
// to absolute Unix nanoseconds, choosing the UTC day that puts the result
// closest to refUnixNs (the host's best estimate of true UTC at reception).
// This handles the midnight wrap in both directions as long as the host clock
// is within 12 h of the truth.
//
// Values in [86400 s, 86401 s) come from the legacy Radarcape second-of-day
// range 1..86400 ("Daysec+1", jetvision wiki); they are folded back by one
// second and legacy is reported true. (In that legacy mode every other
// timestamp is also one second ahead; the feed package corrects that when the
// receiver's status frame says so, and the analyzer flags a -1 s receiver
// delay otherwise.) ok is false for negative or out-of-range input.
func GPSToUnixNs(gpsNs, refUnixNs int64) (unixNs int64, legacy, ok bool) {
	if gpsNs < 0 || gpsNs >= nsPerDay+nsPerSec {
		return 0, false, false
	}
	if gpsNs >= nsPerDay {
		gpsNs -= nsPerSec
		legacy = true
	}
	day := floorDiv(refUnixNs, nsPerDay)
	t := day*nsPerDay + gpsNs
	switch d := t - refUnixNs; {
	case d > nsPerDay/2:
		t -= nsPerDay
	case d < -nsPerDay/2:
		t += nsPerDay
	}
	return t, legacy, true
}

// NearestEpoch returns the ADS-B time of applicability of a T=1 position:
// the 0.2 s UTC epoch of the parity given by the CPR format bit (odd=false:
// even epoch, odd=true: odd epoch) that is nearest to the reception time
// recvUnixNs (DO-260A Appendix A §A.1.4.2.3.1).
//
// An "even-numbered 0.2 second epoch" is an even number of 200 ms intervals
// after an even-numbered UTC second (12.0, 12.4, 12.8, 13.2 ... s); odd ones
// are 12.2, 12.6, 13.0, 13.4 ... s. Counting 200 ms intervals from the Unix
// epoch (an even second; days have an even number of seconds) gives exactly
// that parity. Leap seconds are ignored (as by Unix time).
func NearestEpoch(recvUnixNs int64, odd bool) int64 {
	k := floorDiv(recvUnixNs+EpochNs/2, EpochNs) // nearest epoch of any parity
	if (k&1 == 1) != odd {
		// The two neighbours of the right parity are k-1 and k+1.
		e := k * EpochNs
		if recvUnixNs >= e {
			k++
		} else {
			k--
		}
	}
	return k * EpochNs
}

// ecef converts WGS84 geodetic coordinates (degrees, metres above the
// ellipsoid) to earth-centred earth-fixed metres (internal/mlat/geo,
// DESIGN §9.1).
func ecef(latDeg, lonDeg, hM float64) (x, y, z float64) {
	v := mgeo.ToECEF(latDeg, lonDeg, hM)
	return v.X, v.Y, v.Z
}

// receiverGeo caches the receiver position.
type receiverGeo struct {
	ok              bool
	lat, lon, hM    float64
	x, y, z         float64 // ECEF
	upX, upY, upZ   float64 // local vertical unit vector
	sinLat, cosLat  float64
	sinLon, cosLon  float64
	station         *geo.Station
	heightAMSLInput float64
}

func newReceiverGeo(lat, lon, heightAMSL float64) receiverGeo {
	if !geo.ValidLatLon(lat, lon) || math.IsNaN(heightAMSL) || math.IsInf(heightAMSL, 0) {
		return receiverGeo{}
	}
	r := receiverGeo{ok: true, lat: lat, lon: lon, hM: heightAMSL, heightAMSLInput: heightAMSL}
	r.x, r.y, r.z = ecef(lat, lon, heightAMSL)
	r.sinLat, r.cosLat = math.Sincos(lat * math.Pi / 180)
	r.sinLon, r.cosLon = math.Sincos(lon * math.Pi / 180)
	r.upX, r.upY, r.upZ = r.cosLat*r.cosLon, r.cosLat*r.sinLon, r.sinLat
	r.station = geo.NewStation(lat, lon, heightAMSL, 0)
	return r
}

// slant returns the straight-line distance (m) from the receiver antenna to
// an aircraft at lat/lon and altM (metres; heights of both ends are treated
// as heights above the ellipsoid - the geoid undulation is nearly common to
// both and cancels in the difference) and the geometric elevation angle in
// degrees.
func (r *receiverGeo) slant(lat, lon, altM float64) (distM, elevDeg float64) {
	x, y, z := ecef(lat, lon, altM)
	dx, dy, dz := x-r.x, y-r.y, z-r.z
	distM = math.Sqrt(dx*dx + dy*dy + dz*dz)
	if distM > 0 {
		elevDeg = math.Asin(math.Max(-1, math.Min(1, (dx*r.upX+dy*r.upY+dz*r.upZ)/distM))) * 180 / math.Pi
	}
	return
}

// atmosphere is the reference atmosphere of the propagation delay: the
// exponential refractivity profile of internal/mlat/geo (N0 = 315,
// H = 7 km, ITU-R P.453), shared with the MLAT engine (DESIGN §9.1).
var atmosphere = mgeo.DefaultAtmosphere()

// propagationNs is the one-way radio propagation time over a slant path of
// straight-line length distM between heights h1 and h2 (metres), including
// the tropospheric refractivity integrated along the refracted ray.
func propagationNs(distM, h1, h2 float64) float64 {
	a := atmosphere
	return a.PropagationNsDist(distM, h1, h2)
}

// frameRef is the receiver-delay reference instant in use.
func (a *Analyzer) frameRef() string {
	if a.opt.NoFrameEndCorrection {
		return RefEndOfFrame
	}
	return RefStartOfFrame
}

// hostClock estimates the offset of an unsynchronised host clock against
// the receiver's GPS time from the minimum of (host receive - GPS time of
// arrival): the receiver -> host delay of a Radarcape is a few ms at its
// floor, so a floor far from that is the host clock's error. The minimum is
// taken over 10 s intervals; the estimate is the smallest of the last six
// (one minute), or of the current interval until one is complete.
type hostClock struct {
	ivStart, ivMin int64
	ivN            int
	ring           [6]int64
	ringN, ringPos int

	est         int64 // estimated host - GPS UTC (ns), valid when have
	have        bool
	active      bool // |est| large: host-side stages are corrected by est
	ambiguous1s bool // est ~ -1 s: host 1 s behind OR legacy +1 s receiver stamps
}

const (
	hostIntervalNs = 10 * nsPerSec
	hostOnNs       = int64(100_000_000) // switch the correction on beyond this
	hostOffNs      = int64(50_000_000)  // and off again below this
	hostMinSamples = 20
)

func (h *hostClock) clear() { *h = hostClock{} }

func (h *hostClock) observe(recvNs, d int64) {
	if h.ivN == 0 || recvNs-h.ivStart >= hostIntervalNs || recvNs < h.ivStart {
		if h.ivN >= hostMinSamples {
			h.ring[h.ringPos] = h.ivMin
			h.ringPos = (h.ringPos + 1) % len(h.ring)
			if h.ringN < len(h.ring) {
				h.ringN++
			}
		}
		h.ivStart, h.ivMin, h.ivN = recvNs, d, 0
	}
	h.ivN++
	if d < h.ivMin {
		h.ivMin = d
	}
	est, ok := int64(math.MaxInt64), false
	for i := 0; i < h.ringN; i++ {
		est, ok = min(est, h.ring[i]), true
	}
	if !ok && h.ivN >= hostMinSamples {
		est, ok = h.ivMin, true
	}
	if !ok {
		return
	}
	// GPS-time stamping (receiver ~18 s ahead of UTC) is a receiver setting,
	// not a host clock error: leave it to classifyAbsurd.
	if est < -15*nsPerSec && est > -20*nsPerSec {
		h.est, h.have, h.active, h.ambiguous1s = est, true, false, false
		return
	}
	h.est, h.have = est, true
	ae := est
	if ae < 0 {
		ae = -ae
	}
	switch {
	case ae > hostOnNs:
		h.active = true
	case ae < hostOffNs:
		h.active = false
	}
	h.ambiguous1s = h.active && est < -nsPerSec+30_000_000 && est > -nsPerSec-30_000_000
}
