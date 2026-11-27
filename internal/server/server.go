// Package server is AirView's HTTP server: the JSON API, the Server-Sent
// Events live stream, the admin login, the map tile cache proxy and the
// embedded web UI.
//
// The seam used by cmd/airview is
//
//	func New(eng *engine.Engine, opts Options) *Server
//	type Options struct { PortOverride int; GUI bool; Version string }
//	func (*Server) Start(ctx context.Context) (addr string, port int, err error)
//	func (*Server) Shutdown(ctx context.Context) error
//	func (*Server) LocalURL() string
//
// Every API endpoint is declared once, in the route table of routes.go; the
// table drives the mux, the tests and the generated reference docs/API.md.
//
// Files: routes.go (table), respond.go (JSON encoding, gzip, parameters),
// cache.go (response cache + request de-duplication), auth.go (admin
// sessions, CSRF checks), sse.go (live stream), tilecache.go (tile proxy),
// api_*.go (handlers).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/engine"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
	"airview/internal/rfstat"
	"airview/web"
)

// Options configures a Server.
type Options struct {
	// PortOverride replaces Config.HTTP.Port when > 0 (the -port flag).
	PortOverride int
	// GUI is reported in /api/status: the UI runs inside the desktop window.
	GUI bool
	// Version is reported in /api/status; "" = the engine's version.
	Version string
	// StrictPort: fail Start when the configured port cannot be bound
	// instead of falling back to 8080.. (headless aggregators, SO-3: a
	// service behind a reverse proxy must not move to another port).
	StrictPort bool
	// Updater is the program's self-update (api_update.go; nil = the
	// update routes report "not supported").
	Updater UpdateController
}

// Server is the HTTP server. Create it with New.
type Server struct {
	eng  *engine.Engine
	crlb crlbCache // admin MLAT CRLB map (api_mlat.go)
	// mlatHist samples the MLAT engine once a minute (mlat_history.go);
	// testMLAT (tests) replaces the engine's MLAT view.
	mlatHist *mlatHistory
	testMLAT mlatSource
	opts     Options
	log      *slog.Logger

	handler http.Handler
	started time.Time

	// resetMu lets a factory reset wait for the API requests in flight and
	// keeps new ones out while the engine is being rebuilt.
	resetMu sync.RWMutex
	streams streamSet // exports streaming under resetMu.RLock (streams.go)

	auth  *auth
	cache *respCache

	// Aggregator mode (net.go, access.go, ratelimit.go).
	netw    netState
	limiter *rateLimiter
	// acctLimiter limits the routes marked AccountLimited per owner
	// account (finding PRIV-1: the MLAT peer view).
	acctLimiter *rateLimiter
	// bandRefs freezes the position peer distance bands are computed from
	// per viewer station and tag period (finding PRIV-1).
	bandRefs bandRefTable
	pub      pubCache // shared public live picture (api_view.go)
	// shares holds the owners' share links (api_share.go, sharestore.go),
	// created on first use in the data directory.
	sharesOnce sync.Once
	shares     *shareStore
	// verify watches the MLAT engine's ADS-B verification (api_verify.go).
	verify verifyWatch
	// improve caches the CRLB coverage-improvement analysis (api_improve.go).
	improve  improveCache
	rates    laneRates // recent frame rates of the public station list
	testRate *rateSpec // tests: replaces aggregator.limits.publicRps/publicBurst
	// testGuard (tests) replaces the island-guard predicate when set.
	testGuard publicdto.Guard
	live      *liveHub
	tiles     *tileCache

	exportSem chan struct{} // concurrent CSV exports
	heavySem  chan struct{} // concurrent uncached heavy computations

	// elevation looks up terrain height (overridden in tests).
	elevation func(ctx context.Context, lat, lon float64) (float64, error)

	// Shared RF report (rfstat.Report costs ~0.3 ms and 300 kB: computed
	// at most every 2 s for all /api/rf/* endpoints).
	rfMu   sync.Mutex
	rfRep  *rfstat.Report
	rfFrom *rfstat.Stats
	rfAt   time.Time

	mu     sync.Mutex
	http   *http.Server
	ln     net.Listener
	addr   string
	port   int
	bind   string
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a server for a started engine.
func New(eng *engine.Engine, opts Options) *Server {
	if opts.Version == "" {
		opts.Version = eng.Version()
	}
	s := &Server{
		eng:         eng,
		opts:        opts,
		log:         slog.Default(),
		started:     time.Now(),
		auth:        newAuth(),
		cache:       newRespCache(),
		limiter:     newRateLimiter(),
		acctLimiter: newRateLimiter(),
		exportSem:   make(chan struct{}, 2),
		heavySem:    make(chan struct{}, 3),
		mlatHist:    newMLATHistory(),
	}
	s.live = newLiveHub(s)
	s.tiles = newTileCache(s)
	s.elevation = func(ctx context.Context, lat, lon float64) (float64, error) {
		wx := s.eng.Weather()
		if wx == nil {
			return 0, errors.New("weather client not available")
		}
		return wx.Client().Elevation(ctx, lat, lon)
	}
	s.handler = s.buildHandler()
	// The federation export uses the public map's island guard.
	eng.SetFederationGuard(s.islandGuard)
	return s
}

// Handler returns the complete HTTP handler (API, tile cache and UI). It is
// what Start serves; tests use it with httptest.
func (s *Server) Handler() http.Handler { return s.handler }

// fallbackPorts are tried in order when the wanted port cannot be bound; 0
// (an ephemeral port) is the last resort.
var fallbackPorts = []int{8080, 8081, 8082, 8083, 8084, 8085, 8086, 8087, 8088, 8089, 0}

// listen binds the wanted port or the first fallback that works.
func listen(ctx context.Context, bind string, want int) (net.Listener, error) {
	var lc net.ListenConfig
	try := append([]int{want}, fallbackPorts...)
	var firstErr error
	for i, p := range try {
		if i > 0 && p == want {
			continue
		}
		ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(bind, strconv.Itoa(p)))
		if err == nil {
			return ln, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// Start binds the HTTP port (Config.HTTP.Port or the override; when that is
// busy 8080..8089, then an ephemeral port) and serves in the background. It
// returns the listen address and the actual port.
func (s *Server) Start(ctx context.Context) (addr string, port int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		return s.addr, s.port, errors.New("server: already started")
	}
	cfg := s.eng.Config().Get()
	want := cfg.HTTP.Port
	if s.opts.PortOverride > 0 {
		want = s.opts.PortOverride
	}
	if want <= 0 || want > 65535 {
		want = 80
	}
	var ln net.Listener
	if s.opts.StrictPort {
		var lc net.ListenConfig
		ln, err = lc.Listen(ctx, "tcp", net.JoinHostPort(cfg.HTTP.BindAddr, strconv.Itoa(want)))
		if err != nil {
			return "", 0, fmt.Errorf("server: HTTP port %d cannot be bound (no fallback for a headless aggregator; free the port or change http.port): %w", want, err)
		}
	} else {
		ln, err = listen(ctx, cfg.HTTP.BindAddr, want)
	}
	if err != nil {
		return "", 0, fmt.Errorf("server: no HTTP port could be bound: %w", err)
	}
	ln = s.limitListener(ln)
	s.ln = ln
	s.addr = ln.Addr().String()
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.bind = cfg.HTTP.BindAddr
	if s.port != want {
		s.log.Warn("HTTP port is not available, using a fallback", "wanted", want, "port", s.port)
	}

	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(4)
	go func() { defer s.wg.Done(); s.runVerify(bg) }()
	go func() { defer s.wg.Done(); s.live.run(bg) }()
	go func() { defer s.wg.Done(); s.tiles.run(bg) }()
	go func() { defer s.wg.Done(); s.mlatHist.run(bg, s) }()

	// No ReadTimeout / WriteTimeout: they would cut the SSE stream, CSV
	// exports and database uploads. Slow-header attacks are covered by
	// ReadHeaderTimeout, slow bodies by the per-request body deadline of
	// routeHandler (bodyDeadline), JSON writes by its write deadline, and
	// connection floods by the aggregator's connection caps (connlimit.go).
	s.http = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	srv := s.http
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP server stopped", "err", err)
		}
	}()
	s.log.Info("HTTP server listening", "addr", s.addr, "url", s.localURL())
	if s.aggregatorMode() && !isLoopbackBind(cfg.HTTP.BindAddr) {
		switch {
		case cfg.HTTP.AllowInsecureLogin:
			s.log.Warn("http.allowInsecureLogin is set: passwords and admin sessions are accepted over plain HTTP from the network; put an HTTPS reverse proxy in front instead")
		case len(cfg.Aggregator.TrustedProxies) == 0:
			s.log.Warn("plain HTTP without a trusted reverse proxy: logins are accepted only from this machine; put an HTTPS reverse proxy in front (aggregator.trustedProxies) for remote logins")
		}
	}
	return s.addr, s.port, nil
}

// Shutdown stops the server, waiting for requests in flight until ctx ends.
// Live streams are closed at once.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.http
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel() // ends the live hub, which closes every SSE stream
		s.wg.Wait()
	}
	s.shareStore().flush() // share-link use counters
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	return err
}

// Port returns the bound port (0 before Start).
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Addr returns the listen address ("" before Start).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// LocalURL returns the URL of the UI as seen from this machine, e.g.
// "http://127.0.0.1:8080/" ("" before Start).
func (s *Server) LocalURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localURL()
}

func (s *Server) localURL() string {
	if s.port == 0 {
		return ""
	}
	host := "127.0.0.1"
	if ip := net.ParseIP(s.bind); ip != nil && !ip.IsUnspecified() {
		host = ip.String()
	}
	hp := net.JoinHostPort(host, strconv.Itoa(s.port))
	if s.port == 80 {
		hp = host
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			hp = "[" + host + "]"
		}
	}
	return "http://" + hp + "/"
}

// buildHandler mounts the route table, the tile cache and the UI.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	for i := range routes {
		rt := &routes[i]
		mux.Handle(rt.Method+" "+rt.Path, s.routeHandler(rt))
	}
	mux.HandleFunc("OPTIONS /api/", s.handlePreflight)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		setCORS(w, r)
		writeError(w, http.StatusNotFound, "not found")
	})
	mux.HandleFunc("/tilecache/", s.tiles.serve)
	// Framing is allowed on purpose: no X-Frame-Options and no CSP
	// frame-ancestors. In remote-web mode the desktop window shows
	// remote.html from THIS AirView, which frames the UI of ANOTHER AirView
	// host (any LAN address or name the user configured), so the framed
	// host cannot know its legitimate ancestor; a frame-ancestors list wide
	// enough for that ("http: https:") would protect nothing. Clickjacking
	// is contained instead by the admin model: state-changing endpoints need
	// a loopback client or an admin session cookie (SameSite=Lax, not sent
	// in a cross-site frame) plus the same-origin checks in auth.go.
	mux.Handle("/", web.Handler()) // sets JavaScript MIME types; never X-Frame-Options
	return s.recoverer(mux)
}

// recoverer turns a handler panic into a 500 instead of a dropped connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("HTTP handler panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// setCORS allows simple cross-origin reads: the remote shell page and other
// AirView instances on the LAN fetch status and live data from this host.
// No credentials are allowed, so a foreign page never acts as a logged-in
// admin; mutating requests are refused cross-origin (see checkOrigin).
func setCORS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == "" {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Add("Vary", "Origin")
}

// corsReadable reports whether foreign pages may read a route (SEC-R3):
// only the routes whose answer is the same for every client (public) or
// is projected per viewer from the session cookie, which a cross-origin
// request without credentials never carries (viewer). Configuration,
// alerts, history, database exports and the other admin-scope routes get
// no Access-Control-Allow-Origin; in standalone mode a cross-site request
// is never admin either (isAdmin), so /api/config answers its public form.
func corsReadable(rt *Route) bool {
	return rt.Access == AccessPublic || rt.Access == AccessViewer
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Cache-Control, Last-Event-ID")
	h.Set("Access-Control-Max-Age", "600")
	h.Add("Vary", "Origin")
	w.WriteHeader(http.StatusNoContent)
}

// routeHandler wraps one route with CORS, the CSRF and admin checks, the
// factory-reset gate, the response cache and JSON encoding.
func (s *Server) routeHandler(rt *Route) http.Handler {
	mutating := rt.Method != http.MethodGet && rt.Method != http.MethodHead
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mutating {
			if corsReadable(rt) {
				setCORS(w, r)
			}
		} else if rt.Path != "/api/login" {
			if msg := s.checkOrigin(r); msg != "" {
				writeError(w, http.StatusForbidden, msg)
				return
			}
		}
		if rt.Raw == nil || mutating {
			r = s.bodyDeadline(w, r, rt)
		}
		if rt.Credentials && s.insecureCredentials(r) {
			writeError(w, http.StatusForbidden, errInsecureLogin)
			return
		}
		if rt.Admin && !s.isAdmin(r) {
			writeError(w, http.StatusUnauthorized, "admin password required")
			return
		}
		// Aggregator mode: route scope, public rate limit, station
		// selection (access.go). Standalone: only NetOnly routes are cut.
		r, ok := s.gate(w, r, rt)
		if !ok {
			return
		}
		if rt.Raw != nil {
			rt.Raw(s, w, r)
			return
		}
		if rt.Exclusive {
			s.lockExclusive()
			defer s.resetMu.Unlock()
		} else {
			s.resetMu.RLock()
			defer s.resetMu.RUnlock()
		}
		if !s.eng.Running() {
			writeError(w, http.StatusServiceUnavailable, "engine is not running")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), apiTimeout)
		defer cancel()
		r = r.WithContext(ctx)

		if rt.Cache > 0 {
			ent, err := s.cache.do(s.viewerCacheKey(r, rt), rt.Cache, func() (any, error) {
				// Detached from the first caller: its disconnect must not
				// fail the others waiting for the same result.
				c2, cancel2 := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
				defer cancel2()
				select {
				case s.heavySem <- struct{}{}:
					defer func() { <-s.heavySem }()
				case <-c2.Done():
					return nil, c2.Err()
				}
				return rt.API(s, r.WithContext(c2))
			})
			if err != nil {
				s.writeAPIError(w, r, err)
				return
			}
			writeEntry(w, r, ent, rt.ContentType)
			return
		}
		v, err := rt.API(s, r)
		if err != nil {
			s.writeAPIError(w, r, err)
			return
		}
		status := http.StatusOK
		if sr, ok := v.(statusResult); ok {
			status, v = sr.status, sr.v
		}
		body, err := marshal(v)
		if err != nil {
			s.writeAPIError(w, r, err)
			return
		}
		writeBody(w, r, status, body, nil, rt.ContentType)
	})
}

// apiTimeout bounds the work of one JSON request.
const apiTimeout = 90 * time.Second

func (s *Server) writeAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	var rl *rateLimitedError
	switch {
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", retryAfter(rl.retry))
		writeError(w, http.StatusTooManyRequests, rl.Error())
	case errors.As(err, &he):
		writeError(w, he.status, he.msg)
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "request cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "request timed out")
	default:
		msg := err.Error()
		// A factory reset or shutdown swaps the database under a reader.
		if strings.Contains(msg, "database is closed") || strings.Contains(msg, "store: closed") {
			writeError(w, http.StatusServiceUnavailable, "database is restarting")
			return
		}
		s.log.Warn("API request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, msg)
	}
}

// status returns the complete model.Status for one client.
func (s *Server) status(admin bool) model.Status {
	st := s.eng.Status()
	st.Version = s.opts.Version
	st.GUI = s.opts.GUI
	st.HTTPPort = s.Port()
	st.AdminLocked = !admin
	if !admin {
		st.DataDir = "" // contains the OS user name
	}
	if st.TileCache {
		st.TileCacheHosts = s.tiles.hosts()
	}
	if st.Feeds == nil {
		st.Feeds = []model.FeedStatus{}
	}
	return st
}
