package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"

	"airview/internal/model"
)

const (
	logFileName = "airview.log"
	// logMaxBytes caps the log file until the configuration is loaded
	// (process.logMaxMb, SCALE-10); when it is reached the file is renamed
	// to airview.log.1 (older ones shift to .2 ... up to process.logFiles).
	logMaxBytes = 4 << 20
)

// rotatingFile is a size-capped append-only log file. Write errors are
// swallowed: logging must never take the application down (full disk,
// read-only data directory).
type rotatingFile struct {
	mu    sync.Mutex
	path  string
	max   int64
	files int // rotated files kept (airview.log.1 .. .files)
	f     *os.File
	size  int64
}

func openLogFile(path string) *rotatingFile {
	r := &rotatingFile{path: path, max: logMaxBytes, files: 1}
	r.open()
	return r
}

func (r *rotatingFile) open() {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.f = nil
		return
	}
	r.f = f
	r.size = 0
	if st, err := f.Stat(); err == nil {
		r.size = st.Size()
	}
}

func (r *rotatingFile) rotate() {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	n := max(r.files, 1)
	_ = os.Remove(r.path + "." + strconv.Itoa(n))
	for i := n - 1; i >= 1; i-- {
		_ = os.Rename(r.path+"."+strconv.Itoa(i), r.path+"."+strconv.Itoa(i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		// Cannot move it away (e.g. open elsewhere): start over in place.
		_ = os.Truncate(r.path, 0)
	}
	r.open()
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return len(p), nil
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		r.rotate()
		if r.f == nil {
			return len(p), nil
		}
	}
	n, _ := r.f.Write(p)
	r.size += int64(n)
	return len(p), nil
}

// setLimits changes the size cap and the number of rotated files kept.
func (r *rotatingFile) setLimits(maxBytes int64, files int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxBytes > 0 {
		r.max = maxBytes
	}
	if files > 0 {
		r.files = files
	}
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// teeHandler sends every record to two slog handlers.
type teeHandler struct{ a, b slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if t.a.Enabled(ctx, r.Level) {
		_ = t.a.Handle(ctx, r.Clone())
	}
	if t.b.Enabled(ctx, r.Level) {
		_ = t.b.Handle(ctx, r)
	}
	return nil
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{t.a.WithAttrs(attrs), t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{t.a.WithGroup(name), t.b.WithGroup(name)}
}

// logLevel is the level of the log file (process.logLevel); stderrLevel is
// the level of the standard-error copy (process.journalLevel).
var logLevel, stderrLevel slog.LevelVar

// debugForced reports AIRVIEW_DEBUG=1.
func debugForced() bool {
	v := os.Getenv("AIRVIEW_DEBUG")
	return v == "1" || v == "true"
}

// underSystemd reports whether standard error is connected to the systemd
// journal (systemd sets JOURNAL_STREAM for services whose output goes
// there).
func underSystemd() bool { return os.Getenv("JOURNAL_STREAM") != "" }

func parseLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// setLogLevels applies process.logLevel ("" = info) to the log file and
// process.journalLevel to standard error ("" = warn under systemd, else
// the file's level). AIRVIEW_DEBUG=1 wins for both.
func setLogLevels(p model.ProcessConfig) {
	l, s := parseLevel(p.LogLevel), parseLevel(p.EffectiveJournalLevel(underSystemd()))
	if debugForced() {
		l, s = slog.LevelDebug, slog.LevelDebug
	}
	logLevel.Set(l)
	stderrLevel.Set(s)
}

// setLogLevel applies process.logLevel with the automatic journal level.
func setLogLevel(name string) { setLogLevels(model.ProcessConfig{LogLevel: name}) }

// newLogger logs to stderr and to the log file. AIRVIEW_DEBUG=1 enables
// debug records; process.logLevel / journalLevel set the levels once the
// configuration is loaded. Under systemd the stderr copy starts at warn so
// the journal on an SD card does not store every line of airview.log again.
func newLogger(stderr io.Writer, file io.Writer) *slog.Logger {
	setLogLevel("")
	return slog.New(teeHandler{
		a: slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: &stderrLevel}),
		b: slog.NewTextHandler(file, &slog.HandlerOptions{Level: &logLevel}),
	})
}
