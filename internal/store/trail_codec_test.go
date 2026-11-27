package store

import (
	"math"
	"math/rand/v2"
	"os"
	"reflect"
	"testing"
	"time"
)

// synthTrail builds a plausible trail: a smooth track with noise, gaps,
// NULL runs and source changes.
func synthTrail(rng *rand.Rand, fid int64, n int) []TrailRow {
	pts := make([]TrailRow, 0, n)
	t := int64(1_700_000_000 + rng.IntN(1_000_000))
	lat, lon := 5_000_000+rng.Int64N(100_000), 800_000+rng.Int64N(100_000)
	vlat, vlon := rng.Int64N(800)-400, rng.Int64N(800)-400
	alt := int64(rng.IntN(40_000))
	for i := 0; i < n; i++ {
		t += 5
		if rng.IntN(40) == 0 {
			t += int64(rng.IntN(600))
		}
		vlat += rng.Int64N(21) - 10
		vlon += rng.Int64N(21) - 10
		lat += vlat
		lon += vlon
		alt += int64(rng.IntN(201) - 100)
		r := TrailRow{FlightID: fid, T: t, LatE5: lat, LonE5: lon, Src: int64(rng.IntN(2) * rng.IntN(2))}
		if rng.IntN(10) != 0 {
			r.Alt, r.Has = alt, r.Has|TrailHasAlt
		}
		if rng.IntN(5) != 0 {
			r.GS, r.Has = int64(200+rng.IntN(300)), r.Has|TrailHasGS
			r.Track, r.Has = int64(rng.IntN(360)), r.Has|TrailHasTrack
		}
		if rng.IntN(3) != 0 {
			r.RSSIX10, r.Has = -int64(rng.IntN(400)), r.Has|TrailHasRSSI
		}
		pts = append(pts, r)
	}
	return pts
}

func TestTrailCodecRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 2, 3, 17, 255, 256, 1000} {
		for _, level := range []int{trailLevelNone, trailLevelSeal, trailLevelCompact} {
			pts := synthTrail(rng, 42, n)
			blob := encodeTrailChunk(nil, pts, level)
			got, err := decodeTrailChunk(nil, 42, blob)
			if err != nil {
				t.Fatalf("n=%d level=%d: %v", n, level, err)
			}
			if len(got) != len(pts) || (n > 0 && !reflect.DeepEqual(got, pts)) {
				t.Fatalf("n=%d level=%d: round trip differs", n, level)
			}
		}
	}
	// Extreme values.
	pts := []TrailRow{
		{FlightID: 7, T: math.MinInt64 / 4, LatE5: math.MaxInt64 / 4, LonE5: math.MinInt64 / 4, Src: -3},
		{FlightID: 7, T: math.MaxInt64 / 4, LatE5: math.MinInt64 / 4, LonE5: math.MaxInt64 / 4, Src: 1 << 40,
			Has: TrailHasAlt | TrailHasRSSI, Alt: math.MaxInt64 / 4, RSSIX10: math.MinInt64 / 4},
		{FlightID: 7, T: 0, Has: 0x0f, Alt: -1, GS: -1, Track: -1, RSSIX10: -1},
	}
	got, err := decodeTrailChunk(nil, 7, encodeTrailChunk(nil, pts, trailLevelSeal))
	if err != nil || !reflect.DeepEqual(got, pts) {
		t.Fatalf("extremes: %v %v", err, got)
	}
	// Appends to dst.
	pre := []TrailRow{{FlightID: 1, T: 5}}
	got, err = decodeTrailChunk(pre, 7, encodeTrailChunk(nil, pts, 0))
	if err != nil || len(got) != 4 || got[0].T != 5 || !reflect.DeepEqual(got[1:], pts) {
		t.Fatal("append to dst broken")
	}
}

func TestTrailCodecCorrupt(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	pts := synthTrail(rng, 1, 100)
	for _, level := range []int{0, 1} {
		blob := encodeTrailChunk(nil, pts, level)
		for i := 0; i < len(blob); i++ {
			for _, cut := range []bool{false, true} {
				b := append([]byte(nil), blob...)
				if cut {
					b = b[:i]
				} else {
					b[i] ^= 0x55
				}
				_, _ = decodeTrailChunk(nil, 1, b) // must not panic
			}
		}
	}
	if _, err := decodeTrailChunk(nil, 1, nil); err == nil {
		t.Fatal("empty blob accepted")
	}
	if _, err := decodeTrailChunk(nil, 1, []byte{9, 1, 2}); err == nil {
		t.Fatal("unknown codec accepted")
	}
}

func FuzzTrailCodec(f *testing.F) {
	rng := rand.New(rand.NewPCG(5, 6))
	for _, n := range []int{0, 1, 5, 40} {
		f.Add(encodeTrailChunk(nil, synthTrail(rng, 1, n), 0))
		f.Add(encodeTrailChunk(nil, synthTrail(rng, 1, n), 1))
	}
	f.Fuzz(func(t *testing.T, blob []byte) {
		pts, err := decodeTrailChunk(nil, 1, blob)
		if err != nil {
			return
		}
		// Whatever decodes must re-encode to something that decodes equal.
		again, err := decodeTrailChunk(nil, 1, encodeTrailChunk(nil, pts, 1))
		if err != nil || len(again) != len(pts) || (len(pts) > 0 && !reflect.DeepEqual(again, pts)) {
			t.Fatalf("re-encode differs: %v", err)
		}
	})
}

func TestTrailRowPosition(t *testing.T) {
	p := Position{FlightID: 3, T: 100, Lat: 50.123456, Lon: -8.000014, Alt: 35000.4, GS: math.NaN(), Track: 359.6,
		RSSI: -12.34, Src: SrcMLAT}
	r, ok := trailRowFromPosition(&p)
	if !ok || r.LatE5 != 5012346 || r.LonE5 != -800001 || r.Alt != 35000 || r.Has&TrailHasGS != 0 ||
		r.Track != 360 || r.RSSIX10 != -123 || r.Src != 1 {
		t.Fatalf("%+v", r)
	}
	q := r.Position()
	if q.Lat != 50.12346 || q.Lon != -8.00001 || q.Alt != 35000 || !math.IsNaN(q.GS) || q.RSSI != -12.3 {
		t.Fatalf("%+v", q)
	}
	if _, ok := trailRowFromPosition(&Position{FlightID: 1, Lat: 91}); ok {
		t.Fatal("invalid latitude accepted")
	}
}

// TestTrailCodecGen7 measures the codec on a real generator database:
// AIRVIEW_TRAILDB=<copy of airview.db with a 1.0.0 positions table>.
func TestTrailCodecGen7(t *testing.T) {
	path := os.Getenv("AIRVIEW_TRAILDB")
	if path == "" {
		t.Skip("AIRVIEW_TRAILDB not set")
	}
	s, err := OpenWithOptions(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := db.Query(`SELECT ` + positionCols + ` FROM positions ORDER BY flight_id, t`)
	if err != nil {
		t.Fatal(err)
	}
	var all [][]TrailRow
	var cur []TrailRow
	var npts int
	for rs.Next() {
		r, err := scanTrailRow(rs)
		if err != nil {
			t.Fatal(err)
		}
		if len(cur) > 0 && (cur[0].FlightID != r.FlightID || len(cur) == trailChunkMax) {
			all = append(all, cur)
			cur = nil
		}
		cur = append(cur, r)
		npts++
	}
	if len(cur) > 0 {
		all = append(all, cur)
	}
	rs.Close()
	for _, level := range []int{trailLevelNone, trailLevelSeal, trailLevelCompact} {
		var bytes int
		var buf []byte
		var dec []TrailRow
		t0 := time.Now()
		for _, c := range all {
			buf = encodeTrailChunk(buf[:0], c, level)
			bytes += len(buf)
		}
		enc := time.Since(t0)
		blobs := make([][]byte, len(all))
		for i, c := range all {
			blobs[i] = encodeTrailChunk(nil, c, level)
		}
		t1 := time.Now()
		for i, b := range blobs {
			dec, err = decodeTrailChunk(dec[:0], all[i][0].FlightID, b)
			if err != nil {
				t.Fatal(err)
			}
		}
		decT := time.Since(t1)
		for i, b := range blobs {
			dec, _ = decodeTrailChunk(dec[:0], all[i][0].FlightID, b)
			if !reflect.DeepEqual(dec, all[i]) {
				t.Fatalf("chunk %d differs", i)
			}
		}
		t.Logf("level %d: %d points, %d chunks, %.2f B/pt, encode %.1f ns/pt, decode %.1f ns/pt", level, npts,
			len(all), float64(bytes)/float64(npts), float64(enc.Nanoseconds())/float64(npts),
			float64(decT.Nanoseconds())/float64(npts))
	}
}
