package navdata

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Dataset names one importable dataset.
type Dataset string

// Datasets.
const (
	DatasetCountries Dataset = "countries"
	DatasetAirports  Dataset = "airports"
	DatasetRunways   Dataset = "runways"
	DatasetNavaids   Dataset = "navaids"
	DatasetRoutes    Dataset = "routes"
)

// AllDatasets in import order.
var AllDatasets = []Dataset{DatasetCountries, DatasetAirports, DatasetRunways, DatasetNavaids, DatasetRoutes}

// Source describes where a dataset comes from (show Licence + Attribution in the UI).
type Source struct {
	Dataset     Dataset `json:"dataset"`
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Homepage    string  `json:"homepage"`
	Licence     string  `json:"licence"`
	LicenceURL  string  `json:"licenceUrl"`
	Attribution string  `json:"attribution"`
	ApproxBytes int64   `json:"approxBytes"`
	Format      string  `json:"format"` // "csv" | "zip"
}

const (
	ourAirportsBase = "https://davidmegginson.github.io/ourairports-data/"
	ourAirportsHome = "https://ourairports.com/data/"
	ourAirportsLic  = "Public domain (OurAirports data dumps; repository licence: The Unlicense)"
	standingDataZip = "https://github.com/vradarserver/standing-data/archive/refs/heads/main.zip"
)

// Sources returns the default sources (verified November 2026).
func Sources() []Source {
	oa := func(ds Dataset, file string, size int64) Source {
		return Source{Dataset: ds, Name: "OurAirports " + file, URL: ourAirportsBase + file, Homepage: ourAirportsHome,
			Licence: ourAirportsLic, LicenceURL: "https://github.com/davidmegginson/ourairports-data",
			Attribution: "Airport data: OurAirports (ourairports.com)", ApproxBytes: size, Format: "csv"}
	}
	return []Source{
		oa(DatasetCountries, "countries.csv", 25_000),
		oa(DatasetAirports, "airports.csv", 12_800_000),
		oa(DatasetRunways, "runways.csv", 4_000_000),
		oa(DatasetNavaids, "navaids.csv", 1_500_000),
		{Dataset: DatasetRoutes, Name: "Virtual Radar Server standing data (routes)", URL: standingDataZip,
			Homepage: "https://github.com/vradarserver/standing-data", Licence: "CC0 1.0 Universal",
			LicenceURL:  "https://github.com/vradarserver/standing-data/blob/main/LICENSE",
			Attribution: "Routes: Virtual Radar Server standing data (sdm.virtualradarserver.co.uk contributors)",
			ApproxBytes: 7_200_000, Format: "zip"},
	}
}

// SourceFor returns the default source of ds.
func SourceFor(ds Dataset) (Source, bool) {
	for _, s := range Sources() {
		if s.Dataset == ds {
			return s, true
		}
	}
	return Source{}, false
}

// ImportOptions select what to import.
type ImportOptions struct {
	Datasets []Dataset          `json:"datasets,omitempty"` // default AllDatasets
	URLs     map[Dataset]string `json:"-"`                  // per-dataset URL override (tests, mirrors)
	// Force re-imports even when the server reports the file unchanged (ETag).
	Force bool `json:"force,omitempty"`
	// Progress is called (from the import goroutine) at most ~4 times/s plus at stage changes.
	Progress func(Progress) `json:"-"`
	// MaxBytes caps each download (default 256 MiB).
	MaxBytes int64 `json:"-"`
	// IdleTimeout aborts a download that delivers no data for this long (default 60 s).
	IdleTimeout time.Duration `json:"-"`
}

// Progress reports the state of a running import.
type Progress struct {
	Dataset    Dataset `json:"dataset"`
	Stage      string  `json:"stage"` // "download" | "import" | "unchanged" | "done" | "error"
	BytesRead  int64   `json:"bytesRead"`
	BytesTotal int64   `json:"bytesTotal"` // -1 unknown
	Records    int     `json:"records"`
	Skipped    int     `json:"skipped"`
	Index      int     `json:"index"` // 1-based dataset number
	Count      int     `json:"count"` // datasets in this run
	Error      string  `json:"error,omitempty"`
}

// Fraction returns the overall completion 0..1.
func (p Progress) Fraction() float64 {
	if p.Count <= 0 {
		return 0
	}
	f := 0.0
	if p.BytesTotal > 0 {
		f = float64(p.BytesRead) / float64(p.BytesTotal)
		if f > 1 {
			f = 1
		}
	}
	if p.Stage == "done" || p.Stage == "unchanged" {
		f = 1
	}
	return (float64(p.Index-1) + f) / float64(p.Count)
}

// DatasetResult is the outcome of one dataset import.
type DatasetResult struct {
	Dataset      Dataset `json:"dataset"`
	URL          string  `json:"url,omitempty"`
	Records      int     `json:"records"`
	Skipped      int     `json:"skipped"`
	Bytes        int64   `json:"bytes"`
	Unchanged    bool    `json:"unchanged,omitempty"`
	LastModified string  `json:"lastModified,omitempty"`
	ETag         string  `json:"etag,omitempty"`
	ElapsedMs    int64   `json:"elapsedMs"`
	Error        string  `json:"error,omitempty"`
}

// ImportResult is the outcome of an Import call.
type ImportResult struct {
	Datasets  []DatasetResult `json:"datasets"`
	Started   int64           `json:"started"` // Unix ms
	Finished  int64           `json:"finished"`
	Cancelled bool            `json:"cancelled,omitempty"`
}

const chunkRows = 10000

// Import downloads and imports the selected datasets one after another. It
// blocks; run it in a goroutine or use StartImport. Datasets that fail keep
// their previous contents; the first error is returned after all datasets
// were tried (cancellation stops at once).
func (n *NavData) Import(ctx context.Context, o ImportOptions) (ImportResult, error) {
	if n.st == nil {
		return ImportResult{}, ErrNoStore
	}
	if len(o.Datasets) == 0 {
		o.Datasets = AllDatasets
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 256 << 20
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 60 * time.Second
	}
	res := ImportResult{Started: time.Now().UnixMilli()}
	free0 := n.freePages(ctx)
	changed := false
	defer func() {
		if changed && ctx.Err() == nil {
			n.releaseImportSpace(ctx, free0)
		}
	}()
	var firstErr error
	for i, ds := range o.Datasets {
		src, ok := SourceFor(ds)
		if !ok {
			err := fmt.Errorf("%w: unknown dataset %q", ErrBadInput, ds)
			res.Datasets = append(res.Datasets, DatasetResult{Dataset: ds, Error: err.Error()})
			firstErr = cmpErr(firstErr, err)
			continue
		}
		if u := o.URLs[ds]; u != "" {
			src.URL = u
		}
		rep := newReporter(o.Progress, ds, i+1, len(o.Datasets))
		dr, err := n.importOne(ctx, src, o, rep)
		changed = changed || (err == nil && !dr.Unchanged)
		if err != nil {
			dr.Error = err.Error()
			rep.final("error", err)
			n.log.Warn("navdata: import failed", "dataset", ds, "err", err)
		} else {
			stage := "done"
			if dr.Unchanged {
				stage = "unchanged"
			}
			rep.final(stage, nil)
			n.log.Info("navdata: imported", "dataset", ds, "records", dr.Records, "skipped", dr.Skipped,
				"unchanged", dr.Unchanged, "ms", dr.ElapsedMs)
		}
		res.Datasets = append(res.Datasets, dr)
		if ctx.Err() != nil {
			res.Cancelled = true
			res.Finished = time.Now().UnixMilli()
			return res, ctx.Err()
		}
		firstErr = cmpErr(firstErr, err)
	}
	res.Finished = time.Now().UnixMilli()
	return res, firstErr
}

// vacuumStepPages is the number of free pages one incremental-vacuum step
// returns to the file system (one writer transaction, ~8 MB).
const vacuumStepPages = 2000

// freePages returns the database's free-list length (0 when unknown).
func (n *NavData) freePages(ctx context.Context) int64 {
	var free int64
	if err := n.st.DB().QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0
	}
	return free
}

// releaseImportSpace runs after an import that replaced data: the swap
// dropped the previous tables (and the stage tables were refilled), so the
// file holds that many free pages, and the WAL grew by the whole dataset.
// It returns the pages the import freed (beyond the free0 there were
// before) to the file system with incremental vacuum, vacuumStepPages per
// writer transaction with a pause in between so the station's own writes
// interleave, and then truncates the WAL (TRUNCATE checkpoint). A file
// created without incremental auto-vacuum keeps its free pages for reuse.
// Errors are logged: the import itself has succeeded.
func (n *NavData) releaseImportSpace(ctx context.Context, free0 int64) {
	var mode int
	if err := n.st.DB().QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		n.log.Debug("navdata: auto_vacuum", "err", err)
	}
	var released int64
	if mode == 2 { // INCREMENTAL
		goal := n.freePages(ctx) - free0
		for released < goal && ctx.Err() == nil {
			step := min(int64(vacuumStepPages), goal-released)
			before := n.freePages(ctx)
			err := n.st.Write(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, step))
				return err
			})
			if err != nil {
				n.log.Warn("navdata: returning free pages after the import", "err", err)
				break
			}
			got := before - n.freePages(ctx)
			if got <= 0 {
				break
			}
			released += got
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	if err := n.st.Checkpoint(ctx); err != nil && ctx.Err() == nil {
		n.log.Debug("navdata: checkpoint after the import", "err", err)
	}
	if released > 0 {
		n.log.Info("navdata: space of the replaced data returned", "pages", released)
	}
}

func cmpErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}

// ImportReader imports one dataset from a local reader (a user-supplied file:
// a CSV for the OurAirports datasets, a standing-data zip or a single routes
// CSV for DatasetRoutes).
func (n *NavData) ImportReader(ctx context.Context, ds Dataset, r io.Reader, progress func(Progress)) (DatasetResult, error) {
	if n.st == nil {
		return DatasetResult{}, ErrNoStore
	}
	rep := newReporter(progress, ds, 1, 1)
	start := time.Now()
	free0 := n.freePages(ctx)
	cr := &countReader{r: r, rep: rep, total: -1}
	st, err := n.load(ctx, ds, cr, -1, 1<<30, rep)
	if err == nil && ctx.Err() == nil {
		defer n.releaseImportSpace(ctx, free0)
	}
	dr := DatasetResult{Dataset: ds, Records: st.Records, Skipped: st.Skipped, Bytes: cr.n.Load(),
		ElapsedMs: time.Since(start).Milliseconds()}
	if err == nil {
		err = n.writeMeta(ctx, dr)
	}
	if err != nil {
		dr.Error = err.Error()
		rep.final("error", err)
		return dr, err
	}
	rep.final("done", nil)
	return dr, nil
}

func (n *NavData) importOne(ctx context.Context, src Source, o ImportOptions, rep *reporter) (DatasetResult, error) {
	start := time.Now()
	dr := DatasetResult{Dataset: src.Dataset, URL: src.URL}
	opts := n.options()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return dr, err
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	if !o.Force {
		if m, ok := n.meta(ctx, src.Dataset); ok && m.ETag != "" && m.URL == src.URL && m.Records > 0 {
			req.Header.Set("If-None-Match", m.ETag)
		}
	}
	rep.stage("download", 0, -1)
	resp, err := opts.Client.Do(req)
	if err != nil {
		return dr, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		dr.Unchanged = true
		dr.ElapsedMs = time.Since(start).Milliseconds()
		return dr, nil
	}
	if resp.StatusCode != http.StatusOK {
		return dr, fmt.Errorf("navdata: %s: HTTP %s", src.URL, resp.Status)
	}
	dr.LastModified = resp.Header.Get("Last-Modified")
	dr.ETag = resp.Header.Get("ETag")
	total := resp.ContentLength
	if total > o.MaxBytes {
		return dr, ErrTooLarge
	}
	cr := &countReader{r: resp.Body, rep: rep, total: total, max: o.MaxBytes}
	cr.last.Store(time.Now().UnixNano())
	// Idle watchdog.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if time.Since(time.Unix(0, cr.last.Load())) > o.IdleTimeout {
					cancel(ErrIdle)
					return
				}
			}
		}
	}()
	st, err := n.load(ctx, src.Dataset, cr, total, o.MaxBytes, rep)
	close(stop)
	wg.Wait()
	if c := context.Cause(ctx); errors.Is(c, ErrIdle) {
		err = ErrIdle
	}
	dr.Records, dr.Skipped, dr.Bytes = st.Records, st.Skipped, cr.n.Load()
	dr.ElapsedMs = time.Since(start).Milliseconds()
	if err != nil {
		return dr, err
	}
	return dr, n.writeMeta(ctx, dr)
}

// load parses r into the stage table of ds and swaps it in.
func (n *NavData) load(ctx context.Context, ds Dataset, r io.Reader, total, max int64, rep *reporter) (ParseStats, error) {
	var tbl table
	switch ds {
	case DatasetCountries:
		tbl = tblCountry
	case DatasetAirports:
		tbl = tblAirport
	case DatasetRunways:
		tbl = tblRunway
	case DatasetNavaids:
		tbl = tblNavaid
	case DatasetRoutes:
		tbl = tblRoute
	default:
		return ParseStats{}, fmt.Errorf("%w: unknown dataset %q", ErrBadInput, ds)
	}
	w := &stageWriter{n: n, ctx: ctx, tbl: tbl, rep: rep}
	if err := w.begin(); err != nil {
		return ParseStats{}, err
	}
	rep.stage("import", 0, total)
	var st ParseStats
	var err error
	switch ds {
	case DatasetCountries:
		st, err = ParseCountries(r, func(c Country) error { return w.add(c.Code, c.Name, c.Continent) })
	case DatasetAirports:
		st, err = ParseAirports(r, func(a Airport) error {
			return w.add(a.ID, a.Ident, a.Type, int(a.Size), a.Name, a.Lat, a.Lon, ni(a.ElevFt), a.Continent, a.Country,
				a.Region, a.Municipality, b2i(a.Scheduled), a.ICAO, a.IATA, a.GPS, a.Local)
		})
	case DatasetRunways:
		st, err = ParseRunways(r, func(x Runway) error {
			return w.add(x.ID, x.AirportID, x.AirportIdent, ni(x.LengthFt), ni(x.WidthFt), x.Surface, b2i(x.Lighted), b2i(x.Closed),
				x.LE.Ident, nf(x.LE.Lat), nf(x.LE.Lon), ni(x.LE.ElevFt), nf(x.LE.HeadingT), ni(x.LE.DisplacedFt),
				x.HE.Ident, nf(x.HE.Lat), nf(x.HE.Lon), ni(x.HE.ElevFt), nf(x.HE.HeadingT), ni(x.HE.DisplacedFt))
		})
	case DatasetNavaids:
		st, err = ParseNavaids(r, func(x Navaid) error {
			return w.add(x.ID, x.Ident, x.Name, x.Type, ni(x.FreqKHz), x.Lat, x.Lon, ni(x.ElevFt), x.Country, x.DMEChannel,
				nf(x.MagVarDeg), x.Usage, x.Power, x.Airport)
		})
	case DatasetRoutes:
		st, err = parseRoutesAny(ctx, r, max, func(rr RouteRecord) error {
			return w.add(rr.Callsign, rr.Airline, rr.Number, strings.Join(rr.Codes, "-"))
		})
	}
	rep.counts(st.Records, st.Skipped)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && st.Records == 0 {
		err = ErrNoRecords
	}
	if err == nil {
		err = w.commit()
	}
	if err != nil {
		w.abort()
		return st, err
	}
	return st, nil
}

// parseRoutesAny accepts a standing-data repository zip (all
// routes/schema-01/**/*.csv members) or a single routes CSV.
func parseRoutesAny(ctx context.Context, r io.Reader, max int64, fn func(RouteRecord) error) (ParseStats, error) {
	br := &peekReader{r: r}
	head, _ := br.peek(4)
	if !bytes.Equal(head, []byte("PK\x03\x04")) {
		return ParseRoutesCSV(br, fn)
	}
	data, err := io.ReadAll(io.LimitReader(br, max+1))
	if err != nil {
		return ParseStats{}, err
	}
	if int64(len(data)) > max {
		return ParseStats{}, ErrTooLarge
	}
	return ParseRoutesZip(ctx, bytes.NewReader(data), int64(len(data)), fn)
}

// ParseRoutesZip parses every routes CSV inside a standing-data repository zip.
func ParseRoutesZip(ctx context.Context, ra io.ReaderAt, size int64, fn func(RouteRecord) error) (ParseStats, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return ParseStats{}, fmt.Errorf("navdata: routes zip: %w", err)
	}
	var total ParseStats
	files := 0
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "routes/schema-01/") || !strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if f.UncompressedSize64 > 64<<20 {
			total.Skipped++
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return total, err
		}
		st, err := ParseRoutesCSV(io.LimitReader(rc, 64<<20), fn)
		rc.Close()
		total.Records += st.Records
		total.Skipped += st.Skipped
		if errors.Is(err, ErrBadHeader) {
			total.Skipped++
			continue
		}
		if err != nil {
			return total, err
		}
		files++
	}
	if files == 0 {
		return total, fmt.Errorf("%w: no routes/schema-01 CSV files in the zip", ErrNoRecords)
	}
	return total, nil
}

type peekReader struct {
	r   io.Reader
	buf []byte
}

func (p *peekReader) peek(k int) ([]byte, error) {
	for len(p.buf) < k {
		b := make([]byte, k-len(p.buf))
		m, err := p.r.Read(b)
		p.buf = append(p.buf, b[:m]...)
		if err != nil {
			return p.buf, err
		}
	}
	return p.buf[:k], nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.buf) > 0 {
		m := copy(b, p.buf)
		p.buf = p.buf[m:]
		return m, nil
	}
	return p.r.Read(b)
}

// stageWriter collects rows and writes them to the stage table in chunks.
type stageWriter struct {
	n    *NavData
	ctx  context.Context
	tbl  table
	rep  *reporter
	rows [][]any
	done int
}

func (w *stageWriter) begin() error {
	return w.n.st.Write(w.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(w.ctx, `DELETE FROM `+w.tbl.stage())
		return err
	})
}

func (w *stageWriter) add(args ...any) error {
	w.rows = append(w.rows, args)
	if len(w.rows) >= chunkRows {
		return w.flush()
	}
	return nil
}

func (w *stageWriter) flush() error {
	if len(w.rows) == 0 {
		return nil
	}
	rows := w.rows
	err := w.n.st.Write(w.ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(w.ctx, fmt.Sprintf(w.tbl.insert, w.tbl.stage()))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, a := range rows {
			if _, err := stmt.ExecContext(w.ctx, a...); err != nil {
				return err
			}
		}
		return nil
	})
	w.done += len(rows)
	w.rows = w.rows[:0]
	w.rep.counts(w.done, -1)
	return err
}

func (w *stageWriter) commit() error {
	if err := w.flush(); err != nil {
		return err
	}
	w.rows = nil // release the chunk buffer
	return w.n.st.Write(w.ctx, func(tx *sql.Tx) error {
		for _, q := range w.tbl.swapSQL() {
			if _, err := tx.ExecContext(w.ctx, q); err != nil {
				return fmt.Errorf("navdata: swap %s: %w", w.tbl.name, err)
			}
		}
		return nil
	})
}

func (w *stageWriter) abort() {
	w.rows = nil
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = w.n.st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM `+w.tbl.stage())
		return err
	})
}

// countReader counts bytes, enforces the size cap and reports progress.
type countReader struct {
	r     io.Reader
	n     atomic.Int64
	last  atomic.Int64
	total int64
	max   int64
	rep   *reporter
}

func (c *countReader) Read(b []byte) (int, error) {
	m, err := c.r.Read(b)
	if m > 0 {
		v := c.n.Add(int64(m))
		c.last.Store(time.Now().UnixNano())
		if c.max > 0 && v > c.max {
			return m, ErrTooLarge
		}
		c.rep.bytes(v, c.total)
	}
	return m, err
}

// reporter throttles progress callbacks.
type reporter struct {
	fn   func(Progress)
	p    Progress
	last time.Time
}

func newReporter(fn func(Progress), ds Dataset, idx, count int) *reporter {
	return &reporter{fn: fn, p: Progress{Dataset: ds, Index: idx, Count: count, BytesTotal: -1}}
}

func (r *reporter) emit(force bool) {
	if r.fn == nil {
		return
	}
	if !force && time.Since(r.last) < 250*time.Millisecond {
		return
	}
	r.last = time.Now()
	r.fn(r.p)
}

func (r *reporter) stage(s string, read, total int64) {
	r.p.Stage = s
	if read > 0 {
		r.p.BytesRead = read
	}
	if total != 0 {
		r.p.BytesTotal = total
	}
	r.emit(true)
}

func (r *reporter) bytes(read, total int64) {
	r.p.BytesRead = read
	r.p.BytesTotal = total
	r.emit(false)
}

func (r *reporter) counts(rec, skipped int) {
	r.p.Records = rec
	if skipped >= 0 {
		r.p.Skipped = skipped
	}
	r.emit(false)
}

func (r *reporter) final(stage string, err error) {
	r.p.Stage = stage
	if err != nil {
		r.p.Error = err.Error()
	}
	r.emit(true)
}

func ni(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nf(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- import job (for the HTTP API) ---

// ImportStatus is the state of the background import job.
type ImportStatus struct {
	Running  bool          `json:"running"`
	Progress *Progress     `json:"progress,omitempty"`
	Fraction float64       `json:"fraction"`
	Result   *ImportResult `json:"result,omitempty"` // last finished run
	Error    string        `json:"error,omitempty"`  // last run's error
	Started  int64         `json:"started,omitempty"`
}

type importJob struct {
	cancel context.CancelFunc
	done   chan struct{}
	status ImportStatus
}

// StartImport runs Import in the background (one at a time). parent bounds
// the job's lifetime (pass the engine's context); CancelImport stops it.
func (n *NavData) StartImport(parent context.Context, o ImportOptions) error {
	if n.st == nil {
		return ErrNoStore
	}
	n.mu.Lock()
	if n.job != nil && n.job.status.Running {
		n.mu.Unlock()
		return ErrImportRunning
	}
	ctx, cancel := context.WithCancel(parent)
	job := &importJob{cancel: cancel, done: make(chan struct{}),
		status: ImportStatus{Running: true, Started: time.Now().UnixMilli()}}
	n.job = job
	n.mu.Unlock()

	user := o.Progress
	o.Progress = func(p Progress) {
		n.mu.Lock()
		pp := p
		job.status.Progress = &pp
		job.status.Fraction = p.Fraction()
		n.mu.Unlock()
		if user != nil {
			user(p)
		}
	}
	go func() {
		defer close(job.done)
		defer cancel()
		res, err := n.Import(ctx, o)
		// The parse buffers (a routes zip is read into memory) are garbage
		// now: return them to the OS so a Pi does not keep the peak.
		debug.FreeOSMemory()
		n.mu.Lock()
		job.status.Running = false
		job.status.Result = &res
		if err != nil {
			job.status.Error = err.Error()
		} else {
			job.status.Fraction = 1
		}
		n.mu.Unlock()
	}()
	return nil
}

// CancelImport cancels a running import (no-op otherwise) and waits until it stopped.
func (n *NavData) CancelImport() {
	n.mu.Lock()
	job := n.job
	n.mu.Unlock()
	if job == nil {
		return
	}
	job.cancel()
	<-job.done
}

// ImportStatus returns the state of the current or last import job.
func (n *NavData) ImportStatus() ImportStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.job == nil {
		return ImportStatus{}
	}
	s := n.job.status
	if s.Progress != nil {
		p := *s.Progress
		s.Progress = &p
	}
	return s
}
