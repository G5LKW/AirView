package server

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"airview/internal/analytics"
	"airview/internal/astro"
	"airview/internal/engine"
	"airview/internal/geo"
	"airview/internal/met"
	"airview/internal/model"
	"airview/internal/modes"
	"airview/internal/refdata"
	"airview/internal/store"
)

// ---------------------------------------------------------------------------
// Location guesser
// ---------------------------------------------------------------------------

func apiGuess(s *Server, r *http.Request) (any, error) {
	ant, err := qAnt(r)
	if err != nil {
		return nil, err
	}
	return s.eng.Analyzer().GuessLocation(r.Context(), ant)
}

// apiGuessRun recomputes the estimate from the data as it is now instead of
// serving the cached one.
func apiGuessRun(s *Server, r *http.Request) (any, error) {
	ant, err := qAnt(r)
	if err != nil {
		return nil, err
	}
	// The heat grid is flushed every 15 s: make the newest cells visible.
	_ = s.eng.Tracker().Flush(r.Context())
	s.eng.Analyzer().Invalidate()
	s.cache.invalidate()
	return s.eng.Analyzer().GuessLocation(r.Context(), ant)
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

type aircraftListReply struct {
	Total  int                 `json:"total"` // rows matching the search
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
	Sort   string              `json:"sort"`
	Desc   bool                `json:"desc"`
	Rows   []store.AircraftRow `json:"rows"`
}

var aircraftSorts = map[string]bool{
	"last_seen": true, "first_seen": true, "icao": true, "callsign": true, "reg": true, "type": true,
	"operator": true, "country": true, "msgs": true, "flights": true, "positions": true,
	"max_dist_km": true, "max_alt": true,
}

func apiHistoryAircraft(s *Server, r *http.Request) (any, error) {
	q := store.AircraftQuery{Search: qStr(r, "q"), Sort: qStr(r, "sort")}
	if q.Sort == "" {
		q.Sort = "last_seen"
	}
	if !aircraftSorts[q.Sort] {
		return nil, badRequest("parameter sort: unknown column %q", q.Sort)
	}
	if len(q.Search) > 64 {
		return nil, badRequest("parameter q: too long")
	}
	// Newest / largest first unless asked otherwise; text columns ascending.
	defDesc := true
	switch q.Sort {
	case "icao", "callsign", "reg", "type", "operator", "country":
		defDesc = false
	}
	q.Desc = qBool(r, "desc", defDesc)
	var err error
	if q.Limit, err = qInt(r, "limit", 100, 1, 1000); err != nil {
		return nil, err
	}
	if q.Offset, err = qInt(r, "offset", 0, 0, 1<<30); err != nil {
		return nil, err
	}
	since, err := qInt64(r, "since", 0)
	if err != nil {
		return nil, err
	}
	until, err := qInt64(r, "until", 0)
	if err != nil {
		return nil, err
	}
	q.Since, q.Until = since/1000, until/1000
	s.freshAircraftRows(r.Context())
	rows, total, err := s.eng.Store().ListAircraft(r.Context(), q)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []store.AircraftRow{}
	}
	return aircraftListReply{Total: total, Limit: q.Limit, Offset: q.Offset, Sort: q.Sort, Desc: q.Desc, Rows: rows}, nil
}

type historyAircraftReply struct {
	*aircraftDetail
	Events []store.Event `json:"events"` // emergencies, records ... of this airframe, newest first
}

func apiHistoryAircraftOne(s *Server, r *http.Request) (any, error) {
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	d, err := s.aircraftDetail(r, key, limit)
	if err != nil {
		return nil, err
	}
	if d.Aircraft == nil && d.Seen == nil && len(d.Flights) == 0 && d.ACDB == nil {
		return nil, notFound("aircraft %s has never been seen", modes.FormatICAO(key))
	}
	ev, err := s.eng.Store().Events(r.Context(), store.EventQuery{ICAO: key, Limit: 50})
	if err != nil {
		return nil, err
	}
	if ev == nil {
		ev = []store.Event{}
	}
	return historyAircraftReply{d, ev}, nil
}

type flightsReply struct {
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
	Flights []store.Flight `json:"flights"` // newest first
}

func apiHistoryFlights(s *Server, r *http.Request) (any, error) {
	limit, err := qInt(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	offset, err := qInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		return nil, err
	}
	st := s.eng.Store()
	var fl []store.Flight
	if v := qStr(r, "icao"); v != "" {
		key, err := parseICAOParam(v)
		if err != nil {
			return nil, err
		}
		fl, err = st.FlightsForAircraft(r.Context(), key, limit, offset)
		if err != nil {
			return nil, err
		}
	} else if from, to, ok, err := qSpan(r); err != nil {
		return nil, err
	} else if ok {
		all, err := st.FlightsBetween(r.Context(), from.Unix(), to.Unix(), limit+offset)
		if err != nil {
			return nil, err
		}
		if offset < len(all) {
			fl = all[offset:]
		}
	} else if fl, err = st.RecentFlights(r.Context(), limit, offset); err != nil {
		return nil, err
	}
	if fl == nil {
		fl = []store.Flight{}
	}
	return flightsReply{Limit: limit, Offset: offset, Flights: fl}, nil
}

type flightReply struct {
	Flight   store.Flight   `json:"flight"`
	Trail    trailList      `json:"trail"` // stored (down-sampled) trail
	Info     aircraftInfo   `json:"info"`
	Live     bool           `json:"live"`     // the flight is still being tracked
	Airports flightAirports `json:"airports"` // guessed departure / arrival airports
}

func apiHistoryFlight(s *Server, r *http.Request) (any, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return nil, badRequest("invalid flight id")
	}
	ctx := r.Context()
	st := s.eng.Store()
	f, ok, err := st.FlightByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFound("flight %d not found", id)
	}
	out := flightReply{Flight: f, Trail: trailList{}}
	ps, err := st.Positions(ctx, id, 0, 0)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		out.Trail = append(out.Trail, p.TrailPoint())
	}
	out.Airports = s.flightAirports(ps)
	var rowp *store.AircraftRow
	s.freshAircraftRows(ctx)
	if row, ok, err := st.AircraftByICAO(ctx, f.ICAO); err != nil {
		return nil, err
	} else if ok {
		rowp = &row
	}
	var livep *model.Aircraft
	if ac, ok := s.eng.Tracker().Aircraft(f.ICAO); ok {
		out.Live = ac.FlightID == id
		if out.Live {
			livep = &ac
		}
	}
	out.Info = buildInfo(f.ICAO, livep, rowp, nil)
	if f.Callsign != "" && (out.Info.Callsign == nil || out.Info.Callsign.Callsign != f.Callsign) {
		ci := refdata.LookupCallsignFor(f.Callsign, f.ICAO&^modes.NonICAOFlag)
		out.Info.Callsign, out.Info.Spoken = &ci, ci.Spoken()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

type acdbInstalled struct {
	Installed bool                   `json:"installed"` // an import was recorded
	Rows      int64                  `json:"rows"`      // rows in the aircraft database
	Info      *engine.ACDBImportInfo `json:"info"`      // what was imported, null = nothing yet
}

func (s *Server) acdbInstalled(ctx context.Context) (acdbInstalled, error) {
	info, rows, ok, err := s.eng.AircraftDBInfo(ctx)
	if err != nil {
		return acdbInstalled{}, err
	}
	out := acdbInstalled{Installed: ok, Rows: rows}
	if ok {
		out.Info = &info
	}
	return out, nil
}

type dbStatsReply struct {
	Stats   store.Stats         `json:"stats"`
	Tables  map[string]int64    `json:"tables"`  // row count per table
	Storage model.StorageConfig `json:"storage"` // retention settings in force
	ACDB    acdbInstalled       `json:"acdb"`
	Tiles   tileStats           `json:"tileCache"`
	// Profile is the resolved storage profile (storage.profile) and
	// Migration the state of the background data migrations.
	Profile   store.ProfileView   `json:"profile"`
	Migration store.MigrationInfo `json:"migration"`
}

func apiDBStats(s *Server, r *http.Request) (any, error) {
	st := s.eng.Store()
	tables, err := st.TableCounts(r.Context())
	if err != nil {
		return nil, err
	}
	if tables == nil {
		tables = map[string]int64{}
	}
	ac, err := s.acdbInstalled(r.Context())
	if err != nil {
		return nil, err
	}
	out := dbStatsReply{
		Stats: st.Stats(), Tables: tables, Storage: s.eng.Config().Get().Storage, ACDB: ac, Tiles: s.tiles.stats(),
		Profile: st.Profile().View(), Migration: st.MigrationInfo(),
	}
	if !s.isAdmin(r) {
		out.Stats.Path, out.Tiles.Dir = "", "" // contain the OS user name
		if out.Migration.Backup != "" {
			out.Migration.Backup = filepath.Base(out.Migration.Backup)
		}
	}
	return out, nil
}

// maintCtx gives a maintenance job time beyond the request timeout and
// keeps it running when the browser gives up.
func maintCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Minute)
}

type maintReply struct {
	OK          bool               `json:"ok"`
	ElapsedMs   int64              `json:"elapsedMs"`
	BeforeBytes int64              `json:"beforeBytes"`
	AfterBytes  int64              `json:"afterBytes"`
	Hours       int                `json:"hours,omitempty"` // rollup: hours written
	Pruned      *store.PruneResult `json:"pruned,omitempty"`
	Total       int64              `json:"total,omitempty"` // prune: rows deleted
}

func (s *Server) maintain(r *http.Request, name string, fn func(ctx context.Context, out *maintReply) error) (any, error) {
	ctx, cancel := maintCtx(r)
	defer cancel()
	st := s.eng.Store()
	out := maintReply{BeforeBytes: st.DBSize()}
	t0 := time.Now()
	if err := fn(ctx, &out); err != nil {
		return nil, err
	}
	out.OK, out.ElapsedMs, out.AfterBytes = true, time.Since(t0).Milliseconds(), st.DBSize()
	s.log.Info("database maintenance", "job", name, "ms", out.ElapsedMs, "client", s.logClient(r))
	s.cache.invalidate()
	return out, nil
}

func apiDBVacuum(s *Server, r *http.Request) (any, error) {
	return s.maintain(r, "vacuum", func(ctx context.Context, _ *maintReply) error {
		return s.eng.Store().Vacuum(ctx)
	})
}

func apiDBPrune(s *Server, r *http.Request) (any, error) {
	return s.maintain(r, "prune", func(ctx context.Context, out *maintReply) error {
		res, err := s.eng.Store().Prune(ctx, s.eng.Config().Get().Storage)
		out.Pruned, out.Total = &res, res.Total()
		return err
	})
}

func apiDBCheckpoint(s *Server, r *http.Request) (any, error) {
	return s.maintain(r, "checkpoint", func(ctx context.Context, _ *maintReply) error {
		if err := s.eng.Tracker().Flush(ctx); err != nil {
			return err
		}
		return s.eng.Store().Checkpoint(ctx)
	})
}

func apiDBRollup(s *Server, r *http.Request) (any, error) {
	return s.maintain(r, "rollup", func(ctx context.Context, out *maintReply) error {
		n, err := s.eng.Store().RollupHours(ctx)
		out.Hours = n
		return err
	})
}

type acdbSourcesReply struct {
	Sources   []refdata.Source    `json:"sources"`
	Installed acdbInstalled       `json:"installed"`
	Status    engine.ImportStatus `json:"status"`
}

func apiACDBSources(s *Server, r *http.Request) (any, error) {
	ac, err := s.acdbInstalled(r.Context())
	if err != nil {
		return nil, err
	}
	return acdbSourcesReply{Sources: refdata.Sources(), Installed: ac, Status: s.eng.ImportStatus()}, nil
}

func apiACDBProgress(s *Server, r *http.Request) (any, error) {
	return s.eng.ImportStatus(), nil
}

func apiACDBImport(s *Server, r *http.Request) (any, error) {
	var req engine.ImportRequest
	if r.ContentLength != 0 {
		if err := readJSON(nil, r, &req, 8<<10); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	if req.Source != "" {
		if _, ok := refdata.SourceByID(req.Source); !ok {
			return nil, badRequest("unknown aircraft database source %q", req.Source)
		}
	}
	if req.URL != "" {
		u, err := url.Parse(req.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, badRequest("url must be an http or https address")
		}
	}
	if err := s.eng.ImportAircraftDB(req); err != nil {
		if errors.Is(err, engine.ErrImportRunning) {
			return nil, errStatus(http.StatusConflict, "an aircraft database import is already running")
		}
		return nil, err
	}
	return s.eng.ImportStatus(), nil
}

func apiACDBCancel(s *Server, r *http.Request) (any, error) {
	s.eng.CancelImport()
	return s.eng.ImportStatus(), nil
}

// acdbUploadMax bounds an uploaded database file.
const acdbUploadMax = 600 << 20

// acdbUploadMaxDuration bounds reading and importing one uploaded file.
const acdbUploadMaxDuration = time.Hour

// rawACDBUpload imports a database file sent as the request body. The body
// is parsed while it arrives (no spooling of up to 600 MB), so the request
// holds resetMu.RLock for as long as the client takes to send it; it is
// therefore registered as an abortable stream: a factory reset cancels the
// import and breaks a body read blocked on a slow sender instead of waiting
// for it.
func rawACDBUpload(s *Server, w http.ResponseWriter, r *http.Request) {
	s.resetMu.RLock()
	defer s.resetMu.RUnlock()
	r, end := s.beginAbortable(w, r, acdbUploadMaxDuration)
	defer end()
	if !s.eng.Running() {
		writeError(w, http.StatusServiceUnavailable, "engine is not running")
		return
	}
	if r.ContentLength > acdbUploadMax {
		writeError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	name := qStr(r, "name")
	if len(name) > 200 {
		name = name[:200]
	}
	st, err := s.eng.ImportAircraftDBFile(r.Context(), http.MaxBytesReader(w, r.Body, acdbUploadMax), r.ContentLength, name)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, engine.ErrImportRunning):
			code = http.StatusConflict
		case errors.Is(err, context.Canceled):
			code = 499
		}
		writeError(w, code, "import failed: "+err.Error())
		return
	}
	s.cache.invalidate()
	writeJSON(w, r, http.StatusOK, st)
}

type acdbLookupReply struct {
	ICAO   string           `json:"icao"`
	Found  bool             `json:"found"`
	Record *store.ACDBEntry `json:"record"`
	Info   aircraftInfo     `json:"info"`
}

func apiACDBLookup(s *Server, r *http.Request) (any, error) {
	key, err := parseICAOParam(qStr(r, "icao"))
	if err != nil {
		return nil, err
	}
	out := acdbLookupReply{ICAO: modes.FormatICAO(key)}
	e, ok, err := s.eng.Store().ACDBLookup(r.Context(), key&^modes.NonICAOFlag)
	if err != nil {
		return nil, err
	}
	if ok && key&modes.NonICAOFlag == 0 {
		out.Found, out.Record = true, &e
	}
	out.Info = buildInfo(key, nil, nil, out.Record)
	return out, nil
}

// exportTable describes one exportable table: its time column ("" = none)
// and the unit of that column in seconds.
type exportTable struct {
	timeCol string
	unit    int64
}

var exportTables = map[string]exportTable{
	"aircraft":        {"last_seen", 1},
	"flights":         {"start", 1},
	"positions":       {"t", 1},
	"stats_minute":    {"t", 1},
	"stats_hour":      {"t", 1},
	"range_hour":      {"t", 1},
	"coverage":        {"day", 86400},
	"signal_hist":     {"day", 86400},
	"elev_hist":       {"day", 86400},
	"heat":            {"", 0},
	"weather_obs":     {"t", 1},
	"weather_fc":      {"t", 1},
	"weather_fc_log":  {"t", 1},
	"ntp_samples":     {"t", 1},
	"met_obs":         {"t", 1},
	"met_profile":     {"hour", 1},
	"records":         {"", 0},
	"events":          {"t", 1},
	"acdb":            {"", 0},
	"receiver_status": {"t", 1},
}

// ExportTables lists the tables GET /api/db/export/<table>.csv accepts.
func ExportTables() []string {
	out := make([]string, 0, len(exportTables))
	for k := range exportTables {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func apiDBExportList(s *Server, r *http.Request) (any, error) {
	type item struct {
		Table   string `json:"table"`
		URL     string `json:"url"`
		Rows    int64  `json:"rows"`
		TimeCol string `json:"timeColumn,omitempty"` // column the from / to parameters filter on
	}
	// The row counts include what is still held in memory.
	s.flushForExport(r.Context())
	counts, err := s.eng.Store().TableCounts(r.Context())
	if err != nil {
		return nil, err
	}
	out := []item{}
	for _, t := range ExportTables() {
		out = append(out, item{t, "/api/db/export/" + t + ".csv", counts[t], exportTables[t].timeCol})
	}
	return map[string]any{"tables": out}, nil
}

// freshAircraftRows writes the aircraft rows the tracker holds back
// (storage.tuning.aircraftFlushSec) before a reply that shows stored
// aircraft rows, so that they are at most a minute old, as in 1.0.0. At
// most one extra commit a minute; errors only make the rows older.
func (s *Server) freshAircraftRows(ctx context.Context) {
	if tr := s.eng.Tracker(); tr != nil {
		_ = tr.FlushAircraftRows(ctx)
	}
}

// openHourFlusher is implemented by collectors that keep the open hour's
// rows in memory until the hour closes (the met collector's profile
// increments): FlushOpenHour writes them now without changing what the
// collector's own pages show.
type openHourFlusher interface {
	FlushOpenHour(ctx context.Context) error
}

// flushForExport writes what the station still holds in memory before a
// table export or the export list: the tracker's statistics increments
// (heat, coverage, histograms, the open hour's range; kept for up to the
// storage profile's flush intervals, docs/CONFIG.md) and the open hour of
// the met profile, so an export contains everything collected so far, as
// in 1.0.0 (15 s). Exports are rare; this costs one extra commit.
func (s *Server) flushForExport(ctx context.Context) {
	if c := s.eng.Met(); c != nil {
		if f, ok := any(c).(openHourFlusher); ok {
			_ = f.FlushOpenHour(ctx)
		}
	}
	_ = s.eng.Tracker().Flush(ctx) // ends with Store.Flush: everything above is committed
}

// rawDBExport streams one table as CSV (header row, RFC 4180, BLOBs as
// hex, NULL as an empty field).
func rawDBExport(s *Server, w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(r.PathValue("file"), ".csv")
	tbl, known := exportTables[name]
	if !ok || !known {
		writeError(w, http.StatusNotFound, "unknown table: use /api/db/export/<table>.csv")
		return
	}
	from, to, haveSpan, err := qSpan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := qInt64(r, "limit", 0)
	if err != nil || limit < 0 {
		writeError(w, http.StatusBadRequest, "parameter limit: not a positive integer")
		return
	}
	r, done, ok := s.exportBegin(w, r)
	if !ok {
		return
	}
	defer done()
	s.flushForExport(r.Context())

	// The table name comes from the whitelist above, never from the client.
	// Tables whose layout changed since 1.0.0 (positions, met_obs, heat ...)
	// are read through their logical form, which yields the 1.0.0 columns
	// and rows (store.ScanTable).
	var span *store.Span
	if haveSpan && tbl.timeCol != "" {
		span = &store.Span{From: from.Unix() / tbl.unit, To: (to.Unix() + tbl.unit - 1) / tbl.unit}
	}

	h := w.Header()
	var cw *csv.Writer
	var zw *gzip.Writer
	var rec []string
	rc := http.NewResponseController(w)
	header := func(cols []string) error {
		h.Set("Content-Type", "text/csv; charset=utf-8")
		h.Set("Content-Disposition", `attachment; filename="airview-`+name+`.csv"`)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		var out io.Writer = w
		if acceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
			zw, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
			out = zw
		}
		w.WriteHeader(http.StatusOK)
		cw = csv.NewWriter(out)
		rec = make([]string, len(cols))
		return cw.Write(cols)
	}
	errStop := errors.New("stop")
	n := 0
	row := func(vals []any) error {
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				rec[i] = ""
			case int64:
				rec[i] = strconv.FormatInt(x, 10)
			case float64:
				rec[i] = strconv.FormatFloat(x, 'g', -1, 64)
			case string:
				rec[i] = x
			case []byte:
				rec[i] = hex.EncodeToString(x)
			case bool:
				rec[i] = strconv.FormatBool(x)
			case time.Time:
				rec[i] = x.UTC().Format(time.RFC3339)
			default:
				rec[i] = fmt.Sprint(x)
			}
		}
		if cw.Write(rec) != nil {
			return errStop
		}
		if n++; n%2000 == 0 {
			if r.Context().Err() != nil {
				return errStop // aborted (factory reset) or exportMaxDuration reached
			}
			// A stalled client must not hold the read connection forever.
			_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
			cw.Flush()
			if cw.Error() != nil {
				return errStop
			}
		}
		return nil
	}
	err = s.eng.Store().ScanTable(r.Context(), name, tbl.timeCol, span, limit, header, row)
	if cw == nil {
		// Failed before the header: nothing was sent yet.
		msg := "export failed"
		if err != nil {
			msg = err.Error()
		}
		writeError(w, http.StatusInternalServerError, msg)
		return
	}
	if errors.Is(err, errStop) {
		return // client gone, aborted or stalled
	}
	if err != nil {
		// A row that cannot be read ends the download (as in 1.0.0).
		s.log.Debug("table export ended early", "table", name, "err", err)
	}
	cw.Flush()
	if zw != nil {
		_ = zw.Close()
	}
}

// ---------------------------------------------------------------------------
// Geo / astro / reference data
// ---------------------------------------------------------------------------

type geoFormats struct {
	Decimal     string `json:"decimal"`     // "50.05000, 8.60000"
	DMS         string `json:"dms"`         // degrees, minutes, seconds
	DDM         string `json:"ddm"`         // degrees and decimal minutes
	Maidenhead  string `json:"maidenhead"`  // 6-character locator
	Maidenhead8 string `json:"maidenhead8"` // 8-character locator
}

func formatsOf(lat, lon float64) geoFormats {
	return geoFormats{
		Decimal: geo.FormatDecimal(lat, lon, 5), DMS: geo.FormatDMS(lat, lon, 1), DDM: geo.FormatDDM(lat, lon, 3),
		Maidenhead: geo.Maidenhead(lat, lon, 6), Maidenhead8: geo.Maidenhead(lat, lon, 8),
	}
}

type geoParseReply struct {
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"` // why the text was not understood (ok = false)
	Input     string      `json:"input"`
	Lat       num         `json:"lat"`
	Lon       num         `json:"lon"`
	Format    string      `json:"format,omitempty"` // detected notation, e.g. "decimal degrees"
	Formatted *geoFormats `json:"formatted,omitempty"`
	// Distance and bearing from the configured receiver (when located).
	DistKm  num `json:"distKm"`
	Bearing num `json:"bearing"`
}

// geoParsePublic is geoParseReply without the receiver-relative members.
type geoParsePublic struct {
	OK        bool        `json:"ok"`
	Input     string      `json:"input"`
	Lat       num         `json:"lat"`
	Lon       num         `json:"lon"`
	Format    string      `json:"format,omitempty"`
	Formatted *geoFormats `json:"formatted,omitempty"`
}

// apiGeoParse always answers 200: "not understood" is a normal result for
// a text box that is still being typed into.
func apiGeoParse(s *Server, r *http.Request) (any, error) {
	in := r.URL.Query().Get("q")
	if len(in) > 2048 {
		return nil, badRequest("parameter q: too long")
	}
	out := geoParseReply{Input: in, Lat: num(nan), Lon: num(nan), DistKm: num(nan), Bearing: num(nan)}
	if strings.TrimSpace(in) == "" {
		out.Error = "enter coordinates"
		return out, nil
	}
	lat, lon, format, err := geo.ParseLatLon(in)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	out.OK, out.Lat, out.Lon, out.Format = true, rnd(lat, 7), rnd(lon, 7), format
	f := formatsOf(lat, lon)
	out.Formatted = &f
	if s.netRestricted(r) {
		// Aggregator mode below admin: the distance and bearing from the
		// host receiver would locate it (DESIGN §8.2).
		return geoParsePublic{OK: out.OK, Input: out.Input, Lat: out.Lat, Lon: out.Lon, Format: out.Format, Formatted: out.Formatted}, nil
	}
	if rx := s.eng.EffectiveConfig().Receiver; rx.Located {
		km, brg := geo.DistanceBearing(rx.Lat, rx.Lon, lat, lon)
		out.DistKm, out.Bearing = rnd(km, 2), rnd(brg, 1)
	}
	return out, nil
}

func qLatLon(r *http.Request) (lat, lon float64, err error) {
	if qStr(r, "lat") == "" || qStr(r, "lon") == "" {
		return 0, 0, badRequest("parameters lat and lon are required")
	}
	if lat, err = qFloat(r, "lat", 0); err != nil {
		return
	}
	if lon, err = qFloat(r, "lon", 0); err != nil {
		return
	}
	if !geo.ValidLatLon(lat, lon) {
		err = badRequest("lat / lon out of range")
	}
	return
}

func apiGeoFormat(s *Server, r *http.Request) (any, error) {
	lat, lon, err := qLatLon(r)
	if err != nil {
		return nil, err
	}
	return map[string]any{"lat": lat, "lon": lon, "formatted": formatsOf(lat, lon)}, nil
}

type elevationReply2 struct {
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	ElevationM  num     `json:"elevationM"` // terrain height above mean sea level
	Source      string  `json:"source"`
	Attribution string  `json:"attribution"`
}

func apiGeoElevation(s *Server, r *http.Request) (any, error) {
	lat, lon, err := qLatLon(r)
	if err != nil {
		return nil, err
	}
	// Round to about 10 m so that the response cache is effective.
	lat, lon = math.Round(lat*1e4)/1e4, math.Round(lon*1e4)/1e4
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	m, err := s.elevation(ctx, lat, lon)
	if err != nil {
		return nil, errStatus(http.StatusBadGateway, "elevation look-up failed: %v", err)
	}
	return elevationReply2{
		Lat: lat, Lon: lon, ElevationM: rnd(m, 1), Source: "open-meteo",
		Attribution: "Elevation data by Open-Meteo.com (Copernicus DEM GLO-90)",
	}, nil
}

type magvarReply struct {
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	AltKm     float64 `json:"altKm"`
	T         int64   `json:"t"`
	MagVarDeg num     `json:"magVarDeg"` // declination, degrees east of true north
	InclDeg   num     `json:"inclDeg"`   // inclination, degrees down
	TotalNT   num     `json:"totalNt"`   // total field intensity
	HorizNT   num     `json:"horizNt"`   // horizontal intensity
	Model     string  `json:"model"`     //
	ModelYear num     `json:"decimalYear"`
}

func apiGeoMagVar(s *Server, r *http.Request) (any, error) {
	lat, lon, err := qLatLon(r)
	if err != nil {
		return nil, err
	}
	alt, err := qFloat(r, "altKm", 0)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	year := met.DecimalYear(now)
	out := magvarReply{
		Lat: lat, Lon: lon, AltKm: alt, T: now.UnixMilli(), Model: "WMM2025", ModelYear: rnd(year, 3),
		MagVarDeg: num(nan), InclDeg: num(nan), TotalNT: num(nan), HorizNT: num(nan),
	}
	if f, ok := met.MagField(lat, lon, alt, year); ok {
		out.MagVarDeg, out.InclDeg, out.TotalNT, out.HorizNT = rnd(f.D, 3), rnd(f.I, 3), rnd(f.F, 1), rnd(f.H, 1)
	}
	return out, nil
}

func apiAstroNow(s *Server, r *http.Request) (any, error) {
	tMs, err := qInt64(r, "t", 0)
	if err != nil {
		return nil, err
	}
	var t time.Time
	if tMs > 0 {
		t = time.UnixMilli(tMs)
	}
	return s.eng.Analyzer().Sun(r.Context(), t)
}

func apiAstroTerminator(s *Server, r *http.Request) (any, error) {
	tMs, err := qInt64(r, "t", 0)
	if err != nil {
		return nil, err
	}
	t := time.Now()
	if tMs > 0 {
		t = time.UnixMilli(tMs)
	}
	step, err := qFloat(r, "step", astro.DefaultTerminatorStepDeg)
	if err != nil {
		return nil, err
	}
	if step < 0.5 || step > 10 {
		return nil, badRequest("parameter step: 0.5 to 10 degrees")
	}
	return astro.NightSideAt(t, step).GeoJSON(), nil
}

type astroDayReply struct {
	Located  bool              `json:"located"`
	Receiver *analytics.LatLon `json:"receiver"`
	Days     []astro.EventsMs  `json:"days"` // one entry per day, starting today
}

func apiAstroDays(s *Server, r *http.Request) (any, error) {
	n, err := qInt(r, "days", 7, 1, 366)
	if err != nil {
		return nil, err
	}
	rx := s.eng.EffectiveConfig().Receiver
	lat, lon := rx.Lat, rx.Lon
	located := rx.Located
	if qStr(r, "lat") != "" || qStr(r, "lon") != "" {
		if lat, lon, err = qLatLon(r); err != nil {
			return nil, err
		}
		located = true
	}
	out := astroDayReply{Located: located, Days: []astro.EventsMs{}}
	if !located {
		return out, nil
	}
	out.Receiver = &analytics.LatLon{Lat: lat, Lon: lon}
	t := time.Now()
	for i := 0; i < n; i++ {
		out.Days = append(out.Days, astro.DayEvents(t.Add(time.Duration(i)*24*time.Hour), lat, lon).Ms())
	}
	return out, nil
}

func apiRefICAO(s *Server, r *http.Request) (any, error) {
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	return buildInfo(key, nil, nil, nil), nil
}

type callsignReply struct {
	refdata.CallsignInfo
	Spoken string `json:"spoken,omitempty"`
}

func apiRefCallsign(s *Server, r *http.Request) (any, error) {
	cs := r.PathValue("callsign")
	if len(cs) > 16 {
		return nil, badRequest("callsign too long")
	}
	ci := refdata.LookupCallsign(cs)
	return callsignReply{ci, ci.Spoken()}, nil
}

type typeReply struct {
	Found bool `json:"found"`
	refdata.AircraftType
	ClassText string `json:"classText,omitempty"` // "Landplane, 2 jet engines"
	WTCText   string `json:"wtcText,omitempty"`
	Engines   int    `json:"engines,omitempty"`
}

func apiRefType(s *Server, r *http.Request) (any, error) {
	code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
	t, ok := refdata.LookupType(code)
	if !ok {
		return typeReply{AircraftType: refdata.AircraftType{Designator: code}}, nil
	}
	return typeReply{Found: true, AircraftType: t, ClassText: t.ClassText(), WTCText: t.WTCText(), Engines: t.Engines()}, nil
}

type squawkReply struct {
	Found bool `json:"found"`
	Valid bool `json:"valid"` // four octal digits
	refdata.SquawkInfo
}

func apiRefSquawk(s *Server, r *http.Request) (any, error) {
	code := strings.TrimSpace(r.PathValue("code"))
	out := squawkReply{Valid: refdata.ValidSquawk(code)}
	out.Code = code
	if info, ok := refdata.Squawk(code); ok {
		out.Found, out.SquawkInfo = true, info
	}
	return out, nil
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
