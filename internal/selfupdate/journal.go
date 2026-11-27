package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Journal phases.
const (
	PhaseIdle       = ""           // nothing in progress
	PhaseSwapped    = "swapped"    // new binary in place, on trial
	PhaseCommitted  = "committed"  // new binary kept
	PhaseRolledBack = "rolledback" // old binary restored
)

// journalFile is the name of the update journal inside the state directory.
const journalFile = "selfupdate.json"

// Journal is the persistent record of the last update transaction. It lets
// a restarted process (new or old binary) and a separate supervisor agree
// on what happened, and keeps the anti-replay watermark.
type Journal struct {
	Phase   string    `json:"phase"`
	Product string    `json:"product,omitempty"`
	From    string    `json:"from,omitempty"` // version before the update
	To      string    `json:"to,omitempty"`   // version installed
	Target  string    `json:"target,omitempty"`
	Exe     string    `json:"exe,omitempty"`     // path of the program file
	Old     string    `json:"old,omitempty"`     // path of the previous binary (rename-aside)
	FromSHA string    `json:"fromSha,omitempty"` // SHA-256 of the previous binary
	ToSHA   string    `json:"toSha,omitempty"`   // SHA-256 of the installed binary
	Started time.Time `json:"started,omitzero"`
	// Deadline is the end of the health window of the supervisor.
	Deadline time.Time `json:"deadline,omitzero"`
	// Result is a one-line human-readable outcome.
	Result   string    `json:"result,omitempty"`
	Finished time.Time `json:"finished,omitzero"`
}

// The start-up counter of a trial lives in selfupdate.boots (one line per
// start, appended by Recover), and the outcome of the last check plus the
// anti-replay watermark (newest accepted publication time per channel) in
// selfupdate-status.json; both are next to the journal in StateDir.

func loadJournal(dir string) (Journal, error) {
	var j Journal
	b, err := os.ReadFile(filepath.Join(dir, journalFile))
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return Journal{}, fmt.Errorf("update journal %s: %w", filepath.Join(dir, journalFile), err)
	}
	return j, nil
}

// saveJournal writes the journal atomically (temp file, fsync, rename).
func saveJournal(dir string, j Journal) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, journalFile), append(b, '\n'), 0o644)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// ErrBusy is returned when another update is in progress.
var ErrBusy = errors.New("another update is in progress")

// lockStaleAfter: a lock older than this is considered abandoned (a
// transaction takes at most a download plus the health window).
const lockStaleAfter = 45 * time.Minute

// acquireLock creates dir/selfupdate.lock exclusively. It returns a
// release function.
func acquireLock(dir string, now time.Time) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "selfupdate.lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %d\n", os.Getpid(), now.Unix())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		b, _ := os.ReadFile(path)
		fields := strings.Fields(string(b))
		stale := true
		if len(fields) == 2 {
			if ts, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				stale = now.Sub(time.Unix(ts, 0)) > lockStaleAfter
			}
		}
		if !stale {
			return nil, ErrBusy
		}
		os.Remove(path)
	}
	return nil, ErrBusy
}
