package server

import (
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
	"airview/internal/privacy"
	"airview/internal/privacy/publicdto"
	"airview/internal/registry"
	"airview/internal/tracker"
)

// Share links (WP-H2, DESIGN Â§8.2 "My map": session only, share links in
// phase 4; Â§14 phase 4 "revocable and scoped, with a location warning").
//
// An owner creates a link for one or more own stations. Anyone holding the
// link sees, read-only:
//
//   - scope "map": the aircraft those stations heard in the last 60 s, in
//     the public aircraft shape (publicdto: no receiver list, no distance,
//     bearing, signal or station data);
//   - scope "map+stats": additionally the stations' non-locating
//     statistics (message and position rate, aircraft, uptime), named as
//     on the public list (the station name only when its owner publishes
//     it, else "Station 1", "Station 2", ...).
//
// Never shared: station positions or pins, SIDs, receiver lists, range,
// coverage, polar or heat data, IPs. Low aircraft are rounded to the
// island-guard grid unless the owner switches that off (coarseLow), and
// the public island guard always applies on top.
//
// Location warning: the aircraft a station hears outline where it is. The
// owner must acknowledge that (`acknowledge: true`) before a link is made;
// the text is shareLocationWarning.
//
// Lifetime: 1..365 days (default 30), revocable by the owner and the
// operator at any time; a link also stops working for a station that is
// no longer owned by the link's account (unclaim, transfer, deletion) and
// when the account is deleted. The secret is shown once; only its SHA-256
// is stored (sharestore.go).

const shareLocationWarning = "Anyone who has this link sees the aircraft your station receives, live. " +
	"Where those aircraft are, and especially the low ones, shows roughly where your antenna is. " +
	"Share it only with people you would tell your location to. You can revoke the link at any time."

func init() {
	routes = append(routes, shareRoutes...)
}

var pShareToken = Param{"token", "the secret of a share link (path; 32 characters)"}

var shareRoutes = []Route{
	{Method: "GET", Path: "/api/me/shares", Group: grpMe, Access: AccessOwner, NetOnly: true, API: apiMeShares, NoAuto: true,
		Summary: "The account's share links (no secrets): label, stations, scope, state, expiry, last use and use count, plus the location warning.",
		Returns: "`{\"links\":[ShareLink],\"max\":n,\"warning\":\"...\"}`"},
	{Method: "POST", Path: "/api/me/shares", Group: grpMe, Access: AccessOwner, NetOnly: true, API: apiMeShareCreate, NoAuto: true,
		Summary: "Create a share link for own stations (read-only live view). The secret is returned once.",
		Body:    "`{\"stations\":[sid],\"label\":\"...\",\"scope\":\"map|map+stats\",\"days\":1..365,\"coarseLow\":true,\"acknowledge\":true}` (`acknowledge` confirms the location warning; required)",
		Returns: "`{\"link\":ShareLink,\"token\":\"...\",\"path\":\"/#/share/<token>\"}`; `400` with the warning without `acknowledge`, `429` over the limit of 20 active links",
		Notes:   "Audited (`share.create`)."},
	{Method: "DELETE", Path: "/api/me/shares/{id}", Group: grpMe, Access: AccessOwner, NetOnly: true, API: apiMeShareRevoke, NoAuto: true,
		Summary: "Revoke an own share link (it stops working at once).",
		Notes:   "Audited (`share.revoke`)."},
	{Method: "GET", Path: "/api/admin/shares", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true, API: apiAdminShares, NoAuto: true,
		Params:  []Param{{"q", "search in label, owner and station SIDs"}, {"state", "`active`, `revoked` or `expired`"}, {"offset, limit", "paging (limit 1..500, default 100)"}},
		Summary: "Every share link of the network (no secrets) with its owner, stations and use.",
		Returns: "`{\"links\":[ShareLink],\"total\":n,\"counts\":{\"active\":n,\"revoked\":n,\"expired\":n}}`"},
	{Method: "DELETE", Path: "/api/admin/shares/{id}", Group: grpNetAdm, Access: AccessAdmin, NetOnly: true, Admin: true, API: apiAdminShareRevoke, NoAuto: true,
		Summary: "Revoke any share link.",
		Notes:   "Audited (`share.revoke`)."},
	{Method: "GET", Path: "/api/share/{token}", Group: grpPublic, Access: AccessPublic, NetOnly: true, API: apiShareMeta, NoAuto: true,
		Params:  []Param{pShareToken},
		Summary: "What a share link shows: label, scope, number of stations, expiry and the network name (no station data).",
		Returns: "`{\"label\",\"scope\",\"stationCount\":n,\"expires\":ms,\"coarseLow\":bool,\"network\":\"...\"}`; `404` for an unknown, revoked or expired link"},
	{Method: "GET", Path: "/api/share/{token}/aircraft", Group: grpPublic, Access: AccessPublic, NetOnly: true, API: apiShareAircraft, NoAuto: true,
		Params:  []Param{pShareToken},
		Summary: "The aircraft the link's stations heard in the last 60 s (`publicdto.Snapshot`: no receiver data; low aircraft on the island-guard grid when the link says so).",
		Returns: "`publicdto.Snapshot`; `404` for an unknown, revoked or expired link"},
	{Method: "GET", Path: "/api/share/{token}/stats", Group: grpPublic, Access: AccessPublic, NetOnly: true, API: apiShareStats, NoAuto: true,
		Params:  []Param{pShareToken},
		Summary: "Non-locating statistics of the link's stations (scope `map+stats` only).",
		Returns: "`{\"entries\":[{\"name\",\"online\",\"stats\":StationStats}]}` (named `entries`: a public payload never has a `stations` member); `404` when the link has another scope"},
}

// shareStore returns the store (created on first use in the data dir).
func (s *Server) shareStore() *shareStore {
	s.sharesOnce.Do(func() {
		path := ""
		if s.eng != nil {
			if dir := s.eng.DataDir(); dir != "" {
				path = filepath.Join(dir, shareFile)
			}
		}
		s.shares = newShareStore(path)
	})
	return s.shares
}

// shareLinkDTO is a link as its owner and the admin see it (no secret).
type shareLinkDTO struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	Scope     string            `json:"scope"`
	CoarseLow bool              `json:"coarseLow"`
	Stations  []shareStationRef `json:"stations"`
	Created   int64             `json:"created"`
	Expires   int64             `json:"expires"`
	Revoked   int64             `json:"revoked,omitempty"`
	RevokedBy string            `json:"revokedBy,omitempty"`
	LastUsed  int64             `json:"lastUsed,omitempty"`
	Uses      uint64            `json:"uses"`
	State     string            `json:"state"` // active | revoked | expired
	OwnerID   int64             `json:"ownerId,omitempty"`
	Owner     string            `json:"owner,omitempty"` // admin list only
}

// shareStationRef names a station of a link for its owner / the admin.
// Owned is false when the station no longer belongs to the link's account
// (the link then skips it).
type shareStationRef struct {
	SID   string `json:"sid"`
	Name  string `json:"name"`
	Owned bool   `json:"owned"`
}

func (s *Server) shareDTO(reg *registry.Registry, l *shareLink, now time.Time) shareLinkDTO {
	d := shareLinkDTO{ID: l.ID, Label: l.Label, Scope: l.Scope, CoarseLow: l.CoarseLow, Created: l.Created, Expires: l.Expires,
		Revoked: l.Revoked, RevokedBy: l.RevokedBy, LastUsed: l.LastUsed, Uses: l.Uses, State: l.state(now), Stations: []shareStationRef{}}
	for _, sid := range l.SIDs {
		ref := shareStationRef{SID: sid}
		if reg != nil {
			if st, ok := reg.BySID(sid); ok {
				ref.Name = st.Name
				ref.Owned = st.OwnerID == l.OwnerID
			}
		}
		d.Stations = append(d.Stations, ref)
	}
	return d
}

func apiMeShares(s *Server, r *http.Request) (any, error) {
	if err := s.shareGate(); err != nil {
		return nil, err
	}
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	owner := s.viewerOf(r).OwnerID
	ls, err := s.shareStore().list(func(l *shareLink) bool { return l.OwnerID == owner })
	if err != nil {
		return nil, errStatus(http.StatusServiceUnavailable, "share links unavailable: %v", err)
	}
	now := time.Now()
	out := make([]shareLinkDTO, 0, len(ls))
	for i := range ls {
		out = append(out, s.shareDTO(reg, &ls[i], now))
	}
	lim := s.shareCfg()
	return map[string]any{"links": out, "max": lim.MaxPerOwner, "maxDays": lim.MaxDays, "defaultDays": lim.DefaultDays, "coarseLowDefault": lim.CoarseLow,
		"warning": shareLocationWarning}, nil
}

type shareCreateBody struct {
	Stations    []string `json:"stations"`
	Label       string   `json:"label"`
	Scope       string   `json:"scope"`
	Days        int      `json:"days"`
	CoarseLow   *bool    `json:"coarseLow"`
	Acknowledge bool     `json:"acknowledge"`
}

func apiMeShareCreate(s *Server, r *http.Request) (any, error) {
	if err := s.shareGate(); err != nil {
		return nil, err
	}
	lim := s.shareCfg()
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	var b shareCreateBody
	if err := decodeBody(r, &b, 4<<10); err != nil {
		return nil, err
	}
	if !b.Acknowledge {
		return nil, badRequest("please confirm the location warning (acknowledge: true). %s", shareLocationWarning)
	}
	switch b.Scope {
	case "":
		b.Scope = shareScopeMap
	case shareScopeMap, shareScopeMapStats:
	default:
		return nil, badRequest("scope: map | map+stats")
	}
	if b.Days == 0 {
		b.Days = lim.DefaultDays
	}
	if b.Days < 1 || b.Days > lim.MaxDays {
		return nil, badRequest("days: 1..%d", lim.MaxDays)
	}
	b.Label = strings.TrimSpace(b.Label)
	if len([]rune(b.Label)) > 60 || strings.ContainsFunc(b.Label, func(c rune) bool { return c < ' ' }) {
		return nil, badRequest("label: at most 60 printable characters")
	}
	owner := s.viewerOf(r).OwnerID
	if owner == 0 {
		return nil, errStatus(http.StatusForbidden, "share links need an owner account")
	}
	var sids []string
	for _, sid := range b.Stations {
		sid = strings.TrimSpace(sid)
		if sid == "" || slices.Contains(sids, sid) {
			continue
		}
		// Unknown and foreign stations answer alike (no probing).
		if !reg.Owns(owner, sid) {
			return nil, notFound("station %q is not one of your stations", sid)
		}
		sids = append(sids, sid)
	}
	if len(sids) == 0 {
		return nil, badRequest("stations: at least one of your stations")
	}
	if len(sids) > shareMaxStations {
		return nil, badRequest("stations: at most %d", shareMaxStations)
	}
	coarse := lim.CoarseLow
	if b.CoarseLow != nil {
		coarse = *b.CoarseLow
	}
	now := time.Now()
	l, secret, err := s.shareStore().create(shareLink{OwnerID: owner, SIDs: sids, Label: b.Label, Scope: b.Scope, CoarseLow: coarse,
		Expires: now.Add(time.Duration(b.Days) * 24 * time.Hour).UnixMilli()}, lim.MaxPerOwner)
	switch {
	case errors.Is(err, errShareLimit):
		return nil, errStatus(http.StatusTooManyRequests, "at most %d active share links per account: revoke one first", lim.MaxPerOwner)
	case err != nil:
		return nil, errStatus(http.StatusServiceUnavailable, "share links unavailable: %v", err)
	}
	_ = reg.Audit(r.Context(), s.actorOf(r), "share.create", sids[0],
		`{"id":"`+l.ID+`","stations":`+strconv.Itoa(len(sids))+`,"scope":"`+jsonSafe(strings.ReplaceAll(l.Scope, "+", "_"))+`","days":`+strconv.Itoa(b.Days)+`}`)
	return map[string]any{"link": s.shareDTO(reg, l, now), "token": secret, "path": "/#/share/" + secret, "warning": shareLocationWarning}, nil
}

func (s *Server) shareRevoke(r *http.Request, owner int64) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	actor := s.actorOf(r)
	l, err := s.shareStore().revoke(r.PathValue("id"), owner, actor.Name)
	switch {
	case errors.Is(err, errShareNotFound):
		return nil, notFound("no such share link")
	case err != nil:
		return nil, errStatus(http.StatusServiceUnavailable, "share links unavailable: %v", err)
	}
	sid := ""
	if len(l.SIDs) > 0 {
		sid = l.SIDs[0]
	}
	_ = reg.Audit(r.Context(), actor, "share.revoke", sid, `{"id":"`+l.ID+`"}`)
	return map[string]any{"ok": true, "link": s.shareDTO(reg, &l, time.Now())}, nil
}

func apiMeShareRevoke(s *Server, r *http.Request) (any, error) {
	if err := s.shareGate(); err != nil {
		return nil, err
	}
	owner := s.viewerOf(r).OwnerID
	if owner == 0 {
		return nil, notFound("no such share link")
	}
	return s.shareRevoke(r, owner)
}

func apiAdminShareRevoke(s *Server, r *http.Request) (any, error) { return s.shareRevoke(r, 0) }

func apiAdminShares(s *Server, r *http.Request) (any, error) {
	reg, err := s.reg()
	if err != nil {
		return nil, err
	}
	limit, err := qInt(r, "limit", 100, 1, 500)
	if err != nil {
		return nil, err
	}
	offset, err := qInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		return nil, err
	}
	state := qStr(r, "state")
	switch state {
	case "", "active", "revoked", "expired":
	default:
		return nil, badRequest("state: active | revoked | expired")
	}
	q := strings.ToLower(strings.TrimSpace(qStr(r, "q")))
	ls, err := s.shareStore().list(nil)
	if err != nil {
		return nil, errStatus(http.StatusServiceUnavailable, "share links unavailable: %v", err)
	}
	now := time.Now()
	names := map[int64]string{}
	ownerName := func(id int64) string {
		if n, ok := names[id]; ok {
			return n
		}
		n := ""
		if a, err := reg.Accounts().ByID(r.Context(), id); err == nil {
			n = a.Username
		}
		names[id] = n
		return n
	}
	out := []shareLinkDTO{}
	counts := map[string]int{"active": 0, "revoked": 0, "expired": 0}
	total := 0
	for i := range ls {
		l := &ls[i]
		st := l.state(now)
		counts[st]++
		if state != "" && st != state {
			continue
		}
		if q != "" {
			hay := strings.ToLower(l.Label + " " + ownerName(l.OwnerID) + " " + strings.Join(l.SIDs, " ") + " " + l.ID)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		total++
		if total <= offset || len(out) >= limit {
			continue
		}
		d := s.shareDTO(reg, l, now)
		d.OwnerID, d.Owner = l.OwnerID, ownerName(l.OwnerID)
		out = append(out, d)
	}
	return map[string]any{"links": out, "total": total, "counts": counts}, nil
}

// ------------------------------------------------------------ public side

// shareResolve finds the active link of the request's token and the
// stations it may still show (owned by the link's account, not deleted).
// Every failure is the same 404.
func (s *Server) shareResolve(r *http.Request) (shareLink, []registry.Station, error) {
	gone := notFound("this share link does not exist, has expired or was revoked")
	reg := s.registry()
	if reg == nil || s.shareCfg().Disabled {
		return shareLink{}, nil, gone
	}
	l, ok := s.shareStore().resolve(r.PathValue("token"))
	if !ok {
		return shareLink{}, nil, gone
	}
	var sts []registry.Station
	for _, sid := range l.SIDs {
		if st, ok := reg.BySID(sid); ok && st.OwnerID == l.OwnerID && !st.Deleting {
			sts = append(sts, st)
		}
	}
	if len(sts) == 0 {
		return shareLink{}, nil, gone
	}
	return l, sts, nil
}

// shareMetaDTO is GET /api/share/{token}: no station data at all.
type shareMetaDTO struct {
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	Stations  int    `json:"stationCount"`
	Expires   int64  `json:"expires"`
	CoarseLow bool   `json:"coarseLow"`
	Network   string `json:"network,omitempty"`
}

func apiShareMeta(s *Server, r *http.Request) (any, error) {
	l, sts, err := s.shareResolve(r)
	if err != nil {
		return nil, err
	}
	return shareMetaDTO{Label: l.Label, Scope: l.Scope, Stations: len(sts), Expires: l.Expires, CoarseLow: l.CoarseLow,
		Network: s.eng.EffectiveConfig().Aggregator.Name}, nil
}

func apiShareAircraft(s *Server, r *http.Request) (any, error) {
	l, sts, err := s.shareResolve(r)
	if err != nil {
		return nil, err
	}
	snap := s.shareSnapshot(&l, sts)
	return &snap, nil
}

// shareSnapshot is the shared picture: the aircraft the stations heard in
// the last 60 s (the tracker's owner projection, `Mine`), converted to the
// public shape. Without network mode the tracker cannot attribute
// aircraft to stations and the picture is empty (never the whole map).
func (s *Server) shareSnapshot(l *shareLink, sts []registry.Station) publicdto.Snapshot {
	tr := s.eng.Tracker()
	own := make([]model.StationIdx, 0, len(sts))
	for _, st := range sts {
		own = append(own, st.Idx)
	}
	raw := tr.SnapshotView(tracker.View{Scope: model.ScopeOwner, Own: own})
	sub := model.LiveSnapshot{Now: raw.Now}
	if tr.Network() {
		for i := range raw.Aircraft {
			a := &raw.Aircraft[i]
			if !a.Mine {
				continue
			}
			sub.Aircraft = append(sub.Aircraft, *a)
			c := &sub.Counts
			c.Aircraft++
			if a.Lat != nil && a.Lon != nil {
				c.WithPos++
			}
			switch {
			case a.Source == model.SrcADSB:
				c.ADSB++
			case strings.HasPrefix(string(a.Source), model.SrcMLAT):
				c.MLAT++
			}
		}
	}
	return publicdto.FromSnapshot(&sub, s.shareGuard(l, own), privacy.IslandGridDeg)
}

// shareGuard combines the public island guard with the link's own
// low-aircraft rounding (coarseLow).
func (s *Server) shareGuard(l *shareLink, own []model.StationIdx) publicdto.Guard {
	pub := s.islandGuard()
	if !l.CoarseLow {
		return pub
	}
	_ = own // the ground estimate never uses station heights (PRIV-A5)
	return func(a *model.Aircraft) bool {
		if pub != nil && pub(a) {
			return true
		}
		if a.Lat == nil || a.Lon == nil {
			return false
		}
		if !privacy.IslandLowAGL(a.OnGround, a.AltBaro, a.AltGeom, privacy.IslandMaxGroundFt) {
			return false
		}
		return privacy.IslandLowAGL(a.OnGround, a.AltBaro, a.AltGeom, s.islandGroundFt(*a.Lat, *a.Lon, true))
	}
}

// shareStationDTO is one station of GET /api/share/{token}/stats.
type shareStationDTO struct {
	Name   string                  `json:"name"`
	Online bool                    `json:"online"`
	Stats  *publicdto.StationStats `json:"stats"`
}

func apiShareStats(s *Server, r *http.Request) (any, error) {
	l, sts, err := s.shareResolve(r)
	if err != nil {
		return nil, err
	}
	if l.Scope != shareScopeMapStats {
		return nil, notFound("this share link has no statistics")
	}
	now := time.Now()
	out := make([]shareStationDTO, 0, len(sts))
	for i := range sts {
		st := &sts[i]
		name := "Station " + strconv.Itoa(i+1)
		if st.ShowName && st.Name != "" {
			name = st.Name
		}
		out = append(out, shareStationDTO{Name: name, Online: s.stationOnline(st, now), Stats: s.stationStats(st, now)})
	}
	return map[string]any{"entries": out, "now": unixMs(now)}, nil
}
