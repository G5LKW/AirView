package server

import (
	"net/http"
	"time"
)

// Federation routes (DESIGN §12.1 phase 5; internal/federation WIRING.md
// §3). The status lists peer addresses, so it is admin-only. Remote
// aircraft are drawn on the public map (and thus on every map that shows
// the public picture) unless aggregator.federation.hideRemote is set; they
// are public-safe as received (the origin applied its own island guard).

func init() {
	routes = append(routes, Route{Method: "GET", Path: "/api/admin/federation", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true,
		API: apiAdminFederation, NoAuto: true,
		Summary: "Federation: node ID, TLS pin, peers (state, direction, RTT, counters, last error), received aircraft and out-feeds.",
		Returns: "`{\"enabled\":bool,\"running\":bool,\"error\":string,\"status\":{nodeId,peers[],received,outFeeds[]}}`"})
}

func apiAdminFederation(s *Server, r *http.Request) (any, error) {
	cfg := s.eng.EffectiveConfig().Aggregator.Federation
	out := map[string]any{"enabled": cfg.Enabled, "running": false, "at": unixMs(time.Now())}
	if e := s.eng.FederationError(); e != "" {
		out["error"] = e
	}
	if n := s.eng.Federation(); n != nil {
		out["running"] = true
		out["status"] = n.Status()
	}
	return out, nil
}
