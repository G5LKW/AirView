package server

import (
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"airview/internal/model"
	"airview/internal/registry"
)

// Persistence of the MLAT console history (admin MLAT console, owner
// Timing & MLAT section).
//
// The rings of mlat_history.go live in memory. So that a restart (an
// update, a reboot of the VPS) does not blank the last 24 h of the admin
// charts and the last 2 h of every owner's clock chart, the server writes
// them to <data>/mlat-history.gob.gz every mlatHistSaveEvery and when it
// stops, and reads the file back when it starts:
//
//   - network points newer than 24 h are restored at once;
//   - station rings are stored by SID (station indexes are an in-memory
//     detail of the registry) and attached at the first sample after the
//     start, when the registry is available; points older than 2 h and
//     SIDs the registry no longer knows are dropped;
//   - the first sample after the restart has no rates (the engine's
//     counters start again from zero; the gap shows as null, as after an
//     engine rebuild).
//
// Size: about 0.2 MB for the network ring and 28 B per station point
// before compression (≈ 3.4 MB at 1,000 stations with full rings; gzip
// roughly halves it). The write is atomic (temporary file + rename); a
// missing, foreign or corrupt file is ignored (logged) and replaced at the
// next save. The file holds clock statistics per SID, no positions; it is
// covered by the owner's data deletion only indirectly (the SID disappears
// from the registry, so its ring is dropped at the next load and
// overwritten at the next save, at most mlatHistSaveEvery later).

const (
	mlatHistFile      = "mlat-history.gob.gz"
	mlatHistSaveEvery = 10 * time.Minute
	mlatHistVersion   = 1
	// mlatHistMaxFile bounds what is read back (a hostile or damaged file
	// must not exhaust memory): 1,440 network points and 100,000 stations
	// of 120 points each are far below it.
	mlatHistMaxFile = 512 << 20
)

// mlatHistDisk is the file format (gob).
type mlatHistDisk struct {
	Version int
	SavedMs int64
	Net     []mlatPoint              // oldest first
	St      map[string][]mlatStPoint // by SID, oldest first
}

// snapshot copies the rings for saving; sidOf maps a station index to its
// SID ("" = unknown, skipped). Pending (not yet attached) station rings
// from the previous file are carried over unchanged.
func (h *mlatHistory) snapshot(sidOf func(model.StationIdx) string, now time.Time) mlatHistDisk {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := mlatHistDisk{Version: mlatHistVersion, SavedMs: now.UnixMilli(), St: map[string][]mlatStPoint{}}
	for i := 0; i < h.n; i++ {
		d.Net = append(d.Net, h.pts[(h.head-h.n+i+len(h.pts))%len(h.pts)])
	}
	for sid, pts := range h.pending {
		d.St[sid] = pts
	}
	for idx, r := range h.st {
		if sid := sidOf(idx); sid != "" {
			d.St[sid] = r.list(0)
		}
	}
	return d
}

// restore loads a saved file into an empty history: network points at
// once, station rings into h.pending until attach. Points older than the
// rings' spans or from the future are dropped.
func (h *mlatHistory) restore(d mlatHistDisk, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nowMs := now.UnixMilli()
	netFrom := nowMs - int64(mlatHistNet)*mlatHistEvery.Milliseconds()
	stFrom := nowMs - int64(mlatHistStation)*mlatHistEvery.Milliseconds()
	last := int64(0)
	if h.n > 0 {
		last = h.pts[(h.head-1+len(h.pts))%len(h.pts)].T
	}
	for _, p := range d.Net {
		if p.T < netFrom || p.T > nowMs || p.T <= last {
			continue
		}
		h.pts[h.head] = p
		h.head = (h.head + 1) % len(h.pts)
		h.n = min(h.n+1, len(h.pts))
		last = p.T
	}
	h.pending = map[string][]mlatStPoint{}
	for sid, pts := range d.St {
		var keep []mlatStPoint
		lastT := int64(0)
		for _, p := range pts {
			if p.T >= stFrom && p.T <= nowMs && p.T > lastT {
				keep = append(keep, p)
				lastT = p.T
			}
		}
		if len(keep) > mlatHistStation {
			keep = keep[len(keep)-mlatHistStation:]
		}
		if len(keep) > 0 && sid != "" {
			h.pending[sid] = keep
		}
	}
	if len(h.pending) == 0 {
		h.pending = nil
	}
}

// attachPending moves restored station rings onto the registry's current
// station indexes (called before a sample, under no lock). Stations that
// already have a ring keep it; unknown SIDs are dropped.
func (h *mlatHistory) attachPending(idxOf map[string]model.StationIdx) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return
	}
	for sid, pts := range h.pending {
		idx, ok := idxOf[sid]
		if !ok || h.st[idx] != nil {
			continue
		}
		r := &stRing{seen: h.gen}
		for _, p := range pts {
			r.add(p)
		}
		h.st[idx] = r
	}
	h.pending = nil
}

// hasPending reports whether restored station rings wait for attach.
func (h *mlatHistory) hasPending() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pending != nil
}

// empty reports whether there is nothing to save.
func (h *mlatHistory) empty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n == 0 && len(h.st) == 0 && h.pending == nil
}

// writeMLATHistory writes d to path atomically.
func writeMLATHistory(path string, d mlatHistDisk) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mlat-history-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after the rename
	zw := gzip.NewWriter(tmp)
	err = gob.NewEncoder(zw).Encode(&d)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if serr := tmp.Sync(); err == nil {
		err = serr
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return renameOver(name, path)
}

// renameOver renames tmp to path, replacing path (Windows refuses a plain
// rename onto an existing file only when it is open elsewhere; retry once
// after removing it).
func renameOver(tmp, path string) error {
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}

// readMLATHistory reads a saved history (fs.ErrNotExist when there is none).
func readMLATHistory(path string) (mlatHistDisk, error) {
	var d mlatHistDisk
	f, err := os.Open(path)
	if err != nil {
		return d, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(io.LimitReader(f, mlatHistMaxFile))
	if err != nil {
		return d, err
	}
	if err := gob.NewDecoder(io.LimitReader(zr, mlatHistMaxFile)).Decode(&d); err != nil {
		return d, err
	}
	if d.Version != mlatHistVersion {
		return mlatHistDisk{}, fmt.Errorf("mlat history: version %d not supported", d.Version)
	}
	return d, nil
}

// mlatHistPath is the history file of the running engine ("" = none: no
// data directory, e.g. some tests).
func (s *Server) mlatHistPath() string {
	if s.eng == nil {
		return ""
	}
	dir := s.eng.DataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, mlatHistFile)
}

// loadMLATHistory restores the saved history (at server start).
func (s *Server) loadMLATHistory(now time.Time) {
	path := s.mlatHistPath()
	if path == "" {
		return
	}
	d, err := readMLATHistory(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		s.log.Warn("MLAT history file ignored", "file", path, "err", err)
		return
	}
	s.mlatHist.restore(d, now)
}

// saveMLATHistory writes the history (periodically and at shutdown).
func (s *Server) saveMLATHistory(now time.Time) {
	path := s.mlatHistPath()
	if path == "" || s.mlatHist.empty() {
		return
	}
	sids := map[model.StationIdx]string{}
	if reg, err := s.reg(); err == nil && reg != nil {
		for _, st := range reg.List(registry.Filter{}) {
			sids[st.Idx] = st.SID
		}
	}
	d := s.mlatHist.snapshot(func(idx model.StationIdx) string { return sids[idx] }, now)
	if err := writeMLATHistory(path, d); err != nil {
		s.log.Warn("MLAT history not saved", "file", path, "err", err)
	}
}
