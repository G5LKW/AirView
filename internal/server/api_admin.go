package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"airview/internal/ingest/beastin"
	"airview/internal/model"
	"airview/internal/registry"
	"airview/internal/tracker"
)

// Admin routes (/api/admin/*, DESIGN §11 "Admin console"). The route gate
// has checked the admin session (with second factor). Every view that shows
// exact station data or addresses is written to the audit log (§8.2). The
// reply shapes follow web/static/js/net/endpoints.js.

type adminStationsReply struct {
	Stations []stationDTO   `json:"stations"`
	Total    int            `json:"total"`  // matching stations before limit / offset
	Counts   map[string]int `json:"counts"` // per admission state, all stations
}

// apiAdminStations lists the registry with exact positions (the admin map
// view); the call is audited as "station.list".
func apiAdminStations(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 500, 1, 5000)
	if err != nil {
		return nil, err
	}
	offset, err := qInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		return nil, err
	}
	after, err := qInt(r, "after", 0, 0, 1<<31-1)
	if err != nil {
		return nil, err
	}
	online := qStr(r, "online")
	if err := reg.Audit(r.Context(), s.actorOf(r), "station.list", "", ""); err != nil {
		return nil, regErr(err) // no exact data without an audit record
	}
	f := registry.Filter{Query: qStr(r, "q"), Admission: qStr(r, "admission"), Kind: qStr(r, "kind"),
		Unclaimed: qBool(r, "unclaimed", false), After: model.StationIdx(after)}
	sortKey, desc := qStr(r, "sort"), qStr(r, "dir") == "desc"
	if !adminStationSorts[sortKey] {
		return nil, badRequest("sort: name | sid | kind | admission | online | lastSeen | trust | created | owner")
	}
	now := time.Now()
	out := adminStationsReply{Stations: []stationDTO{}, Counts: reg.Count()}
	var match []registry.Station
	for _, st := range reg.List(f) {
		if online != "" {
			want := online == "1" || online == "true"
			if s.stationOnline(&st, now) != want {
				continue
			}
		}
		match = append(match, st)
	}
	out.Total = len(match)
	if sortKey != "" {
		s.sortStations(r, match, sortKey, desc, now)
	}
	for i := offset; i < len(match) && len(out.Stations) < limit; i++ {
		out.Stations = append(out.Stations, s.stationDTO(r.Context(), &match[i], true, now))
	}
	return out, nil
}

// adminStationSorts are the sort keys of GET /api/admin/stations ("" =
// registry order, by station index).
var adminStationSorts = map[string]bool{"": true, "name": true, "sid": true, "kind": true, "admission": true, "online": true,
	"lastSeen": true, "trust": true, "created": true, "owner": true}

// sortStations orders the matched stations before paging (stable; ties by
// station index), so that pages of a 1,000-station registry are consistent.
func (s *Server) sortStations(r *http.Request, sts []registry.Station, key string, desc bool, now time.Time) {
	var owners map[int64]string
	if key == "owner" {
		owners = map[int64]string{}
		if reg := s.registry(); reg != nil {
			for _, st := range sts {
				if _, ok := owners[st.OwnerID]; ok || st.OwnerID == 0 {
					continue
				}
				if a, err := reg.Accounts().ByID(r.Context(), st.OwnerID); err == nil {
					owners[st.OwnerID] = strings.ToLower(a.Username)
				}
			}
		}
	}
	online := map[model.StationIdx]bool{}
	if key == "online" {
		for i := range sts {
			online[sts[i].Idx] = s.stationOnline(&sts[i], now)
		}
	}
	cmpStr := func(a, b string) int { return strings.Compare(a, b) }
	cmpInt := func(a, b int64) int {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	}
	b2i := func(v bool) int64 {
		if v {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(sts, func(a, b registry.Station) int {
		c := 0
		switch key {
		case "name":
			c = cmpStr(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case "sid":
			c = cmpStr(a.SID, b.SID)
		case "kind":
			c = cmpStr(a.Kind, b.Kind)
		case "admission":
			c = cmpStr(a.Admission, b.Admission)
		case "online":
			c = cmpInt(b2i(online[a.Idx]), b2i(online[b.Idx]))
			if c == 0 {
				c = cmpInt(a.LastSeen.UnixMilli(), b.LastSeen.UnixMilli())
			}
		case "lastSeen":
			c = cmpInt(a.LastSeen.UnixMilli(), b.LastSeen.UnixMilli())
		case "trust":
			c = cmpInt(int64(a.Trust), int64(b.Trust))
		case "created":
			c = cmpInt(a.Created.UnixMilli(), b.Created.UnixMilli())
		case "owner":
			c = cmpStr(owners[a.OwnerID], owners[b.OwnerID])
		}
		if desc {
			c = -c
		}
		if c == 0 {
			c = cmpInt(int64(a.Idx), int64(b.Idx))
		}
		return c
	})
}

// adminLinkDTO is a live link with its address (admin only, audited).
type adminLinkDTO struct {
	linkDTO
	UUID     string `json:"uuid,omitempty"`
	FeederID string `json:"feederId,omitempty"`
	SID      string `json:"sid,omitempty"`
}

func (s *Server) adminLinks(filter func(*LiveLink) bool) []adminLinkDTO {
	reg := s.registry()
	out := []adminLinkDTO{}
	for _, l := range s.liveLinks() {
		if filter != nil && !filter(&l) {
			continue
		}
		d := adminLinkDTO{linkDTO: toLinkDTO(&l, true), UUID: l.UUID, FeederID: l.FeederID}
		if reg != nil && l.Station != 0 {
			if st, ok := reg.Station(l.Station); ok {
				d.SID = st.SID
			}
		}
		out = append(out, d)
	}
	return out
}

type adminStationDetail struct {
	stationDetail
	LiveLinks []adminLinkDTO `json:"liveLinks"`
}

func apiAdminStation(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	if err := reg.Audit(r.Context(), s.actorOf(r), "station.reveal", st.SID, ""); err != nil {
		return nil, regErr(err)
	}
	d, err := s.stationDetail(r.Context(), &st, true)
	if err != nil {
		return nil, err
	}
	idx := st.Idx
	return adminStationDetail{stationDetail: *d, LiveLinks: s.adminLinks(func(l *LiveLink) bool { return l.Station == idx })}, nil
}

// revealReply is POST /api/admin/stations/{sid}/reveal.
type revealReply struct {
	Lat    *float64 `json:"lat"`
	Lon    *float64 `json:"lon"`
	Alt    *float64 `json:"alt"`
	Source string   `json:"source,omitempty"`
	SigmaM float64  `json:"sigmaM"`
	IP     string   `json:"ip,omitempty"` // of the oldest live link
}

func apiAdminReveal(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	if err := reg.Audit(r.Context(), s.actorOf(r), "station.reveal", st.SID, ""); err != nil {
		return nil, regErr(err)
	}
	d := s.stationDTO(r.Context(), &st, true, time.Now())
	out := revealReply{Lat: d.Lat, Lon: d.Lon, Alt: d.Alt, Source: d.LocSource, SigmaM: d.SigmaM}
	var since time.Time
	for _, l := range s.stationLinks(st.Idx) {
		if l.IP.IsValid() && (since.IsZero() || l.Since.Before(since)) {
			out.IP, since = l.IP.String(), l.Since
		}
	}
	return out, nil
}

// adminStationUpdate is the body of POST /api/admin/stations/{sid} and PUT
// (the UI's edit form). Every member is optional. owner is an account ID
// (number) or a user name (string); 0 / "" removes the owner. trust is the
// new absolute value, trustDelta a change.
type adminStationUpdate struct {
	Admission  *string         `json:"admission"`
	OwnerID    *int64          `json:"ownerId"`
	Owner      json.RawMessage `json:"owner"`
	Name       *string         `json:"name"`
	Trust      *int            `json:"trust"`
	TrustDelta *int            `json:"trustDelta"`
	Anchor     *bool           `json:"anchor"`
	// NeverAnchor bars the station from being an MLAT time anchor (the
	// clock filter's admin flag, DESIGN §9.2.5 condition (c)).
	NeverAnchor *bool `json:"neverAnchor"`
}

func apiAdminStationUpdate(s *Server, r *http.Request) (any, error) {
	var u adminStationUpdate
	if err := decodeBody(r, &u, 4<<10); err != nil {
		return nil, err
	}
	return s.applyAdminUpdate(r, u)
}

// apiAdminAdmission is POST .../admission {admission}.
func apiAdminAdmission(s *Server, r *http.Request) (any, error) {
	var b struct {
		Admission *string `json:"admission"`
	}
	if err := decodeBody(r, &b, 1<<10); err != nil {
		return nil, err
	}
	if b.Admission == nil {
		return nil, badRequest("admission is required")
	}
	return s.applyAdminUpdate(r, adminStationUpdate{Admission: b.Admission})
}

func (s *Server) applyAdminUpdate(r *http.Request, u adminStationUpdate) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	ctx, actor := r.Context(), s.actorOf(r)
	if u.Admission != nil {
		switch *u.Admission {
		case model.AdmApproved, model.AdmPending, model.AdmDenied, model.AdmRevoked:
		default:
			return nil, badRequest("admission: approved | pending | denied | revoked")
		}
		if st, err = reg.SetAdmission(ctx, actor, st.Idx, *u.Admission); err != nil {
			return nil, regErr(err)
		}
	}
	owner, hasOwner := int64(0), false
	if u.OwnerID != nil {
		owner, hasOwner = *u.OwnerID, true
	}
	if len(u.Owner) > 0 && string(u.Owner) != "null" {
		hasOwner = true
		var name string
		if err := json.Unmarshal(u.Owner, &owner); err != nil {
			if err := json.Unmarshal(u.Owner, &name); err != nil {
				return nil, badRequest("owner: an account ID or a user name")
			}
			owner = 0
			if name = strings.TrimSpace(name); name != "" {
				a, err := reg.Accounts().ByUsername(ctx, name)
				if err != nil {
					return nil, notFound("no account %q", name)
				}
				owner = a.ID
			}
		}
	}
	if hasOwner {
		if st, err = reg.AssignOwner(ctx, actor, st.Idx, owner); err != nil {
			return nil, regErr(err)
		}
	}
	if u.Name != nil {
		if len([]rune(strings.TrimSpace(*u.Name))) > 64 {
			return nil, badRequest("name: at most 64 characters")
		}
		if st, err = reg.SetName(ctx, st.Idx, *u.Name); err != nil {
			return nil, regErr(err)
		}
	}
	delta := 0
	if u.Trust != nil {
		delta = *u.Trust - st.Trust
	}
	if u.TrustDelta != nil {
		delta += *u.TrustDelta
	}
	if delta != 0 {
		if st, err = reg.AddTrust(ctx, st.Idx, delta); err != nil {
			return nil, regErr(err)
		}
	}
	if u.Anchor != nil {
		if st, err = reg.SetAnchor(ctx, st.Idx, *u.Anchor); err != nil {
			return nil, regErr(err)
		}
	}
	if u.NeverAnchor != nil {
		if st, err = reg.SetNoAnchor(ctx, st.Idx, *u.NeverAnchor); err != nil {
			return nil, regErr(err)
		}
	}
	s.cache.invalidate()
	return s.stationDTO(ctx, &st, true, time.Now()), nil
}

func apiAdminStationDelete(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	var b struct {
		Reason string `json:"reason"`
	}
	if raw, _ := readBody(r, 4<<10); len(strings.TrimSpace(string(raw))) > 0 {
		if err := decodeBodyBytes(raw, &b); err != nil {
			return nil, err
		}
	}
	if err := reg.DeleteStation(r.Context(), s.actorOf(r), st.Idx, b.Reason); err != nil {
		return nil, regErr(err)
	}
	s.cache.invalidate()
	return map[string]bool{"ok": true}, nil
}

func apiAdminRevokeFeeder(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	if err := reg.RevokeFeeder(r.Context(), s.actorOf(r), r.PathValue("id")); err != nil {
		return nil, regErr(err)
	}
	s.cache.invalidate()
	return map[string]bool{"ok": true}, nil
}

// tokenDTO is an enrol token without its secret.
type tokenDTO struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	MaxUses   int    `json:"maxUses"`
	Uses      int    `json:"uses"`
	Created   int64  `json:"created"`
	Expires   int64  `json:"expires"`
	Revoked   bool   `json:"revoked"`
	Usable    bool   `json:"usable"`
	CreatedBy int64  `json:"createdBy"`
	LastUsed  int64  `json:"lastUsed"`
}

func toTokenDTO(t registry.EnrolToken, now time.Time) tokenDTO {
	return tokenDTO{ID: t.ID, Label: t.Note, MaxUses: t.MaxUses, Uses: t.Uses, Created: unixMs(t.Created),
		Expires: unixMs(t.Expires), Revoked: t.Revoked, Usable: t.Usable(now), CreatedBy: t.CreatedBy, LastUsed: unixMs(t.LastUsed)}
}

func apiAdminTokens(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	ts, err := reg.Tokens(r.Context())
	if err != nil {
		return nil, regErr(err)
	}
	now := time.Now()
	out := []tokenDTO{}
	for _, t := range ts {
		out = append(out, toTokenDTO(t, now))
	}
	return map[string]any{"tokens": out}, nil
}

// mintReply is POST /api/admin/tokens: the token with its secret (shown
// once, never stored in clear).
type mintReply struct {
	tokenDTO
	Token string `json:"token"`
}

func apiAdminMintToken(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	var b struct {
		Label    string  `json:"label"`
		Note     string  `json:"note"`
		MaxUses  int     `json:"maxUses"`
		Days     float64 `json:"days"`
		TTLHours float64 `json:"ttlHours"`
	}
	if raw, _ := readBody(r, 4<<10); len(strings.TrimSpace(string(raw))) > 0 {
		if err := decodeBodyBytes(raw, &b); err != nil {
			return nil, err
		}
	}
	if b.Label == "" {
		b.Label = b.Note
	}
	ttl := b.TTLHours
	if b.Days > 0 {
		ttl = b.Days * 24
	}
	if b.MaxUses < 0 || b.MaxUses > 100000 || ttl < 0 || ttl > 24*365 || len(b.Label) > 200 {
		return nil, badRequest("maxUses 0..100000 (0 = default), days 0..365 (0 = default), label up to 200 characters")
	}
	secret, tok, err := reg.MintToken(r.Context(), s.actorOf(r), b.Label, b.MaxUses, time.Duration(ttl*float64(time.Hour)))
	if err != nil {
		return nil, regErr(err)
	}
	return mintReply{tokenDTO: toTokenDTO(tok, time.Now()), Token: secret}, nil
}

func apiAdminRevokeToken(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	if err := reg.RevokeToken(r.Context(), s.actorOf(r), r.PathValue("id")); err != nil {
		return nil, regErr(err)
	}
	return map[string]bool{"ok": true}, nil
}

type auditDTO struct {
	ID      int64  `json:"id"`
	T       int64  `json:"t"`
	ActorID int64  `json:"actorId"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Target  string `json:"target,omitempty"` // station SID
	Detail  string `json:"detail,omitempty"`
}

func apiAdminAudit(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 200, 1, 1000)
	if err != nil {
		return nil, err
	}
	before, err := qInt64(r, "before", 0)
	if err != nil {
		return nil, err
	}
	es, err := reg.AuditLog(r.Context(), registry.AuditFilter{SID: qStr(r, "sid"), ActionPrefix: qStr(r, "action"), Before: before, Limit: limit})
	if err != nil {
		return nil, regErr(err)
	}
	out := []auditDTO{}
	names := map[int64]string{} // UX-08: entries written with an actor ID only (station.claim) get its name
	for _, e := range es {
		actor := e.Actor
		if actor == "" && e.ActorID != 0 {
			n, ok := names[e.ActorID]
			if !ok {
				if a, err := reg.Accounts().ByID(r.Context(), e.ActorID); err == nil {
					n = a.Username
				}
				names[e.ActorID] = n
			}
			actor = n
		}
		out = append(out, auditDTO{ID: e.ID, T: unixMs(e.T), ActorID: e.ActorID, Actor: actor, Action: e.Action, Target: e.SID, Detail: e.Detail})
	}
	return map[string]any{"entries": out}, nil
}

func apiAdminRetention(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	rep, err := reg.RunRetention(r.Context())
	if err != nil {
		return nil, regErr(err)
	}
	ran := rep.ConnLog + rep.Audit + rep.Claims + rep.Tokens + rep.Sessions + rep.Estimates + rep.Salts + int64(rep.Purged+rep.Deleted)
	for _, n := range rep.Stats {
		ran += n
	}
	return map[string]any{
		"ran": ran, "connLog": rep.ConnLog, "audit": rep.Audit, "claims": rep.Claims, "tokens": rep.Tokens, "sessions": rep.Sessions,
		"estimates": rep.Estimates, "salts": rep.Salts, "stats": rep.Stats, "purged": rep.Purged, "deleted": rep.Deleted,
		"tookMs": rep.Took.Milliseconds(),
	}, nil
}

func apiAdminLinks(s *Server, r *http.Request) (any, error) {
	if reg := s.registry(); reg != nil {
		if err := reg.Audit(r.Context(), s.actorOf(r), "links.view", "", ""); err != nil {
			return nil, regErr(err)
		}
	}
	return map[string]any{"links": s.adminLinks(nil)}, nil
}

type beastAdminReply struct {
	Stats beastin.Stats       `json:"stats"`
	Links []beastin.LinkStats `json:"links"`
}

func apiAdminBeast(s *Server, r *http.Request) (any, error) {
	b := s.network().Beast
	if b == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "the Beast listener is not running")
	}
	if reg := s.registry(); reg != nil {
		if err := reg.Audit(r.Context(), s.actorOf(r), "links.view", "", `{"listener":"beast"}`); err != nil {
			return nil, regErr(err)
		}
	}
	links := b.Links()
	if links == nil {
		links = []beastin.LinkStats{}
	}
	return beastAdminReply{Stats: b.Stats(), Links: links}, nil
}

func apiAdminBeastDisconnect(s *Server, r *http.Request) (any, error) {
	b := s.network().Beast
	if b == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "the Beast listener is not running")
	}
	id, err := strconv.ParseUint(r.PathValue("link"), 10, 64)
	if err != nil {
		return nil, badRequest("invalid link id")
	}
	if !b.Disconnect(id) {
		return nil, notFound("no such link")
	}
	return map[string]bool{"ok": true}, nil
}

type adminAircraftReply struct {
	Aircraft  *model.Aircraft `json:"aircraft"`
	Receivers []receiverRef   `json:"receivers"`
}

type receiverRef struct {
	SID  string `json:"sid"`
	Name string `json:"name,omitempty"`
}

func apiAdminAircraft(s *Server, r *http.Request) (any, error) {
	key, err := parseICAOParam(r.PathValue("icao"))
	if err != nil {
		return nil, err
	}
	tr := s.eng.Tracker()
	out := adminAircraftReply{Receivers: []receiverRef{}}
	if a, ok := tr.AircraftView(key, tracker.View{Scope: model.ScopeAdmin, Station: model.HostStation}); ok {
		out.Aircraft = &a
	}
	reg := s.registry()
	for _, idx := range tr.HeardBy(key) {
		ref := receiverRef{}
		if reg != nil {
			if st, ok := reg.Station(idx); ok {
				ref.SID, ref.Name = st.SID, st.Name
			}
		}
		if ref.SID == "" {
			ref.SID = "#" + strconv.FormatUint(uint64(idx), 10)
		}
		out.Receivers = append(out.Receivers, ref)
	}
	return out, nil
}

// apiAdminNetwork is the engine's operator view of the aggregator
// (listeners, ingest, tracker lock statistics, lanes).
func apiAdminNetwork(s *Server, r *http.Request) (any, error) {
	return s.eng.NetworkStatus(), nil
}

// rawUplinks mounts the uplink manager's handler (GET status, POST
// actions). Its replies carry no secrets except a claim code, which the
// manager marks Cache-Control: no-store. Standalone: reading the status is
// open like every GET; the POST is admin-only through Route.Admin.
func rawUplinks(s *Server, w http.ResponseWriter, r *http.Request) {
	h := s.network().Uplinks
	if h == nil {
		writeError(w, http.StatusServiceUnavailable, "the uplink manager is not running")
		return
	}
	h.ServeHTTP(w, r)
}
