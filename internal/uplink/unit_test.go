package uplink

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"airview/internal/avf"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
)

func testRec(i int, hostNs int64) *rec {
	return &rec{typ: avf.TFrames, hostNs: hostNs, body: []byte(fmt.Sprintf("body-%06d", i))}
}

func TestSpoolRoundTripAckRewind(t *testing.T) {
	dir := t.TempDir()
	sp, err := openSpool(dir, 1<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := sp.Append(testRec(i, int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	if len(sp.segs) < 3 {
		t.Fatalf("expected several segments, got %d", len(sp.segs))
	}
	var got []*rec
	for i := 0; i < 400; i++ {
		r, ok := sp.Next()
		if !ok {
			t.Fatal("short read", i)
		}
		if !bytes.Equal(r.body, testRec(i, 0).body) || r.hostNs != int64(i) || r.typ != avf.TFrames {
			t.Fatalf("record %d: %q", i, r.body)
		}
		got = append(got, r)
	}
	// ack the first 100, rewind: reading resumes at 100
	now := time.Now()
	sp.Ack(got[99].spoolEnd, now)
	sp.Rewind()
	r, ok := sp.Next()
	if !ok || !bytes.Equal(r.body, testRec(100, 0).body) {
		t.Fatalf("after rewind: %q", r.body)
	}
	// persisted progress survives a reopen
	sp.Ack(r.spoolEnd, now.Add(time.Hour))
	if err := sp.Close(now); err != nil {
		t.Fatal(err)
	}
	sp, err = openSpool(dir, 1<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		r, ok := sp.Next()
		if !ok {
			break
		}
		if n == 0 && !bytes.Equal(r.body, testRec(101, 0).body) {
			t.Fatalf("reopen starts at %q", r.body)
		}
		n++
		sp.Ack(r.spoolEnd, now)
	}
	if n != 899 {
		t.Fatalf("read %d after reopen, want 899", n)
	}
	if sp.Bytes() != 0 || len(sp.segs) != 0 {
		t.Fatalf("fully acknowledged spool not deleted: %d bytes, %d segs", sp.Bytes(), len(sp.segs))
	}
	if ents, _ := filepath.Glob(filepath.Join(dir, "*"+segSuffix)); len(ents) != 0 {
		t.Fatal("segment files left:", ents)
	}
}

func TestSpoolSizeCapAndCorruption(t *testing.T) {
	dir := t.TempDir()
	sp, err := openSpool(dir, 20000, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		_ = sp.Append(testRec(i, int64(i)))
	}
	_ = sp.CloseWriter()
	if sp.Bytes() > 20000 {
		t.Fatalf("spool %d bytes over its cap", sp.Bytes())
	}
	if sp.droppedBytes == 0 {
		t.Fatal("no drop accounted")
	}
	// the newest records survive
	var last *rec
	for {
		r, ok := sp.Next()
		if !ok {
			break
		}
		last = r
	}
	if last == nil || !bytes.Equal(last.body, testRec(1999, 0).body) {
		t.Fatal("newest record lost")
	}
	// corrupt the tail of the last segment: reading stops cleanly there
	sp.Rewind()
	lastSeg := sp.segs[len(sp.segs)-1]
	p := sp.segPath(lastSeg.id)
	b, _ := os.ReadFile(p)
	b[len(b)-3] ^= 0xFF
	_ = os.WriteFile(p, b, 0o600)
	cnt := 0
	for {
		r, ok := sp.Next()
		if !ok {
			break
		}
		if bytes.Equal(r.body, testRec(1999, 0).body) {
			t.Fatal("corrupt record returned")
		}
		cnt++
	}
	if cnt == 0 {
		t.Fatal("nothing read before the corrupt record")
	}
}

func TestQueueConnectClassification(t *testing.T) {
	o := &Options{}
	o.normalize()
	q := newQueue(o, nil)
	now := time.Unix(10_000, 0)
	// 3 records sent before a drop: seq 1..3, data 2 min, 20 s and 5 s old
	for i, age := range []time.Duration{2 * time.Minute, 20 * time.Second, 5 * time.Second} {
		r := testRec(i, now.Add(-age).UnixNano())
		q.push(r)
		q.live.popFront()
		q.sent(r, uint64(i+1))
	}
	// unsent: an old RECEIVER (dropped), old frames (backfill), young frames (live)
	q.push(&rec{typ: avf.TReceiver, hostNs: now.Add(-time.Minute).UnixNano(), body: []byte("{}")})
	q.push(testRec(10, now.Add(-time.Minute).UnixNano()))
	q.push(testRec(11, now.Add(-time.Second).UnixNano()))
	q.disconnect()
	q.connect(1, true, now) // the server holds seq 1
	if q.inflight.len() != 0 {
		t.Fatal("inflight not cleared")
	}
	var live, back []string
	for _, r := range q.live.take() {
		live = append(live, string(r.body))
	}
	for _, r := range q.back.take() {
		back = append(back, string(r.body))
		if !r.late {
			t.Fatal("backfill record not late")
		}
	}
	wantLive := []string{"body-000001", "body-000002", "body-000011"}
	wantBack := []string{"body-000010"}
	if fmt.Sprint(live) != fmt.Sprint(wantLive) || fmt.Sprint(back) != fmt.Sprint(wantBack) {
		t.Fatalf("live %v back %v", live, back)
	}
}

func TestQueueRingLimitsSpill(t *testing.T) {
	dir := t.TempDir()
	o := &Options{RingAge: time.Minute, RingBytes: 1 << 20}
	o.normalize()
	sp, _ := openSpool(dir, 1<<20, 1<<16)
	t.Cleanup(func() { _ = sp.Close(time.Now()) })
	q := newQueue(o, sp)
	base := int64(1e18)
	for i := 0; i < 300; i++ { // 5 minutes of 1 record/s
		q.push(testRec(i, base+int64(i)*1e9))
	}
	if span := q.newest - q.oldestRing().hostNs; span > int64(time.Minute) {
		t.Fatalf("ring spans %v", time.Duration(span))
	}
	if q.spilled == 0 || q.dropped != 0 {
		t.Fatalf("spilled %d dropped %d", q.spilled, q.dropped)
	}
	// without a spool the same data is dropped
	q2 := newQueue(o, nil)
	for i := 0; i < 300; i++ {
		q2.push(testRec(i, base+int64(i)*1e9))
	}
	if q2.dropped == 0 {
		t.Fatal("nothing dropped without a spool")
	}
	// while connected, ring overflow is dropped, not spilled (SD protection)
	q3 := newQueue(o, sp)
	q3.connected = true
	before := q3.sp.written
	for i := 0; i < 300; i++ {
		q3.push(testRec(i, base+int64(i)*1e9))
	}
	if q3.sp.written != before || q3.dropped == 0 {
		t.Fatal("spool written while connected")
	}
}

func df17(icao uint32, me uint64) []byte {
	f := make([]byte, 14)
	f[0] = 0x8D
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	for i := 0; i < 7; i++ {
		f[4+i] = byte(me >> (48 - 8*i))
	}
	modes.SetParity(f, 0)
	return f
}

func df11(icao uint32) []byte {
	f := make([]byte, 7)
	f[0] = 0x5D
	f[1], f[2], f[3] = byte(icao>>16), byte(icao>>8), byte(icao)
	modes.SetParity(f, 0)
	return f
}

func df4(icao uint32) []byte {
	f := make([]byte, 7)
	f[0] = 0x20
	f[1], f[2], f[3] = 0x00, 0x13, 0x37
	modes.SetParity(f, icao)
	return f
}

func TestFrameAddr(t *testing.T) {
	if a, df := frameAddr(df17(0x4840D6, 0x58C382D690C8AC)); a != 0x4840D6 || df != 17 {
		t.Fatalf("%x %d", a, df)
	}
	if a, df := frameAddr(df4(0xABCDEF)); a != 0xABCDEF || df != 4 {
		t.Fatalf("%x %d", a, df)
	}
	if a, df := frameAddr([]byte{0x12, 0x34}); a>>32 != 1 || df != 255 {
		t.Fatal(a, df)
	}
}

func TestSelectiveFilter(t *testing.T) {
	f := newSelFilter()
	f.apply(&avf.Interest{Mode: avf.ModeSelective, ReduceMs: 500, SyncAdd: []string{"4840D6"}, MLATAdd: []string{"ABCDEF"}})
	if !f.active() {
		t.Fatal("not active")
	}
	t0 := int64(1e18)
	sync := df17(0x4840D6, 1)
	ml := df4(0xABCDEF)
	other := df17(0x111111, 2)
	n := map[string]int{}
	for i := 0; i < 100; i++ { // 10 s at 10 Hz
		h := t0 + int64(i)*100e6
		if f.pass(sync, h) {
			n["sync"]++
		}
		if f.pass(ml, h) {
			n["mlat"]++
		}
		if f.pass(other, h) {
			n["other"]++
		}
	}
	if n["sync"] != 100 || n["mlat"] != 100 || n["other"] != 20 {
		t.Fatal(n)
	}
	f.apply(&avf.Interest{Reset: true, Mode: avf.ModeFull})
	if f.active() || len(f.sync) != 0 {
		t.Fatal("reset/full not applied")
	}
	// slow factor halves the budget of a 100 fps stream
	f.slow = 0.5
	passed := 0
	for i := 0; i < 2000; i++ {
		h := t0 + int64(i)*10e6
		f.observe(h)
		if f.pass(df17(uint32(i%50), uint64(i)), h) && i >= 1000 {
			passed++
		}
	}
	if passed > 600 {
		t.Fatalf("slow factor not applied: %d of 1000 passed", passed)
	}
}

func TestMinuteStats(t *testing.T) {
	var m minuteStats
	t0 := int64(1_763_000_040) * 1e9 // a minute start
	for i := 0; i < 10; i++ {
		if _, done := m.add(df17(0x4840D6, 0x58C382D690C8AC), -10.5, t0+int64(i)*1e9, 1); done {
			t.Fatal("early finish")
		}
	}
	bad := df17(0x4840D7, 0x58C382D690C8AC)
	bad[13] ^= 1
	m.add(bad, math.NaN(), t0+11e9, 1)
	m.add(df11(0x400F01), -60, t0+12e9, 1)
	m.add([]byte{0x12, 0x34}, -3, t0+13e9, 1)
	ms, done := m.add(df11(0x400F01), -20, t0+60e9, 1)
	if !done {
		t.Fatal("minute not finished")
	}
	if ms.Minute != t0/1e9 || ms.DF["17"] != 11 || ms.DF["11"] != 1 || ms.ModeAC != 1 || ms.ICAOs != 2 ||
		ms.Positions != 10 || ms.CRCFail != 1 || ms.RSSI.Lo != -50 || ms.RSSI.Counts[39] != 10 || ms.RSSI.Counts[0] != 1 {
		t.Fatalf("%+v", ms)
	}
	if m.minute != t0/1e9+60 || m.frames != 1 {
		t.Fatal("next minute not started")
	}
}

func TestFixUpdate(t *testing.T) {
	llh := geo.LLH{Lat: 51.5, Lon: -0.1, H: 10000}
	p := llh.ECEF()
	fix := model.MLATFix{ICAO: 0x4840D6, TUnixNs: 1_763_000_000_000_000_000, ECEF: [3]float64{p.X, p.Y, p.Z},
		Grade: model.GradeA, Vel: &[3]float32{100, 100, 5}}
	// 30 m horizontal sigma, 50 m vertical, expressed in ECEF
	fr := geo.NewFrame(llh)
	c := fr.CovToECEF([9]float64{900, 0, 0, 0, 900, 0, 0, 0, 2500})
	fix.Cov = [6]float32{float32(c[0]), float32(c[1]), float32(c[2]), float32(c[4]), float32(c[5]), float32(c[8])}
	u := FixUpdate(&fix, "uplink/club", time.Now())
	if math.Abs(*u.Lat-51.5) > 1e-7 || math.Abs(*u.Lon+0.1) > 1e-7 || math.Abs(*u.AltGeom-32808) > 1 {
		t.Fatalf("%v %v %v", *u.Lat, *u.Lon, *u.AltGeom)
	}
	if math.Abs(*u.SigmaM-math.Sqrt(1800)) > 0.1 {
		t.Fatalf("sigma %v", *u.SigmaM)
	}
	if math.Abs(*u.Track-45) > 1e-6 || math.Abs(*u.GS-math.Hypot(100, 100)*mpsToKt) > 1e-6 || math.Abs(*u.VertRate-984) > 1 {
		t.Fatal(*u.Track, *u.GS, *u.VertRate)
	}
	if !u.MLAT || u.Src != model.SrcMLATAV || !IsInjectedFeed(u.FeedID) || u.Recv.UnixNano() != fix.TUnixNs {
		t.Fatalf("%+v", u)
	}
}

func TestIdentityFiles(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.FeederID != b.FeederID || a.UUID != b.UUID || !model.ValidID(a.FeederID) || a.UUID == "" {
		t.Fatal(a.FeederID, b.FeederID, a.UUID)
	}
	c, err := NewIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.FeederID == a.FeederID || c.UUID != a.UUID {
		t.Fatal("new-identity must change the key and keep the UUID")
	}
}

func TestRootOnSD(t *testing.T) {
	if !rootOnSD("/dev/mmcblk0p2 / ext4 rw 0 0\n/dev/mmcblk0p1 /boot vfat rw 0 0\n") {
		t.Fatal("SD root not detected")
	}
	if rootOnSD("/dev/sda2 / ext4 rw 0 0\n/dev/mmcblk0p1 /boot vfat rw 0 0\n") {
		t.Fatal("USB root reported as SD")
	}
}

func TestBackoff(t *testing.T) {
	b := newBackoff(time.Second, 60*time.Second)
	var last time.Duration
	for i := 0; i < 10; i++ {
		last = b.next()
		if last < 800*time.Millisecond || last > 72*time.Second {
			t.Fatal(last)
		}
	}
	if last < 48*time.Second {
		t.Fatal("did not reach the maximum", last)
	}
	b.reset()
	if d := b.next(); d > 1200*time.Millisecond {
		t.Fatal(d)
	}
}

// After a seq-space rotation every held record has seq 0: none of them may
// be taken as acknowledged by the new space's ackSeq 0.
func TestQueueConnectAfterRotation(t *testing.T) {
	o := &Options{}
	o.normalize()
	q := newQueue(o, nil)
	now := time.Unix(10_000, 0)
	for i := 0; i < 3; i++ {
		r := testRec(i, now.Add(-time.Second).UnixNano())
		q.push(r)
		q.live.popFront()
		q.sent(r, uint64(i+5))
	}
	q.disconnect()
	q.resetSeqs()
	q.connect(0, true, now)
	if q.live.len() != 3 {
		t.Fatalf("%d records left after a rotation, want 3", q.live.len())
	}
}

// A seq-0 placeholder (an unsendable spool record) between held records
// does not stop the trimming of what the server acknowledged.
func TestQueueConnectPlaceholder(t *testing.T) {
	o := &Options{}
	o.normalize()
	q := newQueue(o, nil)
	now := time.Unix(10_000, 0)
	h := now.Add(-time.Second).UnixNano()
	a, ph, b, c := testRec(1, h), testRec(2, h), testRec(3, h), testRec(4, h)
	q.sent(a, 1)
	q.inflight.push(ph) // seq 0
	q.sent(b, 2)
	q.sent(c, 3)
	q.disconnect()
	q.connect(2, true, now)
	var live []string
	for _, r := range q.live.take() {
		live = append(live, string(r.body))
	}
	if fmt.Sprint(live) != "[body-000002 body-000004]" {
		t.Fatalf("live after connect: %v", live)
	}
}

// C3: records sent before an outage longer than RingAge are not spilled to
// the spool; the WELCOME's ackSeq settles them, so none is backfilled again.
func TestInflightKeptThroughOutage(t *testing.T) {
	dir := t.TempDir()
	o := &Options{RingAge: time.Minute, RingBytes: 1 << 20}
	o.normalize()
	sp, _ := openSpool(dir, 1<<20, 1<<16)
	t.Cleanup(func() { _ = sp.Close(time.Now()) })
	q := newQueue(o, sp)
	q.connected = true
	base := int64(1e18)
	for i := 0; i < 5; i++ { // sent with seq 1..5, not yet acknowledged
		r := testRec(i, base+int64(i)*1e8)
		q.push(r)
		q.live.popFront()
		q.sent(r, uint64(i+1))
	}
	q.disconnect()
	for i := 0; i < 180; i++ { // 3 minutes of data during the outage
		q.push(testRec(100+i, base+int64(time.Second)+int64(i)*1e9))
	}
	if q.spilled == 0 {
		t.Fatal("the outage spilled nothing")
	}
	q.connect(5, true, time.Unix(0, base+int64(200*time.Second)))
	for r := q.nextBackfill(); r != nil; r = q.nextBackfill() {
		var n int
		if _, err := fmt.Sscanf(string(r.body), "body-%06d", &n); err == nil && n < 5 {
			t.Fatalf("acknowledged record %d backfilled again", n)
		}
	}
}
