package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"airview/internal/privacy/publicdto"
)

// europePicture returns n moving aircraft over Europe at time step t (s):
// 70 % in a dense core around 49N 8E (σ 6° x 9°), 30 % spread evenly over
// 35..70N, 25W..45E - roughly the traffic pattern of a 10k-aircraft
// network at its busiest. Each aircraft flies a great-circle-ish straight
// line at its ground speed, a tenth of them climb or descend.
func europePicture(n, t int) publicdto.Snapshot {
	rng := rand.New(rand.NewPCG(42, 7))
	s := publicdto.Snapshot{Now: 1_700_000_000_000 + int64(t)*1000, Aircraft: make([]publicdto.Aircraft, n)}
	f := func(v float64) *float64 { return &v }
	for i := range s.Aircraft {
		var lat, lon float64
		if rng.Float64() < 0.7 {
			lat, lon = 49+rng.NormFloat64()*6, 8+rng.NormFloat64()*9
		} else {
			lat, lon = 35+rng.Float64()*35, -25+rng.Float64()*70
		}
		trk := rng.Float64() * 360
		gs := 180 + rng.Float64()*320
		vr := 0.0
		alt := 3000 + math.Round(rng.Float64()*38)*1000
		if i%10 == 0 {
			vr = 1500 * (rng.Float64()*2 - 1)
		}
		d := gs * 1852 / 3600 * float64(t) / 111_320 // degrees along track
		lat += d * math.Cos(trk*math.Pi/180)
		lon += d * math.Sin(trk*math.Pi/180) / math.Cos(lat*math.Pi/180)
		alt += vr * float64(t) / 60
		s.Aircraft[i] = publicdto.Aircraft{
			ICAO: fmt.Sprintf("%06x", 0x300000+i), Callsign: fmt.Sprintf("EU%05d", i), Squawk: "2000",
			Category: "A3", TypeCode: "A320", Country: "Germany", CountryCode: "DE", Source: "adsb",
			Lat: f(lat), Lon: f(lon), PosAge: f(0.4), AltBaro: f(alt), AltGeom: f(alt + 400),
			VertRate: f(vr), GS: f(gs), Track: f(trk), NIC: new(int), NACp: new(int),
			FirstSeen: s.Now - 600_000, LastSeen: s.Now - 200,
		}
		if i%9 == 0 { // no position (Mode S only)
			s.Aircraft[i].Lat, s.Aircraft[i].Lon = nil, nil
		}
	}
	return s
}

// frameData returns the JSON of an SSE frame.
func frameData(t *testing.T, frame []byte) (event string, data []byte) {
	t.Helper()
	s := string(frame)
	if !strings.HasPrefix(s, "event: ") {
		t.Fatalf("not an event: %.80q", s)
	}
	parts := strings.SplitN(strings.TrimPrefix(s, "event: "), "\ndata: ", 2)
	return parts[0], []byte(strings.TrimSuffix(parts[1], "\n\n"))
}

// viewClient is the client side of a viewport delta stream (live.js).
type viewClient struct {
	state map[string]map[string]any
	seq   uint64
}

func (vc *viewClient) apply(t *testing.T, frame []byte) {
	t.Helper()
	ev, data := frameData(t, frame)
	switch ev {
	case "aircraft":
		var k struct {
			Aircraft []map[string]any
			Seq      uint64
		}
		if err := json.Unmarshal(data, &k); err != nil {
			t.Fatal(err)
		}
		vc.state = map[string]map[string]any{}
		for _, a := range k.Aircraft {
			vc.state[a["icao"].(string)] = a
		}
		vc.seq = k.Seq
	case "delta":
		applyDelta(t, vc.state, &vc.seq, frame)
	default:
		t.Fatalf("unexpected event %s", ev)
	}
}

// checkView verifies the client state against the encoder's picture: every
// held aircraft equals the picture's aircraft (all members), every
// positioned aircraft inside the box is held, and nothing outside the
// padded box is held.
func checkView(t *testing.T, step int, d *deltaEncoder, c *liveClient, vc *viewClient) {
	t.Helper()
	want := pictureState(t, d.keep)
	pad := c.box.padded()
	for icao, a := range vc.state {
		w, ok := want[icao]
		if !ok {
			t.Fatalf("step %d: client holds %s, which left the picture", step, icao)
		}
		if !reflect.DeepEqual(a, w) {
			t.Fatalf("step %d: %s differs:\n got %v\nwant %v", step, icao, a, w)
		}
		lat, lon := a["lat"].(float64), a["lon"].(float64)
		if !pad.contains(lat, lon) {
			t.Fatalf("step %d: %s at %.3f,%.3f is outside the padded box", step, icao, lat, lon)
		}
		if _, ok := c.inView[icao]; !ok {
			t.Fatalf("step %d: client holds %s, server does not know", step, icao)
		}
	}
	if len(vc.state) != len(c.inView) {
		t.Fatalf("step %d: client holds %d, server thinks %d", step, len(vc.state), len(c.inView))
	}
	for icao, w := range want {
		lat, ok1 := w["lat"].(float64)
		lon, ok2 := w["lon"].(float64)
		if ok1 && ok2 && c.box.contains(lat, lon) {
			if _, ok := vc.state[icao]; !ok {
				t.Fatalf("step %d: %s at %.3f,%.3f is in the box but not held", step, icao, lat, lon)
			}
		}
	}
}

// SO-1 viewport: a client applying keyframe + viewport deltas holds exactly
// the picture's aircraft in (and near) its box, through moving traffic,
// aircraft leaving the picture, a lost frame and viewport changes
// (including one across the antimeridian and the whole world).
func TestViewDeltaChain(t *testing.T) {
	const n = 4000
	var d deltaEncoder
	d.advance(europePicture(n, 0), 1)
	c := &liveClient{id: 1, delta: true, view: true, box: &geoBox{s: 48, w: 2, n: 52, e: 12}}
	vc := &viewClient{}
	vc.apply(t, d.viewKeyframe(c))
	checkView(t, 0, &d, c, vc)
	if len(vc.state) < 100 {
		t.Fatalf("only %d aircraft in the test box", len(vc.state))
	}
	boxes := map[int]*geoBox{
		12: {s: 40, w: -5, n: 46, e: 6},        // pan
		20: nil,                                // whole world
		24: {s: 30, w: 170, n: 60, e: -170},    // antimeridian (empty here)
		27: {s: 50.5, w: 7.5, n: 51.5, e: 9.5}, // zoom in
		33: {s: -90, w: -180, n: 90, e: 180},   // explicit world
		36: {s: 45, w: 0, n: 55, e: 15},        // back
	}
	for step := 1; step <= 40; step++ {
		pic := europePicture(n, step*5) // 5 s per step: aircraft cross cells quickly
		if step%7 == 0 {                // some aircraft leave the picture
			pic.Aircraft = pic.Aircraft[step:]
		}
		if b, ok := boxes[step]; ok {
			c.box = b
		}
		d.advance(pic, uint64(step+1))
		if step == 16 {
			// A lost frame: the hub sets needKey, the client gets a keyframe.
			_ = d.viewDelta(c)
			vc.apply(t, d.viewKeyframe(c))
		} else {
			f := d.viewDelta(c)
			if f == nil {
				t.Fatalf("step %d: no frame", step)
			}
			vc.apply(t, f)
			if d.viewDelta(c) != nil {
				t.Fatalf("step %d: second frame for the same seq", step)
			}
		}
		checkView(t, step, &d, c, vc)
	}
}

// SO-1 viewport: the encoder's index answers box queries exactly like a
// scan, for random boxes including antimeridian ones and huge ones.
func TestViewIndexQuery(t *testing.T) {
	pic := europePicture(3000, 0)
	// Some aircraft near the antimeridian and the poles.
	for i := 0; i < 200; i++ {
		lat, lon := -89.5+float64(i)*0.9, 179.9-float64(i%20)*0.02
		if i%2 == 1 {
			lon = -lon
		}
		pic.Aircraft[i].Lat, pic.Aircraft[i].Lon = &lat, &lon
	}
	var x viewIndex
	x.build(&pic)
	rng := rand.New(rand.NewPCG(1, 2))
	for k := 0; k < 2000; k++ {
		s := -90 + rng.Float64()*180
		nn := math.Min(90, s+rng.Float64()*rng.Float64()*120)
		w := -180 + rng.Float64()*360
		e := -180 + rng.Float64()*360
		if k%3 == 0 {
			e = math.Min(180, w+rng.Float64()*10)
		}
		b := &geoBox{s: s, w: w, n: nn, e: e}
		var got, want []int32
		x.query(b, func(i int32) { got = append(got, i) })
		for i := range x.ents {
			if b.contains(x.ents[i].lat, x.ents[i].lon) {
				want = append(want, int32(i))
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("box %+v: %d hits, scan %d", *b, len(got), len(want))
		}
	}
}

// The padded box contains the box and wraps correctly.
func TestViewPadded(t *testing.T) {
	cases := []struct{ in, want geoBox }{
		{geoBox{50, 0, 52, 10}, geoBox{49.8, -1, 52.2, 11}},
		{geoBox{-90, 170, 90, -170}, geoBox{-90, 168, 90, -168}},
		{geoBox{0, -180, 1, 180}, geoBox{-0.1, -180, 1.1, 180}},
		{geoBox{10, 179.5, 10.01, 179.98}, geoBox{9.95, 179.45, 10.06, -179.97}},
	}
	for _, c := range cases {
		got := c.in.padded()
		if math.Abs(got.s-c.want.s) > 1e-9 || math.Abs(got.n-c.want.n) > 1e-9 ||
			math.Abs(got.w-c.want.w) > 1e-9 || math.Abs(got.e-c.want.e) > 1e-9 {
			t.Errorf("padded(%+v) = %+v, want %+v", c.in, *got, c.want)
		}
	}
}

// gzSize is the size of b gzip-compressed as one stream event (the SSE
// streams flush the compressor after every event).
func gzSize(zw *gzip.Writer, buf *bytes.Buffer, b []byte) int {
	n0 := buf.Len()
	_, _ = zw.Write(b)
	_ = zw.Flush()
	return buf.Len() - n0
}

// SO-1 acceptance: SSE bandwidth per viewer at 10k simulated aircraft with a
// typical viewport, against the unfiltered delta stream and full frames.
// Map views of a 1920x1080 browser window centred on 51N 6E at Web
// Mercator zoom 6..9. Reported raw and gzip-compressed (per-event flush,
// as served).
func TestViewBandwidth10k(t *testing.T) {
	const n, steps = 10000, 30
	views := []struct {
		name string
		box  *geoBox
	}{{"whole picture", nil}}
	for _, z := range []int{6, 7, 8, 9} {
		lonSpan := 1920 * 360 / (256 * math.Exp2(float64(z)))
		latSpan := 1080 * 360 / (256 * math.Exp2(float64(z))) * math.Cos(51*math.Pi/180)
		views = append(views, struct {
			name string
			box  *geoBox
		}{fmt.Sprintf("zoom %d (%.1f° x %.1f°)", z, lonSpan, latSpan),
			&geoBox{s: 51 - latSpan/2, w: 6 - lonSpan/2, n: 51 + latSpan/2, e: 6 + lonSpan/2}})
	}
	pics := make([]publicdto.Snapshot, steps+1)
	for i := range pics {
		pics[i] = europePicture(n, i)
	}
	var full deltaEncoder
	full.advance(pics[0], 1)
	fullKey := len(full.keyframeFrame())
	var fullFrames int
	for i := 1; i <= steps; i++ {
		f, _ := marshal(&pics[i])
		fullFrames += len(sseFrame("aircraft", f))
	}
	var report []string
	report = append(report, fmt.Sprintf("full frames (no delta, no viewport): %.0f kB/s", float64(fullFrames)/steps/1000))
	type res struct {
		key, keyGz, perSec, perSecGz, held int
	}
	results := map[string]res{}
	for _, v := range views {
		var d deltaEncoder
		d.advance(pics[0], 1)
		c := &liveClient{id: 1, delta: true, view: true, box: v.box}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		kf := d.viewKeyframe(c)
		r := res{key: len(kf), keyGz: gzSize(zw, &buf, kf)}
		vc := &viewClient{}
		vc.apply(t, kf)
		var tot, totGz, held int
		for i := 1; i <= steps; i++ {
			d.advance(pics[i], uint64(i+1))
			f := d.viewDelta(c)
			vc.apply(t, f)
			tot += len(f)
			totGz += gzSize(zw, &buf, f)
			held += len(c.inView)
		}
		checkView(t, steps, &d, c, vc)
		r.perSec, r.perSecGz, r.held = tot/steps, totGz/steps, held/steps
		results[v.name] = r
		report = append(report, fmt.Sprintf("%-28s aircraft held %5d  keyframe %6.1f kB (gzip %6.1f)  delta %6.1f kB/s (gzip %5.1f kB/s)",
			v.name, r.held, float64(r.key)/1000, float64(r.keyGz)/1000, float64(r.perSec)/1000, float64(r.perSecGz)/1000))
	}
	t.Logf("10k aircraft, %d s:\n%s\n(unfiltered keyframe via the shared encoder: %.0f kB)", steps, strings.Join(report, "\n"), float64(fullKey)/1000)
	whole, z8 := results["whole picture"], results[views[3].name]
	if z8.perSec*10 > whole.perSec {
		t.Errorf("a zoom-8 viewport saves too little: %d B/s against %d B/s", z8.perSec, whole.perSec)
	}
}

// SO-1 viewport over HTTP: ?bbox= opens a viewport stream with a "stream"
// event, a filtered keyframe and filtered deltas; POST /api/events/view
// changes the viewport; ?replace= closes the old stream of a token; a bad
// bbox is refused.
func TestViewStreamHTTP(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(3)
	h := f.srv.live
	// A box around the traffic.
	snap, _, _ := f.srv.publicSnapshotGen()
	var lat, lon float64
	found := false
	for _, a := range snap.Aircraft {
		if a.Lat != nil && a.Lon != nil {
			lat, lon, found = *a.Lat, *a.Lon, true
			break
		}
	}
	if !found {
		t.Skip("no positioned aircraft in the fixture")
	}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
				h.tickFrames(false)
			}
		}
	}()
	defer close(stop)
	q := fmt.Sprintf("bbox=%.4f,%.4f,%.4f,%.4f", lat-1, lon-1, lat+1, lon+1)
	body := string(f.readSSE(t, "/api/public/events?delta=1&"+q, "198.51.100.40:1", 3500*time.Millisecond))
	si := strings.Index(body, "event: stream\ndata: {\"id\":\"")
	ki := strings.Index(body, "event: aircraft")
	di := strings.Index(body, "event: delta")
	if si < 0 || ki < si || di < ki {
		t.Fatalf("stream %d keyframe %d delta %d:\n%.600s", si, ki, di, body)
	}
	// Every aircraft of the keyframe is inside the box.
	_, data := frameData(t, []byte(body[ki:strings.Index(body[ki:], "\n\n")+ki+2]))
	var kf publicdto.Snapshot
	if err := json.Unmarshal(data, &kf); err != nil {
		t.Fatal(err)
	}
	box := &geoBox{lat - 1, lon - 1, lat + 1, lon + 1}
	for _, a := range kf.Aircraft {
		if a.Lat == nil || !box.contains(*a.Lat, *a.Lon) {
			t.Fatalf("keyframe aircraft %s outside the box", a.ICAO)
		}
	}
	if len(kf.Aircraft) == 0 {
		t.Fatal("empty keyframe")
	}
	// Full public stream with a box far away: no aircraft.
	far := string(f.readSSE(t, "/api/public/events?bbox=-60,-60,-59,-59", "198.51.100.41:1", 1500*time.Millisecond))
	if !strings.Contains(far, `"aircraft":[]`) {
		t.Fatalf("far box: %.400s", far)
	}
	// Bad box.
	req := httptest.NewRequest("GET", "/api/public/events?bbox=1,2,3", nil)
	req.RemoteAddr = "198.51.100.42:1"
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad bbox: %d", rec.Code)
	}

	// Viewport change and replace, at hub level.
	c := &liveClient{ch: make(chan []byte, 64), ev: make(chan []byte, 8), kick: make(chan struct{}), id: 1 << 40,
		ip: "198.51.100.43", forcePublic: true, delta: true, needKey: true, view: true, token: "t0k", box: box}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, c); h.mu.Unlock() }()
	vc := &viewClient{}
	drain := func(d time.Duration) {
		end := time.After(d)
		for {
			select {
			case fr := <-c.ch:
				vc.apply(t, fr)
			case <-end:
				return
			}
		}
	}
	drain(1500 * time.Millisecond)
	if vc.state == nil {
		t.Fatal("no keyframe")
	}
	inBox := len(vc.state)
	rec = httptest.NewRecorder()
	rawEventsView(f.srv, rec, httptest.NewRequest("POST", "/api/events/view?id=t0k", nil)) // whole world
	if rec.Code != http.StatusNoContent {
		t.Fatalf("view: %d %s", rec.Code, rec.Body.String())
	}
	drain(2500 * time.Millisecond)
	h.dmu.Lock()
	world := len(h.denc.idx.ents)
	h.dmu.Unlock()
	if len(vc.state) < world*9/10 || len(vc.state) < inBox {
		t.Fatalf("after widening: %d held, picture has %d positioned (box had %d)", len(vc.state), world, inBox)
	}
	rec = httptest.NewRecorder()
	rawEventsView(f.srv, rec, httptest.NewRequest("POST", "/api/events/view?id=nope&bbox=1,1,2,2", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}
	h.mu.Lock()
	h.replaceLocked("t0k")
	_, still := h.clients[c]
	h.mu.Unlock()
	select {
	case <-c.kick:
	default:
		t.Fatal("replace did not close the old stream")
	}
	if still {
		t.Fatal("replaced stream still registered")
	}
}

// The viewport keyframe and delta of the hub are consistent when built for
// many clients at once (frame cache keys are per client).
func TestViewFrameKeysDistinct(t *testing.T) {
	h := newLiveHub(nil)
	var keys []string
	for i := 0; i < 3; i++ {
		c := &liveClient{id: uint64(i + 1), delta: true, view: true, forcePublic: true}
		k, _ := h.viewDeltaFrameFor(c)
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			t.Fatal("two viewport clients share a frame")
		}
	}
}

// BenchmarkViewDelta is the per-client cost of one viewport delta (10k
// aircraft, zoom-8 view, about 650 aircraft held): what the hub spends per
// viewport viewer and second.
func BenchmarkViewDelta(b *testing.B) {
	const n = 10000
	pics := []publicdto.Snapshot{europePicture(n, 0), europePicture(n, 1), europePicture(n, 2)}
	var d deltaEncoder
	d.advance(pics[0], 1)
	d.advance(pics[1], 2)
	box := &geoBox{s: 49.15, w: 0.75, n: 52.85, e: 11.25}
	c := &liveClient{id: 1, delta: true, view: true, box: box}
	_ = d.viewKeyframe(c)
	d.advance(pics[2], 3)
	b.ReportAllocs()
	b.ResetTimer()
	bytes := 0
	for i := 0; i < b.N; i++ {
		c.viewSeq = d.seq - 1 // rebuild the same delta
		bytes += len(d.viewDelta(c))
	}
	b.ReportMetric(float64(bytes)/float64(b.N), "B/frame")
}
