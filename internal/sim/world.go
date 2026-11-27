package sim

import "math"

// kind is the behavioural class of a simulated aircraft.
type kind uint8

const (
	kOverflight kind = iota
	kArrival
	kDeparture
	kGA
	kHeli
	kMil
	numKinds
)

// milSpawnFactor compensates the longer dwell time of military aircraft, see
// randomKind.
const milSpawnFactor = 0.62

var kindNames = [numKinds]string{"overflight", "arrival", "departure", "ga", "helicopter", "military"}

// phase is the flight phase state of an aircraft.
type phase uint8

const (
	phNav      phase = iota // airborne, flying the waypoint route
	phClimbOut              // airborne, runway heading until 1500 ft AGL
	phHold                  // airborne, racetrack holding pattern
	phFinal                 // airborne, established on the 3 degree glide path
	phFlare                 // airborne, past the threshold, about to touch down
	phHover                 // helicopter hovering
	phTaxiOut               // on ground, taxiing to the runway
	phLineUp                // on ground, lining up / waiting for take-off
	phTakeoff               // on ground, take-off roll
	phRollout               // on ground, decelerating after landing
	phTaxiIn                // on ground, taxiing to the apron
	phParked                // on ground, stationary before shutting down
)

var phaseNames = [...]string{
	"enroute", "climbout", "hold", "final", "flare", "hover",
	"taxi-out", "line-up", "takeoff", "rollout", "taxi-in", "parked",
}

const (
	wpHold uint8 = 1 << iota // enter the holding pattern at this fix
)

type waypoint struct {
	lat, lon float64
	legKm    float64 // distance from the previous waypoint
	flags    uint8
}

const maxRoute = 5

// link is the cached mean link budget of one aircraft towards one antenna.
type link struct {
	mean  float32 // mean received level, dBFS, before fast fading
	sigma float32 // scatter amplitude of the Rician fading model
}

// airport is a synthetic airport with a single runway.
type airport struct {
	lat, lon           float64
	elevFt             float64
	rwyHdg             float64 // landing / take-off direction, deg true
	thrLat, thrLon     float64 // runway threshold (start of take-off roll)
	holdLat, holdLon   float64 // holding point beside the threshold
	apronLat, apronLon float64
	fafLat, fafLon     float64 // final approach fix on the extended centreline
}

const (
	finalKm = 16.0 // distance from the final approach fix to the threshold
	// glide path: 3 degrees, crossing the threshold at 50 ft
	gpFtPerKm  = 1000 * 0.052408 / ftToM
	platformFt = 50 + finalKm*gpFtPerKm
)

// kin is the kinematic state of an aircraft. It is valid at the start of a
// step; dlat/dlon/vrate/trkRate are the rates used to extrapolate inside the
// step. Every transmission of a step is encoded from exactly this state, so
// it is also what ground truth reports.
type kin struct {
	lat, lon   float64 // deg
	alt        float64 // pressure altitude, ft
	trk        float64 // true track, deg
	gs         float64 // kt
	tas        float64 // kt
	vrate      float64 // ft/min
	roll       float64 // deg, + = right wing down
	trkRate    float64 // deg/s
	dlat, dlon float64 // deg/s
	windN      float64 // kt, air mass motion
	windE      float64
	onGround   bool
}

// at extrapolates the position to dt seconds after the step start.
func (k *kin) at(dt float64) (lat, lon, alt float64) {
	lat = k.lat + k.dlat*dt
	lon = k.lon + k.dlon*dt
	alt = k.alt
	if !k.onGround {
		alt += k.vrate / 60 * dt
	}
	return lat, lon, alt
}

// aircraft is the complete state of one simulated aircraft: the kinematic
// state of the current step (embedded), that of the previous step (the one
// the most recently delivered frames were generated from), and everything
// else.
type aircraft struct {
	// identity / equipage
	icao      uint32
	callsign  [8]byte
	kind      kind
	catTC     uint8 // identification type code (4 = category set A ...)
	cat       uint8 // emitter category within the set
	version   uint8 // ADS-B version (0 or 2)
	posTC     uint8 // airborne position type code (NIC)
	nacp      uint8
	nacv      uint8
	sil       uint8
	lenWidth  uint8
	squawk    uint16
	adsb      bool // transmits extended squitter
	mlat      bool // Mode S only, positioned by synthetic MLAT results
	gnssPos   bool // sends TC20 (GNSS height) instead of baro positions
	ehs       bool // answers Comm-B EHS registers 4,0 / 5,0 / 6,0
	mrar      bool // answers Comm-B register 4,4
	acas      bool
	diversity bool // top + bottom antenna, squitters alternate
	singleAnt bool
	utcT      bool  // UTC-coupled position source: airborne positions carry T=1
	t1Lead    int64 // T=1: register load time before the 0.2 s epoch, ns (50..150 ms)
	vrBaro    bool  // vertical rate source reported in TC19
	eirp      float64
	survRate  float64 // DF4/5/20/21 replies per second
	acasRate  float64 // DF0 replies per second
	longFrac  float64 // fraction of surveillance replies that are Comm-B

	// kinematic state
	kin
	prev   kin // state at the start of the previous step
	prev2  kin // and of the one before (frames received one step late)
	cosLat float64

	// guidance
	phase     phase
	route     [maxRoute]waypoint
	wpIdx     uint8
	wpN       uint8
	wpDist    float64 // km to the active waypoint
	desTrk    float64
	forceTurn float64 // +1 / -1 forces the turn direction while far off track
	tgtAlt    float64
	ffVrate   float64 // feed-forward vertical rate (path following), ft/min
	tgtTAS    float64
	climbRate float64
	descRate  float64
	maxBank   float64
	rollRate  float64 // deg/s
	accel     float64 // kt/s
	cruiseAlt float64
	cruiseCAS float64
	cruiseM   float64
	cruiseTAS float64 // GA / helicopters
	vapp      float64 // approach speed, kt CAS
	vr        float64 // rotation speed, kt
	gndElev   float64 // ft, terrain below (airport elevation for arrivals/departures)
	apt       int8
	holdLaps  int8
	holdLeg   uint8
	holdInb   float64
	timer     float64 // phase timer, s
	life      float64 // remaining lifetime for wandering traffic, s
	ending    bool
	slowT     float64 // time to the next guidance update
	slowDt    float64
	linkT     float64

	// status
	emerg     uint8 // ADS-B emergency state (0 = none)
	sqChanged float64
	born      float64
	dead      bool

	// transmit schedule (world seconds)
	nextPos, nextVel, nextIdent float64
	nextTSS, nextOps, nextEmg   float64
	nextAllCall, nextSurv       float64
	nextACAS, nextMLAT          float64
	cprOdd                      bool
	top                         bool

	// reception
	link      [maxAnt]link
	topPen    float32 // extra loss when radiating from the top antenna, dB
	shadow    float64 // slow aspect-dependent shadowing, dB
	stDist    float64 // km from the station
	slantKm   float64 // slant range to the first antenna, km (propagation delay)
	stBrg     float64 // deg true from the station
	stElev    float64 // elevation angle seen from the first antenna, deg
	lastHeard float64
}

func (a *aircraft) setRoute(pts ...waypoint) {
	a.wpN = uint8(copy(a.route[:], pts))
	a.wpIdx = 0
	for i := 1; i < int(a.wpN); i++ {
		a.route[i].legKm, _ = distBearing(a.route[i-1].lat, a.route[i-1].lon, a.route[i].lat, a.route[i].lon)
	}
}

// remAfter is the route length (km) beyond the active waypoint.
func (a *aircraft) remAfter() float64 {
	var d float64
	for i := int(a.wpIdx) + 1; i < int(a.wpN); i++ {
		d += a.route[i].legKm
	}
	return d
}

// fs returns the Mode S flight status field.
func (a *aircraft) fs(t float64) uint8 {
	alert := a.emerg != 0 || t-a.sqChanged < 18
	switch {
	case alert && a.onGround:
		return 3
	case alert:
		return 2
	case a.onGround:
		return 1
	}
	return 0
}

func (a *aircraft) jet() bool {
	return a.kind == kOverflight || a.kind == kArrival || a.kind == kDeparture || a.kind == kMil
}

// selAlt is the altitude shown in the MCP/FCU window.
func (a *aircraft) selAlt() float64 {
	v := a.tgtAlt
	if a.kind == kArrival {
		switch a.phase {
		case phFinal, phFlare:
			v = a.gndElev + platformFt
		default:
			v = math.Floor(v/1000) * 1000
		}
	}
	return math.Max(100, math.Round(v/100)*100)
}

// --- airports ----------------------------------------------------------------

func (s *Sim) buildAirports() {
	type def struct{ brg, km, hdg, dElevM float64 }
	defs := []def{
		{75, 12, 265, -6},
		{205, 62, 45, 25},
		{320, 105, 140, 60},
		{110, 140, 10, -4},
	}
	// rotate the whole layout by a seed-dependent angle so different seeds
	// give different pictures
	rot := s.rng.Float64() * 360
	for _, d := range defs {
		var ap airport
		ap.lat, ap.lon = destination(s.st.lat, s.st.lon, d.brg+rot, d.km)
		ap.elevFt = math.Max(0, s.st.groundM+d.dElevM) / ftToM
		ap.rwyHdg = wrap360(d.hdg + rot)
		ap.thrLat, ap.thrLon = destination(ap.lat, ap.lon, ap.rwyHdg+180, 1.5)
		ap.holdLat, ap.holdLon = destination(ap.thrLat, ap.thrLon, ap.rwyHdg+90, 0.12)
		ap.apronLat, ap.apronLon = destination(ap.lat, ap.lon, ap.rwyHdg+90, 0.45)
		ap.fafLat, ap.fafLon = destination(ap.thrLat, ap.thrLon, ap.rwyHdg+180, finalKm)
		s.apts = append(s.apts, ap)
	}
}

// --- spawning ----------------------------------------------------------------

func (s *Sim) uni(lo, hi float64) float64 { return lo + (hi-lo)*s.rng.Float64() }

// expo draws an exponential waiting time for a Poisson process of the given rate.
func (s *Sim) expo(rate float64) float64 {
	if rate <= 0 {
		return 1e9
	}
	return math.Min(-math.Log(1-s.rng.Float64())/rate, 60/rate)
}

func (s *Sim) randomKind() kind {
	w := [numKinds]float64{kOverflight: 0.50, kArrival: 0.17, kDeparture: 0.17, kGA: 0.11, kHeli: 0.05}
	if h := s.env.localHour; h < 6 || h > 21.5 {
		w[kGA], w[kHeli] = 0.01, 0.02
	}
	// Military traffic takes its configured share of the picture. It stays
	// in the area about one and a half times as long as the average civil
	// flight, so it is spawned correspondingly less often.
	var civil float64
	for _, v := range w {
		civil += v
	}
	if f := s.cfg.MilitaryFraction; f > 0 {
		w[kMil] = milSpawnFactor * civil * f / (1 - f)
	}
	return kind(s.pickIndex(int(numKinds), func(i int) float64 { return w[i] }))
}

// spawn creates one aircraft. initial is true while seeding the picture at
// start-up, when aircraft are placed mid-flight instead of at the edge.
func (s *Sim) spawn(k kind, initial bool, t float64) {
	if len(s.ac) >= s.maxAC {
		return
	}
	var a aircraft
	a.kind = k
	a.born = t
	a.apt = -1
	a.slowDt = 1
	a.gndElev = s.st.groundM / ftToM
	a.maxBank = 25
	a.rollRate = 5
	a.accel = 1.2
	a.descRate = 3000
	a.climbRate = 2000
	s.identify(&a)

	R := s.cfg.RangeKm
	r := s.rng
	switch k {
	case kOverflight:
		b1 := s.uni(0, 360)
		b2 := b1 + 180 + s.uni(-110, 110)
		elat, elon := destination(s.st.lat, s.st.lon, b1, R)
		xlat, xlon := destination(s.st.lat, s.st.lon, b2, R*1.08)
		a.setRoute(waypoint{lat: xlat, lon: xlon})
		d, brg := distBearing(elat, elon, xlat, xlon)
		a.lat, a.lon = elat, elon
		if initial {
			a.lat, a.lon = destination(elat, elon, brg, d*s.uni(0.03, 0.9))
			_, brg = distBearing(a.lat, a.lon, xlat, xlon)
		}
		a.trk = brg
		// semicircular rule: eastbound odd levels, westbound even
		fl := 300 + 20*float64(r.IntN(6)) // 300..400 even
		if brg < 180 {
			fl += 10 // 310..410 odd
		}
		a.alt = fl * 100
		a.cruiseAlt, a.tgtAlt = a.alt, a.alt
		a.cruiseCAS = 340
		// Choose the Mach number that yields a ground speed of 430-490 kt in
		// the current wind.
		s.env2wind(&a)
		want := s.uni(430, 490)
		sn, cs := math.Sincos(a.trk * deg2rad)
		tas := math.Hypot(want*cs-a.windN, want*sn-a.windE)
		// (capped so that the true airspeed stays below 495 kt, as for real airliners)
		snd := soundKt(isaTempK(a.alt) + s.env.dT)
		a.cruiseM = clamp(tas/snd, 0.70, math.Min(0.87, 495/snd))

	case kArrival:
		a.apt = int8(r.IntN(len(s.apts)))
		ap := &s.apts[a.apt]
		a.gndElev = ap.elevFt
		b1 := s.uni(0, 360)
		elat, elon := destination(s.st.lat, s.st.lon, b1, R)
		// Build the approach: optional downwind point, base point, FAF, threshold.
		axis := wrap360(ap.rwyHdg + 180) // direction from the threshold out along the approach
		_, toAc := distBearing(ap.lat, ap.lon, elat, elon)
		rel := wrap180(toAc - axis)
		side := 1.0
		if rel < 0 {
			side = -1
		}
		blat, blon := destination(ap.fafLat, ap.fafLon, axis, 9)
		blat, blon = destination(blat, blon, axis+90*side, 9*math.Min(1, math.Abs(rel)/45))
		base := waypoint{lat: blat, lon: blon}
		if r.Float64() < 0.2 {
			base.flags |= wpHold
			a.holdLaps = int8(2 + r.IntN(3))
		}
		faf := waypoint{lat: ap.fafLat, lon: ap.fafLon}
		thr := waypoint{lat: ap.thrLat, lon: ap.thrLon}
		if math.Abs(rel) > 100 {
			dlat, dlon := destination(ap.lat, ap.lon, axis+90*side, 12)
			a.setRoute(waypoint{lat: dlat, lon: dlon}, base, faf, thr)
		} else {
			a.setRoute(base, faf, thr)
		}
		a.cruiseAlt = 100 * (300 + 10*float64(r.IntN(10)))
		a.cruiseCAS = s.uni(270, 300)
		a.cruiseM = s.uni(0.74, 0.82)
		a.vapp = s.uni(128, 150)
		d, brg := distBearing(elat, elon, a.route[0].lat, a.route[0].lon)
		a.lat, a.lon = elat, elon
		if initial {
			a.lat, a.lon = destination(elat, elon, brg, d*s.uni(0.05, 0.92))
			d, brg = distBearing(a.lat, a.lon, a.route[0].lat, a.route[0].lon)
		}
		a.trk = brg
		a.wpDist = d
		dtg := d + a.remAfter()
		req := a.gndElev + platformFt + math.Max(0, dtg-finalKm)*180
		a.alt = math.Min(a.cruiseAlt, req)
		a.tgtAlt = a.alt

	case kDeparture:
		a.apt = int8(r.IntN(len(s.apts)))
		ap := &s.apts[a.apt]
		a.gndElev = ap.elevFt
		a.cruiseAlt = 100 * (260 + 10*float64(r.IntN(14)))
		a.cruiseCAS = s.uni(280, 310)
		a.cruiseM = s.uni(0.74, 0.82)
		a.vr = s.uni(138, 158)
		xlat, xlon := destination(s.st.lat, s.st.lon, s.uni(0, 360), R*1.08)
		exit := waypoint{lat: xlat, lon: xlon}
		if initial && r.Float64() < 0.65 {
			// already airborne, somewhere in the climb
			d, brg := distBearing(ap.lat, ap.lon, xlat, xlon)
			along := math.Min(s.uni(8, 300), d*0.9)
			a.lat, a.lon = destination(ap.lat, ap.lon, brg, along)
			_, a.trk = distBearing(a.lat, a.lon, xlat, xlon)
			a.alt = math.Min(a.cruiseAlt, a.gndElev+1500+260*along)
			a.tgtAlt = a.cruiseAlt
			a.setRoute(exit)
		} else {
			a.onGround = true
			a.phase = phTaxiOut
			a.lat, a.lon = ap.apronLat, ap.apronLon
			a.alt = a.gndElev
			a.tgtAlt = a.cruiseAlt
			a.trk = wrap360(ap.rwyHdg + 180)
			a.slowDt = 0.2
			a.setRoute(waypoint{lat: ap.holdLat, lon: ap.holdLon}, waypoint{lat: ap.thrLat, lon: ap.thrLon})
			// the en-route exit is remembered in the last route slot
			a.route[maxRoute-1] = exit
		}

	case kGA, kHeli:
		maxKm, loAGL, hiAGL := 110.0, 1200.0, 6500.0
		a.cruiseTAS = s.uni(85, 160)
		a.maxBank, a.rollRate, a.accel = 30, 12, 2
		a.climbRate, a.descRate = 700, 700
		if k == kHeli {
			maxKm, loAGL, hiAGL = 65, 500, 2500
			a.cruiseTAS = s.uni(70, 135)
			a.climbRate, a.descRate = 900, 900
		}
		a.lat, a.lon = destination(s.st.lat, s.st.lon, s.uni(0, 360), s.uni(4, maxKm))
		a.tgtAlt = a.gndElev + 100*math.Round(s.uni(loAGL, hiAGL)/100)
		a.alt = a.tgtAlt
		a.life = s.uni(900, 4200)
		if initial {
			a.life *= s.uni(0.2, 1)
		} else {
			// just departed from a strip below coverage: still climbing
			a.alt = a.gndElev + s.uni(150, 400)
		}
		s.wanderWaypoint(&a)
		_, a.trk = distBearing(a.lat, a.lon, a.route[0].lat, a.route[0].lon)

	case kMil:
		a.lat, a.lon = destination(s.st.lat, s.st.lon, s.uni(0, 360), R)
		if initial {
			a.lat, a.lon = destination(s.st.lat, s.st.lon, s.uni(0, 360), s.uni(30, R*0.8))
		}
		if r.Float64() < 0.5 { // fast jet
			a.cruiseM = s.uni(0.70, 0.80)
			a.cruiseCAS = s.uni(320, 380)
			a.maxBank, a.rollRate, a.accel = 45, 30, 4
			a.climbRate, a.descRate = 5000, 5000
			a.alt = 100 * math.Round(s.uni(120, 280))
		} else { // transport / tanker
			a.cruiseM = s.uni(0.55, 0.72)
			a.cruiseCAS = s.uni(240, 280)
			a.alt = 1000 * math.Round(s.uni(18, 27))
		}
		a.cruiseAlt, a.tgtAlt = a.alt, a.alt
		a.life = s.uni(900, 3000)
		s.wanderWaypoint(&a)
		_, a.trk = distBearing(a.lat, a.lon, a.route[0].lat, a.route[0].lon)
	}

	// settle the derived state
	a.desTrk = a.trk
	a.cosLat = math.Cos(a.lat * deg2rad)
	s.env2wind(&a)
	s.setPerformance(&a)
	if !a.onGround {
		a.tas = a.tgtTAS
		a.vrate = clamp((a.tgtAlt-a.alt)*8, -a.descRate, a.climbRate)
		if a.kind == kArrival && a.ffVrate != 0 {
			a.vrate = a.ffVrate
		}
	}
	s.dynamics(&a)
	a.shadow = 2 * r.NormFloat64()
	s.updateLink(&a)
	a.slowT = s.uni(0, a.slowDt)
	a.linkT = s.uni(0, 1)

	a.nextPos = t + s.uni(0, 0.5)
	a.nextVel = t + s.uni(0, 0.5)
	a.nextIdent = t + s.uni(0, 5)
	a.nextTSS = t + s.uni(0, 1.25)
	a.nextOps = t + s.uni(0, 2.5)
	a.nextEmg = t + s.uni(0, 5)
	a.nextAllCall = t + s.uni(0, 1)
	a.nextSurv = t + s.expo(a.survRate)
	a.nextACAS = t + s.expo(a.acasRate)
	a.nextMLAT = t + s.uni(0, 4)
	a.lastHeard = -1e9
	a.prev = a.kin
	a.prev2 = a.kin

	s.ac = append(s.ac, a)
	s.spawned++
}

// env2wind caches the wind at the aircraft's level.
func (s *Sim) env2wind(a *aircraft) {
	if a.onGround {
		a.windN, a.windE = 0, 0
		return
	}
	a.windN, a.windE = s.windVec(a.alt)
}

// wanderWaypoint gives a wandering aircraft (GA, helicopter, military) its
// next random destination.
func (s *Sim) wanderWaypoint(a *aircraft) {
	maxKm := 110.0
	switch a.kind {
	case kHeli:
		maxKm = 65
	case kMil:
		maxKm = s.cfg.RangeKm * 0.75
	}
	lat, lon := destination(s.st.lat, s.st.lon, s.uni(0, 360), s.uni(3, maxKm))
	a.setRoute(waypoint{lat: lat, lon: lon})
}

// --- guidance (runs at 1 Hz per aircraft, 5 Hz near and on the ground) --------

func (s *Sim) slowUpdate(a *aircraft, t float64) {
	dt := a.slowDt
	a.cosLat = math.Cos(a.lat * deg2rad)
	s.env2wind(a)
	s.navigate(a, t, dt)
	if a.dead {
		return
	}
	s.setPerformance(a)

	a.linkT -= dt
	if a.linkT <= 0 {
		a.linkT += 1
		// slow aspect shadowing: Ornstein-Uhlenbeck, ~30 s correlation, 2 dB sigma
		a.shadow = 0.97*a.shadow + 0.486*s.rng.NormFloat64()
		s.updateLink(a)
		s.hazards(a, t)
		R := s.cfg.RangeKm
		if a.stDist > R*1.03 && t-a.born > 120 {
			a.dead = true
		}
	}
}

// hazards rolls the once-per-second random events of an aircraft.
func (s *Sim) hazards(a *aircraft, t float64) {
	r := s.rng
	if a.emerg == 0 && !a.onGround && s.cfg.EmergencyPerHour > 0 && r.Float64() < s.cfg.EmergencyPerHour/3600 {
		s.declareEmergency(a, t)
	}
	// ATC hand-off: new discrete code now and then
	if a.emerg == 0 && a.jet() && !a.onGround && a.squawk != 0x1000 && r.Float64() < 1.0/1500 {
		a.squawk = s.discreteSquawk()
		a.sqChanged = t
	}
	switch a.kind {
	case kOverflight:
		// occasional step climb / descent
		if r.Float64() < 1.0/2400 && math.Abs(a.tgtAlt-a.alt) < 50 {
			step := 2000.0
			if r.Float64() < 0.4 {
				step = -2000
			}
			a.tgtAlt = clamp(a.tgtAlt+step, 29000, 41000)
		}
	case kHeli:
		if a.phase == phNav && !a.ending && r.Float64() < 1.0/300 {
			a.phase = phHover
			a.timer = s.uni(40, 200)
		}
	}
}

func (s *Sim) declareEmergency(a *aircraft, t float64) {
	switch x := s.rng.Float64(); {
	case x < 0.62:
		a.emerg, a.squawk = 1, 0x7700 // general
	case x < 0.72:
		a.emerg, a.squawk = 2, 0x7700 // medical
	case x < 0.80:
		a.emerg, a.squawk = 3, 0x7700 // minimum fuel
	case x < 0.98:
		a.emerg, a.squawk = 4, 0x7600 // no communications
	default:
		a.emerg, a.squawk = 5, 0x7500 // unlawful interference
	}
	a.sqChanged = t
}

func (s *Sim) captureKm(a *aircraft) float64 {
	switch {
	case a.onGround:
		return 0.02
	case a.phase == phFinal:
		return 0.12
	}
	return math.Max(0.8, a.gs*ktToMps*12/1000)
}

func (s *Sim) navigate(a *aircraft, t, dt float64) {
	switch a.phase {
	case phNav, phFinal, phTaxiOut, phTaxiIn:
		if a.wpIdx < a.wpN {
			wp := &a.route[a.wpIdx]
			a.wpDist, a.desTrk = distBearing(a.lat, a.lon, wp.lat, wp.lon)
			if a.wpDist < s.captureKm(a) {
				s.onWaypoint(a)
			}
		}
		if a.ending && a.alt < a.gndElev+300 {
			a.dead = true // landed at a strip below coverage
		}
	case phClimbOut:
		if a.alt > a.gndElev+1500 {
			a.phase = phNav
			a.slowDt = 1
		}
	case phHold:
		a.timer -= dt
		err := math.Abs(wrap180(a.desTrk - a.trk))
		switch a.holdLeg {
		case 0: // turning outbound
			if err < 4 {
				a.holdLeg, a.timer, a.forceTurn = 1, 60, 0
			}
		case 1: // outbound leg
			if a.timer <= 0 {
				a.holdLeg, a.forceTurn, a.desTrk = 2, 1, a.holdInb
			}
		case 2: // turning inbound
			if err < 4 {
				a.holdLeg, a.timer, a.forceTurn = 3, 60, 0
			}
		case 3: // inbound leg
			if a.timer <= 0 {
				a.holdLaps--
				if a.holdLaps <= 0 {
					a.phase = phNav
					s.nextWaypoint(a)
				} else {
					a.holdLeg, a.forceTurn, a.desTrk = 0, 1, wrap360(a.holdInb+180)
				}
			}
		}
	case phHover:
		a.timer -= dt
		if a.timer <= 0 {
			a.phase = phNav
		}
	case phLineUp:
		a.timer -= dt
		if a.timer <= 0 && a.gs < 0.5 && math.Abs(wrap180(a.desTrk-a.trk)) < 0.5 {
			a.phase = phTakeoff
			a.trk = a.desTrk
		}
	case phParked:
		a.timer -= dt
		if a.timer <= 0 {
			a.dead = true // transponder switched off
		}
	}
}

// onWaypoint handles reaching the active waypoint.
func (s *Sim) onWaypoint(a *aircraft) {
	wp := &a.route[a.wpIdx]
	if wp.flags&wpHold != 0 && a.holdLaps > 0 && a.phase == phNav {
		a.phase = phHold
		a.holdInb = a.trk
		a.holdLeg = 0
		a.forceTurn = 1
		a.desTrk = wrap360(a.trk + 180)
		return
	}
	s.nextWaypoint(a)
}

func (s *Sim) nextWaypoint(a *aircraft) {
	a.wpIdx++
	if a.wpIdx < a.wpN {
		wp := &a.route[a.wpIdx]
		a.wpDist, a.desTrk = distBearing(a.lat, a.lon, wp.lat, wp.lon)
		if a.kind == kArrival && a.wpIdx == a.wpN-1 && !a.onGround {
			a.phase = phFinal
			a.slowDt = 0.2
		}
		return
	}
	// route exhausted
	switch a.phase {
	case phFinal:
		a.phase = phFlare
		a.desTrk = s.apts[a.apt].rwyHdg
	case phTaxiOut:
		a.phase = phLineUp
		a.desTrk = s.apts[a.apt].rwyHdg
		a.timer = s.uni(8, 40)
	case phTaxiIn:
		a.phase = phParked
		a.timer = s.uni(20, 90)
	case phNav:
		switch a.kind {
		case kGA, kHeli:
			if a.life <= 0 {
				a.ending = true
				a.tgtAlt = a.gndElev + 150
			}
			s.wanderWaypoint(a)
		case kMil:
			if a.life <= 0 {
				lat, lon := destination(s.st.lat, s.st.lon, s.uni(0, 360), s.cfg.RangeKm*1.1)
				a.setRoute(waypoint{lat: lat, lon: lon})
			} else {
				s.wanderWaypoint(a)
				if s.rng.Float64() < 0.5 {
					a.tgtAlt = 1000 * math.Round(s.uni(10, 30))
				}
			}
		}
	}
}

// setPerformance derives the speed target and climb/descent limits from the
// flight phase, and the altitude target for arrivals.
func (s *Sim) setPerformance(a *aircraft) {
	if a.onGround {
		a.ffVrate = 0
		return
	}
	if a.kind == kGA || a.kind == kHeli {
		a.life -= a.slowDt
		a.tgtTAS = a.cruiseTAS
		if a.phase == phHover {
			a.tgtTAS = 0
		}
		return
	}
	if a.kind == kMil {
		a.life -= a.slowDt
	}
	p := isaPressHPa(a.alt)
	tK := isaTempK(a.alt) + s.env.dT
	agl := a.alt - a.gndElev
	cas := a.cruiseCAS
	a.ffVrate = 0
	if a.kind != kMil {
		a.climbRate = clamp(3000-2300*a.alt/41000, 500, 3000)
		if a.alt < 10000 {
			cas = math.Min(cas, 250)
		}
	}
	switch a.kind {
	case kDeparture:
		if agl < 3000 {
			cas = math.Min(cas, 170+40*clamp(agl/3000, 0, 1))
		}
	case kArrival:
		plat := a.gndElev + platformFt
		switch a.phase {
		case phNav, phHold:
			dtg := a.wpDist + a.remAfter()
			req := plat + math.Max(0, dtg-finalKm)*180
			if req < a.cruiseAlt {
				a.tgtAlt = math.Max(req, plat)
				if a.phase == phNav && req > plat && a.alt < req+400 {
					a.ffVrate = -a.gs * 1.852 / 60 * 180
				}
			} else {
				a.tgtAlt = a.cruiseAlt
			}
			if dtg < 45 || a.phase == phHold {
				cas = math.Min(cas, 215)
			}
			if dtg < 28 {
				cas = math.Min(cas, 185)
			}
		case phFinal:
			gp := a.gndElev + 50 + a.wpDist*gpFtPerKm
			if gp < plat {
				a.tgtAlt = gp
				a.ffVrate = -a.gs * 1.852 / 60 * gpFtPerKm
			} else {
				a.tgtAlt = plat
			}
			a.descRate = 1500
			cas = a.vapp
			if a.wpDist > 9 {
				cas += 25
			}
		case phFlare:
			a.tgtAlt = a.gndElev - 200
			a.descRate = 600
			cas = a.vapp - 5
		}
	}
	a.tgtTAS = math.Min(machFromCAS(cas, p), a.cruiseM) * soundKt(tK)
}

// --- dynamics (every step) ----------------------------------------------------

// dynamics advances the control loops by one step and computes the rates used
// to extrapolate the state during the next step.
func (s *Sim) dynamics(a *aircraft) {
	sn, cs := math.Sincos(a.trk * deg2rad)
	if a.onGround {
		s.groundDynamics(a)
	} else {
		// lateral: bank-to-turn towards the desired track
		err := wrap180(a.desTrk - a.trk)
		var want float64
		if a.forceTurn != 0 && math.Abs(err) > 25 {
			want = a.forceTurn * a.maxBank
		} else {
			want = clamp(err*1.5, -a.maxBank, a.maxBank)
		}
		a.roll += clamp(want-a.roll, -a.rollRate*stepDt, a.rollRate*stepDt)
		v := math.Max(a.tas, 40) * ktToMps
		a.trkRate = gravity * math.Tan(a.roll*deg2rad) / v * rad2deg

		// vertical: capture the target altitude
		dAlt := a.tgtAlt - a.alt
		cmd := clamp(dAlt*8+a.ffVrate, -a.descRate, a.climbRate)
		if math.Abs(dAlt) < 10 && a.ffVrate == 0 {
			cmd = 0
		}
		lim := 400 * stepDt
		a.vrate += clamp(cmd-a.vrate, -lim, lim)

		// speed
		a.tas += clamp(a.tgtTAS-a.tas, -1.5*a.accel*stepDt, a.accel*stepDt)

		// wind triangle: ground speed along the track. Hovering and very
		// slow helicopters hold their position against the wind; the wind
		// drift is blended in between 25 and 45 kt.
		a.gs = a.tas
		if a.tas > 25 {
			cross := a.windE*cs - a.windN*sn
			along := a.windN*cs + a.windE*sn
			g := math.Max(a.tas*0.5, 5)
			if q := a.tas*a.tas - cross*cross; q > 1 {
				g = math.Max(along+math.Sqrt(q), 5)
			}
			a.gs += clamp((a.tas-25)/20, 0, 1) * (g - a.tas)
		}
	}
	v := a.gs * ktToMps * degPerM
	a.dlat = v * cs
	a.dlon = v * sn / a.cosLat
}

func (s *Sim) groundDynamics(a *aircraft) {
	a.roll, a.vrate = 0, 0
	err := wrap180(a.desTrk - a.trk)
	switch a.phase {
	case phTaxiOut, phTaxiIn:
		a.trkRate = clamp(err/stepDt, -12, 12)
		tgt := 15.0
		if math.Abs(err) > 30 {
			tgt = 6
		}
		a.gs += clamp(tgt-a.gs, -3*stepDt, 2*stepDt)
	case phLineUp:
		a.trkRate = clamp(err/stepDt, -10, 10)
		a.gs = math.Max(0, a.gs-3*stepDt)
	case phTakeoff:
		a.trkRate = 0
		a.gs += 3 * stepDt
		if a.gs >= a.vr {
			// rotate
			a.onGround = false
			a.phase = phClimbOut
			a.slowDt = 1
			a.vrate = 0
			exit := a.route[maxRoute-1]
			a.setRoute(exit)
			a.wpDist, _ = distBearing(a.lat, a.lon, exit.lat, exit.lon)
		}
	case phRollout:
		a.trkRate = 0
		a.gs = math.Max(0, a.gs-3.5*stepDt)
		if a.gs <= 22 {
			ap := &s.apts[a.apt]
			a.phase = phTaxiIn
			a.setRoute(waypoint{lat: ap.apronLat, lon: ap.apronLon})
		}
	case phParked:
		a.trkRate = 0
		a.gs = math.Max(0, a.gs-3*stepDt)
	}
	a.tas = a.gs
}

// move integrates one step and then prepares the next one.
func (s *Sim) move(a *aircraft, t float64) {
	a.prev2 = a.prev
	a.prev = a.kin
	a.lat += a.dlat * stepDt
	a.lon += a.dlon * stepDt
	if a.lon > 180 {
		a.lon -= 360
	} else if a.lon < -180 {
		a.lon += 360
	}
	a.lat = clamp(a.lat, -89.9, 89.9)
	a.trk = wrap360(a.trk + a.trkRate*stepDt)
	if !a.onGround {
		a.alt += a.vrate / 60 * stepDt
		if a.phase == phFlare && a.alt <= a.gndElev {
			// touchdown
			a.onGround = true
			a.alt = a.gndElev
			a.vrate, a.roll, a.trkRate = 0, 0, 0
			a.phase = phRollout
			a.trk = s.apts[a.apt].rwyHdg
			a.desTrk = a.trk
		} else if a.alt < a.gndElev+50 && a.phase != phFlare {
			a.alt = a.gndElev + 50
		}
	}
	a.slowT -= stepDt
	if a.slowT <= 0 {
		a.slowT += a.slowDt
		s.slowUpdate(a, t)
		if a.dead {
			return
		}
	}
	s.dynamics(a)
}

// populate keeps the number of aircraft following the diurnal target.
func (s *Sim) populate(t float64) {
	target := int(float64(s.cfg.Aircraft)*s.env.density + 0.5)
	if target < 1 {
		target = 1
	}
	if n := len(s.ac); n < target {
		p := float64(target-n)/45 + 0.01 // expected spawns this second
		for ; p > 0; p-- {
			if s.rng.Float64() < p {
				s.spawn(s.randomKind(), false, t)
			}
		}
	}
}

// reap removes dead aircraft.
func (s *Sim) reap() {
	for i := 0; i < len(s.ac); {
		if s.ac[i].dead {
			delete(s.used, s.ac[i].icao)
			last := len(s.ac) - 1
			s.ac[i] = s.ac[last]
			s.ac = s.ac[:last]
			continue
		}
		i++
	}
}

// airData is the air-referenced state derived from the kinematic state.
type airData struct {
	hdgTrue, hdgMag  float64
	ias, mach, oatC  float64
	windDir, windKt  float64
	pressHPa, gnssFt float64
}

func (s *Sim) airData(a *kin) airData {
	var d airData
	d.pressHPa = isaPressHPa(a.alt)
	tK := isaTempK(a.alt) + s.env.dT
	d.oatC = tK - 273.15
	d.gnssFt = a.alt + s.gnssMinusBaro(a.alt)
	if a.onGround || a.tas < 45 {
		d.hdgTrue = a.trk
		d.ias = a.tas
		d.mach = a.tas / soundKt(tK)
	} else {
		sn, cs := math.Sincos(a.trk * deg2rad)
		d.hdgTrue = wrap360(math.Atan2(a.gs*sn-a.windE, a.gs*cs-a.windN) * rad2deg)
		d.mach = a.tas / soundKt(tK)
		d.ias = casFromMach(d.mach, d.pressHPa)
	}
	d.hdgMag = wrap360(d.hdgTrue - s.cfg.MagVarDeg)
	d.windKt = math.Hypot(a.windN, a.windE)
	if d.windKt > 0 {
		d.windDir = wrap360(math.Atan2(-a.windE, -a.windN) * rad2deg)
	}
	return d
}
