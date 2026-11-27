package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	avauth "airview/internal/auth"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
	"airview/internal/registry"
)

// Owner routes (/api/me/*, DESIGN §7, §8.2, §11). The route gate has
// already checked the session (AccessOwner) and, for {sid} routes, that
// the station belongs to the viewer (StationPath); a foreign or unknown
// SID answers 404. The reply shapes follow web/static/js/net/endpoints.js.

// errNoRegistry is answered when aggregator services are not wired yet.
var errNoRegistry = errStatus(http.StatusServiceUnavailable, "the station registry is not available")

func (s *Server) reg() (*registry.Registry, error) {
	if r := s.registry(); r != nil {
		return r, nil
	}
	return nil, errNoRegistry
}

// actorOf returns the audit actor of r.
func (s *Server) actorOf(r *http.Request) registry.Actor {
	if _, acct, ok := s.netSession(r); ok {
		return registry.Actor{ID: acct.ID, Name: acct.Username}
	}
	return registry.Actor{}
}

// ---------------------------------------------------------------- DTOs

// posDTO is an exact position (owner / admin scope only).
type posDTO struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Alt    float64 `json:"alt"`
	AltRef string  `json:"altRef,omitempty"`
}

func posOf(p registry.Position, ref string) *posDTO {
	if !p.Valid {
		return nil
	}
	return &posDTO{Lat: p.Lat, Lon: p.Lon, Alt: p.Alt, AltRef: ref}
}

// geoDTO is the effective position used for geometry and MLAT (HAE).
type geoDTO struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Alt    float64 `json:"alt"` // HAE, metres
	SigmaM float64 `json:"sigmaM"`
	Source string  `json:"source"`
}

// stationDTO is a station with exact data (the station's owner or the
// admin): "OwnerStation" of the web UI.
type stationDTO struct {
	SID        string `json:"sid"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	FeederID   string `json:"feederId,omitempty"`
	RxNo       int    `json:"rxNo"`
	UUID       string `json:"uuid,omitempty"`
	ClockClass string `json:"clockClass"`
	Admission  string `json:"admission"`
	Online     bool   `json:"online"`
	Created    int64  `json:"created"`  // Unix ms
	LastSeen   int64  `json:"lastSeen"` // Unix ms, 0 = never

	// Location: the effective position (else the declared one).
	Located   bool     `json:"located"`
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	Alt       *float64 `json:"alt"`
	LocStatus string   `json:"locStatus"`
	LocSource string   `json:"locSource,omitempty"`
	SigmaM    float64  `json:"sigmaM,omitempty"`

	Pin         string `json:"pin"`
	ShowName    bool   `json:"showName"`
	PublicStats bool   `json:"publicStats"`
	// PublicPoint is the stored public point exactly as others see it (null
	// = no public pin).
	PublicPoint  *publicdto.Pin          `json:"publicPoint"`
	Anchor       bool                    `json:"anchor"`
	Unidentified bool                    `json:"unidentified,omitempty"`
	Stats        *publicdto.StationStats `json:"stats"`

	// Admin view only.
	OwnerID   int64   `json:"ownerId,omitempty"`
	Owner     string  `json:"owner,omitempty"`
	Trust     int     `json:"trust,omitempty"`
	MsgRate   float64 `json:"msgRate,omitempty"`
	TokenUsed string  `json:"tokenId,omitempty"`
	// NeverAnchor: the admin barred the station from anchoring.
	NeverAnchor bool `json:"neverAnchor,omitempty"`
}

func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fptr(v float64) *float64 { return &v }

// stationDTO builds the exact view of st for its owner (admin=false) or the
// admin.
func (s *Server) stationDTO(ctx context.Context, st *registry.Station, admin bool, now time.Time) stationDTO {
	d := stationDTO{
		SID: st.SID, Name: st.Name, Kind: st.Kind, FeederID: st.FeederID, RxNo: st.RxNo, UUID: st.UUID,
		ClockClass: st.ClockCls.String(), Admission: st.Admission, Online: s.stationOnline(st, now),
		Created: unixMs(st.Created), LastSeen: unixMs(st.LastSeen),
		Located: st.Located, LocStatus: st.LocStatus,
		Pin: st.Pin, ShowName: st.ShowName, PublicStats: st.PublicStats,
		Anchor: st.Anchor, Unidentified: st.Has(registry.FlagUnidentified),
		Stats: s.stationStats(st, now),
	}
	switch {
	case st.Located:
		g := st.Effective
		d.Lat, d.Lon, d.Alt, d.LocSource, d.SigmaM = fptr(g.Lat), fptr(g.Lon), fptr(g.HAE), g.Source, g.SigmaM
	case st.Declared.Valid:
		p := st.Declared
		d.Lat, d.Lon, d.Alt, d.LocSource = fptr(p.Lat), fptr(p.Lon), fptr(p.Alt), model.LocSrcManual
	}
	if hasPublicPin(st) {
		d.PublicPoint = &publicdto.Pin{Lat: st.Public.Lat, Lon: st.Public.Lon, Precision: st.Public.Effective}
	}
	if admin {
		d.OwnerID, d.Trust, d.MsgRate, d.TokenUsed = st.OwnerID, st.Trust, d.Stats.MsgRate, st.TokenID
		d.NeverAnchor = st.Has(registry.FlagNoAnchor)
		if st.OwnerID != 0 {
			if reg := s.registry(); reg != nil {
				if a, err := reg.Accounts().ByID(ctx, st.OwnerID); err == nil {
					d.Owner = a.Username
				}
			}
		}
	}
	return d
}

// linkDTO is a live link of a station as its owner sees it.
type linkDTO struct {
	Proto      string  `json:"proto"`
	Since      int64   `json:"since"` // Unix ms
	MsgRate    float64 `json:"msgRate"`
	PosRate    float64 `json:"posRate,omitempty"`
	KBps       float64 `json:"kbps"`
	RTTMs      float64 `json:"rttMs,omitempty"`
	ClockClass string  `json:"clockClass,omitempty"`
	Synced     *bool   `json:"mlatSynced,omitempty"`
	Software   string  `json:"software,omitempty"`
	Pending    bool    `json:"pending,omitempty"`
	// IP is the link's address: the owner's own current IP (DESIGN §8.2)
	// on the owner's station pages and in the admin views; never public.
	IP string `json:"ip,omitempty"`
	// SplitSync: mlat-client links only (split_sync offered); ADSBRate: the
	// client's reported ADS-B positions per second; HostClampMs: an AVF
	// feeder's host-clock correction (ms, 0 = none).
	SplitSync   *bool   `json:"splitSync,omitempty"`
	ADSBRate    float64 `json:"adsbRate,omitempty"`
	HostClampMs float64 `json:"hostClampMs,omitempty"`
	// Bytes received on this connection so far (0 = unknown).
	Bytes uint64 `json:"bytes,omitempty"`
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func toLinkDTO(l *LiveLink, withIP bool) linkDTO {
	d := linkDTO{Proto: l.Proto, Since: unixMs(l.Since), MsgRate: round1(l.MsgRate),
		PosRate: round1(l.PosRate), KBps: round1(l.KBps), RTTMs: round1(l.RTTMs), ClockClass: l.ClockClass,
		Synced: l.Synced, Software: l.Software, Pending: l.Pending, SplitSync: l.SplitSync,
		ADSBRate: round1(l.ADSBRate), HostClampMs: round1(l.HostClampMs), Bytes: l.Bytes}
	if withIP && l.IP.IsValid() {
		d.IP = l.IP.String()
	}
	return d
}

// idTail is "…" plus the last 6 characters of a UUID or feeder ID.
func idTail(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	if s == "" {
		return ""
	}
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return "…" + s
}

func normUUID(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

// stationLinks returns the live links of station idx.
func (s *Server) stationLinks(idx model.StationIdx) []LiveLink {
	var out []LiveLink
	for _, l := range s.liveLinks() {
		if l.Station == idx {
			out = append(out, l)
		}
	}
	s.fillLinkRates(out, time.Now())
	return out
}

// fillLinkRates sets the message and position rates of the AVF links from
// their station's lane counters (UX-01): an AVF session carries several
// receivers, so the per-station lane is the right source, and the same
// rates are what the admin station list shows.
func (s *Server) fillLinkRates(links []LiveLink, now time.Time) {
	if s.eng == nil {
		return
	}
	tr := s.eng.Tracker()
	if tr == nil {
		return
	}
	lanes := tr.Lanes()
	if lanes == nil {
		return
	}
	for i := range links {
		l := &links[i]
		if l.Proto != "avf" || l.Station == 0 {
			continue
		}
		if li, ok := lanes.Info(l.Station); ok {
			fps, pps := s.rates.rates(l.Station, li.Frames, li.Positions, now)
			if fps > 0 || l.MsgRate == 0 {
				l.MsgRate = fps
			}
			l.PosRate = pps
		}
	}
}

// ---------------------------------------------------------------- handlers

type meReply struct {
	ID       int64        `json:"id"`
	Username string       `json:"username"`
	Scope    string       `json:"scope"`
	Stations []stationDTO `json:"stations"`
}

func (s *Server) ownStationDTOs(r *http.Request) ([]stationDTO, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []stationDTO{}
	for _, st := range reg.OwnerStations(s.viewerOf(r).OwnerID) {
		out = append(out, s.stationDTO(r.Context(), &st, false, now))
	}
	return out, nil
}

func apiMe(s *Server, r *http.Request) (any, error) {
	sts, err := s.ownStationDTOs(r)
	if err != nil {
		return nil, err
	}
	_, acct, _ := s.netSession(r)
	return meReply{ID: acct.ID, Username: acct.Username, Scope: s.viewerOf(r).Scope.String(), Stations: sts}, nil
}

func apiMeStations(s *Server, r *http.Request) (any, error) {
	sts, err := s.ownStationDTOs(r)
	if err != nil {
		return nil, err
	}
	return map[string]any{"stations": sts}, nil
}

// estimateDTO is a location estimate (owner / admin).
type estimateDTO struct {
	ID     string  `json:"id"`
	T      int64   `json:"t"`
	Method string  `json:"method"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Alt    float64 `json:"alt"` // HAE
	SigmaM float64 `json:"sigmaM"`
	SigmaE float64 `json:"sigmaE"`
	SigmaN float64 `json:"sigmaN"`
	SigmaU float64 `json:"sigmaU"`
	N      int     `json:"n"`
	Status string  `json:"status"`
}

// locationDTO is the Location section of a station page.
type locationDTO struct {
	Status    string        `json:"status"`
	Effective *geoDTO       `json:"effective"`
	Declared  *posDTO       `json:"declared"`
	GNSS      *posDTO       `json:"gnss"`
	Estimated *posDTO       `json:"estimated"`
	EstSigmaM float64       `json:"estSigmaM,omitempty"`
	Estimates []estimateDTO `json:"estimates"`
}

// connectionDTO is the Connection section of a station page.
type connectionDTO struct {
	Proto      string    `json:"proto,omitempty"` // of the oldest live link
	Software   string    `json:"software,omitempty"`
	Since      int64     `json:"since,omitempty"`
	KBps       float64   `json:"kbps"`
	RTTMs      float64   `json:"rttMs,omitempty"` // mean of the live links
	LateFrames uint64    `json:"lateFrames"`
	Links      []linkDTO `json:"links"`
}

// laneDTO is the station's lane counters (internal/tracker).
type laneDTO struct {
	Frames     uint64 `json:"frames"`
	Positions  uint64 `json:"positions"`
	LateFrames uint64 `json:"lateFrames"`
	Aircraft   int    `json:"aircraft"` // this minute
	FirstMs    int64  `json:"firstMs"`
	LastMs     int64  `json:"lastMs"`
}

// stationDetail is GET /api/me/stations/{sid}: the station plus its
// sections. Timing and MLAT stay null until the MLAT engine publishes
// per-station state.
type stationDetail struct {
	stationDTO
	Connection connectionDTO `json:"connection"`
	Location   locationDTO   `json:"location"`
	Lane       *laneDTO      `json:"lane"`
	Timing     any           `json:"timing"`
	MLAT       any           `json:"mlat"`
	// PendingPosition: a feeder-announced position held for confirmation.
	PendingPosition *pendingPosDTO `json:"pendingPosition,omitempty"`
	// MLATPosition: the MLAT position source, the survey and the last
	// survey-related change (owner notice, DESIGN §19.3).
	MLATPosition *mlatPositionDTO `json:"mlatPosition,omitempty"`
}

func (s *Server) stationDetail(ctx context.Context, st *registry.Station, admin bool) (*stationDetail, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := &stationDetail{stationDTO: s.stationDTO(ctx, st, admin, now)}
	loc := locationDTO{Status: st.LocStatus, Declared: posOf(st.Declared, st.DeclaredAltRef), GNSS: posOf(st.GNSS, ""),
		Estimated: posOf(st.Estimated, ""), EstSigmaM: st.EstSigmaM, Estimates: []estimateDTO{}}
	if st.Located {
		g := st.Effective
		loc.Effective = &geoDTO{Lat: g.Lat, Lon: g.Lon, Alt: g.HAE, SigmaM: g.SigmaM, Source: g.Source}
	}
	es, err := reg.Estimates(ctx, st.Idx)
	if err != nil {
		return nil, regErr(err)
	}
	for _, e := range es {
		loc.Estimates = append(loc.Estimates, estimateDTO{ID: strconv.FormatInt(e.ID, 10), T: unixMs(e.T), Method: e.Method,
			Lat: e.Lat, Lon: e.Lon, Alt: e.HAE, SigmaM: horizSigma(e.SigmaE, e.SigmaN),
			SigmaE: e.SigmaE, SigmaN: e.SigmaN, SigmaU: e.SigmaU, N: e.N, Status: e.Status})
	}
	out.Location = loc
	conn := connectionDTO{Links: []linkDTO{}}
	links := s.stationLinks(st.Idx)
	sort.Slice(links, func(i, j int) bool { return links[i].Since.Before(links[j].Since) })
	var rtt float64
	nRTT := 0
	for i := range links {
		l := &links[i]
		if i == 0 {
			conn.Proto, conn.Since = l.Proto, unixMs(l.Since)
		}
		if conn.Software == "" {
			conn.Software = l.Software
		}
		conn.KBps += l.KBps
		if l.RTTMs > 0 {
			rtt += l.RTTMs
			nRTT++
		}
		conn.Links = append(conn.Links, toLinkDTO(l, true))
	}
	conn.KBps = round1(conn.KBps)
	if nRTT > 0 {
		conn.RTTMs = round1(rtt / float64(nRTT))
	}
	if lanes := s.eng.Tracker().Lanes(); lanes != nil {
		if li, ok := lanes.Info(st.Idx); ok {
			out.Lane = &laneDTO{Frames: li.Frames, Positions: li.Positions, LateFrames: li.LateFrames,
				Aircraft: li.Aircraft, FirstMs: li.FirstMs, LastMs: li.LastMs}
			conn.LateFrames = li.LateFrames
		}
	}
	out.Connection = conn
	s.fillHardenDetail(ctx, reg, st, admin, out)
	return out, nil
}

// horizSigma combines east and north 1-sigma values into one horizontal
// 1-sigma (root mean square).
func horizSigma(e, n float64) float64 {
	return round1(math.Sqrt((e*e + n*n) / 2))
}

func apiMeStation(s *Server, r *http.Request) (any, error) {
	st, _ := selectedStation(r)
	return s.stationDetail(r.Context(), &st, false)
}

// stationUpdate is the body of POST /api/me/stations/{sid}; every member is
// optional.
type stationUpdate struct {
	Name        *string         `json:"name"`
	Pin         *string         `json:"pin"`
	ShowName    *bool           `json:"showName"`
	PublicStats *bool           `json:"publicStats"`
	Declared    json.RawMessage `json:"declared"`
}

func decodeBody(r *http.Request, v any, limit int64) error {
	b, err := readBody(r, limit)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return badRequest("a JSON body is required")
	}
	return decodeBodyBytes(b, v)
}

// decodeBodyBytes decodes an already read JSON body (unknown members are
// refused).
func decodeBodyBytes(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// regErr maps registry errors to HTTP answers with user-safe messages.
func regErr(err error) error {
	var rl *registry.RateLimitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rl):
		return &rateLimitedError{retry: rl.RetryAfter}
	case errors.Is(err, registry.ErrClaimInvalid), errors.Is(err, registry.ErrNotClaimable),
		errors.Is(err, registry.ErrNeedsAdmin), errors.Is(err, registry.ErrIPMismatch):
		return errStatus(http.StatusBadRequest, "%s", err.Error())
	case errors.Is(err, registry.ErrAlreadyOwned):
		return errStatus(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, registry.ErrNotOwner):
		return errStatus(http.StatusForbidden, "%s", err.Error())
	case errors.Is(err, registry.ErrStationNotFound), errors.Is(err, registry.ErrUnknownAccount),
		errors.Is(err, avauth.ErrAccountNotFound):
		return notFound("not found")
	case errors.Is(err, registry.ErrInvalid), errors.Is(err, registry.ErrTokenInvalid):
		return badRequest("%s", strings.TrimPrefix(err.Error(), "registry: "))
	case errors.Is(err, registry.ErrClosed):
		return errNoRegistry
	}
	return err
}

// rateLimitedError is a 429 with a Retry-After header.
type rateLimitedError struct{ retry time.Duration }

func (e *rateLimitedError) Error() string { return registry.ErrRateLimited.Error() }

// applyStationUpdate applies u to the selected own station and returns the
// new owner view.
func (s *Server) applyStationUpdate(r *http.Request, u stationUpdate) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	ctx := r.Context()
	if u.Pin != nil || u.ShowName != nil || u.PublicStats != nil {
		pin, show, pub := st.Pin, st.ShowName, st.PublicStats
		if u.Pin != nil {
			if !model.ValidPin(*u.Pin) {
				return nil, badRequest("pin: hidden | region | cell01")
			}
			pin = *u.Pin
		}
		if u.ShowName != nil {
			show = *u.ShowName
		}
		if u.PublicStats != nil {
			pub = *u.PublicStats
		}
		if st, err = reg.SetPrivacy(ctx, st.Idx, pin, show, pub); err != nil {
			return nil, regErr(err)
		}
	}
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if len([]rune(name)) > 64 {
			return nil, badRequest("name: at most 64 characters")
		}
		if st, err = reg.SetName(ctx, st.Idx, name); err != nil {
			return nil, regErr(err)
		}
	}
	if len(u.Declared) > 0 {
		var p registry.Position
		ref := ""
		if string(u.Declared) != "null" {
			var d posDTO
			if err := json.Unmarshal(u.Declared, &d); err != nil {
				return nil, badRequest("declared: %v", err)
			}
			if d.Lat < -90 || d.Lat > 90 || d.Lon < -180 || d.Lon > 180 || d.Alt < -500 || d.Alt > 9000 || (d.Lat == 0 && d.Lon == 0) {
				return nil, badRequest("location: latitude -90..90, longitude -180..180, alt -500..9000 m")
			}
			p = registry.Position{Lat: d.Lat, Lon: d.Lon, Alt: d.Alt, Valid: true}
			ref = d.AltRef
			if ref == "" {
				ref = model.AltRefMSL
			}
		}
		if st, err = reg.SetDeclared(ctx, st.Idx, p, ref); err != nil {
			return nil, regErr(err)
		}
	}
	s.cache.invalidate()
	return s.stationDTO(ctx, &st, false, time.Now()), nil
}

func apiMeStationUpdate(s *Server, r *http.Request) (any, error) {
	var u stationUpdate
	if err := decodeBody(r, &u, 16<<10); err != nil {
		return nil, err
	}
	return s.applyStationUpdate(r, u)
}

// apiMePrivacy is PUT .../privacy {pin, publicStats, showName}.
func apiMePrivacy(s *Server, r *http.Request) (any, error) {
	var b struct {
		Pin         *string `json:"pin"`
		ShowName    *bool   `json:"showName"`
		PublicStats *bool   `json:"publicStats"`
	}
	if err := decodeBody(r, &b, 4<<10); err != nil {
		return nil, err
	}
	return s.applyStationUpdate(r, stationUpdate{Pin: b.Pin, ShowName: b.ShowName, PublicStats: b.PublicStats})
}

// apiMeName is PUT .../name {name}.
func apiMeName(s *Server, r *http.Request) (any, error) {
	var b struct {
		Name *string `json:"name"`
	}
	if err := decodeBody(r, &b, 4<<10); err != nil {
		return nil, err
	}
	if b.Name == nil {
		return nil, badRequest("name is required")
	}
	return s.applyStationUpdate(r, stationUpdate{Name: b.Name})
}

// apiMeLocation is PUT .../location {lat, lon, alt, altRef} (the declared
// position) or {"clear":true}.
func apiMeLocation(s *Server, r *http.Request) (any, error) {
	raw, err := readBody(r, 4<<10)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Clear bool `json:"clear"`
	}
	_ = json.Unmarshal(raw, &probe)
	if probe.Clear {
		return s.applyStationUpdate(r, stationUpdate{Declared: json.RawMessage("null")})
	}
	var d posDTO
	if err := decodeBodyBytes(raw, &d); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(d)
	return s.applyStationUpdate(r, stationUpdate{Declared: b})
}

// apiMeLocationConfirm is POST .../location/confirm {estimate: "<id>"}: the
// owner accepts a survey estimate as the station's position (DESIGN §10.1
// "used for MLAT after owner confirmation").
func apiMeLocationConfirm(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	var b struct {
		Estimate json.Number `json:"estimate"`
	}
	if err := decodeBody(r, &b, 1<<10); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(b.Estimate.String(), 10, 64)
	if err != nil {
		return nil, badRequest("estimate: an estimate id from the station's location section")
	}
	st, _ := selectedStation(r)
	es, err := reg.Estimates(r.Context(), st.Idx)
	if err != nil {
		return nil, regErr(err)
	}
	for _, e := range es {
		if e.ID != id {
			continue
		}
		src := model.LocSrcEstimated
		if e.Method == "gnss" {
			src = model.LocSrcGNSS
		}
		g := model.StationGeo{Lat: e.Lat, Lon: e.Lon, HAE: e.HAE, SigmaM: horizSigma(e.SigmaE, e.SigmaN), Source: src}
		if st, err = reg.SetEffective(r.Context(), st.Idx, g, true, model.LocConfirmed); err != nil {
			return nil, regErr(err)
		}
		_ = reg.Audit(r.Context(), s.actorOf(r), "station.location.confirm", st.SID, `{"method":"`+jsonSafe(e.Method)+`"}`)
		s.cache.invalidate()
		return s.stationDTO(r.Context(), &st, false, time.Now()), nil
	}
	return nil, notFound("no such estimate")
}

// jsonSafe keeps only characters that need no escaping in a JSON string.
func jsonSafe(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func apiMeUnclaim(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	if _, err := reg.Unclaim(registry.WithActorName(r.Context(), s.actorOf(r).Name), s.viewerOf(r).OwnerID, st.Idx); err != nil {
		return nil, regErr(err)
	}
	s.cache.invalidate()
	return map[string]bool{"ok": true}, nil
}

type stationStatsReply struct {
	SID      string                  `json:"sid"`
	Now      int64                   `json:"now"`
	Online   bool                    `json:"online"`
	Stats    *publicdto.StationStats `json:"stats"`
	Lane     *laneDTO                `json:"lane"`
	Links    []linkDTO               `json:"links"`
	LastSeen int64                   `json:"lastSeen"`
}

func apiMeStationStats(s *Server, r *http.Request) (any, error) {
	st, _ := selectedStation(r)
	now := time.Now()
	out := stationStatsReply{SID: st.SID, Now: now.UnixMilli(), Online: s.stationOnline(&st, now), Stats: s.stationStats(&st, now),
		Links: []linkDTO{}, LastSeen: unixMs(st.LastSeen)}
	if lanes := s.eng.Tracker().Lanes(); lanes != nil {
		if li, ok := lanes.Info(st.Idx); ok {
			out.Lane = &laneDTO{Frames: li.Frames, Positions: li.Positions, LateFrames: li.LateFrames,
				Aircraft: li.Aircraft, FirstMs: li.FirstMs, LastMs: li.LastMs}
		}
	}
	for _, l := range s.stationLinks(st.Idx) {
		out.Links = append(out.Links, toLinkDTO(&l, true))
	}
	return out, nil
}

type connDTO struct {
	On     int64  `json:"on"`
	Off    int64  `json:"off"` // 0 = still open
	IPHash string `json:"ipHash"`
	Proto  string `json:"proto"`
	Bytes  int64  `json:"bytes"`
}

func apiMeConnections(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	days, err := qInt(r, "days", 7, 1, 30)
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 200, 1, 1000)
	if err != nil {
		return nil, err
	}
	since := time.Now().AddDate(0, 0, -days)
	if st.OwnedSince.After(since) {
		since = st.OwnedSince // never the previous owner's connections
	}
	es, err := reg.ConnLog(r.Context(), st.Idx, since, limit)
	if err != nil {
		return nil, regErr(err)
	}
	// UX-01: the byte count of a connection is stored when it closes; an
	// open row shows the bytes of the live links of its protocol so far.
	liveBytes := map[string]uint64{}
	for _, l := range s.stationLinks(st.Idx) {
		liveBytes[l.Proto] += l.Bytes
	}
	out := []connDTO{}
	for _, e := range es {
		d := connDTO{On: unixMs(e.On), Off: unixMs(e.Off), IPHash: e.IPHash, Proto: e.Proto, Bytes: e.Bytes}
		if e.Off.IsZero() && d.Bytes == 0 {
			d.Bytes = int64(liveBytes[e.Proto])
			liveBytes[e.Proto] = 0 // once per protocol
		}
		out = append(out, d)
	}
	return map[string]any{"connections": out}, nil
}

func apiMeAircraft(s *Server, r *http.Request) (any, error) {
	v := s.viewerOf(r)
	st, ok := selectedStation(r)
	snapAny := s.snapshotFor(v, st.Idx, ok)
	snap, isModel := snapAny.(model.LiveSnapshot)
	if !isModel {
		return snapAny, nil
	}
	if r.URL.Query().Get("only") == "mine" {
		out := snap
		out.Aircraft = make([]model.Aircraft, 0, len(snap.Aircraft))
		for _, a := range snap.Aircraft {
			if a.Mine || (v.Scope == model.ScopeAdmin && len(a.Stations) > 0) {
				out.Aircraft = append(out.Aircraft, a)
			}
		}
		return out, nil
	}
	return snap, nil
}

func apiMeTarAircraft(s *Server, r *http.Request) (any, error) {
	v := s.viewerOf(r)
	tr := s.eng.Tracker()
	snap := tr.SnapshotView(s.trackerView(v, 0, false))
	pub := publicdto.Snapshot{Now: snap.Now, Aircraft: []publicdto.Aircraft{}}
	if tr.Network() {
		for i := range snap.Aircraft {
			a := &snap.Aircraft[i]
			if a.Mine || (v.Scope == model.ScopeAdmin && len(a.Stations) > 0) {
				pub.Aircraft = append(pub.Aircraft, publicdto.FromAircraft(a))
			}
		}
	}
	f := publicdto.TarFromSnapshot(&pub, 0)
	return &f, nil
}

// ---------------------------------------------------------------- feed check

// checkLink is one link of the Feed Check page. It holds only non-locating
// link data; the station's SID and name only for its owner.
type checkLink struct {
	Proto        string  `json:"proto"`
	IDTail       string  `json:"idTail,omitempty"` // "…" + last 6 of the UUID / feeder ID
	UUIDMissing  bool    `json:"uuidMissing"`
	UUIDMismatch bool    `json:"uuidMismatch"`
	Since        int64   `json:"since"`
	MsgRate      float64 `json:"msgRate"`
	PosRate      float64 `json:"posRate"`
	KBps         float64 `json:"kbps"`
	RTTMs        float64 `json:"rttMs,omitempty"`
	ClockClass   string  `json:"clockClass,omitempty"`
	MLATSynced   *bool   `json:"mlatSynced"`
	Pending      bool    `json:"pending,omitempty"`
	// Registration state of the attached station (false/empty when the
	// link is not attached yet).
	Registered  bool   `json:"registered"`
	Claimed     bool   `json:"claimed"`
	Yours       bool   `json:"yours"`
	Claimable   bool   `json:"claimable"`
	ClaimMethod string `json:"claimMethod,omitempty"` // "code" (native: claim code from the feeder), "uuid" (compat, from this address), "admin"
	Admission   string `json:"admission,omitempty"`
	SID         string `json:"sid,omitempty"`         // owner only
	StationName string `json:"stationName,omitempty"` // owner only
	// Software of the link (own stations only); Details reports whether
	// the rates, clock and MLAT fields are filled (own stations only).
	Software string `json:"software,omitempty"`
	Details  bool   `json:"details"`
}

type checkReply struct {
	Now   int64       `json:"now"`
	Links []checkLink `json:"links"`
	// Matched reports whether any live link comes from the visitor's address.
	Matched bool `json:"matched"`
	// Count is the number of matching links; Limited is set when the
	// visitor is not signed in, so the links are not listed (PRIV-A8).
	Count   int  `json:"count"`
	Limited bool `json:"limited,omitempty"`
}

// apiMeCheck is the Feed Check page: the live links from the visitor's own
// address (exact IPv4, IPv6 /64). It shows only non-locating link data and
// answers 200 with an empty list when nothing matches.
//
// PRIV-A8: one public address can be shared by many subscribers
// (carrier-grade NAT, a hosting /64), so the address alone proves nothing.
// A visitor who is not signed in learns only that links exist (Matched,
// Count). A signed-in owner gets the full details of the links of their
// own stations and, for the other links, only what claiming needs
// (protocol, ID tail, connected since, registered / claimed / claimable).
func apiMeCheck(s *Server, r *http.Request) (any, error) {
	out := checkReply{Now: time.Now().UnixMilli(), Links: []checkLink{}}
	ip, err := netip.ParseAddr(s.remoteIP(r))
	if err != nil {
		return out, nil
	}
	ip = ip.Unmap()
	reg := s.registry()
	v := s.viewerOf(r)
	signedIn := v.Scope >= model.ScopeOwner && v.OwnerID != 0 || v.Scope == model.ScopeAdmin
	var mine []LiveLink
	uuids := map[string]bool{}
	for _, l := range s.liveLinks() {
		if avauth.SameClient(l.IP, ip) {
			mine = append(mine, l)
			if l.UUID != "" {
				uuids[normUUID(l.UUID)] = true
			}
		}
	}
	out.Count = len(mine)
	out.Matched = len(mine) > 0
	if !signedIn {
		out.Limited = out.Matched
		return out, nil
	}
	s.fillLinkRates(mine, time.Now())
	sort.Slice(mine, func(i, j int) bool { return mine[i].Since.Before(mine[j].Since) })
	for i := range mine {
		l := &mine[i]
		id := l.UUID
		if id == "" {
			id = l.FeederID
		}
		c := checkLink{Proto: l.Proto, IDTail: idTail(id), UUIDMissing: l.Proto != "avf" && l.UUID == "",
			UUIDMismatch: len(uuids) > 1 && l.UUID != "", Since: unixMs(l.Since)}
		yours := false
		if reg != nil && l.Station != 0 {
			if st, ok := reg.Station(l.Station); ok && !st.Deleting {
				c.Registered, c.Claimed = true, st.OwnerID != 0
				if v.OwnerID != 0 && st.OwnerID == v.OwnerID {
					yours = true
					c.Yours, c.SID, c.StationName = true, st.SID, st.Name
				}
				if v.Scope == model.ScopeAdmin {
					yours = true // the admin sees every link anyway
				}
				if yours {
					c.Admission = st.Admission
				}
				if st.OwnerID == 0 {
					switch {
					case st.Kind == model.StationKindAVF:
						c.Claimable, c.ClaimMethod = true, "code"
					case st.UUID != "":
						c.Claimable, c.ClaimMethod = true, "uuid"
					default:
						c.ClaimMethod = "admin"
					}
				}
			}
		}
		if yours || (l.Station == 0 && v.Scope == model.ScopeAdmin) {
			c.MsgRate, c.PosRate, c.KBps, c.RTTMs = round1(l.MsgRate), round1(l.PosRate), round1(l.KBps), round1(l.RTTMs)
			c.ClockClass, c.MLATSynced, c.Pending, c.Software = l.ClockClass, l.Synced, l.Pending, l.Software
		}
		c.Details = yours
		out.Links = append(out.Links, c)
	}
	return out, nil
}

// ---------------------------------------------------------------- claiming

type claimBody struct {
	Code string `json:"code"`
	UUID string `json:"uuid"`
}

func apiMeClaim(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	var b claimBody
	if err := decodeBody(r, &b, 4<<10); err != nil {
		return nil, err
	}
	v := s.viewerOf(r)
	if v.OwnerID == 0 {
		return nil, errStatus(http.StatusForbidden, "claiming needs an owner account")
	}
	client := s.remoteIP(r)
	var st registry.Station
	ctx := registry.WithActorName(r.Context(), s.actorOf(r).Name) // UX-08
	switch {
	case strings.TrimSpace(b.Code) != "":
		st, err = reg.RedeemClaim(ctx, v.OwnerID, b.Code, client)
	case strings.TrimSpace(b.UUID) != "":
		visitor, _ := netip.ParseAddr(client)
		sameIP := compatClaimLink(s.liveLinks(), normUUID(b.UUID), visitor.Unmap(), time.Now())
		st, err = reg.ClaimCompat(ctx, v.OwnerID, b.UUID, sameIP, client)
	default:
		return nil, badRequest("send {\"code\":...} or {\"uuid\":...}")
	}
	if err != nil {
		return nil, regErr(err)
	}
	s.cache.invalidate()
	return s.stationDTO(r.Context(), &st, false, time.Now()), nil
}

// compatClaimMinAge is how long the station's link must have been up
// before a same-address compat claim is accepted.
const compatClaimMinAge = 5 * time.Minute

// compatClaimLink decides the "same IP" proof of a compat claim (SEC-04).
// The Beast UUID is self-declared and port 30004 is unauthenticated, so a
// link that merely carries the UUID proves nothing: an attacker could open
// one from their own address just before claiming. The claim is accepted
// only when every live link with this UUID comes from one client (several
// clients = a clone or a spoof: refused), the visitor is that client, and
// the earliest of those links has been up for compatClaimMinAge.
func compatClaimLink(links []LiveLink, want string, visitor netip.Addr, now time.Time) bool {
	var first *LiveLink
	for i := range links {
		l := &links[i]
		if l.UUID == "" || normUUID(l.UUID) != want {
			continue
		}
		if first != nil && !avauth.SameClient(first.IP, l.IP) {
			return false // the UUID is live from two clients
		}
		if first == nil || l.Since.Before(first.Since) {
			first = l
		}
	}
	return first != nil && avauth.SameClient(first.IP, visitor) && !first.Since.IsZero() &&
		now.Sub(first.Since) >= compatClaimMinAge
}

// ---------------------------------------------------------------- export / delete

func rawMeExport(s *Server, w http.ResponseWriter, r *http.Request) {
	reg := s.registry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, errNoRegistry.Error())
		return
	}
	v := s.viewerOf(r)
	if v.OwnerID == 0 {
		writeError(w, http.StatusForbidden, "no owner account")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="airview-my-data.zip"`)
	if err := reg.ExportOwner(r.Context(), v.OwnerID, w); err != nil {
		// The zip has started: the client sees a broken file.
		s.log.Warn("owner export failed", "err", err)
	}
}

func rawMeDelete(s *Server, w http.ResponseWriter, r *http.Request) {
	reg := s.registry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, errNoRegistry.Error())
		return
	}
	var b struct {
		Confirm string `json:"confirm"`
	}
	if raw, _ := readBody(r, 1<<10); len(strings.TrimSpace(string(raw))) > 0 {
		if err := decodeBodyBytes(raw, &b); err != nil || b.Confirm != "DELETE" {
			writeError(w, http.StatusBadRequest, `send {"confirm":"DELETE"} or no body`)
			return
		}
	}
	v := s.viewerOf(r)
	if v.Scope != model.ScopeOwner || v.OwnerID == 0 {
		writeError(w, http.StatusForbidden, "only owner accounts can delete themselves here")
		return
	}
	actor := s.actorOf(r)
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.auth.net.service().Logout(r.Context(), c.Value)
	}
	if err := reg.DeleteOwner(r.Context(), actor, v.OwnerID, true); err != nil {
		var he *httpError
		if errors.As(regErr(err), &he) {
			writeError(w, he.status, he.msg)
		} else {
			writeError(w, http.StatusInternalServerError, "deleting the account failed")
		}
		return
	}
	s.shareStore().revokeOwner(v.OwnerID, "account deleted")
	s.cache.invalidate()
	http.SetCookie(w, netCookie("", -1, s.client(r).Secure))
	writeJSON(w, r, http.StatusOK, map[string]bool{"ok": true})
}
