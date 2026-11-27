package rfstat

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// knownCodes are the documented message codes (WIRING.md section 6).
var knownCodes = map[string]bool{
	"gain.no-data": true, "gain.ok": true, "gain.too-high": true, "gain.high": true, "gain.low": true,
	"interference.no-data": true, "interference.ok": true, "interference.elevated": true, "interference.likely": true,
	"detection.no-data": true, "detection.measured": true,
	"sectors.no-data": true, "sectors.clear": true, "sectors.blocked": true,
	"updates.no-data": true, "updates.ok": true, "updates.low": true,
	"gnss.no-data": true, "gnss.ok": true, "gnss.elevated": true, "gnss.interference": true,
	"timing.unavailable": true, "timing.measured": true,
	"advice.gain-reduce": true, "advice.gain-slightly-high": true, "advice.gain-low": true,
	"advice.interference": true, "advice.sectors-blocked": true, "advice.gnss": true, "advice.healthy": true,
}

var numRe = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?`)

type printed struct {
	v   float64
	tol float64 // half a unit of the last printed digit
}

func printedNumbers(text string) []printed {
	var out []printed
	for _, s := range numRe.FindAllString(text, -1) {
		s = strings.ReplaceAll(s, ",", "")
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		dec := 0
		if i := strings.IndexByte(s, '.'); i >= 0 {
			dec = len(s) - i - 1
		}
		out = append(out, printed{v, 0.5*math.Pow(10, -float64(dec)) + 1e-9})
	}
	return out
}

// checkMessage asserts that a message carries a documented code and that
// every known number in data is printed in text (absolute value, at the
// text's precision).
func checkMessage(t *testing.T, where, code, text string, data Data) {
	t.Helper()
	if !knownCodes[code] {
		t.Errorf("%s: undocumented or missing code %q (%s)", where, code, text)
	}
	if data == nil {
		t.Errorf("%s %s: data is nil (want {})", where, code)
	}
	nums := printedNumbers(text)
	for k, v := range data {
		if !v.Valid() {
			continue
		}
		ok := false
		for _, n := range nums {
			if math.Abs(math.Abs(float64(v))-n.v) <= n.tol {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("%s %s: data.%s = %v not printed in %q", where, code, k, float64(v), text)
		}
	}
}

// checkHealthData checks every health item and piece of advice.
func checkHealthData(t *testing.T, h Health) {
	t.Helper()
	for _, it := range h.Items {
		checkMessage(t, "item "+it.Key, it.Code, it.Text, it.Data)
		if !strings.HasPrefix(it.Code, it.Key+".") {
			t.Errorf("item %s has code %s", it.Key, it.Code)
		}
	}
	if len(h.AdviceItems) != len(h.Advice) {
		t.Fatalf("%d advice items for %d advice sentences", len(h.AdviceItems), len(h.Advice))
	}
	for i, a := range h.AdviceItems {
		if a.Text != h.Advice[i] {
			t.Errorf("advice item %d text %q != advice %q", i, a.Text, h.Advice[i])
		}
		checkMessage(t, "advice", a.Code, a.Text, a.Data)
	}
}

// Every branch of the health heuristics, on synthetic reports.
func TestHealthCodesAndData(t *testing.T) {
	base := func() *Report {
		r := &Report{}
		r.Signal.Frames = 50000
		r.Signal.P50, r.Signal.DecodeFloor, r.Signal.SNRMargin = -21.37, -33.04, 11.7
		r.Channel.Frames = 50000
		r.Channel.FailFrac, r.Channel.RejectedFrac, r.Channel.OverlapProb = 0.123, 0.0456, 0.081
		r.Channel.FPS, r.Channel.OccupancyPct = 1234.4, 9.876
		r.Detection.Types = make([]PdType, 3)
		for i := range r.Detection.Types {
			r.Detection.Types[i].Pd, r.Detection.Types[i].Expected = Num(0.71-0.1*float64(i)), 50000
		}
		r.Detection.Range90Km, r.Detection.Range50Km = 187.4, 311.6
		r.Detection.VsBearing.Pos = make([]Num, 36)
		for i := range r.Detection.VsBearing.Pos {
			r.Detection.VsBearing.Pos[i] = 0.7
		}
		r.Detection.Gaps = make([]GapBand, 3)
		for i := range r.Detection.Gaps {
			r.Detection.Gaps[i] = GapBand{N: 1000, Mean: 1.2, PUpdate: []Num{0.5, 0.8, 0.973, 0.99, 1}}
		}
		r.Census.ADSB, r.Census.GNSSIndexNow, r.Census.JumpAircraft = 40, 1.25, 2
		r.Timing.Available, r.Timing.AltRegularity = true, 0.9876
		return r
	}
	codes := map[string]bool{}
	run := func(name string, r *Report) Health {
		h := healthReport(r)
		t.Run(name, func(t *testing.T) { checkHealthData(t, h) })
		for _, it := range h.Items {
			codes[it.Code] = true
		}
		for _, a := range h.AdviceItems {
			codes[a.Code] = true
		}
		return h
	}

	h := run("healthy", base())
	if h.Items[0].Code != "gain.ok" || float64(h.Items[0].Data["p50Dbfs"]) != -21.37 || float64(h.Items[1].Data["rateHz"]) != 1234.4 {
		t.Errorf("healthy items %+v", h.Items[:2])
	}

	r := base()
	r.Signal.StrongFrac = 0.08
	r.Channel.ExcessFail, r.Channel.BurstCount = 0.2, 3
	r.Detection.VsBearing.Pos[4], r.Detection.VsBearing.Pos[5], r.Detection.VsBearing.Pos[20] = 0.1, 0.2, 0.05
	r.Census.GNSSIndexNow = 31.5
	for i := range r.Detection.Gaps {
		r.Detection.Gaps[i].PUpdate[2] = 0.71
	}
	h = run("bad", r)
	if h.Items[0].Code != "gain.too-high" || h.AdviceItems[0].Code != "advice.gain-reduce" || float64(h.AdviceItems[0].Data["strongPct"]) != 8 {
		t.Errorf("too-high: %+v / %+v", h.Items[0], h.AdviceItems[0])
	}
	if d := h.Items[3].Data; h.Items[3].Code != "sectors.blocked" || d["fromDeg"] != 40 || d["toDeg"] != 60 {
		t.Errorf("sectors: %+v", h.Items[3])
	}
	// Every printed sector is in data (fromDegN / toDegN),
	// in the item and in its advice.
	var adv *AdviceItem
	for i := range h.AdviceItems {
		if h.AdviceItems[i].Code == "advice.sectors-blocked" {
			adv = &h.AdviceItems[i]
		}
	}
	if adv == nil {
		t.Fatal("no advice.sectors-blocked")
	}
	for _, d := range []Data{h.Items[3].Data, adv.Data} {
		if len(h.Blocked) != 2 {
			t.Fatalf("blocked %+v", h.Blocked)
		}
		if _, ok := d["fromDeg3"]; ok {
			t.Errorf("data has a third sector: %+v", d)
		}
		for i, b := range h.Blocked {
			from, to := d["fromDeg"+strconv.Itoa(i+1)], d["toDeg"+strconv.Itoa(i+1)]
			if from != Num(b.FromDeg) || to != Num(b.ToDeg) {
				t.Errorf("sector %d: data %v-%v, want %d-%d", i+1, from, to, b.FromDeg, b.ToDeg)
			}
		}
	}
	if d := h.Items[3].Data; d["fromDeg2"] != 200 || d["toDeg2"] != 210 {
		t.Errorf("second sector: %+v", d)
	}

	r = base()
	r.Signal.StrongFrac = 0.02
	r.Channel.ExcessFail = 0.07
	r.Census.GNSSIndexNow = 7.5
	run("warn", r)

	r = base()
	r.Signal.StrongFrac, r.Signal.SNRMargin = 0, 4
	run("gain-low", r)

	run("empty", &Report{Detection: DetectionStats{Types: make([]PdType, 3)}})

	for c := range knownCodes {
		if !codes[c] {
			t.Errorf("code %s never produced", c)
		}
	}
}
