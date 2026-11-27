package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"airview/internal/latency"
	"airview/internal/modes"
	"airview/internal/ntp"
)

// Latency-chain endpoints (internal/latency, see its WIRING.md) and the
// manual NTP check.

func (s *Server) latencyAnalyzer() (*latency.Analyzer, error) {
	a := s.eng.Latency()
	if a == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "latency analysis not available")
	}
	return a, nil
}

type latencyAircraftReply struct {
	Aircraft []latency.AircraftRow `json:"aircraft"`
}

func apiTimingLatencyAircraft(s *Server, r *http.Request) (any, error) {
	a, err := s.latencyAnalyzer()
	if err != nil {
		return nil, err
	}
	rows := a.Aircraft()
	if rows == nil {
		rows = []latency.AircraftRow{}
	}
	return latencyAircraftReply{Aircraft: rows}, nil
}

type latencyAircraftDetail struct {
	Row       latency.AircraftRow `json:"row"`
	Waterfall latency.Waterfall   `json:"waterfall"`
}

func apiTimingLatencyAircraftOne(s *Server, r *http.Request) (any, error) {
	a, err := s.latencyAnalyzer()
	if err != nil {
		return nil, err
	}
	icao, err := modes.ParseICAO(r.PathValue("icao"))
	if err != nil {
		return nil, badRequest("invalid ICAO address")
	}
	row, wf, ok := a.AircraftDetail(icao)
	if !ok {
		return nil, notFound("unknown aircraft")
	}
	return latencyAircraftDetail{Row: row, Waterfall: wf}, nil
}

type latencyFeedReply struct {
	Feed   string               `json:"feed"`
	Feeds  []string             `json:"feeds"` // every tracked feed ID
	Series []latency.FeedMinute `json:"series"`
}

func apiTimingLatencyFeed(s *Server, r *http.Request) (any, error) {
	a, err := s.latencyAnalyzer()
	if err != nil {
		return nil, err
	}
	since, err := qInt64(r, "since", 0)
	if err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	series, ok := a.FeedSeries(id, since)
	if !ok {
		return nil, notFound("unknown feed")
	}
	if series == nil {
		series = []latency.FeedMinute{}
	}
	ids := a.FeedIDs()
	if ids == nil {
		ids = []string{}
	}
	return latencyFeedReply{Feed: id, Feeds: ids, Series: series}, nil
}

func apiTimingNTPCheck(s *Server, r *http.Request) (any, error) {
	a, err := s.latencyAnalyzer()
	if err != nil {
		return nil, err
	}
	return a.NTPCheck(), nil
}

// ntpProbeReply is the answer of POST /api/timing/ntp/sample with a server:
// one query to that server, nothing stored or applied.
type ntpProbeReply struct {
	OK          bool        `json:"ok"`
	Server      string      `json:"server"`         // normalised host:port
	Addr        string      `json:"addr,omitempty"` // address that answered
	T           int64       `json:"t"`              // Unix ms (host clock)
	Stratum     int         `json:"stratum"`
	RefID       string      `json:"refId"`
	Leap        int         `json:"leap"`
	OffsetMs    latency.Num `json:"offsetMs"` // true UTC - host clock
	DelayMs     latency.Num `json:"delayMs"`
	RootDelayMs latency.Num `json:"rootDelayMs"`
	RootDispMs  latency.Num `json:"rootDispMs"`
	Error       string      `json:"error,omitempty"`
	Saved       bool        `json:"saved"` // always false: the configuration is not changed
}

// probeServer is the ?server= / {"server": ...} argument of POST
// /api/timing/ntp/sample ("" = none).
func probeServer(r *http.Request) (string, error) {
	srv := strings.TrimSpace(r.URL.Query().Get("server"))
	if srv != "" {
		return srv, nil
	}
	body, err := readBody(r, 4096)
	if err != nil {
		return "", err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", nil
	}
	var in struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return "", badRequest("invalid body: %v", err)
	}
	return strings.TrimSpace(in.Server), nil
}

// validProbeHost checks a user-entered NTP server ("host", "host:port", IPv4
// or IPv6 literal) and returns it normalised.
func validProbeHost(srv string) (string, error) {
	if len(srv) > 253+8 {
		return "", errors.New("server name too long")
	}
	addr, err := ntp.NormalizeServer(srv)
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if net.ParseIP(strings.SplitN(host, "%", 2)[0]) != nil {
		return addr, nil
	}
	// Host name: letters, digits, '-', '.', '_' only.
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 {
			return "", errors.New("invalid host name")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", errors.New("invalid host name")
			}
		}
	}
	return addr, nil
}

// probeTimeout bounds a one-off query to a manually entered server.
const probeTimeout = 3 * time.Second

func apiTimingSample(s *Server, r *http.Request) (any, error) {
	srv, err := probeServer(r)
	if err != nil {
		return nil, err
	}
	if srv == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		sm, err := s.eng.NTP().SampleNow(ctx)
		if err != nil {
			return nil, errStatus(http.StatusBadGateway, "NTP query failed: %v", err)
		}
		return sm, nil
	}
	addr, err := validProbeHost(srv)
	if err != nil {
		return nil, badRequest("invalid NTP server %q: %v", srv, err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	c := ntp.Client{Timeout: probeTimeout}
	resp, err := c.Query(ctx, addr)
	out := ntpProbeReply{Server: addr, T: time.Now().UnixMilli()}
	if err != nil {
		out.Error = err.Error()
		nan := latency.Num(math.NaN())
		out.OffsetMs, out.DelayMs, out.RootDelayMs, out.RootDispMs = nan, nan, nan, nan
		return out, nil
	}
	out.OK = true
	out.Addr = resp.Addr
	out.T = resp.Time.UnixMilli()
	out.Stratum, out.RefID, out.Leap = resp.Stratum, resp.RefID, resp.Leap
	out.OffsetMs = latency.Num(float64(resp.Offset) / 1e6)
	out.DelayMs = latency.Num(float64(resp.Delay) / 1e6)
	out.RootDelayMs = latency.Num(float64(resp.RootDelay) / 1e6)
	out.RootDispMs = latency.Num(float64(resp.RootDispersion) / 1e6)
	return out, nil
}
