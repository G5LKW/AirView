package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/model"
)

// Default endpoints.
const (
	DefaultForecastURL           = "https://api.open-meteo.com/v1/forecast"
	DefaultHistoricalForecastURL = "https://historical-forecast-api.open-meteo.com/v1/forecast"
	DefaultArchiveURL            = "https://archive-api.open-meteo.com/v1/archive"
	DefaultElevationURL          = "https://api.open-meteo.com/v1/elevation"
	DefaultRainViewerURL         = "https://api.rainviewer.com/public/weather-maps.json"
)

// Open-Meteo free (non-commercial) tier limits, in weighted API calls
// (https://open-meteo.com/en/terms, https://open-meteo.com/en/pricing). A
// request counts as max(1, variables/10 × max(1, days/14)) calls (see
// estimateCost); the default AirView poll (21 current + 109 hourly variables,
// 9 days) costs 13.0 calls, i.e. about 1,250 calls/day (39,000/month) at the
// default 15-minute interval and at most 3,750/day (116,000/month) at the
// minimum 5-minute interval. Limits are applied per client IP address.
const (
	FreeTierPerMinute = 600
	FreeTierPerHour   = 5000
	FreeTierPerDay    = 10000
	FreeTierPerMonth  = 300000 // documented; "not enforced" at the time of writing
)

// Budget is the client's self-imposed ceiling on weighted Open-Meteo calls.
// Zero fields take the defaults (half of the free tier).
type Budget struct {
	PerMinute float64
	PerHour   float64
	PerDay    float64
}

// DefaultBudget keeps AirView at no more than half of the free-tier limits
// (5,000 calls/day is also about half of the monthly allowance).
var DefaultBudget = Budget{PerMinute: 300, PerHour: 2500, PerDay: 5000}

// Usage reports the weighted Open-Meteo calls made by this client.
type Usage struct {
	Minute float64 `json:"minute"` // last 60 s
	Hour   float64 `json:"hour"`   // last 60 min
	Day    float64 `json:"day"`    // last 24 h
	Budget Budget  `json:"budget"`
	// ServerRemaining is the server-reported remaining allowance in the
	// current rate window (X-RateLimit-Remaining); -1 when unknown.
	ServerRemaining float64 `json:"serverRemaining"`
}

// ErrBudget is returned when a request would exceed the client's Budget.
var ErrBudget = errors.New("weather: Open-Meteo request budget exhausted")

// APIError is a non-success HTTP answer from a weather service.
type APIError struct {
	Status int    // HTTP status code
	Reason string // server-provided reason, if any
	// RetryAfter is how long to wait before trying again (rate limiting);
	// 0 when the server gave no hint.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("weather: HTTP %d: %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("weather: HTTP %d %s", e.Status, http.StatusText(e.Status))
}

// RateLimited reports whether the error is an HTTP 429.
func (e *APIError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// Options configures a Client. The zero value is ready for production use.
type Options struct {
	// HTTPClient defaults to a dedicated client using the system proxy
	// settings. Timeouts are applied per request through contexts.
	HTTPClient *http.Client
	// UserAgent defaults to "AirView/<version>".
	UserAgent string
	// Timeout bounds one HTTP attempt including the body (default 25 s).
	Timeout time.Duration
	// HistoryTimeout bounds one attempt of a historical backfill request
	// (default 90 s): the archive back-ends read cold storage and can be slow.
	HistoryTimeout time.Duration
	// Retries is the number of extra attempts after a network error or a 5xx
	// answer (default 2; negative = none). 4xx answers are never retried.
	Retries int
	// Budget caps weighted Open-Meteo calls; zero fields use DefaultBudget.
	Budget Budget
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// Endpoint overrides (tests, self-hosted Open-Meteo instances).
	ForecastURL           string
	HistoricalForecastURL string
	ArchiveURL            string
	ElevationURL          string
	RainViewerURL         string
}

// Client talks to Open-Meteo and RainViewer. It is safe for concurrent use.
type Client struct {
	hc          *http.Client
	ua          string
	timeout     time.Duration
	histTimeout time.Duration
	retries     int
	budget      Budget
	log         *slog.Logger

	forecastURL   string
	histFcURL     string
	archiveURL    string
	elevationURL  string
	rainViewerURL string

	retryBase time.Duration    // first retry delay (tests shorten it)
	now       func() time.Time // clock (tests)

	mu           sync.Mutex
	usage        []usageBucket     // per-minute cost buckets, ascending, last 24 h
	srvRemaining float64           // X-RateLimit-Remaining, -1 unknown
	srvReset     time.Time         // X-RateLimit-Reset
	cache        map[string]cached // conditional-GET cache keyed by URL
	skip         map[string]bool   // variables the server rejected as unknown
}

type usageBucket struct {
	minute int64 // Unix minute
	cost   float64
}

// cached is a conditional-GET cache entry. At the time of writing Open-Meteo
// sends no validators at all and RainViewer sends an ETag but still answers
// 200 to If-None-Match, so this only starts saving bandwidth once a service
// honours it; the handling is exercised against a fake server in the tests.
type cached struct {
	etag    string
	lastMod string
	body    []byte
	stored  time.Time
}

const (
	maxBody         = 32 << 20
	maxCacheEntries = 8
	maxCacheBody    = 1 << 20
)

// NewClient creates a Client.
func NewClient(o Options) *Client {
	c := &Client{
		hc:            o.HTTPClient,
		ua:            o.UserAgent,
		timeout:       o.Timeout,
		histTimeout:   o.HistoryTimeout,
		retries:       o.Retries,
		budget:        o.Budget,
		log:           o.Logger,
		forecastURL:   o.ForecastURL,
		histFcURL:     o.HistoricalForecastURL,
		archiveURL:    o.ArchiveURL,
		elevationURL:  o.ElevationURL,
		rainViewerURL: o.RainViewerURL,
		retryBase:     time.Second,
		now:           time.Now,
		srvRemaining:  -1,
		cache:         make(map[string]cached),
		skip:          make(map[string]bool),
	}
	if c.hc == nil {
		// A private transport (system proxy settings, own small connection
		// pool). Every attempt is bounded by a context deadline, so no
		// transport-level response timeout is set.
		tr := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			tr = dt.Clone()
		}
		tr.MaxIdleConns = 4
		tr.MaxIdleConnsPerHost = 2
		tr.IdleConnTimeout = 90 * time.Second
		tr.TLSHandshakeTimeout = 10 * time.Second
		c.hc = &http.Client{Transport: tr}
	}
	if c.ua == "" {
		c.ua = model.AppName + "/" + model.Version
	}
	if c.timeout <= 0 {
		c.timeout = 25 * time.Second
	}
	if c.histTimeout <= 0 {
		c.histTimeout = 90 * time.Second
	}
	if c.retries == 0 {
		c.retries = 2
	} else if c.retries < 0 {
		c.retries = 0
	}
	if c.budget.PerMinute <= 0 {
		c.budget.PerMinute = DefaultBudget.PerMinute
	}
	if c.budget.PerHour <= 0 {
		c.budget.PerHour = DefaultBudget.PerHour
	}
	if c.budget.PerDay <= 0 {
		c.budget.PerDay = DefaultBudget.PerDay
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&c.forecastURL, DefaultForecastURL)
	def(&c.histFcURL, DefaultHistoricalForecastURL)
	def(&c.archiveURL, DefaultArchiveURL)
	def(&c.elevationURL, DefaultElevationURL)
	def(&c.rainViewerURL, DefaultRainViewerURL)
	return c
}

// UserAgent returns the User-Agent header the client sends.
func (c *Client) UserAgent() string { return c.ua }

// Usage returns the client's Open-Meteo call accounting.
func (c *Client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	m, h, d := c.usageLocked(now)
	u := Usage{Minute: m, Hour: h, Day: d, Budget: c.budget, ServerRemaining: -1}
	if c.srvRemaining >= 0 && now.Before(c.srvReset) {
		u.ServerRemaining = c.srvRemaining
	}
	return u
}

// usageLocked prunes old buckets and returns the cost of the last minute
// (current and previous clock minute), hour and day.
func (c *Client) usageLocked(now time.Time) (minute, hour, day float64) {
	cur := now.Unix() / 60
	i := 0
	for i < len(c.usage) && c.usage[i].minute <= cur-1440 {
		i++
	}
	if i > 0 {
		c.usage = append(c.usage[:0], c.usage[i:]...)
	}
	for _, b := range c.usage {
		day += b.cost
		if b.minute > cur-60 {
			hour += b.cost
		}
		if b.minute >= cur-1 {
			minute += b.cost
		}
	}
	return minute, hour, day
}

func (c *Client) addUsageLocked(now time.Time, cost float64) {
	cur := now.Unix() / 60
	if n := len(c.usage); n > 0 && c.usage[n-1].minute == cur {
		c.usage[n-1].cost += cost
		if c.usage[n-1].cost < 0 {
			c.usage[n-1].cost = 0
		}
		return
	}
	if cost < 0 {
		cost = 0
	}
	c.usage = append(c.usage, usageBucket{minute: cur, cost: cost})
}

// reserve books cost against the budget. With pace set it waits for the
// per-minute window (ours and the server's) to free up instead of failing;
// hourly and daily exhaustion always fail.
func (c *Client) reserve(ctx context.Context, cost float64, pace bool) error {
	if cost <= 0 {
		return nil
	}
	for {
		c.mu.Lock()
		now := c.now()
		m, h, d := c.usageLocked(now)
		if d+cost > c.budget.PerDay {
			c.mu.Unlock()
			return fmt.Errorf("%w (%.0f of %.0f calls in the last 24 h)", ErrBudget, d, c.budget.PerDay)
		}
		if h+cost > c.budget.PerHour {
			c.mu.Unlock()
			return fmt.Errorf("%w (%.0f of %.0f calls in the last hour)", ErrBudget, h, c.budget.PerHour)
		}
		var wait time.Duration
		if m+cost > c.budget.PerMinute && m > 0 {
			// Our accounting works on clock minutes: two minutes ahead the
			// current usage has certainly left the window.
			wait = time.Duration((now.Unix()/60+2)*60-now.Unix())*time.Second + time.Second
		}
		if c.srvRemaining >= 0 && now.Before(c.srvReset) && c.srvRemaining < cost+10 {
			if w := c.srvReset.Sub(now) + time.Second; w > wait {
				wait = w
			}
		}
		if wait == 0 {
			c.addUsageLocked(now, cost)
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		if !pace {
			return fmt.Errorf("%w (per-minute window full, retry in %v)", ErrBudget, wait.Round(time.Second))
		}
		if wait > 3*time.Minute {
			wait = 3 * time.Minute
		}
		c.log.Debug("weather: pacing Open-Meteo requests", "wait", wait)
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// request describes one GET.
type request struct {
	url     string
	cost    float64       // estimated weighted Open-Meteo calls (0 for other services)
	pace    bool          // background bulk request: wait for rate windows instead of failing
	timeout time.Duration // per-attempt timeout; 0 = Client timeout
}

// maxPacedWait is the longest server-requested pause a paced request sits out
// before giving up (hourly/daily limits are reported to the caller instead).
const maxPacedWait = 2 * time.Minute

// get performs the request with budget accounting, conditional GET, retries
// and error decoding. It returns the body and the weighted cost charged.
func (c *Client) get(ctx context.Context, r request) ([]byte, float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := c.reserve(ctx, r.cost, r.pace); err != nil {
		return nil, 0, err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		body, cost, retry, err := c.attempt(ctx, r)
		if err == nil {
			return body, cost, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		delay := c.retryBase << uint(attempt)
		if r.pace {
			// Bulk requests are in no hurry, and a timed-out archive request
			// may still occupy one of our concurrency slots upstream.
			delay *= 5
		}
		delay += time.Duration(rand.Int64N(int64(delay)/2 + 1))
		var ae *APIError
		if r.pace && errors.As(err, &ae) && ae.RateLimited() && ae.RetryAfter <= maxPacedWait {
			// A bulk request sits out short rate-limit windows ("Too many
			// concurrent requests", the per-minute limit) instead of failing.
			retry = true
			if c.retryBase >= time.Second { // tests shorten retryBase to skip the waits
				delay = ae.RetryAfter
			}
		}
		if !retry || attempt >= c.retries {
			return nil, 0, lastErr
		}
		c.log.Debug("weather: retrying request", "attempt", attempt+1, "delay", delay, "err", err)
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, 0, err
		}
		// A retried request is a new call for the server.
		if err := c.reserve(ctx, r.cost, r.pace); err != nil {
			return nil, 0, lastErr
		}
	}
}

func (c *Client) attempt(ctx context.Context, r request) (body []byte, cost float64, retry bool, err error) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = c.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, 0, false, fmt.Errorf("weather: %w", err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")

	c.mu.Lock()
	ce, haveCache := c.cache[r.url]
	c.mu.Unlock()
	if haveCache {
		if ce.etag != "" {
			req.Header.Set("If-None-Match", ce.etag)
		}
		if ce.lastMod != "" {
			req.Header.Set("If-Modified-Since", ce.lastMod)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, true, fmt.Errorf("weather: %w", scrubURL(err))
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	cost = c.account(r, resp.Header)
	if rerr != nil {
		return nil, cost, true, fmt.Errorf("weather: reading response: %w", rerr)
	}
	if len(data) > maxBody {
		return nil, cost, false, errors.New("weather: response too large")
	}

	switch {
	case resp.StatusCode == http.StatusNotModified && haveCache:
		return ce.body, cost, false, nil
	case resp.StatusCode == http.StatusOK:
		etag, lastMod := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		if (etag != "" || lastMod != "") && len(data) <= maxCacheBody {
			c.storeCache(r.url, cached{etag: etag, lastMod: lastMod, body: data, stored: c.now()})
		}
		return data, cost, false, nil
	}
	ae := &APIError{Status: resp.StatusCode, Reason: errorReason(data)}
	if resp.StatusCode == http.StatusTooManyRequests {
		ae.RetryAfter = retryAfter(resp.Header, ae.Reason, c.now())
		return nil, cost, false, ae
	}
	return nil, cost, resp.StatusCode >= 500, ae
}

// account reconciles the reserved estimate with the server-reported cost and
// remembers the server's rate window. It returns the cost charged.
func (c *Client) account(r request, h http.Header) float64 {
	cost := r.cost
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.cost > 0 {
		if v, err := strconv.ParseFloat(h.Get("X-RateLimit-Cost"), 64); err == nil && v > 0 && v < 1e6 {
			c.addUsageLocked(c.now(), v-r.cost)
			cost = v
		}
		rem, err1 := strconv.ParseFloat(h.Get("X-RateLimit-Remaining"), 64)
		reset, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
		if err1 == nil && err2 == nil && reset > 0 {
			c.srvRemaining = rem
			c.srvReset = time.Unix(reset, 0)
		}
	}
	return cost
}

func (c *Client) storeCache(url string, e cached) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.cache[url]; !ok && len(c.cache) >= maxCacheEntries {
		var oldest string
		var ot time.Time
		for k, v := range c.cache {
			if oldest == "" || v.stored.Before(ot) {
				oldest, ot = k, v.stored
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[url] = e
}

// scrubURL removes the request URL (which contains the station coordinates)
// from net/http errors so it does not end up in logs and status messages.
func scrubURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// errorReason extracts Open-Meteo's {"error":true,"reason":"..."} message.
func errorReason(body []byte) string {
	var e struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Reason != "" {
			return truncate(e.Reason, 300)
		}
		if e.Message != "" {
			return truncate(e.Message, 300)
		}
	}
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(s, "<") { // HTML error page from a proxy
		return ""
	}
	return truncate(s, 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// retryAfter derives a back-off from a 429 answer: the Retry-After header,
// else the wording of Open-Meteo's reason ("Minutely/Hourly/Daily API request
// limit exceeded...", "Too many concurrent requests"), else the rate-window
// reset header.
func retryAfter(h http.Header, reason string, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return clampDur(time.Duration(secs)*time.Second, time.Second, 24*time.Hour)
		}
		if t, err := http.ParseTime(v); err == nil {
			return clampDur(t.Sub(now), time.Second, 24*time.Hour)
		}
	}
	lr := strings.ToLower(reason)
	switch {
	case strings.Contains(lr, "daily") || strings.Contains(lr, "tomorrow"):
		next := now.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + 5*time.Minute)
		return next.Sub(now)
	case strings.Contains(lr, "hourly") || strings.Contains(lr, "next hour"):
		next := now.Truncate(time.Hour).Add(time.Hour + time.Minute)
		return next.Sub(now)
	case strings.Contains(lr, "concurrent"):
		// Not a quota: earlier requests of ours are still being processed
		// (typically after a timeout on the slow archive back-end).
		return 30 * time.Second
	}
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
		if d := time.Unix(reset, 0).Sub(now); d > 0 {
			return clampDur(d+time.Second, time.Second, time.Hour)
		}
	}
	if strings.Contains(lr, "minute") {
		return 61 * time.Second
	}
	return 5 * time.Minute
}

func clampDur(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// validCoord reports whether lat/lon are finite and in range.
func validCoord(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lon) &&
		lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// ErrBadCoordinate is returned for out-of-range or non-finite coordinates.
var ErrBadCoordinate = errors.New("weather: invalid coordinate")

// fmtCoord renders a coordinate with 4 decimals (~11 m): far finer than any
// weather model grid, and it keeps URLs stable for a fixed station.
func fmtCoord(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
