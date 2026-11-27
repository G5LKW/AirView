package navdata

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// Online lookup results (navdata_route_cache, navdata_photo) are written
// with the store's group commit (Store.QueueTx) instead of a commit of their
// own per lookup (docs/perf/DB-EFFICIENCY-DESIGN.md §3.20: up to ~1,000
// route lookups a day were ~40 single-row commits an hour on an SD card).
// Until the queued row is visible in the database (at most the store's
// flush interval, 30 s on the low-wear profile), the result is served from
// this overlay, so a second lookup of the same key never asks the online
// service again.

// pendingHold is how long a queued result stays in the overlay: longer
// than any store flush interval. Keeping it after the commit is harmless
// (the overlay holds the same values as the committed row, and expiry is
// checked against the row's own expiry).
const pendingHold = 2 * time.Minute

type pendingRoute struct {
	rr      *RouteRecord // nil = negative answer
	expires int64        // Unix s, as the row
	hold    time.Time
}

type pendingPhoto struct {
	p       Photo // as cachedPhoto would read it back
	expires int64
	hold    time.Time
}

// pendingCache is the overlay of queued cache rows.
type pendingCache struct {
	mu     sync.Mutex
	routes map[string]pendingRoute
	photos map[uint32]pendingPhoto
}

// sweep drops entries past their hold (callers hold mu).
func (c *pendingCache) sweep(now time.Time) {
	for k, e := range c.routes {
		if now.After(e.hold) {
			delete(c.routes, k)
		}
	}
	for k, e := range c.photos {
		if now.After(e.hold) {
			delete(c.photos, k)
		}
	}
}

func (c *pendingCache) putRoute(cs string, rr *RouteRecord, expires int64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(now)
	if c.routes == nil {
		c.routes = map[string]pendingRoute{}
	}
	if rr != nil {
		cp := *rr
		cp.Codes = append([]string(nil), rr.Codes...)
		rr = &cp
	}
	c.routes[cs] = pendingRoute{rr: rr, expires: expires, hold: now.Add(pendingHold)}
}

// route returns a queued route answer (ok false: none).
func (c *pendingCache) route(cs string, now time.Time) (rr *RouteRecord, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.routes[cs]
	if !ok || now.After(e.hold) || e.expires <= now.Unix() {
		return nil, false
	}
	if e.rr == nil {
		return nil, true
	}
	cp := *e.rr
	cp.Codes = append([]string(nil), e.rr.Codes...)
	return &cp, true
}

func (c *pendingCache) putPhoto(icao uint32, p Photo, expires int64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(now)
	if c.photos == nil {
		c.photos = map[uint32]pendingPhoto{}
	}
	c.photos[icao] = pendingPhoto{p: p, expires: expires, hold: now.Add(pendingHold)}
}

func (c *pendingCache) photo(icao uint32, now time.Time) (Photo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.photos[icao]
	if !ok || now.After(e.hold) || e.expires <= now.Unix() {
		return Photo{}, false
	}
	return clonePhoto(e.p), true
}

func (c *pendingCache) clearRoutes() {
	c.mu.Lock()
	clear(c.routes)
	c.mu.Unlock()
}

func clonePhoto(p Photo) Photo {
	if p.Thumbnail != nil {
		t := *p.Thumbnail
		p.Thumbnail = &t
	}
	if p.Large != nil {
		l := *p.Large
		p.Large = &l
	}
	return p
}

// queueWrite hands one cache write to the store's group commit.
func (n *NavData) queueWrite(name string, fn func(ctx context.Context, tx *sql.Tx) error) {
	n.st.QueueTx(name, 1, fn)
}
