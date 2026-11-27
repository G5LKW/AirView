package server

import (
	"net/http"
	"time"

	"airview/internal/model"
)

// Mode A/C targets (DESIGN §9.9, WP-MODEAC): aircraft without a Mode S
// address located by Mode A/C multilateration among the anchors. The MLAT
// engine publishes them once a second as model.Aircraft records with a
// synthetic "#xxxxxx" ICAO (never a valid hex address, so no reference
// data, photo or flight lookups apply), AddrType "modeac" and Source mlat.
// They carry no station identities, so the same records go to every scope.

func init() {
	routes = append(routes, Route{Method: "GET", Path: "/api/admin/mlat/modeac", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true,
		API: apiAdminModeAC, NoAuto: true,
		Summary: "Mode A/C multilateration: counters and the shown targets (synthetic id, squawk, Mode C altitude, position, grade, σ).",
		Returns: "`{\"enabled\":bool,\"stats\":{...},\"targets\":[ModeACTarget]}`"})
}

// appendModeAC adds the shown Mode A/C targets to a live picture
// (aggregator mode with aggregator.mlat.modeAc only).
func (s *Server) appendModeAC(snap *model.LiveSnapshot) {
	if !s.aggregatorMode() || !s.eng.EffectiveConfig().Aggregator.MLAT.ModeAC {
		return
	}
	m := s.eng.MLAT()
	if m == nil {
		return
	}
	if ac := m.ModeACAircraft(); len(ac) > 0 {
		snap.Aircraft = append(snap.Aircraft, ac...)
	}
}

func apiAdminModeAC(s *Server, r *http.Request) (any, error) {
	out := map[string]any{"enabled": s.eng.EffectiveConfig().Aggregator.MLAT.ModeAC, "targets": []any{}, "at": unixMs(time.Now())}
	if m := s.eng.MLAT(); m != nil {
		out["stats"] = m.ModeACStats()
		if t := m.ModeACTargets(); t != nil {
			out["targets"] = t
		}
	}
	return out, nil
}
