package trajectory

import (
	"math"
	"sort"
)

// holdInfo is the cached result of holding-pattern detection.
type holdInfo struct {
	ok          bool
	orbit       bool    // continuous circling (no straight legs)
	w           float64 // signed turn rate in turns, rad/s
	legSec      float64
	legCourse   float64
	straightFor float64 // seconds since the last turn ended (0 when turning)
	cLat, cLon  float64
	radiusKm    float64
	laps        float64
}

// detectHold looks for >= 1.25 laps of same-direction turning confined to a
// small area within the last 8 minutes of history.
func detectHold(tr *track) holdInfo {
	var hi holdInfo
	n := tr.histN
	if n < 24 {
		return hi
	}
	var sum, opp, turnSum float64
	var turnN int
	used := 1
	for i := 0; i < n-1; i++ {
		a, b := tr.histAt(i), tr.histAt(i+1)
		if a.t-b.t > 20 {
			break
		}
		d := angDiff(float64(a.trk), float64(b.trk))
		sum += d
		used = i + 2
		if math.Abs(sum) >= 720 {
			break
		}
	}
	if math.Abs(sum) < 450 {
		return hi
	}
	dir := 1.0
	if sum < 0 {
		dir = -1
	}
	// confinement and consistency
	var clat, clon float64
	for i := 0; i < used; i++ {
		h := tr.histAt(i)
		clat += float64(h.lat)
		clon += float64(h.lon)
	}
	clat /= float64(used)
	clon /= float64(used)
	maxD := 0.0
	type seg struct{ dur, crs float64 }
	var legs [24]seg
	nl := 0
	runStart, runN := -1, 0
	var runC, runS float64
	straightFor := -1.0
	for i := 0; i < used-1; i++ {
		a, b := tr.histAt(i), tr.histAt(i+1)
		dd := haversineM(clat, clon, float64(a.lat), float64(a.lon)) / 1000
		if dd > maxD {
			maxD = dd
		}
		d := angDiff(float64(a.trk), float64(b.trk))
		dtt := a.t - b.t
		if d*dir < 0 {
			opp += math.Abs(d)
		}
		rate := math.Abs(d) / math.Max(dtt, 1)
		if rate < 0.5 { // straight (deg/s)
			if runStart < 0 {
				runStart = i
				runC, runS = 0, 0
				runN = 0
			}
			s, c := math.Sincos(float64(a.trk) * deg2rad)
			runS += s
			runC += c
			runN++
		} else {
			turnSum += rate
			turnN++
			if straightFor < 0 {
				straightFor = tr.histAt(0).t - a.t
				if runStart < 0 {
					straightFor = 0
				}
			}
			if runStart >= 0 {
				dur := tr.histAt(runStart).t - b.t
				if dur >= 20 && nl < len(legs) {
					legs[nl] = seg{dur, normDeg(math.Atan2(runS, runC) * rad2deg)}
					nl++
				}
				runStart = -1
			}
		}
	}
	if maxD > 20 || opp > 0.2*math.Abs(sum) || turnN == 0 {
		return hi
	}
	hi.ok = true
	hi.cLat, hi.cLon, hi.radiusKm = clat, clon, maxD
	hi.laps = math.Abs(sum) / 360
	hi.w = dir * math.Max(turnSum/float64(turnN), 1) * deg2rad
	if straightFor < 0 {
		straightFor = 0
	}
	hi.straightFor = straightFor
	if nl == 0 {
		hi.orbit = true
		return hi
	}
	ds := make([]float64, 0, 24)
	for i := 0; i < nl; i++ {
		ds = append(ds, legs[i].dur)
	}
	sort.Float64s(ds)
	hi.legSec = ds[len(ds)/2]
	hi.legCourse = legs[0].crs
	return hi
}

// HoldInfo describes a detected holding pattern or orbit.
type HoldInfo struct {
	Kind      string  `json:"kind"`      // "hold" (racetrack) | "orbit"
	Direction string  `json:"direction"` // "right" | "left"
	CenterLat float64 `json:"centerLat"`
	CenterLon float64 `json:"centerLon"`
	RadiusKm  float64 `json:"radiusKm"`
	LegCourse float64 `json:"legCourse,omitempty"` // course of the latest straight leg, deg true
	LegSec    float64 `json:"legSec,omitempty"`
	TurnDps   float64 `json:"turnDps"`
	Laps      float64 `json:"laps"`
}

// ApproachInfo describes an aircraft established on approach.
type ApproachInfo struct {
	Airport      string  `json:"airport,omitempty"`
	Runway       string  `json:"runway,omitempty"`
	DistKm       float64 `json:"distKm"`       // along-track distance to the threshold
	ETASec       float64 `json:"etaSec"`       // time to the threshold at the current ground speed
	CrossTrackM  float64 `json:"crossTrackM"`  // + = right of the extended centre line
	GlideDevFt   float64 `json:"glideDevFt"`   // altitude minus the 3 degree glide path
	HeightAGLFt  float64 `json:"heightAglFt"`  // above threshold elevation
	Decelerating bool    `json:"decelerating"` //
	Confidence   float64 `json:"confidence"`   // 0..1
}

// CPA is the closest point of approach of an aircraft to the receiver.
type CPA struct {
	InSec  float64 `json:"inSec"` // 0 = now (receding)
	T      int64   `json:"t"`     // Unix ms
	DistKm float64 `json:"distKm"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	AltFt  float64 `json:"alt"`
}

// LevelOff is the predicted end of the current climb or descent.
type LevelOff struct {
	InSec  float64 `json:"inSec"`
	AltFt  float64 `json:"alt"`
	Target string  `json:"target"` // "selected" | "ceiling" | "ground"
}

// Intent is the intent-aware analysis of one aircraft.
type Intent struct {
	ICAO  string `json:"icao"`
	T     int64  `json:"t"`
	Phase string `json:"phase"` // "ground","takeoff","climb","cruise","descent","approach","holding","turning","unknown"
	Plan  string `json:"plan"`  // turn plan used by the predictor
	// SelTrackDeg is the selected heading converted to a true-track target (when known).
	SelTrackDeg *float64      `json:"selTrack,omitempty"`
	Holding     *HoldInfo     `json:"holding,omitempty"`
	Approach    *ApproachInfo `json:"approach,omitempty"`
	CPA         *CPA          `json:"cpa,omitempty"`
	// ExitSec is the predicted time until the aircraft leaves coverage (needs a RangeFunc).
	ExitSec  *float64  `json:"exitSec,omitempty"`
	LevelOff *LevelOff `json:"levelOff,omitempty"`
	// DescentPending: level, and the selected altitude is well below: top of descent is near.
	DescentPending bool `json:"descentPending,omitempty"`
	ClimbPending   bool `json:"climbPending,omitempty"`
}

// levelOff computes the vertical target and time to reach it from a pstate.
func levelOff(ps *pstate, p *params) *LevelOff {
	if !ps.vOK || ps.plan == planGround || ps.altHold {
		return nil
	}
	h, r := ps.h+ps.hd*ps.vElapsed, ps.hd
	if math.Abs(r) < 5 { // < 300 ft/min
		return nil
	}
	lo := &LevelOff{}
	switch {
	case ps.selOK && (ps.selAlt-h)*r > 0 && math.Abs(ps.selAlt-h) > 50:
		lo.AltFt, lo.Target = ps.selAlt, "selected"
	case r > 0:
		lo.AltFt, lo.Target = math.Max(p.ceilFt, h), "ceiling"
	default:
		lo.AltFt, lo.Target = math.Min(ps.floorFt, h), "ground"
	}
	// time until within 1 % / 30 ft of the target with the capture model
	rem := math.Abs(lo.AltFt - h)
	band := math.Abs(r) * p.levelTau
	if rem > band {
		lo.InSec = (rem-band)/math.Abs(r) + p.levelTau*math.Log(band/30)
	} else {
		lo.InSec = p.levelTau * math.Log(math.Max(rem, 30)/30)
	}
	lo.InSec = fin(math.Max(lo.InSec, 0))
	return lo
}

// approach detection against supplied runways.
func detectApproach(lat, lon, trk, gsMS, altFt, vrFpm, accel float64, navApp bool, rws []Runway) *ApproachInfo {
	var best *ApproachInfo
	bestScore := 0.0
	for i := range rws {
		rw := &rws[i]
		if !finite(rw.Lat) || !finite(rw.Lon) || !finite(rw.HeadingDeg) {
			continue
		}
		dTrk := math.Abs(angDiff(trk, rw.HeadingDeg))
		if dTrk > 20 {
			continue
		}
		dist := haversineM(lat, lon, rw.Lat, rw.Lon)
		brgToThr := bearingDeg(lat, lon, rw.Lat, rw.Lon)
		along := dist * math.Cos(angDiff(brgToThr, rw.HeadingDeg)*deg2rad)
		cross := -dist * math.Sin(angDiff(brgToThr, rw.HeadingDeg)*deg2rad)
		if along < 0 || along > 40000 {
			continue
		}
		maxCross := 1500 + 0.05*along
		if math.Abs(cross) > maxCross {
			continue
		}
		hagl := altFt - rw.ElevationFt
		gp := along * math.Tan(3*deg2rad) * ftPerM
		dev := hagl - gp
		if hagl < -200 || hagl > 12000 {
			continue
		}
		descending := vrFpm < -200 || hagl < 1500
		if !descending && !navApp {
			continue
		}
		score := 1 - dTrk/40 - math.Abs(cross)/(2*maxCross)
		if math.Abs(dev) < 500 {
			score += 0.3
		}
		if navApp {
			score += 0.2
		}
		if accel < -0.05 {
			score += 0.1
		}
		if score > bestScore {
			bestScore = score
			eta := 0.0
			if gsMS > 1 {
				eta = along / gsMS
			}
			best = &ApproachInfo{
				Airport: rw.Airport, Runway: rw.Ident, DistKm: along / 1000, ETASec: eta,
				CrossTrackM: cross, GlideDevFt: dev, HeightAGLFt: hagl,
				Decelerating: accel < -0.05, Confidence: clamp(score/1.6, 0, 1),
			}
		}
	}
	if best != nil {
		best.DistKm, best.ETASec, best.CrossTrackM = fin(best.DistKm), fin(best.ETASec), fin(best.CrossTrackM)
		best.GlideDevFt, best.HeightAGLFt = fin(best.GlideDevFt), fin(best.HeightAGLFt)
	}
	return best
}

// cpaToPoint walks the predicted path (mean only) and returns the time and
// state of minimum great-circle distance to (rlat, rlon) within horizon.
func cpaToPoint(ps0 pstate, p *params, rlat, rlon, horizon float64) (tBest, dBest float64, best pstate) {
	const step = 10.0
	ps := ps0
	lat, lon := ps.fr.toLatLon(ps.x[ix], ps.x[iy])
	dBest = haversineM(lat, lon, rlat, rlon)
	best = ps
	prev := ps
	var tPrev float64
	bestPrev := ps
	tBestPrev := 0.0
	for t := step; t <= horizon+1e-9; t += step {
		prev = ps
		ps.propagate(step, p, false)
		la, lo := ps.fr.toLatLon(ps.x[ix], ps.x[iy])
		d := haversineM(la, lo, rlat, rlon)
		if d < dBest {
			dBest, tBest, best = d, t, ps
			bestPrev, tBestPrev = prev, tPrev
		}
		tPrev = t
		if d > dBest+50000 {
			break
		}
	}
	if tBest == 0 {
		return 0, dBest, best
	}
	// refine in [tBest-step, tBest+step] by 1 s steps
	ps = bestPrev
	tt := tBestPrev
	for i := 0; i < int(2*step); i++ {
		ps.propagate(1, p, false)
		tt++
		la, lo := ps.fr.toLatLon(ps.x[ix], ps.x[iy])
		d := haversineM(la, lo, rlat, rlon)
		if d < dBest {
			dBest, tBest, best = d, tt, ps
		}
	}
	return tBest, dBest, best
}

// exitTime finds when the predicted path first exceeds the coverage range.
func exitTime(ps0 pstate, p *params, rlat, rlon float64, rf RangeFunc, horizon float64) (float64, bool) {
	const step = 10.0
	ps := ps0
	outside := func(ps *pstate) bool {
		la, lo := ps.fr.toLatLon(ps.x[ix], ps.x[iy])
		alt, _, _ := ps.vertical(p)
		d := haversineM(rlat, rlon, la, lo) / 1000
		r := rf(bearingDeg(rlat, rlon, la, lo), alt)
		return finite(r) && r > 0 && d > r
	}
	if outside(&ps) {
		return 0, false
	}
	for t := step; t <= horizon+1e-9; t += step {
		prev := ps
		ps.propagate(step, p, false)
		if outside(&ps) {
			lo, hi := t-step, t
			q := prev
			for i := 0; i < 5; i++ {
				mid := (lo + hi) / 2
				c := q
				c.propagate(mid-lo, p, false)
				if outside(&c) {
					hi = mid
				} else {
					lo, q = mid, c
				}
			}
			return (lo + hi) / 2, true
		}
	}
	return 0, false
}
