package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"airview/internal/latency"
	"airview/internal/model"
	"airview/internal/ntp"
)

// TestLatencyEndpoints covers the latency-chain endpoints on the demo feed.
func TestLatencyEndpoints(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.waitTraffic(3)
	a := r.eng.Latency()
	deadline := time.Now().Add(30 * time.Second)
	for len(a.Aircraft()) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if len(a.Aircraft()) == 0 {
		t.Fatal("no aircraft reached the latency analysis")
	}

	var lat struct {
		Feeds      []json.RawMessage   `json:"feeds"`
		Chain      *latency.Summary    `json:"chain"`
		Histograms []latency.Histogram `json:"histograms"`
	}
	rec := r.get("/api/timing/latency")
	if err := json.Unmarshal(rec.Body.Bytes(), &lat); err != nil || rec.Code != 200 {
		t.Fatalf("latency: %d %v", rec.Code, err)
	}
	if lat.Feeds == nil || lat.Chain == nil || len(lat.Chain.Stages) == 0 || lat.Chain.Counts.Frames == 0 || lat.Histograms == nil {
		t.Errorf("latency reply incomplete: chain=%v histograms=%d", lat.Chain != nil, len(lat.Histograms))
	}
	if lat.Chain != nil && !lat.Chain.ReceiverSet {
		t.Error("demo receiver not handed to the latency analysis")
	}

	var list struct {
		Aircraft []latency.AircraftRow `json:"aircraft"`
	}
	if err := json.Unmarshal(r.get("/api/timing/latency/aircraft").Body.Bytes(), &list); err != nil || len(list.Aircraft) == 0 {
		t.Fatalf("latency aircraft: %v %d", err, len(list.Aircraft))
	}
	icao := list.Aircraft[0].ICAO
	var det struct {
		Row       latency.AircraftRow `json:"row"`
		Waterfall latency.Waterfall   `json:"waterfall"`
	}
	rec = r.get("/api/timing/latency/aircraft/" + icao)
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil || rec.Code != 200 || det.Row.ICAO != icao {
		t.Errorf("latency aircraft %s: %d %v %+v", icao, rec.Code, err, det.Row.ICAO)
	}
	if rec := r.get("/api/timing/latency/aircraft/ABCDEF"); rec.Code != 404 || !strings.Contains(rec.Body.String(), "unknown aircraft") {
		t.Errorf("unknown aircraft: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/api/timing/latency/aircraft/xyz"); rec.Code != 400 {
		t.Errorf("bad address: %d", rec.Code)
	}

	var fs struct {
		Feed   string               `json:"feed"`
		Feeds  []string             `json:"feeds"`
		Series []latency.FeedMinute `json:"series"`
	}
	rec = r.get("/api/timing/latency/feeds/demo?since=0")
	if err := json.Unmarshal(rec.Body.Bytes(), &fs); err != nil || rec.Code != 200 || fs.Feed != "demo" || fs.Series == nil || len(fs.Feeds) == 0 {
		t.Errorf("feed series: %d %v %+v", rec.Code, err, fs)
	}
	if rec := r.get("/api/timing/latency/feeds/nope"); rec.Code != 404 {
		t.Errorf("unknown feed: %d", rec.Code)
	}
	if rec := r.get("/api/timing/latency/feeds/demo?since=x"); rec.Code != 400 {
		t.Errorf("bad since: %d", rec.Code)
	}

	var chk latency.NTPCheck
	rec = r.get("/api/timing/ntpcheck")
	if err := json.Unmarshal(rec.Body.Bytes(), &chk); err != nil || rec.Code != 200 || chk.Quality == "" || chk.Explanation == "" {
		t.Errorf("ntpcheck: %d %v %+v", rec.Code, err, chk.Quality)
	}

	// Effective station position.
	var st model.Status
	_ = json.Unmarshal(r.get("/api/status").Body.Bytes(), &st)
	if st.Station.Source != "config" || st.Station.Lat == nil || st.Station.Lon == nil {
		t.Errorf("status station %+v", st.Station)
	}
}

// fakeNTP answers NTP client requests on a local UDP port as a stratum 1
// GPS server.
func fakeNTP(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req, err := ntp.ParsePacket(buf[:n])
			if err != nil {
				continue
			}
			now := ntp.TimestampFromTime(time.Now())
			resp := ntp.Packet{Version: 4, Mode: ntp.ModeServer, Stratum: 1, Poll: 6, Precision: -20,
				RootDispersion: ntp.ShortFromDuration(500 * time.Microsecond),
				RefID:          uint32('G')<<24 | uint32('P')<<16 | uint32('S')<<8,
				Reference:      now, Origin: req.Transmit, Receive: now, Transmit: now}
			b := resp.Marshal()
			_, _ = pc.WriteTo(b[:], addr)
		}
	}()
	return pc.LocalAddr().String()
}

// TestNTPProbe: POST /api/timing/ntp/sample with a server queries it once
// without touching the configuration.
func TestNTPProbe(t *testing.T) {
	r := newRig(t, rigOptions{})
	addr := fakeNTP(t)
	before := r.eng.Config().Get().NTP

	type probe struct {
		OK         bool     `json:"ok"`
		Server     string   `json:"server"`
		Stratum    int      `json:"stratum"`
		RefID      string   `json:"refId"`
		OffsetMs   *float64 `json:"offsetMs"`
		DelayMs    *float64 `json:"delayMs"`
		RootDispMs *float64 `json:"rootDispMs"`
		Error      string   `json:"error"`
		Saved      bool     `json:"saved"`
	}
	for _, c := range []struct{ url, body string }{
		{"/api/timing/ntp/sample?server=" + addr, ""},
		{"/api/timing/ntp/sample", `{"server":"` + addr + `"}`},
	} {
		rec := r.do("POST", c.url, loopbackAddr, c.body, "Content-Type", "application/json")
		var p probe
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != 200 {
			t.Fatalf("%s %s: %d %v %s", c.url, c.body, rec.Code, err, rec.Body.String())
		}
		if !p.OK || p.Stratum != 1 || p.RefID != "GPS" || p.OffsetMs == nil || p.DelayMs == nil || p.RootDispMs == nil || p.Saved || p.Server != addr {
			t.Errorf("probe %s: %+v", c.url, p)
		}
	}
	// Nothing listens on this port: ok=false with an error, still 200.
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	dead := pc.LocalAddr().String()
	_ = pc.Close()
	rec := r.do("POST", "/api/timing/ntp/sample?server="+dead, loopbackAddr, "")
	var p probe
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != 200 || p.OK || p.Error == "" || p.OffsetMs != nil {
		t.Errorf("dead server: %d %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"a b", "host/x", "[::1", "bad!host", "h:99999"} {
		if rec := r.do("POST", "/api/timing/ntp/sample?server="+strings.ReplaceAll(bad, " ", "%20"), loopbackAddr, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid server %q: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	if after := r.eng.Config().Get().NTP; after != before || r.eng.NTP().Config().Server != before.Server {
		t.Errorf("probe changed the NTP configuration: %+v -> %+v", before, after)
	}
}

// TestNTPServerLive: a saved config.ntp.server is the sampler's (and the
// latency clock's) reference at once.
func TestNTPServerLive(t *testing.T) {
	r := newRig(t, rigOptions{})
	addr := fakeNTP(t)
	cfg := r.eng.Config().Get()
	body, _ := json.Marshal(map[string]any{"ntp": model.NTPConfig{Enabled: true, Server: addr, IntervalSec: 64}})
	if rec := r.do("PUT", "/api/config", loopbackAddr, string(body), "Content-Type", "application/json"); rec.Code != 200 {
		t.Fatalf("PUT config: %d %s", rec.Code, rec.Body.String())
	}
	_ = cfg
	if got := r.eng.NTP().Config().Server; got != addr {
		t.Fatalf("sampler server %q, want %q", got, addr)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !r.eng.NTP().Synced() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	var chk latency.NTPCheck
	_ = json.Unmarshal(r.get("/api/timing/ntpcheck").Body.Bytes(), &chk)
	if !chk.NTP.Enabled || chk.NTP.Server != addr || chk.NTP.Stratum != 1 {
		t.Errorf("ntpcheck does not follow config.ntp.server: %+v", chk.NTP)
	}
	// Without a server argument the endpoint samples the configured server.
	rec := r.do("POST", "/api/timing/ntp/sample", loopbackAddr, "")
	var sm ntp.Sample
	if err := json.Unmarshal(rec.Body.Bytes(), &sm); err != nil || rec.Code != 200 || sm.Stratum != 1 {
		t.Errorf("configured sample: %d %s", rec.Code, rec.Body.String())
	}
}
