package mlatc

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func parseAll(t *testing.T, line string) []message {
	t.Helper()
	var p parser
	if err := p.begin([]byte(line)); err != nil {
		t.Fatalf("begin %q: %v", line, err)
	}
	var out []message
	for {
		more, err := p.next()
		if err != nil {
			t.Fatalf("next %q: %v", line, err)
		}
		if !more {
			return out
		}
		m := p.msg
		m.addrs = append([]uint32(nil), m.addrs...)
		m.rates = append([]AircraftRate(nil), m.rates...)
		for i := range m.m {
			m.m[i] = append([]byte(nil), m.m[i]...)
		}
		out = append(out, m)
	}
}

func TestParseFrames(t *testing.T) {
	long := "8d4840d6202cc371c32ce0576098"
	short := "5d4840d6c0ffee"
	ms := parseAll(t, `{"mlat":{"t":123456789012,"m":"`+short+`"}}`)
	if len(ms) != 1 || ms[0].kind != kMLAT || ms[0].n != 1 || !ms[0].tok[0] || ms[0].t[0] != 123456789012 ||
		hex.EncodeToString(ms[0].m[0]) != short {
		t.Fatalf("mlat: %+v", ms)
	}
	ms = parseAll(t, ` { "sync" : { "et" : 10 , "em":"`+long+`", "ot":20,"om":"`+strings.ToUpper(long)+`" , "x":[1,{"y":null}]} }  `+"\r\n")
	m := ms[0]
	if m.kind != kSync || m.n != 2 || m.t != [2]uint64{10, 20} || hex.EncodeToString(m.m[0]) != long || hex.EncodeToString(m.m[1]) != long {
		t.Fatalf("sync: %+v", m)
	}
	ms = parseAll(t, `{"ssync":{"m":"`+long+`","t":1.5e3}}`)
	if ms[0].kind != kSSync || ms[0].t[0] != 1500 || !ms[0].tok[0] {
		t.Fatalf("ssync float t: %+v", ms[0])
	}
	// negative or fractional timestamps are not usable; bad hex is nil
	ms = parseAll(t, `{"mlat":{"t":-5,"m":"zz"}}`)
	if ms[0].tok[0] || ms[0].m[0] != nil {
		t.Fatalf("bad values accepted: %+v", ms[0])
	}
	ms = parseAll(t, `{"mlat":{"t":2.5,"m":"`+short+`"}}`)
	if ms[0].tok[0] {
		t.Fatal("fractional tick accepted")
	}
	// max uint64 and a 20-digit overflow
	ms = parseAll(t, `{"mlat":{"t":18446744073709551615,"m":"`+short+`"}}`)
	if !ms[0].tok[0] || ms[0].t[0] != 18446744073709551615 {
		t.Fatalf("max uint64: %+v", ms[0])
	}
	ms = parseAll(t, `{"mlat":{"t":99999999999999999999,"m":"`+short+`"}}`)
	if ms[0].tok[0] {
		t.Fatal("overflow accepted")
	}
	// Mode A/C frame (2 bytes)
	ms = parseAll(t, `{"mlat":{"t":7,"m":"1234"}}`)
	if len(ms[0].m[0]) != 2 {
		t.Fatal("mode a/c frame")
	}
}

func TestParseControl(t *testing.T) {
	ms := parseAll(t, `{"seen":["4840d6","ABCDEF","x",7,"1234567"]}`)
	if ms[0].kind != kSeen || len(ms[0].addrs) != 3 || ms[0].addrs[0] != 0x4840d6 || ms[0].addrs[1] != 0xabcdef || ms[0].addrs[2] != 7 || ms[0].skipped != 2 {
		t.Fatalf("seen: %+v", ms[0])
	}
	ms = parseAll(t, `{"lost":[]}`)
	if ms[0].kind != kLost || len(ms[0].addrs) != 0 {
		t.Fatal("lost")
	}
	ms = parseAll(t, `{"rate_report":{"4840D6":1.25,"ABCDEF":0,"zz":3,"4840d7":-1}}`)
	if ms[0].kind != kRate || len(ms[0].rates) != 2 || ms[0].rates[0].Addr != 0x4840d6 || ms[0].rates[0].Rate != 1.25 {
		t.Fatalf("rates: %+v", ms[0])
	}
	ms = parseAll(t, `{"clock_reset":{"reason":"Decoder mode changed to RADARCAPE","frequency":1000000000,"epoch":"utc_midnight","mode":"RADARCAPE"}}`)
	m := ms[0]
	if m.kind != kClockReset || m.freq != 1e9 || m.epoch != "utc_midnight" || m.mode != "RADARCAPE" || !strings.Contains(m.reason, "RADARCAPE") {
		t.Fatalf("clock_reset: %+v", m)
	}
	ms = parseAll(t, `{"clock_reset":{"reason":"Epoch rollover detected"}}`)
	if ms[0].mode != "" || ms[0].freq != 0 || ms[0].reason != "Epoch rollover detected" {
		t.Fatalf("rollover: %+v", ms[0])
	}
	ms = parseAll(t, `{"clock_reset":{"reason":"sample_drop","epoch":7,"frequency":12000000,"mode":"sdr12"}}`)
	if ms[0].epoch != "7" || ms[0].mode != "sdr12" {
		t.Fatalf("numeric epoch: %+v", ms[0])
	}
	for line, k := range map[string]msgKind{
		`{"clock_jump":true}`:                        kClockJump,
		`{"input_connected":"connected"}`:            kInputConnected,
		`{"input_disconnected":{}}`:                  kInputDisconnected,
		`{"heartbeat":{"client_time":1700000000.5}}`: kHeartbeat,
		`{"whatever":{"a":[1,2,{"b":"c"}]}}`:         kUnknown,
	} {
		if ms := parseAll(t, line); len(ms) != 1 || ms[0].kind != k {
			t.Fatalf("%s: %+v", line, ms)
		}
	}
	ms = parseAll(t, `{"position_update":{"lat":51.5,"lon":-0.1,"alt":45}}`)
	if !ms[0].hasPos || ms[0].lat != 51.5 || ms[0].lon != -0.1 || ms[0].alt != 45 {
		t.Fatalf("position: %+v", ms[0])
	}
	// two keys in one object
	ms = parseAll(t, `{"seen":["000001"],"lost":["000002"]}`)
	if len(ms) != 2 || ms[0].kind != kSeen || ms[1].kind != kLost {
		t.Fatalf("two keys: %+v", ms)
	}
	// escaped strings
	ms = parseAll(t, `{"clock_reset":{"reason":"a\"b\\cé😀"}}`)
	if ms[0].reason != "a\"b\\cé😀" {
		t.Fatalf("escapes: %q", ms[0].reason)
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		``, `[]`, `{`, `{"mlat":}`, `{"mlat":{"t":1,}}`, `{"a":1,}`, `{"a":01}`, `{"a":1.}`, `{"a":.5}`,
		`{"a":tru}`, `{"a":"x\q"}`, "{\"a\":\"\x01\"}", `{"a":1} x`, `{'a':1}`, `{"a":NaN}`, `{"a":[1,]}`,
		`{"a":1}{"b":2}`, `{"a" 1}`, strings.Repeat(`{"a":[`, 40) + strings.Repeat(`]}`, 40),
	}
	for _, s := range bad {
		var p parser
		err := p.begin([]byte(s))
		for err == nil {
			var more bool
			more, err = p.next()
			if !more && err == nil {
				break
			}
		}
		if err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestParseZeroAlloc(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"mlat":{"t":123456789012,"m":"5d4840d6c0ffee"}}`),
		[]byte(`{"sync":{"et":10,"em":"8d4840d6202cc371c32ce0576098","ot":20,"om":"8d4840d6202cc371c32ce0576098"}}`),
	}
	var p parser
	n := testing.AllocsPerRun(1000, func() {
		for _, l := range lines {
			if p.begin(l) != nil {
				t.Fatal("begin")
			}
			for {
				more, err := p.next()
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
		}
	})
	if n != 0 {
		t.Fatalf("%v allocs per run", n)
	}
}

// FuzzParser checks that the scanner accepts exactly the JSON objects
// encoding/json accepts, and never panics.
func FuzzParser(f *testing.F) {
	for _, s := range []string{
		`{"mlat":{"t":1,"m":"5d4840d6c0ffee"}}`, `{"sync":{"et":1,"em":"aa","ot":2,"om":"bb"}}`,
		`{"seen":["4840d6"]}`, `{"rate_report":{"4840D6":1.5}}`, `{"clock_reset":{"reason":"x"}}`,
		`{"a":[1,2.5e-3,-0,true,false,null,"A"]}`, `{}`, ` {"x" : {"y":{}}} `,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var p parser
		err := p.begin(b)
		for err == nil {
			var more bool
			more, err = p.next()
			if !more && err == nil {
				break
			}
		}
		var v map[string]json.RawMessage
		valid := json.Unmarshal(b, &v) == nil && v != nil
		// encoding/json tolerates invalid UTF-8 inside strings and so do
		// we; it also rejects deeper nesting than us, never shallower.
		if (err == nil) != valid {
			if err != nil && valid && strings.Count(string(b), "[")+strings.Count(string(b), "{") > maxDepth {
				return
			}
			t.Fatalf("parser err=%v, encoding/json valid=%v for %q", err, valid, b)
		}
	})
}
