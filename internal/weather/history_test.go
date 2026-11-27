package weather

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// syntheticHistory answers /hf and /archive like Open-Meteo does for a place
// whose model archive begins at surfaceFrom (pressure levels at levelsFrom):
// the historical-forecast API accepts earlier dates but returns nulls, the
// ERA5 archive always has (surface) data. Model values are 1, ERA5 values 2.
func syntheticHistory(surfaceFrom, levelsFrom time.Time) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, err1 := time.Parse("2006-01-02", q.Get("start_date"))
		end, err2 := time.Parse("2006-01-02", q.Get("end_date"))
		if err1 != nil || err2 != nil || end.Before(start) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":true,"reason":"bad dates"}`))
			return
		}
		hours := int(end.Sub(start)/time.Hour) + 24
		var sb strings.Builder
		sb.WriteString(`{"latitude":50,"longitude":8,"elevation":100,"hourly":{"time":[`)
		for i := 0; i < hours; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(strconv.FormatInt(start.Add(time.Duration(i)*time.Hour).Unix(), 10))
		}
		sb.WriteByte(']')
		for _, name := range strings.Split(q.Get("hourly"), ",") {
			sb.WriteString(`,"` + name + `":[`)
			for i := 0; i < hours; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				t := start.Add(time.Duration(i) * time.Hour)
				switch {
				case r.URL.Path == "/archive":
					sb.WriteString("2")
				case t.Before(surfaceFrom), strings.HasSuffix(name, "hPa") && t.Before(levelsFrom):
					sb.WriteString("null")
				default:
					sb.WriteString("1")
				}
			}
			sb.WriteByte(']')
		}
		sb.WriteString("}}")
		_, _ = w.Write([]byte(sb.String()))
	}
}

type histCall struct {
	path, start, end string
	probe            bool
}

func histCalls(rec *recorder, from int) []histCall {
	var out []histCall
	for i := from; i < rec.count(""); i++ {
		q := rec.request(i).URL.Query()
		out = append(out, histCall{
			path: rec.request(i).URL.Path, start: q.Get("start_date"), end: q.Get("end_date"),
			probe: q.Get("hourly") == "temperature_2m",
		})
	}
	return out
}

// The historical-forecast API accepts dates from 2016 but has no data before
// the model archives begin (verified live: surface from 2017, profiles from
// 2021 at the test location). HistoryAuto must find that boundary and serve
// ERA5 before it, hour by hour.
func TestHistoryAutoFallsBackToArchive(t *testing.T) {
	date := func(s string) time.Time {
		d, err := time.Parse("2006-01-02T15", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	surfaceFrom, levelsFrom := date("2017-01-05T06"), date("2017-02-01T00")
	rec := newRecorder(t, syntheticHistory(surfaceFrom, levelsFrom))
	c := testClient(rec, Options{})
	ctx := context.Background()

	var kinds []string
	var all []HourPoint
	var cost float64
	err := c.HistoryEach(ctx, 50, 8, date("2016-12-01T00"), date("2017-02-10T00"), HistoryOptions{}, func(b *Bundle) error {
		kinds = append(kinds, b.Kind)
		all = append(all, b.Hourly...)
		cost += b.Cost
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Request sequence: one model chunk that comes back empty, a bisection for
	// the first day with model data, then ERA5 up to that day and model chunks
	// from it (the first of them topped up from ERA5 for its empty hours).
	calls := histCalls(rec, 0)
	var chunks []histCall
	probes := 0
	for _, cl := range calls {
		if cl.probe {
			probes++
			if cl.path != "/hf" || cl.start != cl.end {
				t.Errorf("probe request %+v should be a one-day model request", cl)
			}
			continue
		}
		chunks = append(chunks, cl)
	}
	want := []histCall{
		{"/hf", "2016-12-01", "2016-12-14", false},
		{"/archive", "2016-12-01", "2016-12-14", false},
		{"/archive", "2016-12-15", "2016-12-28", false},
		{"/archive", "2016-12-29", "2017-01-04", false},
		{"/hf", "2017-01-05", "2017-01-18", false},
		{"/archive", "2017-01-05", "2017-01-18", false},
		{"/hf", "2017-01-19", "2017-02-01", false},
		{"/hf", "2017-02-02", "2017-02-10", false},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunk requests = %+v\nwant %+v", chunks, want)
	}
	for i := range want {
		if chunks[i] != want[i] {
			t.Errorf("chunk request %d = %+v, want %+v", i, chunks[i], want[i])
		}
	}
	if probes < 2 || probes > 7 { // 1 + ceil(log2(58 days))
		t.Errorf("%d probe requests, want 2..7", probes)
	}
	if calls[1].start != "2017-02-10" || !calls[1].probe {
		t.Errorf("the bisection should start at the end of the range, got %+v", calls[1])
	}
	wantKinds := []string{KindArchive, KindArchive, KindArchive, KindMixed, KindHistoricalForecast, KindHistoricalForecast}
	if strings.Join(kinds, ",") != strings.Join(wantKinds, ",") {
		t.Errorf("chunk kinds = %v, want %v", kinds, wantKinds)
	}
	// Every request was charged to exactly one delivered chunk.
	if u := c.Usage(); cost < u.Day-1e-9 || cost > u.Day+1e-9 {
		t.Errorf("bundle costs add up to %v, client usage is %v", cost, u.Day)
	}

	// Every hour is there exactly once, from the right source.
	if len(all) != 72*24 {
		t.Fatalf("%d hours, want %d", len(all), 72*24)
	}
	for i, h := range all {
		wantT := date("2016-12-01T00").Add(time.Duration(i) * time.Hour)
		if h.T != wantT.UnixMilli() {
			t.Fatalf("hour %d is %v, want %v", i, h.Time(), wantT)
		}
		switch {
		case wantT.Before(surfaceFrom):
			if h.TempC != 2 || h.Levels != nil || h.Wind100Kmh != 2 || h.CAPE.Valid() {
				t.Fatalf("%v should be an ERA5 hour: temp %v levels %v", wantT, h.TempC, h.Levels)
			}
		case wantT.Before(levelsFrom):
			if h.TempC != 1 || len(h.Levels) != 14 || h.HasProfile() || h.CAPE != 1 {
				t.Fatalf("%v should be a model hour without profile: temp %v", wantT, h.TempC)
			}
		default:
			if h.TempC != 1 || !h.HasProfile() || h.Levels[13].HeightM != 1 {
				t.Fatalf("%v should be a model hour with profile", wantT)
			}
		}
	}

	// History() reports the combination.
	n0 := rec.count("")
	b, err := c.History(ctx, 50, 8, date("2016-12-20T00"), date("2017-01-20T00"), HistoryOptions{ChunkDays: 31})
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != KindMixed || len(b.Hourly) != 32*24 || len(b.LevelsHPa) != 14 || b.Current != nil {
		t.Errorf("History: kind %s, %d hours, levels %v", b.Kind, len(b.Hourly), b.LevelsHPa)
	}
	// A partly covered chunk needs no bisection: one model + one ERA5 request.
	if got := histCalls(rec, n0); len(got) != 3 || got[0].path != "/hf" || got[1].path != "/archive" || got[2].path != "/hf" {
		t.Errorf("requests for a straddling range = %+v", got)
	}

	// A range entirely before the model archive: one wasted model chunk, one
	// probe, everything else straight from ERA5.
	n0 = rec.count("")
	b, err = c.History(ctx, 50, 8, date("2016-03-01T00"), date("2016-04-30T00"), HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hf, ar, pr := 0, 0, 0
	for _, cl := range histCalls(rec, n0) {
		switch {
		case cl.probe:
			pr++
		case cl.path == "/hf":
			hf++
		default:
			ar++
		}
	}
	if b.Kind != KindArchive || len(b.Hourly) != 61*24 || hf != 1 || pr != 1 || ar != 5 {
		t.Errorf("pre-archive range: kind %s, %d hours, %d model / %d probe / %d ERA5 requests", b.Kind, len(b.Hourly), hf, pr, ar)
	}
	if b.Hourly[0].TempC != 2 || b.Hourly[len(b.Hourly)-1].TempC != 2 || b.LevelsHPa != nil {
		t.Error("pre-archive range should consist of ERA5 hours")
	}

	// A single empty chunk is filled without probing.
	n0 = rec.count("")
	b, err = c.History(ctx, 50, 8, date("2016-06-01T00"), date("2016-06-03T00"), HistoryOptions{})
	if err != nil || b.Kind != KindArchive || len(b.Hourly) != 72 || rec.count("")-n0 != 2 {
		t.Errorf("single empty chunk: %v, kind %s, %d requests", err, b.Kind, rec.count("")-n0)
	}

	// Forced sources never mix: the model source reports its gaps as missing.
	n0 = rec.count("")
	b, err = c.History(ctx, 50, 8, date("2016-06-01T00"), date("2016-06-02T00"), HistoryOptions{Source: HistoryModel})
	if err != nil || b.Kind != KindHistoricalForecast || b.Hourly[0].TempC.Valid() || rec.count("")-n0 != 1 {
		t.Errorf("forced model source: %v, kind %s, %d requests", err, b.Kind, rec.count("")-n0)
	}
	// Before 2016 the model API is not even asked.
	n0 = rec.count("")
	b, err = c.History(ctx, 50, 8, date("2015-12-30T00"), date("2015-12-31T00"), HistoryOptions{})
	if err != nil || b.Kind != KindArchive || rec.count("")-n0 != 1 || rec.request(-1).URL.Path != "/archive" {
		t.Errorf("pre-2016 range: %v, kind %s, %d requests", err, b.Kind, rec.count("")-n0)
	}
}

func TestFillFromArchive(t *testing.T) {
	hour := func(t int64, temp Num, levels bool) HourPoint {
		h := nanHour
		h.T, h.TempC = t*3600000, temp
		if levels {
			h.Levels = []Level{nanLevel}
		}
		return h
	}
	m := &Bundle{Kind: KindHistoricalForecast, Cost: 11, LevelsHPa: []int{850},
		Hourly: []HourPoint{hour(1, NaN, true), hour(2, NaN, true), hour(3, 7, true), hour(4, NaN, true)}}
	// The archive answer lacks hour 2 and has an extra hour 5.
	a := &Bundle{Kind: KindArchive, Cost: 2.5,
		Hourly: []HourPoint{hour(0, 20, false), hour(1, 21, false), hour(3, 23, false), hour(4, 24, false), hour(5, 25, false)}}
	out := fillFromArchive(m, a)
	if out != m || out.Kind != KindMixed || out.Cost != 13.5 || len(out.Hourly) != 4 {
		t.Fatalf("merged bundle: kind %s cost %v hours %d", out.Kind, out.Cost, len(out.Hourly))
	}
	h := out.Hourly
	if h[0].TempC != 21 || h[0].Levels != nil || h[1].TempC.Valid() || h[1].Levels == nil ||
		h[2].TempC != 7 || h[2].Levels == nil || h[3].TempC != 24 || h[3].T != 4*3600000 {
		t.Errorf("merged hours = %v %v %v %v", h[0].TempC, h[1].TempC, h[2].TempC, h[3].TempC)
	}
	// Nothing usable in the model bundle: the archive bundle is returned as is.
	empty := &Bundle{Kind: KindHistoricalForecast, Cost: 11, Hourly: []HourPoint{hour(1, NaN, true)}}
	a2 := &Bundle{Kind: KindArchive, Cost: 2.5, Hourly: []HourPoint{hour(1, 21, false)}}
	if out := fillFromArchive(empty, a2); out != a2 || out.Kind != KindArchive || out.Cost != 13.5 {
		t.Errorf("empty model bundle: kind %s cost %v", out.Kind, out.Cost)
	}
}
