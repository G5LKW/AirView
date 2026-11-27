package server

import (
	"encoding/json"
	"strconv"
	"testing"
)

// TestCoverageOutlineAltBand checks the altBand selection of the coverage
// outline route (analytics.CoverageOutline).
func TestCoverageOutlineAltBand(t *testing.T) {
	r := shared(t)
	type fc struct {
		Type     string `json:"type"`
		Features []struct {
			Properties map[string]any `json:"properties"`
		} `json:"features"`
	}
	get := func(q string) fc {
		rec := r.get("/api/antenna/coverage.geojson?days=30" + q)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		return decode[fc](t, rec.Body.Bytes())
	}
	bands := func(c fc) map[float64]int {
		m := map[float64]int{}
		for _, f := range c.Features {
			b, _ := f.Properties["band"].(float64)
			m[b]++
		}
		return m
	}
	def := get("")
	if def.Type != "FeatureCollection" || len(def.Features) == 0 {
		t.Fatalf("default outline: %+v", def)
	}
	all := bands(get("&altBand=all"))
	if len(all) == 0 || all[-1] != 0 {
		t.Errorf("altBand=all: %v (no overall outline expected)", all)
	}
	over := bands(get("&altBand=-1&kind=max"))
	if len(over) != 1 || over[-1] != 1 {
		t.Errorf("altBand=-1&kind=max: %v", over)
	}
	for b := range all {
		one := bands(get("&altBand=" + strconv.Itoa(int(b))))
		if len(one) != 1 || one[b] == 0 {
			t.Errorf("altBand=%v: %v", b, one)
		}
		break
	}
	for _, bad := range []string{"&altBand=7", "&altBand=x", "&altBand=all&kind=nope"} {
		if rec := r.get("/api/antenna/coverage.geojson?days=30" + bad); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

// TestRFInterrogatorsShape: GET /api/rf/interrogators is {t, interrogators}.
func TestRFInterrogatorsShape(t *testing.T) {
	r := shared(t)
	rec := r.get("/api/rf/interrogators")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var v struct {
		T             int64           `json:"t"`
		Interrogators json.RawMessage `json:"interrogators"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.T <= 0 {
		t.Fatalf("shape: %v t=%d", err, v.T)
	}
	var inner struct {
		Codes        []json.RawMessage `json:"codes"`
		ActiveRadars *int              `json:"activeRadars"`
	}
	if err := json.Unmarshal(v.Interrogators, &inner); err != nil || inner.ActiveRadars == nil || inner.Codes == nil {
		t.Fatalf("interrogators object: %v %.200s", err, v.Interrogators)
	}
}
