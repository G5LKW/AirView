package store_test

// Database growth measurement with simnet (DESIGN §20.2: "measured DB growth
// per feeder per day"). It drives the real aggregator write path: simnet
// receivers -> a minimal grouper (first copy of a transmission to
// tracker.Handle, later copies to Lanes.Credit) -> tracker lanes ->
// stations.db, plus flight_station rows into airview.db, all at data time.
//
// It is skipped unless AIRVIEW_GROWTH=1:
//
//	AIRVIEW_GROWTH=1 AIRVIEW_GROWTH_FEEDERS=100 AIRVIEW_GROWTH_MINUTES=60 \
//	AIRVIEW_GROWTH_OUT=growth-100.json go test -run TestGrowth -timeout 2h ./internal/store/
//
// Optional: AIRVIEW_GROWTH_AIRCRAFT (default scales with the area),
// AIRVIEW_GROWTH_RADIUS_KM (feeder placement radius, default keeps the
// feeder density of 100 feeders in 400 km), AIRVIEW_GROWTH_DIR (keep the
// databases there), AIRVIEW_GROWTH_REPLICATE_DAYS (materialise that many
// days of the measured rows, then run Maintain, to measure retention on
// disk).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
	"airview/internal/sim/simnet"
	"airview/internal/store"
	"airview/internal/tracker"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// growthRes resolves stations to the simnet receivers' declared positions.
type growthRes struct {
	geo []model.StationGeo
	sid []string
}

func (r *growthRes) idx(i model.StationIdx) int { return int(i) - 2 }

func (r *growthRes) Info(i model.StationIdx) (model.StationMeta, bool) {
	k := r.idx(i)
	if k < 0 || k >= len(r.sid) {
		return model.StationMeta{}, false
	}
	return model.StationMeta{Idx: i, SID: r.sid[k], Located: true}, true
}

func (r *growthRes) Geo(i model.StationIdx) (model.StationGeo, bool) {
	k := r.idx(i)
	if k < 0 || k >= len(r.geo) {
		return model.StationGeo{}, false
	}
	return r.geo[k], true
}

// grouper is the minimal ingest of the measurement: the observations of one
// transmission arrive together (simnet merges by arrival), the first one is
// handled by the tracker, the others are credited to their lanes.
type grouper struct {
	n       *simnet.Network
	tr      *tracker.Tracker
	lanes   *tracker.Lanes
	results map[uint64]tracker.Result
	order   []uint64
	frames  uint64
	copies  uint64
	lastMs  int64
	nextTk  int64
	// Distinct (station, aircraft) pairs of the run: the flight_station
	// estimate (a station hears a typical flight for an hour or more).
	pairs map[uint64]struct{}
}

func (g *grouper) pair(rx int, icao uint32, _ int64) {
	g.pairs[uint64(rx)<<32|uint64(icao)] = struct{}{}
}

func (g *grouper) Observe(obs []simnet.Observation) {
	for i := range obs {
		o := &obs[i]
		g.copies++
		if r, ok := g.results[o.Seq]; ok {
			g.lanes.Credit(model.StationIdx(o.Rx+2), -1, o.SignalDBFS, &r)
			if r.Status == tracker.ResultOK && r.Key != 0 {
				g.pair(o.Rx, r.Key, o.HostNs/1e6)
			}
			continue
		}
		f := g.n.RawFrame(o)
		r := g.tr.Handle(f)
		if r.Status == tracker.ResultOK && r.Key != 0 {
			g.pair(o.Rx, r.Key, o.HostNs/1e6)
		}
		g.frames++
		g.results[o.Seq] = r
		g.order = append(g.order, o.Seq)
		if ms := o.HostNs / 1e6; ms > g.lastMs {
			g.lastMs = ms
		}
	}
	// Results are needed for about a second (the grouper window).
	if len(g.order) > 200_000 {
		for _, s := range g.order[:100_000] {
			delete(g.results, s)
		}
		g.order = append(g.order[:0], g.order[100_000:]...)
	}
}

func (g *grouper) Advance(h float64) {
	ms := g.n.UnixNs(h) / 1e6
	if ms >= g.nextTk {
		g.lanes.Tick(ms)
		g.nextTk = ms + 1000
	}
}

// GrowthReport is what TestGrowth measures (written as JSON to
// AIRVIEW_GROWTH_OUT).
type GrowthReport struct {
	Feeders, Aircraft    int
	RadiusKm             float64
	SimMinutes           int
	WallSeconds          float64
	UniqueFrames, Copies uint64
	// FlightStationRowsPerFeederDay: estimated flight_station rows (see
	// TestGrowth); the flight_station table measured below holds one
	// synthetic day of them.
	FlightStationRowsPerFeederDay float64
	ActiveStations                int
	StationsDBBytes               int64
	MainDBBytes                   int64
	Tables                        map[string]TableGrowth
	PerFeederDay                  map[string]float64 // extrapolated bytes per feeder per day, per table
	PerFeederDayTotal             float64
	Retention                     map[string]float64 // steady-state bytes per feeder under DefaultTierPolicy
	RetentionTotal                float64
	Replicated                    *ReplicaReport `json:",omitempty"`
	StoreStats                    store.Stats
	PeakHeapMB                    float64
}

// TableGrowth is the measured content of one table.
type TableGrowth struct {
	Rows        int64
	Bytes       int64
	BytesPerRow float64
	Keys        int64 // distinct ant / sid keys
}

// ReplicaReport is the on-disk check of the retention model.
type ReplicaReport struct {
	Days          int
	BeforeBytes   int64 // pages in use before Maintain (dbstat)
	AfterBytes    int64 // pages in use after Maintain + VACUUM (dbstat)
	FileBytes     int64 // stations.db file after VACUUM
	Maintain      store.MaintReport
	MaintainTook  float64
	Tables        map[string]int64
	BytesPerFeed  float64
	MaintainCalls int
}

func TestGrowth(t *testing.T) {
	if os.Getenv("AIRVIEW_GROWTH") != "1" {
		t.Skip("set AIRVIEW_GROWTH=1 to run the database growth measurement")
	}
	feeders := envInt("AIRVIEW_GROWTH_FEEDERS", 100)
	minutes := envInt("AIRVIEW_GROWTH_MINUTES", 30)
	radius := float64(envInt("AIRVIEW_GROWTH_RADIUS_KM", int(400*math.Sqrt(float64(feeders)/100))))
	aircraft := envInt("AIRVIEW_GROWTH_AIRCRAFT", min(simnet.MaxAircraft, int(800*(radius/400)*(radius/400))))
	dir := os.Getenv("AIRVIEW_GROWTH_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	ctx := context.Background()

	specs := simnet.RandomReceivers(7, feeders, simnet.Placement{CenterLat: 51, CenterLon: 6, RadiusKm: radius})
	// Data time ends now, so that Maintain (wall clock) sees recent data.
	simStart := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute).Truncate(time.Minute)
	n, err := simnet.New(simnet.Config{Seed: 7, Start: simStart, Aircraft: aircraft, Receivers: specs, Diurnal: true})
	if err != nil {
		t.Fatal(err)
	}
	res := &growthRes{}
	for i, s := range n.Config().Receivers {
		res.geo = append(res.geo, model.StationGeo{Lat: s.Lat, Lon: s.Lon, HAE: s.GroundHAE + s.AntennaAGL, SigmaM: 10, Source: model.LocSrcManual})
		res.sid = append(res.sid, fmt.Sprintf("S%05d", i))
	}

	// The simulation runs many times faster than real time: give the queue
	// room so that no row is shed (StoreStats.Dropped* must stay 0, else
	// the measurement undercounts).
	opts := store.Options{FlushInterval: 2 * time.Second, MaxQueueRows: 100_000_000}
	stations, err := store.OpenStations(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer stations.Close()
	mainDB, err := store.OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer mainDB.Close()

	var fsMu sync.Mutex
	var fsRows int
	lanes := tracker.NewLanes(tracker.LaneOptions{
		Resolver: res, Store: stations,
		OnFlightStation: func(rows []tracker.FlightStation) {
			conv := make([]store.FlightStation, len(rows))
			for i, r := range rows {
				conv[i] = store.FlightStation{FlightID: r.FlightID, SID: r.SID, ICAO: r.ICAO, First: r.First, Last: r.Last,
					Msgs: r.Msgs, Positions: r.Positions, MaxDistKm: r.MaxDistKm, MinDistKm: r.MinDistKm,
					RSSIMin: r.RSSIMin, RSSIMax: r.RSSIMax, RSSIAvg: r.RSSIAvg}
			}
			fsMu.Lock()
			fsRows += len(conv)
			fsMu.Unlock()
			mainDB.UpsertFlightStation(conv)
		},
	})
	cfg := model.DefaultConfig()
	tr := tracker.New(tracker.Options{Config: &cfg})
	tr.SetNetwork(res, lanes)
	g := &grouper{n: n, tr: tr, lanes: lanes, results: map[uint64]tracker.Result{}, pairs: map[uint64]struct{}{}}
	n.AddSink(g)

	start := time.Now()
	var peakHeap uint64
	for m := 0; m < minutes; m++ {
		if err := n.Advance(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		peakHeap = max(peakHeap, ms.HeapAlloc)
		if m%10 == 9 {
			t.Logf("sim minute %d: %d frames, %d copies, %.0f s wall", m+1, g.frames, g.copies, time.Since(start).Seconds())
		}
	}
	lanes.FlushAll(g.lastMs)
	if err := stations.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := stations.RollupHours(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mainDB.Flush(); err != nil {
		t.Fatal(err)
	}
	wall := time.Since(start)
	if err := stations.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}

	rep := GrowthReport{Feeders: feeders, Aircraft: aircraft, RadiusKm: radius, SimMinutes: minutes,
		WallSeconds: wall.Seconds(), UniqueFrames: g.frames, Copies: g.copies, Tables: map[string]TableGrowth{},
		StoreStats: stations.Stats(), PeakHeapMB: float64(peakHeap) / (1 << 20)}
	if st := rep.StoreStats; st.DroppedLow+st.DroppedMid+st.DroppedHigh > 0 {
		t.Errorf("rows were shed by the write queue, the measurement is incomplete: %+v", st)
	}
	rep.StationsDBBytes = rep.StoreStats.DBBytes
	rep.MainDBBytes = mainDB.Stats().DBBytes
	measure := func(st *store.Store, keyCol map[string]string) {
		sizes, err := st.TableSizes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, z := range sizes {
			col, ok := keyCol[z.Name]
			if !ok {
				continue
			}
			var rows, keys int64
			if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT `+col+`) FROM `+z.Name).Scan(&rows, &keys); err != nil {
				t.Fatal(err)
			}
			tg := TableGrowth{Rows: rows, Bytes: z.Bytes, Keys: keys}
			if rows > 0 {
				tg.BytesPerRow = float64(z.Bytes) / float64(rows)
			}
			rep.Tables[z.Name] = tg
		}
	}
	measure(stations, map[string]string{"stats_minute": "ant", "stats_hour": "ant", "range_hour": "ant", "coverage": "ant",
		"signal_hist": "ant", "elev_hist": "ant", "heat": "ant"})
	// flight_station: flights close only after a 30-minute gap, so a short
	// run writes few rows. Estimate the rows as the (station, aircraft)
	// pairs per hour (a station hears a typical flight for about an hour)
	// and measure their size by writing one day of such rows.
	// Runs shorter than an hour count the pairs of the run as one hour.
	pairsPerHour := float64(len(g.pairs)) / math.Max(1, float64(minutes)/60)
	rep.FlightStationRowsPerFeederDay = pairsPerHour * 24 / float64(max(feeders, 1))
	if rows := int(pairsPerHour * 24); rows > 0 {
		rows = min(rows, 3_000_000)
		fs := make([]store.FlightStation, 0, 10000)
		now := time.Now().Unix()
		for i := 0; i < rows; i++ {
			fs = append(fs, store.FlightStation{FlightID: int64(1_000_000 + i/max(feeders/10, 1)), SID: res.sid[i%feeders],
				ICAO: uint32(0x400000 + i%5000), First: now - int64(i%86400), Last: now - int64(i%86400) + 1800,
				Msgs: int64(200 + i%800), Positions: int64(20 + i%80), MaxDistKm: 150 + float64(i%200), MinDistKm: 20 + float64(i%50),
				RSSIMin: -30, RSSIMax: -5, RSSIAvg: -18.5})
			if len(fs) == cap(fs) {
				mainDB.UpsertFlightStation(fs)
				fs = fs[:0]
			}
		}
		mainDB.UpsertFlightStation(fs)
		if err := mainDB.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	measure(mainDB, map[string]string{"flight_station": "sid"})
	if st, ok := rep.Tables["stats_minute"]; ok {
		rep.ActiveStations = int(st.Keys)
	}
	extrapolate(&rep)

	if days := envInt("AIRVIEW_GROWTH_REPLICATE_DAYS", 0); days > 0 {
		rep.Replicated = replicate(t, stations, days, minutes, rep.ActiveStations)
	}

	b, _ := json.MarshalIndent(rep, "", "  ")
	t.Logf("growth report:\n%s", b)
	if out := os.Getenv("AIRVIEW_GROWTH_OUT"); out != "" {
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// extrapolate turns the measured rows into bytes per feeder per day.
//
//   - minute and hour tables grow linearly: rows per active key per
//     simulated minute (hour) times 1440 (24);
//   - the per-day histogram tables (coverage, signal_hist, elev_hist)
//     fill their bins during the day: the measured distinct rows are scaled
//     by sqrt(day / simulated time), bounded by the bin space - a rough
//     model, use a 24 h run (AIRVIEW_GROWTH_MINUTES=1440) for real numbers;
//   - heat is all-time (no day dimension): it saturates at the cells a
//     station can hear, so its per-day growth is reported as the measured
//     size (an upper bound for day 1) and in Retention as the saturated grid.
func extrapolate(rep *GrowthReport) {
	act := float64(max(rep.ActiveStations, 1))
	simMin := float64(rep.SimMinutes)
	rep.PerFeederDay = map[string]float64{}
	rep.Retention = map[string]float64{}
	perRow := func(name string) float64 { return rep.Tables[name].BytesPerRow }
	day := 0.0
	if tg, ok := rep.Tables["stats_minute"]; ok {
		v := float64(tg.Rows) / act / simMin * 1440 * perRow("stats_minute")
		rep.PerFeederDay["stats_minute"] = v
		day += v
	}
	for _, name := range []string{"stats_hour", "range_hour"} {
		if tg, ok := rep.Tables[name]; ok {
			hours := math.Max(1, math.Ceil(simMin/60))
			v := float64(tg.Rows) / act / hours * 24 * perRow(name)
			rep.PerFeederDay[name] = v
			day += v
		}
	}
	binSpace := map[string]float64{"coverage": 180 * 7, "signal_hist": 100 * 60, "elev_hist": 184}
	for name, space := range binSpace {
		if tg, ok := rep.Tables[name]; ok {
			rowsPerKey := float64(tg.Rows) / act
			scaled := math.Min(space, rowsPerKey*math.Sqrt(1440/simMin))
			v := scaled * perRow(name)
			rep.PerFeederDay[name] = v
			day += v
		}
	}
	if tg, ok := rep.Tables["flight_station"]; ok && tg.Rows > 0 {
		v := rep.FlightStationRowsPerFeederDay * perRow("flight_station")
		rep.PerFeederDay["flight_station"] = v
		day += v
	}
	if tg, ok := rep.Tables["heat"]; ok {
		rep.PerFeederDay["heat(first day, saturates)"] = float64(tg.Bytes) / act
	}
	rep.PerFeederDayTotal = day

	// Steady state under DefaultTierPolicy: minute 30 d, hour 395 d, day
	// rows forever (~1 per key per day; counted for 395 d here), histograms
	// 35 days per day + 12 month buckets (~ a saturated day each), heat
	// saturated (taken as 4x the first-run grid).
	p := store.DefaultTierPolicy()
	rep.Retention["stats_minute"] = rep.PerFeederDay["stats_minute"] * float64(p.MinuteDays)
	rep.Retention["stats_hour"] = rep.PerFeederDay["stats_hour"] * float64(p.HourDays)
	rep.Retention["range_hour"] = rep.PerFeederDay["range_hour"] * float64(p.HourDays)
	rep.Retention["stats_day"] = perRow("stats_hour") * float64(p.HourDays)
	for name, space := range binSpace {
		monthRows := math.Min(space, rep.PerFeederDay[name]/math.Max(perRow(name), 1)*3)
		rep.Retention[name] = rep.PerFeederDay[name]*float64(p.HistDailyDays) + 12*monthRows*perRow(name)
	}
	// flight_station lives in airview.db and follows the flights retention.
	rep.Retention["flight_station (airview.db)"] = rep.PerFeederDay["flight_station"] * float64(p.FlightStationDays)
	rep.Retention["heat(saturated, est.)"] = 4 * rep.PerFeederDay["heat(first day, saturates)"]
	for _, v := range rep.Retention {
		rep.RetentionTotal += v
	}
}

// replicate materialises a long-running network from the measured window:
// the window of minute rows (and of hour rows) is copied backwards until
// it fills a whole day, that day is copied to the `days`-1 days before it
// (minute, hour and per-day histogram tables), then Maintain runs with the
// default tiers and the file is vacuumed to see the live data. It reports
// the file size before and after and how long Maintain took.
func replicate(t *testing.T, st *store.Store, days, minutes, active int) *ReplicaReport {
	ctx := context.Background()
	rr := &ReplicaReport{Days: days, Tables: map[string]int64{}}
	var t1 int64
	if err := st.DB().QueryRowContext(ctx, `SELECT MAX(t) + 60 FROM stats_minute`).Scan(&t1); err != nil {
		t.Fatal(err)
	}
	w := int64(minutes) * 60
	wh := int64(math.Ceil(float64(minutes)/60)) * 3600
	t0 := t1 - w
	h1 := t1 - t1%3600 + 3600
	h0 := h1 - wh
	minuteCols := `ant, frames, df0, df4, df5, df11, df16, df17, df18, df20, df21, df_other, crc_bad, positions,
  mlat_positions, aircraft, aircraft_pos, adsb, mlat, modes, max_km, rssi_avg, rssi_min, rssi_max, secs`
	rangeCols := `ant, max_km, p99_km, p95_km, p90_km, p50_km, positions, aircraft, far_count, resid_db, resid_n, dist_hist, sector_max`
	exec := func(q string, a ...any) {
		t.Helper()
		if err := st.Write(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, q, a...); return err }); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// 1. Fill the day ending at t1.
	for k := int64(1); k*w < 86400; k++ {
		exec(`INSERT OR IGNORE INTO stats_minute SELECT t - ?, `+minuteCols+` FROM stats_minute WHERE t >= ? AND t < ?`, k*w, t0, t1)
	}
	for k := int64(1); k*wh < 86400; k++ {
		exec(`INSERT OR IGNORE INTO range_hour SELECT t - ?, `+rangeCols+` FROM range_hour WHERE t >= ? AND t < ?`, k*wh, h0, h1)
	}
	// 2. Copy that day to the days before it.
	d1 := t1 - 86400
	var day int64
	if err := st.DB().QueryRowContext(ctx, `SELECT MAX(day) FROM coverage`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	for d := int64(1); d < int64(days); d++ {
		exec(`INSERT OR IGNORE INTO stats_minute SELECT t - ?, `+minuteCols+` FROM stats_minute WHERE t >= ? AND t < ?`, d*86400, d1, t1)
		exec(`INSERT OR IGNORE INTO range_hour SELECT t - ?, `+rangeCols+` FROM range_hour WHERE t >= ? AND t < ?`, d*86400, h1-86400, h1)
		exec(`INSERT OR IGNORE INTO coverage SELECT day - ?, ant, brg_bin, alt_band, max_km, count, rssi_sum, rssi_n FROM coverage WHERE day = ?`, d, day)
		exec(`INSERT OR IGNORE INTO signal_hist SELECT day - ?, ant, dist_bin, rssi_bin, count FROM signal_hist WHERE day = ?`, d, day)
		exec(`INSERT OR IGNORE INTO elev_hist SELECT day - ?, ant, elev_bin, count, rssi_sum, resid_sum FROM elev_hist WHERE day = ?`, d, day)
	}
	// The copies bypassed the writer's rollup bookkeeping: roll up from
	// scratch.
	if err := st.KVDelete(ctx, "store.rollup_hour"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RollupHours(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	rr.BeforeBytes = sumSizes(t, st)
	start := time.Now()
	mrep, err := st.Maintain(ctx, store.DefaultTierPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rr.Maintain = mrep
	rr.MaintainCalls = 1
	rr.MaintainTook = time.Since(start).Seconds()
	if err := st.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	rr.AfterBytes = sumSizes(t, st)
	rr.FileBytes = st.Stats().DBBytes
	sizes, _ := st.TableSizes(ctx)
	sort.Slice(sizes, func(i, j int) bool { return sizes[i].Bytes > sizes[j].Bytes })
	for _, z := range sizes {
		rr.Tables[z.Name] = z.Bytes
	}
	rr.BytesPerFeed = float64(rr.AfterBytes) / float64(max(active, 1))
	return rr
}

// sumSizes is the space in use by all tables (dbstat), independent of WAL
// state and free pages.
func sumSizes(t *testing.T, st *store.Store) int64 {
	t.Helper()
	sizes, err := st.TableSizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, z := range sizes {
		n += z.Bytes
	}
	return n
}
