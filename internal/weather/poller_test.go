package weather

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airview/internal/model"
)

var (
	wxOn    = model.WeatherConfig{Enabled: true, IntervalMin: 15}
	station = model.Receiver{Located: true, Lat: 50, Lon: 8}
)

// pollerHarness is a Poller wired to a fake API with fast timing.
type pollerHarness struct {
	rec     *recorder
	p       *Poller
	updates chan *Bundle
	radar   chan *Radar
	failN   atomic.Int64 // number of upcoming /forecast requests to fail with 500
	cancel  context.CancelFunc
	done    chan error
}

func newPollerHarness(t *testing.T, cfg model.WeatherConfig, rx model.Receiver, interval time.Duration) *pollerHarness {
	t.Helper()
	h := &pollerHarness{updates: make(chan *Bundle, 64), radar: make(chan *Radar, 64)}
	forecast, radar := fixture(t, "forecast.json"), fixture(t, "rainviewer.json")
	h.rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/forecast":
			if h.failN.Load() > 0 {
				h.failN.Add(-1)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(forecast)
		case "/radar":
			_, _ = w.Write(radar)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	h.p = NewPoller(cfg, rx, PollerOptions{Client: testClient(h.rec, Options{Retries: -1})})
	h.p.backoffBase = 5 * time.Millisecond
	h.p.radarEvery = 25 * time.Millisecond
	h.p.intervalFn = func(model.WeatherConfig) time.Duration { return interval }
	h.p.OnUpdate(func(b *Bundle) {
		select {
		case h.updates <- b:
		default:
		}
	})
	h.p.OnRadar(func(r *Radar) {
		select {
		case h.radar <- r:
		default:
		}
	})
	return h
}

func (h *pollerHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.p.Run(ctx) }()
	t.Cleanup(func() { h.stop(t) })
}

func (h *pollerHarness) stop(t *testing.T) {
	t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not stop")
	}
}

func (h *pollerHarness) nextUpdate(t *testing.T) *Bundle {
	t.Helper()
	select {
	case b := <-h.updates:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no weather update within 5 s")
		return nil
	}
}

func TestPollerRespectsDisabledAndUnlocated(t *testing.T) {
	h := newPollerHarness(t, model.WeatherConfig{Enabled: false, RadarOverlay: true}, station, 30*time.Millisecond)
	h.run(t)
	time.Sleep(80 * time.Millisecond)
	if n := h.rec.count(""); n != 0 {
		t.Fatalf("%d requests while disabled", n)
	}
	if _, err := h.p.Refresh(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Errorf("Refresh while disabled: %v", err)
	}
	if st := h.p.Status(); st.Enabled || st.NextPoll != 0 || st.RadarEnabled {
		t.Errorf("status = %+v", st)
	}

	// Enabled but no coordinates yet: still silent.
	h.p.Reconfigure(wxOn, model.Receiver{Located: false, Lat: 50, Lon: 8})
	time.Sleep(80 * time.Millisecond)
	if n := h.rec.count("/forecast"); n != 0 {
		t.Fatalf("%d forecast requests while unlocated", n)
	}
	if _, err := h.p.Refresh(context.Background()); !errors.Is(err, ErrNotLocated) {
		t.Errorf("Refresh while unlocated: %v", err)
	}
	if h.p.Latest() != nil {
		t.Error("Latest() non-nil before any fetch")
	}
	// Garbage coordinates count as unlocated.
	h.p.Reconfigure(wxOn, model.Receiver{Located: true, Lat: 123, Lon: 8})
	if st := h.p.Status(); st.Located {
		t.Error("out-of-range coordinates accepted as a location")
	}

	// Setting the location starts polling at once.
	h.p.Reconfigure(wxOn, station)
	b := h.nextUpdate(t)
	if b.Lat != 50 || b.Lon != 8 || len(b.Hourly) != 24 || h.p.Latest() != b {
		t.Errorf("first bundle: lat %v lon %v hours %d", b.Lat, b.Lon, len(b.Hourly))
	}
	st := h.p.Status()
	if !st.Enabled || !st.Located || st.LastOK == 0 || st.LastAttempt == 0 || st.Failures != 0 || st.LastError != "" || st.NextPoll == 0 {
		t.Errorf("status = %+v", st)
	}
	if st.Usage.Day < 13 {
		t.Errorf("usage not accounted: %+v", st.Usage)
	}
	// ... and keeps polling on the interval.
	h.nextUpdate(t)
	h.nextUpdate(t)

	// Moving the station drops the old bundle and fetches for the new place.
	h.p.Reconfigure(wxOn, model.Receiver{Located: true, Lat: 51.5, Lon: 9.25})
	deadline := time.After(5 * time.Second)
	for {
		var nb *Bundle
		select {
		case nb = <-h.updates:
		case <-deadline:
			t.Fatal("no bundle for the new location")
		}
		if nb.Lat == 51.5 && nb.Lon == 9.25 {
			break
		}
	}
	if q := h.rec.request(-1).URL.Query(); q.Get("latitude") != "51.5000" || q.Get("longitude") != "9.2500" {
		t.Errorf("request after move: %s", h.rec.request(-1).URL.RawQuery)
	}
	if l := h.p.Latest(); l == nil || l.Lat != 51.5 {
		t.Errorf("Latest after move = %+v", l)
	}

	// Disabling stops all traffic.
	h.p.Reconfigure(model.WeatherConfig{Enabled: false}, station)
	time.Sleep(60 * time.Millisecond)
	n := h.rec.count("")
	time.Sleep(120 * time.Millisecond)
	if got := h.rec.count(""); got != n {
		t.Errorf("%d requests after disabling", got-n)
	}
	h.stop(t)
}

func TestPollerBackoffAndRecovery(t *testing.T) {
	h := newPollerHarness(t, wxOn, station, time.Hour)
	h.failN.Store(3)
	h.run(t)
	b := h.nextUpdate(t)
	if b == nil || h.rec.count("/forecast") != 4 {
		t.Errorf("requests = %d, want 3 failures + 1 success", h.rec.count("/forecast"))
	}
	if st := h.p.Status(); st.Failures != 0 || st.LastError != "" || st.LastOK == 0 {
		t.Errorf("status after recovery = %+v", st)
	}
	// With a one-hour interval nothing else is fetched.
	time.Sleep(80 * time.Millisecond)
	if n := h.rec.count("/forecast"); n != 4 {
		t.Errorf("extra requests: %d", n)
	}
	h.stop(t)

	// The back-off schedule with production timing.
	p := NewPoller(wxOn, station, PollerOptions{Client: testClient(h.rec, Options{})})
	interval := 15 * time.Minute
	within := func(name string, got, want time.Duration) {
		t.Helper()
		if got < time.Duration(0.89*float64(want)) || got > time.Duration(1.11*float64(want)) {
			t.Errorf("%s: %v, want %v ±10%%", name, got, want)
		}
	}
	boom := errors.New("boom")
	within("1st failure", p.backoff(boom, 1, interval), 30*time.Second)
	within("2nd failure", p.backoff(boom, 2, interval), time.Minute)
	within("4th failure", p.backoff(boom, 4, interval), 4*time.Minute)
	within("10th failure", p.backoff(boom, 10, interval), interval)
	within("429 with hint", p.backoff(&APIError{Status: 429, RetryAfter: time.Hour}, 1, interval), time.Hour)
	within("429 short hint", p.backoff(&APIError{Status: 429, RetryAfter: time.Second}, 1, interval), 30*time.Second)
	within("budget", p.backoff(ErrBudget, 1, interval), interval)
	if d := pollInterval(model.WeatherConfig{IntervalMin: 0}); d != 15*time.Minute {
		t.Errorf("default interval = %v", d)
	}
	if d := pollInterval(model.WeatherConfig{IntervalMin: 1}); d != 5*time.Minute {
		t.Errorf("minimum interval = %v", d)
	}
	if d := pollInterval(model.WeatherConfig{IntervalMin: 100000}); d != 6*time.Hour {
		t.Errorf("maximum interval = %v", d)
	}
}

func TestPollerFailureStatus(t *testing.T) {
	h := newPollerHarness(t, wxOn, station, time.Hour)
	h.failN.Store(1 << 30)
	_, err := h.p.Refresh(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	st := h.p.Status()
	if st.Failures != 1 || st.LastError == "" || st.LastOK != 0 || st.LastAttempt == 0 || h.p.Latest() != nil {
		t.Errorf("status = %+v", st)
	}
}

func TestPollerRefresh(t *testing.T) {
	h := newPollerHarness(t, wxOn, station, time.Hour)
	ctx := context.Background()
	a, err := h.p.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.p.Latest() != a || h.rec.count("/forecast") != 1 {
		t.Fatal("first refresh")
	}
	// A second refresh right away is served from the fresh bundle.
	b, err := h.p.Refresh(ctx)
	if err != nil || b != a || h.rec.count("/forecast") != 1 {
		t.Errorf("refresh inside the politeness gap hit the network (%d requests)", h.rec.count("/forecast"))
	}
	h.p.refreshGap = 0
	c, err := h.p.Refresh(ctx)
	if err != nil || c == a || h.rec.count("/forecast") != 2 {
		t.Errorf("forced refresh: %v, %d requests", err, h.rec.count("/forecast"))
	}
	if len(h.updates) != 2 {
		t.Errorf("OnUpdate called %d times, want 2", len(h.updates))
	}
	// A scheduled poll that was queued behind a manual refresh finds the
	// schedule already moved on and does not fetch again.
	if d, err := h.p.fetch(ctx, false); err != nil || d != c || h.rec.count("/forecast") != 2 {
		t.Errorf("scheduled poll right after a refresh: %v, %d requests", err, h.rec.count("/forecast"))
	}

	// Concurrent refreshes are serialised and all succeed.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.p.Refresh(ctx); err != nil {
				t.Errorf("concurrent refresh: %v", err)
			}
		}()
	}
	wg.Wait()

	// A cancelled context is reported and does not count as a failure.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.p.Refresh(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled refresh: %v", err)
	}
	if st := h.p.Status(); st.Failures != 0 {
		t.Errorf("cancel counted as failure: %+v", st)
	}
}

func TestPollerRadar(t *testing.T) {
	h := newPollerHarness(t, model.WeatherConfig{Enabled: true, RadarOverlay: true}, model.Receiver{}, time.Hour)
	h.run(t)
	select {
	case r := <-h.radar:
		if len(r.Past) != 13 || h.p.Radar() == nil {
			t.Errorf("radar = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no radar index within 5 s")
	}
	// The index is re-polled, but an unchanged index does not fire the callback.
	deadline := time.Now().Add(5 * time.Second)
	for h.rec.count("/radar") < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.rec.count("/radar") < 4 {
		t.Fatal("radar index not re-polled")
	}
	if len(h.radar) != 0 {
		t.Error("OnRadar fired for an unchanged index")
	}
	if h.rec.count("/forecast") != 0 {
		t.Error("weather fetched for an unlocated station")
	}
	if st := h.p.Status(); !st.RadarEnabled || st.RadarLastOK == 0 || st.RadarError != "" {
		t.Errorf("status = %+v", st)
	}
	// Switching the overlay off clears the index and stops the polling.
	h.p.Reconfigure(model.WeatherConfig{Enabled: true, RadarOverlay: false}, model.Receiver{})
	if h.p.Radar() != nil {
		t.Error("Radar() non-nil after the overlay was disabled")
	}
	time.Sleep(60 * time.Millisecond)
	n := h.rec.count("/radar")
	time.Sleep(100 * time.Millisecond)
	if got := h.rec.count("/radar"); got != n {
		t.Errorf("%d radar requests after disabling the overlay", got-n)
	}
}

func TestPollerSeed(t *testing.T) {
	h := newPollerHarness(t, wxOn, station, time.Hour)
	b, err := parseBundle(fixture(t, "forecast.json"), forecastVars, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Kind, b.Lat, b.Lon = KindForecast, 50, 8
	b.Fetched = time.Now().Add(-time.Minute).UnixMilli()

	other := *b
	other.Lat = 52
	if h.p.Seed(&other) {
		t.Error("bundle for another location accepted")
	}
	future := *b
	future.Fetched = time.Now().Add(time.Hour).UnixMilli()
	if h.p.Seed(&future) {
		t.Error("bundle from the future accepted")
	}
	hist := *b
	hist.Kind = KindArchive
	if h.p.Seed(&hist) || h.p.Seed(nil) {
		t.Error("non-forecast bundle accepted")
	}
	if !h.p.Seed(b) || h.p.Latest() != b {
		t.Fatal("valid seed rejected")
	}
	if h.p.Seed(b) {
		t.Error("same bundle seeded twice")
	}
	// A fresh seed means no request at start-up.
	h.run(t)
	time.Sleep(80 * time.Millisecond)
	if n := h.rec.count("/forecast"); n != 0 {
		t.Errorf("%d requests despite a fresh seeded bundle", n)
	}
	if st := h.p.Status(); st.LastOK != b.Fetched {
		t.Errorf("status = %+v", st)
	}
	h.stop(t)

	// A stale seed is shown but refreshed immediately.
	h2 := newPollerHarness(t, wxOn, station, time.Hour)
	stale := *b
	stale.Fetched = time.Now().Add(-2 * time.Hour).UnixMilli()
	if !h2.p.Seed(&stale) {
		t.Fatal("stale seed rejected")
	}
	h2.run(t)
	if nb := h2.nextUpdate(t); nb == &stale || h2.p.Latest() != nb {
		t.Error("stale seed was not replaced")
	}
}
