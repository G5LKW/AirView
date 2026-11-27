package server

// Experimental bistatic SSR self-location (internal/engine/ssrwire.go,
// internal/mlat/ssr; docs/aggregator/SSR-RESEARCH.md). Off by default
// (ssr.enabled). Admin only in every mode: the result is an estimate of the
// receiver's own position, so it is never public and never part of a
// public DTO (DESIGN §8, §19.3). Nothing here applies the result to a
// station position; the settings are the ordinary `ssr` configuration
// block, edited through PUT /api/config.

import (
	"net/http"
)

func init() {
	routes = append(routes, ssrRoutes...)
}

var ssrRoutes = []Route{
	{Method: "GET", Path: "/api/ssr", Group: grpSystem, Access: AccessAdmin, API: apiSSRStatus, NoAuto: true,
		Summary: "EXPERIMENTAL: state of the SSR self-location (enabled, feed, counters, estimator statistics incl. `pairMatches`, the last solve with the interrogators it used, and `radars`: the interrogators located from their beam sweeps, with formal 1σ ellipses). Advisory only: the result is never applied to a position.",
		Returns: "`SSRStatus` (`experimental`, `enabled`, ...); `enabled:false` when `ssr.enabled` is off"},
	{Method: "POST", Path: "/api/ssr/solve", Group: grpSystem, Access: AccessAdmin, Admin: true, API: apiSSRSolve, NoAuto: true,
		Summary: "EXPERIMENTAL: run an SSR self-location solve now (about 0.3 s; poll `GET /api/ssr`).",
		Returns: "`202` `{\"queued\":true}`; `409` when `ssr.enabled` is off or a solve is already queued"},
}

func apiSSRStatus(s *Server, r *http.Request) (any, error) {
	return s.eng.SSRStatus(), nil
}

func apiSSRSolve(s *Server, r *http.Request) (any, error) {
	if !s.eng.SSRSolveNow() {
		return nil, errStatus(http.StatusConflict, "SSR self-location is off (ssr.enabled) or a solve is already queued")
	}
	return accepted(map[string]any{"queued": true}), nil
}
