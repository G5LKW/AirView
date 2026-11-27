package server

// ACARS / VDL2 messages from an external decoder (internal/acars,
// internal/engine/acarswire.go). Admin only in aggregator mode: the
// messages are the host station's reception and their free text can name
// crew or passengers, so they are never public and never part of a public
// DTO. In standalone mode every client of the station sees them, like the
// rest of the station's data.

import (
	"net/http"
	"strconv"
	"strings"

	"airview/internal/acars"
)

func init() {
	routes = append(routes, acarsRoutes...)
}

var acarsRoutes = []Route{
	{Method: "GET", Path: "/api/acars", Group: grpCore, Access: AccessAdmin, API: apiACARS,
		Summary: "ACARS / VDL2 messages received from an external decoder (acarsdec, dumpvdl2), newest first. `enabled:false` and no messages when the input is off (`acars.enabled`).",
		Notes: "Messages are kept in memory only (`acars.keep`, default 2000). `icao` is the aircraft address the decoder reported (`icaoFrom:\"message\"`) or, " +
			"for messages without one, the live Mode S aircraft with the same registration (`icaoFrom:\"reg\"`). `labelName` is a generic description " +
			"of the label; many labels are airline-defined. In demo mode the messages are synthetic (station `DEMO`).",
		Params: []Param{
			{"icao", "only this aircraft address (6 hex digits)"},
			{"reg", "only this registration (dots, dashes and case ignored)"},
			{"flight", "flight id prefix (case-insensitive)"},
			{"label", "exact ACARS label, e.g. `H1`"},
			{"q", "text contains (case-insensitive)"},
			{"since", "only messages with a larger `id` (polling)"},
			{"limit", "1..2000 (default 100)"},
		},
		Returns: "`{\"enabled\":bool,\"messages\":[Message…]}`"},
	{Method: "GET", Path: "/api/acars/status", Group: grpCore, Access: AccessAdmin, API: apiACARSStatus,
		Summary: "State of the ACARS input: listen addresses and errors, counters (total, duplicates, parse errors, rate-limited), most frequent labels.",
		Returns: "`ACARSStatus`"},
}

// acarsList is the answer of GET /api/acars.
type acarsList struct {
	Enabled  bool            `json:"enabled"`
	Messages []acarsListItem `json:"messages"`
}

// acarsListItem is a message with the description of its label.
type acarsListItem struct {
	acars.Message
	LabelName string `json:"labelName,omitempty"`
}

func apiACARS(s *Server, r *http.Request) (any, error) {
	q := r.URL.Query()
	limit, err := qInt(r, "limit", 100, 1, 2000)
	if err != nil {
		return nil, err
	}
	f := acars.Filter{
		ICAO:   strings.TrimSpace(q.Get("icao")),
		Reg:    q.Get("reg"),
		Flight: q.Get("flight"),
		Label:  strings.TrimSpace(q.Get("label")),
		Text:   q.Get("q"),
		Limit:  limit,
	}
	if v := q.Get("since"); v != "" {
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, errStatus(http.StatusBadRequest, "since must be a message id")
		}
		f.SinceID = id
	}
	msgs, ok := s.eng.ACARSMessages(f)
	out := acarsList{Enabled: ok, Messages: make([]acarsListItem, len(msgs))}
	for i, m := range msgs {
		out.Messages[i] = acarsListItem{Message: m, LabelName: acars.LabelName(m.Label)}
	}
	return out, nil
}

func apiACARSStatus(s *Server, r *http.Request) (any, error) {
	return s.eng.ACARSStatus(), nil
}
