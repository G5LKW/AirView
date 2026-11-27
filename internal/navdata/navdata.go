package navdata

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// Errors.
var (
	ErrNoStore        = errors.New("navdata: no store")
	ErrImportRunning  = errors.New("navdata: an import is already running")
	ErrNoRecords      = errors.New("navdata: the file contained no usable records")
	ErrTooLarge       = errors.New("navdata: download exceeds the size limit")
	ErrIdle           = errors.New("navdata: download stalled")
	ErrDisabled       = errors.New("navdata: online lookup is disabled")
	ErrRateLimited    = errors.New("navdata: online lookup rate limit reached, try again later")
	ErrContactMissing = errors.New("navdata: planespotters.net requires a contact URL or e-mail in the User-Agent; set Photos.Contact")
	ErrBadInput       = errors.New("navdata: invalid input")
)

// Options configure a NavData. The zero value is valid: imports only, both
// online lookups off.
type Options struct {
	// Client is used for all HTTP requests (default: a client with sane timeouts).
	Client *http.Client
	// UserAgent for downloads (default "AirView/<version>").
	UserAgent string
	Routes    RouteOptions
	Photos    PhotoOptions
	Logger    *slog.Logger
}

// NavData is the package entry point. Safe for concurrent use.
type NavData struct {
	st *store.Store

	mu     sync.Mutex
	opts   Options
	log    *slog.Logger
	job    *importJob
	online *limiter // adsbdb route lookups
	photo  *limiter // planespotters lookups
	flight singleflight
	pend   pendingCache // queued online cache rows (pending.go)
}

// New returns a NavData over st (which must stay open while NavData is used).
// st may be nil for parse-only use; database methods then return ErrNoStore.
func New(st *store.Store, opts Options) *NavData {
	n := &NavData{st: st}
	n.SetOptions(opts)
	return n
}

// SetOptions applies new options live (e.g. after a config change).
func (n *NavData) SetOptions(opts Options) {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 0, Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConnsPerHost:   2,
		}}
	}
	if opts.UserAgent == "" {
		opts.UserAgent = model.AppName + "/" + model.Version
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.Routes = opts.Routes.withDefaults()
	opts.Photos = opts.Photos.withDefaults()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.opts = opts
	n.log = opts.Logger
	if n.online == nil {
		n.online = newLimiter()
		n.photo = newLimiter()
	}
	n.online.configure(opts.Routes.MinInterval, opts.Routes.DailyLimit)
	n.photo.configure(opts.Photos.MinInterval, opts.Photos.DailyLimit)
}

func (n *NavData) options() Options {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.opts
}

// limiter is a polite request pacer: at most one request per interval and a
// rolling 24 h budget. It never queues: callers that would have to wait
// longer than their context allows get ErrRateLimited.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	daily    int
	next     time.Time
	day      time.Time
	count    int
	now      func() time.Time
}

func newLimiter() *limiter { return &limiter{now: time.Now} }

func (l *limiter) configure(interval time.Duration, daily int) {
	l.mu.Lock()
	l.interval, l.daily = interval, daily
	l.mu.Unlock()
}

// reserve returns how long the caller must wait before its request, or
// ErrRateLimited when the daily budget is spent or the wait exceeds maxWait.
func (l *limiter) reserve(maxWait time.Duration) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.day) >= 24*time.Hour {
		l.day, l.count = now, 0
	}
	if l.daily > 0 && l.count >= l.daily {
		return 0, ErrRateLimited
	}
	wait := time.Duration(0)
	if l.next.After(now) {
		wait = l.next.Sub(now)
	}
	if wait > maxWait {
		return 0, ErrRateLimited
	}
	l.count++
	l.next = now.Add(wait + l.interval)
	return wait, nil
}

// backoff pushes the next allowed request out (after HTTP 429 / errors).
func (l *limiter) backoff(d time.Duration) {
	l.mu.Lock()
	if t := l.now().Add(d); t.After(l.next) {
		l.next = t
	}
	l.mu.Unlock()
}

// singleflight de-duplicates concurrent identical lookups.
type singleflight struct {
	mu sync.Mutex
	m  map[string]*call
}

type call struct {
	done chan struct{}
	val  any
	err  error
}

func (g *singleflight) do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err
	}
	c := &call{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()
	c.val, c.err = fn()
	close(c.done)
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.val, c.err
}
