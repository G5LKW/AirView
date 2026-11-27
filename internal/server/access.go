package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	avauth "airview/internal/auth"
	"airview/internal/model"
	"airview/internal/registry"
)

// Route scopes (docs/aggregator/DESIGN.md §8.2, §8.4).
//
// Every route declares an Access class and a station selector in the route
// table. TestRouteAccessLint fails when one is missing or inconsistent with
// the path (/api/public/* must be public, /api/me/* owner or IP-matched,
// /api/admin/* admin, {sid} routes must use StationPath). Deny is the
// default: an undeclared route answers 403 in aggregator mode.
//
// Standalone mode ignores Access: every client sees all data there and
// only mutations are restricted (Route.Admin), exactly as before.

// Access is the visibility class of a route in aggregator mode.
type Access uint8

const (
	accessUnset Access = iota
	// AccessPublic: the answer holds no station data at all and is the same
	// for every viewer (reference data, the public projections).
	AccessPublic
	// AccessViewer: open to everyone; the handler projects the answer per
	// viewer (s.viewerOf(r)): public viewers get the publicdto shapes.
	AccessViewer
	// AccessOwner: needs an owner (or admin) session.
	AccessOwner
	// AccessAdmin: needs an admin session with a second factor. The legacy
	// single-station routes (statistics, antenna, coverage, timing, ...)
	// are admin-only in aggregator mode: their data is the host station's.
	AccessAdmin
	// AccessSession: the handler checks the session itself (the account
	// routes of auth_routes.go).
	AccessSession
)

func (a Access) String() string {
	switch a {
	case AccessPublic:
		return "public"
	case AccessViewer:
		return "viewer"
	case AccessOwner:
		return "owner"
	case AccessAdmin:
		return "admin"
	case AccessSession:
		return "session"
	}
	return "unset"
}

// StationSel says how a route selects a station.
type StationSel uint8

const (
	// StationNone: the route does not select a station.
	StationNone StationSel = iota
	// StationPath: the station is the {sid} path value; the viewer must own
	// it (or be admin), else 404.
	StationPath
	// StationQuery: an optional ?station=<sid>; when given, the viewer must
	// own it (or be admin), else 404.
	StationQuery
)

// routeAccess returns the effective access class of rt (every route
// declares it; the account routes, which check their session themselves,
// declare AccessSession).
func routeAccess(rt *Route) Access {
	return rt.Access
}

type ctxKey int

const (
	ctxViewerKey ctxKey = iota
	ctxStationKey
)

// viewerOf returns the request's viewer: the one resolved by routeHandler
// (stored in the context, with the selected station) or a fresh one.
func (s *Server) viewerOf(r *http.Request) model.Viewer {
	if v, ok := r.Context().Value(ctxViewerKey).(model.Viewer); ok {
		return v
	}
	return s.viewer(r)
}

// selectedStation returns the station selected by the route's StationSel
// (ok false when none was selected).
func selectedStation(r *http.Request) (registry.Station, bool) {
	st, ok := r.Context().Value(ctxStationKey).(registry.Station)
	return st, ok
}

// gate applies the route's access class in aggregator mode. It returns the
// request to continue with (carrying the viewer and the selected station in
// its context) or false after writing the refusal.
func (s *Server) gate(w http.ResponseWriter, r *http.Request, rt *Route) (*http.Request, bool) {
	if !s.aggregatorMode() {
		if rt.NetOnly {
			writeError(w, http.StatusNotFound, "only available in aggregator mode")
			return r, false
		}
		return r, true
	}
	acc := routeAccess(rt)
	v := s.viewer(r)
	switch acc {
	case AccessPublic, AccessViewer, AccessSession:
	case AccessOwner:
		if !v.Scope.Allows(model.ScopeOwner) {
			writeError(w, http.StatusUnauthorized, "login required")
			return r, false
		}
	case AccessAdmin:
		if !v.Scope.Allows(model.ScopeAdmin) {
			if v.Scope == model.ScopePublic {
				writeError(w, http.StatusUnauthorized, "login required")
			} else {
				writeError(w, http.StatusForbidden, "not allowed for this account")
			}
			return r, false
		}
	default:
		writeError(w, http.StatusForbidden, "not available")
		return r, false
	}
	// Rate limit for the public scope (DESIGN §8.4), keyed per IPv4
	// address or IPv6 /64. The health endpoint and the raw routes (live
	// stream: one stream per address; data files) are exempt, except the
	// credential routes (login, sign-up, setup): their PBKDF2 work must not
	// be floodable (SEC-06).
	// Per-account limit for owner routes that must not be crawled (the
	// MLAT peer view: finding PRIV-1).
	if rt.AccountLimited && v.Scope == model.ScopeOwner && v.OwnerID != 0 {
		if wait, ok := s.acctLimiter.allow("acct:"+strconv.FormatInt(v.OwnerID, 10)+"|"+rt.Path, acctRate); !ok {
			w.Header().Set("Retry-After", retryAfter(wait))
			writeError(w, http.StatusTooManyRequests, "too many requests; slow down")
			return r, false
		}
	}
	if v.Scope == model.ScopePublic && rt.Path != healthPath && (rt.Raw == nil || rt.Credentials) {
		if wait, ok := s.limiter.allow(avauth.ClientKeyString(s.remoteIP(r)), s.publicRate()); !ok {
			w.Header().Set("Retry-After", retryAfter(wait))
			writeError(w, http.StatusTooManyRequests, "too many requests; slow down")
			return r, false
		}
	}
	ctx := r.Context()
	if rt.Station != StationNone {
		sid := ""
		switch rt.Station {
		case StationPath:
			sid = r.PathValue("sid")
		case StationQuery:
			sid = strings.TrimSpace(r.URL.Query().Get("station"))
		}
		if sid != "" || rt.Station == StationPath {
			st, ok := s.resolveStation(v, sid)
			if !ok {
				writeError(w, http.StatusNotFound, "unknown station")
				return r, false
			}
			v.SID = st.SID
			ctx = context.WithValue(ctx, ctxStationKey, st)
		}
	}
	ctx = context.WithValue(ctx, ctxViewerKey, v)
	return r.WithContext(ctx), true
}

// resolveStation returns station sid when the viewer may see its exact
// data: an admin any station, an owner only their own. Unknown and foreign
// stations both fail, so an owner cannot probe which SIDs exist.
func (s *Server) resolveStation(v model.Viewer, sid string) (registry.Station, bool) {
	reg := s.registry()
	if reg == nil || sid == "" {
		return registry.Station{}, false
	}
	st, ok := reg.BySID(sid)
	if !ok {
		return registry.Station{}, false
	}
	switch {
	case v.Scope == model.ScopeAdmin:
		return st, true
	case v.Scope == model.ScopeOwner && v.OwnerID != 0 && st.OwnerID == v.OwnerID:
		return st, true
	}
	return registry.Station{}, false
}

// viewerCacheKey returns the response-cache key of r for route rt: in
// aggregator mode it includes the viewer (scope, owner, station), so an
// answer computed for one viewer is never served to another (DESIGN §8.4).
// Public routes share one key per URL and viewer class: anonymous viewers
// one, signed-in viewers another. The operator's public gates
// (publicStats, publicMap, ...) refuse only anonymous viewers, so an answer
// computed for a signed-in viewer must never be cached where an anonymous
// one finds it (finding PRIV-3).
func (s *Server) viewerCacheKey(r *http.Request, rt *Route) string {
	k := cacheKey(r)
	if k == "" || !s.aggregatorMode() {
		return k
	}
	if routeAccess(rt) == AccessPublic && rt.Station == StationNone && !strings.HasPrefix(rt.Path, "/api/me/") {
		if s.viewerOf(r).Scope == model.ScopePublic {
			return "pub|" + k
		}
		return "pub+|" + k
	}
	return s.viewerOf(r).CacheKey() + "|" + k
}
