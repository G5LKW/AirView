package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// The Windows service has no console and no journal: it logs to
// <data>\logs\feeder.log, rotated by size (feeder.1.log is the newest old
// file). The tray's "Open logs" opens that directory. The Windows Event Log
// gets start, stop and failure events only (service_windows.go).

const (
	logFileName     = "feeder.log"
	defaultLogMaxMB = 10
	defaultLogKeep  = 5
	statusURLFile   = "status-url.txt" // in the log directory: where the tray finds the page
	crashFileName   = "crash.log"      // fatal Go runtime errors (debug.SetCrashOutput)
)

// logSettings locate and bound the log files.
type logSettings struct {
	Dir   string
	MaxMB int
	Keep  int
}

// resolve fills the defaults: flags, then AIRVIEW_FEEDER_LOG_*, then
// <data>\logs, 10 MB, 5 old files.
func (l *logSettings) resolve(dataDir string, getenv func(string) string) {
	if l.Dir == "" {
		l.Dir = getenv(envLogDir)
	}
	if l.Dir == "" {
		l.Dir = filepath.Join(dataDir, "logs")
	}
	if l.MaxMB <= 0 {
		if n, err := strconv.Atoi(getenv(envLogMaxMB)); err == nil && n > 0 {
			l.MaxMB = n
		} else {
			l.MaxMB = defaultLogMaxMB
		}
	}
	if l.Keep <= 0 {
		if n, err := strconv.Atoi(getenv(envLogKeep)); err == nil && n > 0 {
			l.Keep = n
		} else {
			l.Keep = defaultLogKeep
		}
	}
	if l.Keep > 100 {
		l.Keep = 100
	}
}

// rotatingLog is an io.Writer that appends to dir\feeder.log and rotates
// it when it would grow beyond max bytes, keeping keep old files.
type rotatingLog struct {
	mu   sync.Mutex
	dir  string
	max  int64
	keep int
	f    *os.File
	size int64
}

func openRotatingLog(dir string, maxBytes int64, keep int) (*rotatingLog, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if maxBytes < 4096 {
		maxBytes = 4096
	}
	if keep < 1 {
		keep = 1
	}
	l := &rotatingLog{dir: dir, max: maxBytes, keep: keep}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) path(i int) string {
	if i == 0 {
		return filepath.Join(l.dir, logFileName)
	}
	return filepath.Join(l.dir, fmt.Sprintf("feeder.%d.log", i))
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write implements io.Writer. A failed rotation keeps appending to the
// current file rather than losing lines.
func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) rotate() {
	if err := l.f.Close(); err != nil {
		return
	}
	l.f = nil
	_ = os.Remove(l.path(l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		_ = os.Rename(l.path(i), l.path(i+1))
	}
	_ = os.Rename(l.path(0), l.path(1))
	if err := l.open(); err != nil {
		// Could not create a new file (disk full, permissions): reopen
		// whatever is there so Write keeps trying.
		l.f = nil
	}
}

// Close closes the current file.
func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// writeStatusURL records the status page URL for the tray (the data
// directory may not be readable by the user; the log directory is).
func writeStatusURL(logDir, url string) {
	if url == "" {
		return
	}
	tmp := filepath.Join(logDir, statusURLFile+".tmp")
	if err := os.WriteFile(tmp, []byte(url+"\n"), 0o644); err == nil {
		_ = os.Rename(tmp, filepath.Join(logDir, statusURLFile))
	}
}
