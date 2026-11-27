package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/mlat"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/quality"
	"airview/internal/model"
	"airview/internal/sim"
	"airview/internal/store"
)

// fakeMLAT is a populated MLAT engine view over the net fixture's stations:
// every station has a clock report whose peers are all the other stations
// (hidden ones included), events, an MLAT report, a quality report, public
// cells (one below the 5-station rule) and a CRLB map with a cell exactly
// at a station. The golden crawls run with it so that every MLAT route is
// crawled with identities and positions present in the engine's answers.
type fakeMLAT struct {
	mu    sync.Mutex
	idx   []model.StationIdx
	stats mlat.Stats
	crlb  []quality.CRLBCell
	key   *int
	// maxPeers bounds the peers per station (0 = all other stations).
	maxPeers int
}

func newFakeMLAT(f *netFixture) *fakeMLAT {
	m := &fakeMLAT{key: new(int)}
	for _, k := range []string{"A", "B", "C", "N"} {
		m.idx = append(m.idx, f.stations[k].st.Idx)
	}
	c := f.stations["C"]
	m.crlb = []quality.CRLBCell{
		{Lat: c.lat, Lon: c.lon, AltM: 914.4, SigmaHM: 40, N: 4},
		{Lat: 60, Lon: 10, AltM: 914.4, SigmaHM: 120, N: 4},
		{Lat: 60, Lon: 10.25, AltM: 914.4, SigmaHM: 400, N: 4},
		{Lat: 60, Lon: 10.5, AltM: 914.4, SigmaHM: 1500, N: 3},
		{Lat: 0, Lon: 0, AltM: 9144, SigmaHM: 30, N: 6},
	}
	m.stats = mlat.Stats{Groups: 1000, Clusters: 500, Solves: 400, SolveOK: 380, Rejected: 20, Published: 300,
		ResultsSent: 50, HoldoutFixes: 30, HoldoutTracks: 40,
		Clock: clock.Stats{Stations: 4, Components: 1, LastEpochNs: 2_500_000}}
	return m
}

func (m *fakeMLAT) Stats() mlat.Stats { m.mu.Lock(); defer m.mu.Unlock(); return m.stats }

func (m *fakeMLAT) advance(d uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.Groups += 60 * d
	m.stats.Solves += 30 * d
	m.stats.SolveOK += 27 * d
	m.stats.Published += 12 * d
}

func (m *fakeMLAT) QualityReport(windowMin int, cells bool) mlat.Quality {
	q := mlat.Quality{Fraction: 0.1, WindowMin: windowMin,
		Fix:   quality.Summary{N: 120, P50: 21.5, P95: 80.25, CrossP95: 30, Within1Sigma: 0.66},
		Track: quality.Summary{N: 200, P50: 18.25, P95: 64.5, CrossP95: 22, Within1Sigma: 0.7}}
	if cells {
		q.Cells = []quality.Cell{{Lat: -42, Lon: 174, Band: 1, N: 50, P50: 20, P95: 70, Stations: 2}}
	}
	return q
}

func (m *fakeMLAT) PublicQuality() mlat.PublicQuality {
	return mlat.PublicQuality{P50M: 18.3, P95M: 64.5, P99M: 120, Samples: 200, FixesPerMin: 42,
		Cells: []quality.Cell{
			{Lat: -42, Lon: 174, Band: 1, N: 50, P50: 20, P95: 70, Stations: 5},
			{Lat: 64, Lon: -22, Band: 0, N: 50, P50: 20, P95: 70, Stations: 3}, // must not be published
		}}
}

func (m *fakeMLAT) Station(st model.StationIdx) (mlat.StationReport, bool) {
	known := false
	var peers []clock.PeerStat
	for i, x := range m.idx {
		if x == st {
			known = true
			continue
		}
		if m.maxPeers > 0 && len(peers) >= m.maxPeers {
			continue
		}
		peers = append(peers, clock.PeerStat{Peer: x, Count: 10 + i, RMSNs: 85.25, RelPPB: 12.5})
	}
	if !known {
		return mlat.StationReport{}, false
	}
	return mlat.StationReport{PositionSource: "declared",
		Clock: &mlat.ClockReport{Class: "gps_utc", AnchorState: "anchor", Synced: true, SigmaThetaNs: 25.5, DriftPPM: 0.0012,
			BetaShortNs: 3, JitterNs: 40, OutlierPct: 1.5, Steps: 1, SyncedPeers: len(peers),
			Events: []clock.EventRecord{{T: 12.5, Reason: "promoted", Detail: "gps lock 10 min"}}, Peers: peers},
		MLAT: &quality.StationReport{ContribPerHour: 321, UsedPct: 97.5, ExcludedPct: 2.5, OutlierPct: 3, ResidualBiasNs: -12,
			WScale: 1.1, LearnedJitterNs: 44},
		TimedPerMin: 900, ResultsEligible: true}, true
}

func (m *fakeMLAT) SyncMatrix() []mlat.SyncEdge {
	var out []mlat.SyncEdge
	for _, a := range m.idx {
		for _, b := range m.idx {
			if a != b {
				out = append(out, mlat.SyncEdge{A: a, B: b, Count: 7, RMSNs: 90, RelPPB: 3})
			}
		}
	}
	return out
}

func (m *fakeMLAT) CRLBMap(quality.CRLBParams) []quality.CRLBCell { return m.crlb }
func (m *fakeMLAT) ClockOriginUnixNs() int64                      { return 1_800_000_000_000_000_000 }
func (m *fakeMLAT) Key() any                                      { return m.key }

// TestMLATConsoleRoutes checks the admin console and owner answers with a
// populated engine: shapes, units, admin-only data and the owner's view of
// peers (tags and distance bands only).
func TestMLATConsoleRoutes(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	fake := newFakeMLAT(f)
	f.srv.testMLAT = fake
	sidA := f.stations["A"].st.SID

	// History: two samples a minute apart give rates; nothing before.
	t0 := time.Now().Add(-2 * time.Minute)
	f.srv.mlatHist.sample(fake, fake.idx, t0)
	fake.advance(1)
	f.srv.mlatHist.sample(fake, fake.idx, t0.Add(time.Minute))

	var hist mlatHistoryDTO
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/mlat/history?minutes=60", ""), &hist)
	if len(hist.T) != 2 || hist.Groups[0] != nil || hist.Groups[1] == nil || *hist.Groups[1] != 60 || *hist.Solves[1] != 30 ||
		hist.TrackP95[1] != 64.5 || hist.FixN[1] != 120 || hist.Synced[1] != 4 || hist.EpochMs[1] != 2.5 || hist.WindowMin != mlatHistWindowMin {
		t.Fatalf("history: %+v", hist)
	}
	if rec := f.as(f.admin, "GET", "/api/admin/mlat/history?minutes=0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("minutes=0: %d", rec.Code)
	}

	// Station table: every station, no positions.
	rec := f.as(f.admin, "GET", "/api/admin/mlat/stations", "")
	must(t, rec, 200, "stations")
	var tab struct {
		Stations []mlatStationRowDTO
	}
	jsonOf(t, rec, &tab)
	if len(tab.Stations) != 4 || tab.Stations[0].Peers != 3 || !tab.Stations[0].Synced || tab.Stations[0].ContribPerHour != 321 {
		t.Fatalf("station table: %+v", tab.Stations)
	}
	for k, s := range f.stations {
		if strings.Contains(rec.Body.String(), trim3(s.lat)) || strings.Contains(rec.Body.String(), trim3(s.lon)) {
			t.Errorf("station table carries the position of %s", k)
		}
	}

	// Sync matrix: axis with names.
	var sync struct {
		Stations []mlatSyncStationDTO
		Edges    []mlatSyncEdgeDTO
	}
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/mlat/sync", ""), &sync)
	if len(sync.Stations) != 4 || len(sync.Edges) != 12 || sync.Stations[0].Class != "gps_utc" {
		t.Fatalf("sync: %+v", sync)
	}

	// CRLB summary.
	var crlb struct {
		Summary []crlbSummaryDTO
		Cells   []quality.CRLBCell
	}
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/mlat/crlb", ""), &crlb)
	if len(crlb.Summary) != 2 || crlb.Summary[0].Cells != 4 || crlb.Summary[1].AltM != 9144 || crlb.Summary[0].AreaKm2A == 0 ||
		crlb.Summary[0].AreaKm2B == 0 || crlb.Summary[0].AreaKm2C == 0 || crlb.Summary[0].AreaKm2D == 0 {
		t.Fatalf("crlb summary: %+v", crlb.Summary)
	}

	// Owner: tags and bands, summary, history, event times in Unix ms.
	rec = f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/mlat", "")
	must(t, rec, 200, "owner mlat")
	var own mlatStationDTO
	jsonOf(t, rec, &own)
	if own.Clock == nil || len(own.Clock.Peers) != 3 || own.Summary.Peers != 3 || own.Summary.SyncedPeers != 3 ||
		own.Summary.ContribPerHour != 321 || own.History == nil || len(own.History.T) != 2 {
		t.Fatalf("owner: %s", rec.Body.String())
	}
	for _, p := range own.Clock.Peers {
		if p.SID != "" || len(p.Tag) == 0 || p.Band != "far" {
			t.Errorf("owner peer %+v", p)
		}
	}
	if ev := own.Clock.Events; len(ev) != 1 || ev[0].T != 1_800_000_012_500 {
		t.Errorf("event time %+v", ev)
	}
	for _, k := range []string{"B", "C", "N"} {
		if s := f.stations[k]; strings.Contains(rec.Body.String(), s.st.SID) || strings.Contains(rec.Body.String(), s.name) {
			t.Errorf("owner view names station %s", k)
		}
	}
	// Results received: reported once a link carries the counter.
	if own.Summary.ResultsReceived != nil {
		t.Errorf("results reported without a counter")
	}
	n := uint64(77)
	f.links.mu.Lock()
	for i := range f.links.ls {
		if f.links.ls[i].Station == f.stations["A"].st.Idx {
			f.links.ls[i].Results = &n
		}
	}
	f.links.mu.Unlock()
	jsonOf(t, f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/mlat", ""), &own)
	if own.Summary.ResultsReceived == nil || *own.Summary.ResultsReceived != 77 {
		t.Errorf("results received %+v", own.Summary)
	}

	// Admin station view: peers by SID.
	var adm mlatStationDTO
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/stations/"+sidA+"/mlat", ""), &adm)
	if adm.Clock == nil || len(adm.Clock.Peers) != 3 || adm.Clock.Peers[0].SID == "" || adm.Clock.Peers[0].Tag != "" {
		t.Errorf("admin peers %+v", adm.Clock)
	}

	// Owners never reach the admin console.
	for _, u := range []string{"/api/admin/mlat/history", "/api/admin/mlat/stations", "/api/admin/mlat/sync", "/api/admin/mlat/crlb"} {
		if rec := f.as(f.alice, "GET", u, ""); rec.Code != http.StatusForbidden {
			t.Errorf("owner %s: %d", u, rec.Code)
		}
		if rec := f.anon("GET", u, "192.0.2.10:4000", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", u, rec.Code)
		}
	}

	// Public: quality, and only the cell with ≥ 5 stations.
	var pub struct {
		Quality *struct{ P50M, P95M float64 }
		Cells   []struct{ Lat, Lon int }
	}
	jsonOf(t, f.anon("GET", "/api/public/mlat", "192.0.2.10:4000", ""), &pub)
	if pub.Quality == nil || pub.Quality.P50M != 18.3 || len(pub.Cells) != 1 || pub.Cells[0].Lat != -42 {
		t.Errorf("public mlat %+v", pub)
	}
}

// trim3 prints a coordinate to 3 decimals (the golden scanner's precision).
func trim3(v float64) string {
	b, _ := json.Marshal(math.Trunc(v*1000) / 1000)
	return string(b)
}

// TestMLATHistoryReset: a rebuilt engine (another key, or counters going
// backwards) starts a new baseline: no rates for that point, never negative.
func TestMLATHistoryReset(t *testing.T) {
	h := newMLATHistory()
	a := &fakeMLAT{key: new(int), stats: mlat.Stats{Groups: 600}}
	t0 := time.Now()
	h.sample(a, nil, t0)
	a.advance(1)
	h.sample(a, nil, t0.Add(time.Minute))
	a.stats.Groups = 5 // counters reset
	h.sample(a, nil, t0.Add(2*time.Minute))
	b := &fakeMLAT{key: new(int), stats: mlat.Stats{Groups: 1e6}}
	h.sample(b, nil, t0.Add(3*time.Minute))
	b.advance(2)
	h.sample(b, nil, t0.Add(4*time.Minute))
	d := historyDTO(h.points(t0.Add(5*time.Minute), 60))
	want := []float64{-1, 60, -1, -1, 120}
	for i, w := range want {
		g := d.Groups[i]
		if (w < 0) != (g == nil) || g != nil && *g != w {
			t.Errorf("point %d: groups/min %v, want %v", i, g, w)
		}
	}
	// The ring keeps 24 h.
	for i := 0; i < mlatHistNet+10; i++ {
		h.sample(b, nil, t0.Add(time.Duration(10+i)*time.Minute))
	}
	if n := len(h.points(t0.Add(time.Duration(mlatHistNet+20)*time.Minute), mlatHistNet)); n != mlatHistNet {
		t.Errorf("ring holds %d points", n)
	}
}

// TestCRLBSummary checks the area per grade against a hand computation.
func TestCRLBSummary(t *testing.T) {
	cells := []quality.CRLBCell{
		{Lat: 0, Lon: 0, AltM: 100, SigmaHM: 10},
		{Lat: 60, Lon: 0, AltM: 100, SigmaHM: 10},
		{Lat: 60, Lon: 1, AltM: 100, SigmaHM: 100},
		{Lat: 60, Lon: 2, AltM: 100, SigmaHM: 600},
	}
	s := crlbSummary(cells, 0.25)
	side := 0.25 * 111.32
	wantA := side*side + side*side*0.5
	if len(s) != 1 || math.Abs(s[0].AreaKm2A-math.Round(wantA)) > 0 || math.Abs(s[0].AreaKm2B-math.Round(side*side*0.5)) > 0 ||
		s[0].AreaKm2C != 0 || s[0].AreaKm2D != math.Round(side*side*0.5) || s[0].MinSigmaM != 10 || s[0].P50SigmaM != 100 {
		t.Fatalf("summary %+v (want A %.0f)", s, wantA)
	}
}

// TestDistBand: the owner's peer distance bands at their edges.
func TestDistBand(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	a := f.stations["A"].st
	b := a
	for _, c := range []struct {
		dKm  float64
		want string
	}{{0, "near"}, {49, "near"}, {51, "mid"}, {149, "mid"}, {151, "far"}} {
		b.Effective.Lat = a.Effective.Lat + c.dKm/111.195
		b.Effective.Lon = a.Effective.Lon
		if g := distBand(a.Effective.Lat, a.Effective.Lon, &b); g != c.want {
			t.Errorf("%.0f km: %q, want %q", c.dKm, g, c.want)
		}
	}
	b.Located = false
	if g := distBand(a.Effective.Lat, a.Effective.Lon, &b); g != "" {
		t.Errorf("unlocated peer: %q", g)
	}
}

// BenchmarkMLATHistorySample: one minute sample over 1,000 stations.
func BenchmarkMLATHistorySample(b *testing.B) {
	h := newMLATHistory()
	m := &fakeMLAT{key: new(int), maxPeers: 20}
	for i := 1; i <= 1000; i++ {
		m.idx = append(m.idx, model.StationIdx(i))
	}
	t0 := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.sample(m, m.idx, t0.Add(time.Duration(i)*time.Minute))
	}
	if len(h.st) != 1000 {
		b.Fatalf("%d station rings", len(h.st))
	}
}

// mlatICAO is the Mode S-only aircraft withMLAT puts on the map.
const mlatICAO = 0x7C1A2B

// withMLAT populates the fixture's MLAT view (fake engine, two history
// samples) and puts an MLAT-tracked aircraft with grade and σ on the live
// map, so the golden crawls see every MLAT surface filled.
func (f *netFixture) withMLAT() *fakeMLAT {
	fake := newFakeMLAT(f)
	f.srv.testMLAT = fake
	t0 := time.Now().Add(-2 * time.Minute)
	f.srv.mlatHist.sample(fake, fake.idx, t0)
	fake.advance(1)
	f.srv.mlatHist.sample(fake, fake.idx, t0.Add(time.Minute))
	f.injectMLAT()
	return fake
}

// injectMLAT feeds two agreeing MLAT results of the own engine (grade B,
// σ 85 m) for mlatICAO into the tracker; the track is attributed to station
// A (as the engine attributes nothing: Station 0), never to a receiver list.
func (f *netFixture) injectMLAT() {
	// near the demo receiver (the fixture's tracker range-gates positions)
	lat, lon, alt, sig := sim.DefaultLat+0.61, sim.DefaultLon+0.73, 23000.0, 85.0
	for i := 0; i < 3; i++ {
		la, lo := lat+float64(i)*0.002, lon+float64(i)*0.002
		f.eng.Tracker().HandleUpdate(model.DecodedUpdate{Recv: time.Now(), ICAO: mlatICAO, MLAT: true, Src: model.SrcMLATAV,
			Lat: &la, Lon: &lo, AltBaro: &alt, SigmaM: &sig, Grade: model.GradeB, Msgs: 1})
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPublicMLATAircraft: the public map shows the MLAT aircraft with its
// grade and σ, marked as MLAT, and nothing about receivers.
func TestPublicMLATAircraft(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.withMLAT()
	fx := f.publicFixture()
	for _, u := range []string{"/api/public/aircraft", "/api/aircraft", "/data/aircraft.json"} {
		// The live picture is rebuilt about once a second and cached; under
		// load the first request may still get the one from before the
		// injection, so poll for up to 5 s.
		var body string
		i := -1
		for try := 0; try < 50 && i < 0; try++ {
			if try > 0 {
				time.Sleep(100 * time.Millisecond)
			}
			rec := f.anon("GET", u, "192.0.2.30:4000", "")
			must(t, rec, 200, u)
			body = rec.Body.String()
			i = strings.Index(strings.ToLower(body), "7c1a2b")
		}
		if i < 0 {
			t.Errorf("%s: MLAT aircraft missing: %.300s", u, body)
			continue
		}
		obj := body[i:min(len(body), i+700)]
		if u != "/data/aircraft.json" && (!strings.Contains(obj, `"mlatGrade":"B"`) || !strings.Contains(obj, `"mlatSigmaM":85`)) {
			t.Errorf("%s: no grade / σ: %s", u, obj)
		}
		scanOrFail(t, fx, "anonymous "+u, []byte(body))
	}
}

// edgeFake serves a fixed sync matrix.
type edgeFake struct {
	*fakeMLAT
	edges []mlat.SyncEdge
}

func (e edgeFake) SyncMatrix() []mlat.SyncEdge { return e.edges }

// TestMergedPeers: a station that serves as the reference of others gets
// those peers too (mirrored); the busier direction wins; itself never.
func TestMergedPeers(t *testing.T) {
	m := edgeFake{fakeMLAT: &fakeMLAT{key: new(int)}, edges: []mlat.SyncEdge{
		{A: 2, B: 1, Count: 9, RMSNs: 50, RelPPB: 4},  // 2 synced against 1
		{A: 3, B: 1, Count: 2, RMSNs: 70, RelPPB: -1}, // weaker than 1's own record of 3
		{A: 1, B: 1, Count: 5},                        // self: ignored
		{A: 4, B: 5, Count: 7},                        // not involving 1
	}}
	own := []clock.PeerStat{{Peer: 3, Count: 6, RMSNs: 30, RelPPB: 2}, {Peer: 6, Count: 0}}
	got := map[model.StationIdx]clock.PeerStat{}
	for _, p := range mergedPeers(m, 1, own) {
		got[p.Peer] = p
	}
	if len(got) != 2 || got[2].Count != 9 || got[2].RelPPB != -4 || got[3].Count != 6 || got[3].RelPPB != 2 {
		t.Fatalf("merged %+v", got)
	}
}

// TestMLATLongterm: hourly aggregation of the persisted minute rows and the
// summed hold-out histograms, scopes, and the owner's own-only view.
func TestMLATLongterm(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.srv.testMLAT = newFakeMLAT(f)
	st := f.eng.Store()
	sidA := f.stations["A"].st.SID
	h0 := time.Now().Add(-3*time.Hour).Unix() / 3600 * 3600
	nan := math.NaN()
	var rows []store.MLATMinute
	for m := int64(0); m < 30; m++ { // 30 minutes of hour 0
		rows = append(rows, store.MLATMinute{T: h0 + m*60, SID: "", Frames: 600, Groups: 120, Fixes: 60, Outliers: 3, Peers: 5 + m%3,
			SyncedSecs: nan, SigmaNs: nan, ResidRMSNs: nan, DriftPPM: nan})
		rows = append(rows, store.MLATMinute{T: h0 + m*60, SID: sidA, Frames: 60, Groups: 12, Fixes: 6, Outliers: 1, Peers: 3,
			SyncedSecs: 30, SigmaNs: 20 + float64(m%2)*10, ResidRMSNs: 50, DriftPPM: nan})
	}
	rows = append(rows, store.MLATMinute{T: h0 + 3600, SID: sidA, Fixes: 10, SyncedSecs: 60, SigmaNs: nan, ResidRMSNs: nan, DriftPPM: nan})
	st.AddMLATMinutes(rows)
	hist := make([]uint32, store.MLATErrBins)
	hist[store.MLATErrBin(20)] = 50 // 50 errors of 20 m
	hist[store.MLATErrBin(300)] = 50
	st.MergeMLATQuality([]store.MLATQualityInc{{T: h0, Cell: 1, AltBand: 0, Hist: hist}, {T: h0 + 120, Cell: 2, AltBand: 1, Hist: hist}})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	var net mlatLongtermDTO
	jsonOf(t, f.as(f.admin, "GET", "/api/admin/mlat/longterm?days=1", ""), &net)
	if len(net.T) != 1 || net.T[0] != h0*1000 || net.Minutes[0] != 30 || net.FixesPM[0] != 60 || net.OutliersPM[0] != 3 ||
		net.Peers[0] != 7 || net.SyncedShare[0] != nil || net.HoldN[0] != 200 || net.HoldP50 == nil || net.HoldP50[0] == nil {
		t.Fatalf("network: %+v", net)
	}
	if p50, p95 := *net.HoldP50[0], *net.HoldP95[0]; p50 < 15 || p50 > 330 || p95 < 230 || p95 > 400 {
		t.Errorf("hold-out percentiles %v / %v", p50, p95)
	}

	rec := f.as(f.alice, "GET", "/api/me/stations/"+sidA+"/mlat/longterm?days=1", "")
	must(t, rec, 200, "owner longterm")
	var own mlatLongtermDTO
	jsonOf(t, rec, &own)
	if len(own.T) != 2 || own.FixesPM[0] != 6 || own.FixesPM[1] != 10 || *own.SyncedShare[0] != 0.5 || *own.SyncedShare[1] != 1 ||
		*own.SigmaNs[0] != 25 || own.SigmaNs[1] != nil || own.HoldN != nil {
		t.Fatalf("owner: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hold") {
		t.Errorf("owner series carries network hold-out data")
	}
	if rec := f.as(f.alice, "GET", "/api/admin/mlat/longterm", ""); rec.Code != http.StatusForbidden {
		t.Errorf("owner on admin longterm: %d", rec.Code)
	}
	if rec := f.as(f.alice, "GET", "/api/me/stations/"+f.stations["B"].st.SID+"/mlat/longterm", ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign station: %d", rec.Code)
	}
	if rec := f.as(f.admin, "GET", "/api/admin/mlat/longterm?days=91", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("days=91: %d", rec.Code)
	}
}
