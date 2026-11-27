package mlatc

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"airview/internal/ingest"
	"airview/internal/model"
)

// offlineHello is a handshake line for the Offline tests.
func offlineHello(t *testing.T, uuid string, extra map[string]any) []byte {
	t.Helper()
	h := defaultHello(uuid)
	h["compress"] = []string{"none"}
	for k, v := range extra {
		h[k] = v
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func replySplit(t *testing.T, reply []byte) bool {
	t.Helper()
	var r struct {
		Split *bool `json:"split_sync"`
	}
	if err := json.Unmarshal(reply, &r); err != nil || r.Split == nil {
		t.Fatalf("reply %q", reply)
	}
	return *r.Split
}

func newOfflineServer(t *testing.T, o Options) (*Server, *fakeSink, *MemRegistry) {
	t.Helper()
	reg, sink := NewMemRegistry(), newFakeSink()
	if o.Now == nil {
		clk := newTestClock()
		o.Now = clk.Now
	}
	srv, err := New(o, reg, sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		if reg.Attached() != 0 {
			t.Errorf("%d attachments left after Close", reg.Attached())
		}
	})
	return srv, sink, reg
}

const uuidH = "0123456789abcdef0123456789abcdef"

// TestSplitSyncDefaultAndFallback: split_sync is offered by default; a
// client whose single-sync messages are unusable is disconnected once and
// answered without split_sync from then on; On and Off are honoured.
func TestSplitSyncDefaultAndFallback(t *testing.T) {
	clk := newTestClock()
	srv, sink, _ := newOfflineServer(t, Options{Now: clk.Now})
	ip := netip.MustParseAddr("192.0.2.10")
	now := clk.Now()
	off, reply, err := srv.Offline(offlineHello(t, uuidH, nil), ip, now)
	if err != nil {
		t.Fatal(err)
	}
	if !replySplit(t, reply) {
		t.Fatal("split_sync not offered by default")
	}
	// valid single syncs count; frames reach the sink
	if err := off.Lines([]byte(`{"ssync":{"t":1000000,"m":"`+longHex+`"}}`+"\n"), now); err != nil {
		t.Fatal(err)
	}
	if l := srv.Links(); len(l) != 1 || !l[0].SplitSync || l[0].SSyncs != 1 || sink.n.Load() != 1 {
		t.Fatalf("links %+v frames %d", l, sink.n.Load())
	}
	off.Close()

	// a second client of another identity sends only broken ssync lines
	off2, _, err := srv.Offline(offlineHello(t, strings.Repeat("ab", 16), nil), ip, now)
	if err != nil {
		t.Fatal(err)
	}
	var bad strings.Builder
	for i := 0; i < splitBadMin+1; i++ {
		fmt.Fprintf(&bad, `{"ssync":{"et":%d,"em":"%s"}}`+"\n", 1000000+i, longHex) // not the expected fields
	}
	if err := off2.Lines([]byte(bad.String()), now); err != ErrOfflineClosed {
		t.Fatalf("broken ssync stream: err %v, want a disconnect", err)
	}
	off2.Close()
	if st := srv.Stats(); st.SplitFallbacks != 1 || st.Reasons[ReasonProtocol] != 1 {
		t.Fatalf("stats %+v", st)
	}
	clk.Add(time.Minute)
	off3, reply, err := srv.Offline(offlineHello(t, strings.Repeat("ab", 16), nil), ip, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if replySplit(t, reply) {
		t.Fatal("split_sync offered again after the fallback")
	}
	// paired sync still works for it
	if err := off3.Lines([]byte(fmt.Sprintf(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n",
		2000000, longHex, 8000000, strings.Replace(longHex, "8d", "8D", 1))), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	off3.Close()
	if got := srv.SplitSyncFallbacks(); len(got) != 1 {
		t.Fatalf("fallbacks %v", got)
	}
	// the fallback expires
	clk.Add(splitFallbackFor + time.Hour)
	off4, reply, err := srv.Offline(offlineHello(t, strings.Repeat("ab", 16), nil), ip, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !replySplit(t, reply) {
		t.Fatal("fallback did not expire")
	}
	off4.Close()

	for _, m := range []SplitSyncMode{SplitSyncOn, SplitSyncOff} {
		s, _, _ := newOfflineServer(t, Options{SplitSync: m})
		o, reply, err := s.Offline(offlineHello(t, uuidH, nil), ip, now)
		if err != nil {
			t.Fatal(err)
		}
		if replySplit(t, reply) != (m == SplitSyncOn) {
			t.Fatalf("mode %v: reply %s", m, reply)
		}
		o.Close()
	}
	if m, ok := ParseSplitSync("off"); !ok || m != SplitSyncOff || m.String() != "off" {
		t.Fatal("ParseSplitSync")
	}
	if _, ok := ParseSplitSync("maybe"); ok {
		t.Fatal("ParseSplitSync accepted garbage")
	}
}

// TestSplitSyncSilentFallback: a client offered split_sync that sends mlat
// frames and unknown messages but no sync data at all falls back too.
func TestSplitSyncSilentFallback(t *testing.T) {
	clk := newTestClock()
	srv, _, _ := newOfflineServer(t, Options{Now: clk.Now})
	ip := netip.MustParseAddr("192.0.2.11")
	off, _, err := srv.Offline(offlineHello(t, uuidH, nil), ip, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < splitUnknownMin; i++ {
		fmt.Fprintf(&b, `{"single_sync":{"t":%d,"m":"%s"}}`+"\n", 1000000+i, longHex)
		fmt.Fprintf(&b, `{"mlat":{"t":%d,"m":"%s"}}`+"\n", 1000500+i, shortHex)
	}
	if err := off.Lines([]byte(b.String()), clk.Now().Add(10*time.Second)); err != nil {
		t.Fatalf("fell back too early: %v", err)
	}
	if err := off.Lines([]byte(`{"mlat":{"t":2000000,"m":"`+shortHex+`"}}`+"\n"), clk.Now().Add(splitSilentAfter+time.Second)); err != ErrOfflineClosed {
		t.Fatalf("no fallback for a silent client: %v", err)
	}
	off.Close()
	if srv.Stats().SplitFallbacks != 1 {
		t.Fatal("fallback not counted")
	}
}

// TestSyncPairBackdated: the older half of a sync pair gets its reception
// host time (arrival minus the tick difference), the newer the arrival.
func TestSyncPairBackdated(t *testing.T) {
	srv, sink, _ := newOfflineServer(t, Options{SplitSync: SplitSyncOff})
	ip := netip.MustParseAddr("192.0.2.12")
	now := time.Unix(1_700_000_000, 0)
	off, _, err := srv.Offline(offlineHello(t, uuidH, nil), ip, now)
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	even, odd := uint64(50_000_000), uint64(50_000_000+18_000_000) // odd 1.5 s later at 12 MHz
	line := fmt.Sprintf(`{"sync":{"et":%d,"em":"%s","ot":%d,"om":"%s"}}`+"\n", even, longHex, odd,
		strings.Replace(longHex, "8d", "8D", 1))
	if err := off.Lines([]byte(line), now); err != nil {
		t.Fatal(err)
	}
	fr, _ := sink.snapshot()
	if len(fr) != 2 {
		t.Fatalf("frames %d", len(fr))
	}
	var older, newer model.RawFrame
	if fr[0].Ticks < fr[1].Ticks {
		older, newer = fr[0], fr[1]
	} else {
		older, newer = fr[1], fr[0]
	}
	if newer.HostNs != now.UnixNano() {
		t.Fatalf("newer half host %d, want the arrival %d", newer.HostNs, now.UnixNano())
	}
	if d := now.UnixNano() - older.HostNs; d < 1_499_000_000 || d > 1_501_000_000 {
		t.Fatalf("older half back-dated by %d ns, want 1.5 s", d)
	}
	// a single ssync / mlat frame keeps the arrival time
	if err := off.Lines([]byte(`{"mlat":{"t":90000000,"m":"`+shortHex+`"}}`+"\n"), now); err != nil {
		t.Fatal(err)
	}
	if fr, _ = sink.snapshot(); fr[2].HostNs != now.UnixNano() {
		t.Fatal("mlat frame back-dated")
	}
}

type quarSet map[model.StationIdx]bool

func (q quarSet) Quarantined(st model.StationIdx) bool { return q[st] }

// TestRateReportAndProbe: rate_report is kept per client (Links, ADSBRate,
// ADSBRates); a quarantined station in selective mode is asked for its
// highest-rate aircraft, a trusted one is not.
func TestRateReportAndProbe(t *testing.T) {
	q := quarSet{}
	srv, sink, reg := newOfflineServer(t, Options{Trust: q, ProbeAircraft: 2})
	sink.mode = ingest.ModeSelective
	ip := netip.MustParseAddr("192.0.2.13")
	now := time.Unix(1_700_000_000, 0)
	off, _, err := srv.Offline(offlineHello(t, uuidH, nil), ip, now)
	if err != nil {
		t.Fatal(err)
	}
	st := off.Station().Station
	q[st] = true
	rate := `{"rate_report":{"4840D6":2.0,"ABCDEF":0.5,"A00001":1.5}}` + "\n"
	if err := off.Lines([]byte(rate), now); err != nil {
		t.Fatal(err)
	}
	l := srv.Links()
	if len(l) != 1 || l[0].ADSBAircraft != 3 || l[0].ADSBRate != 4.0 {
		t.Fatalf("links %+v", l)
	}
	if r, ok := srv.ADSBRate(st, 0xabcdef); !ok || r != 0.5 {
		t.Fatalf("rate %v %v", r, ok)
	}
	if all, ok := srv.ADSBRates(st, nil); !ok || len(all) != 3 {
		t.Fatalf("rates %v", all)
	}
	out := string(off.Poll(now))
	if !strings.Contains(out, `"start_sending":["4840d6","a00001"]`) {
		t.Fatalf("probe requests: %q", out)
	}
	// proven: the next report clears the probe
	q[st] = false
	if err := off.Lines([]byte(rate), now); err != nil {
		t.Fatal(err)
	}
	if out := string(off.Poll(now)); !strings.Contains(out, `"stop_sending":["4840d6","a00001"]`) {
		t.Fatalf("probe not withdrawn: %q", out)
	}
	off.Close()
	_ = reg
}

// TestOfflineSession covers the Offline API: refusal, frames, results via
// Poll, Server.Close ending an open session.
func TestOfflineSession(t *testing.T) {
	srv, sink, reg := newOfflineServer(t, Options{})
	ip := netip.MustParseAddr("192.0.2.14")
	now := time.Unix(1_700_000_000, 0)
	if _, reply, err := srv.Offline([]byte(`{"version":1}`), ip, now); err == nil || !strings.Contains(string(reply), "deny") {
		t.Fatalf("version 1 accepted: %v %s", err, reply)
	}
	off, _, err := srv.Offline(offlineHello(t, uuidH, nil), ip, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := off.Lines([]byte(`{"mlat":{"t":1000000,"m":"`+shortHex+`"}}`), now); err != nil { // no final newline
		t.Fatal(err)
	}
	if sink.n.Load() != 1 {
		t.Fatalf("frames %d", sink.n.Load())
	}
	st := off.Station().Station
	if !srv.SendResult(st, Result{MLATFix: model.MLATFix{ICAO: 0x4840d6, TUnixNs: now.UnixNano(), Grade: model.GradeA}}) {
		t.Fatal("result refused")
	}
	if out := string(off.Poll(now)); !strings.Contains(out, `"result"`) {
		t.Fatalf("poll %q", out)
	}
	if l := srv.Links(); l[0].Results != 1 {
		t.Fatalf("results counter %d", l[0].Results)
	}
	if reg.Attached() != 1 {
		t.Fatal("not attached")
	}
	srv.Close() // ends the open session
	if err := off.Lines([]byte("{}\n"), now); err != ErrOfflineClosed {
		t.Fatalf("lines after Close: %v", err)
	}
	off.Close() // idempotent
}
