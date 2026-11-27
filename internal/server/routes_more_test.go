package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"airview/internal/navdata"
)

const boardAirportsCSV = `"id","ident","type","name","latitude_deg","longitude_deg","elevation_ft","continent","iso_country","iso_region","municipality","scheduled_service","icao_code","iata_code","gps_code","local_code","home_link","wikipedia_link","keywords"
2434,"EGLL","large_airport","London Heathrow Airport",51.4706,-0.461941,83,"EU","GB","GB-ENG","London","yes","EGLL","LHR","EGLL",,,,
3622,"KJFK","large_airport","John F Kennedy International Airport",40.639447,-73.779317,13,"NA","US","US-NY","New York","yes","KJFK","JFK","KJFK","JFK",,,
`

// TestRouteBoardAPI: the airport board, airport references, batch, stats and
// providers answer with the right shapes; unknown airports are 404.
func TestRouteBoardAPI(t *testing.T) {
	r := newRig(t, rigOptions{})
	if _, err := r.eng.NavData().ImportReader(context.Background(), navdata.DatasetAirports, strings.NewReader(boardAirportsCSV), nil); err != nil {
		t.Fatal(err)
	}
	rec := r.get("/api/routes/board/LHR")
	if rec.Code != http.StatusOK {
		t.Fatalf("board: %d %s", rec.Code, rec.Body.String())
	}
	var b struct {
		Airport struct {
			Code, IATA string
		} `json:"airport"`
		Arrivals, Departures, Landed []json.RawMessage
		Note                         string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Airport.Code != "EGLL" || b.Arrivals == nil || b.Departures == nil || b.Landed == nil || b.Note == "" {
		t.Fatalf("board body %s (%v)", rec.Body.String(), err)
	}
	if rec := r.get("/api/routes/board/ZZZZ"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown airport: %d", rec.Code)
	}
	rec = r.get("/api/routes/airports?codes=jfk,EGLL,NOPE")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"code":"KJFK"`) || !strings.Contains(rec.Body.String(), `"iata":"LHR"`) || strings.Contains(rec.Body.String(), "NOPE") {
		t.Fatalf("airports: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/api/routes/airports?codes=" + strings.Repeat("A,", 201)); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many codes: %d", rec.Code)
	}
	if rec := r.get("/api/routes/batch?callsigns=BAW1,EZY2"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"routes":{`) {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.get("/api/routes/stats?kind=bogus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", rec.Code)
	}
	for _, k := range []string{"routes", "airports", "airlines", "coverage"} {
		if rec := r.get("/api/routes/stats?kind=" + k); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rows":[`) {
			t.Fatalf("stats %s: %d %s", k, rec.Code, rec.Body.String())
		}
	}
	if rec := r.get("/api/routes/providers"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"adsbdb"`) || !strings.Contains(rec.Body.String(), `"lic":"restricted"`) {
		t.Fatalf("providers: %d %s", rec.Code, rec.Body.String())
	}
}
