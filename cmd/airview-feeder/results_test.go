package main

import (
	"bufio"
	"math"
	"net"
	"testing"
	"time"

	"airview/internal/feed"
	"airview/internal/mlat/geo"
	"airview/internal/model"
	"airview/internal/modes"
)

func fixAt(icao uint32, lat, lon, h float64, vel *[3]float32) model.MLATFix {
	p := geo.ToECEF(lat, lon, h)
	return model.MLATFix{ICAO: icao, TUnixNs: time.Now().UnixNano(), ECEF: [3]float64{p.X, p.Y, p.Z},
		Cov: [6]float32{400, 0, 0, 400, 0, 400}, Vel: vel, Grade: model.GradeB}
}

// parseBeast parses a Beast byte stream with the production parser.
func parseBeast(b []byte) []feed.BeastFrame {
	var out []feed.BeastFrame
	p := feed.NewBeastParser(func(f *feed.BeastFrame) {
		c := *f
		c.Payload = append([]byte(nil), f.Payload...)
		out = append(out, c)
	})
	p.Write(b)
	return out
}

func TestEncodeFixDecodes(t *testing.T) {
	vel := [3]float32{100, -50, -5}                    // east 100 m/s, south 50 m/s, descending 5 m/s
	fix := fixAt(0x1a1a1a, 51.47, -0.4543, 3000, &vel) // ICAO full of escape bytes
	b := encodeFix(nil, &fix, time.Now())
	frames := parseBeast(b)
	if len(frames) != 3 {
		t.Fatalf("frames %d", len(frames))
	}
	var even, odd [2]uint32
	for i, f := range frames {
		if !feed.IsMLATTimestamp(f.Timestamp) {
			t.Fatalf("frame %d: timestamp %x is not the MLAT magic", i, f.Timestamp)
		}
		m, err := modes.DecodeFrame(f.Payload)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if m.DF != 17 || m.ICAO != 0x1a1a1a || m.CRC != modes.CRCOK {
			t.Fatalf("frame %d: %+v", i, m)
		}
		me := uint64(0)
		for _, x := range f.Payload[4:11] {
			me = me<<8 | uint64(x)
		}
		tc := me >> 51
		switch i {
		case 0, 1:
			if tc != 22 { // sigma ~28 m
				t.Fatalf("type code %d", tc)
			}
			cl, co := uint32(me>>17)&0x1FFFF, uint32(me)&0x1FFFF
			if (me>>34)&1 == 1 {
				odd = [2]uint32{cl, co}
			} else {
				even = [2]uint32{cl, co}
			}
			ac := uint16(me>>36) & 0xFFF
			code, _ := modes.EncodeAC12(int32(math.Round(3000 * 3.280839895)))
			if ac != code {
				t.Fatalf("altitude field %x want %x", ac, code)
			}
		case 2:
			if tc != 19 || (me>>48)&7 != 1 {
				t.Fatalf("velocity tc %d st %d", tc, (me>>48)&7)
			}
			sew, vew := (me>>42)&1, (me>>32)&0x3FF
			sns, vns := (me>>31)&1, (me>>21)&0x3FF
			svr, vr := (me>>19)&1, (me>>10)&0x1FF
			if sew != 0 || math.Abs(float64(vew-1)-194.4) > 1 || sns != 1 || math.Abs(float64(vns-1)-97.2) > 1 {
				t.Fatalf("speed fields %d %d %d %d", sew, vew, sns, vns)
			}
			if svr != 1 || math.Abs(float64(vr-1)*64-984) > 64 {
				t.Fatalf("vertical rate %d %d", svr, vr)
			}
		}
	}
	lat, lon, ok := modes.CPRAirborneGlobal(even[0], even[1], odd[0], odd[1], true)
	if !ok || math.Abs(lat-51.47) > 0.001 || math.Abs(lon+0.4543) > 0.001 {
		t.Fatalf("CPR decode %.5f %.5f %v", lat, lon, ok)
	}
}

func TestPosTypeCode(t *testing.T) {
	if posTypeCode(5) != 20 || posTypeCode(20) != 21 || posTypeCode(100) != 22 || posTypeCode(0) != 22 {
		t.Fatal("type codes")
	}
}

func TestResultsOutputs(t *testing.T) {
	// a fake readsb Beast input
	rl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	got := make(chan []byte, 4)
	go func() {
		c, err := rl.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		got <- buf[:n]
	}()

	r := newResultsOut(t.Logf)
	defer r.close()
	port := freePort(t)
	r.apply(model.FeederResults{BeastPort: port, ReadsbAddr: rl.Addr().String()})
	if r.listenAddr() == "" {
		t.Fatalf("not listening: %+v", r.status())
	}
	c, err := net.Dial("tcp", r.listenAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, 5*time.Second, func() bool {
		s := r.status()
		return s.Clients == 1 && s.ReadsbUp
	})
	r.handle("u", []model.MLATFix{fixAt(0x4ca123, 53, -6, 10000, nil), {ICAO: 0x123}}) // second has no usable position? (ECEF 0)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := bufio.NewReader(c).Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if fr := parseBeast(buf[:n]); len(fr) < 2 || !feed.IsMLATTimestamp(fr[0].Timestamp) {
		t.Fatalf("listener frames: %d", len(fr))
	}
	select {
	case b := <-got:
		if fr := parseBeast(b); len(fr) < 2 {
			t.Fatalf("readsb frames %d", len(fr))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing pushed to readsb")
	}
	if s := r.status(); s.Received != 2 || s.Frames < 4 {
		t.Fatalf("status %+v", s)
	}
	// turning the output off closes the listener
	r.apply(model.FeederResults{})
	if r.listenAddr() != "" || r.status().ReadsbAddr != "" {
		t.Fatal("outputs still on")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func BenchmarkEncodeFix(b *testing.B) {
	vel := [3]float32{100, 50, 0}
	f := fixAt(0x4ca123, 53, -6, 10000, &vel)
	now := time.Now()
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = encodeFix(buf[:0], &f, now)
	}
}
