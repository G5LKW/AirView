package auth

import (
	"strings"
	"sync"
	"time"
)

// Throttle slows down guessing per key (a client IP, or "user:<name>").
//
// The first Free failures inside Window cost nothing; after that every
// failure blocks the key for 2 s, 4 s, 8 s ... up to MaxDelay. A key is
// forgotten Window after its last failure, and a success clears it. This is
// the algorithm of the standalone admin login (internal/server/auth.go),
// generalised so aggregator logins use "the existing throttle" (DESIGN §7)
// for both the address and the account name.
type Throttle struct {
	Free     int
	Window   time.Duration
	MaxDelay time.Duration
	MaxKeys  int
	now      func() time.Time

	mu   sync.Mutex
	keys map[string]*throttleEntry
}

type throttleEntry struct {
	fails int
	last  time.Time
	until time.Time
}

// NewThrottle returns a throttle; zero arguments take the standalone
// defaults (5 free failures, 15 min window, 5 min cap, 4096 keys).
func NewThrottle(free int, window, maxDelay time.Duration, maxKeys int) *Throttle {
	if free <= 0 {
		free = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	if maxDelay <= 0 {
		maxDelay = 5 * time.Minute
	}
	if maxKeys <= 0 {
		maxKeys = 4096
	}
	return &Throttle{Free: free, Window: window, MaxDelay: maxDelay, MaxKeys: maxKeys,
		now: time.Now, keys: map[string]*throttleEntry{}}
}

// Blocked returns how long key still has to wait (0 = may try now).
func (t *Throttle) Blocked(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.keys[key]
	if e == nil {
		return 0
	}
	now := t.now()
	if now.Sub(e.last) > t.Window {
		delete(t.keys, key)
		return 0
	}
	return max(e.until.Sub(now), 0)
}

// Fail records a failure and returns the wait now imposed on key.
func (t *Throttle) Fail(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.keys[key]
	if e == nil || now.Sub(e.last) > t.Window {
		if len(t.keys) >= t.MaxKeys {
			for k, v := range t.keys {
				if now.Sub(v.last) > t.Window {
					delete(t.keys, k)
				}
			}
			if len(t.keys) >= t.MaxKeys {
				// Flooded from many keys: evict the entry idle longest
				// rather than forgetting everybody's state.
				var oldK string
				var oldT time.Time
				for k, v := range t.keys {
					if oldK == "" || v.last.Before(oldT) {
						oldK, oldT = k, v.last
					}
				}
				delete(t.keys, oldK)
			}
		}
		e = &throttleEntry{}
		t.keys[key] = e
	}
	e.fails++
	e.last = now
	if e.fails < t.Free {
		return 0
	}
	d := time.Second << min(e.fails-t.Free+1, 20)
	d = min(d, t.MaxDelay)
	e.until = now.Add(d)
	return d
}

// Success forgets key.
func (t *Throttle) Success(key string) {
	t.mu.Lock()
	delete(t.keys, key)
	t.mu.Unlock()
}

// ForgetPrefix forgets every key starting with prefix.
func (t *Throttle) ForgetPrefix(prefix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.keys {
		if strings.HasPrefix(k, prefix) {
			delete(t.keys, k)
		}
	}
}

// Len returns the number of tracked keys.
func (t *Throttle) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.keys)
}

// WindowLimiter allows at most N events per key in any sliding Window
// (DESIGN §7: claims 5 per 10 min per IP and 20 per day per account;
// registrations).
type WindowLimiter struct {
	N       int
	Window  time.Duration
	MaxKeys int
	now     func() time.Time

	mu   sync.Mutex
	keys map[string][]time.Time
}

// NewWindowLimiter returns a limiter of n events per window.
func NewWindowLimiter(n int, window time.Duration, maxKeys int) *WindowLimiter {
	if maxKeys <= 0 {
		maxKeys = 8192
	}
	return &WindowLimiter{N: max(n, 1), Window: window, MaxKeys: maxKeys, now: time.Now,
		keys: map[string][]time.Time{}}
}

// Allow records an event for key if the budget allows it. When it does not,
// it returns false and how long until the oldest event leaves the window.
func (l *WindowLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ev := l.prune(key, now)
	if len(ev) >= l.N {
		return false, ev[0].Add(l.Window).Sub(now)
	}
	if _, ok := l.keys[key]; !ok && len(l.keys) >= l.MaxKeys {
		for k := range l.keys {
			if len(l.prune(k, now)) == 0 {
				delete(l.keys, k)
			}
		}
		if len(l.keys) >= l.MaxKeys {
			// Fail closed for new keys while flooded.
			return false, l.Window
		}
	}
	l.keys[key] = append(ev, now)
	return true, 0
}

// Peek reports whether an event would be allowed now, without recording it.
func (l *WindowLimiter) Peek(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, l.now())) < l.N
}

func (l *WindowLimiter) prune(key string, now time.Time) []time.Time {
	ev := l.keys[key]
	i := 0
	for i < len(ev) && now.Sub(ev[i]) >= l.Window {
		i++
	}
	if i > 0 {
		ev = append(ev[:0], ev[i:]...)
		if len(ev) == 0 {
			delete(l.keys, key)
		} else {
			l.keys[key] = ev
		}
	}
	return ev
}
