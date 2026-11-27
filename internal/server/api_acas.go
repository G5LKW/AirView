package server

// ACAS (TCAS) resolution advisories reported by aircraft (internal/acas).
// Owner scope in aggregator mode: event positions are exact and may lie
// low near a feeder's station (the island guard of the public map does not
// apply to this list).

import (
	"net/http"
	"time"

	"airview/internal/acas"
)

func init() {
	routes = append(routes, acasRoutes...)
}

var acasRoutes = []Route{
	{Method: "GET", Path: "/api/acas/events", Group: grpCore, Access: AccessOwner, API: apiACASEvents, Sample: "/api/acas/events?hours=24",
		Summary: "ACAS (TCAS II) resolution advisories reported by aircraft (DF16, BDS 3,0, TC28), newest first: when, where, which advisories (\"Climb\", \"Do not descend\", ...), the threat when reported, and whether the aircraft reported the RA as ended.",
		Params:  params(pRange, Param{"icao", "only this aircraft (6 hex digits)"}, Param{"limit", "1..5000 (default 500)"}),
		Notes:   "Default range: the last 7 days. Open events (`open:true`, `id` 0) are included. A report is the aircraft's own statement that its ACAS issued an advisory; it says nothing about the crew's response, and a missing report does not mean there was none (BDS 3,0 is only sent when a ground station asks for it)."},
	{Method: "GET", Path: "/api/acas/status", Group: grpCore, Access: AccessOwner, API: apiACASStatus,
		Summary: "Counters of the resolution advisory recorder (events since the start, open events, reports dropped by the flood guard)."},
}

func apiACASEvents(s *Server, r *http.Request) (any, error) {
	rec := s.eng.ACAS()
	if rec == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "ACAS recorder not running")
	}
	from, to, err := qSpanDefault(r, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	q := acas.Query{From: from.UnixMilli(), To: to.UnixMilli()}
	if v := qStr(r, "icao"); v != "" {
		key, err := parseICAOParam(v)
		if err != nil {
			return nil, err
		}
		q.ICAO = key
	}
	n, err := qInt(r, "limit", 500, 1, 5000)
	if err != nil {
		return nil, err
	}
	q.Limit = n
	evs, err := rec.Events(r.Context(), q)
	if err != nil {
		return nil, err
	}
	if evs == nil {
		evs = []acas.Event{}
	}
	return map[string]any{"events": evs}, nil
}

func apiACASStatus(s *Server, r *http.Request) (any, error) {
	rec := s.eng.ACAS()
	if rec == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "ACAS recorder not running")
	}
	return rec.Stats(), nil
}
