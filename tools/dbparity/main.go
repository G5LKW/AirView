// Command dbparity is the database parity harness: it proves that a new
// AirView build (typically one with storage / database efficiency changes)
// loses no feature against an old build (by default the released
// dist/airview.exe), by running both on identical data and comparing every
// GET route of the station API, the exports and the stored rows, and by
// testing the migration of an old-format database, including a crash in
// the middle of it.
//
//	go run ./tools/dbparity                 # build the tree, compare with dist/airview.exe
//	go run ./tools/dbparity -new old        # self-test: the old build against itself
//
// See tools/dbparity/README.md.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/sim"
)

type options struct {
	oldExe    string
	newExe    string
	work      string
	port      int
	seed      uint64
	aircraft  int
	sessions  int
	session   time.Duration
	live      time.Duration
	settle    time.Duration
	retention string
	phases    string
	refresh   bool
	kills     int
	migWait   time.Duration
}

var logOut io.Writer = os.Stderr
var logMu sync.Mutex

func logf(f string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(logOut, "%s "+f+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
}

func main() {
	if helperMain(os.Args[1:]) {
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "schema" {
		if err := dumpSchema(context.Background(), os.Stdout, os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	var o options
	defExe := "airview"
	if runtime.GOOS == "windows" {
		defExe += ".exe"
	}
	flag.StringVar(&o.oldExe, "old", filepath.Join("dist", defExe), "old (reference) AirView `binary`; it is copied into the work directory and only the copy is run")
	flag.StringVar(&o.newExe, "new", "", "new AirView `binary`; \"\" = build ./cmd/airview from this tree, \"old\" = the old binary again (self-test)")
	flag.StringVar(&o.work, "work", filepath.Join(os.TempDir(), "airview-dbparity"), "work `directory` (binaries, fixtures, data directories, reports)")
	flag.IntVar(&o.port, "port", 18911, "HTTP `port` of the old instance; the new one uses port+1 (or a free port)")
	flag.Uint64Var(&o.seed, "seed", 1, "simulator `seed`")
	flag.IntVar(&o.aircraft, "aircraft", 60, "simulated aircraft (constant density)")
	flag.IntVar(&o.sessions, "sessions", 3, "fixture recording sessions, one simulated day apart (history = sessions-1 days)")
	flag.DurationVar(&o.session, "session", 3*time.Minute, "length of each fixture recording session")
	flag.DurationVar(&o.live, "live", 4*time.Minute, "length of the live (both builds fed at once) phase")
	flag.DurationVar(&o.settle, "settle", 20*time.Second, "wait after start before comparing (start-up maintenance)")
	flag.StringVar(&o.retention, "retention", "short", "\"short\" (trails 2 d, minute stats 1 d: the pruning tiers run on the fixture) or \"default\"")
	flag.StringVar(&o.phases, "phases", "fixture,migrate,crash,live", "comma-separated phases to run")
	flag.BoolVar(&o.refresh, "refresh", false, "rebuild the fixture even when a cached one exists")
	flag.IntVar(&o.kills, "kills", 5, "crash phase: number of kills during the start-up, and again during the data migration, before the final start")
	flag.DurationVar(&o.migWait, "migwait", 15*time.Minute, "longest wait for the new build's background data migration to complete")
	flag.Parse()

	ensureConsole()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := run(ctx, o)
	if err != nil {
		logf("ERROR: %v", err)
		if code == 0 {
			code = 2
		}
	}
	os.Exit(code)
}

// harness is one run.
type harness struct {
	o        options
	work     string
	oldBin   string
	newBin   string
	self     bool
	recv     model.Receiver
	fixture  string // data directory of the fixture
	meta     fixtureMeta
	results  []phaseResult
	newStart time.Duration // time-to-healthy of the new build (migrate phase)
	running  []*instance
	runMu    sync.Mutex
	mig      migState // data migration of the new build in the migrate phase
}

// phaseResult is the outcome of one phase.
type phaseResult struct {
	Phase    string
	OK       bool
	Calls    int
	Failed   []callResult  `json:",omitempty"`
	Tables   []tableResult `json:",omitempty"`
	Problems []string      `json:",omitempty"`
	Notes    []string      `json:",omitempty"`
}

type callResult struct {
	Key    string
	URL    string
	Status [2]int
	Diffs  []string
}

func run(ctx context.Context, o options) (int, error) {
	work, err := filepath.Abs(o.work)
	if err != nil {
		return 2, err
	}
	h := &harness{o: o, work: work}
	if err := os.MkdirAll(filepath.Join(work, "bin"), 0o755); err != nil {
		return 2, err
	}
	lf, err := os.Create(filepath.Join(work, "dbparity.log"))
	if err == nil {
		logOut = io.MultiWriter(os.Stderr, lf)
		defer lf.Close()
	}
	defer h.stopAll()
	if err := h.prepareBinaries(ctx); err != nil {
		return 2, err
	}
	// The simulator's station: the receiver both builds are configured with.
	h.recv = sim.New(sim.Config{Seed: o.seed}).Station()
	h.recv.Located = true

	phases := map[string]bool{}
	for _, p := range strings.Split(o.phases, ",") {
		phases[strings.TrimSpace(p)] = true
	}
	if err := h.ensureFixture(ctx, phases["fixture"] || o.refresh); err != nil {
		return 2, fmt.Errorf("fixture: %w", err)
	}
	for _, p := range []struct {
		name string
		fn   func(context.Context) phaseResult
	}{{"migrate", h.phaseMigrate}, {"crash", h.phaseCrash}, {"live", h.phaseLive}} {
		if !phases[p.name] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		logf("=== phase %s", p.name)
		r := p.fn(ctx)
		h.results = append(h.results, r)
		logf("=== phase %s: %s", p.name, okText(r.OK))
	}
	return h.report()
}

func okText(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// prepareBinaries copies the old binary and builds (or copies) the new one.
func (h *harness) prepareBinaries(ctx context.Context) error {
	ext := filepath.Ext(h.o.oldExe)
	h.oldBin = filepath.Join(h.work, "bin", "airview-old"+ext)
	if err := copyFile(h.o.oldExe, h.oldBin); err != nil {
		return fmt.Errorf("copying the old binary: %w", err)
	}
	switch h.o.newExe {
	case "old":
		h.self = true
		h.newBin = filepath.Join(h.work, "bin", "airview-old2"+ext)
		if err := copyFile(h.o.oldExe, h.newBin); err != nil {
			return err
		}
	case "":
		if _, err := os.Stat(filepath.Join("cmd", "airview")); err != nil {
			return errors.New("run from the repository root (./cmd/airview is built), or pass -new <binary>")
		}
		h.newBin = filepath.Join(h.work, "bin", "airview-new"+ext)
		logf("building ./cmd/airview -> %s", h.newBin)
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", h.newBin, "./cmd/airview")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go build: %w", err)
		}
	default:
		h.newBin = filepath.Join(h.work, "bin", "airview-new"+ext)
		if err := copyFile(h.o.newExe, h.newBin); err != nil {
			return err
		}
	}
	logf("old: %s (sha256 %s)", h.oldBin, fileHash(h.oldBin)[:16])
	logf("new: %s (sha256 %s)%s", h.newBin, fileHash(h.newBin)[:16], map[bool]string{true: " [self-test]"}[h.self])
	return nil
}

func fileHash(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return strings.Repeat("0", 64)
	}
	defer f.Close()
	s := sha256.New()
	_, _ = io.Copy(s, f)
	return hex.EncodeToString(s.Sum(nil))
}

// fixtureMeta describes a fixture (fixture.json next to its data).
type fixtureMeta struct {
	OldSHA256 string
	Seed      uint64
	Sessions  int
	Session   string
	Retention string
	// Windows are the recording windows in Unix ms after all shifts.
	Windows [][2]int64
	// Cutoff is the time (Unix s) the fixture was completed: rows up to it
	// are history; later rows were written by the instances under test.
	Cutoff int64
	Shifts []string `json:",omitempty"`
}

// ensureFixture builds the old-format database with history (see
// history.go) unless a matching one is cached.
func (h *harness) ensureFixture(ctx context.Context, rebuild bool) error {
	key := fmt.Sprintf("fixture-%s-s%d-k%d-%s-%s", fileHash(h.oldBin)[:12], h.o.seed, h.o.sessions, h.o.session, h.o.retention)
	dir := filepath.Join(h.work, key)
	h.fixture = filepath.Join(dir, "data")
	metaPath := filepath.Join(dir, "fixture.json")
	if b, err := os.ReadFile(metaPath); err == nil && !h.o.refresh {
		if json.Unmarshal(b, &h.meta) == nil && h.meta.Cutoff > 0 {
			logf("fixture: using cached %s (completed %s)", dir, time.Unix(h.meta.Cutoff, 0).Format(time.RFC3339))
			return nil
		}
	}
	if !rebuild {
		return errors.New("no cached fixture; run the fixture phase")
	}
	if err := os.RemoveAll(dir); err != nil { // our own earlier fixture
		return err
	}
	if err := os.MkdirAll(h.fixture, 0o755); err != nil {
		return err
	}
	logf("=== phase fixture: %d session(s) of %s with the OLD build -> %s", h.o.sessions, h.o.session, dir)
	h.meta = fixtureMeta{OldSHA256: fileHash(h.oldBin), Seed: h.o.seed, Sessions: h.o.sessions, Session: h.o.session.String(), Retention: h.o.retention}
	for k := 0; k < h.o.sessions; k++ {
		fo, err := newFanout(h.o.seed+uint64(k), h.o.aircraft, 1, splitStreams)
		if err != nil {
			return err
		}
		in := &instance{Label: "fixture", Exe: h.oldBin, Dir: h.fixture, Port: h.o.port}
		if err := writeStationConfig(h.fixture, stationConfig(h.o.port, splitStreams, fo.Ports(), h.recv, h.o.retention)); err != nil {
			return err
		}
		if err := h.start(ctx, in); err != nil {
			fo.Close()
			return err
		}
		fctx, cancel := context.WithTimeout(ctx, h.o.session)
		t0 := time.Now()
		ferr := fo.Run(fctx, h.recv)
		cancel()
		fo.Close()
		t1 := time.Now()
		time.Sleep(3 * time.Second) // let the tracker hand the last frames to the store
		if err := h.stop(in); err != nil {
			return err
		}
		if ferr != nil && ctx.Err() == nil && !errors.Is(ferr, context.DeadlineExceeded) {
			return ferr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logf("fixture: session %d/%d recorded %d frames, %d bytes", k+1, h.o.sessions, fo.frames.Load(), fo.bytes.Load())
		h.meta.Windows = append(h.meta.Windows, [2]int64{t0.UnixMilli(), t1.UnixMilli()})
		if k < h.o.sessions-1 {
			for _, f := range dbFiles(h.fixture) {
				rep, err := shiftDays(ctx, f, 1)
				if err != nil {
					return fmt.Errorf("shift %s: %w", f, err)
				}
				h.meta.Shifts = append(h.meta.Shifts, fmt.Sprintf("%s: %d tables, %d kv; columns %s", filepath.Base(f), rep.Tables, rep.KV, strings.Join(rep.Columns, " ")))
			}
			for i := range h.meta.Windows {
				h.meta.Windows[i][0] -= 86400000
				h.meta.Windows[i][1] -= 86400000
			}
		}
	}
	// Leftovers of the instance that are not part of the old format.
	_ = os.Remove(filepath.Join(h.fixture, "airview.run"))
	h.meta.Cutoff = time.Now().Unix()
	b, _ := json.MarshalIndent(h.meta, "", "  ")
	return os.WriteFile(metaPath, b, 0o644)
}

// histRange is the time range (Unix ms) of the fixture's history: from the
// first recording window to the end of the next-to-last one.
func (h *harness) histRange() (int64, int64) {
	w := h.meta.Windows
	if len(w) == 0 {
		return 0, 0
	}
	from := w[0][0] - 60000
	to := w[len(w)-1][1] + 60000
	if len(w) > 1 {
		to = w[len(w)-2][1] + 60000
	}
	return from, to
}

// pair prepares two instances on fresh copies of the fixture.
func (h *harness) pair(phase string) (*instance, *instance, error) {
	base := filepath.Join(h.work, "run", phase)
	if err := os.RemoveAll(base); err != nil { // our own earlier run
		return nil, nil, err
	}
	pb := h.o.port + 1
	if !portFree(pb) {
		var err error
		if pb, err = freePort(); err != nil {
			return nil, nil, err
		}
	}
	a := &instance{Label: "old", Exe: h.oldBin, Dir: filepath.Join(base, "old", "data"), Port: h.o.port}
	b := &instance{Label: "new", Exe: h.newBin, Dir: filepath.Join(base, "new", "data"), Port: pb}
	for _, in := range []*instance{a, b} {
		if err := copyDir(h.fixture, in.Dir); err != nil {
			return nil, nil, err
		}
	}
	return a, b, nil
}

func (h *harness) start(ctx context.Context, in *instance) error {
	if !portFree(in.Port) {
		return fmt.Errorf("%s: port %d is in use", in.Label, in.Port)
	}
	if err := in.Start(); err != nil {
		return err
	}
	h.runMu.Lock()
	h.running = append(h.running, in)
	h.runMu.Unlock()
	return in.WaitHealthy(ctx, 3*time.Minute)
}

func (h *harness) stop(in *instance) error {
	err := in.Stop(2 * time.Minute)
	h.runMu.Lock()
	defer h.runMu.Unlock()
	for i, x := range h.running {
		if x == in {
			h.running = append(h.running[:i], h.running[i+1:]...)
			break
		}
	}
	return err
}

// stopAll stops every instance this run started (on errors / Ctrl+C).
func (h *harness) stopAll() {
	h.runMu.Lock()
	l := append([]*instance(nil), h.running...)
	h.runMu.Unlock()
	for _, in := range l {
		if in.PID() != 0 {
			_ = h.stop(in)
		}
	}
}

// startPair starts both instances at the same time.
func (h *harness) startPair(ctx context.Context, a, b *instance) error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, in := range []*instance{a, b} {
		wg.Add(1)
		go func(i int, in *instance) {
			defer wg.Done()
			t0 := time.Now()
			errs[i] = h.start(ctx, in)
			if in == b && errs[i] == nil {
				h.newStart = time.Since(t0)
			}
		}(i, in)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopPair stops both instances at the same time.
func (h *harness) stopPair(a, b *instance) []string {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, in := range []*instance{a, b} {
		wg.Add(1)
		go func(i int, in *instance) { defer wg.Done(); errs[i] = h.stop(in) }(i, in)
	}
	wg.Wait()
	var out []string
	for _, e := range errs {
		if e != nil {
			out = append(out, e.Error())
		}
	}
	return out
}

// compareAll calls every route on both instances and compares.
func (h *harness) compareAll(ctx context.Context, mode string, a, b *instance, r *phaseResult) {
	ph := placeholders(ctx, a, b)
	from, to := h.histRange()
	calls := buildCalls(ph, from, to)
	r.Notes = append(r.Notes, fmt.Sprintf("placeholders: live=%q flight=%q seen=%q histFlight=%q histICAO=%q", ph["live"], ph["flight"], ph["seen"], ph["histFlight"], ph["histICAO"]))
	r.Calls = len(calls)
	logf("%s: comparing %d calls (%s mode)", r.Phase, len(calls), mode)
	nonEmpty := 0
	for _, c := range calls {
		if ctx.Err() != nil {
			return
		}
		var wg sync.WaitGroup
		type resp struct {
			code int
			ct   string
			body []byte
			err  error
		}
		var ra, rb resp
		wg.Add(2)
		go func() { defer wg.Done(); ra.code, ra.ct, ra.body, ra.err = fetch(ctx, a.URL(), c.URL) }()
		go func() { defer wg.Done(); rb.code, rb.ct, rb.body, rb.err = fetch(ctx, b.URL(), c.URL) }()
		wg.Wait()
		cr := callResult{Key: c.Key, URL: c.URL, Status: [2]int{ra.code, rb.code}}
		d := &differ{mode: mode, route: c.Key, max: 12}
		switch {
		case ra.err != nil || rb.err != nil:
			d.add("", "request failed: old %v, new %v", ra.err, rb.err)
		case ra.code != rb.code:
			d.add("", "status old %d, new %d: %s", ra.code, rb.code, trunc(string(rb.body)))
		case ra.code != 200:
			// Same non-200 answer (e.g. 404 for a feature that is off):
			// the status is the comparison.
		case c.Kind == "json" || c.Kind == "geojson":
			var va, vb any
			ea := json.Unmarshal(ra.body, &va)
			eb := json.Unmarshal(rb.body, &vb)
			if ea != nil || eb != nil {
				d.add("", "invalid JSON: old %v, new %v", ea, eb)
				break
			}
			if len(ra.body) > 64 {
				nonEmpty++
			}
			if replayUntruncated(c.Route, va, vb) {
				// replay-untruncated (design §3.14): 1.0.0 ran out of its
				// point budget; the new build counts points used and answers
				// complete. Its frames must still cover the old ones.
				if msg := replayCovers(va, vb); msg != "" {
					d.add("frames", "%s", msg)
				}
				r.Notes = append(r.Notes, "replay-untruncated: "+c.URL+" was truncated in the old build and is complete in the new one")
				break
			}
			d.compareJSON("", normalize(c.Route, mode, va), normalize(c.Route, mode, vb))
		case c.Kind == "binary":
			if len(ra.body) != len(rb.body) {
				d.add("", "binary length old %d, new %d", len(ra.body), len(rb.body))
			}
		default:
			if len(ra.body) > 64 {
				nonEmpty++
			}
			d.compareText(ra.body, rb.body)
			if mode == modeStrict && c.Route == "/api/db/export/{file}" && len(d.diffs) == 0 && !bytes.Equal(ra.body, rb.body) {
				if sameLines(ra.body, rb.body) {
					d.add("order", "same rows, different row order than the old build (the export must keep the 1.0.0 order)")
				} else {
					r.Notes = append(r.Notes, "not byte-identical (within tolerance): "+c.Key)
				}
			}
		}
		if len(d.diffs) > 0 {
			cr.Diffs = d.diffs
			r.Failed = append(r.Failed, cr)
			logf("%s: DIFF %s (%s)", r.Phase, c.Key, c.URL)
			for _, x := range d.diffs {
				logf("      %s", x)
			}
		}
	}
	r.Notes = append(r.Notes, fmt.Sprintf("%d of %d responses carried data (body > 64 bytes)", nonEmpty, len(calls)))
}

// phaseMigrate: both builds start on copies of the old-format fixture
// without input; the new one migrates it. Every route (with historic range
// variants) must answer the same, strictly; after a graceful stop the rows
// up to the fixture's cutoff must be identical and every old migration
// must still be recorded.
func (h *harness) phaseMigrate(ctx context.Context) phaseResult {
	r := phaseResult{Phase: "migrate", OK: true}
	a, b, err := h.pair("migrate")
	if err != nil {
		return failed(r, err)
	}
	if err := h.writeIdleConfigs(a, b); err != nil {
		return failed(r, err)
	}
	if err := h.startPair(ctx, a, b); err != nil {
		return failed(r, err)
	}
	r.Notes = append(r.Notes, fmt.Sprintf("new build healthy %.1fs after start (DDL migrations included)", h.newStart.Seconds()))
	sleepCtx(ctx, h.o.settle)
	// The data migrations run in the background after the start; compare
	// once they are done (the readers understand both layouts meanwhile,
	// the live phase covers that).
	st := h.waitMigrated(ctx, b, h.o.migWait)
	r.Notes = append(r.Notes, st.note())
	if st.err != nil {
		r.Problems = append(r.Problems, "data migration: "+st.err.Error())
	}
	h.mig = st
	h.compareAll(ctx, modeStrict, a, b, &r)
	r.Problems = append(r.Problems, h.stopPair(a, b)...)
	h.rowCheck(ctx, a.Dir, b.Dir, &r)
	r.Problems = append(r.Problems, checkMigrations(ctx, h.fixture, b.Dir)...)
	r.Notes = append(r.Notes, "migrations old: "+strings.Join(migrationList(ctx, filepath.Join(h.fixture, "airview.db")), " "))
	r.Notes = append(r.Notes, "migrations new: "+strings.Join(migrationList(ctx, filepath.Join(b.Dir, "airview.db")), " "))
	r.OK = len(r.Failed) == 0 && len(r.Problems) == 0 && tablesOK(r.Tables)
	return r
}

// phaseCrash: the new build is killed (TerminateProcess / SIGKILL) at
// several moments of its start on a copy of the old-format fixture - spread
// over the start-up time measured in the migrate phase (database open, DDL
// migrations, start-up maintenance) - and then -kills more times at random
// moments of its background data migration (design §5.4; the window comes
// from the migrate phase). It is then started normally: the migration must
// complete, the file must pass integrity_check, every old migration must be
// recorded, every API answer must equal the old build's (strict) and the
// logical rows must equal the old build's rows.
func (h *harness) phaseCrash(ctx context.Context) phaseResult {
	r := phaseResult{Phase: "crash", OK: true}
	base := filepath.Join(h.work, "run", "crash")
	if err := os.RemoveAll(base); err != nil {
		return failed(r, err)
	}
	in := &instance{Label: "new", Exe: h.newBin, Dir: filepath.Join(base, "new", "data"), Port: h.o.port + 1}
	if !portFree(in.Port) {
		p, err := freePort()
		if err != nil {
			return failed(r, err)
		}
		in.Port = p
	}
	if err := copyDir(h.fixture, in.Dir); err != nil {
		return failed(r, err)
	}
	if err := h.writeIdleConfigs(in); err != nil {
		return failed(r, err)
	}
	kill := func(label string, d time.Duration) {
		if err := in.Start(); err != nil {
			r.Problems = append(r.Problems, "start: "+err.Error())
			return
		}
		h.runMu.Lock()
		h.running = append(h.running, in)
		h.runMu.Unlock()
		sleepCtx(ctx, d)
		pid := in.PID()
		_ = in.Kill()
		h.runMu.Lock()
		h.running = h.running[:len(h.running)-1]
		h.runMu.Unlock()
		r.Notes = append(r.Notes, fmt.Sprintf("%s: pid %d killed after %s; data migrations %s", label, pid, d.Round(time.Millisecond), migProgress(ctx, in.Dir)))
	}
	// Start-up kills.
	span := h.newStart * 6 / 5
	if span < 2*time.Second {
		span = 2 * time.Second
	}
	for i := 1; i <= h.o.kills; i++ {
		d := span * time.Duration(i) / time.Duration(h.o.kills+1)
		if d < 50*time.Millisecond {
			d = 50 * time.Millisecond
		}
		kill(fmt.Sprintf("start-up kill %d", i), d)
	}
	// Kills at random moments of the data migration.
	if lo, hi, ok := h.mig.window(); ok && !h.self {
		rng := rand.New(rand.NewPCG(h.o.seed, uint64(time.Now().UnixNano())))
		for i := 1; i <= h.o.kills; i++ {
			d := lo + time.Duration(rng.Int64N(int64(hi-lo)+1))
			kill(fmt.Sprintf("migration kill %d", i), d)
		}
	} else {
		r.Notes = append(r.Notes, "no data migration observed in the migrate phase: migration kills skipped")
	}

	// Final start: the migration completes; then the old build on its own
	// copy of the fixture for the strict comparison.
	if err := h.start(ctx, in); err != nil {
		r.Problems = append(r.Problems, "restart after the kills: "+err.Error())
		_ = h.stop(in)
		r.OK = false
		return r
	}
	st := h.waitMigrated(ctx, in, h.o.migWait)
	r.Notes = append(r.Notes, "after the kills: "+st.note())
	if st.err != nil {
		r.Problems = append(r.Problems, "data migration after the kills: "+st.err.Error())
	}
	old := &instance{Label: "old", Exe: h.oldBin, Dir: filepath.Join(base, "old", "data"), Port: h.o.port}
	if err := copyDir(h.fixture, old.Dir); err != nil {
		_ = h.stop(in)
		return failed(r, err)
	}
	if err := h.writeIdleConfigs(old); err != nil {
		_ = h.stop(in)
		return failed(r, err)
	}
	if err := h.start(ctx, old); err != nil {
		_ = h.stop(in)
		return failed(r, err)
	}
	sleepCtx(ctx, h.o.settle)
	h.compareAll(ctx, modeStrict, old, in, &r)
	r.Problems = append(r.Problems, h.stopPair(old, in)...)
	r.Problems = append(r.Problems, checkMigrations(ctx, h.fixture, in.Dir)...)
	h.rowCheck(ctx, old.Dir, in.Dir, &r)
	r.OK = len(r.Failed) == 0 && len(r.Problems) == 0 && tablesOK(r.Tables)
	return r
}

// phaseLive: both builds on copies of the fixture, fed the identical Beast
// streams in real time; then the feed stops and every route (plus the
// historic variants) is compared with the live tolerances.
func (h *harness) phaseLive(ctx context.Context) phaseResult {
	r := phaseResult{Phase: "live", OK: true}
	a, b, err := h.pair("live")
	if err != nil {
		return failed(r, err)
	}
	fo, err := newFanout(h.o.seed+1000, h.o.aircraft, 2, combinedStreams)
	if err != nil {
		return failed(r, err)
	}
	defer fo.Close()
	for _, in := range []*instance{a, b} {
		if err := writeStationConfig(in.Dir, stationConfig(in.Port, combinedStreams, fo.Ports(), h.recv, h.o.retention)); err != nil {
			return failed(r, err)
		}
	}
	if err := h.startPair(ctx, a, b); err != nil {
		return failed(r, err)
	}
	logf("live: feeding both builds for %s", h.o.live)
	fctx, cancel := context.WithTimeout(ctx, h.o.live)
	ferr := fo.Run(fctx, h.recv)
	cancel()
	if ferr != nil && ctx.Err() == nil {
		r.Problems = append(r.Problems, "feed: "+ferr.Error())
	}
	if fo.Diverged() {
		r.Problems = append(r.Problems, "feed: an instance lost or re-opened a stream; inputs diverged")
	}
	r.Notes = append(r.Notes, fmt.Sprintf("fed %d frames / %d bytes", fo.frames.Load(), fo.bytes.Load()))
	// The simulation has stopped; the connections stay open while the
	// builds are compared (a disconnect would add timing-dependent events).
	sleepCtx(ctx, 3*time.Second) // tracker -> store hand-over in both
	// visible-after (design §3.2): the new build commits the small writers'
	// rows (events, met, guess samples ...) every group-commit interval
	// instead of every 2 s; wait for one interval so database-only reads
	// see the same rows. Pages that merge the tracker's in-memory overlay
	// need no wait.
	// Both builds also write the rows of the flights and aircraft that were
	// in view every 60 s (tracker cadence, not aligned between the two
	// processes), so the wait covers one such period as well.
	w := max(groupCommit(ctx, b), liveSettle)
	r.Notes = append(r.Notes, fmt.Sprintf("visible-after: waited %s after the feed stopped", w))
	sleepCtx(ctx, w-3*time.Second)
	h.compareAll(ctx, modeLive, a, b, &r)
	r.Problems = append(r.Problems, h.stopPair(a, b)...)
	fo.Close()
	// Historic rows must be untouched by the live phase in both builds.
	// The fixture's last day is left out: its day / hour buckets receive
	// the live data (the strict migrate phase covers it).
	h.rowCheckAt(ctx, a.Dir, b.Dir, time.Unix(h.meta.Cutoff, 0).Add(-24*time.Hour), &r)
	r.OK = len(r.Failed) == 0 && len(r.Problems) == 0 && tablesOK(r.Tables)
	return r
}

// writeIdleConfigs: the instances of the strict phases get a feed address
// nobody listens on (port 1), so they receive nothing.
func (h *harness) writeIdleConfigs(ins ...*instance) error {
	for _, in := range ins {
		if err := writeStationConfig(in.Dir, stationConfig(in.Port, splitStreams, []int{1, 1}, h.recv, h.o.retention)); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) rowCheck(ctx context.Context, dirA, dirB string, r *phaseResult) {
	h.rowCheckAt(ctx, dirA, dirB, time.Unix(h.meta.Cutoff, 0), r)
}

func (h *harness) rowCheckAt(ctx context.Context, dirA, dirB string, cutoff time.Time, r *phaseResult) {
	res, err := compareDirs(ctx, dirA, dirB, cutoff)
	if err != nil {
		r.Problems = append(r.Problems, "row check: "+err.Error())
		return
	}
	r.Tables = res
	for _, t := range res {
		if len(t.Diffs) > 0 {
			logf("%s: ROWS %s/%s (%s): %s", r.Phase, t.DB, t.Table, t.Mode, strings.Join(t.Diffs, "; "))
		}
	}
}

func tablesOK(ts []tableResult) bool {
	for _, t := range ts {
		if len(t.Diffs) > 0 {
			return false
		}
	}
	return true
}

func failed(r phaseResult, err error) phaseResult {
	r.OK = false
	r.Problems = append(r.Problems, err.Error())
	logf("%s: %v", r.Phase, err)
	return r
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// report prints the summary and writes report.json; exit code 0 = all
// phases passed, 1 = a difference was found.
func (h *harness) report() (int, error) {
	ok := len(h.results) > 0
	var b strings.Builder
	fmt.Fprintf(&b, "\nAirView DB parity report  %s\nold %s\nnew %s%s\nfixture %s (%d sessions, cutoff %s)\n\n",
		time.Now().Format(time.RFC3339), h.oldBin, h.newBin, map[bool]string{true: "  [self-test]"}[h.self],
		h.fixture, len(h.meta.Windows), time.Unix(h.meta.Cutoff, 0).Format(time.RFC3339))
	for _, r := range h.results {
		ok = ok && r.OK
		rows := 0
		full, logical := 0, 0
		for _, t := range r.Tables {
			rows += int(t.RowsA)
			switch t.Mode {
			case "full":
				full++
			case "logical":
				logical++
			}
		}
		fmt.Fprintf(&b, "%-8s %s  calls %d, differing %d; tables %d (full %d, logical %d, %d rows); problems %d\n",
			r.Phase, okText(r.OK), r.Calls, len(r.Failed), len(r.Tables), full, logical, rows, len(r.Problems))
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "         - %s\n", n)
		}
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "         ! %s\n", p)
		}
		for _, c := range r.Failed {
			fmt.Fprintf(&b, "         x %s  %s  status %d/%d\n", c.Key, c.URL, c.Status[0], c.Status[1])
			for _, d := range c.Diffs {
				fmt.Fprintf(&b, "             %s\n", d)
			}
		}
		sort.Slice(r.Tables, func(i, j int) bool { return r.Tables[i].Table < r.Tables[j].Table })
		for _, t := range r.Tables {
			if len(t.Diffs) > 0 {
				fmt.Fprintf(&b, "         x rows %s/%s (%s, %d/%d rows): %s\n", t.DB, t.Table, t.Mode, t.RowsA, t.RowsB, strings.Join(t.Diffs, "; "))
			}
		}
	}
	fmt.Fprintf(&b, "\nRESULT: %s\n", okText(ok))
	fmt.Fprint(os.Stdout, b.String())
	stamp := time.Now().Format("20060102-150405")
	_ = os.WriteFile(filepath.Join(h.work, "report-"+stamp+".txt"), []byte(b.String()), 0o644)
	js, _ := json.MarshalIndent(struct {
		Old, New string
		Self     bool
		Fixture  fixtureMeta
		Phases   []phaseResult
		OK       bool
	}{h.oldBin, h.newBin, h.self, h.meta, h.results, ok}, "", "  ")
	_ = os.WriteFile(filepath.Join(h.work, "report-"+stamp+".json"), js, 0o644)
	logf("report: %s", filepath.Join(h.work, "report-"+stamp+".txt"))
	if !ok {
		return 1, nil
	}
	return 0, nil
}

// replayUntruncated: the old /api/replay answer hit its point budget
// (truncated) and the new one did not.
func replayUntruncated(route string, va, vb any) bool {
	if route != "/api/replay" {
		return false
	}
	ma, _ := va.(map[string]any)
	mb, _ := vb.(map[string]any)
	return ma != nil && mb != nil && ma["truncated"] == true && mb["truncated"] != true
}

// replayCovers checks that the complete new replay has at least the old
// frame times.
func replayCovers(va, vb any) string {
	times := func(v any) map[string]bool {
		out := map[string]bool{}
		m, _ := v.(map[string]any)
		fr, _ := m["frames"].([]any)
		for _, f := range fr {
			if fm, ok := f.(map[string]any); ok {
				out[fmt.Sprint(fm["t"])] = true
			}
		}
		return out
	}
	ta, tb := times(va), times(vb)
	missing := 0
	for k := range ta {
		if !tb[k] {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Sprintf("%d frame times of the old replay are missing in the new one", missing)
	}
	return ""
}

// sameLines reports whether two text bodies hold the same lines in a
// different order.
func sameLines(a, b []byte) bool {
	la, lb := splitLines(a), splitLines(b)
	if len(la) != len(lb) {
		return false
	}
	sort.Strings(la)
	sort.Strings(lb)
	for i := range la {
		if la[i] != lb[i] {
			return false
		}
	}
	return true
}

// liveSettle is the shortest wait between the end of the live feed and the
// comparison: one 60-s tracker flush period of both builds plus margin, and
// past the feeds' 60-s "no data" timeout, so that both builds have logged
// the feed_down / feed_up events of the idle connection before the
// comparison (at 65 s one build sometimes had and the other had not).
const liveSettle = 80 * time.Second

// groupCommit returns the new build's group-commit interval plus 2 s (0
// when the build does not report a storage profile).
func groupCommit(ctx context.Context, in *instance) time.Duration {
	var v struct {
		Profile *struct {
			FlushIntervalMs int64 `json:"flushIntervalMs"`
		} `json:"profile"`
	}
	if err := getJSON(ctx, in.URL(), "/api/db/stats", &v); err != nil || v.Profile == nil {
		return 0
	}
	return time.Duration(v.Profile.FlushIntervalMs)*time.Millisecond + 2*time.Second
}
