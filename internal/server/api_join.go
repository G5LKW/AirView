package server

import (
	"net/http"
	"strings"

	"airview/internal/model"
)

// GET /api/public/join: what a new feeder needs to connect to this network
// (the web UI's "Feed this network" page, #/feeder, DESIGN §12.2 "The
// aggregator's /feeder page also shows the two-line readsb/Ultrafeeder
// snippets"). Operator facts only: the host name feeders use, the open
// listener ports, the admission and registration policies and the backup
// retention owners are told about in the deletion notice (consent text C8).
// Nothing about any station.
//
// The host is aggregator.publicHost; when it is empty the page uses the
// host name it was loaded from (the server never echoes a request header).

type joinDTO struct {
	Network      bool   `json:"network"` // false in standalone mode (every other field empty)
	Name         string `json:"name,omitempty"`
	Host         string `json:"host"`      // "" = the host name the page was loaded from
	AVFPort      int    `json:"avfPort"`   // 0 = closed
	BeastPort    int    `json:"beastPort"` // 0 = closed
	MLATPort     int    `json:"mlatPort"`  // 0 = closed (also while the MLAT engine is off)
	MLAT         bool   `json:"mlat"`      // the network runs its own MLAT engine
	Admission    string `json:"admission"`
	Registration string `json:"registration"`
	BackupDays   int    `json:"backupDays"`
	// PrivacyNotice is aggregator.privacyNoticeUrl ("" = none).
	PrivacyNotice string `json:"privacyNotice"`
}

func apiPublicJoin(s *Server, r *http.Request) (any, error) {
	if !s.aggregatorMode() {
		return joinDTO{}, nil // standalone: nothing to join
	}
	a := s.eng.EffectiveConfig().Aggregator
	out := joinDTO{Network: true, Name: a.Name, Host: strings.TrimSpace(a.PublicHost), AVFPort: a.AVFPort, BeastPort: a.BeastPort,
		MLAT: a.MLAT.Enabled, Admission: a.Admission, Registration: a.Auth.Registration, BackupDays: a.Retention.BackupDays,
		PrivacyNotice: a.PrivacyNoticeURL}
	if a.MLAT.Enabled {
		out.MLATPort = a.MLATPort
	}
	if out.Registration == "" {
		out.Registration = model.RegistrationOpen
	}
	return out, nil
}
