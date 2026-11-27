package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/config"
	"airview/internal/hub"
	"airview/internal/model"
	"airview/internal/refdata"
	"airview/internal/sim"
	"airview/internal/store"
	"airview/internal/weather"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func withPositions(e *Engine) int {
	n := 0
	for _, a := range e.Tracker().Snapshot().Aircraft {
		if a.Lat != nil && a.Lon != nil {
			n++
		}
	}
	return n
}

// settledGoroutines waits for the goroutine count to stop falling.
func settledGoroutines(limit int) int {
	n := runtime.NumGoroutine()
	for i := 0; i < 60 && n > limit; i++ {
		time.Sleep(50 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

func TestDemoLifecycle(t *testing.T) {
	dir := t.TempDir()
	before := runtime.NumGoroutine()

	e := New(dir, Options{Demo: true, Offline: true, DemoSeed: 42, Logger: quietLogger()})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Start(context.Background()); err != ErrRunning {
		t.Fatalf("second Start = %v, want ErrRunning", err)
	}
	notices, cancelNotices := e.Notices()
	defer cancelNotices()

	if !e.Demo() || e.Mode() != model.ModeStandalone {
		t.Fatalf("demo=%v mode=%q", e.Demo(), e.Mode())
	}
	// The stored configuration stays unlocated; the effective one uses the
	// simulator's site.
	if e.Config().Get().Receiver.Located {
		t.Fatal("stored config became located")
	}
	eff := e.EffectiveConfig()
	if !eff.Receiver.Located || eff.Receiver.Lat != sim.DefaultLat || eff.Receiver.Lon != sim.DefaultLon {
		t.Fatalf("effective receiver = %+v", eff.Receiver)
	}

	waitFor(t, "aircraft with positions", 30*time.Second, func() bool { return withPositions(e) >= 3 })

	st := e.Status()
	if !st.Demo || !st.Located || st.Counts.Aircraft == 0 || st.App != model.AppName || st.DataDir != dir {
		t.Fatalf("status = %+v", st)
	}
	if _, err := json.Marshal(st); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	var demoFeed *model.FeedStatus
	for i := range st.Feeds {
		if st.Feeds[i].ID == DemoFeedID {
			demoFeed = &st.Feeds[i]
		} else if st.Feeds[i].Enabled {
			t.Errorf("configured feed %q is still enabled in demo mode", st.Feeds[i].ID)
		}
	}
	if demoFeed == nil || !demoFeed.Connected || demoFeed.Frames == 0 {
		t.Fatalf("demo feed status = %+v", demoFeed)
	}

	// The feed watcher announces the simulator.
	waitFor(t, "feed_up notice", 5*time.Second, func() bool {
		for {
			select {
			case n := <-notices:
				if n.Kind == "feed_up" {
					return true
				}
			default:
				return false
			}
		}
	})

	// Live configuration change: hub on (ephemeral-ish port), TTL, HTTP port.
	cfg := e.Config().Get()
	cfg.Hub.Enabled = true
	cfg.Hub.Port = 39154
	cfg.Hub.Password = "secret"
	cfg.UI.AircraftTTLs = 45
	cfg.Receiver.Name = "Test Station"
	next, _, err := e.ApplyConfig(cfg)
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if next.Receiver.Name != "Test Station" || !next.Hub.Enabled {
		t.Fatalf("stored config = %+v", next)
	}
	if e.RestartRequired() {
		t.Fatal("restart required without an HTTP change")
	}
	if hs := e.HubStatus(); !hs.Enabled || hs.Port != 39154 {
		t.Fatalf("hub status = %+v (err %q)", hs, e.HubError())
	}
	// A hub client receives the station and frames.
	gotStation := make(chan hub.Station, 1)
	cctx, ccancel := context.WithCancel(context.Background())
	cl := hub.NewClient(hub.ClientOptions{Host: "127.0.0.1", Port: 39154, Password: "secret",
		Logger: quietLogger(),
		OnStation: func(s hub.Station) {
			select {
			case gotStation <- s:
			default:
			}
		}})
	sink := &countSink{}
	clDone := make(chan struct{})
	go func() { defer close(clDone); _ = cl.Run(cctx, sink) }()
	select {
	case s := <-gotStation:
		if s.Receiver.Name != "Test Station" || len(s.Antennas) != len(next.Antennas) {
			t.Errorf("hub station = %+v", s)
		}
	case <-time.After(10 * time.Second):
		t.Error("no station from the hub")
	}
	waitFor(t, "frames over the hub", 10*time.Second, func() bool { return sink.frames.Load() > 20 })
	ccancel()
	<-clDone

	// The saved file has the change; the demo site is not in it.
	saved, err := config.Load(dir)
	if err != nil || saved.Receiver.Name != "Test Station" || saved.Receiver.Located || saved.UI.AircraftTTLs != 45 {
		t.Fatalf("saved config = %+v, err %v", saved.Receiver, err)
	}

	// HTTP port change -> restart required; hub off again.
	cfg = e.Config().Get()
	cfg.HTTP.Port = 8123
	cfg.Hub.Enabled = false
	if _, _, err := e.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !e.RestartRequired() {
		t.Fatal("HTTP port change did not set RestartRequired")
	}
	if e.Hub() != nil || e.HubStatus().Enabled {
		t.Fatal("hub still running")
	}
	if withPositions(e) == 0 {
		t.Fatal("aircraft lost by a configuration change")
	}

	// Rows reach the database on Flush.
	if err := e.Tracker().Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if rows, total, err := e.Store().ListAircraft(context.Background(), store.AircraftQuery{}); err != nil || total == 0 || len(rows) == 0 {
		t.Fatalf("aircraft rows: %d, total %d, err %v", len(rows), total, err)
	}

	// Factory reset.
	if err := e.FactoryReset(); err != nil {
		t.Fatalf("FactoryReset: %v", err)
	}
	if got := e.Config().Get(); !config.Equal(got, model.DefaultConfig()) {
		t.Fatalf("config after reset = %+v", got)
	}
	if e.RestartRequired() {
		t.Fatal("RestartRequired still set after the reset restored the port")
	}
	if _, total, err := e.Store().ListAircraft(context.Background(), store.AircraftQuery{}); err != nil {
		t.Fatalf("ListAircraft after reset: %v", err)
	} else if total > 0 && e.Tracker().Counters().Frames == 0 {
		t.Fatalf("database not wiped: %d aircraft", total)
	}
	// The -demo flag stays in force: traffic comes back.
	waitFor(t, "aircraft after reset", 30*time.Second, func() bool { return withPositions(e) >= 3 })

	e.Stop()
	e.Stop() // idempotent
	if _, _, err := e.ApplyConfig(model.DefaultConfig()); err != ErrNotRunning {
		t.Fatalf("ApplyConfig after Stop = %v", err)
	}
	if _, ok := <-notices; ok {
		// Drain: the channel must end up closed.
		for range notices {
		}
	}

	after := settledGoroutines(before + 3)
	if after > before+3 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutine leak: %d before, %d after\n%s", before, after, buf)
	}
}

type countSink struct {
	frames atomic.Int64
}

func (c *countSink) HandleFrame(model.RawFrame)       { c.frames.Add(1) }
func (c *countSink) HandleUpdate(model.DecodedUpdate) {}

func TestEffectiveConfig(t *testing.T) {
	base := model.ExampleRadarcapeConfig()
	base.Receiver.Located = true
	base.Receiver.Lat, base.Receiver.Lon, base.Receiver.GroundElevM = 48.1, 11.5, 520

	t.Run("standalone", func(t *testing.T) {
		e := New(t.TempDir(), Options{Logger: quietLogger()})
		eff := e.computeEffective(base)
		if eff.demo || eff.mode != model.ModeStandalone || len(eff.feeds) != 1 || !eff.feeds[0].Enabled {
			t.Fatalf("%+v", eff)
		}
	})
	t.Run("demo keeps a real location", func(t *testing.T) {
		c := config.Clone(base)
		c.Demo.Enabled = true
		e := New(t.TempDir(), Options{Logger: quietLogger()})
		eff := e.computeEffective(c)
		if !eff.demo || eff.cfg.Receiver.Lat != 48.1 || len(eff.feeds) != 2 {
			t.Fatalf("%+v", eff)
		}
		if eff.feeds[0].Enabled || eff.feeds[1].ID != DemoFeedID || eff.feeds[1].Type != model.FeedDemo {
			t.Fatalf("feeds %+v", eff.feeds)
		}
		// A different antenna set or aircraft count restarts the simulator.
		c2 := config.Clone(c)
		c2.Antennas[1].AzimuthDeg = 123
		if e.computeEffective(c2).feeds[1] == eff.feeds[1] {
			t.Fatal("demo feed definition did not change with the antennas")
		}
	})
	t.Run("demo beyond 70 degrees uses the simulator site", func(t *testing.T) {
		c := config.Clone(base)
		c.Receiver.Lat = 78
		e := New(t.TempDir(), Options{Demo: true, Logger: quietLogger()})
		eff := e.computeEffective(c)
		if eff.cfg.Receiver.Lat != sim.DefaultLat || eff.cfg.Receiver.GroundElevM != sim.DefaultGroundElevM {
			t.Fatalf("%+v", eff.cfg.Receiver)
		}
	})
	t.Run("remote modes need permission", func(t *testing.T) {
		c := config.Clone(base)
		c.Mode = model.ModeRemoteData
		c.Remote.Host = "pi.local"
		e := New(t.TempDir(), Options{Logger: quietLogger()})
		if eff := e.computeEffective(c); eff.mode != model.ModeStandalone || eff.feeds[0].ID != "radarcape" {
			t.Fatalf("%+v", eff)
		}
		c.Mode = model.ModeRemoteWeb
		if eff := e.computeEffective(c); eff.mode != model.ModeStandalone {
			t.Fatalf("%+v", eff)
		}
	})
	t.Run("remote-data adopts the host station", func(t *testing.T) {
		c := config.Clone(base)
		c.Mode = model.ModeRemoteData
		c.Remote.Host = "pi.local"
		c.Remote.DataPort = 30999
		c.Demo.Enabled = true // ignored outside standalone
		e := New(t.TempDir(), Options{AllowRemoteData: true, AllowRemoteWeb: true, Logger: quietLogger()})
		eff := e.computeEffective(c)
		if eff.demo || eff.mode != model.ModeRemoteData || len(eff.feeds) != 1 {
			t.Fatalf("%+v", eff)
		}
		f := eff.feeds[0]
		if f.Type != model.FeedHub || f.Host != "pi.local" || f.Port != 30999 || !f.Enabled {
			t.Fatalf("hub feed %+v", f)
		}
		if eff.cfg.Receiver.Lat != 48.1 {
			t.Fatal("receiver changed without a host station")
		}
		e.hostStation = &hub.Station{
			Receiver: model.Receiver{Name: "Pi", Located: true, Lat: 52, Lon: 5, GroundElevM: 3},
			Antennas: []model.Antenna{{ID: "omni", Type: model.AntVertical}},
			Feeds:    []hub.StationFeed{{ID: "rc-yagi", Name: "Yagi", Type: model.FeedBeast, AntennaID: "omni"}},
		}
		eff = e.computeEffective(c)
		if eff.cfg.Receiver.Lat != 52 || len(eff.cfg.Antennas) != 1 || len(eff.feeds) != 1 {
			t.Fatalf("%+v", eff)
		}
		if len(eff.cfg.Feeds) != 2 || eff.cfg.Feeds[1].ID != "rc-yagi" || eff.cfg.Feeds[1].AntennaID != "omni" {
			t.Fatalf("tracker feeds %+v", eff.cfg.Feeds)
		}
		c.Remote.UseHostReceiver = false
		if eff = e.computeEffective(c); eff.cfg.Receiver.Lat != 48.1 {
			t.Fatal("host receiver adopted although UseHostReceiver is off")
		}
		// remote-web: everything suspended.
		c.Mode = model.ModeRemoteWeb
		eff = e.computeEffective(c)
		if eff.mode != model.ModeRemoteWeb || len(eff.feeds) != 1 || eff.feeds[0].Enabled {
			t.Fatalf("%+v", eff)
		}
		// The -demo flag wins over a remote mode.
		e.opts.Demo = true
		if eff = e.computeEffective(c); !eff.demo || eff.mode != model.ModeStandalone {
			t.Fatalf("%+v", eff)
		}
	})
}

// synthetic bundle: `past` analysis hours up to the current hour, `future`
// forecast hours, with a pressure-level profile that has a strong surface
// inversion (super-refraction).
func testBundle(now time.Time, past, future int) *weather.Bundle {
	hour := now.Truncate(time.Hour)
	b := &weather.Bundle{Source: weather.Source, Kind: weather.KindForecast, Lat: 50, Lon: 8, Fetched: now.UnixMilli()}
	for i := -past + 1; i <= future; i++ {
		var h weather.HourPoint
		raw, _ := json.Marshal(map[string]any{"t": hour.Add(time.Duration(i) * time.Hour).UnixMilli()})
		_ = json.Unmarshal(raw, &h) // every Num starts as NaN
		h.T = hour.Add(time.Duration(i) * time.Hour).UnixMilli()
		h.TempC, h.RH, h.DewPointC = 12+weather.Num(i)*0.1, 80, 8.6
		h.PressureMSL, h.SurfacePressure = 1018, 1005
		h.WindKmh, h.WindDir, h.Cloud = 9, 240, 40
		h.Levels = []weather.Level{
			{HPa: 1000, TempC: 12, RH: 85, HeightM: 150, WindKmh: 10, WindDir: 240},
			{HPa: 975, TempC: 15, RH: 40, HeightM: 360, WindKmh: 20, WindDir: 250},
			{HPa: 950, TempC: 13.5, RH: 35, HeightM: 580, WindKmh: 25, WindDir: 255},
			{HPa: 900, TempC: 10, RH: 30, HeightM: 1020, WindKmh: 30, WindDir: 260},
			{HPa: 850, TempC: 7, RH: 30, HeightM: 1490, WindKmh: 35, WindDir: 265},
			{HPa: 700, TempC: -2, RH: 25, HeightM: 3050, WindKmh: 50, WindDir: 270},
		}
		b.Hourly = append(b.Hourly, h)
	}
	return b
}

func TestPersistWeather(t *testing.T) {
	e := New(t.TempDir(), Options{Offline: true, Logger: quietLogger()})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ctx := context.Background()
	st := e.Store()
	rx := model.Receiver{Located: true, Lat: 50, Lon: 8, GroundElevM: 110}
	now := time.Now()
	hour := now.Unix() / 3600 * 3600

	n, full := e.persistWeather(testBundle(now, 48, 168), rx, now)
	if !full || n == 0 {
		t.Fatalf("first persist: n=%d full=%v", n, full)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	obs, err := st.WeatherObs(ctx, hour-60*3600, hour+3600)
	if err != nil || len(obs) != 48 {
		t.Fatalf("weather_obs: %d rows, err %v", len(obs), err)
	}
	last := obs[len(obs)-1]
	if last.T != hour || last.TempC != 12 || last.PressureMSL != 1018 {
		t.Fatalf("last obs = %+v", last)
	}
	// Derived radio metrics are filled in.
	if !(last.NSurface > 250 && last.NSurface < 450) {
		t.Errorf("n_surface = %v", last.NSurface)
	}
	if !(last.DNDH < -40) {
		t.Errorf("dn_dh = %v, want super-refractive (< -40 N/km)", last.DNDH)
	}
	if !(last.KFactor > 1.33 || last.KFactor < 0) {
		t.Errorf("k_factor = %v, want above standard (or negative = ducting)", last.KFactor)
	}
	if last.Duct != last.Duct { // NaN
		t.Errorf("duct is NaN")
	}
	if !strings.Contains(last.ProfileJSON, `"levels"`) {
		t.Errorf("profile_json = %q", last.ProfileJSON)
	}

	fc, err := st.WeatherForecast(ctx, hour, hour+200*3600)
	if err != nil || len(fc) != 168 {
		t.Fatalf("weather_fc: %d rows, err %v", len(fc), err)
	}
	if fc[0].T != hour+3600 || fc[0].Issued != now.Unix() || fc[0].ProfileJSON == "" {
		t.Fatalf("first forecast row = %+v", fc[0])
	}
	log, err := st.WeatherForecastLog(ctx, hour, hour+200*3600)
	if err != nil || len(log) != len(fcLogLeads) {
		t.Fatalf("weather_fc_log: %d rows, want %d, err %v", len(log), len(fcLogLeads), err)
	}
	leads := map[int]bool{}
	for _, r := range log {
		leads[r.LeadH] = true
		if r.T != hour+int64(r.LeadH)*3600 {
			t.Errorf("log row t=%d lead=%d", r.T, r.LeadH)
		}
	}
	for _, l := range fcLogLeads {
		if !leads[l] {
			t.Errorf("lead %d h not logged", l)
		}
	}

	// A second update in the same hour only refreshes the newest hours.
	if n, full := e.persistWeather(testBundle(now, 48, 168), rx, now.Add(time.Second)); full || n != 3 {
		t.Fatalf("second persist: n=%d full=%v", n, full)
	}
	// A new location is a full write again.
	rx.Lat = 51
	if _, full := e.persistWeather(testBundle(now, 48, 168), rx, now); !full {
		t.Fatal("location change did not trigger a full write")
	}

	// The forecast log is bounded.
	old := store.NewWeather(hour - (fcLogKeepDays+5)*86400)
	old.Issued = old.T - 3600
	old.TempC = 1
	st.AddWeatherForecastLog([]store.Weather{old})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	e.pruneForecastLog(ctx)
	if rows, _ := st.WeatherForecastLog(ctx, 0, hour-86400); len(rows) != 0 {
		t.Fatalf("%d old forecast log rows survived", len(rows))
	}
}

func TestAircraftDBImportFile(t *testing.T) {
	dir := t.TempDir()
	e := New(dir, Options{Offline: true, Logger: quietLogger()})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ctx := context.Background()

	// Build a file in the staging format's source form by round-tripping
	// records through refdata's own test data.
	src, err := os.ReadFile(filepath.Join("..", "refdata", "testdata", "tar1090.csv"))
	if err != nil {
		t.Skipf("refdata test data not available: %v", err)
	}
	status, err := e.ImportAircraftDBFile(ctx, bytes.NewReader(src), int64(len(src)), "upload.csv")
	if err != nil {
		t.Fatalf("import: %v (status %+v)", err, status)
	}
	if status.Running || status.Stage != "done" || status.Stored == 0 {
		t.Fatalf("status = %+v", status)
	}
	info, rows, ok, err := e.AircraftDBInfo(ctx)
	if err != nil || !ok || rows != status.Stored || info.Records != rows || info.Source != "upload.csv" {
		t.Fatalf("info = %+v rows=%d ok=%v err=%v", info, rows, ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "acdb-staging.tmp")); !os.IsNotExist(err) {
		t.Fatalf("staging file left behind: %v", err)
	}

	// Garbage is rejected and leaves the installed database alone.
	if _, err := e.ImportAircraftDBFile(ctx, strings.NewReader("not a database\n"), 0, ""); err == nil {
		t.Fatal("garbage import succeeded")
	}
	if st := e.ImportStatus(); st.Running || st.Stage != "error" || st.Error == "" {
		t.Fatalf("status after failure = %+v", st)
	}
	if n, _ := e.Store().ACDBCount(ctx); n != rows {
		t.Fatalf("acdb rows = %d after a failed import, want %d", n, rows)
	}
}

func TestStagingRoundTrip(t *testing.T) {
	recs := []refdata.ACRecord{
		{ICAO: 0x3C6444, Reg: "D-AIBD", Type: "A319", Desc: "AIRBUS A-319", Operator: "Lufthansa", Flags: 0, Year: 2010},
		{ICAO: 0xAE1234, Flags: refdata.ACMilitary | refdata.ACInteresting},
		{ICAO: 0x4CA123, Reg: "EI-ÄÖÜ", Desc: strings.Repeat("x", 300)},
	}
	var buf []byte
	for _, r := range recs {
		buf = appendStaged(buf, r)
	}
	var rerr error
	i := 0
	for ent := range stagedSeq(bufio.NewReader(bytes.NewReader(buf)), &rerr, func(int64) {}) {
		r := recs[i]
		if ent.ICAO != r.ICAO || ent.Reg != r.Reg || ent.Type != r.Type || ent.Desc != r.Desc ||
			ent.Operator != r.Operator || ent.Flags != int(r.Flags) || ent.Year != r.Year {
			t.Errorf("record %d: got %+v want %+v", i, ent, r)
		}
		i++
	}
	if i != len(recs) || rerr != nil {
		t.Fatalf("read %d records, err %v", i, rerr)
	}
	// A truncated file is an error, not a silent short read.
	rerr = nil
	for range stagedSeq(bufio.NewReader(bytes.NewReader(buf[:len(buf)-5])), &rerr, func(int64) {}) {
	}
	if rerr == nil {
		t.Fatal("truncated staging file not detected")
	}
}

func TestNoticeBroker(t *testing.T) {
	b := newBroker()
	b.publish(Notice{Text: "early"})
	ch, cancel := b.subscribe()
	for i := 0; i < noticeBuffer+40; i++ { // overflow must not block
		b.publish(Notice{Level: "warn", Text: fmt.Sprint(i)})
	}
	if got := len(ch); got != noticeBuffer {
		t.Fatalf("buffered %d notices, want %d", got, noticeBuffer)
	}
	n := <-ch
	if n.Text != "0" || n.Level != "warn" || n.T == 0 {
		t.Fatalf("first notice = %+v", n)
	}
	if bl := b.backlog(); len(bl) != noticeBacklog || bl[len(bl)-1].Text != fmt.Sprint(noticeBuffer+39) {
		t.Fatalf("backlog len %d, last %+v", len(bl), bl[len(bl)-1])
	}
	cancel()
	cancel() // idempotent
	for range ch {
	}
	ch2, cancel2 := b.subscribe()
	b.close()
	if _, ok := <-ch2; ok {
		t.Fatal("channel not closed by broker close")
	}
	cancel2()
	b.publish(Notice{Text: "after close"}) // no panic
	ch3, _ := b.subscribe()
	if _, ok := <-ch3; ok {
		t.Fatal("subscription after close is not closed")
	}
}

func TestClockPlausible(t *testing.T) {
	e := New(t.TempDir(), Options{Logger: quietLogger()})
	if e.clockPlausible(time.Date(1970, 1, 1, 0, 5, 0, 0, time.UTC)) {
		t.Fatal("1970 accepted")
	}
	if e.clockPlausible(time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("2019 accepted")
	}
	if !e.clockPlausible(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("2026 rejected")
	}
}
