package server

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"airview/internal/privacy/publicdto"
)

// synthPicture returns n moving aircraft at time step t (seconds).
func synthPicture(n, t int) publicdto.Snapshot {
	s := publicdto.Snapshot{Now: 1_700_000_000_000 + int64(t)*1000, Aircraft: make([]publicdto.Aircraft, n)}
	f := func(v float64) *float64 { return &v }
	for i := range s.Aircraft {
		ang := float64(i) * 0.37
		gs := 250 + float64(i%200)
		d := gs * 0.514 * float64(t) / 111_000 // degrees moved
		s.Aircraft[i] = publicdto.Aircraft{
			ICAO: fmt.Sprintf("%06x", 0x400000+i), Callsign: fmt.Sprintf("TST%04d", i%10000), Squawk: "1000",
			Category: "A3", Registration: "G-TEST", TypeCode: "A320", Operator: "Test Air", Country: "United Kingdom",
			CountryCode: "GB", Source: "adsb",
			Lat: f(50 + float64(i%100)*0.05 + d*math.Cos(ang)), Lon: f(5 + float64(i/100)*0.05 + d*math.Sin(ang)),
			PosAge: f(0.4), AltBaro: f(30000 + float64(i%40)*100), AltGeom: f(30500 + float64(i%40)*100),
			VertRate: f(0), GS: f(gs), Track: f(math.Mod(ang*57.3, 360)), NIC: new(int), NACp: new(int),
			FirstSeen: s.Now - 600_000, LastSeen: s.Now - 200,
		}
	}
	return s
}

// applyDelta is the client side of a delta frame (live.js onDelta).
func applyDelta(t *testing.T, state map[string]map[string]any, seq *uint64, frame []byte) {
	t.Helper()
	data := strings.TrimSuffix(strings.SplitN(string(frame), "\ndata: ", 2)[1], "\n\n")
	var d struct {
		Seq, Base uint64
		Upd       []map[string]any
		Del       []string
	}
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatal(err)
	}
	if d.Base != *seq {
		t.Fatalf("delta base %d, client at %d", d.Base, *seq)
	}
	for _, u := range d.Upd {
		icao := u["icao"].(string)
		a := state[icao]
		if a == nil {
			a = map[string]any{}
			state[icao] = a
		}
		for k, v := range u {
			if v == nil {
				delete(a, k)
			} else {
				a[k] = v
			}
		}
	}
	for _, icao := range d.Del {
		delete(state, icao)
	}
	*seq = d.Seq
}

func pictureState(t *testing.T, s publicdto.Snapshot) map[string]map[string]any {
	t.Helper()
	b, _ := json.Marshal(s.Aircraft)
	var list []map[string]any
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, a := range list {
		out[a["icao"].(string)] = a
	}
	return out
}

// SO-1: a client applying the deltas holds exactly the server's picture,
// and at 10k aircraft a delta is a fraction of the full picture.
func TestPublicDeltaStream(t *testing.T) {
	const n = 10000
	var enc deltaEncoder
	enc.advance(synthPicture(n, 0), 1)
	kf := enc.keyframeFrame()
	var key struct {
		publicdto.Snapshot
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.SplitN(string(kf), "\ndata: ", 2)[1], "\n\n")), &key); err != nil {
		t.Fatal(err)
	}
	state, seq := pictureState(t, key.Snapshot), key.Seq
	full := len(kf)
	total := 0
	for step := 1; step <= 5; step++ {
		pic := synthPicture(n, step)
		if step == 3 { // one aircraft leaves, one loses its squawk
			pic.Aircraft = pic.Aircraft[1:]
			pic.Aircraft[0].Squawk = ""
		}
		d := enc.advance(pic, uint64(step+1))
		if d == nil {
			t.Fatal("no delta for a new picture")
		}
		if enc.advance(pic, uint64(step+1)) != nil {
			t.Fatal("the same picture produced a second delta")
		}
		total += len(d)
		applyDelta(t, state, &seq, d)
		if want := pictureState(t, publicQuantise(pic)); !reflect.DeepEqual(state, want) {
			t.Fatalf("step %d: client state differs from the picture", step)
		}
	}
	perSec := total / 5
	t.Logf("10k aircraft: keyframe %d kB, delta %d kB/s (%.0f%%)", full/1000, perSec/1000, 100*float64(perSec)/float64(full))
	if perSec > full/4 {
		t.Fatalf("delta %d bytes/s is more than a quarter of the full picture (%d)", perSec, full)
	}
}

// SO-1: the public stream with ?delta=1 starts with a keyframe (with seq)
// and continues with delta events; a full public stream never gets the
// same picture twice.
func TestPublicEventsDeltaAndNoDuplicate(t *testing.T) {
	f := newNetFixture(t, rigOptions{})
	f.waitTraffic(3)
	// The fixture's hub does not run: drive its ticks by hand.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(400 * time.Millisecond):
				f.srv.live.tickFrames(false)
			}
		}
	}()
	defer close(stop)
	body := string(f.readSSE(t, "/api/public/events?delta=1", "198.51.100.30:1", 3500*time.Millisecond))
	ki := strings.Index(body, "event: aircraft")
	di := strings.Index(body, "event: delta")
	if ki < 0 || di < 0 || di < ki {
		t.Fatalf("keyframe %d delta %d:\n%.600s", ki, di, body)
	}
	if !strings.Contains(body[ki:di], `"seq":`) {
		t.Fatalf("keyframe without seq: %.300s", body[ki:])
	}
	plain := string(f.readSSE(t, "/api/public/events", "198.51.100.31:1", 1500*time.Millisecond))
	var prev string
	for _, blk := range strings.Split(plain, "\n\n") {
		if strings.HasPrefix(blk, "event: aircraft") {
			if blk == prev {
				t.Fatal("the same picture was sent twice")
			}
			prev = blk
		}
	}
}
