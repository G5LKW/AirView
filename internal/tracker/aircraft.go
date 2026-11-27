package tracker

import (
	"math"
	"sort"

	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/refdata"
	"airview/internal/store"
)

// cprFrame is the last received CPR position of one parity.
type cprFrame struct {
	lat, lon uint32
	t        int64
	valid    bool
	surface  bool
	mlat     bool
}

// pendingFix is a decoded position waiting for a second, consistent fix.
type pendingFix struct {
	lat, lon float64
	t        int64
	valid    bool
}

// aircraft is the full live state of one tracked address. Times are data-time
// milliseconds; 0 = never.
type aircraft struct {
	key      uint32 // address | modes.NonICAOFlag
	hex      string
	addrType modes.AddrType

	firstSeen   int64
	lastSeen    int64
	confirmedMs int64 // last frame with a self-checking CRC
	nConfirmed  uint8 // saturating count of such frames
	fromUpdate  bool  // fed by SBS / JSON updates
	lastESMs    int64 // last DF17 extended squitter
	lastTISBMs  int64 // last TIS-B / ADS-R message

	msgs     uint64
	prevMsgs uint64
	rate     float64
	ew       ewRate

	// Identity.
	csRaw      [8]byte
	callsign   string
	csMs       int64
	squawk     uint16
	sqPending  uint16
	sqMs       int64
	category   uint8
	categoryOK bool

	// Vertical.
	altBaro   float64
	altBaroMs int64
	altGNSS   float64 // TC20-22 height (low confidence)
	altGNSSMs int64
	gnssDelta float64 // GNSS minus baro (TC19)
	gnssDMs   int64
	vertRate  float64
	vrMs      int64
	selAlt    float64
	selAltMs  int64
	qnh       float64
	qnhMs     int64

	// Horizontal.
	gs, track   float64
	velMs       int64
	velADSBMs   int64 // last velocity from an extended squitter
	ias         float64
	iasMs       int64
	tas         float64
	tasMs       int64
	mach        float64
	machMs      int64
	magHdg      float64
	magHdgMs    int64
	roll        float64
	rollMs      int64
	trackRate   float64
	trackRateMs int64
	selHdg      float64
	selHdgMs    int64
	navModes    modes.NavModes
	navMs       int64

	// Meteorology.
	mrarWindSpd, mrarWindDir float64
	mrarWindMs               int64
	mrarTemp                 float64
	mrarTempMs               int64
	windDir, windSpd         float64
	windMs                   int64
	oat                      float64
	oatMs                    int64
	hookMs                   int64

	// Integrity / status.
	version    int8 // -1 unknown
	nicA, nicC bool
	nic        int8
	rcM        float64
	nicMs      int64
	nacp       uint8
	nacpMs     int64
	nacv       uint8
	nacvMs     int64
	sil        uint8
	silMs      int64
	emergency  uint8
	emMs       int64
	alert, spi bool
	alertMs    int64
	spiMs      int64
	onGround   bool
	groundMs   int64

	// Position.
	lat, lon float64
	posMs    int64
	posValid bool
	posSrc   uint8  // store.Src*
	posLabel string // model.Src*
	nPos     uint64
	// MLAT quality of the position at mlatPosMs (network MLAT results,
	// DecodedUpdate.Grade / SigmaM); shown only while that is the current
	// position.
	mlatPosMs int64
	mlatSigma float64 // m, NaN = unknown
	mlatGrade uint8
	cpr       [2]cprFrame
	pend      pendingFix

	// Geometry relative to the receiver (valid when geoValid).
	geoValid               bool
	dist, brg, elev, slant float64
	cell                   uint32

	// Reception.
	rssi, rssiAvg float64
	rssiMs        int64
	antSeen       [maxSlots]int64
	feedSeen      []int64 // by feedState.idx
	minMask       uint32  // slots that heard it in the current minute
	minPosMask    uint32  // slots with a position in the current minute
	hourMask      uint32  // slots with a position in the current hour

	// Reference data.
	refDone                 bool // acdb looked up (or not applicable)
	acdbOK                  bool
	acdbRec                 refdata.ACRecord
	reg, typeCode, typeDesc string
	operator                string
	country, countryCode    string
	military, interesting   bool
	announcedEmergency      string
	announcedCatch          bool
	recAlt, recGS           float64 // previous readings for record plausibility
	recAltMs, recGSMs       int64
	trail                   trail
	trailTrack, trailAlt    float64
	lastStoredSec           int64
	flight                  store.Flight
	flightOpen, flightDirty bool
	flightSavedSec          int64
	flightRecSec            int64 // last sample of the longest-flight record (flightRecord)
	flRSSISum               float64
	flRSSIN                 int64
	savedMsgs               uint64
	savedPos                uint64
	newFlights              int64 // flights opened since the last aircraft upsert
	seenDirty               bool
	seenSavedSec            int64
	maxDistKm, maxAltFt     float64 // since the last aircraft upsert
	srcMask                 int
	msgsBase                uint64 // msgs when the previous flight closed
	tjAltMs                 int64  // last altitude handed to the tracking filter
	guessMs                 int64  // last Locate Me sample (guess.go)

	// heard is the aggregator-mode set of stations that heard the aircraft
	// in the last 60 s (lock-free: Lanes.Credit writes it without mu).
	heard heardBy
}

// shown reports whether the aircraft is established enough to be listed and
// to own a flight: two CRC-confirmed frames, or data from a decoded feed.
func (ac *aircraft) shown() bool { return ac.nConfirmed >= 2 || ac.fromUpdate }

// bestAlt returns the altitude used for geometry and trails: barometric when
// fresh, else geometric, else NaN.
func (ac *aircraft) bestAlt(now int64) float64 {
	if fresh(ac.altBaroMs, now, fieldTTLMs) {
		return ac.altBaro
	}
	if g, ok := ac.geomAlt(now); ok {
		return g
	}
	return math.NaN()
}

// geomAlt returns the geometric altitude: barometric + the TC19 GNSS delta
// when both are fresh (the trustworthy source), otherwise the TC20-22 height.
func (ac *aircraft) geomAlt(now int64) (float64, bool) {
	if fresh(ac.gnssDMs, now, fieldTTLMs) && fresh(ac.altBaroMs, now, fieldTTLMs) {
		return ac.altBaro + ac.gnssDelta, true
	}
	if fresh(ac.altGNSSMs, now, fieldTTLMs) {
		return ac.altGNSS, true
	}
	return 0, false
}

// emergencyName returns the model.Aircraft.Emergency value.
func (ac *aircraft) emergencyName(now int64) string {
	if ac.emergency != 0 && fresh(ac.emMs, now, fieldTTLMs) {
		if s := modes.EmergencyName(ac.emergency); s != "" {
			return s
		}
	}
	if fresh(ac.sqMs, now, identTTLMs) {
		return modes.SquawkEmergency(ac.squawk)
	}
	return ""
}

// source returns the model.Src* label of the aircraft at time now.
func (ac *aircraft) source(now int64) string { return ac.sourceWithin(now, posTTLMs, heardTTLMs) }

// sourceWithin is source with explicit freshness windows for the position
// and for the last extended squitter.
func (ac *aircraft) sourceWithin(now, posTTL, esTTL int64) string {
	if ac.posValid && now-ac.posMs <= posTTL {
		return ac.posLabel
	}
	switch {
	case fresh(ac.lastESMs, now, esTTL):
		return model.SrcADSB
	case fresh(ac.lastTISBMs, now, esTTL):
		return model.SrcTISB
	case ac.fromUpdate && ac.nConfirmed == 0:
		return model.SrcOther
	}
	return model.SrcModeS
}

// dtoAlloc hands out pointers into shared backing arrays so that building a
// snapshot costs a handful of allocations instead of dozens per aircraft.
type dtoAlloc struct {
	f []float64
	i []int
	b []bool
}

func (a *dtoAlloc) fp(v float64) *float64 {
	if !finite(v) {
		return nil
	}
	if len(a.f) == cap(a.f) {
		a.f = make([]float64, 0, 1024)
	}
	a.f = append(a.f, v)
	return &a.f[len(a.f)-1]
}

func (a *dtoAlloc) ip(v int) *int {
	if len(a.i) == cap(a.i) {
		a.i = make([]int, 0, 256)
	}
	a.i = append(a.i, v)
	return &a.i[len(a.i)-1]
}

func (a *dtoAlloc) bp(v bool) *bool {
	if len(a.b) == cap(a.b) {
		a.b = make([]bool, 0, 128)
	}
	a.b = append(a.b, v)
	return &a.b[len(a.b)-1]
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// dto fills the API form of ac. Caller holds mu.
func (t *Tracker) dto(ac *aircraft, now int64, al *dtoAlloc) model.Aircraft {
	a := model.Aircraft{
		ICAO:         ac.hex,
		AddrType:     ac.addrType.Label(),
		Callsign:     ac.callsign,
		Registration: ac.reg,
		TypeCode:     ac.typeCode,
		TypeDesc:     ac.typeDesc,
		Operator:     ac.operator,
		Country:      ac.country,
		CountryCode:  ac.countryCode,
		Military:     ac.military,
		Source:       ac.source(now),
		Msgs:         ac.msgs,
		MsgRate:      round1(ac.rate),
		FirstSeen:    ac.firstSeen,
		LastSeen:     ac.lastSeen,
	}
	if ac.flightOpen {
		a.FlightID = ac.flight.ID
	}
	if ac.fromUpdate && ac.nConfirmed == 0 && ac.key&modes.NonICAOFlag == 0 {
		a.AddrType = "icao"
	}
	if fresh(ac.sqMs, now, identTTLMs) {
		a.Squawk = modes.FormatSquawk(ac.squawk)
	}
	if ac.categoryOK {
		a.Category = modes.CategoryName(ac.category)
	}

	if t.predictedPos(ac, now, &a, al) {
		// Coasting: the predicted position (never stored anywhere).
	} else if ac.posValid && now-ac.posMs <= posTTLMs {
		a.Lat, a.Lon = al.fp(ac.lat), al.fp(ac.lon)
		a.PosAge = al.fp(round1(float64(now-ac.posMs) / 1000))
		if ac.posSrc == store.SrcMLAT && ac.mlatPosMs != 0 && ac.mlatPosMs == ac.posMs {
			if model.Displayable(ac.mlatGrade) {
				a.MLATGrade = model.GradeString(ac.mlatGrade)
			}
			a.MLATSigmaM = al.fp(ac.mlatSigma)
		}
		if ac.geoValid {
			a.DistKm = al.fp(ac.dist)
			a.Bearing = al.fp(ac.brg)
			a.Elevation = al.fp(ac.elev)
			a.SlantKm = al.fp(ac.slant)
		}
	}
	if fresh(ac.groundMs, now, fieldTTLMs) {
		a.OnGround = al.bp(ac.onGround)
	}
	if fresh(ac.altBaroMs, now, fieldTTLMs) {
		a.AltBaro = al.fp(ac.altBaro)
	}
	if g, ok := ac.geomAlt(now); ok {
		a.AltGeom = al.fp(g)
	}
	if fresh(ac.vrMs, now, fieldTTLMs) {
		a.VertRate = al.fp(ac.vertRate)
	}
	if fresh(ac.selAltMs, now, ehsTTLMs) {
		a.SelAlt = al.fp(ac.selAlt)
	}
	if fresh(ac.qnhMs, now, ehsTTLMs) {
		a.QNH = al.fp(ac.qnh)
	}
	if fresh(ac.velMs, now, fieldTTLMs) {
		a.GS, a.Track = al.fp(ac.gs), al.fp(ac.track)
	}
	if fresh(ac.iasMs, now, ehsTTLMs) {
		a.IAS = al.fp(ac.ias)
	}
	if fresh(ac.tasMs, now, ehsTTLMs) {
		a.TAS = al.fp(ac.tas)
	}
	if fresh(ac.machMs, now, ehsTTLMs) {
		a.Mach = al.fp(ac.mach)
	}
	if fresh(ac.magHdgMs, now, ehsTTLMs) {
		a.MagHeading = al.fp(ac.magHdg)
	}
	if fresh(ac.rollMs, now, ehsTTLMs) {
		a.Roll = al.fp(ac.roll)
	}
	if fresh(ac.trackRateMs, now, ehsTTLMs) {
		a.TrackRate = al.fp(ac.trackRate)
	}
	if fresh(ac.selHdgMs, now, ehsTTLMs) {
		a.SelHeading = al.fp(ac.selHdg)
	}
	if fresh(ac.windMs, now, fieldTTLMs) {
		a.WindDir, a.WindSpd = al.fp(ac.windDir), al.fp(ac.windSpd)
	}
	if fresh(ac.oatMs, now, fieldTTLMs) {
		a.OAT = al.fp(ac.oat)
	}

	if ac.version >= 0 {
		a.ADSBVersion = al.ip(int(ac.version))
	}
	if fresh(ac.nicMs, now, fieldTTLMs) {
		a.NIC = al.ip(int(ac.nic))
		if ac.rcM > 0 {
			a.RcM = al.fp(ac.rcM)
		}
	}
	if fresh(ac.nacpMs, now, fieldTTLMs) {
		a.NACp = al.ip(int(ac.nacp))
	}
	if fresh(ac.nacvMs, now, fieldTTLMs) {
		a.NACv = al.ip(int(ac.nacv))
	}
	if fresh(ac.silMs, now, fieldTTLMs) {
		a.SIL = al.ip(int(ac.sil))
	}
	a.Emergency = ac.emergencyName(now)
	a.Alert = ac.alert && fresh(ac.alertMs, now, fieldTTLMs)
	a.SPI = ac.spi && fresh(ac.spiMs, now, fieldTTLMs)
	if ac.navModes != 0 && fresh(ac.navMs, now, ehsTTLMs) {
		a.NavModes = ac.navModes.AppendNames(nil)
	}

	if fresh(ac.rssiMs, now, fieldTTLMs) {
		a.RSSI = al.fp(round1(ac.rssi))
		a.RSSIAvg = al.fp(round1(ac.rssiAvg))
	}
	for i := 1; i < len(t.slots) && i < maxSlots; i++ {
		if fresh(ac.antSeen[i], now, heardTTLMs) {
			a.Antennas = append(a.Antennas, t.slots[i].id)
		}
	}
	for i, ft := range ac.feedSeen {
		if fresh(ft, now, heardTTLMs) && i < len(t.feedList) {
			a.Feeds = append(a.Feeds, t.feedList[i].id)
		}
	}
	return a
}

// Snapshot returns the live picture. It is rebuilt at most once per second
// of data time and shared between callers: treat it as read-only.
func (t *Tracker) Snapshot() model.LiveSnapshot {
	s, _ := t.snapshotSide()
	return s
}

// snapshotSide returns the shared snapshot and, in aggregator mode, its
// per-aircraft side data (parallel to Aircraft; read-only).
func (t *Tracker) snapshotSide() (model.LiveSnapshot, []netSide) {
	if s := t.sh.Load(); s != nil {
		return s.snapshotSide(t)
	}
	t.mu.Lock()
	if t.snap != nil && t.snapTick == t.tickN {
		s, side := *t.snap, t.snapSide
		t.mu.Unlock()
		return s, side
	}
	t.mu.Unlock()

	// One build at a time; the enrichment hook runs without mu so that the
	// frame path is never blocked by it.
	t.snapMu.Lock()
	defer t.snapMu.Unlock()
	t.mu.Lock()
	if t.snap != nil && t.snapTick == t.tickN {
		s, side := *t.snap, t.snapSide
		t.mu.Unlock()
		return s, side
	}
	s, side := t.buildSnapshot()
	tick := t.tickN
	enrich := t.enrich
	t.mu.Unlock()
	if enrich != nil {
		enrich(s)
	}
	t.mu.Lock()
	t.snap, t.snapSide, t.snapTick = s, side, tick
	t.mu.Unlock()
	return *s, side
}

// buildSnapshot builds a new live picture (and, in aggregator mode, its
// side data). Caller holds mu.
func (t *Tracker) buildSnapshot() (*model.LiveSnapshot, []netSide) {
	now := t.nowMs
	s := &model.LiveSnapshot{Now: now, Aircraft: make([]model.Aircraft, 0, t.live+8)}
	al := &dtoAlloc{
		f: make([]float64, 0, 24*(t.live+8)),
		i: make([]int, 0, 5*(t.live+8)),
		b: make([]bool, 0, t.live+8),
	}
	var side []netSide
	if t.net != nil {
		side = make([]netSide, 0, t.live+8)
	}
	for _, ac := range t.ac {
		if !ac.shown() || (now-ac.lastSeen > t.ttlMs && !t.coasting(ac, now)) {
			continue
		}
		a := t.dto(ac, now, al)
		s.Aircraft = append(s.Aircraft, a)
		if t.net != nil {
			side = append(side, netSide{})
			sd := &side[len(side)-1]
			sd.n = uint8(len(ac.heard.list(now/1000, sd.st[:0])))
		}
		s.Counts.Aircraft++
		if a.Predicted {
			s.Counts.Predicted++
		} else if a.Lat != nil {
			s.Counts.WithPos++
			if a.DistKm != nil && *a.DistKm > s.Counts.MaxRangeKm {
				s.Counts.MaxRangeKm = *a.DistKm
			}
		}
		switch a.Source {
		case model.SrcADSB:
			s.Counts.ADSB++
		case model.SrcMLAT:
			s.Counts.MLAT++
		case model.SrcTISB, model.SrcADSR:
			s.Counts.TISB++
		case model.SrcModeS:
			s.Counts.ModeS++
		}
	}
	if side != nil {
		sort.Sort(netSorter{a: s.Aircraft, side: side})
	} else {
		sort.Slice(s.Aircraft, func(i, j int) bool { return s.Aircraft[i].ICAO < s.Aircraft[j].ICAO })
	}
	s.Counts.MsgRate = round1(t.msgRate)
	s.Counts.PosRate = round1(t.posRate)
	s.Counts.MaxRangeKm = round1(s.Counts.MaxRangeKm)
	return s, side
}

// Aircraft returns the current state of one aircraft. icao is the address
// key (24-bit address, plus modes.NonICAOFlag for non-ICAO addresses, as
// returned by modes.ParseICAO). Aircraft that left the live list are still
// reported for up to 30 minutes, with their stale fields omitted.
func (t *Tracker) Aircraft(icao uint32) (model.Aircraft, bool) {
	if s := t.sh.Load(); s != nil {
		return s.parts[ShardOf(icao, s.n)].Aircraft(icao)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ac := t.ac[icao]
	if ac == nil || !ac.shown() {
		return model.Aircraft{}, false
	}
	return t.dto(ac, t.nowMs, &dtoAlloc{}), true
}

// Trails returns the in-memory trails of all live aircraft, limited to the
// last `minutes` minutes (<= 0 = everything kept), keyed by the aircraft's
// hex address as in model.Aircraft.ICAO.
func (t *Tracker) Trails(minutes int) map[string][]model.TrailPoint {
	if s := t.sh.Load(); s != nil {
		out := make(map[string][]model.TrailPoint)
		for _, p := range s.parts {
			for k, v := range p.Trails(minutes) {
				out[k] = v
			}
		}
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.nowMs
	from := int64(math.MinInt64)
	if minutes > 0 {
		from = now - int64(minutes)*60_000
	}
	out := make(map[string][]model.TrailPoint, t.live)
	for _, ac := range t.ac {
		if now-ac.lastSeen > t.ttlMs || !ac.shown() || ac.trail.n == 0 {
			continue
		}
		if pts := ac.trail.since(from); len(pts) > 0 {
			out[ac.hex] = pts
		}
	}
	return out
}

// Trail returns the in-memory trail of one aircraft (oldest first), or nil.
func (t *Tracker) Trail(icao uint32) []model.TrailPoint {
	if s := t.sh.Load(); s != nil {
		return s.parts[ShardOf(icao, s.n)].Trail(icao)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ac := t.ac[icao]
	if ac == nil {
		return nil
	}
	return ac.trail.since(math.MinInt64)
}

// trail is a ring of trail points, oldest first, growing up to
// maxTrailPoints.
type trail struct {
	buf   []model.TrailPoint
	start int
	n     int
}

func (tr *trail) last() *model.TrailPoint {
	if tr.n == 0 {
		return nil
	}
	return &tr.buf[(tr.start+tr.n-1)%len(tr.buf)]
}

func (tr *trail) push(p model.TrailPoint) {
	if tr.n == len(tr.buf) {
		if len(tr.buf) >= maxTrailPoints {
			// Full: overwrite the oldest.
			tr.buf[tr.start] = p
			tr.start = (tr.start + 1) % len(tr.buf)
			return
		}
		nc := 2 * len(tr.buf)
		if nc < 32 {
			nc = 32
		}
		if nc > maxTrailPoints {
			nc = maxTrailPoints
		}
		nb := make([]model.TrailPoint, nc)
		for i := 0; i < tr.n; i++ {
			nb[i] = tr.buf[(tr.start+i)%len(tr.buf)]
		}
		tr.buf, tr.start = nb, 0
	}
	tr.buf[(tr.start+tr.n)%len(tr.buf)] = p
	tr.n++
}

// dropBefore removes points older than t.
func (tr *trail) dropBefore(t int64) {
	for tr.n > 0 && tr.buf[tr.start].T < t {
		tr.start = (tr.start + 1) % len(tr.buf)
		tr.n--
	}
}

// since returns a copy of the points with T >= from.
func (tr *trail) since(from int64) []model.TrailPoint {
	if tr.n == 0 {
		return nil
	}
	i := 0
	for i < tr.n && tr.buf[(tr.start+i)%len(tr.buf)].T < from {
		i++
	}
	if i == tr.n {
		return nil
	}
	out := make([]model.TrailPoint, 0, tr.n-i)
	for ; i < tr.n; i++ {
		out = append(out, tr.buf[(tr.start+i)%len(tr.buf)])
	}
	return out
}
