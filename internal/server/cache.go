package server

import (
	"net/http"
	"sync"
	"time"
)

// respCache is the response cache of the heavy GET endpoints. It does two
// things: identical requests that arrive while one is being computed wait
// for that result instead of starting their own (five open browsers cost
// one computation), and the encoded body is kept for the route's TTL so
// polling pages cost neither the analysis nor the JSON encoding.
type respCache struct {
	mu    sync.Mutex
	m     map[string]*cacheEntry
	calls map[string]*cacheCall
	gen   uint64 // bumped by invalidate: results of older calls are not stored
}

type cacheEntry struct {
	body, gz []byte
	expires  time.Time
	ttl      time.Duration // < 0: the result asked not to be stored
}

// cacheTTLer is implemented by results whose useful lifetime depends on
// their content (analytics answers on a fresh, near-empty database). The
// route's TTL is capped by it; a value <= 0 means "do not store".
type cacheTTLer interface {
	CacheTTL() time.Duration
}

type cacheCall struct {
	done chan struct{}
	ent  *cacheEntry
	err  error
}

const cacheMaxEntries = 256

func newRespCache() *respCache {
	return &respCache{m: map[string]*cacheEntry{}, calls: map[string]*cacheCall{}}
}

// do returns the cached entry for key or computes it once. Errors are not
// cached. key "" disables caching and de-duplication.
func (c *respCache) do(key string, ttl time.Duration, fn func() (any, error)) (*cacheEntry, error) {
	build := func() (*cacheEntry, error) {
		v, err := fn()
		if err != nil {
			return nil, err
		}
		b, err := marshal(v)
		if err != nil {
			return nil, err
		}
		ent := &cacheEntry{body: b, ttl: ttl}
		if t, ok := v.(cacheTTLer); ok {
			if d := t.CacheTTL(); d <= 0 {
				ent.ttl = -1
			} else {
				ent.ttl = min(ttl, d)
			}
		}
		if len(b) >= gzipMin {
			ent.gz = gzipBytes(b)
		}
		return ent, nil
	}
	if key == "" {
		return build()
	}
	now := time.Now()
	c.mu.Lock()
	if ent, ok := c.m[key]; ok && now.Before(ent.expires) {
		c.mu.Unlock()
		return ent, nil
	}
	if call, ok := c.calls[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.ent, call.err
	}
	call := &cacheCall{done: make(chan struct{})}
	c.calls[key] = call
	gen := c.gen
	c.mu.Unlock()

	finished := false
	defer func() {
		// Also runs when fn panics, so waiters are never left hanging.
		c.mu.Lock()
		if c.calls[key] == call {
			delete(c.calls, key)
		}
		if !finished && call.err == nil {
			call.ent, call.err = nil, errStatus(http.StatusInternalServerError, "internal error")
		}
		if call.ent != nil && call.ent.ttl > 0 && c.gen == gen {
			call.ent.expires = time.Now().Add(call.ent.ttl)
			if len(c.m) >= cacheMaxEntries {
				c.evictLocked()
			}
			c.m[key] = call.ent
		}
		c.mu.Unlock()
		close(call.done)
	}()
	call.ent, call.err = build()
	finished = true
	return call.ent, call.err
}

// evictLocked drops the expired entries, or everything when none expired.
func (c *respCache) evictLocked() {
	now := time.Now()
	for k, e := range c.m {
		if !now.Before(e.expires) {
			delete(c.m, k)
		}
	}
	if len(c.m) >= cacheMaxEntries {
		clear(c.m)
	}
}

// invalidate drops every cached response (configuration change, factory
// reset, database import).
func (c *respCache) invalidate() {
	c.mu.Lock()
	clear(c.m)
	c.gen++
	c.mu.Unlock()
}

func (c *respCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func writeEntry(w http.ResponseWriter, r *http.Request, ent *cacheEntry, ctype string) {
	writeBody(w, r, http.StatusOK, ent.body, ent.gz, ctype)
}
