package refdata

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memSink collects records.
type memSink struct {
	recs []ACRecord
}

func (s *memSink) Put(r ACRecord) error {
	s.recs = append(s.recs, r)
	return nil
}

func (s *memSink) byICAO(t *testing.T) map[uint32]ACRecord {
	t.Helper()
	m := make(map[uint32]ACRecord, len(s.recs))
	for _, r := range s.recs {
		if _, dup := m[r.ICAO]; dup {
			t.Errorf("record %06X delivered twice", r.ICAO)
		}
		m[r.ICAO] = r
	}
	return m
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func checkRecords(t *testing.T, got map[uint32]ACRecord, want []ACRecord) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%d records delivered, want %d", len(got), len(want))
	}
	for _, w := range want {
		g, ok := got[w.ICAO]
		if !ok {
			t.Errorf("record %06X missing", w.ICAO)
			continue
		}
		if g != w {
			t.Errorf("record %06X\n got %+v\nwant %+v", w.ICAO, g, w)
		}
	}
}

// The tar1090-db fixture holds real lines of aircraft.csv plus a few broken
// ones.
var wantTar1090 = []ACRecord{
	{ICAO: 0x000001, Flags: ACMilitary, Operator: "Miscode - VARIOUS"},
	{ICAO: 0x3C6444, Reg: "D-AIBD", Type: "A319", Desc: "AIRBUS A-319"},
	{ICAO: 0x3C60B2, Reg: "D-AHER", Type: "F900", Desc: "DASSAULT Falcon 900", Flags: ACLADD},
	{ICAO: 0x406A9F, Reg: "G-ZBJH", Type: "B788", Desc: "BOEING 787-8 Dreamliner"},
	{ICAO: 0x43C6F3, Reg: "ZZ330", Type: "A332", Desc: "Airbus Voyager KC2 (A330-243MRTT)", Flags: ACMilitary},
	{ICAO: 0x48479C, Reg: "PH-730", Type: "GLID", Desc: `Grob G102 "CLUB ASTIR III B"`},
	{ICAO: 0x05E0BB, Reg: "5T-ONE", Type: "B737", Desc: "BOEING 737-700", Flags: ACInteresting | ACLADD},
	{ICAO: 0x0703FA, Reg: "6V-SEN", Type: "A20N", Desc: "AIRBUS A-320neo", Flags: ACMilitary | ACInteresting,
		Operator: "Government of Senegal"},
	{ICAO: 0x7C4920, Reg: "VH-OQA", Type: "A388", Desc: "AIRBUS A-380-800", Year: 2008, Operator: "QANTAS AIRWAYS LIMITED"},
	{ICAO: 0xA00001, Reg: "N1", Type: "C680", Desc: "CESSNA 680 Citation Sovereign", Flags: ACLADD,
		Operator: "FEDERAL AVIATION ADMINISTRATION"},
	{ICAO: 0xA00599, Flags: ACPIA},
	{ICAO: 0xAE01CE, Reg: "62-4134", Type: "R135", Desc: "Boeing RC-135W Rivet Joint", Flags: ACMilitary},
	{ICAO: 0xC0172E, Reg: "C-FIUF", Type: "B77L", Desc: "BOEING 777-200LR", Year: 2007, Operator: "Air Canada"},
	// Lower-case input, escaped separator and backslash, date instead of year.
	{ICAO: 0xABC123, Reg: "X-TEST", Type: "A320", Desc: `Semi;colon and back\slash`, Year: 1999, Operator: "Owner, Inc."},
	// Line without the trailing fields.
	{ICAO: 0x4CA242, Reg: "EI-DCL", Type: "B738", Desc: "BOEING 737-800"},
}

func TestParseTar1090(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		data := fixture(t, "tar1090.csv")
		if compressed {
			data = gz(t, data)
		}
		var sink memSink
		res, err := Parse(context.Background(), bytes.NewReader(data), &sink, ParseOptions{})
		if err != nil {
			t.Fatalf("gzip=%v: %v", compressed, err)
		}
		if res.Format != FormatTar1090 || res.Gzip != compressed {
			t.Errorf("gzip=%v: format %q gzip %v", compressed, res.Format, res.Gzip)
		}
		// Skipped: the text line, the non-hex address and the 5-digit
		// address. Empty: the record without any information.
		if res.Records != int64(len(wantTar1090)) || res.Skipped != 3 || res.Empty != 1 {
			t.Errorf("gzip=%v: %d records, %d skipped, %d empty; want %d, 3, 1",
				compressed, res.Records, res.Skipped, res.Empty, len(wantTar1090))
		}
		if res.BytesRead != int64(len(data)) {
			t.Errorf("gzip=%v: BytesRead %d, want %d", compressed, res.BytesRead, len(data))
		}
		checkRecords(t, sink.byICAO(t), wantTar1090)
		// File order is preserved.
		if len(sink.recs) > 1 && (sink.recs[0].ICAO != 0x000001 || sink.recs[1].ICAO != 0x3C6444) {
			t.Errorf("records out of order: %06X, %06X", sink.recs[0].ICAO, sink.recs[1].ICAO)
		}
	}
}

func TestParseOpenSkyComplete(t *testing.T) {
	var sink memSink
	res, err := Parse(context.Background(), bytes.NewReader(fixture(t, "opensky_complete.csv")), &sink, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != FormatOpenSky || res.Gzip {
		t.Errorf("format %q gzip %v", res.Format, res.Gzip)
	}
	want := []ACRecord{
		{ICAO: 0x000001, Reg: "SP-FGR", Type: "AN2", Desc: "Antonov 2", Operator: "Private"},
		{ICAO: 0xA5CFE8, Reg: "N4739X", Type: "C150", Desc: "Cessna 150G", Operator: "Kimberly Jack L", Year: 1966},
		{ICAO: 0xA5CFF6, Reg: "N474A", Type: "B407", Desc: "Bell 407", Operator: "Tvpx Aircraft Solutions Inc Trustee", Year: 2002},
		{ICAO: 0xA5CFFB, Reg: "N474AE", Desc: "Boeing 737-448", Operator: "Frontera Flight Holdings Inc"},
		// Record spanning two lines, doubled quotes, lower-case values.
		{ICAO: 0x3C6444, Reg: "D-AIBD", Type: "A319", Desc: "Airbus A319-112", Operator: "Lufthansa, 'Deutsche' AG", Year: 2010},
	}
	// Skipped: address 000000, the short row with a bad address and the row
	// cut off after four fields. Empty: the row with only a category.
	if res.Records != int64(len(want)) || res.Skipped != 3 || res.Empty != 1 {
		t.Errorf("%d records, %d skipped, %d empty; want %d, 3, 1", res.Records, res.Skipped, res.Empty, len(want))
	}
	checkRecords(t, sink.byICAO(t), want)
}

func TestParseOpenSkyLegacy(t *testing.T) {
	data := fixture(t, "opensky_legacy.csv")
	if !bytes.HasPrefix(data, []byte("\xEF\xBB\xBF")) {
		t.Fatal("fixture lost its byte order mark")
	}
	var sink memSink
	res, err := Parse(context.Background(), bytes.NewReader(data), &sink, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != FormatOpenSky {
		t.Errorf("format %q", res.Format)
	}
	want := []ACRecord{
		{ICAO: 0xAA3487, Reg: "N757F", Type: "BE36", Desc: "Raytheon Aircraft Company A36", Operator: "Vintage Aircraft Llc"},
		{ICAO: 0xA4FA61, Reg: "N42MH", Type: "PA31", Desc: "Piper PA-31-350", Operator: "Tvpx Aircraft Solutions Inc Trustee", Year: 1977},
		// Operator preferred over owner; manufacturer not repeated.
		{ICAO: 0x4B1801, Reg: "HB-JCJ", Type: "BCS3", Desc: "Airbus A220-300", Operator: "Swiss", Year: 2017},
	}
	if res.Records != int64(len(want)) || res.Skipped != 1 || res.Empty != 0 { // the all-empty second line
		t.Errorf("%d records, %d skipped, %d empty; want %d, 1, 0", res.Records, res.Skipped, res.Empty, len(want))
	}
	checkRecords(t, sink.byICAO(t), want)
}

func TestDetectFormat(t *testing.T) {
	for line, want := range map[string]Format{
		"3C6444;D-AIBD;A319;00;AIRBUS A-319;;;":      FormatTar1090,
		"000001;;;10;;;Miscode - VARIOUS;":           FormatTar1090,
		"abc123;;;00;;;;":                            FormatTar1090,
		`"icao24","registration","manufacturericao"`: FormatOpenSky,
		`'icao24','timestamp','acars'`:               FormatOpenSky,
		"icao24,registration,typecode":               FormatOpenSky,
		"  ICAO24 , Registration":                    FormatOpenSky,
		"<!DOCTYPE html>":                            FormatAuto,
		"<html><body>404 Not Found</body></html>":    FormatAuto,
		"3C6444,D-AIBD,A319":                         FormatAuto,
		"3C644;D-AIBD":                               FormatAuto,
		"XYZ123;D-AIBD;A319;00":                      FormatAuto,
		"":                                           FormatAuto,
		`{"icao":"3C6444"}`:                          FormatAuto,
	} {
		if got := detectFormat([]byte(line)); got != want {
			t.Errorf("detectFormat(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		input string
		opts  ParseOptions
		want  error
	}{
		{"html error page", "<!DOCTYPE html>\n<html><body>Not found</body></html>\n", ParseOptions{}, ErrUnknownFormat},
		{"empty", "", ParseOptions{}, ErrNoRecords},
		{"blank lines", "\n\r\n   \n", ParseOptions{}, ErrNoRecords},
		{"header only", "'icao24','registration','typecode'\n", ParseOptions{}, ErrNoRecords},
		{"header without the columns we need", "icao24,foo,bar\nabc123,1,2\n", ParseOptions{}, ErrUnknownFormat},
		{"only empty records", "ABCDEF;;;00;;;;\nABCDF0;;;00;;;;\n", ParseOptions{}, ErrNoRecords},
		{"forced format on wrong data", "3C6444;D-AIBD;A319;00;;;;\n", ParseOptions{Format: FormatOpenSky}, ErrUnknownFormat},
		{"unknown format name", "3C6444;D-AIBD;A319;00;;;;\n", ParseOptions{Format: "sqlite"}, ErrUnknownFormat},
	}
	for _, tc := range tests {
		var sink memSink
		res, err := Parse(ctx, strings.NewReader(tc.input), &sink, tc.opts)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if res.Records != 0 || len(sink.recs) != 0 {
			t.Errorf("%s: %d records delivered", tc.name, len(sink.recs))
		}
	}

	if _, err := Parse(ctx, strings.NewReader("3C6444;D-AIBD;A319;00;;;;\n"), nil, ParseOptions{}); err == nil {
		t.Error("nil sink accepted")
	}

	// A forced format still parses.
	var sink memSink
	if _, err := Parse(ctx, strings.NewReader("3C6444;D-AIBD;A319;00;;;;\n"), &sink, ParseOptions{Format: FormatTar1090}); err != nil || len(sink.recs) != 1 {
		t.Errorf("forced tar1090 format: %v, %d records", err, len(sink.recs))
	}

	// A truncated gzip stream is an error, not a silent short import.
	full := gz(t, bytes.Repeat([]byte("3C6444;D-AIBD;A319;00;AIRBUS A-319;;;\n"), 2000))
	sink = memSink{}
	if _, err := Parse(ctx, bytes.NewReader(full[:len(full)/2]), &sink, ParseOptions{}); err == nil {
		t.Error("truncated gzip stream gave no error")
	}
	if _, err := Parse(ctx, bytes.NewReader([]byte{0x1f, 0x8b, 0xff, 0xff}), &sink, ParseOptions{}); err == nil {
		t.Error("corrupt gzip header gave no error")
	}
}

func TestParseSinkError(t *testing.T) {
	boom := errors.New("disk full")
	n := 0
	sink := SinkFunc(func(ACRecord) error {
		n++
		if n > 3 {
			return boom
		}
		return nil
	})
	res, err := Parse(context.Background(), bytes.NewReader(fixture(t, "tar1090.csv")), sink, ParseOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if res.Records != 3 || n != 4 {
		t.Errorf("%d records counted, sink called %d times; want 3, 4", res.Records, n)
	}
}

// endless produces valid tar1090-db lines for ever.
type endless struct {
	n   uint32
	buf []byte
}

func (e *endless) Read(p []byte) (int, error) {
	for len(e.buf) < len(p) {
		e.n++
		e.buf = append(e.buf, fmt.Sprintf("%06X;N%d;C172;00;CESSNA 172 Skyhawk;1999;SOMEBODY;\n", e.n&0xFFFFFF|1, e.n)...)
	}
	n := copy(p, e.buf)
	e.buf = e.buf[n:]
	return n, nil
}

func TestParseCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got int64
	sink := SinkFunc(func(ACRecord) error {
		got++
		if got == 1000 {
			cancel()
		}
		return nil
	})
	done := make(chan struct{})
	var res ParseResult
	var err error
	go func() {
		defer close(done)
		res, err = Parse(ctx, &endless{}, sink, ParseOptions{})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Parse did not stop after the context was cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.Records < 1000 || res.Records > 1000+2*ctxCheckEvery {
		t.Errorf("%d records delivered before stopping; want 1000..%d", res.Records, 1000+2*ctxCheckEvery)
	}

	// An already cancelled context stops within the first check interval.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	var sink2 memSink
	res, err = Parse(ctx2, &endless{}, &sink2, ParseOptions{})
	if !errors.Is(err, context.Canceled) || res.Records > ctxCheckEvery {
		t.Errorf("cancelled context: err = %v after %d records", err, res.Records)
	}
}

func TestParseMaxBytes(t *testing.T) {
	line := "3C6444;D-AIBD;A319;00;AIRBUS A-319;;;\n"
	data := strings.Repeat(line, 1000)

	// Plain input over the limit.
	var sink memSink
	_, err := Parse(context.Background(), strings.NewReader(data), &sink, ParseOptions{MaxBytes: 10 * 1024})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("plain: err = %v, want ErrTooLarge", err)
	}
	// The limit applies to the decompressed size (decompression bomb).
	sink = memSink{}
	small := gz(t, []byte(data))
	if len(small) > 4096 {
		t.Fatalf("test data compresses to %d bytes", len(small))
	}
	_, err = Parse(context.Background(), bytes.NewReader(small), &sink, ParseOptions{MaxBytes: 10 * 1024})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("gzip: err = %v, want ErrTooLarge", err)
	}
	// Exactly at the limit is fine.
	sink = memSink{}
	res, err := Parse(context.Background(), strings.NewReader(data), &sink, ParseOptions{MaxBytes: int64(len(data))})
	if err != nil || res.Records != 1000 {
		t.Errorf("at the limit: %v, %d records", err, res.Records)
	}
}

func TestParseOverlongLine(t *testing.T) {
	var b strings.Builder
	b.WriteString("3C6444;D-AIBD;A319;00;AIRBUS A-319;;;\n")
	b.WriteString("406A9F;G-ZBJH;B788;00;")
	b.WriteString(strings.Repeat("x", 3*maxLineBytes))
	b.WriteString(";;;\n")
	b.WriteString("4CA242;EI-DCL;B738;00;BOEING 737-800;;;\r\n")
	b.WriteString("484161;PH-BXH;B738;00;BOEING 737-800;;;") // no final newline
	var sink memSink
	res, err := Parse(context.Background(), strings.NewReader(b.String()), &sink, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 3 || res.Skipped != 1 {
		t.Errorf("%d records, %d skipped; want 3, 1", res.Records, res.Skipped)
	}
	got := sink.byICAO(t)
	if got[0x4CA242].Desc != "BOEING 737-800" || got[0x484161].Reg != "PH-BXH" {
		t.Errorf("records after the overlong line are damaged: %+v", got)
	}
}

func TestParseProgress(t *testing.T) {
	data := strings.Repeat("3C6444;D-AIBD;A319;00;AIRBUS A-319;;;\n", 5000)
	var calls []Progress
	// The sink dawdles now and then, so that the (coarse, on Windows) clock
	// is guaranteed to advance between progress checks.
	n := 0
	sink := SinkFunc(func(ACRecord) error {
		if n++; n%500 == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		return nil
	})
	res, err := Parse(context.Background(), strings.NewReader(data), sink, ParseOptions{
		Progress:         func(p Progress) { calls = append(calls, p) },
		ProgressInterval: time.Nanosecond,
		BytesTotal:       int64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) < 3 {
		t.Fatalf("only %d progress calls", len(calls))
	}
	var prev Progress
	for i, p := range calls {
		last := i == len(calls)-1
		if last != (p.Stage == "done") || (!last && p.Stage != "download") {
			t.Errorf("call %d: stage %q", i, p.Stage)
		}
		if p.BytesTotal != int64(len(data)) || p.BytesRead < prev.BytesRead || p.Records < prev.Records {
			t.Errorf("call %d: not monotonic or wrong total: %+v after %+v", i, p, prev)
		}
		if f := p.Fraction(); f < 0 || f > 1 {
			t.Errorf("call %d: fraction %v", i, f)
		}
		prev = p
	}
	final := calls[len(calls)-1]
	if final.Records != res.Records || final.Records != 5000 || final.BytesRead != int64(len(data)) || final.Fraction() != 1 {
		t.Errorf("final progress %+v, result %+v", final, res)
	}

	// No "done" report for a failed parse.
	calls = nil
	_, err = Parse(context.Background(), strings.NewReader("<html>"), sink, ParseOptions{
		Progress: func(p Progress) { calls = append(calls, p) },
	})
	if err == nil || len(calls) != 0 {
		t.Errorf("failed parse: err %v, %d progress calls", err, len(calls))
	}
}

func TestProgressFraction(t *testing.T) {
	tests := []struct {
		p    Progress
		want float64
	}{
		{Progress{Stage: "connect", BytesTotal: -1}, -1},
		{Progress{Stage: "download", BytesRead: 50, BytesTotal: 200}, 0.25},
		{Progress{Stage: "download", BytesRead: 300, BytesTotal: 200}, 1},
		{Progress{Stage: "download", BytesRead: 300, BytesTotal: 0}, -1},
		{Progress{Stage: "done", BytesTotal: -1}, 1},
	}
	for _, tc := range tests {
		if got := tc.p.Fraction(); got != tc.want {
			t.Errorf("%+v.Fraction() = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestFieldHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"  AIRBUS A-319 ":    "AIRBUS A-319",
		"":                   "",
		"   ":                "",
		"tab\there":          "tab here",
		"nul\x00byte":        "nul byte",
		"Zürich Flugzeug":    "Zürich Flugzeug",
		"latin1 \xfc umlaut": "latin1 � umlaut",
		"\x01\x02":           "",
	} {
		if got := cleanField([]byte(in)); got != want {
			t.Errorf("cleanField(%q) = %q, want %q", in, got, want)
		}
	}
	// Truncation never cuts a multi-byte character in half.
	long := strings.Repeat("é", 200)
	got := cleanField([]byte(long))
	if len(got) > maxFieldBytes || len(got) < maxFieldBytes-2 || strings.ContainsRune(got, '�') || strings.Trim(got, "é") != "" {
		t.Errorf("cleanField truncated to %d bytes: %q", len(got), got)
	}

	for in, want := range map[string]int{
		"1998": 1998, " 2008 ": 2008, "1998-05-01": 1998, "2017-01-01 00:00:00": 2017,
		"": 0, "98": 0, "19985": 0, "abcd": 0, "1899": 0, "2101": 0, "0000": 0, "20x8": 0,
	} {
		if got := parseYear([]byte(in)); got != want {
			t.Errorf("parseYear(%q) = %d, want %d", in, got, want)
		}
	}

	tests := []struct{ mfr, model, want string }{
		{"Airbus", "A319-112", "Airbus A319-112"},
		{"Airbus", "Airbus A220-300", "Airbus A220-300"},
		{"AIRBUS", "Airbus A220-300", "Airbus A220-300"},
		{"", "Antonov 2", "Antonov 2"},
		{"Cessna", "", "Cessna"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := joinMakeModel(tc.mfr, tc.model); got != tc.want {
			t.Errorf("joinMakeModel(%q, %q) = %q, want %q", tc.mfr, tc.model, got, tc.want)
		}
	}
}

func TestCSVRecord(t *testing.T) {
	p := &parser{}
	tests := []struct {
		line     string
		quote    byte
		want     []string
		complete bool
	}{
		{`a,b,c`, '"', []string{"a", "b", "c"}, true},
		{`"a","b,c",""`, '"', []string{"a", "b,c", ""}, true},
		{`"say ""hi""",x`, '"', []string{`say "hi"`, "x"}, true},
		{`'it''s',plain text,'',`, '\'', []string{"it's", "plain text", "", ""}, true},
		{`'a',O'Hare Aviation,'b'`, '\'', []string{"a", "O'Hare Aviation", "b"}, true},
		{`'5' wide',x`, '\'', []string{"5' wide", "x"}, true}, // stray quote inside a quoted field
		{`a,'open`, '\'', nil, false},
		{``, '"', []string{""}, true},
		{`,`, '"', []string{"", ""}, true},
		{`"only"`, '"', []string{"only"}, true},
	}
	for _, tc := range tests {
		got, complete := p.csvRecord([]byte(tc.line), tc.quote)
		if complete != tc.complete {
			t.Errorf("csvRecord(%q): complete = %v, want %v", tc.line, complete, tc.complete)
			continue
		}
		if !complete {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("csvRecord(%q) = %q, want %q", tc.line, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("csvRecord(%q)[%d] = %q, want %q", tc.line, i, got[i], tc.want[i])
			}
		}
	}
}

func TestSources(t *testing.T) {
	srcs := Sources()
	if len(srcs) != 2 || srcs[0].ID != SourceTar1090 || srcs[1].ID != SourceOpenSky {
		t.Fatalf("Sources() = %+v", srcs)
	}
	for _, s := range srcs {
		if s.Name == "" || s.Licence == "" || s.Attribution == "" || s.Format == FormatAuto ||
			s.ApproxBytes <= 0 || s.ApproxRecords <= 0 {
			t.Errorf("source %s is incomplete: %+v", s.ID, s)
		}
		for _, u := range []string{s.URL, s.Homepage, s.LicenceURL} {
			pu, err := url.Parse(u)
			if err != nil || pu.Scheme != "https" || pu.Host == "" {
				t.Errorf("source %s: bad URL %q", s.ID, u)
			}
		}
		got, ok := SourceByID(s.ID)
		if !ok || got.URL != s.URL {
			t.Errorf("SourceByID(%s) = %+v, %v", s.ID, got, ok)
		}
	}
	if !strings.HasSuffix(srcs[0].URL, "/csv/aircraft.csv.gz") {
		t.Errorf("tar1090-db URL %q does not point at the csv branch", srcs[0].URL)
	}
	if _, ok := SourceByID("nope"); ok {
		t.Error("SourceByID(nope) found something")
	}
	// The returned slice is a copy.
	srcs[0].Name = "changed"
	if Sources()[0].Name == "changed" {
		t.Error("Sources() exposes the internal table")
	}
}

func TestNewestKey(t *testing.T) {
	const prefix = "metadata/aircraft-database-complete-"
	listing := `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>data-samples</Name>
<Prefix>metadata/aircraft-database-complete-</Prefix><KeyCount>5</KeyCount><IsTruncated>false</IsTruncated>
<Contents><Key>metadata/aircraft-database-complete-2024-10.csv</Key><Size>106269499</Size></Contents>
<Contents><Key>metadata/aircraft-database-complete-2025-08.csv</Key><Size>107956517</Size></Contents>
<Contents><Key>metadata/aircraft-database-complete-2025-02.csv</Key><Size>106269499</Size></Contents>
<Contents><Key>metadata/aircraft-database-complete-2026-01.csv.md5</Key><Size>32</Size></Contents>
<Contents><Key>metadata/aircraft-database-complete-2099-01.csv?x=http://evil.example/</Key><Size>1</Size></Contents>
<Contents><Key>metadata/aircraft-database-complete-2099/../../secret.csv</Key><Size>1</Size></Contents>
<Contents><Key>metadata/other-2030-01.csv</Key><Size>1</Size></Contents>
</ListBucketResult>`
	got, err := newestKey(strings.NewReader(listing), prefix)
	if err != nil || got != "metadata/aircraft-database-complete-2025-08.csv" {
		t.Errorf("newestKey = %q, %v", got, err)
	}
	if _, err := newestKey(strings.NewReader(`<ListBucketResult></ListBucketResult>`), prefix); err == nil {
		t.Error("empty listing gave no error")
	}
	if _, err := newestKey(strings.NewReader(`<html><body>Access denied`), prefix); err == nil {
		t.Error("broken listing gave no error")
	}
}

// ---- Import against a local test server -------------------------------------

// rewrite sends every request to the test server, whatever host it names, so
// that the built-in source URLs can be exercised offline.
type rewrite struct {
	target *url.URL
	base   http.RoundTripper
	mu     sync.Mutex
	seen   []string // original URLs requested
}

func (rt *rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.seen = append(rt.seen, req.URL.String())
	rt.mu.Unlock()
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	r2.Host = rt.target.Host
	return rt.base.RoundTrip(r2)
}

func rewriteClient(t *testing.T, srv *httptest.Server) (*http.Client, *rewrite) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rt := &rewrite{target: u, base: srv.Client().Transport}
	return &http.Client{Transport: rt}, rt
}

func TestImportTar1090(t *testing.T) {
	body := gz(t, fixture(t, "tar1090.csv"))
	var userAgent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent.Store(r.Header.Get("User-Agent"))
		if r.URL.Path != "/wiedehopf/tar1090-db/raw/refs/heads/csv/aircraft.csv.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Last-Modified", "Wed, 11 Nov 2026 09:30:00 GMT")
		w.Header().Set("ETag", `"abc123"`)
		w.Write(body)
	}))
	defer srv.Close()
	client, rt := rewriteClient(t, srv)

	var progress []Progress
	var sink memSink
	res, err := Import(context.Background(), ImportOptions{
		Client:   client,
		Progress: func(p Progress) { progress = append(progress, p) },
	}, &sink)
	if err != nil {
		t.Fatal(err)
	}
	// Default source, its real URL, licence and attribution for the UI.
	if res.Source.ID != SourceTar1090 || res.URL != res.Source.URL ||
		!strings.Contains(res.Source.Licence, "ODC-By") || !strings.Contains(res.Source.Attribution, "Mictronics") {
		t.Errorf("result source: %+v", res)
	}
	if len(rt.seen) != 1 || rt.seen[0] != "https://github.com/wiedehopf/tar1090-db/raw/refs/heads/csv/aircraft.csv.gz" {
		t.Errorf("requests: %q", rt.seen)
	}
	if res.Format != FormatTar1090 || res.Records != int64(len(wantTar1090)) || res.Skipped != 3 || res.Empty != 1 || res.Bytes != int64(len(body)) {
		t.Errorf("result: %+v", res)
	}
	if res.ETag != `"abc123"` || !res.LastModified.Equal(time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("ETag %q, Last-Modified %v", res.ETag, res.LastModified)
	}
	if res.Elapsed < 0 { // may be 0 on a coarse clock
		t.Errorf("Elapsed = %v", res.Elapsed)
	}
	if ua, _ := userAgent.Load().(string); !strings.HasPrefix(ua, "AirView/") {
		t.Errorf("User-Agent = %q", ua)
	}
	checkRecords(t, sink.byICAO(t), wantTar1090)

	if len(progress) < 2 {
		t.Fatalf("%d progress calls", len(progress))
	}
	first, last := progress[0], progress[len(progress)-1]
	if first.Stage != "connect" || first.Source != SourceTar1090 || first.URL != res.URL || first.Fraction() != -1 {
		t.Errorf("first progress: %+v", first)
	}
	if last.Stage != "done" || last.Records != res.Records || last.Skipped != 3 || last.Empty != 1 || last.BytesTotal != int64(len(body)) ||
		last.Source != SourceTar1090 || last.URL != res.URL {
		t.Errorf("last progress: %+v", last)
	}
}

func TestImportOpenSkySnapshotLookup(t *testing.T) {
	csv := fixture(t, "opensky_complete.csv")
	listingOK := true
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/data-samples" && r.URL.Query().Get("list-type") == "2":
			mu.Lock()
			ok := listingOK
			mu.Unlock()
			if !ok {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if got := r.URL.Query().Get("prefix"); got != "metadata/aircraft-database-complete-" {
				t.Errorf("listing prefix = %q", got)
			}
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
				`<Contents><Key>metadata/aircraft-database-complete-2025-08.csv</Key></Contents>`+
				`<Contents><Key>metadata/aircraft-database-complete-2027-03.csv</Key></Contents>`+
				`<Contents><Key>metadata/aircraft-database-complete-2026-12.csv</Key></Contents></ListBucketResult>`)
		case strings.HasPrefix(r.URL.Path, "/data-samples/metadata/aircraft-database-complete-"):
			w.Header().Set("Content-Type", "text/csv")
			w.Write(csv)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, _ := rewriteClient(t, srv)

	// The newest snapshot in the bucket listing is used.
	var sink memSink
	res, err := Import(context.Background(), ImportOptions{Source: SourceOpenSky, Client: client}, &sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://s3.opensky-network.org/data-samples/metadata/aircraft-database-complete-2027-03.csv" {
		t.Errorf("URL = %q, want the newest snapshot", res.URL)
	}
	if res.Source.ID != SourceOpenSky || res.Format != FormatOpenSky || res.Records != 5 || res.Skipped != 3 || res.Empty != 1 {
		t.Errorf("result: %+v", res)
	}
	if !strings.Contains(strings.ToLower(res.Source.Licence), "unlicensed") {
		t.Errorf("OpenSky licence text: %q", res.Source.Licence)
	}

	// When the listing fails the built-in URL is the fallback.
	mu.Lock()
	listingOK = false
	mu.Unlock()
	sink = memSink{}
	res, err = Import(context.Background(), ImportOptions{Source: SourceOpenSky, Client: client}, &sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != res.Source.URL || res.Records != 5 {
		t.Errorf("fallback: URL %q, %d records", res.URL, res.Records)
	}
}

func TestImportCustomURLAutoDetect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/legacy.csv":
			w.Write(fixture(t, "opensky_legacy.csv"))
		case "/plain.csv":
			w.Write(fixture(t, "tar1090.csv"))
		case "/page.html":
			io.WriteString(w, "<!DOCTYPE html><html><body>Sign in to continue</body></html>")
		default:
			http.Error(w, "gone", http.StatusGone)
		}
	}))
	defer srv.Close()

	var sink memSink
	res, err := Import(context.Background(), ImportOptions{URL: srv.URL + "/legacy.csv", Client: srv.Client()}, &sink)
	if err != nil || res.Format != FormatOpenSky || res.Records != 3 || res.URL != srv.URL+"/legacy.csv" {
		t.Errorf("legacy: %+v, %v", res, err)
	}
	sink = memSink{}
	res, err = Import(context.Background(), ImportOptions{URL: srv.URL + "/plain.csv", Client: srv.Client()}, &sink)
	if err != nil || res.Format != FormatTar1090 || res.Records != int64(len(wantTar1090)) {
		t.Errorf("plain: %+v, %v", res, err)
	}

	// Failures.
	sink = memSink{}
	if _, err = Import(context.Background(), ImportOptions{URL: srv.URL + "/page.html", Client: srv.Client()}, &sink); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("html page: err = %v, want ErrUnknownFormat", err)
	}
	if _, err = Import(context.Background(), ImportOptions{URL: srv.URL + "/missing", Client: srv.Client()}, &sink); err == nil || !strings.Contains(err.Error(), "410") {
		t.Errorf("HTTP error: err = %v", err)
	}
	if _, err = Import(context.Background(), ImportOptions{Source: "nope"}, &sink); err == nil {
		t.Error("unknown source accepted")
	}
	if _, err = Import(context.Background(), ImportOptions{}, nil); err == nil {
		t.Error("nil sink accepted")
	}
	if _, err = Import(context.Background(), ImportOptions{URL: "http://127.0.0.1:1/x", Client: srv.Client()}, &sink); err == nil {
		t.Error("connection failure gave no error")
	}
	if len(sink.recs) != 0 {
		t.Errorf("%d records delivered by failed imports", len(sink.recs))
	}
}

// slowServer streams valid records for ever (until the client goes away),
// or sends a little and then stalls.
func slowServer(t *testing.T, stallAfter int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		line := []byte("3C6444;D-AIBD;A319;00;AIRBUS A-319;;;\n")
		chunk := bytes.Repeat(line, 512)
		for i := 0; ; i++ {
			if stallAfter > 0 && i >= stallAfter {
				select { // stall: keep the connection open without sending
				case <-r.Context().Done():
				case <-time.After(20 * time.Second):
				}
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}))
}

func TestImportCancel(t *testing.T) {
	srv := slowServer(t, 0)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sink memSink
	start := time.Now()
	res, err := Import(ctx, ImportOptions{
		URL:              srv.URL,
		Client:           srv.Client(),
		ProgressInterval: time.Millisecond,
		Progress: func(p Progress) {
			if p.Stage == "download" && p.Records > 0 {
				cancel()
			}
		},
	}, &sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("cancellation took %v", time.Since(start))
	}
	// Counts are valid for a cancelled import, and the records delivered so
	// far stay delivered.
	if res.Records == 0 || res.Records != int64(len(sink.recs)) {
		t.Errorf("result counts %d records, sink holds %d", res.Records, len(sink.recs))
	}

	// A context that is already cancelled never connects.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := Import(ctx2, ImportOptions{URL: srv.URL, Client: srv.Client()}, &memSink{}); !errors.Is(err, context.Canceled) {
		t.Errorf("pre-cancelled: err = %v", err)
	}
	// A deadline is reported as such.
	ctx3, cancel3 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel3()
	if _, err := Import(ctx3, ImportOptions{URL: srv.URL, Client: srv.Client()}, &memSink{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: err = %v", err)
	}
}

func TestImportIdleTimeout(t *testing.T) {
	srv := slowServer(t, 3)
	defer srv.Close()
	var sink memSink
	start := time.Now()
	res, err := Import(context.Background(), ImportOptions{
		URL:         srv.URL,
		Client:      srv.Client(),
		IdleTimeout: 200 * time.Millisecond,
	}, &sink)
	if !errors.Is(err, ErrImportIdle) {
		t.Fatalf("err = %v, want ErrImportIdle", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("idle abort took %v", d)
	}
	if res.Records == 0 {
		t.Error("records received before the stall were not counted")
	}

	// A slow sink is not a stalled download: time spent in Put does not count.
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "tar1090.csv"))
	}))
	defer fast.Close()
	n := 0
	slow := SinkFunc(func(ACRecord) error {
		if n++; n <= 3 {
			time.Sleep(120 * time.Millisecond)
		}
		return nil
	})
	if _, err := Import(context.Background(), ImportOptions{URL: fast.URL, Client: fast.Client(), IdleTimeout: 100 * time.Millisecond}, slow); err != nil {
		t.Errorf("slow sink: %v", err)
	}
}

// Live download. Not run by default:
//
//	AIRVIEW_REFDATA_LIVE=1        tar1090-db (about 9 MB)
//	AIRVIEW_REFDATA_LIVE=opensky  OpenSky (over 100 MB)
//	AIRVIEW_REFDATA_LIVE=all      both
func TestImportLive(t *testing.T) {
	mode := os.Getenv("AIRVIEW_REFDATA_LIVE")
	if mode == "" {
		t.Skip("set AIRVIEW_REFDATA_LIVE=1 (tar1090-db), =opensky or =all to download the real databases")
	}
	run := func(source string, minRecords int64) {
		t.Run(source, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			known := map[uint32]ACRecord{}
			var n int64
			lastLog := time.Now()
			res, err := Import(ctx, ImportOptions{
				Source:    source,
				UserAgent: "refdata-live-test",
				Progress: func(p Progress) {
					if p.Stage != "download" || time.Since(lastLog) > 5*time.Second {
						lastLog = time.Now()
						t.Logf("%-8s %9d records  fraction %5.2f  %s", p.Stage, p.Records, p.Fraction(), p.URL)
					}
				},
			}, SinkFunc(func(r ACRecord) error {
				n++
				switch r.ICAO {
				case 0x3C6444, 0xA00001, 0x7C4920:
					known[r.ICAO] = r
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d records, %d skipped, %d empty, %d bytes in %v from %s (last modified %v)",
				res.Source.Name, res.Records, res.Skipped, res.Empty, res.Bytes, res.Elapsed.Round(time.Millisecond), res.URL, res.LastModified)
			t.Logf("licence: %s", res.Source.Licence)
			if res.Records != n || res.Records < minRecords {
				t.Errorf("%d records (sink saw %d), want at least %d", res.Records, n, minRecords)
			}
			if res.Skipped > res.Records/200 {
				t.Errorf("%d lines skipped as malformed (%d records): the format may have changed", res.Skipped, res.Records)
			}
			for icao, r := range known {
				t.Logf("%06X: %+v", icao, r)
			}
			if r := known[0x3C6444]; r.Reg != "D-AIBD" || r.Type != "A319" {
				t.Errorf("3C6444 = %+v, want D-AIBD / A319", r)
			}
			if r := known[0xA00001]; r.Reg != "N1" {
				t.Errorf("A00001 = %+v, want N1", r)
			}
		})
	}
	if mode != "opensky" {
		run(SourceTar1090, 500000)
	}
	if mode == "opensky" || mode == "all" {
		run(SourceOpenSky, 400000)
	}
}
