package refdata

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"airview/internal/model"
)

// Optional aircraft database import.
//
// The embedded tables only know what can be derived from the address. A free
// downloadable aircraft database adds registration, type, description and
// operator for several hundred thousand airframes. Import downloads one on
// request and streams it record by record into a caller supplied Sink (in
// AirView: the store's acdb table); nothing is kept in memory.

// ACRecord flag bits (ACRecord.Flags). The tar1090-db database supplies all
// four; the OpenSky database supplies none.
const (
	ACMilitary    uint32 = 1 << 0 // military airframe
	ACInteresting uint32 = 1 << 1 // "interesting" airframe (government, special, historic)
	ACPIA         uint32 = 1 << 2 // FAA Privacy ICAO Address programme: temporary anonymous address
	ACLADD        uint32 = 1 << 3 // FAA Limiting Aircraft Data Displayed: owner asked not to be shown
)

// ACRecord is one airframe of an imported aircraft database.
type ACRecord struct {
	ICAO     uint32 // 24-bit address
	Reg      string // registration, e.g. "D-AIBD"
	Type     string // ICAO type designator, e.g. "A319"
	Desc     string // type description, e.g. "AIRBUS A-319"
	Operator string // owner / operator, when the source has one
	Flags    uint32 // AC* bits
	Year     int    // year built, 0 = unknown
}

// Military reports whether the database flags the airframe as military.
func (r ACRecord) Military() bool { return r.Flags&ACMilitary != 0 }

// Apply copies the database record into the live aircraft DTO. Database
// values replace what is there (in particular a registration derived from the
// address by Info.Apply); empty database fields leave the DTO untouched. When
// the record has a type designator but no description, the embedded type
// table supplies TypeDesc. The military flag is only ever set, never cleared.
func (r ACRecord) Apply(a *model.Aircraft) {
	if a == nil {
		return
	}
	if r.Reg != "" {
		a.Registration = r.Reg
	}
	if r.Type != "" {
		a.TypeCode = r.Type
	}
	if r.Desc != "" {
		a.TypeDesc = r.Desc
	} else if n := TypeName(r.Type); n != "" {
		a.TypeDesc = n
	}
	if r.Operator != "" {
		a.Operator = r.Operator
	}
	if r.Military() {
		a.Military = true
	}
}

// Sink receives the records of an import, in file order, from a single
// goroutine. Returning an error aborts the import.
//
// Records already delivered stay delivered when an import fails or is
// cancelled half way; a sink that must not end up with a partial database
// should write into a transaction or a staging table and swap on success.
type Sink interface {
	Put(ACRecord) error
}

// SinkFunc adapts a function to the Sink interface.
type SinkFunc func(ACRecord) error

// Put calls f(r).
func (f SinkFunc) Put(r ACRecord) error { return f(r) }

// Format identifies an aircraft database file format.
type Format string

const (
	// FormatAuto: detect from the first line (and detect gzip from the magic
	// bytes).
	FormatAuto Format = ""
	// FormatTar1090: wiedehopf/tar1090-db aircraft.csv - semicolon separated,
	// no header, no quoting, backslash escapes:
	//   ICAO;registration;type;flags;description;year;owner/operator;
	FormatTar1090 Format = "tar1090"
	// FormatOpenSky: OpenSky Network aircraft database CSV - comma separated
	// with a header line naming the columns; either the legacy layout
	// (double-quoted, lower-case column names) or the "complete" monthly
	// snapshot layout (single-quoted, camelCase column names).
	FormatOpenSky Format = "opensky"
)

// Source IDs accepted by ImportOptions.Source.
const (
	SourceTar1090 = "tar1090-db"
	SourceOpenSky = "opensky"
)

// Source describes a downloadable aircraft database, including what the UI
// has to show as attribution.
type Source struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`      // download URL (for OpenSky: the newest snapshot known at build time)
	Homepage    string `json:"homepage"` // where a human can read about the data
	Format      Format `json:"format"`
	Licence     string `json:"licence"`
	LicenceURL  string `json:"licenceUrl"`
	Attribution string `json:"attribution"` // text to show next to the data
	Notes       string `json:"notes"`
	// ApproxBytes is the approximate download size and ApproxRecords the
	// approximate number of airframes (as of late 2026), for the UI.
	ApproxBytes   int64 `json:"approxBytes"`
	ApproxRecords int   `json:"approxRecords"`

	// indexURL, when set, is an S3 style bucket listing that is consulted for
	// the newest snapshot (keys starting with indexPrefix); the snapshot is
	// downloaded from indexBase + key.
	indexURL, indexPrefix, indexBase string
}

var sources = []Source{
	{
		ID:         SourceTar1090,
		Name:       "tar1090-db (Mictronics aircraft database)",
		URL:        "https://github.com/wiedehopf/tar1090-db/raw/refs/heads/csv/aircraft.csv.gz",
		Homepage:   "https://github.com/wiedehopf/tar1090-db",
		Format:     FormatTar1090,
		Licence:    "Open Data Commons Attribution License (ODC-By) v1.0",
		LicenceURL: "https://opendatacommons.org/licenses/by/1-0/",
		Attribution: "Aircraft data: tar1090-db by wiedehopf, built from the Mictronics aircraft database " +
			"(www.mictronics.de/aircraft-database, ODC-By 1.0) with additions from the ADS-B Exchange basic aircraft database.",
		Notes: "Recommended. Updated about weekly; includes military / interesting / PIA / LADD flags. " +
			"The tar1090-db repository itself carries no licence file; the licence shown is that of the Mictronics database it is built from.",
		ApproxBytes:   9 << 20,
		ApproxRecords: 650000,
	},
	{
		ID:         SourceOpenSky,
		Name:       "OpenSky Network aircraft database",
		URL:        "https://s3.opensky-network.org/data-samples/metadata/aircraft-database-complete-2025-08.csv",
		Homepage:   "https://opensky-network.org/data/aircraft",
		Format:     FormatOpenSky,
		Licence:    "No licence: OpenSky states that the aircraft database is unlicensed, is not covered by its terms of use and is offered \"as is\"",
		LicenceURL: "https://opensky-network.org/data/aircraft",
		Attribution: "Aircraft data: The OpenSky Network aircraft database, https://opensky-network.org " +
			"(crowdsourced, unlicensed, provided as is).",
		Notes: "Large uncompressed download (over 100 MB) and, according to OpenSky, no longer kept up to date " +
			"(newest snapshot: August 2025). The newest snapshot is looked up in the bucket listing at import time. " +
			"No military / privacy flags.",
		ApproxBytes:   108 << 20,
		ApproxRecords: 600000,
		indexURL:      "https://s3.opensky-network.org/data-samples?list-type=2&prefix=metadata/aircraft-database-complete-",
		indexPrefix:   "metadata/aircraft-database-complete-",
		indexBase:     "https://s3.opensky-network.org/data-samples/",
	},
}

// Sources returns the aircraft databases Import knows how to download, the
// recommended one first. The slice is a copy.
func Sources() []Source {
	out := make([]Source, len(sources))
	copy(out, sources)
	return out
}

// SourceByID returns the source with the given ID.
func SourceByID(id string) (Source, bool) {
	for i := range sources {
		if sources[i].ID == id {
			return sources[i], true
		}
	}
	return Source{}, false
}

// Progress is a snapshot of a running import or parse.
type Progress struct {
	Source string `json:"source"` // source ID ("" for a bare Parse)
	URL    string `json:"url"`    // URL being downloaded ("" for a bare Parse)
	// Stage: "connect" (before the first byte), "download" (records are
	// flowing) or "done" (final call of a successful run).
	Stage string `json:"stage"`
	// BytesRead counts bytes consumed from the input (compressed bytes for a
	// gzip file); BytesTotal is the expected total, -1 when unknown.
	BytesRead  int64 `json:"bytesRead"`
	BytesTotal int64 `json:"bytesTotal"`
	Records    int64 `json:"records"` // records delivered to the sink
	Skipped    int64 `json:"skipped"` // malformed lines ignored
	Empty      int64 `json:"empty"`   // well-formed records without any information, not delivered
}

// Fraction returns the completed share 0..1, or -1 when the total is unknown.
func (p Progress) Fraction() float64 {
	if p.Stage == "done" {
		return 1
	}
	if p.BytesTotal <= 0 {
		return -1
	}
	f := float64(p.BytesRead) / float64(p.BytesTotal)
	if f > 1 {
		f = 1
	}
	return f
}

// ProgressFunc receives progress snapshots. It is called from the importing
// goroutine and must not block for long.
type ProgressFunc func(Progress)

// ParseOptions tunes Parse.
type ParseOptions struct {
	Format Format // FormatAuto = detect
	// Progress, when set, is called about every ProgressInterval (default
	// 250 ms) while records flow.
	Progress         ProgressFunc
	ProgressInterval time.Duration
	// BytesTotal is reported back in Progress.BytesTotal (0 or negative =
	// unknown).
	BytesTotal int64
	// MaxBytes caps the decompressed size that is read (protection against
	// decompression bombs); 0 = 2 GiB.
	MaxBytes int64

	source, url string // filled in by Import for the progress reports
}

// ParseResult summarises a completed (or aborted) parse.
type ParseResult struct {
	Format    Format `json:"format"`    // the format that was parsed
	Gzip      bool   `json:"gzip"`      // input was gzip compressed
	Records   int64  `json:"records"`   // records delivered to the sink
	Skipped   int64  `json:"skipped"`   // malformed lines ignored (bad address, too few fields, overlong)
	Empty     int64  `json:"empty"`     // well-formed records without any information, not delivered
	BytesRead int64  `json:"bytesRead"` // bytes consumed from the input
}

// Errors returned by Parse and Import.
var (
	// ErrUnknownFormat: the input is neither a tar1090-db nor an OpenSky
	// aircraft database (for example an HTML error page).
	ErrUnknownFormat = errors.New("refdata: unrecognised aircraft database format")
	// ErrTooLarge: the decompressed input exceeded ParseOptions.MaxBytes.
	ErrTooLarge = errors.New("refdata: aircraft database exceeds the size limit")
	// ErrNoRecords: the input was read to the end without a single usable
	// record.
	ErrNoRecords = errors.New("refdata: aircraft database contains no usable records")
)

const (
	defaultMaxBytes         = 2 << 30
	defaultProgressInterval = 250 * time.Millisecond
	maxLineBytes            = 64 << 10 // longer lines are skipped
	maxFieldBytes           = 160      // longer text fields are truncated
	ctxCheckEvery           = 256      // lines between context / progress checks
)

// Parse reads an aircraft database from r and delivers every usable record
// to sink. gzip compression is detected automatically, as is the format
// unless opts.Format names one. Malformed lines are skipped and counted
// (ParseResult.Skipped), as are records without any information
// (ParseResult.Empty); neither is treated as an error. Parse stops with ctx.Err() soon after ctx is cancelled
// and with the sink's error (wrapped) when Put fails.
//
// The returned ParseResult is valid (counts up to the point of failure) even
// when err != nil.
func Parse(ctx context.Context, r io.Reader, sink Sink, opts ParseOptions) (ParseResult, error) {
	if sink == nil {
		return ParseResult{}, errors.New("refdata: nil sink")
	}
	p := &parser{ctx: ctx, sink: sink, opts: opts}
	if p.opts.ProgressInterval <= 0 {
		p.opts.ProgressInterval = defaultProgressInterval
	}
	if p.opts.MaxBytes <= 0 {
		p.opts.MaxBytes = defaultMaxBytes
	}
	if p.opts.BytesTotal <= 0 {
		p.opts.BytesTotal = -1
	}
	err := p.run(r)
	p.res.BytesRead = p.in.n
	if err == nil && p.res.Records == 0 {
		err = ErrNoRecords
	}
	if err == nil {
		p.report("done")
	}
	return p.res, err
}

// countingReader counts the bytes read from the underlying reader.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

type parser struct {
	ctx  context.Context
	sink Sink
	opts ParseOptions
	res  ParseResult

	in       countingReader
	br       *bufio.Reader
	lines    int64
	lastProg time.Time
	fields   []string // reused field slice
	scratch  []byte   // reused unescape buffer
}

func (p *parser) run(r io.Reader) error {
	p.in.r = r
	raw := bufio.NewReaderSize(&p.in, 64<<10)
	var body io.Reader = raw
	if magic, _ := raw.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(raw)
		if err != nil {
			return fmt.Errorf("refdata: gzip: %w", err)
		}
		defer zr.Close()
		body = zr
		p.res.Gzip = true
	}
	// One byte more than the cap, so that exceeding it can be told apart
	// from ending exactly on it.
	lim := &io.LimitedReader{R: body, N: p.opts.MaxBytes + 1}
	p.br = bufio.NewReaderSize(lim, maxLineBytes)
	p.lastProg = time.Now()

	// First non-empty line decides the format.
	var first []byte
	for {
		line, err := p.readLine()
		if err != nil {
			if err == io.EOF {
				return ErrNoRecords
			}
			return err
		}
		line = bytes.TrimPrefix(line, []byte("\xEF\xBB\xBF"))
		if len(bytes.TrimSpace(line)) > 0 {
			first = line
			break
		}
	}
	format := p.opts.Format
	if format == FormatAuto {
		format = detectFormat(first)
	}
	var err error
	switch format {
	case FormatTar1090:
		p.res.Format = FormatTar1090
		err = p.runTar1090(first)
	case FormatOpenSky:
		p.res.Format = FormatOpenSky
		err = p.runOpenSky(first)
	default:
		return ErrUnknownFormat
	}
	if err == nil && lim.N <= 0 {
		return ErrTooLarge
	}
	return err
}

// detectFormat classifies the first line of a database file.
func detectFormat(line []byte) Format {
	line = bytes.TrimSpace(line)
	// OpenSky: a header line that names the icao24 column.
	if len(line) < 4096 && bytes.Contains(bytes.ToLower(line), []byte("icao24")) {
		return FormatOpenSky
	}
	// tar1090-db: six hex digits, then a semicolon.
	if len(line) >= 7 && line[6] == ';' {
		if _, ok := ParseICAO(string(line[:6])); ok {
			return FormatTar1090
		}
	}
	return ""
}

// readLine returns the next line without its line terminator. Lines longer
// than maxLineBytes are consumed, counted as skipped and not returned. The
// returned slice is only valid until the next call.
func (p *parser) readLine() ([]byte, error) {
	for {
		line, err := p.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Overlong line: discard the rest of it.
			for err == bufio.ErrBufferFull {
				_, err = p.br.ReadSlice('\n')
			}
			p.res.Skipped++
			if err != nil && err != io.EOF {
				return nil, p.readErr(err)
			}
			if err == io.EOF {
				return nil, io.EOF
			}
			continue
		}
		if err != nil && err != io.EOF {
			return nil, p.readErr(err)
		}
		if err == io.EOF && len(line) == 0 {
			return nil, io.EOF
		}
		p.lines++
		if p.lines%ctxCheckEvery == 0 {
			if cerr := p.ctx.Err(); cerr != nil {
				return nil, cerr
			}
			if p.opts.Progress != nil && time.Since(p.lastProg) >= p.opts.ProgressInterval {
				p.report("download")
			}
		}
		// Strip "\n" / "\r\n".
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
		}
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		return line, nil
	}
}

// readErr prefers the context's error over the read error it caused.
func (p *parser) readErr(err error) error {
	if cerr := p.ctx.Err(); cerr != nil {
		return cerr
	}
	return fmt.Errorf("refdata: read aircraft database: %w", err)
}

func (p *parser) report(stage string) {
	p.lastProg = time.Now()
	if p.opts.Progress == nil {
		return
	}
	p.opts.Progress(Progress{
		Source: p.opts.source, URL: p.opts.url, Stage: stage,
		BytesRead: p.in.n, BytesTotal: p.opts.BytesTotal,
		Records: p.res.Records, Skipped: p.res.Skipped, Empty: p.res.Empty,
	})
}

// put delivers one record. Records with an impossible address count as
// skipped; records that carry no information (the OpenSky file has tens of
// thousands of address-only rows) are counted as empty and not delivered.
func (p *parser) put(rec ACRecord) error {
	if rec.ICAO == 0 || rec.ICAO > 0xFFFFFF {
		p.res.Skipped++
		return nil
	}
	if rec.Reg == "" && rec.Type == "" && rec.Desc == "" && rec.Operator == "" && rec.Flags == 0 {
		p.res.Empty++
		return nil
	}
	if err := p.sink.Put(rec); err != nil {
		return fmt.Errorf("refdata: aircraft database sink: %w", err)
	}
	p.res.Records++
	return nil
}

// ---- tar1090-db ------------------------------------------------------------

func (p *parser) runTar1090(first []byte) error {
	line := first
	for {
		if rec, ok := p.parseTar1090(line); ok {
			if err := p.put(rec); err != nil {
				return err
			}
		} else if len(bytes.TrimSpace(line)) > 0 {
			p.res.Skipped++
		}
		var err error
		if line, err = p.readLine(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// parseTar1090 parses "ICAO;reg;type;flags;desc;year;ownop;". The file is
// written without quoting; a literal semicolon or backslash inside a field is
// escaped with a backslash.
func (p *parser) parseTar1090(line []byte) (ACRecord, bool) {
	var f [7][]byte
	n := 0
	start := 0
	escaped := false
	for i := 0; i < len(line) && n < len(f); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			escaped = true
			i++
		case line[i] == ';':
			f[n] = line[start:i]
			n++
			start = i + 1
		}
	}
	if n < len(f) && start <= len(line) {
		f[n] = line[start:]
		n++
	}
	if n < 4 {
		return ACRecord{}, false
	}
	icao, ok := ParseICAO(string(bytes.TrimSpace(f[0])))
	if !ok || len(bytes.TrimSpace(f[0])) != 6 {
		return ACRecord{}, false
	}
	text := func(b []byte) string {
		if escaped && bytes.IndexByte(b, '\\') >= 0 {
			p.scratch = p.scratch[:0]
			for i := 0; i < len(b); i++ {
				if b[i] == '\\' && i+1 < len(b) {
					i++
				}
				p.scratch = append(p.scratch, b[i])
			}
			b = p.scratch
		}
		return cleanField(b)
	}
	rec := ACRecord{
		ICAO:     icao,
		Reg:      strings.ToUpper(text(f[1])),
		Type:     strings.ToUpper(text(f[2])),
		Desc:     text(f[4]),
		Operator: text(f[6]),
		Year:     parseYear(f[5]),
	}
	// Flags: a string of '0' / '1' characters, character i = bit i
	// (military, interesting, PIA, LADD). Usually 2 or 4 characters.
	for i, c := range bytes.TrimSpace(f[3]) {
		if i >= 32 {
			break
		}
		if c == '1' {
			rec.Flags |= 1 << uint(i)
		}
	}
	return rec, true
}

// ---- OpenSky ---------------------------------------------------------------

// openSkyColumns are the column indices of the fields we use (-1 = absent).
type openSkyColumns struct {
	icao, reg, typ, model, mfr, operator, owner, built int
	n                                                  int
}

func (p *parser) runOpenSky(header []byte) error {
	quote := byte('"')
	if t := bytes.TrimSpace(header); len(t) > 0 && t[0] == '\'' {
		quote = '\''
	}
	names, _ := p.csvRecord(header, quote)
	cols := openSkyColumns{icao: -1, reg: -1, typ: -1, model: -1, mfr: -1, operator: -1, owner: -1, built: -1, n: len(names)}
	for i, name := range names {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "icao24":
			cols.icao = i
		case "registration":
			cols.reg = i
		case "typecode":
			cols.typ = i
		case "model":
			cols.model = i
		case "manufacturername":
			cols.mfr = i
		case "operator":
			cols.operator = i
		case "owner":
			cols.owner = i
		case "built":
			cols.built = i
		}
	}
	if cols.icao < 0 || (cols.reg < 0 && cols.typ < 0) {
		return ErrUnknownFormat
	}
	for {
		line, err := p.readLine()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		f, complete := p.csvRecord(line, quote)
		// A quoted field may contain line breaks: join continuation lines
		// (bounded, so that one stray quote cannot swallow the file).
		if !complete {
			joined := append([]byte(nil), line...)
			for extra := 0; !complete && extra < 8 && len(joined) < maxLineBytes; extra++ {
				next, err := p.readLine()
				if err != nil {
					if err == io.EOF {
						break
					}
					return err
				}
				joined = append(append(joined, ' '), next...)
				f, complete = p.csvRecord(joined, quote)
			}
		}
		if !complete || len(f) <= cols.icao || len(f) < cols.n-2 {
			p.res.Skipped++
			continue
		}
		get := func(i int) string {
			if i < 0 || i >= len(f) {
				return ""
			}
			return cleanField([]byte(f[i]))
		}
		icaoText := strings.TrimSpace(f[cols.icao])
		icao, ok := ParseICAO(icaoText)
		if !ok || len(icaoText) != 6 {
			p.res.Skipped++
			continue
		}
		rec := ACRecord{
			ICAO:     icao,
			Reg:      strings.ToUpper(get(cols.reg)),
			Type:     strings.ToUpper(get(cols.typ)),
			Operator: get(cols.operator),
			Year:     parseYear([]byte(get(cols.built))),
		}
		if rec.Operator == "" {
			rec.Operator = get(cols.owner)
		}
		rec.Desc = joinMakeModel(get(cols.mfr), get(cols.model))
		if err := p.put(rec); err != nil {
			return err
		}
	}
}

// csvRecord splits one CSV record on commas. A field that starts with the
// quote character is quoted: it ends at a quote that is followed by a comma
// or the end of the record, and a doubled quote inside it is a literal quote.
// Any other quote inside a quoted field is taken literally (lenient). complete
// is false when the record ends inside a quoted field.
//
// The returned strings are freshly allocated only for fields that needed
// unescaping; the slice itself is reused between calls.
func (p *parser) csvRecord(line []byte, quote byte) (fields []string, complete bool) {
	p.fields = p.fields[:0]
	i := 0
	for {
		if i < len(line) && line[i] == quote {
			// Quoted field.
			i++
			p.scratch = p.scratch[:0]
			closed := false
			for i < len(line) {
				c := line[i]
				if c != quote {
					p.scratch = append(p.scratch, c)
					i++
					continue
				}
				switch {
				case i+1 < len(line) && line[i+1] == quote:
					p.scratch = append(p.scratch, quote)
					i += 2
				case i+1 == len(line) || line[i+1] == ',':
					closed = true
					i++
				default:
					p.scratch = append(p.scratch, quote)
					i++
				}
				if closed {
					break
				}
			}
			if !closed {
				return p.fields, false
			}
			p.fields = append(p.fields, string(p.scratch))
		} else {
			j := bytes.IndexByte(line[i:], ',')
			if j < 0 {
				j = len(line) - i
			}
			p.fields = append(p.fields, string(line[i:i+j]))
			i += j
		}
		if i >= len(line) {
			return p.fields, true
		}
		i++ // the comma
		if i == len(line) {
			p.fields = append(p.fields, "") // trailing empty field
			return p.fields, true
		}
	}
}

// joinMakeModel builds a description from manufacturer and model without
// repeating the manufacturer when the model already starts with it.
func joinMakeModel(mfr, model string) string {
	switch {
	case model == "":
		return mfr
	case mfr == "":
		return model
	case len(model) >= len(mfr) && strings.EqualFold(model[:len(mfr)], mfr):
		return model
	}
	return mfr + " " + model
}

// cleanField trims a text field, replaces invalid UTF-8 and control
// characters, and bounds its length.
func cleanField(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	clean := utf8.Valid(b)
	if clean {
		for _, c := range b {
			if c < 0x20 || c == 0x7f {
				clean = false
				break
			}
		}
	}
	var s string
	if clean {
		s = string(b)
	} else {
		s = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r // invalid bytes arrive as utf8.RuneError and are kept as U+FFFD
		}, string(b))
		s = strings.TrimSpace(s)
	}
	if len(s) > maxFieldBytes {
		n := maxFieldBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = strings.TrimSpace(s[:n])
	}
	return s
}

// parseYear accepts "1998" or a date starting with the year ("1998-05-01")
// and returns 0 for anything implausible.
func parseYear(b []byte) int {
	b = bytes.TrimSpace(b)
	if len(b) < 4 {
		return 0
	}
	y := 0
	for _, c := range b[:4] {
		if c < '0' || c > '9' {
			return 0
		}
		y = y*10 + int(c-'0')
	}
	if len(b) > 4 && b[4] >= '0' && b[4] <= '9' {
		return 0 // a longer number, not a year
	}
	if y < 1900 || y > 2100 {
		return 0
	}
	return y
}

// ---- download --------------------------------------------------------------

// ImportOptions configures Import.
type ImportOptions struct {
	// Source is the ID of one of Sources(); "" = SourceTar1090.
	Source string
	// URL overrides the source's download URL (mirror, local file server,
	// test server). When set, no snapshot lookup takes place.
	URL string
	// Format overrides the source's format; FormatAuto with a custom URL
	// detects it.
	Format Format
	// Client is the HTTP client to use; nil = a client with sensible
	// connect / header timeouts and no overall deadline.
	Client *http.Client
	// Progress receives snapshots about every ProgressInterval (default
	// 250 ms) plus a final one with Stage "done".
	Progress         ProgressFunc
	ProgressInterval time.Duration
	// IdleTimeout aborts the download when no data arrives for this long;
	// 0 = 60 s, negative = no idle timeout.
	IdleTimeout time.Duration
	// MaxBytes caps the decompressed size; 0 = 2 GiB.
	MaxBytes int64
	// UserAgent overrides the default "AirView/<version>".
	UserAgent string
}

// ImportResult summarises a completed import.
type ImportResult struct {
	Source       Source        `json:"source"`       // what was imported, with licence and attribution
	URL          string        `json:"url"`          // the URL actually downloaded
	Format       Format        `json:"format"`       //
	Records      int64         `json:"records"`      // records delivered to the sink
	Skipped      int64         `json:"skipped"`      // malformed lines ignored
	Empty        int64         `json:"empty"`        // records without any information, not delivered
	Bytes        int64         `json:"bytes"`        // bytes downloaded
	LastModified time.Time     `json:"lastModified"` // server's Last-Modified, zero when absent
	ETag         string        `json:"etag"`         //
	Elapsed      time.Duration `json:"elapsedNs"`    //
}

// ErrImportIdle is the cause of an import aborted by ImportOptions.IdleTimeout.
var ErrImportIdle = errors.New("refdata: aircraft database download stalled")

// Import downloads an aircraft database and streams it into sink. It blocks
// until the import is complete, fails, or ctx is cancelled (then it returns
// ctx.Err() promptly). Run it in its own goroutine and use opts.Progress to
// follow it.
//
// The returned ImportResult names the source with its licence and attribution
// text; the counts in it are valid even when err != nil.
func Import(ctx context.Context, opts ImportOptions, sink Sink) (ImportResult, error) {
	started := time.Now()
	var res ImportResult
	if sink == nil {
		return res, errors.New("refdata: nil sink")
	}
	id := opts.Source
	if id == "" {
		id = SourceTar1090
	}
	src, ok := SourceByID(id)
	if !ok {
		return res, fmt.Errorf("refdata: unknown aircraft database source %q", id)
	}
	res.Source = src
	client := opts.Client
	if client == nil {
		client = defaultImportClient()
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = model.AppName + "/" + model.Version
	}

	// The whole import runs under a cancellable context so that the idle
	// watchdog can abort a stalled download.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	url := opts.URL
	format := opts.Format
	if url == "" {
		url = src.URL
		if format == FormatAuto {
			format = src.Format
		}
		if src.indexURL != "" {
			if latest, err := latestSnapshot(ctx, client, ua, src); err == nil {
				url = latest
			} else if ctx.Err() != nil {
				return res, context.Cause(ctx)
			} else {
				slog.Warn("refdata: snapshot lookup failed, using built-in URL", "source", src.ID, "err", err)
			}
		}
	}
	res.URL = url
	res.Format = format

	if opts.Progress != nil {
		opts.Progress(Progress{Source: src.ID, URL: url, Stage: "connect", BytesTotal: -1})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return res, fmt.Errorf("refdata: %w", err)
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return res, context.Cause(ctx)
		}
		return res, fmt.Errorf("refdata: download %s: %w", src.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("refdata: download %s: HTTP %s", src.ID, resp.Status)
	}
	res.ETag = resp.Header.Get("ETag")
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		res.LastModified = t
	}

	var body io.Reader = resp.Body
	idle := opts.IdleTimeout
	if idle == 0 {
		idle = 60 * time.Second
	}
	if idle > 0 {
		w := &idleReader{r: resp.Body, d: idle}
		w.t = time.AfterFunc(idle, func() { cancel(ErrImportIdle) })
		w.t.Stop()
		defer w.t.Stop()
		body = w
	}

	pres, err := Parse(ctx, body, sink, ParseOptions{
		Format:           format,
		Progress:         opts.Progress,
		ProgressInterval: opts.ProgressInterval,
		BytesTotal:       resp.ContentLength,
		MaxBytes:         opts.MaxBytes,
		source:           src.ID,
		url:              url,
	})
	res.Format = pres.Format
	res.Records, res.Skipped, res.Empty, res.Bytes = pres.Records, pres.Skipped, pres.Empty, pres.BytesRead
	res.Elapsed = time.Since(started)
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled by the caller (ctx.Err) or by the idle watchdog.
			return res, context.Cause(ctx)
		}
		return res, err
	}
	slog.Info("refdata: aircraft database imported", "source", src.ID, "records", res.Records,
		"skipped", res.Skipped, "empty", res.Empty, "bytes", res.Bytes, "elapsed", res.Elapsed.Round(time.Millisecond))
	return res, nil
}

// idleReader runs a watchdog timer for the duration of every Read, so that
// only time spent waiting for the network counts as idle - not time the sink
// takes to store records between reads.
type idleReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (w *idleReader) Read(b []byte) (int, error) {
	w.t.Reset(w.d)
	n, err := w.r.Read(b)
	w.t.Stop()
	return n, err
}

// defaultImportClient returns an HTTP client without an overall deadline (the
// download may legitimately take minutes on a slow link) but with bounded
// connection set-up and header wait.
func defaultImportClient() *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	tr = tr.Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.TLSHandshakeTimeout = 15 * time.Second
	tr.MaxIdleConns = 2
	tr.IdleConnTimeout = 10 * time.Second
	return &http.Client{Transport: tr}
}

// latestSnapshot asks the source's bucket listing for the newest snapshot and
// returns its URL. Snapshot names end in a sortable date
// (aircraft-database-complete-2025-08.csv), so the greatest key wins.
func latestSnapshot(ctx context.Context, client *http.Client, ua string, src Source) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.indexURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	key, err := newestKey(io.LimitReader(resp.Body, 4<<20), src.indexPrefix)
	if err != nil {
		return "", err
	}
	return src.indexBase + key, nil
}

// newestKey extracts the lexically greatest <Key> starting with prefix and
// ending in ".csv" from an S3 ListBucketResult document.
func newestKey(r io.Reader, prefix string) (string, error) {
	var list struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(r).Decode(&list); err != nil {
		return "", fmt.Errorf("bucket listing: %w", err)
	}
	best := ""
	for _, c := range list.Contents {
		k := c.Key
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, ".csv") {
			continue
		}
		// Only plain names: the key becomes part of a URL.
		plain := true
		for i := 0; i < len(k); i++ {
			ch := k[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '/') {
				plain = false
				break
			}
		}
		if plain && !strings.Contains(k, "..") && k > best {
			best = k
		}
	}
	if best == "" {
		return "", errors.New("bucket listing: no snapshot found")
	}
	return best, nil
}
