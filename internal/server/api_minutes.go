package server

import (
	"net/http"
	"time"

	"airview/internal/engine"
)

// Selective-mode MINUTE_STATS merge (WP-I2): per minute, the feeder's own
// counters, what the server received, the merged best counts and what the
// feeder shed (selective reduce, flow control, budget). Owner/admin only
// (it describes one station's reception).

func init() {
	routes = append(routes,
		Route{Method: "GET", Path: "/api/me/stations/{sid}/minutes", Group: grpMe, Access: AccessOwner, Station: StationPath, NetOnly: true,
			API: apiStationMinutes, NoAuto: true,
			Summary: "The station's last 16 minutes of feeder statistics (AVF/1 MINUTE_STATS merged with what the server received): exact, received, merged and shed counts, selective mode.",
			Returns: "`{\"minutes\":[{minute, exact, received, receivedKnown, merged, shed, selective, records, late}]}`"},
		Route{Method: "GET", Path: "/api/admin/stations/{sid}/minutes", Group: grpNetAdm, Access: AccessAdmin, Station: StationPath, NetOnly: true,
			API: apiStationMinutes, NoAuto: true,
			Summary: "A station's last 16 minutes of feeder statistics (as the owner sees them)."},
	)
}

func apiStationMinutes(s *Server, r *http.Request) (any, error) {
	st, _ := selectedStation(r)
	list := s.eng.StationMinutes(st.Idx)
	if list == nil {
		list = []engine.StationMinute{}
	}
	sel := false
	for _, m := range list {
		sel = sel || m.Selective
	}
	return map[string]any{"minutes": list, "selective": sel, "at": unixMs(time.Now())}, nil
}
