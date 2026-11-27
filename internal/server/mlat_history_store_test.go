package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"airview/internal/model"
)

// TestMLATHistoryPersists: the network ring and the station rings survive a
// save / load cycle (a restart); station rings follow their SID to a new
// index, unknown SIDs and points beyond the rings' spans are dropped, and
// the first sample after the restart has no rates.
func TestMLATHistoryPersists(t *testing.T) {
	h := newMLATHistory()
	m := &fakeMLAT{key: new(int), idx: []model.StationIdx{3, 4, 5}}
	t0 := time.Now().Add(-30 * time.Minute)
	for i := 0; i < 20; i++ {
		h.sample(m, m.idx, t0.Add(time.Duration(i)*time.Minute))
		m.advance(1)
	}
	before := historyDTO(h.points(t0.Add(25*time.Minute), mlatHistNet))
	sid := map[model.StationIdx]string{3: "sid-a", 4: "sid-b", 5: "sid-gone"}
	path := filepath.Join(t.TempDir(), mlatHistFile)
	if err := writeMLATHistory(path, h.snapshot(func(i model.StationIdx) string { return sid[i] }, t0.Add(20*time.Minute))); err != nil {
		t.Fatal(err)
	}

	// "Restart": a new history, the registry gave the stations new indexes
	// and sid-gone was deleted.
	d, err := readMLATHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	now := t0.Add(25 * time.Minute)
	h2 := newMLATHistory()
	h2.restore(d, now)
	after := historyDTO(h2.points(now, mlatHistNet))
	if len(after.T) != 20 || len(after.T) != len(before.T) {
		t.Fatalf("restored %d network points, want 20", len(after.T))
	}
	for i := range before.T {
		if after.T[i] != before.T[i] || after.FixP95[i] != before.FixP95[i] || (after.Groups[i] == nil) != (before.Groups[i] == nil) {
			t.Fatalf("point %d differs: %v/%v", i, after.T[i], before.T[i])
		}
	}
	h2.attachPending(map[string]model.StationIdx{"sid-a": 30, "sid-b": 40})
	if n := len(h2.station(30)); n != 20 {
		t.Fatalf("station sid-a: %d points", n)
	}
	if h2.station(5) != nil || h2.station(3) != nil || len(h2.st) != 2 {
		t.Fatalf("unexpected rings %v", h2.st)
	}
	// The first sample after the restart keeps the restored station rings
	// (the engine knows the station under its new index) and has no rates.
	m2 := &fakeMLAT{key: new(int), idx: []model.StationIdx{30, 40}}
	h2.sample(m2, m2.idx, now.Add(time.Minute))
	if n := len(h2.station(30)); n != 21 {
		t.Fatalf("station ring after the first sample: %d points", n)
	}
	last := historyDTO(h2.points(now.Add(2*time.Minute), mlatHistNet))
	if g := last.Groups[len(last.Groups)-1]; g != nil {
		t.Fatalf("first sample after a restart has rates %v", *g)
	}

	// Points older than the rings' spans are dropped on load.
	h3 := newMLATHistory()
	h3.restore(d, t0.Add(25*time.Hour))
	if n := len(h3.points(t0.Add(25*time.Hour), mlatHistNet)); n != 0 {
		t.Fatalf("stale network points restored: %d", n)
	}
	if h3.hasPending() {
		t.Fatal("stale station points restored")
	}

	// A damaged file is an error, not a crash; a missing one is ErrNotExist.
	if err := os.WriteFile(path, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMLATHistory(path); err == nil {
		t.Fatal("corrupt file accepted")
	}
	if _, err := readMLATHistory(path + ".missing"); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
}

// TestMLATHistoryFileFromServer: Server.saveMLATHistory / loadMLATHistory
// through the data directory of a real engine.
func TestMLATHistoryFileFromServer(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.withMLAT()
	now := time.Now()
	f.srv.saveMLATHistory(now)
	path := f.srv.mlatHistPath()
	if path == "" {
		t.Skip("fixture engine has no data directory")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history not written: %v", err)
	}
	want := len(f.srv.mlatHist.points(now, mlatHistNet))
	f.srv.mlatHist = newMLATHistory()
	f.srv.loadMLATHistory(now)
	if got := len(f.srv.mlatHist.points(now, mlatHistNet)); got != want || want == 0 {
		t.Fatalf("restored %d points, want %d", got, want)
	}
	if !f.srv.mlatHist.hasPending() {
		t.Fatal("station rings not restored")
	}
}
