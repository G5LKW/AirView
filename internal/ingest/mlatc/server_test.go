package mlatc

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/mlat/geo"
	"airview/internal/model"
)

const (
	uuidA = "0123456789abcdef0123456789abcdef"
	uuidB = "fedcba9876543210fedcba9876543210"
	uuidC = "00112233445566778899aabbccddeeff"
)

var (
	longHex  = "8d4840d6202cc371c32ce0576098" // DF17
	shortHex = "5d4840d6c0ffee"               // DF11
)

func TestHandshakeNegotiation(t *testing.T) {
	env := startServer(t, Options{Policy: Policy{MOTD: "hello"}}, nil, nil)
	for _, tc := range []struct {
		offer any
		want  string
	}{
		{[]string{"none", "zlib", "zlib2"}, "zlib2"},
		{[]string{"none", "zlib"}, "zlib"},
		{[]string{"none"}, "none"},
		{"zlib", "zlib"},
		{nil, "none"},
	} {
		h := defaultHello("")
		h["uuid"] = fmt.Sprintf("%032x", len(tc.want)*7919+len(fmt.Sprint(tc.offer)))
		if tc.offer == nil {
			delete(h, "compress")
		} else {
			h["compress"] = tc.offer
		}
		c := dialClient(t, env.addr, h)
		if d := c.denied(); d != nil {
			t.Fatalf("denied: %v", d)
		}
		if c.compress != tc.want {
			t.Fatalf("offer %v: got %q want %q", tc.offer, c.compress, tc.want)
		}
		var r struct {
			UDP   bool   `json:"udp_transport"`
			Split bool   `json:"split_sync"`
			MOTD  string `json:"motd"`
		}
		b, _ := json.Marshal(c.reply)
		json.Unmarshal(b, &r)
		if r.UDP || !r.Split || r.MOTD != "hello" { // split_sync is offered by default (SplitSyncAuto)
			t.Fatalf("reply %s", b)
		}
		before := env.sink.n.Load()
		if err := c.send(`{"input_connected":"connected"}`+"\n", `{"mlat":{"t":1000000,"m":"`+shortHex+`"}}`+"\n"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "frame via "+tc.want, 3*time.Second, func() bool { return env.sink.n.Load() == before+1 })
	}
	env2 := startServer(t, Options{DisableCompression: true}, nil, nil)
	if c := dialClient(t, env2.addr, defaultHello(uuidA)); c.compress != "none" {
		t.Fatalf("compression not disabled: %q", c.compress)
	}
}

func TestHandshakeIdentity(t *testing.T) {
	var got Identity
	var mu sync.Mutex
	reg := NewMemRegistry()
	reg.Decide = func(id Identity) (string, error) {
		mu.Lock()
		got = id
		mu.Unlock()
		return model.AdmApproved, nil
	}
	env := startServer(t, Options{}, reg, nil)
	h := defaultHello("{01234567-89AB-CDEF-0123-456789ABCDEF}")
	h["user"] = "  Joe\x07's  receiver  "
	h["clock_type"] = "radarcape_gps"
	h["privacy"] = false
	h["alt"] = "123.5" // numeric string tolerated
	c := dialClient(t, env.addr, h)
	if c.denied() != nil {
		t.Fatal(c.denied())
	}
	mu.Lock()
	if got.UUID != uuidA || got.User != "Joe's  receiver" || got.ClockCls != model.ClockGPSUTC || !got.PrivacyOff ||
		got.Privacy || !got.Located || got.Lat != 51.5 || got.Alt != 123.5 || got.AltRef != model.AltRefAmbiguous ||
		got.Kind != model.StationKindMLATC || got.Slot != -1 || !strings.HasPrefix(got.IPKey, "ip:") {
		t.Fatalf("identity %+v", got)
	}
	// privacy flag absent: neither
	h2 := defaultHello(uuidB)
	delete(h2, "privacy")
	h2["lat"], h2["lon"] = 0.0, 0.0 // null island = not located
	mu.Unlock()
	dialClient(t, env.addr, h2)
	mu.Lock()
	defer mu.Unlock()
	if got.Privacy || got.PrivacyOff || got.Located {
		t.Fatalf("identity %+v", got)
	}
}

func TestHandshakeDeny(t *testing.T) {
	gate := true
	var gmu sync.Mutex
	reg := NewMemRegistry()
	reg.Decide = func(id Identity) (string, error) {
		if id.UUID == uuidC {
			return model.AdmDenied, nil
		}
		return model.AdmApproved, nil
	}
	env := startServer(t, Options{Gate: func() bool { gmu.Lock(); defer gmu.Unlock(); return gate }}, reg, nil)

	h := defaultHello(uuidA)
	h["version"] = 1
	if d := dialClient(t, env.addr, h).denied(); len(d) != 1 || !strings.Contains(d[0], "version") {
		t.Fatalf("version: %v", d)
	}
	// not JSON
	c := dialRaw(t, env.addr, "hello there\n")
	if !strings.Contains(c, "deny") {
		t.Fatalf("garbage handshake: %q", c)
	}
	// registry refusal, then cool-down
	if d := dialClient(t, env.addr, defaultHello(uuidC)).denied(); len(d) != 1 || !strings.Contains(d[0], "denied") {
		t.Fatalf("registry: %v", d)
	}
	reg.Decide = nil
	if d := dialClient(t, env.addr, defaultHello(uuidC)).denied(); len(d) != 1 || !strings.Contains(d[0], "temporarily") {
		t.Fatalf("cool-down: %v", d)
	}
	gmu.Lock()
	gate = false
	gmu.Unlock()
	if d := dialClient(t, env.addr, defaultHello(uuidB)).denied(); len(d) != 1 || d[0] != denyNotReady {
		t.Fatalf("gate: %v", d)
	}
	st := env.srv.Stats()
	if st.Refused < 4 || st.Handshakes != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func dialRaw(t *testing.T, addr, send string) string {
	t.Helper()
	c := dialClient2(t, addr)
	defer c.Close()
	c.Write([]byte(send))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

func TestDuplicateAndReplace(t *testing.T) {
	clk := newTestClock()
	env := startServer(t, Options{Now: clk.Now}, nil, nil)
	a := dialClient(t, env.addr, defaultHello(uuidA))
	if a.denied() != nil {
		t.Fatal("first denied")
	}
	if d := dialClient(t, env.addr, defaultHello(uuidA)).denied(); len(d) != 1 || d[0] != denyDuplicate {
		t.Fatalf("clone admitted: %v", d)
	}
	clk.Add(31 * time.Second) // the first went silent: a half-open connection
	b := dialClient(t, env.addr, defaultHello(uuidA))
	if b.denied() != nil {
		t.Fatalf("replacement denied: %v", b.denied())
	}
	if !a.waitClosed(3 * time.Second) {
		t.Fatal("old connection not closed")
	}
	waitFor(t, "one attachment", 3*time.Second, func() bool { return env.reg.Attached() == 1 })
}

func TestFramesAndEpochs(t *testing.T) {
	env := startServer(t, Options{}, nil, nil)
	h := defaultHello(uuidA)
	h["clock_type"] = "beast"
	c := dialClient(t, env.addr, h)
	st, _, _ := env.reg.Lookup("uuid:" + uuidA)
	raw := uint64(123456789)
	lines := []string{
		`{"input_connected":"connected"}` + "\n",
		fmt.Sprintf(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n", raw-768, longHex, raw+6000000-768, strings.Replace(longHex, "8d", "8D", 1)),
		// the same half again in the next pair: delivered once
		fmt.Sprintf(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n", raw+12000000-768, longHex, raw+6000000-768, longHex),
		fmt.Sprintf(`{"mlat":{"t":%d,"m":"%s"}}`+"\n", raw+100-768, shortHex),
		`{"mlat":{"t":0,"m":"` + shortHex + `"}}` + "\n", // no timestamp
		`{"clock_reset":{"reason":"Decoder mode changed to RADARCAPE","frequency":1000000000,"epoch":"utc_midnight","mode":"RADARCAPE"}}` + "\n",
		`{"mlat":{"t":3600000000000,"m":"` + shortHex + `"}}` + "\n", // 01:00:00 UTC minus 64 µs
		`{"clock_jump":true}` + "\n",
		`{"input_disconnected":"disconnected"}` + "\n",
		`{"input_connected":"connected"}` + "\n",
		`{"heartbeat":{"client_time":1.5}}` + "\n",
	}
	if err := c.send(lines...); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "frames", 3*time.Second, func() bool { return env.sink.n.Load() >= 5 })
	time.Sleep(50 * time.Millisecond)
	frames, events := env.sink.snapshot()
	if len(frames) != 5 {
		t.Fatalf("%d frames", len(frames))
	}
	want := []uint64{raw, raw + 6000000, raw + 12000000, raw + 100}
	for i, w := range want {
		f := frames[i]
		if f.Ticks != w || f.Epoch != 1 || f.ClockCls != model.ClockHW12 || f.Station != st || f.GPSNs != -1 ||
			!math.IsNaN(f.Signal) || f.Channel != -1 || f.HostNs == 0 || f.FeedID == "" {
			t.Fatalf("frame %d: %+v", i, f)
		}
	}
	if frames[3].Kind != model.FrameModeSShort || frames[0].Kind != model.FrameModeSLong {
		t.Fatal("kinds")
	}
	g := frames[4]
	day := time.Now().UTC().Truncate(24 * time.Hour).UnixNano()
	wantGPS := uint64(day + 3600e9 + offGPSShort)
	// the day is chosen within ±12 h of the server clock
	if d := int64(g.Ticks) - int64(wantGPS); g.ClockCls != model.ClockGPSUTC || g.Epoch != 2 ||
		(d != 0 && d != model.NsPerDay && d != -model.NsPerDay) {
		t.Fatalf("gps frame %+v (want ticks %d)", g, wantGPS)
	}
	var reasons []model.ClockEventReason
	for _, e := range events {
		if e.st != st {
			t.Fatal("event station")
		}
		reasons = append(reasons, e.ev.Reason)
		if e.ev.NewEpoch != e.ev.OldEpoch+1 {
			t.Fatalf("epoch numbering %+v", e.ev)
		}
	}
	wantR := []model.ClockEventReason{model.ClockEvRestart, model.ClockEvModeChange, model.ClockEvJump, model.ClockEvRestart}
	if !slices.Equal(reasons, wantR) {
		t.Fatalf("events %v, want %v", reasons, wantR)
	}
	s := env.srv.Stats()
	if s.DroppedDup != 1 || s.DroppedBad != 1 || s.Syncs != 2 || s.MLATs != 3 {
		t.Fatalf("stats %+v", s)
	}
	if ok := env.sink.ts[st]; !ok {
		t.Fatal("timestamps not enabled")
	}
}

func TestSelection(t *testing.T) {
	var seenMu sync.Mutex
	var seen, lost []uint32
	var rates []AircraftRate
	hooks := Hooks{
		OnSeen: func(_ model.StationIdx, a []uint32) { seenMu.Lock(); seen = append(seen, a...); seenMu.Unlock() },
		OnLost: func(_ model.StationIdx, a []uint32) { seenMu.Lock(); lost = append(lost, a...); seenMu.Unlock() },
		OnRates: func(_ model.StationIdx, r []AircraftRate) {
			seenMu.Lock()
			rates = append(rates, r...)
			seenMu.Unlock()
		},
	}
	env := startServer(t, Options{FlushEvery: 20 * time.Millisecond, Hooks: hooks, Policy: Policy{MaxRequested: 3}}, nil, nil)
	c := dialClient(t, env.addr, defaultHello(uuidA))
	st, _, _ := env.reg.Lookup("uuid:" + uuidA)
	c.send(`{"seen":["000001","000002","4840d6","abcdef"]}`+"\n", `{"rate_report":{"4840D6":0.5}}`+"\n")
	var start []string
	json.Unmarshal(c.expect("start_sending", 3*time.Second), &start)
	if !slices.Equal(start, []string{"000001", "000002", "4840d6"}) { // capped at 3
		t.Fatalf("start_sending %v", start)
	}
	// lost: no stop_sending; the freed slot goes to the next aircraft only
	// when it is seen again
	c.send(`{"lost":["000002"]}` + "\n")
	c.send(`{"seen":["abcdef"]}` + "\n")
	json.Unmarshal(c.expect("start_sending", 3*time.Second), &start)
	if !slices.Equal(start, []string{"abcdef"}) {
		t.Fatalf("start_sending after lost %v", start)
	}
	// the ingest switches the station to selective with an interest set
	sess := env.sink.session(0)
	if err := sess.info.Controller.SendInterest(st, ingest.InterestCmd{Mode: ingest.ModeSelective, Reset: true,
		SyncAdd: []uint32{0x4840d6}, MLATAdd: []uint32{0x777777, 1 << 24}}); err != nil {
		t.Fatal(err)
	}
	var stop []string
	json.Unmarshal(c.expect("stop_sending", 3*time.Second), &stop)
	json.Unmarshal(c.expect("start_sending", 3*time.Second), &start)
	if !slices.Equal(stop, []string{"000001", "abcdef"}) || !slices.Equal(start, []string{"777777"}) {
		t.Fatalf("selective: stop %v start %v", stop, start)
	}
	// forced by the server API
	env.srv.StopSending(st, []uint32{0x777777})
	json.Unmarshal(c.expect("stop_sending", 3*time.Second), &stop)
	if !slices.Equal(stop, []string{"777777"}) {
		t.Fatalf("forced stop %v", stop)
	}
	// back to full: the seen set again
	sess.info.Controller.SendInterest(st, ingest.InterestCmd{Mode: ingest.ModeFull, Reset: true})
	json.Unmarshal(c.expect("start_sending", 3*time.Second), &start)
	if !slices.Equal(start, []string{"000001", "abcdef"}) {
		t.Fatalf("full again %v", start)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	// OnSeen reports only addresses new to the seen set: the repeated
	// "abcdef" is not forwarded again (finding ROB-3)
	if len(seen) != 4 || !slices.Equal(lost, []uint32{2}) || len(rates) != 1 || rates[0].Addr != 0x4840d6 {
		t.Fatalf("hooks seen %v lost %v rates %v", seen, lost, rates)
	}
	l := env.srv.Links()
	if len(l) != 1 || l[0].Mode != ingest.ModeFull || l[0].Seen != 3 || l[0].Requested != 3 {
		t.Fatalf("links %+v", l)
	}
}

func TestResultDelivery(t *testing.T) {
	env := startServer(t, Options{}, nil, nil)
	ha := defaultHello(uuidA) // ecef, zlib2
	hb := defaultHello(uuidB) // legacy, plain
	hb["return_result_format"] = "old"
	hb["compress"] = []string{"none"}
	hc := defaultHello(uuidC) // no results
	hc["return_results"] = false
	a, b, c := dialClient(t, env.addr, ha), dialClient(t, env.addr, hb), dialClient(t, env.addr, hc)
	if a.compress != "zlib2" || b.compress != "none" || c.denied() != nil {
		t.Fatal("compression")
	}
	sa, _, _ := env.reg.Lookup("uuid:" + uuidA)
	sb, _, _ := env.reg.Lookup("uuid:" + uuidB)
	sc, _, _ := env.reg.Lookup("uuid:" + uuidC)
	p := geo.ToECEF(52.1, 5.2, 9000)
	fix := model.MLATFix{ICAO: 0x484c01, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{p.X, p.Y, p.Z},
		Cov: [6]float32{100, 0, 0, 100, 0, 400}, NBucket: model.NBucket4, Grade: model.GradeA}
	if !env.srv.SendResult(sa, Result{MLATFix: fix, Callsign: "KLM1"}) {
		t.Fatalf("not queued for a (%d) %+v", sa, env.srv.Links())
	}
	if !env.srv.SendResults(sb, []model.MLATFix{fix}) {
		t.Fatal("not queued for b")
	}
	if env.srv.SendResult(sc, Result{MLATFix: fix}) {
		t.Fatal("queued for a client that asked for none")
	}
	if env.srv.SendResult(999, Result{MLATFix: fix}) {
		t.Fatal("queued for an unknown station")
	}
	var ra struct {
		Addr string     `json:"addr"`
		ECEF [3]float64 `json:"ecef"`
		ND   int        `json:"nd"`
		CS   string     `json:"callsign"`
	}
	json.Unmarshal(a.expect("result", 3*time.Second), &ra)
	if ra.Addr != "484c01" || math.Abs(ra.ECEF[0]-p.X) > 0.06 || ra.ND != 4 || ra.CS != "KLM1" {
		t.Fatalf("ecef result %+v", ra)
	}
	var rb struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
		Alt float64 `json:"alt"`
	}
	json.Unmarshal(b.expect("result", 3*time.Second), &rb)
	if math.Abs(rb.Lat-52.1) > 1e-5 || math.Abs(rb.Lon-5.2) > 1e-5 || math.Abs(rb.Alt-9000/0.3048) > 1 {
		t.Fatalf("legacy result %+v", rb)
	}
	// results switched off by policy
	pol := env.srv.Policy()
	pol.DisableResults = true
	env.srv.SetPolicy(pol)
	if env.srv.SendResult(sa, Result{MLATFix: fix}) {
		t.Fatal("queued with results off")
	}
	if l := env.srv.Links(); len(l) != 3 {
		t.Fatalf("links %d", len(l))
	} else {
		for _, x := range l {
			if x.Station == sa && (x.Results != 1 || x.ResultFormat != "ecef") {
				t.Fatalf("link stats %+v", x)
			}
		}
	}
}

func TestHeartbeatAndStats(t *testing.T) {
	env := startServer(t, Options{HeartbeatEvery: 50 * time.Millisecond, StatsEvery: 50 * time.Millisecond,
		FlushEvery: 20 * time.Millisecond, Stats: statsFunc(func(model.StationIdx) (ClientStats, bool) {
			return ClientStats{PeerCount: 7, OutlierPercent: 2.5}, true
		})}, nil, nil)
	c := dialClient(t, env.addr, defaultHello(uuidA))
	c.expect("heartbeat", 3*time.Second)
	var s struct {
		Peers int     `json:"peer_count"`
		Out   float64 `json:"outlier_percent"`
	}
	json.Unmarshal(c.expect("stats", 3*time.Second), &s)
	if s.Peers != 7 || s.Out != 2.5 {
		t.Fatalf("stats %+v", s)
	}
}

type statsFunc func(model.StationIdx) (ClientStats, bool)

func (f statsFunc) ClientStats(st model.StationIdx) (ClientStats, bool) { return f(st) }

func TestRateLimit(t *testing.T) {
	clk := newTestClock()
	env := startServer(t, Options{Now: clk.Now, Policy: Policy{MaxMsgsPerSec: 20, OverLimitSeconds: 2, CoolDown: time.Hour}}, nil, nil)
	c := dialClient(t, env.addr, defaultHello(uuidA))
	if c.denied() != nil {
		t.Fatalf("denied %v", c.denied())
	}
	tick := uint64(1000000)
	burst := func() {
		var lines []string
		for i := 0; i < 50; i++ {
			tick += 1000
			lines = append(lines, fmt.Sprintf(`{"mlat":{"t":%d,"m":"%s"}}`+"\n", tick, shortHex))
		}
		c.send(lines...)
	}
	for i := 0; i < 4 && !c.waitClosed(0); i++ {
		burst()
		time.Sleep(100 * time.Millisecond)
		clk.Add(time.Second)
		c.send(`{"heartbeat":{}}` + "\n") // a line in the new second runs the accounting
		time.Sleep(50 * time.Millisecond)
	}
	if !c.waitClosed(3 * time.Second) {
		t.Fatal("not disconnected")
	}
	st := env.srv.Stats()
	if st.DroppedRate == 0 || st.Reasons[ReasonRateLimit] != 1 {
		t.Fatalf("stats %+v", st)
	}
	if d := dialClient(t, env.addr, defaultHello(uuidA)).denied(); len(d) != 1 || !strings.Contains(d[0], "temporarily") {
		t.Fatalf("no cool-down: %v", d)
	}
}

// TestUncompressedByteBudget: the byte limit is a compressed budget. An
// uncompressed client counts at 1/uncompressedByteFactor (release soak: a
// busy simulated mlat-client sent 214 kB/s of plain lines against the
// 200 kB/s limit and was refused every 5 minutes), but a flood still trips it.
func TestUncompressedByteBudget(t *testing.T) {
	run := func(lineBytes int) (closed bool) {
		clk := newTestClock()
		env := startServer(t, Options{Now: clk.Now, Policy: Policy{MaxMsgsPerSec: 100000, MaxKBps: 1, OverLimitSeconds: 2, CoolDown: time.Hour}}, nil, nil)
		h := defaultHello(uuidA)
		h["compress"] = []string{"none"}
		c := dialClient(t, env.addr, h)
		if c.denied() != nil {
			t.Fatalf("denied %v", c.denied())
		}
		tick := uint64(1000000)
		for i := 0; i < 5 && !c.waitClosed(0); i++ {
			var lines []string
			for n := 0; n < lineBytes; {
				tick += 1000
				l := fmt.Sprintf(`{"mlat":{"t":%d,"m":"%s"}}`+"\n", tick, shortHex)
				lines = append(lines, l)
				n += len(l)
			}
			c.send(lines...)
			time.Sleep(100 * time.Millisecond)
			clk.Add(time.Second)
			c.send(`{"heartbeat":{}}` + "\n")
			time.Sleep(50 * time.Millisecond)
		}
		return c.waitClosed(500 * time.Millisecond)
	}
	// 2.5 kB/s of plain lines = 0.6 kB/s against the 1 kB/s budget.
	if run(2500) {
		t.Fatal("an uncompressed client under the compressed-equivalent budget was disconnected")
	}
	// 8 kB/s of plain lines = 2 kB/s: over the budget.
	if !run(8000) {
		t.Fatal("an uncompressed flood was not disconnected")
	}
}

func TestGarbageAndIdle(t *testing.T) {
	env := startServer(t, Options{IdleTimeout: 300 * time.Millisecond, Policy: Policy{MaxBadLines: 10}}, nil, nil)
	c := dialClient(t, env.addr, defaultHello(uuidA))
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "this is not json\n")
	}
	c.send(lines...)
	if !c.waitClosed(3 * time.Second) {
		t.Fatal("garbage client not closed")
	}
	d := dialClient(t, env.addr, defaultHello(uuidB))
	if !d.waitClosed(3 * time.Second) {
		t.Fatal("idle client not closed")
	}
	waitFor(t, "reasons", 2*time.Second, func() bool {
		r := env.srv.Stats().Reasons
		return r[ReasonGarbage] == 1 && r[ReasonIdle] == 1
	})
	// an over-long line is skipped, the client stays
	e := dialClient(t, env.addr, defaultHello(uuidC))
	e.send(`{"x":"`+strings.Repeat("a", maxLine+10)+`"}`+"\n", `{"mlat":{"t":5,"m":"`+shortHex+`"}}`+"\n")
	waitFor(t, "frame after long line", 3*time.Second, func() bool { return env.sink.n.Load() == 1 })
	if env.srv.Stats().LongLines != 1 {
		t.Fatal("long line not counted")
	}
}

func TestAddressLimits(t *testing.T) {
	env := startServer(t, Options{Policy: Policy{PerIPStations: 2}}, nil, nil)
	// UUID-less clients get one station per address slot; all stations of
	// an address are capped by PerIPStations
	a := dialClient(t, env.addr, defaultHello(""))
	if a.denied() != nil {
		t.Fatal(a.denied())
	}
	b := dialClient(t, env.addr, defaultHello(uuidB))
	if b.denied() != nil {
		t.Fatal(b.denied())
	}
	for _, u := range []string{"", uuidC} {
		if d := dialClient(t, env.addr, defaultHello(u)).denied(); len(d) != 1 || d[0] != denyStationsIP {
			t.Fatalf("stations per ip (%q): %v", u, d)
		}
	}
	// a reconnect of a live UUID is not a new station: refused as a clone
	// here, but not by the station cap
	if d := dialClient(t, env.addr, defaultHello(uuidB)).denied(); len(d) != 1 || d[0] != denyDuplicate {
		t.Fatalf("reconnect: %v", d)
	}
	env2 := startServer(t, Options{Policy: Policy{MaxConnsPerIP: 2}}, nil, nil)
	dialClient(t, env2.addr, defaultHello(uuidA))
	dialClient(t, env2.addr, defaultHello(uuidB))
	if d := dialClient(t, env2.addr, defaultHello(uuidC)).denied(); len(d) != 1 || d[0] != denyLimit {
		t.Fatalf("conns per ip: %v", d)
	}
	l := env.srv.Links()
	var slot0 bool
	for _, x := range l {
		if x.UUID == "" {
			slot0 = true
		}
	}
	if !slot0 {
		t.Fatal("unidentified link missing")
	}
	if _, _, ok := env.reg.Lookup(env.srv.ipKey(mustAddr("127.0.0.1"))); !ok {
		t.Fatal("unidentified station not keyed by its address key")
	}
}

func TestHooksAndClose(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	rec := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }
	hooks := Hooks{
		OnAttach:   func(model.StationIdx, Identity) { rec("attach") },
		OnDetach:   func(_ model.StationIdx, r string) { rec("detach:" + r) },
		OnInput:    func(_ model.StationIdx, up bool) { rec(fmt.Sprint("input:", up)) },
		OnPosition: func(_ model.StationIdx, lat, lon, alt float64) { rec(fmt.Sprintf("pos:%.1f,%.1f,%.0f", lat, lon, alt)) },
	}
	env := startServer(t, Options{Hooks: hooks}, nil, nil)
	c := dialClient(t, env.addr, defaultHello(uuidA))
	c.send(`{"input_connected":"connected"}`+"\n", `{"position_update":{"lat":50.1,"lon":8.7,"alt":120}}`+"\n",
		`{"input_disconnected":"disconnected"}`+"\n")
	waitFor(t, "hooks", 3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(calls) == 4 })
	env.srv.Close()
	if !c.waitClosed(3 * time.Second) {
		t.Fatal("not closed")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"attach", "input:true", "pos:50.1,8.7,120", "input:false", "detach:shutdown"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	if s := env.sink.session(0); s == nil || !s.closed || s.info.Proto != "mlatc" || len(s.stations) != 1 {
		t.Fatalf("session %+v", s)
	}
}
