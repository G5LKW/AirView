package engine

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"time"

	"airview/internal/refdata"
	"airview/internal/store"
)

// kvACDBImport holds the JSON summary (ACDBImportInfo) of the last import.
const kvACDBImport = "acdb.import"

// ErrImportRunning is returned when an aircraft database import is already
// in progress.
var ErrImportRunning = errors.New("engine: an aircraft database import is already running")

// ImportRequest selects what ImportAircraftDB downloads.
type ImportRequest struct {
	Source string `json:"source"` // refdata source ID; "" = refdata.SourceTar1090
	URL    string `json:"url"`    // optional mirror / custom URL
}

// ImportStatus is the state of the current or last aircraft database import.
type ImportStatus struct {
	Running bool `json:"running"`
	// Stage: "" (never ran), "download" (fetching and parsing into the
	// staging file), "store" (loading the staging file into the database),
	// "done", "error", "cancelled".
	Stage    string            `json:"stage"`
	Source   string            `json:"source,omitempty"`
	Started  int64             `json:"started,omitempty"`  // Unix ms
	Finished int64             `json:"finished,omitempty"` // Unix ms
	Progress *refdata.Progress `json:"progress,omitempty"` // download / parse progress
	Stored   int64             `json:"stored"`             // rows written to the database
	Error    string            `json:"error,omitempty"`
}

// ACDBImportInfo describes the aircraft database currently installed.
type ACDBImportInfo struct {
	Source       string `json:"source"`
	URL          string `json:"url,omitempty"`
	Records      int64  `json:"records"`
	T            int64  `json:"t"`                      // Unix ms of the import
	LastModified int64  `json:"lastModified,omitempty"` // Unix ms, server's Last-Modified
	Attribution  string `json:"attribution,omitempty"`
	Licence      string `json:"licence,omitempty"`
	// Format is the detected file format of an uploaded file ("tar1090",
	// "opensky"); empty for a download, whose Source names it.
	Format string `json:"format,omitempty"`
}

type importState struct {
	status ImportStatus
	cancel context.CancelFunc
	done   chan struct{}
}

// ImportStatus returns the state of the current or last import.
func (e *Engine) ImportStatus() ImportStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.imp.status
	if s.Progress != nil {
		p := *s.Progress
		s.Progress = &p
	}
	return s
}

// AircraftDBInfo returns the summary of the last successful import (ok false
// when none was recorded) and the number of rows in the acdb table.
func (e *Engine) AircraftDBInfo(ctx context.Context) (info ACDBImportInfo, rows int64, ok bool, err error) {
	if e.st == nil {
		return info, 0, false, ErrNotRunning
	}
	rows, err = e.st.ACDBCount(ctx)
	if err != nil {
		return info, 0, false, err
	}
	v, found, err := e.st.KVGet(ctx, kvACDBImport)
	if err != nil || !found {
		return info, rows, false, err
	}
	if json.Unmarshal([]byte(v), &info) != nil {
		return info, rows, false, nil
	}
	return info, rows, true, nil
}

// AircraftDBAttribution returns the attribution text of the installed
// aircraft database (the licence of e.g. tar1090-db requires it next to the
// data), falling back to the source's own text for a download; "" when no
// database is installed. Unlike AircraftDBInfo it does not count the rows, so
// it is cheap enough for every aircraft detail request.
func (e *Engine) AircraftDBAttribution(ctx context.Context) (string, error) {
	if e.st == nil {
		return "", ErrNotRunning
	}
	v, found, err := e.st.KVGet(ctx, kvACDBImport)
	if err != nil || !found {
		return "", err
	}
	var info ACDBImportInfo
	if json.Unmarshal([]byte(v), &info) != nil {
		return "", nil
	}
	if info.Attribution == "" {
		if src, ok := refdata.SourceByID(info.Source); ok {
			return src.Attribution, nil
		}
	}
	return info.Attribution, nil
}

// beginImport reserves the single import slot.
func (e *Engine) beginImport(source string) (context.Context, context.CancelFunc, error) {
	if e.st == nil {
		return nil, nil, ErrNotRunning
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.imp.status.Running {
		return nil, nil, ErrImportRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.imp = importState{
		status: ImportStatus{Running: true, Stage: "download", Source: source, Started: time.Now().UnixMilli()},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	return ctx, cancel, nil
}

func (e *Engine) updateImport(fn func(s *ImportStatus)) {
	e.mu.Lock()
	fn(&e.imp.status)
	e.mu.Unlock()
}

// cancelImport aborts a running import and waits for it to end.
func (e *Engine) cancelImport() {
	e.mu.Lock()
	cancel, done := e.imp.cancel, e.imp.done
	running := e.imp.status.Running
	e.mu.Unlock()
	if !running || cancel == nil {
		return
	}
	cancel()
	<-done
}

// CancelImport aborts a running aircraft database import; the database
// keeps its previous content unless the import had already reached the
// "store" stage.
func (e *Engine) CancelImport() {
	e.cancelImport()
}

// ImportAircraftDB starts a download of an aircraft database (refdata
// source) in the background. Progress is available from ImportStatus; the
// outcome is also announced as a notice. Records are first written to a
// staging file in the data directory and only loaded into the database
// (replacing the old content) after the download completed, so a failed
// download leaves the installed database untouched.
func (e *Engine) ImportAircraftDB(req ImportRequest) error {
	src := req.Source
	if src == "" {
		src = refdata.SourceTar1090
	}
	if _, ok := refdata.SourceByID(src); !ok && req.URL == "" {
		return fmt.Errorf("engine: unknown aircraft database source %q", src)
	}
	ctx, _, err := e.beginImport(src)
	if err != nil {
		return err
	}
	go func() {
		info := ACDBImportInfo{Source: src}
		_ = e.runImport(ctx, &info, func(sink refdata.Sink) error {
			res, err := refdata.Import(ctx, refdata.ImportOptions{
				Source: src, URL: req.URL,
				UserAgent: "AirView/" + e.opts.Version,
				Progress:  e.importProgress,
			}, sink)
			info.URL = res.URL
			info.Attribution = res.Source.Attribution
			info.Licence = res.Source.Licence
			if !res.LastModified.IsZero() {
				info.LastModified = res.LastModified.UnixMilli()
			}
			return err
		})
	}()
	return nil
}

// ImportAircraftDBFile imports a user-supplied database file (tar1090-db or
// OpenSky CSV, optionally gzip-compressed) from r. size is the expected
// number of bytes (<= 0 = unknown). It blocks until the import finished and
// returns the final status.
func (e *Engine) ImportAircraftDBFile(ctx context.Context, r io.Reader, size int64, name string) (ImportStatus, error) {
	if name == "" {
		name = "file"
	}
	ictx, cancel, err := e.beginImport(name)
	if err != nil {
		return e.ImportStatus(), err
	}
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	info := ACDBImportInfo{Source: name}
	err = e.runImport(ictx, &info, func(sink refdata.Sink) error {
		res, err := refdata.Parse(ictx, r, sink, refdata.ParseOptions{BytesTotal: size, Progress: e.importProgress})
		uploadedFormatInfo(&info, res.Format)
		return err
	})
	return e.ImportStatus(), err
}

func (e *Engine) importProgress(p refdata.Progress) {
	e.updateImport(func(s *ImportStatus) { s.Progress = &p })
}

// runImport is the common pipeline: fill the staging file through produce,
// then load it into the acdb table.
func (e *Engine) runImport(ctx context.Context, info *ACDBImportInfo, produce func(refdata.Sink) error) (err error) {
	e.mu.Lock()
	done := e.imp.done
	e.mu.Unlock()
	staging := filepath.Join(e.dir, "acdb-staging.tmp")

	defer func() {
		_ = os.Remove(staging)
		stage, msg := "done", ""
		switch {
		case err == nil:
		case ctx.Err() != nil:
			stage, msg = "cancelled", "cancelled"
		default:
			stage, msg = "error", err.Error()
		}
		e.updateImport(func(s *ImportStatus) {
			s.Running = false
			s.Stage = stage
			s.Error = msg
			s.Finished = time.Now().UnixMilli()
		})
		switch stage {
		case "done":
			e.releaseMemory("aircraft database import")
			e.log.Info("aircraft database imported", "source", info.Source, "records", info.Records)
			e.Notify("info", fmt.Sprintf("Aircraft database imported: %d aircraft.", info.Records))
		case "error":
			e.log.Warn("aircraft database import failed", "source", info.Source, "err", err)
			e.Notify("warn", "Aircraft database import failed: "+msg)
		}
		close(done)
	}()

	f, err := os.Create(staging)
	if err != nil {
		return fmt.Errorf("staging file: %w", err)
	}
	w := bufio.NewWriterSize(f, 256<<10)
	var staged int64
	var scratch []byte
	sink := refdata.SinkFunc(func(r refdata.ACRecord) error {
		scratch = appendStaged(scratch[:0], r)
		staged++
		_, err := w.Write(scratch)
		return err
	})
	err = produce(sink)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if staged == 0 {
		return refdata.ErrNoRecords
	}

	e.updateImport(func(s *ImportStatus) { s.Stage = "store" })
	in, err := os.Open(staging)
	if err != nil {
		return fmt.Errorf("staging file: %w", err)
	}
	defer in.Close()
	var readErr error
	n, err := e.st.ImportACDB(ctx, true, stagedSeq(bufio.NewReaderSize(in, 256<<10), &readErr, func(n int64) {
		if n%20000 == 0 {
			e.updateImport(func(s *ImportStatus) { s.Stored = n })
		}
	}))
	if err == nil {
		err = readErr
	}
	e.updateImport(func(s *ImportStatus) { s.Stored = int64(n) })
	if err != nil {
		return fmt.Errorf("loading into the database: %w", err)
	}

	info.Records = int64(n)
	info.T = time.Now().UnixMilli()
	if b, jerr := json.Marshal(info); jerr == nil {
		if kerr := e.st.KVSet(ctx, kvACDBImport, string(b)); kerr != nil {
			e.log.Warn("aircraft database import: saving summary", "err", kerr)
		}
	}
	e.tr.InvalidateRefdata()
	e.an.Invalidate()
	return nil
}

// Staging record: icao u32, flags u32, year u16, then reg / type / desc /
// operator, each as u16 length + bytes (little endian).
func appendStaged(dst []byte, r refdata.ACRecord) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, r.ICAO)
	dst = binary.LittleEndian.AppendUint32(dst, r.Flags)
	year := r.Year
	if year < 0 || year > 0xFFFF {
		year = 0
	}
	dst = binary.LittleEndian.AppendUint16(dst, uint16(year))
	for _, s := range [...]string{r.Reg, r.Type, r.Desc, r.Operator} {
		if len(s) > 0xFFFF {
			s = s[:0xFFFF]
		}
		dst = binary.LittleEndian.AppendUint16(dst, uint16(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

// stagedSeq streams the staging file as store rows. A read error other than
// a clean end of file is reported through *errp.
func stagedSeq(r *bufio.Reader, errp *error, progress func(n int64)) iter.Seq[store.ACDBEntry] {
	return func(yield func(store.ACDBEntry) bool) {
		var head [10]byte
		var lenb [2]byte
		buf := make([]byte, 0, 256)
		var n int64
		for {
			if _, err := io.ReadFull(r, head[:]); err != nil {
				if err != io.EOF {
					*errp = fmt.Errorf("staging file: %w", err)
				}
				return
			}
			ent := store.ACDBEntry{
				ICAO:  binary.LittleEndian.Uint32(head[0:]),
				Flags: int(binary.LittleEndian.Uint32(head[4:])),
				Year:  int(binary.LittleEndian.Uint16(head[8:])),
			}
			for i := 0; i < 4; i++ {
				if _, err := io.ReadFull(r, lenb[:]); err != nil {
					*errp = fmt.Errorf("staging file: %w", err)
					return
				}
				l := int(binary.LittleEndian.Uint16(lenb[:]))
				if cap(buf) < l {
					buf = make([]byte, 0, l)
				}
				buf = buf[:l]
				if _, err := io.ReadFull(r, buf); err != nil {
					*errp = fmt.Errorf("staging file: %w", err)
					return
				}
				s := string(buf)
				switch i {
				case 0:
					ent.Reg = s
				case 1:
					ent.Type = s
				case 2:
					ent.Desc = s
				case 3:
					ent.Operator = s
				}
			}
			n++
			progress(n)
			if !yield(ent) {
				return
			}
		}
	}
}

// uploadedFormatInfo fills the licence and attribution of an uploaded
// database file from its detected format: an uploaded tar1090-db file is
// under the same ODC-By licence as the one-click download and must keep its
// attribution (shown on the Database tab and in the detail panel's source
// note). The source name stays the file name; Format records what it was.
func uploadedFormatInfo(info *ACDBImportInfo, f refdata.Format) {
	var id string
	switch f {
	case refdata.FormatTar1090:
		id = refdata.SourceTar1090
	case refdata.FormatOpenSky:
		id = refdata.SourceOpenSky
	default:
		return
	}
	info.Format = string(f)
	if src, ok := refdata.SourceByID(id); ok {
		info.Attribution = src.Attribution
		info.Licence = src.Licence
	}
}
