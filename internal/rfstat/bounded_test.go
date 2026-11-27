package rfstat

import (
	"context"
	"database/sql"
	"math"
	"runtime"
	"testing"
	"time"

	"airview/internal/store"
)

// mixedFrame fills f with frame number i of aircraft a (generation gen).
func mixedFrame(f *Frame, ts float64, a, gen, i int) {
	*f = NewFrame(at(ts))
	f.ICAO = 0x100000 + uint32(gen*1000+a)
	f.Signal = -20 - float64(a%30)
	f.DistKm = float64(a % 400)
	f.BearingDeg = float64(a*7) - math.Floor(float64(a*7)/360)*360
	f.ElevDeg = 1 + float64(a%10)
	f.TrackDeg = float64(i % 360)
	f.AltFt = 30000
	f.GPSNs = int64(ts*1e9) % 86400e9
	f.Feed, f.Antenna = 0, int8(a%2)
	f.Version, f.NIC, f.NACp, f.SIL = 2, 8, 9, 3
	switch i % 6 {
	case 0, 1:
		f.DF, f.CRC, f.Kind = 17, CRCOK, KindPosEven+Kind(i%2)
		f.Lat, f.Lon = 48+f.DistKm/111, 11
	case 2:
		f.DF, f.CRC, f.Kind = 17, CRCOK, KindVelocity
		f.GNSSDeltaFt = 250
	case 3:
		f.DF, f.CRC, f.Kind = 4, CRCAddrAccepted, KindSurvAlt
	case 4:
		f.DF, f.CRC, f.Kind, f.IID = 11, CRCOK, KindAllCall, uint8(16+a%5)
	case 5:
		f.DF, f.CRC, f.Kind, f.BDS = 20, CRCAddrAccepted, KindCommBAlt, 0x40
	}
}

func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1e6
}

// TestBoundedMemory: 1000 concurrent aircraft (each replaced every 20 min,
// 7000+ distinct addresses) for 2.5 simulated hours.
func TestBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	st := New(Config{})
	var f Frame
	const n = 1000
	var mid float64
	end := 9000
	for sec := 0; sec < end; sec++ {
		st.Tick(at(float64(sec)))
		for a := 0; a < n; a++ {
			// generation changes every 1200 s, staggered per aircraft
			gen := (sec + a) / 1200
			mixedFrame(&f, float64(sec)+float64(a)/n, a, gen, sec)
			st.Observe(f)
		}
		if sec == 3600 {
			_ = st.Report()
			mid = heapMB()
		}
		if sec%600 == 0 {
			st.mu.Lock()
			if st.tab.n > 1400 {
				t.Errorf("table holds %d aircraft at %d s", st.tab.n, sec)
			}
			st.mu.Unlock()
		}
	}
	rep := st.Report()
	checkHealthData(t, rep.Health)
	endMB := heapMB()
	if endMB-mid > 2 {
		t.Errorf("heap grew from %.1f MB to %.1f MB", mid, endMB)
	}
	if st.dropped != 0 {
		t.Errorf("dropped %d", st.dropped)
	}
	if len(st.History()) != 2 {
		t.Errorf("history %d", len(st.History()))
	}
	if rep.Census.Aircraft > 1400 || rep.Census.Aircraft < 1000 {
		t.Errorf("census aircraft %d", rep.Census.Aircraft)
	}
	mustJSON(t, rep)
	t.Logf("heap %.1f MB -> %.1f MB, report %d bytes", mid, endMB, len(mustJSON(t, rep)))
}

func TestTableFullDropsFrames(t *testing.T) {
	st := New(Config{MaxAircraft: 10})
	for a := 0; a < 20; a++ {
		f := NewFrame(at(1))
		f.DF, f.CRC, f.ICAO, f.Kind = 17, CRCOK, uint32(a+1), KindVelocity
		st.Observe(f)
	}
	if st.dropped != 10 || st.tab.n != 10 {
		t.Fatalf("dropped %d n %d", st.dropped, st.tab.n)
	}
	st.Tick(at(400)) // all time out
	if st.tab.n != 0 {
		t.Fatalf("not evicted: %d", st.tab.n)
	}
}

func TestObserveNoAlloc(t *testing.T) {
	st := New(Config{})
	var f Frame
	i := 0
	allocs := testing.AllocsPerRun(20000, func() {
		mixedFrame(&f, 100+float64(i)*0.001, i%500, 0, i)
		st.Observe(f)
		i++
	})
	if allocs != 0 {
		t.Fatalf("Observe allocates %.2f per call", allocs)
	}
}

func BenchmarkObserve(b *testing.B) {
	st := New(Config{})
	frames := make([]Frame, 6000)
	for i := range frames {
		mixedFrame(&frames[i], 100+float64(i)*0.0005, i%500, 0, i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := frames[i%len(frames)]
		// keep time moving forward slowly (1 ms per frame)
		f.Recv = simBase.Add(time.Duration(i) * time.Millisecond)
		st.Observe(f)
	}
}

func BenchmarkReport(b *testing.B) {
	st := New(Config{})
	var f Frame
	for i := 0; i < 200000; i++ {
		mixedFrame(&f, float64(i)*0.002, i%800, 0, i)
		st.Observe(f)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = st.Report()
	}
}

// TestPersistence writes closed hours through a real store and reads them back.
func TestPersistence(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := New(Config{})
	var f Frame
	for sec := 0; sec < 2*3600+5; sec += 2 {
		st.Tick(at(float64(sec)))
		mixedFrame(&f, float64(sec), sec%20, 0, sec)
		st.Observe(f)
	}
	ctx := context.Background()
	for len(st.pending) > 0 {
		st.write(ctx, db, <-st.pending)
	}
	if err := db.Flush(); err != nil { // queued for the group commit
		t.Fatal(err)
	}
	hs, err := LoadHistory(ctx, db.DB(), simBase, simBase.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].T != simBase.UnixMilli() || hs[1].Frames != 1800 {
		t.Fatalf("history %+v", hs)
	}
	var iid, replies int
	if err := db.DB().QueryRow(`SELECT iid, replies FROM rfstat_iid_hour WHERE t=? ORDER BY replies DESC LIMIT 1`, simBase.Unix()).Scan(&iid, &replies); err != nil {
		t.Fatal(err)
	}
	if iid < 16 || replies <= 0 {
		t.Errorf("iid row %d %d", iid, replies)
	}
	var w Writer = db
	_ = w
	_ = sql.ErrNoRows
}

func TestEmptyAndLateFrames(t *testing.T) {
	st := New(Config{})
	rep := st.Report()
	checkHealthData(t, rep.Health)
	mustJSON(t, rep)
	if rep.Health.Score != -1 || rep.Health.Status != "unknown" {
		t.Errorf("empty health %+v", rep.Health)
	}
	st.Tick(at(1000))
	f := NewFrame(at(10)) // 990 s late
	f.DF, f.CRC = 11, CRCOK
	st.Observe(f)
	st.Tick(at(1001))
	if c := st.Report().Channel; c.Recent.Frames[len(c.Recent.Frames)-1] != 1 {
		t.Errorf("late frame not counted in the current second")
	}
}

// TestRunStops checks the lifecycle: Run returns after cancel.
func TestRunStops(t *testing.T) {
	st := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
