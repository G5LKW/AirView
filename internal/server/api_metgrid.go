package server

// Gridded winds / temperatures aloft and BDS 4,5 hazard reports
// (internal/met/grid.go, internal/engine/metgrid.go).

import (
	"net/http"
	"strconv"
	"time"

	"airview/internal/met"
)

func init() {
	routes = append(routes, metGridRoutes...)
}

var metGridRoutes = []Route{
	{Method: "GET", Path: "/api/met/grid", Group: grpMet, Access: AccessAdmin, API: apiMetGrid, Cache: ttlShort, Sample: "/api/met/grid?level=300",
		Summary: "Winds and temperatures aloft on a latitude/longitude grid at one flight level, from the aircraft observations of the last minutes (Mode S EHS BDS 5,0/6,0 derivation and BDS 4,4).",
		Params: []Param{{"level", "flight level, e.g. `300` (default 300)"}, {"band", "half width of the altitude band in ft, 500..5000 (default 2000)"},
			{"minutes", "time window, 10..360 (default 60)"}, {"cell", "cell size in latitude degrees, 0.1..5 (default 0.5; longitude cells are widened with latitude)"},
			{"minObs", "observations a cell needs, 1..50 (default 2)"}},
		Notes: "Each cell: vector-mean wind (`windDir` FROM, `windKt`), mean static air temperature reduced to the level with the standard lapse rate (`tempC`) and its deviation from the ICAO standard atmosphere (`isaDevC`), counts. Cells without observations are absent: nothing is interpolated."},
	{Method: "GET", Path: "/api/met/hazards", Group: grpMet, Access: AccessAdmin, API: apiMetHazards, Sample: "/api/met/hazards?hours=6",
		Summary: "BDS 4,5 meteorological hazard reports (turbulence, wind shear, microburst, icing, wake vortex: 0 nil .. 3 severe; static air temperature, pressure, radio height), newest first, at most one per aircraft per minute.",
		Params:  []Param{{"hours", "the last N hours, up to 48 (default 6)"}},
		Notes:   "In memory only (the last 2000). BDS 4,5 has no identifying bits, so a reply is only taken as 4,5 when register inference picks it: rare, and less certain than the other registers. The reports never enter the profiles or the grid."},
}

func qFloatParam(r *http.Request, name string, def, lo, hi float64) (float64, error) {
	v := qStr(r, name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < lo || f > hi {
		return 0, badRequest("%s must be a number in %g..%g", name, lo, hi)
	}
	return f, nil
}

func apiMetGrid(s *Server, r *http.Request) (any, error) {
	fl, err := qFloatParam(r, "level", 300, 0, 600)
	if err != nil {
		return nil, err
	}
	band, err := qFloatParam(r, "band", 2000, 500, 5000)
	if err != nil {
		return nil, err
	}
	mins, err := qInt(r, "minutes", 60, 10, 360)
	if err != nil {
		return nil, err
	}
	cell, err := qFloatParam(r, "cell", 0.5, 0.1, 5)
	if err != nil {
		return nil, err
	}
	minObs, err := qInt(r, "minObs", 2, 1, 50)
	if err != nil {
		return nil, err
	}
	return s.eng.MetGrid(r.Context(), met.GridQuery{LevelFt: fl * 100, BandFt: band, CellDeg: cell, MinObs: minObs}, time.Duration(mins)*time.Minute)
}

func apiMetHazards(s *Server, r *http.Request) (any, error) {
	h, err := qInt(r, "hours", 6, 1, 48)
	if err != nil {
		return nil, err
	}
	return map[string]any{"hazards": s.eng.Hazards(time.Now().Add(-time.Duration(h) * time.Hour))}, nil
}
