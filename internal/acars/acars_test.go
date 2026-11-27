package acars

import (
	"context"
	"math/rand/v2"
	"net"
	"strings"
	"testing"
	"time"
)

var now = time.Unix(1_790_000_000, 0)

func TestParseAcarsdec(t *testing.T) {
	line := `{"timestamp":1790000001.5,"station_id":"PI4","channel":1,"freq":131.725,"level":-18.34,"error":0,` +
		`"mode":"2","label":"H1","block_id":"4","ack":false,"tail":".G-EUPT","flight":"ba0123","msgno":"M12A",` +
		`"text":"LINE1\r\nLINE2\u0007","end":true,"depa":"EGLL","dsta":"KJFK","icao":4202766,"app":{"name":"acarsdec","ver":"3.7"}}`
	m, err := Parse([]byte(line), now)
	if err != nil {
		t.Fatal(err)
	}
	want := Message{T: 1790000001500, Source: SrcAcarsdec, Link: LinkACARS, Station: "PI4", FreqMHz: 131.725,
		ICAO: "40210E", ICAOFrom: "message", Reg: "G-EUPT", Flight: "BA0123", Mode: "2", Label: "H1", BlockID: "4",
		MsgNo: "M12A", Dep: "EGLL", Dst: "KJFK", Text: "LINE1\nLINE2"}
	if m.LevelDB == nil || *m.LevelDB != -18.3 {
		t.Fatalf("level %v", m.LevelDB)
	}
	m.LevelDB = nil
	if m != want {
		t.Fatalf("got  %+v\nwant %+v", m, want)
	}
}

func TestParseAcarsdecUplinkStringICAO(t *testing.T) {
	m, err := Parse([]byte(`{"timestamp":1,"freq":131.525,"label":"RA","block_id":"B","ack":"5","tail":"N123AB","text":"HELLO","icao":"a1b2c3","end":false}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Uplink || m.ICAO != "A1B2C3" || m.Ack != "5" || !m.More || m.T != now.UnixMilli() {
		t.Fatalf("%+v", m)
	}
}

func TestParseVDL2(t *testing.T) {
	line := `{"vdl2":{"app":{"name":"dumpvdl2","ver":"2.3.0"},"station":"G5LKW","t":{"sec":1790000002,"usec":250000},` +
		`"freq":136975000,"sig_level":-21.07,"noise_level":-47.1,"avlc":{"src":{"addr":"4CA7B5","type":"Aircraft","status":"Airborne"},` +
		`"dst":{"addr":"1141C4","type":"Ground station"},"cr":"Command","frame_type":"I","acars":{"err":false,"crc_ok":true,"more":false,` +
		`"reg":".EI-DWF","mode":"2","label":"5Z","blk_id":"7","ack":"!","flight":"FR1234","msg_num":"M01","msg_num_seq":"A","msg_text":"/B6 TEST"}}}}`
	m, err := Parse([]byte(line), now)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != SrcDumpVDL2 || m.Link != LinkVDL2 || m.ICAO != "4CA7B5" || m.Uplink || m.Reg != "EI-DWF" ||
		m.MsgNo != "M01A" || m.FreqMHz != 136.975 || m.T != 1790000002250 || m.Text != "/B6 TEST" || m.Label != "5Z" {
		t.Fatalf("%+v", m)
	}
	// Ground to air: the aircraft is the destination.
	up := strings.Replace(strings.Replace(line, `"type":"Aircraft"`, `"type":"Ground station"`, 1), `"addr":"1141C4","type":"Ground station"`, `"addr":"4CA7B5","type":"Aircraft"`, 1)
	m, err = Parse([]byte(up), now)
	if err != nil || !m.Uplink || m.ICAO != "4CA7B5" {
		t.Fatalf("uplink %+v %v", m, err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]error{
		``:                     ErrNotJSON,
		`not json`:             ErrNotJSON,
		`{"foo":1}`:            ErrUnknown,
		`{"label":`:            ErrNotJSON,
		`{"vdl2":{"avlc":{}}}`: ErrNotACARS,
		`{"vdl2":{"avlc":{"src":{"addr":"1","type":"Aircraft"},"acars":{"crc_ok":false,"label":"H1"}}}}`: ErrBadMessage,
	}
	for in, want := range cases {
		if _, err := Parse([]byte(in), now); err != want {
			t.Errorf("%q: %v, want %v", in, err, want)
		}
	}
}

func TestCleanTextBounds(t *testing.T) {
	long := strings.Repeat("é", MaxTextBytes)
	if got := cleanText(long); len(got) > MaxTextBytes || !strings.HasPrefix(got, "é") {
		t.Fatalf("len %d", len(got))
	}
	if got := cleanText("a\xffb"); got != "a�b" {
		t.Fatalf("%q", got)
	}
}

func TestHex6AndNormReg(t *testing.T) {
	for in, want := range map[string]string{"abc": "000ABC", "4CA7B5": "4CA7B5", "0": "", "1234567": "", "xyz": ""} {
		if got := hex6(in); got != want {
			t.Errorf("hex6(%q)=%q", in, got)
		}
	}
	if NormReg(".g-eupt") != "GEUPT" {
		t.Fatal(NormReg(".g-eupt"))
	}
}

func TestStoreRingDupFilterStats(t *testing.T) {
	s := NewStore(MinKeep)
	s.SetResolver(func(r string) string {
		if r == "GEUPT" {
			return "400ABC"
		}
		return ""
	})
	mk := func(i int) Message {
		return Message{T: now.UnixMilli(), Source: SrcAcarsdec, Reg: "G-EUPT", Label: "H1", MsgNo: "M01A", Text: "T" + string(rune('A'+i%26)) + strings.Repeat("x", i)}
	}
	if !s.Add(mk(0), now) || s.Add(mk(0), now.Add(time.Second)) {
		t.Fatal("duplicate not detected")
	}
	if !s.Add(mk(0), now.Add(DupWindow+time.Second)) {
		t.Fatal("repeat after the window must be stored")
	}
	for i := 1; i < 250; i++ {
		s.Add(mk(i), now.Add(time.Minute))
	}
	st := s.Stats()
	if st.Kept != MinKeep || st.Total != 251 || st.Duplicates != 1 || st.Aircraft != 1 || st.Linked != MinKeep {
		t.Fatalf("%+v", st)
	}
	all := s.List(Filter{Limit: 1000})
	if len(all) != MinKeep || all[0].ID != 251 || all[len(all)-1].ID != 152 || all[0].ICAO != "400ABC" || all[0].ICAOFrom != "reg" {
		t.Fatalf("len %d first %+v", len(all), all[0])
	}
	if got := s.List(Filter{SinceID: 249}); len(got) != 2 {
		t.Fatalf("since: %d", len(got))
	}
	if got := s.List(Filter{Reg: "geupt", Text: "TZ", Limit: 3}); len(got) != 3 {
		t.Fatalf("reg/text: %d", len(got))
	}
	if got := s.List(Filter{ICAO: "400abd"}); len(got) != 0 {
		t.Fatal("icao filter")
	}
}

func TestListenerUDPAndTCP(t *testing.T) {
	s := NewStore(0)
	l := NewListener(s, ListenConfig{UDP: "127.0.0.1:0", TCP: "127.0.0.1:0"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()
	var st InputStatus
	for i := 0; i < 200; i++ {
		if st = l.Status(); st.UDP != "" && st.TCP != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.UDP == "" || st.TCP == "" {
		t.Fatalf("not listening: %+v", st)
	}
	u, err := net.Dial("udp", st.UDP)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = u.Write([]byte(`{"label":"Q0","tail":".N1","text":"udp"}` + "\n"))
	_ = u.Close()
	c, err := net.Dial("tcp", st.TCP)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte(`{"label":"Q0","tail":".N2","text":"tcp"}` + "\n" + `garbage` + "\n"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (s.Stats().Total < 2 || s.Stats().ParseErrors < 1) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.Stats(); got.Total != 2 || got.ParseErrors != 1 {
		t.Fatalf("%+v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with an open TCP connection")
	}
	_ = c.Close()
}

func TestRateLimit(t *testing.T) {
	s := NewStore(0)
	l := NewListener(s, ListenConfig{}, nil)
	t0 := now
	l.now = func() time.Time { return t0 }
	for i := 0; i < RateBurst+10; i++ {
		l.Ingest([]byte(`{"label":"Q0","tail":"N` + string(rune('A'+i%26)) + `","text":"` + strings.Repeat("y", i) + `"}`))
	}
	if st := s.Stats(); st.Total != RateBurst || st.Dropped != 10 {
		t.Fatalf("%+v", st)
	}
	t0 = t0.Add(time.Second)
	l.Ingest([]byte(`{"label":"Q0","tail":"NZ","text":"later"}`))
	if s.Stats().Total != RateBurst+1 {
		t.Fatal("bucket did not refill")
	}
}

func TestDemoMessage(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		m := DemoMessage(rnd, "400ABC", "G-ABCD", "BA0001", now)
		if m.Station != "DEMO" || m.Label == "" || (m.Uplink != isUplinkBlock(m.BlockID)) {
			t.Fatalf("%+v", m)
		}
	}
}

func BenchmarkParseVDL2(b *testing.B) {
	line := []byte(`{"vdl2":{"station":"X","t":{"sec":1790000002,"usec":0},"freq":136975000,"sig_level":-21,"avlc":{"src":{"addr":"4CA7B5","type":"Aircraft"},"dst":{"addr":"1141C4","type":"Ground station"},"acars":{"crc_ok":true,"reg":".EI-DWF","mode":"2","label":"H1","blk_id":"7","ack":"!","flight":"FR1234","msg_num":"M01","msg_num_seq":"A","msg_text":"` + strings.Repeat("ABCDEF ", 30) + `"}}}}`)
	for b.Loop() {
		if _, err := Parse(line, now); err != nil {
			b.Fatal(err)
		}
	}
}
