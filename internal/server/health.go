package server

import (
	"net/http"
	"time"
)

// healthPath is the health endpoint for headless operation (DESIGN §20.1):
// load balancers, systemd watchdogs, Docker HEALTHCHECK and uptime monitors
// poll it. It is cheap (no database query), never rate-limited, never
// cached and says nothing about stations.
const healthPath = "/healthz"

type healthReply struct {
	// OK is true with status "ok" or "degraded" (HTTP 200).
	OK      bool              `json:"ok"`
	Status  string            `json:"status"` // "ok" | "degraded" | "failing" | "unavailable"
	Mode    string            `json:"mode"`
	Version string            `json:"version"`
	UptimeS int64             `json:"uptimeS"`
	Checks  map[string]string `json:"checks"`
	// AdminReady (aggregator mode) is false until the first admin account
	// with its authenticator exists; the feeder listeners stay closed
	// until then.
	AdminReady *bool `json:"adminReady,omitempty"`
}

// rawHealth answers 200 while the engine runs ("degraded" when an
// aggregator service is missing), 503 "failing" when a configured feeder
// listener could not be started or a database writer is stuck, and 503
// "unavailable" when the engine does not run.
func rawHealth(s *Server, w http.ResponseWriter, r *http.Request) {
	out := healthReply{Status: "ok", Version: s.opts.Version, UptimeS: int64(time.Since(s.started).Seconds()),
		Checks: map[string]string{}}
	code := http.StatusOK
	if s.resetMu.TryRLock() {
		running := s.eng.Running()
		out.Mode = s.eng.Mode()
		s.resetMu.RUnlock()
		if running {
			out.Checks["engine"] = "ok"
		} else {
			out.Checks["engine"] = "stopped"
			out.Status, code = "unavailable", http.StatusServiceUnavailable
		}
	} else {
		out.Checks["engine"] = "restarting"
		out.Status, code = "unavailable", http.StatusServiceUnavailable
	}
	if s.aggregatorMode() {
		if s.eng != nil {
			ready := s.eng.AdminReady()
			out.AdminReady = &ready
			if !ready {
				out.Checks["admin"] = "setup pending"
			}
		}
		if s.registry() != nil {
			out.Checks["registry"] = "ok"
		} else {
			out.Checks["registry"] = "missing"
			if code == http.StatusOK {
				out.Status = "degraded"
			}
		}
		// Feeder listeners and database writers (SO-2): a configured
		// feeder port that could not be bound, or a writer that stopped
		// writing, makes the aggregator unable to do its job: 503.
		if s.eng != nil && s.resetMu.TryRLock() {
			checks := s.eng.HealthChecks()
			s.resetMu.RUnlock()
			for name, c := range checks {
				out.Checks[name] = c.State
				if c.Fail {
					out.Status, code = "failing", http.StatusServiceUnavailable
				} else if c.Degraded && code == http.StatusOK {
					out.Status = "degraded"
				}
			}
		}
	}
	out.OK = code == http.StatusOK
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, r, code, out)
}
