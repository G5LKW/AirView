package navdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"airview/internal/geo"
)

// RouteOptions configure the optional online route lookup (adsbdb.com).
//
// adsbdb's README says its flight-route data (by David Taylor and Jim Mason)
// "may not be copied, published, or incorporated into other databases without
// the explicit permission of David J Taylor". AirView therefore never merges
// online results into navdata_route; it keeps them only in an expiring cache
// (navdata_route_cache) to avoid repeated requests, and the lookup is off by
// default because it sends callsigns to a third party.
type RouteOptions struct {
	Online      bool          `json:"online"`                // default false
	BaseURL     string        `json:"baseUrl,omitempty"`     // default "https://api.adsbdb.com/v0/callsign/"
	MinInterval time.Duration `json:"minInterval,omitempty"` // default 2 s between requests
	DailyLimit  int           `json:"dailyLimit,omitempty"`  // default 1000 requests per 24 h
	CacheTTL    time.Duration `json:"cacheTtl,omitempty"`    // default 24 h for found routes
	NegativeTTL time.Duration `json:"negativeTtl,omitempty"` // default 12 h for unknown callsigns
	MaxWait     time.Duration `json:"maxWait,omitempty"`     // longest pacing wait accepted per request (default 5 s)
	// Contact is appended to the User-Agent as " (+contact)" (a URL or
	// e-mail; the engine passes the project URL by default).
	Contact string `json:"contact,omitempty"`
}

// AdsbdbCallsignURL is the default online route endpoint (verified Nov 2026:
// GET returns {"response":{"flightroute":{...}}} or 404 {"response":"unknown callsign"}).
const AdsbdbCallsignURL = "https://api.adsbdb.com/v0/callsign/"

func (o RouteOptions) withDefaults() RouteOptions {
	if o.BaseURL == "" {
		o.BaseURL = AdsbdbCallsignURL
	}
	if o.MinInterval <= 0 {
		o.MinInterval = 2 * time.Second
	}
	if o.DailyLimit <= 0 {
		o.DailyLimit = 1000
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 24 * time.Hour
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 12 * time.Hour
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Second
	}
	return o
}

// Point is a position (for the plausibility check).
type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// RouteAirport is one airport of a route (resolved from the airports table;
// Resolved=false when the code is not in the imported data).
type RouteAirport struct {
	Code     string   `json:"code"`
	Resolved bool     `json:"resolved"`
	Airport  *Airport `json:"airport,omitempty"`
}

// RouteCheck is the plausibility of a route against the aircraft position.
type RouteCheck struct {
	Plausible    bool    `json:"plausible"`
	Doubtful     bool    `json:"doubtful"` // = !Plausible, for UI convenience
	Leg          int     `json:"leg"`      // 0-based index of the best fitting leg
	CrossTrackKm float64 `json:"crossTrackKm"`
	AlongPct     float64 `json:"alongPct"` // progress along that leg, 0..100 (may exceed slightly)
	ToleranceKm  float64 `json:"toleranceKm"`
	Reason       string  `json:"reason"`
}

// RouteInfo is the answer for one callsign.
type RouteInfo struct {
	Callsign   string         `json:"callsign"`
	Found      bool           `json:"found"`
	Source     string         `json:"source,omitempty"` // "standing-data" | "adsbdb"
	Airline    string         `json:"airline,omitempty"`
	Airports   []RouteAirport `json:"airports,omitempty"` // in flight order
	Path       [][2]float64   `json:"path,omitempty"`     // great-circle [lon,lat] points over all resolved legs (lon unwrapped)
	DistanceKm float64        `json:"distanceKm,omitempty"`
	Check      *RouteCheck    `json:"check,omitempty"` // when a position was given and >= 2 airports resolved
	Cached     bool           `json:"cached,omitempty"`
}

// Route returns the route of callsign: standing data first, then (when
// enabled) the online lookup. pos (optional) enables the plausibility check.
// A callsign without any route returns Found=false and no error.
func (n *NavData) Route(ctx context.Context, callsign string, pos *Point) (RouteInfo, error) {
	return n.route(ctx, callsign, pos, true)
}

// RouteNoFetch is Route without sending an online request: the standing
// data, then a cached online answer only (public viewers must not spend the
// station's online budget).
func (n *NavData) RouteNoFetch(ctx context.Context, callsign string, pos *Point) (RouteInfo, error) {
	return n.route(ctx, callsign, pos, false)
}

func (n *NavData) route(ctx context.Context, callsign string, pos *Point, fetch bool) (RouteInfo, error) {
	cs := normCallsign(callsign)
	if cs == "" {
		return RouteInfo{Callsign: strings.TrimSpace(callsign)}, fmt.Errorf("%w: callsign", ErrBadInput)
	}
	info := RouteInfo{Callsign: cs}
	rr, ok, err := n.StoredRoute(ctx, cs)
	if err != nil {
		return info, err
	}
	if ok {
		info.Found, info.Source, info.Airline = true, "standing-data", rr.Airline
	} else {
		opts := n.options().Routes
		if !opts.Online {
			return info, nil
		}
		var orr RouteRecord
		var cached bool
		if fetch {
			orr, ok, cached, err = n.onlineRoute(ctx, cs, opts)
		} else {
			orr, ok, cached, err = n.CachedRoute(ctx, cs)
		}
		if err != nil {
			return info, err
		}
		if !ok {
			info.Cached = cached
			return info, nil
		}
		rr = orr
		info.Found, info.Source, info.Airline, info.Cached = true, "adsbdb", rr.Airline, cached
	}
	for _, c := range rr.Codes {
		ra := RouteAirport{Code: c}
		if a, ok, err := n.AirportByCode(ctx, c, false); err != nil {
			return info, err
		} else if ok {
			ra.Resolved, ra.Airport = true, &a
		}
		info.Airports = append(info.Airports, ra)
	}
	info.Path, info.DistanceKm = routePath(info.Airports)
	if pos != nil {
		info.Check = CheckRoute(info.Airports, pos.Lat, pos.Lon)
	}
	return info, nil
}

// routePath builds the great-circle polyline over the resolved airports.
func routePath(as []RouteAirport) ([][2]float64, float64) {
	var pts [][2]float64
	total := 0.0
	var prev *Airport
	for i := range as {
		if !as[i].Resolved || as[i].Airport == nil {
			continue
		}
		a := as[i].Airport
		if prev != nil {
			seg := GreatCircle(prev.Lat, prev.Lon, a.Lat, a.Lon, 50)
			if len(pts) > 0 {
				seg = seg[1:]
			}
			pts = append(pts, seg...)
			total += geo.HaversineKm(prev.Lat, prev.Lon, a.Lat, a.Lon)
		}
		prev = a
	}
	unwrapLon(pts)
	return pts, round(total, 1)
}

// GreatCircle returns [lon,lat] points along the great circle with roughly
// one point per stepKm (2..256 points, lon in [-180,180)).
func GreatCircle(lat1, lon1, lat2, lon2, stepKm float64) [][2]float64 {
	d := geo.HaversineKm(lat1, lon1, lat2, lon2)
	n := int(math.Ceil(d / math.Max(stepKm, 1)))
	n = max(1, min(n, 255))
	out := make([][2]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		var la, lo float64
		switch i {
		case 0:
			la, lo = lat1, lon1
		case n:
			la, lo = lat2, lon2
		default:
			la, lo = geo.Intermediate(lat1, lon1, lat2, lon2, float64(i)/float64(n))
		}
		out = append(out, [2]float64{r6(lo), r6(la)})
	}
	return out
}

// unwrapLon makes consecutive longitudes continuous (no jump across the
// antimeridian) so map libraries draw the short way.
func unwrapLon(pts [][2]float64) {
	for i := 1; i < len(pts); i++ {
		for pts[i][0]-pts[i-1][0] > 180 {
			pts[i][0] -= 360
		}
		for pts[i][0]-pts[i-1][0] < -180 {
			pts[i][0] += 360
		}
	}
}

// CheckRoute tests whether (lat, lon) is plausibly on the route: near the
// great-circle corridor of some leg (tolerance 50 km + 15 % of the leg
// length, max 400 km), or within 40 km of one of its airports. Returns nil
// when fewer than two airports are resolved.
func CheckRoute(as []RouteAirport, lat, lon float64) *RouteCheck {
	if !geo.ValidLatLon(lat, lon) {
		return nil
	}
	var res []*Airport
	for i := range as {
		if as[i].Resolved && as[i].Airport != nil {
			res = append(res, as[i].Airport)
		}
	}
	if len(res) < 2 {
		return nil
	}
	var best *RouteCheck
	bestRatio := math.Inf(1)
	for i := 0; i+1 < len(res); i++ {
		a, b := res[i], res[i+1]
		leg := geo.HaversineKm(a.Lat, a.Lon, b.Lat, b.Lon)
		tol := math.Min(50+0.15*leg, 400)
		xt := geo.CrossTrackKm(lat, lon, a.Lat, a.Lon, b.Lat, b.Lon)
		at := geo.AlongTrackKm(lat, lon, a.Lat, a.Lon, b.Lat, b.Lon)
		dA := geo.HaversineKm(lat, lon, a.Lat, a.Lon)
		dB := geo.HaversineKm(lat, lon, b.Lat, b.Lon)
		c := &RouteCheck{Leg: i, CrossTrackKm: round(xt, 1), ToleranceKm: round(tol, 1)}
		if leg > 0 {
			c.AlongPct = round(at/leg*100, 1)
		}
		var ratio float64
		switch {
		case dA <= 40 || dB <= 40:
			c.Plausible, c.Reason = true, "near a route airport"
			ratio = math.Min(dA, dB) / 40 * 0.5
		case at >= -tol && at <= leg+tol && math.Abs(xt) <= tol:
			c.Plausible, c.Reason = true, "inside the route corridor"
			ratio = math.Abs(xt) / tol
		default:
			c.Reason = fmt.Sprintf("%.0f km off the route corridor", math.Max(math.Abs(xt), math.Min(dA, dB))-tol)
			ratio = 1 + math.Min(dA, dB)/math.Max(leg, 1) + math.Abs(xt)/tol
		}
		if ratio < bestRatio {
			bestRatio, best = ratio, c
		}
	}
	best.Doubtful = !best.Plausible
	return best
}

// adsbdbResponse is the subset of the adsbdb callsign answer we use.
type adsbdbResponse struct {
	Response json.RawMessage `json:"response"`
}

type adsbdbRoute struct {
	FlightRoute struct {
		Callsign string `json:"callsign"`
		Airline  *struct {
			ICAO string `json:"icao"`
		} `json:"airline"`
		Origin      adsbdbAirport  `json:"origin"`
		Midpoint    *adsbdbAirport `json:"midpoint"`
		Destination adsbdbAirport  `json:"destination"`
	} `json:"flightroute"`
}

type adsbdbAirport struct {
	ICAO string `json:"icao_code"`
	IATA string `json:"iata_code"`
}

func (a adsbdbAirport) code() string {
	if a.ICAO != "" {
		return strings.ToUpper(a.ICAO)
	}
	return strings.ToUpper(a.IATA)
}

// CachedRoute returns a cached online answer for callsign without ever
// sending a request (found=false and ok=false when nothing is cached or the
// entry expired; ok=true, found=false for a cached "unknown callsign").
// Callers that must not spend the online budget (public viewers) use it.
func (n *NavData) CachedRoute(ctx context.Context, callsign string) (rr RouteRecord, found, ok bool, err error) {
	cs := normCallsign(callsign)
	if cs == "" || n.st == nil {
		return RouteRecord{}, false, false, nil
	}
	if p, hit := n.pend.route(cs, time.Now()); hit {
		if p == nil {
			return RouteRecord{}, false, true, nil
		}
		return *p, len(p.Codes) >= 2, true, nil
	}
	var f int
	var airline, airports string
	err = n.st.DB().QueryRowContext(ctx, `SELECT found, airline, airports FROM navdata_route_cache WHERE callsign = ? AND expires > ?`,
		cs, time.Now().Unix()).Scan(&f, &airline, &airports)
	if errors.Is(err, sql.ErrNoRows) {
		return RouteRecord{}, false, false, nil
	}
	if err != nil {
		return RouteRecord{}, false, false, err
	}
	if f == 0 {
		return RouteRecord{}, false, true, nil
	}
	rr = RouteRecord{Callsign: cs, Airline: airline, Codes: splitCodes(airports)}
	return rr, len(rr.Codes) >= 2, true, nil
}

// OnlineRoute looks callsign up online (adsbdb) when online look-ups are
// enabled, through the cache, pacing and daily budget. It never reads the
// standing data. ErrDisabled when online look-ups are off.
func (n *NavData) OnlineRoute(ctx context.Context, callsign string) (RouteRecord, bool, error) {
	cs := normCallsign(callsign)
	if cs == "" {
		return RouteRecord{}, false, fmt.Errorf("%w: callsign", ErrBadInput)
	}
	o := n.options().Routes
	if !o.Online {
		return RouteRecord{}, false, ErrDisabled
	}
	rr, ok, _, err := n.onlineRoute(ctx, cs, o)
	return rr, ok, err
}

// OnlineBudget reports the online route look-ups used in the current 24-h
// window, the daily cap and whether the limiter is backing off.
func (n *NavData) OnlineBudget() (used, daily int, backoffUntil time.Time) {
	l := n.online
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now().Sub(l.day) < 24*time.Hour {
		used = l.count
	}
	if l.next.After(l.now()) {
		backoffUntil = l.next
	}
	return used, l.daily, backoffUntil
}

// onlineRoute consults the cache, then adsbdb.
func (n *NavData) onlineRoute(ctx context.Context, cs string, o RouteOptions) (RouteRecord, bool, bool, error) {
	if n.st == nil {
		return RouteRecord{}, false, false, ErrNoStore
	}
	if rr, ok := n.pend.route(cs, time.Now()); ok {
		// Queued, not committed yet (pending.go).
		if rr == nil {
			return RouteRecord{}, false, true, nil
		}
		return *rr, len(rr.Codes) >= 2, true, nil
	}
	now := time.Now().Unix()
	db := n.st.DB()
	var found int
	var airline, airports string
	err := db.QueryRowContext(ctx, `SELECT found, airline, airports FROM navdata_route_cache WHERE callsign = ? AND expires > ?`,
		cs, now).Scan(&found, &airline, &airports)
	if err == nil {
		if found == 0 {
			return RouteRecord{}, false, true, nil
		}
		rr := RouteRecord{Callsign: cs, Airline: airline, Codes: splitCodes(airports)}
		return rr, len(rr.Codes) >= 2, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RouteRecord{}, false, false, err
	}
	v, err := n.flight.do("route:"+cs, func() (any, error) { return n.fetchAdsbdb(ctx, cs, o) })
	if err != nil {
		return RouteRecord{}, false, false, err
	}
	rr := v.(*RouteRecord)
	if rr == nil {
		return RouteRecord{}, false, false, nil
	}
	return *rr, true, false, nil
}

func (n *NavData) fetchAdsbdb(ctx context.Context, cs string, o RouteOptions) (*RouteRecord, error) {
	wait, err := n.online.reserve(o.MaxWait)
	if err != nil {
		return nil, err
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	opts := n.options()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+url.PathEscape(cs), nil)
	if err != nil {
		return nil, err
	}
	ua := opts.UserAgent
	if o.Contact != "" {
		ua += " (+" + o.Contact + ")"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	resp, err := opts.Client.Do(req)
	if err != nil {
		n.online.backoff(30 * time.Second)
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, n.cacheRoute(ctx, cs, nil, now.Add(o.NegativeTTL))
	case http.StatusTooManyRequests:
		n.online.backoff(retryAfter(resp.Header.Get("Retry-After"), 10*time.Minute))
		return nil, ErrRateLimited
	default:
		n.online.backoff(time.Minute)
		return nil, fmt.Errorf("navdata: adsbdb: HTTP %s", resp.Status)
	}
	var env adsbdbResponse
	var r adsbdbRoute
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("navdata: adsbdb: %w", err)
	}
	if err := json.Unmarshal(env.Response, &r); err != nil || r.FlightRoute.Origin.code() == "" || r.FlightRoute.Destination.code() == "" {
		return nil, n.cacheRoute(ctx, cs, nil, now.Add(o.NegativeTTL))
	}
	rr := &RouteRecord{Callsign: cs}
	if r.FlightRoute.Airline != nil {
		rr.Airline = strings.ToUpper(r.FlightRoute.Airline.ICAO)
	}
	codes := []string{r.FlightRoute.Origin.code()}
	if r.FlightRoute.Midpoint != nil && r.FlightRoute.Midpoint.code() != "" {
		codes = append(codes, r.FlightRoute.Midpoint.code())
	}
	codes = append(codes, r.FlightRoute.Destination.code())
	rr.Codes = splitCodes(strings.Join(codes, "-"))
	if len(rr.Codes) < 2 {
		return nil, n.cacheRoute(ctx, cs, nil, now.Add(o.NegativeTTL))
	}
	return rr, n.cacheRoute(ctx, cs, rr, now.Add(o.CacheTTL))
}

// cacheRoute queues the answer for the store's group commit (one commit per
// lookup before) and serves it from the overlay until it is visible.
func (n *NavData) cacheRoute(_ context.Context, cs string, rr *RouteRecord, exp time.Time) error {
	if rr != nil {
		// What onlineRoute reads back from the row (no Number).
		rr = &RouteRecord{Callsign: cs, Airline: rr.Airline, Codes: splitCodes(strings.Join(rr.Codes, "-"))}
	}
	n.pend.putRoute(cs, rr, exp.Unix(), time.Now())
	n.queueWrite("navdata_route_cache", func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().Unix()
		// Opportunistic purge keeps the cache bounded to live entries.
		if _, err := tx.ExecContext(ctx, `DELETE FROM navdata_route_cache WHERE expires <= ?`, now); err != nil {
			return err
		}
		found, airline, airports := 0, "", ""
		if rr != nil {
			found, airline, airports = 1, rr.Airline, strings.Join(rr.Codes, "-")
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO navdata_route_cache(callsign, fetched, expires, found, airline, airports)
  VALUES(?,?,?,?,?,?)`, cs, now, exp.Unix(), found, airline, airports)
		return err
	})
	return nil
}

// ClearRouteCache deletes all cached online route answers (the write
// commits after the queued cache rows, so those go too).
func (n *NavData) ClearRouteCache(ctx context.Context) error {
	if n.st == nil {
		return ErrNoStore
	}
	n.pend.clearRoutes()
	return n.st.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM navdata_route_cache`)
		return err
	})
}

func retryAfter(h string, def time.Duration) time.Duration {
	if h == "" {
		return def
	}
	var s int
	if _, err := fmt.Sscanf(h, "%d", &s); err == nil && s > 0 && s < 86400 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 && d < 24*time.Hour {
			return d
		}
	}
	return def
}
