package federation

import (
	"fmt"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
)

// bigPicture is a 10k-aircraft local picture spread over Europe.
func bigPicture(n int) []model.Aircraft {
	out := make([]model.Aircraft, n)
	for i := range out {
		a := plane(avf.FormatICAO(uint32(0x100000+i), false), fmt.Sprintf("T%05d", i),
			35+float64(i%300)*0.1, -10+float64(i/300)*0.9, float64(1000+i%40000))
		a.Feeds = []string{"avf:x"}
		a.ADSBVersion, a.NIC, a.NACp = ip(2), ip(8), ip(9)
		a.IAS, a.Mach, a.MagHeading = f64(280), f64(0.78), f64(271.5)
		out[i] = a
	}
	return out
}

func benchItems(b *testing.B, n int) []publicdto.Aircraft {
	b.Helper()
	pic := bigPicture(n)
	snap := model.LiveSnapshot{Now: 1_700_000_000_000, Aircraft: pic}
	for i := range snap.Aircraft {
		snap.Aircraft[i].LastSeen = snap.Now - int64(i%1000)
	}
	prep := prepare(&snap, nil, nil)
	out := make([]publicdto.Aircraft, len(prep))
	for i := range prep {
		out[i] = prep[i].a
	}
	return out
}

func encodeAll(items []publicdto.Aircraft, path []string, buf []byte) (int, []byte) {
	total := 0
	for rest := items; len(rest) > 0; {
		var k int
		buf, k = AppendState(buf[:0], 1_700_000_000_000, false, path, rest)
		total += len(buf)
		rest = rest[k:]
	}
	return total, buf
}

func BenchmarkEncodeState10k(b *testing.B) {
	items := benchItems(b, 10_000)
	path := []string{"aaaaaaaaaaaaaaaa"}
	buf := make([]byte, 0, 64<<10)
	var sz int
	b.ReportAllocs()
	for b.Loop() {
		sz, buf = encodeAll(items, path, buf)
	}
	b.ReportMetric(float64(sz)/10_000, "B/aircraft")
}

func BenchmarkDecodeState10k(b *testing.B) {
	items := benchItems(b, 10_000)
	path := []string{"aaaaaaaaaaaaaaaa"}
	var recs [][]byte
	for rest := items; len(rest) > 0; {
		p, k := AppendState(nil, 1_700_000_000_000, false, path, rest)
		recs = append(recs, p)
		rest = rest[k:]
	}
	var scratch []publicdto.Aircraft
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range recs {
			sb, err := DecodeState(p, scratch)
			if err != nil {
				b.Fatal(err)
			}
			scratch = sb.Aircraft[:0]
		}
	}
}

// BenchmarkExportCycle10k: one export to one peer (prepare + policy +
// encode), the per-second work of a 10k-aircraft network per peer.
func BenchmarkExportCycle10k(b *testing.B) {
	pic := bigPicture(10_000)
	snap := model.LiveSnapshot{Now: 1_700_000_000_000, Aircraft: pic}
	pol := Policy{}
	pol.normalize()
	var prep []local
	buf := make([]byte, 0, 64<<10)
	items := make([]publicdto.Aircraft, 0, 10_000)
	b.ReportAllocs()
	for b.Loop() {
		prep = prepare(&snap, nil, prep)
		items = items[:0]
		var a publicdto.Aircraft
		for i := range prep {
			if pol.apply(&prep[i].a, prep[i].island, &a) {
				items = append(items, a)
			}
		}
		_, buf = encodeAll(items, []string{"aaaaaaaaaaaaaaaa"}, buf)
	}
}

func BenchmarkBeastFrame(b *testing.B) {
	id := newID(b)
	n, err := New(Config{Enabled: true, Feeds: []FeedConfig{{Name: "bx", Format: FormatBeast, Mode: ModeListen, Addr: "127.0.0.1:0"}}}, Options{Identity: id})
	if err != nil {
		b.Fatal(err)
	}
	f := n.feeds[0]
	allowed := map[uint32]struct{}{0x4840D6: {}}
	f.allowed.Store(&allowed)
	v := mustHex(df17Vector)
	frames := make([]model.RawFrame, 1024)
	for i := range frames {
		d := append([]byte(nil), v...)
		d[5], d[6] = byte(i), byte(i>>8) // distinct (CRC no longer checks: exercises the filter path)
		frames[i] = model.RawFrame{Kind: model.FrameModeSLong, Data: d}
	}
	frames[0].Data = v
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		n.HandleFrame(frames[i&1023])
		i++
		if i%4096 == 0 {
			f.bmu.Lock()
			f.pending = f.pending[:0]
			f.bmu.Unlock()
		}
	}
}

// TestScale10k: a 10,000-aircraft picture crosses a live federation link
// completely within a few export cycles; reports wire bytes per second.
// Budgets (i9-class core, generous for CI): encode < 30 ms and decode
// < 60 ms for 10k aircraft.
func TestScale10k(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ida, idb := newID(t), newID(t)
	a := startNode(t, ida, "a", "auto", Config{IntervalMs: 1000, Peers: []PeerConfig{{Name: "b", ID: idb.FeederID()}}}, Options{})
	b := startNode(t, idb, "b", "", Config{IntervalMs: 1000, Peers: []PeerConfig{{Name: "a", ID: ida.FeederID(), Addr: a.Addr().String()}}}, Options{})
	a.pic.set(bigPicture(10_000)...)
	start := time.Now()
	waitFor(t, 10*time.Second, "10k aircraft", func() bool { return b.store.len() == 10_000 })
	t.Logf("10k aircraft delivered in %v", time.Since(start).Round(time.Millisecond))
	w0 := peerState(a.Node, "b").WireOut
	time.Sleep(3 * time.Second)
	w1 := peerState(a.Node, "b").WireOut
	t.Logf("steady state: %.0f kB/s on the wire for 10k aircraft at 1 Hz (all changing)", float64(w1-w0)/3/1000)

	items := make([]publicdto.Aircraft, 0, 10_000)
	snap := a.pic.source()
	for _, l := range prepare(&snap, nil, nil) {
		items = append(items, l.a)
	}
	path := []string{ida.FeederID()}
	t0 := time.Now()
	var recs [][]byte
	for rest := items; len(rest) > 0; {
		p, k := AppendState(nil, snap.Now, false, path, rest)
		recs = append(recs, p)
		rest = rest[k:]
	}
	enc := time.Since(t0)
	t0 = time.Now()
	for _, p := range recs {
		if _, err := DecodeState(p, nil); err != nil {
			t.Fatal(err)
		}
	}
	decd := time.Since(t0)
	t.Logf("encode %v, decode %v for 10k aircraft", enc, decd)
	if enc > 30*time.Millisecond || decd > 60*time.Millisecond {
		t.Errorf("over budget: encode %v (30 ms), decode %v (60 ms)", enc, decd)
	}
}
