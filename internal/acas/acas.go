// Package acas records ACAS (TCAS II) resolution advisories reported by
// aircraft in Mode S replies: the DF16 MV field, BDS 3,0 in DF20/21 replies
// and the DF17/18 TC28 subtype 2 squitter (internal/modes decodes them).
//
// Consecutive reports of one aircraft within GapMs form one event: when it
// started and ended, where the aircraft was, every advisory it reported
// (e.g. "Climb" then "Increase climb"), the threat identity when given and
// whether the aircraft reported the RA as terminated. Events are kept in
// memory while open and written to SQLite (table acas_event) when they
// close. The tracker calls Observe under its lock: it is a map update.
//
// What an RA report is and is not: it is the aircraft's own statement that
// its collision avoidance system issued an advisory. A received report says
// nothing about whether the crew followed it, and a missing report does not
// mean there was none (BDS 3,0 is only sent when a ground station asks for
// it; the DF16 reply only goes to another ACAS; not every transponder sends
// TC28). The decoding follows ICAO Annex 10 Vol IV (ARA/RAC/RAT/MTE fields).
package acas

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"airview/internal/modes"
	"airview/internal/store"
)

// MigrationName owns the acas_* tables.
const MigrationName = "acas"

const migrationV1 = `
CREATE TABLE IF NOT EXISTS acas_event(
  id INTEGER PRIMARY KEY,
  icao INTEGER NOT NULL, callsign TEXT NOT NULL DEFAULT '',
  start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL,
  lat REAL, lon REAL, alt_ft REAL,
  advisories TEXT NOT NULL, sense TEXT NOT NULL DEFAULT '', corrective INTEGER NOT NULL DEFAULT 0,
  ara INTEGER NOT NULL DEFAULT 0, rac INTEGER NOT NULL DEFAULT 0, mte INTEGER NOT NULL DEFAULT 0,
  threat_icao INTEGER, threat_alt_ft INTEGER, threat_range_nm REAL, threat_bearing REAL,
  reports INTEGER NOT NULL, sources TEXT NOT NULL DEFAULT '', terminated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS acas_event_start ON acas_event(start_ms);
CREATE INDEX IF NOT EXISTS acas_event_icao ON acas_event(icao, start_ms);
`

func init() {
	if err := store.RegisterMigration(MigrationName, 1, migrationV1); err != nil {
		panic(err)
	}
}

// Tunables.
const (
	GapMs        = 30_000 // reports further apart start a new event
	LiveMs       = 30_000 // an event counts as active this long after its last report
	maxOpen      = 4096   // open events kept in memory (a flood guard)
	retainDays   = 730
	flushEvery   = 5 * time.Second
	maxEventList = 5000
)

// Report is one decoded RA report.
type Report struct {
	T        int64  // data time, Unix ms
	ICAO     uint32 // the reporting aircraft
	Callsign string
	Lat, Lon float64
	PosOK    bool
	AltFt    float64
	AltOK    bool
	DF       uint8 // 16, 17/18 (TC28) or 20/21 (BDS 3,0)
	RA       modes.ACASRA
}

// Source names the message type of a report.
func (r *Report) Source() string {
	switch r.DF {
	case 16:
		return "DF16"
	case 17, 18:
		return "TC28"
	case 20, 21:
		return "BDS3,0"
	}
	return fmt.Sprintf("DF%d", r.DF)
}

// Describe returns the advisory of an RA report in words, e.g. "Climb",
// "Increase descent", "Do not climb", "Multiple threats" ("" when the report
// carries no active advisory).
func Describe(ra *modes.ACASRA) (text, sense string, corrective bool) {
	if ra.ARA&0x2000 == 0 {
		if ra.MultipleThreat {
			return "Multiple threats", "", true
		}
		return "", "", false
	}
	sense = "up"
	if ra.Downward() {
		sense = "down"
	}
	corrective = ra.Corrective()
	dir := map[string]string{"up": "climb", "down": "descent"}[sense]
	verb := map[string]string{"up": "Climb", "down": "Descend"}[sense]
	switch {
	case !corrective && sense == "up":
		text = "Do not descend"
	case !corrective:
		text = "Do not climb"
	case ra.SenseReversal():
		text = "Reversal: " + strings.ToLower(verb)
	case ra.IncreasedRate():
		text = "Increase " + dir
	case ra.Positive() && ra.AltitudeCrossing():
		text = verb + ", crossing"
	case ra.Positive():
		text = verb
	default:
		text = "Level off"
	}
	if ra.MultipleThreat {
		text += " (multiple threats)"
	}
	return text, sense, corrective
}

// Complement describes the RAC field (advisories received from other ACAS
// aircraft), e.g. "do not pass below".
func Complement(rac uint8) string {
	var p []string
	if rac&8 != 0 {
		p = append(p, "do not pass below")
	}
	if rac&4 != 0 {
		p = append(p, "do not pass above")
	}
	if rac&2 != 0 {
		p = append(p, "do not turn left")
	}
	if rac&1 != 0 {
		p = append(p, "do not turn right")
	}
	return strings.Join(p, ", ")
}

// Event is one resolution advisory episode of one aircraft.
type Event struct {
	ID         int64    `json:"id,omitempty"` // 0 while open
	ICAO       string   `json:"icao"`
	Callsign   string   `json:"callsign,omitempty"`
	Start      int64    `json:"start"` // Unix ms of the first report
	End        int64    `json:"end"`   // Unix ms of the last report
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
	AltFt      *float64 `json:"altFt,omitempty"`
	Advisories []string `json:"advisories"`           // every distinct advisory, in order
	Sense      string   `json:"sense,omitempty"`      // "up" | "down" (of the first active advisory)
	Corrective bool     `json:"corrective"`           // any corrective advisory
	ARA        uint16   `json:"ara"`                  // first active ARA field (raw)
	RAC        uint8    `json:"rac,omitempty"`        // OR of all RAC fields
	RACText    string   `json:"racText,omitempty"`    //
	MTE        bool     `json:"multipleThreats"`      //
	ThreatICAO string   `json:"threatIcao,omitempty"` // threat identity (type 1)
	ThreatAlt  *int32   `json:"threatAltFt,omitempty"`
	ThreatRng  *float64 `json:"threatRangeNm,omitempty"`
	ThreatBrg  *float64 `json:"threatBearing,omitempty"` // relative to own heading
	Reports    int      `json:"reports"`
	Sources    []string `json:"sources"`
	Terminated bool     `json:"terminated"` // the aircraft reported the RA as ended
	Open       bool     `json:"open,omitempty"`
}

// Recorder groups reports into events and stores them.
type Recorder struct {
	st  *store.Store
	log *slog.Logger

	mu     sync.Mutex
	open   map[uint32]*Event
	closed []*Event // waiting for the store
	total  uint64
	drops  uint64
}

// New creates a Recorder (st nil = memory only).
func New(st *store.Store, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{st: st, log: log, open: map[uint32]*Event{}}
}

// Observe adds one report (cheap: called under the tracker's lock).
func (r *Recorder) Observe(rep Report) {
	text, sense, corr := Describe(&rep.RA)
	if text == "" && !rep.RA.Terminated && rep.RA.RAC == 0 {
		return // no advisory in force (e.g. an all-zero BDS 3,0)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.open[rep.ICAO]
	if e != nil && rep.T-e.End > GapMs {
		r.closeLocked(rep.ICAO, e)
		e = nil
	}
	if e == nil {
		if text == "" {
			return // a termination or complement without an advisory we saw
		}
		if len(r.open) >= maxOpen {
			r.drops++
			return
		}
		e = &Event{ICAO: fmt.Sprintf("%06X", rep.ICAO&0xFFFFFF), Start: rep.T, End: rep.T, Open: true}
		r.open[rep.ICAO] = e
		r.total++
	}
	if rep.T > e.End {
		e.End = rep.T
	}
	e.Reports++
	if rep.Callsign != "" {
		e.Callsign = rep.Callsign
	}
	if rep.PosOK && e.Lat == nil {
		la, lo := rep.Lat, rep.Lon
		e.Lat, e.Lon = &la, &lo
	}
	if rep.AltOK && e.AltFt == nil {
		a := rep.AltFt
		e.AltFt = &a
	}
	if text != "" {
		if len(e.Advisories) == 0 || e.Advisories[len(e.Advisories)-1] != text {
			if len(e.Advisories) < 16 {
				e.Advisories = append(e.Advisories, text)
			}
		}
		if e.Sense == "" {
			e.Sense, e.ARA = sense, rep.RA.ARA
		}
		e.Corrective = e.Corrective || corr
	}
	e.RAC |= rep.RA.RAC
	e.MTE = e.MTE || rep.RA.MultipleThreat
	e.Terminated = rep.RA.Terminated
	switch rep.RA.ThreatType {
	case 1:
		e.ThreatICAO = fmt.Sprintf("%06X", rep.RA.ThreatICAO&0xFFFFFF)
	case 2:
		if rep.RA.ThreatAltOK {
			v := rep.RA.ThreatAlt
			e.ThreatAlt = &v
		}
		if rep.RA.ThreatRangeOK {
			v := rep.RA.ThreatRangeNM
			e.ThreatRng = &v
		}
		if rep.RA.ThreatBrgOK {
			v := rep.RA.ThreatBearing
			e.ThreatBrg = &v
		}
	}
	src := rep.Source()
	found := false
	for _, s := range e.Sources {
		found = found || s == src
	}
	if !found {
		e.Sources = append(e.Sources, src)
	}
}

func (r *Recorder) closeLocked(icao uint32, e *Event) {
	delete(r.open, icao)
	e.Open = false
	e.RACText = Complement(e.RAC)
	r.closed = append(r.closed, e)
}

// Live is the live-picture mark of an aircraft with an active RA.
type Live struct {
	Advisory string `json:"advisory"`
	Sense    string `json:"sense,omitempty"`
	Since    int64  `json:"since"` // Unix ms
}

// LiveMarks returns the aircraft whose RA is active at nowMs.
func (r *Recorder) LiveMarks(nowMs int64) map[uint32]Live {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out map[uint32]Live
	for k, e := range r.open {
		if nowMs-e.End > LiveMs || len(e.Advisories) == 0 {
			continue
		}
		if out == nil {
			out = map[uint32]Live{}
		}
		out[k] = Live{Advisory: e.Advisories[len(e.Advisories)-1], Sense: e.Sense, Since: e.Start}
	}
	return out
}

// Run closes idle events and writes closed ones until ctx ends (then it
// writes everything, open events included).
func (r *Recorder) Run(ctx context.Context, now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	var lastPrune time.Time
	for {
		select {
		case <-ctx.Done():
			r.closeIdle(math.MaxInt64)
			r.flush(context.Background())
			return
		case <-t.C:
			r.closeIdle(now().UnixMilli() - GapMs)
			r.flush(ctx)
			if n := now(); n.Sub(lastPrune) > 24*time.Hour {
				lastPrune = n
				r.prune(ctx, n)
			}
		}
	}
}

// closeIdle closes the events whose last report is before cutoff (Unix ms).
func (r *Recorder) closeIdle(cutoff int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.open {
		if e.End < cutoff || cutoff == math.MaxInt64 {
			r.closeLocked(k, e)
		}
	}
}

func (r *Recorder) flush(ctx context.Context) {
	r.mu.Lock()
	evs := r.closed
	r.closed = nil
	r.mu.Unlock()
	if len(evs) == 0 || r.st == nil {
		return
	}
	err := r.st.Write(ctx, func(tx *sql.Tx) error {
		for _, e := range evs {
			var icao, ticao uint32
			fmt.Sscanf(e.ICAO, "%X", &icao)
			var threat any
			if e.ThreatICAO != "" {
				fmt.Sscanf(e.ThreatICAO, "%X", &ticao)
				threat = ticao
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO acas_event(icao, callsign, start_ms, end_ms, lat, lon, alt_ft, advisories, sense, corrective,
  ara, rac, mte, threat_icao, threat_alt_ft, threat_range_nm, threat_bearing, reports, sources, terminated)
  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				icao, e.Callsign, e.Start, e.End, nf(e.Lat), nf(e.Lon), nf(e.AltFt), strings.Join(e.Advisories, "|"), e.Sense, b2i(e.Corrective),
				e.ARA, e.RAC, b2i(e.MTE), threat, ni(e.ThreatAlt), nf(e.ThreatRng), nf(e.ThreatBrg), e.Reports, strings.Join(e.Sources, "|"), b2i(e.Terminated))
			if err != nil {
				return err
			}
			e.ID, _ = res.LastInsertId()
		}
		return nil
	})
	if err != nil {
		r.log.Warn("acas: storing events", "events", len(evs), "err", err)
		return
	}
	for _, e := range evs {
		r.log.Info("acas: resolution advisory", "icao", e.ICAO, "callsign", e.Callsign, "advisories", strings.Join(e.Advisories, " > "), "reports", e.Reports)
	}
}

func (r *Recorder) prune(ctx context.Context, now time.Time) {
	if r.st == nil {
		return
	}
	cut := now.Add(-retainDays * 24 * time.Hour).UnixMilli()
	_ = r.st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM acas_event WHERE start_ms < ?`, cut)
		return err
	})
}

// Stats are the recorder's counters.
type Stats struct {
	Events  uint64 `json:"events"`  // events started since the start
	Open    int    `json:"open"`    // events in progress
	Dropped uint64 `json:"dropped"` // reports dropped by the flood guard
}

// Stats returns the counters.
func (r *Recorder) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{Events: r.total, Open: len(r.open), Dropped: r.drops}
}

// Query selects events.
type Query struct {
	From, To int64  // Unix ms (To 0 = now)
	ICAO     uint32 // 0 = all
	Limit    int
}

// Events returns the stored and open events overlapping the range, newest
// first.
func (r *Recorder) Events(ctx context.Context, q Query) ([]Event, error) {
	if q.Limit <= 0 || q.Limit > maxEventList {
		q.Limit = 500
	}
	if q.To <= 0 {
		q.To = math.MaxInt64
	}
	var out []Event
	r.mu.Lock()
	for k, e := range r.open {
		if (q.ICAO == 0 || q.ICAO == k) && e.End >= q.From && e.Start <= q.To {
			c := *e
			c.Advisories = append([]string(nil), e.Advisories...)
			c.Sources = append([]string(nil), e.Sources...)
			c.RACText = Complement(c.RAC)
			out = append(out, c)
		}
	}
	for _, e := range r.closed {
		if (q.ICAO == 0 || fmt.Sprintf("%06X", q.ICAO) == e.ICAO) && e.End >= q.From && e.Start <= q.To {
			out = append(out, *e)
		}
	}
	r.mu.Unlock()
	if r.st != nil {
		where, args := `end_ms >= ? AND start_ms <= ?`, []any{q.From, q.To}
		if q.ICAO != 0 {
			where += ` AND icao = ?`
			args = append(args, q.ICAO)
		}
		args = append(args, q.Limit)
		rows, err := r.st.DB().QueryContext(ctx, `SELECT id, icao, callsign, start_ms, end_ms, lat, lon, alt_ft, advisories, sense, corrective,
  ara, rac, mte, threat_icao, threat_alt_ft, threat_range_nm, threat_bearing, reports, sources, terminated
  FROM acas_event WHERE `+where+` ORDER BY start_ms DESC LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			var icao uint32
			var lat, lon, alt, rng, brg sql.NullFloat64
			var ticao, talt sql.NullInt64
			var adv, srcs string
			var corr, mte, term int
			if err := rows.Scan(&e.ID, &icao, &e.Callsign, &e.Start, &e.End, &lat, &lon, &alt, &adv, &e.Sense, &corr,
				&e.ARA, &e.RAC, &mte, &ticao, &talt, &rng, &brg, &e.Reports, &srcs, &term); err != nil {
				return nil, err
			}
			e.ICAO = fmt.Sprintf("%06X", icao)
			e.Lat, e.Lon, e.AltFt, e.ThreatRng, e.ThreatBrg = fp(lat), fp(lon), fp(alt), fp(rng), fp(brg)
			if ticao.Valid {
				e.ThreatICAO = fmt.Sprintf("%06X", ticao.Int64)
			}
			if talt.Valid {
				v := int32(talt.Int64)
				e.ThreatAlt = &v
			}
			if adv != "" {
				e.Advisories = strings.Split(adv, "|")
			}
			if srcs != "" {
				e.Sources = strings.Split(srcs, "|")
			}
			e.Corrective, e.MTE, e.Terminated = corr != 0, mte != 0, term != 0
			e.RACText = Complement(e.RAC)
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// an event can be both closed-in-memory and freshly stored: keep one
	seen := map[string]bool{}
	dedup := out[:0]
	for _, e := range out {
		k := fmt.Sprintf("%s/%d", e.ICAO, e.Start)
		if seen[k] {
			continue
		}
		seen[k] = true
		dedup = append(dedup, e)
	}
	out = dedup
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nf(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ni(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

func fp(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	x := v.Float64
	return &x
}
