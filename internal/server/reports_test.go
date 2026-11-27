package server

import (
	"context"
	"database/sql"
	"encoding/csv"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestReportAirports checks the dep / arr columns, filters (ICAO and IATA,
// case-insensitive), sorting and the CSV columns of the report query.
func TestReportAirports(t *testing.T) {
	r := newRig(t, rigOptions{seedHours: 3, aircraft: 8})
	for deadline := time.Now().Add(20 * time.Second); r.eng.Region() == nil || r.eng.Region().Len() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("airport region not loaded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	type row struct {
		FlightID int64  `json:"flightId"`
		Dep      string `json:"dep"`
		Arr      string `json:"arr"`
	}
	type reply struct {
		Total int   `json:"total"`
		Rows  []row `json:"rows"`
	}
	all := decode[reply](t, r.get("/api/reports?hours=6&limit=3&sort=start&desc=0").Body.Bytes())
	if len(all.Rows) < 3 {
		t.Fatalf("report: %+v", all)
	}
	a, b := all.Rows[0].FlightID, all.Rows[1].FlightID
	err := r.eng.Store().Write(context.Background(), func(tx *sql.Tx) error {
		// Everything else resolved without airport, so the background
		// resolver cannot interfere.
		if _, err := tx.Exec(`UPDATE flights SET dep = '', arr = ''`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE flights SET dep = 'EDDF', arr = 'EGLL' WHERE id = ?`, a); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE flights SET dep = 'EDDK', arr = '' WHERE id = ?`, b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q    string
		want []int64
	}{
		{"dep=EDDF", []int64{a}}, {"dep=fra", []int64{a}}, {"dep=eddk", []int64{b}}, {"arr=LHR", []int64{a}},
		{"arr=egll&dep=CGN", nil}, {"dep=XXXX", nil},
	} {
		got := decode[reply](t, r.get("/api/reports?hours=6&"+c.q).Body.Bytes())
		if got.Total != len(c.want) || len(got.Rows) != len(c.want) {
			t.Errorf("%s: total %d rows %+v, want %v", c.q, got.Total, got.Rows, c.want)
			continue
		}
		for i, id := range c.want {
			if got.Rows[i].FlightID != id {
				t.Errorf("%s: row %d = %d, want %d", c.q, i, got.Rows[i].FlightID, id)
			}
		}
	}
	one := decode[reply](t, r.get("/api/reports?hours=6&dep=EDDF").Body.Bytes())
	if one.Rows[0].Dep != "EDDF" || one.Rows[0].Arr != "EGLL" {
		t.Errorf("row airports: %+v", one.Rows[0])
	}
	sorted := decode[reply](t, r.get("/api/reports?hours=6&sort=dep&desc=1&limit=2").Body.Bytes())
	if len(sorted.Rows) != 2 || sorted.Rows[0].Dep != "EDDK" || sorted.Rows[1].Dep != "EDDF" {
		t.Errorf("sort=dep desc: %+v", sorted.Rows)
	}

	rec := r.get("/api/reports.csv?hours=6&dep=FRA")
	if rec.Code != 200 {
		t.Fatalf("csv: %d %s", rec.Code, rec.Body.String())
	}
	recs, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv rows: %v %v", recs, err)
	}
	h, v := recs[0], recs[1]
	if h[len(h)-2] != "dep" || h[len(h)-1] != "arr" || v[len(v)-2] != "EDDF" || v[len(v)-1] != "EGLL" || v[0] != strconv.FormatInt(a, 10) {
		t.Errorf("csv: header %v row %v", h, v)
	}
}
