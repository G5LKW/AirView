package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/refdata"
	"airview/internal/store"
)

// ---------------------------------------------------------------- flights

func (t *Tracker) nextFlightID() int64 {
	if t.st != nil {
		return t.st.NextFlightID()
	}
	if t.flightSeq != nil { // a shard: ids unique across the shards
		return t.flightSeq.Add(1)
	}
	t.localFlightID++
	return t.localFlightID
}

// openFlight starts a contact session for ac. It is called before the
// current message is counted.
func (t *Tracker) openFlight(ac *aircraft, tMs int64) {
	start := tMs / 1000
	if ac.msgsBase == 0 && ac.firstSeen > 0 {
		start = ac.firstSeen / 1000 // first session: include the frames that established it
	}
	fl := store.NewFlight(t.nextFlightID(), ac.key, start)
	fl.End = tMs / 1000
	fl.Msgs = int64(ac.msgs - ac.msgsBase)
	fl.Callsign = ac.callsign
	if fresh(ac.sqMs, tMs, identTTLMs) {
		fl.Squawk = modes.FormatSquawk(ac.squawk)
	}
	if fresh(ac.altBaroMs, tMs, fieldTTLMs) {
		fl.MinAlt, fl.MaxAlt = ac.altBaro, ac.altBaro
	}
	if fresh(ac.velMs, tMs, fieldTTLMs) {
		fl.MaxGS = ac.gs
	}
	ac.flight = fl
	ac.flightOpen, ac.flightDirty = true, true
	ac.flightSavedSec, ac.flightRecSec = 0, 0
	ac.flRSSISum, ac.flRSSIN = 0, 0
	ac.lastStoredSec = 0
	ac.announcedEmergency, ac.announcedCatch = "", false
	ac.newFlights++
	ac.seenDirty = true
	t.c.Flights++
	t.checkCatch(ac, tMs)
}

// saveFlight queues the flight row of ac for the next flush.
func (t *Tracker) saveFlight(ac *aircraft, sec int64) {
	fl := &ac.flight
	if ac.flRSSIN > 0 {
		fl.RSSIAvg = ac.flRSSISum / float64(ac.flRSSIN)
	}
	if fl.Positions == 0 {
		fl.SrcMask |= store.MaskModeS
	} else {
		fl.SrcMask &^= store.MaskModeS
	}
	ac.flightDirty = false
	ac.flightSavedSec = sec
	if t.st != nil {
		t.pendFlights = append(t.pendFlights, *fl)
	}
}

// flightRecord samples the duration of the open flight of ac for the
// "longest tracked flight" record. It runs on the 1.0.0 schedule (the
// flight's own 60-s phase, on Flush and when the flight closes), not on the
// row refresh tick, so the record values and their alerts are the ones
// 1.0.0 produced.
func (t *Tracker) flightRecord(ac *aircraft, sec int64) {
	ac.flightRecSec = sec
	fl := &ac.flight
	if dur := float64(fl.End - fl.Start); dur > t.recValue(recMaxFlight) && dur >= 60 {
		t.record(recMaxFlight, dur, ac.key, fl.End, recordMax, ac)
	}
}

// closeFlight ends the open flight of ac.
func (t *Tracker) closeFlight(ac *aircraft) {
	if !ac.flightOpen {
		return
	}
	t.flightRecord(ac, ac.flight.End)
	t.saveFlight(ac, ac.flight.End)
	if t.net != nil && t.net.lanes != nil {
		t.net.lanes.closeFlight(ac.key, ac.flight.ID) // flight_station rows
	}
	ac.flightOpen = false
	ac.msgsBase = ac.msgs
	if ac.msgsBase == 0 {
		ac.msgsBase = 1
	}
}

// saveSeen queues the aircraft-table sighting (deltas since the last one).
// The first row of an airframe instance goes out at the next row check;
// later ones wait for the next aircraft refresh (heldSeen).
func (t *Tracker) saveSeen(ac *aircraft, sec int64) {
	first := ac.seenSavedSec == 0
	ac.seenDirty = false
	ac.seenSavedSec = sec
	seen := store.AircraftSeen{
		ICAO: ac.key, FirstSeen: ac.firstSeen / 1000, LastSeen: ac.lastSeen / 1000,
		Callsign: ac.callsign, Reg: ac.reg, Type: ac.typeCode, Operator: ac.operator,
		Country:   ac.country,
		Msgs:      int64(ac.msgs - ac.savedMsgs),
		Flights:   ac.newFlights,
		Positions: int64(ac.nPos - ac.savedPos),
		MaxDistKm: ac.maxDistKm, MaxAlt: ac.maxAltFt,
		SrcMask: ac.srcMask,
	}
	if ac.categoryOK {
		seen.Category = modes.CategoryName(ac.category)
	}
	if ac.nPos == 0 {
		seen.SrcMask |= store.MaskModeS
	}
	if ac.military {
		seen.Flags |= store.FlagMilitary
	}
	if ac.interesting {
		seen.Flags |= store.FlagInteresting
	}
	ac.savedMsgs, ac.savedPos, ac.newFlights = ac.msgs, ac.nPos, 0
	ac.maxDistKm, ac.maxAltFt = math.NaN(), math.NaN()
	if t.st != nil {
		if first {
			t.pendSeen = append(t.pendSeen, seen)
		} else {
			t.heldSeen = append(t.heldSeen, seen)
		}
	}
}

// remove drops an aircraft from the table, closing its flight.
func (t *Tracker) remove(key uint32, ac *aircraft) {
	t.classify(ac, t.nowMs)
	t.closeFlight(ac)
	if ac.seenDirty && ac.shown() {
		t.saveSeen(ac, ac.lastSeen/1000)
	}
	delete(t.ac, key)
	if t.tj != nil {
		t.tj.Drop(key)
	}
}

// ------------------------------------------------------- records / events

type recKey int

const (
	recMaxDist recKey = iota
	recMaxAlt
	recMaxGS
	recMinRSSIPos
	recMaxAircraft
	recMaxMsgRate
	recMaxFlight
	numRecs
)

// Record keys written to the records table.
const (
	RecordMaxDistKm   = "max_dist_km"       // farthest ADS-B position
	RecordMaxAltFt    = "max_alt_ft"        // highest barometric altitude
	RecordMaxGSKt     = "max_gs_kt"         // fastest ground speed
	RecordMinRSSIPos  = "min_rssi_pos_dbfs" // weakest frame that still gave a position
	RecordMaxAircraft = "max_aircraft"      // most aircraft tracked at once
	RecordMaxMsgRate  = "max_msg_rate"      // peak smoothed frame rate, frames/s
	RecordMaxFlightS  = "max_flight_s"      // longest tracked flight, seconds
)

var recNames = [numRecs]string{
	RecordMaxDistKm, RecordMaxAltFt, RecordMaxGSKt, RecordMinRSSIPos,
	RecordMaxAircraft, RecordMaxMsgRate, RecordMaxFlightS,
}

var recLabels = [numRecs]string{
	"farthest position", "highest aircraft", "fastest aircraft", "weakest decoded position",
	"most aircraft at once", "peak message rate", "longest tracked flight",
}

var recUnits = [numRecs]string{"km", "ft", "kt", "dBFS", "", "msg/s", "s"}

const (
	recordMax = store.RecordMax
	recordMin = store.RecordMin

	recordEventGapS = 30 * 60 // at most one "new record" event per key in this time
)

// recDetail is the context captured when a record is set.
type recDetail struct {
	Callsign string   `json:"callsign,omitempty"`
	Reg      string   `json:"reg,omitempty"`
	Type     string   `json:"type,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	AltFt    *float64 `json:"altFt,omitempty"`
	DistKm   *float64 `json:"distKm,omitempty"`
	Bearing  *float64 `json:"bearing,omitempty"`
	GS       *float64 `json:"gsKt,omitempty"`
	RSSI     *float64 `json:"rssi,omitempty"`
	Previous *float64 `json:"previous,omitempty"`
	Key      string   `json:"key,omitempty"`
	Value    *float64 `json:"value,omitempty"`
}

type record struct {
	value    float64
	have     bool
	icao     uint32
	t        int64
	mode     store.RecordMode
	dirty    bool
	announce bool    // an event is due
	prev     float64 // value before the current streak of improvements
	hadPrev  bool
	lastEv   int64 // data second of the last event
	// Context of the holder, captured without allocating.
	callsign, reg, typ       string
	lat, lon, alt, dist, brg float64
	gs, rssi                 float64
	hasAC                    bool
}

// recTable holds the all-time records. A standalone or unsharded tracker
// owns its table; the shards of an ICAO-sharded tracker share one (shared
// is then true and mu guards r). The current values are mirrored in v
// (math.Float64bits, NaN = no record) so that the frequent "does this beat
// the record" checks read them without the lock.
type recTable struct {
	mu     sync.Mutex
	shared bool
	r      [numRecs]record
	v      [numRecs]atomic.Uint64
}

var recNone = math.Float64bits(math.NaN())

func newRecTable() *recTable {
	rt := &recTable{}
	for k := range rt.v {
		rt.v[k].Store(recNone)
	}
	return rt
}

func (rt *recTable) lock() {
	if rt.shared {
		rt.mu.Lock()
	}
}

func (rt *recTable) unlock() {
	if rt.shared {
		rt.mu.Unlock()
	}
}

// reset forgets every record.
func (rt *recTable) reset() {
	rt.lock()
	rt.r = [numRecs]record{}
	for k := range rt.v {
		rt.v[k].Store(recNone)
	}
	rt.unlock()
}

// recValue returns the current value of a max-record (0 when none).
func (t *Tracker) recValue(k recKey) float64 { return t.recValueOr(k, 0) }

func (t *Tracker) recValueOr(k recKey, def float64) float64 {
	if v := math.Float64frombits(t.recs.v[k].Load()); v == v {
		return v
	}
	return def
}

// record sets a new all-time record if value beats the current one.
func (t *Tracker) record(k recKey, value float64, icao uint32, sec int64, mode store.RecordMode, ac *aircraft) {
	if !finite(value) {
		return
	}
	rt := t.recs
	rt.lock()
	defer rt.unlock()
	r := &rt.r[k]
	if r.have {
		if mode == recordMax && !(value > r.value) || mode == recordMin && !(value < r.value) {
			return
		}
		if !r.announce {
			r.prev, r.hadPrev = r.value, true
		}
		r.announce = true
	}
	r.value, r.have, r.icao, r.t, r.mode, r.dirty = value, true, icao, sec, mode, true
	rt.v[k].Store(math.Float64bits(value))
	r.hasAC = ac != nil
	if ac != nil {
		now := sec * 1000
		r.callsign, r.reg, r.typ = ac.callsign, ac.reg, ac.typeCode
		nan := math.NaN()
		r.lat, r.lon, r.dist, r.brg, r.alt, r.gs = nan, nan, nan, nan, ac.bestAlt(now), nan
		if ac.posValid && now-ac.posMs <= posTTLMs {
			r.lat, r.lon = ac.lat, ac.lon
			if ac.geoValid {
				r.dist, r.brg = ac.dist, ac.brg
			}
		}
		if fresh(ac.velMs, now, fieldTTLMs) {
			r.gs = ac.gs
		}
		r.rssi = ac.rssi
	}
}

func fptr(v float64, digits int) *float64 {
	if !finite(v) {
		return nil
	}
	p := math.Pow(10, float64(digits))
	v = math.Round(v*p) / p
	return &v
}

func (r *record) detail(k recKey, withValue bool) string {
	d := recDetail{}
	if r.hasAC {
		d = recDetail{
			Callsign: r.callsign, Reg: r.reg, Type: r.typ,
			Lat: fptr(r.lat, 5), Lon: fptr(r.lon, 5), AltFt: fptr(r.alt, 0),
			DistKm: fptr(r.dist, 2), Bearing: fptr(r.brg, 1), GS: fptr(r.gs, 0), RSSI: fptr(r.rssi, 1),
		}
	}
	if withValue {
		d.Key, d.Value = recNames[k], fptr(r.value, 2)
		if r.hadPrev {
			d.Previous = fptr(r.prev, 2)
		}
	}
	b, err := json.Marshal(d)
	if err != nil || string(b) == "{}" {
		return ""
	}
	return string(b)
}

// flushRecords writes dirty records to b and raises "new record" events,
// rate-limited per key so that an outbound aircraft extending the range
// record frame by frame gives one event, not thousands.
func (t *Tracker) flushRecords(b *store.Batch, sec int64) {
	rt := t.recs
	rt.lock()
	defer rt.unlock()
	for k := recKey(0); k < numRecs; k++ {
		r := &rt.r[k]
		if r.dirty {
			r.dirty = false
			if b != nil {
				b.SetRecord(store.Record{Key: recNames[k], Value: r.value, ICAO: r.icao, T: r.t, Detail: r.detail(k, false)}, r.mode)
			}
		}
		if r.announce && r.hadPrev && (r.lastEv == 0 || sec-r.lastEv >= recordEventGapS || sec < r.lastEv) {
			r.announce = false
			r.lastEv = sec
			if b != nil {
				t.pendEvents = append(t.pendEvents, store.Event{T: r.t, Kind: "record", ICAO: r.icao, Detail: r.detail(k, true)})
			}
			text := fmt.Sprintf("New record: %s %s %s", recLabels[k], formatValue(r.value), recUnits[k])
			hex := ""
			if r.icao != 0 {
				hex = modes.FormatICAO(r.icao)
				who := hex
				if r.callsign != "" {
					who = r.callsign + " (" + hex + ")"
				}
				text += " - " + who
			}
			t.notify(Notice{Level: "info", Text: text, T: r.t * 1000, Kind: "record", ICAO: hex, Key: recNames[k], Value: fptr(r.value, 2)})
		}
	}
}

func formatValue(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// loadRecords reads the stored all-time records so that only real
// improvements are written and announced.
func (t *Tracker) loadRecords() {
	if t.st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := t.st.Records(ctx)
	if err != nil {
		t.log.Warn("tracker: reading records failed", "err", err)
		return
	}
	for _, row := range rows {
		for k := recKey(0); k < numRecs; k++ {
			if recNames[k] != row.Key || !finite(row.Value) {
				continue
			}
			mode := recordMax
			if k == recMinRSSIPos {
				mode = recordMin
			}
			t.recs.lock()
			t.recs.r[k] = record{value: row.Value, have: true, icao: row.ICAO, t: row.T, mode: mode}
			t.recs.v[k].Store(math.Float64bits(row.Value))
			t.recs.unlock()
		}
	}
}

func (t *Tracker) notify(n Notice) {
	if len(t.notices) < 256 {
		t.notices = append(t.notices, n)
	}
}

// eventDetail is the detail_json of emergency and catch events.
type eventDetail struct {
	Callsign  string   `json:"callsign,omitempty"`
	Squawk    string   `json:"squawk,omitempty"`
	Emergency string   `json:"emergency,omitempty"`
	Reg       string   `json:"reg,omitempty"`
	Type      string   `json:"type,omitempty"`
	Operator  string   `json:"operator,omitempty"`
	Country   string   `json:"country,omitempty"`
	Lat       *float64 `json:"lat,omitempty"`
	Lon       *float64 `json:"lon,omitempty"`
	AltFt     *float64 `json:"altFt,omitempty"`
	DistKm    *float64 `json:"distKm,omitempty"`
	Bearing   *float64 `json:"bearing,omitempty"`
	FlightID  int64    `json:"flightId,omitempty"`
}

func (t *Tracker) eventDetail(ac *aircraft, now int64) eventDetail {
	d := eventDetail{
		Callsign: ac.callsign, Reg: ac.reg, Type: ac.typeCode, Operator: ac.operator, Country: ac.country,
		AltFt: fptr(ac.bestAlt(now), 0),
	}
	if fresh(ac.sqMs, now, identTTLMs) {
		d.Squawk = modes.FormatSquawk(ac.squawk)
	}
	if ac.posValid && now-ac.posMs <= posTTLMs {
		d.Lat, d.Lon = fptr(ac.lat, 5), fptr(ac.lon, 5)
		if ac.geoValid {
			d.DistKm, d.Bearing = fptr(ac.dist, 1), fptr(ac.brg, 1)
		}
	}
	if ac.flightOpen {
		d.FlightID = ac.flight.ID
	}
	return d
}

func (ac *aircraft) who() string {
	if ac.callsign != "" {
		return ac.callsign + " (" + ac.hex + ")"
	}
	return ac.hex
}

func (t *Tracker) addEvent(kind string, ac *aircraft, now int64, d eventDetail) {
	if t.st == nil {
		return
	}
	js, err := json.Marshal(d)
	if err != nil {
		js = nil
	}
	if len(t.pendEvents) < 4096 {
		t.pendEvents = append(t.pendEvents, store.Event{T: now / 1000, Kind: kind, ICAO: ac.key, Detail: string(js)})
	}
}

// checkEmergency raises an event when the aircraft declares an emergency
// (squawk 7500 / 7600 / 7700 or the TC28 emergency state), once per state
// and flight.
func (t *Tracker) checkEmergency(ac *aircraft, now int64) {
	name := ac.emergencyName(now)
	if name == "" || name == ac.announcedEmergency || !ac.flightOpen {
		return
	}
	ac.announcedEmergency = name
	ac.flight.Emergency = name
	ac.flightDirty = true
	t.c.Emergencies++
	d := t.eventDetail(ac, now)
	d.Emergency = name
	t.addEvent("emergency", ac, now, d)
	text := "Emergency: " + ac.who() + " - " + refdata.EmergencyLabel(name)
	if d.Squawk != "" {
		text += ", squawk " + d.Squawk
	}
	t.notify(Notice{Level: "alert", Text: text, T: now, Kind: "emergency", ICAO: ac.hex})
}

// checkCatch raises an event for a military or otherwise interesting
// aircraft, once per flight.
func (t *Tracker) checkCatch(ac *aircraft, now int64) {
	if ac.announcedCatch || !ac.flightOpen || !(ac.military || ac.interesting) {
		return
	}
	ac.announcedCatch = true
	kind, label := "military", "Military aircraft"
	if !ac.military {
		kind, label = "interesting", "Interesting aircraft"
	}
	t.addEvent(kind, ac, now, t.eventDetail(ac, now))
	text := label + ": " + ac.who()
	if ac.typeCode != "" {
		text += " " + ac.typeCode
	}
	if ac.country != "" {
		text += ", " + ac.country
	}
	t.notify(Notice{Level: "info", Text: text, T: now, Kind: kind, ICAO: ac.hex})
}

// --------------------------------------------------------- reference data

type acdbCached struct {
	rec refdata.ACRecord
	ok  bool
}

const (
	acdbCacheSize = 8192
	acdbQueueMax  = 8192
	afFiredMax    = 1 << 16
)

// afCheck is a pending first-ever-sighting check of one aircraft instance.
type afCheck struct {
	icao  uint32
	first int64 // the instance's first-seen time, Unix s (what its aircraft row carries)
}

// initRef fills the allocation-table reference data of a new aircraft and
// schedules its aircraft-database look-up.
func (t *Tracker) initRef(ac *aircraft) {
	if ac.key&modes.NonICAOFlag != 0 {
		ac.refDone = true
		return
	}
	if t.st != nil && len(t.afQ) < acdbQueueMax {
		if _, done := t.afFired[ac.key]; !done {
			t.afQ = append(t.afQ, afCheck{icao: ac.key, first: ac.firstSeen / 1000})
		}
	}
	if c, ok := t.acdb[ac.key]; ok {
		ac.acdbOK, ac.acdbRec, ac.refDone = c.ok, c.rec, true
	} else if t.st != nil && len(t.acdbQ) < acdbQueueMax {
		t.acdbQ = append(t.acdbQ, ac.key)
	}
	t.rebuildRef(ac)
}

// rebuildRef recomputes the reference fields from the ICAO allocation
// tables, the aircraft database record and the callsign, in that order.
func (t *Tracker) rebuildRef(ac *aircraft) {
	var a model.Aircraft
	icao := ac.key & 0xFFFFFF
	if ac.key&modes.NonICAOFlag == 0 {
		refdata.Lookup(icao).Apply(&a)
		if ac.acdbOK {
			ac.acdbRec.Apply(&a)
		}
		if ac.callsign != "" {
			refdata.LookupCallsignFor(ac.callsign, icao).Apply(&a)
		}
	} else if ac.callsign != "" {
		refdata.LookupCallsign(ac.callsign).Apply(&a)
	}
	if a.TypeDesc == "" && a.TypeCode != "" {
		a.TypeDesc = refdata.TypeName(a.TypeCode)
	}
	ac.reg, ac.typeCode, ac.typeDesc, ac.operator = a.Registration, a.TypeCode, a.TypeDesc, a.Operator
	ac.country, ac.countryCode, ac.military = a.Country, a.CountryCode, a.Military
	ac.interesting = ac.acdbOK && ac.acdbRec.Flags&refdata.ACInteresting != 0
	ac.seenDirty = true
	if ac.flightOpen {
		t.checkCatch(ac, t.nowMs)
	}
}

// resolveRefdata looks the queued aircraft up in the store's aircraft
// database - outside the ingest lock, once per aircraft, through a bounded
// cache.
func (t *Tracker) resolveRefdata(ctx context.Context) {
	if t.st == nil {
		return
	}
	t.mu.Lock()
	q, afq := t.acdbQ, t.afQ
	t.acdbQ, t.afQ = nil, nil
	onNewAF := t.onNewAF
	t.mu.Unlock()
	newAF := t.checkNewAirframes(ctx, afq, onNewAF)
	if len(q) == 0 {
		for _, icao := range newAF {
			onNewAF(icao)
		}
		return
	}
	res := make([]acdbCached, len(q))
	for i, icao := range q {
		if ctx.Err() != nil {
			q, res = q[:i], res[:i]
			break
		}
		e, ok, err := t.st.ACDBLookup(ctx, icao)
		if err != nil {
			t.log.Debug("tracker: aircraft database look-up failed", "icao", modes.FormatICAO(icao), "err", err)
			ok = false
		}
		if ok {
			res[i] = acdbCached{ok: true, rec: refdata.ACRecord{
				ICAO: icao, Reg: e.Reg, Type: e.Type, Desc: e.Desc, Operator: e.Operator,
				Flags: uint32(e.Flags), Year: e.Year,
			}}
		}
	}
	t.mu.Lock()
	for i, icao := range q {
		if _, dup := t.acdb[icao]; !dup {
			if len(t.acdbOrder) >= acdbCacheSize {
				delete(t.acdb, t.acdbOrder[0])
				t.acdbOrder = t.acdbOrder[1:]
			}
			t.acdbOrder = append(t.acdbOrder, icao)
		}
		t.acdb[icao] = res[i]
		if ac := t.ac[icao]; ac != nil {
			ac.acdbOK, ac.acdbRec, ac.refDone = res[i].ok, res[i].rec, true
			if res[i].ok {
				t.rebuildRef(ac)
			}
		}
	}
	t.snap = nil
	t.unlockAndDeliver()
	for _, icao := range newAF {
		onNewAF(icao)
	}
}

// checkNewAirframes returns the addresses among q seen for the first time
// ever, each at most once per session. The aircraft row of an instance may
// already have been written by a flush that ran between its creation and
// this check (periodic flushes run from HandleFrame, a final Flush may run
// concurrently with ingest), so "a row exists" does not mean "seen before":
// a row whose first_seen equals the instance's own first-seen time was
// created by this very instance (first_seen is merged as min, so an older
// sighting would have kept its earlier time). Without a hook the
// queue is simply dropped.
func (t *Tracker) checkNewAirframes(ctx context.Context, q []afCheck, hook func(uint32)) []uint32 {
	if hook == nil || len(q) == 0 {
		return nil
	}
	var out []uint32
	for i, c := range q {
		if ctx.Err() != nil {
			// not checked: put the rest back for the next round
			t.mu.Lock()
			t.afQ = append(t.afQ, q[i:]...)
			t.mu.Unlock()
			break
		}
		row, seen, err := t.st.AircraftByICAO(ctx, c.icao)
		if err != nil {
			t.log.Debug("tracker: aircraft look-up failed", "icao", modes.FormatICAO(c.icao), "err", err)
			continue
		}
		first := row.FirstSeen
		if first == 0 {
			first = row.LastSeen
		}
		isNew := !seen || first == c.first
		t.mu.Lock()
		t.afChecked++
		if _, done := t.afFired[c.icao]; done {
			isNew = false
		}
		if isNew {
			if len(t.afOrder) >= afFiredMax {
				delete(t.afFired, t.afOrder[0])
				t.afOrder = t.afOrder[1:]
			}
			t.afFired[c.icao] = struct{}{}
			t.afOrder = append(t.afOrder, c.icao)
		}
		t.mu.Unlock()
		if isNew {
			out = append(out, c.icao)
		}
	}
	return out
}

// InvalidateRefdata forgets the cached aircraft-database look-ups and
// schedules a fresh one for every tracked aircraft. Call it after the
// aircraft database was imported or replaced.
func (t *Tracker) InvalidateRefdata() {
	if s := t.sh.Load(); s != nil {
		for _, p := range s.parts {
			p.InvalidateRefdata()
		}
		s.invalidate()
		return
	}
	t.mu.Lock()
	t.acdb = make(map[uint32]acdbCached)
	t.acdbOrder = t.acdbOrder[:0]
	t.acdbQ = t.acdbQ[:0]
	if t.st != nil {
		for key, ac := range t.ac {
			if key&modes.NonICAOFlag == 0 && len(t.acdbQ) < acdbQueueMax {
				ac.acdbOK, ac.acdbRec = false, refdata.ACRecord{}
				t.rebuildRef(ac)
				t.acdbQ = append(t.acdbQ, key)
			}
		}
	}
	t.mu.Unlock()
}
