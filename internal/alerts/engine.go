package alerts

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/store"
)

// Options configure an Engine. Zero values get sensible defaults.
type Options struct {
	Store  *store.Store // optional: rule/watch-list persistence and the "log" action
	Logger *slog.Logger
	Client *http.Client // webhook client (default: 5 s timeout, no redirects)

	HistorySize        int           // recent alerts kept in memory (default 500)
	MaxStates          int           // per (rule, aircraft) de-dup entries (default 50000)
	StaleAfter         time.Duration // a match gap longer than this ends an episode (default 10 s)
	HighlightHold      time.Duration // highlight lingers after the last match (default 15 s)
	NewAirframeWindow  time.Duration // how long a newly seen airframe counts as "new" (default 15 min)
	MaxAlertsPerMinute int           // global storm guard (default 120)
	WebhookQueue       int           // pending webhook posts (default 64; overflow is dropped)
	WebhookPerMinute   int           // global webhook budget (default 30)
	WebhookTimeout     time.Duration // per POST (default 5 s)
	UserAgent          string        // default "AirView/<version>"

	FeedDownGrace     time.Duration // a feed must be down this long before feed_down (default 60 s)
	RangeCollapseFrac float64       // range_collapse when recent max < frac x baseline (default 0.35)
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HistorySize <= 0 {
		o.HistorySize = 500
	}
	if o.MaxStates <= 0 {
		o.MaxStates = 50000
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 10 * time.Second
	}
	if o.HighlightHold <= 0 {
		o.HighlightHold = 15 * time.Second
	}
	if o.NewAirframeWindow <= 0 {
		o.NewAirframeWindow = 15 * time.Minute
	}
	if o.MaxAlertsPerMinute <= 0 {
		o.MaxAlertsPerMinute = 120
	}
	if o.WebhookQueue <= 0 {
		o.WebhookQueue = 64
	}
	if o.WebhookPerMinute <= 0 {
		o.WebhookPerMinute = 30
	}
	if o.WebhookTimeout <= 0 {
		o.WebhookTimeout = 5 * time.Second
	}
	if o.UserAgent == "" {
		o.UserAgent = model.AppName + "/" + model.Version
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: o.WebhookTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.FeedDownGrace <= 0 {
		o.FeedDownGrace = 60 * time.Second
	}
	if o.RangeCollapseFrac <= 0 || o.RangeCollapseFrac >= 1 {
		o.RangeCollapseFrac = 0.35
	}
	return o
}

// Alert is one fired alert (SSE "alert" event, history, webhook payload).
type Alert struct {
	ID       int64    `json:"id"`
	T        int64    `json:"t"` // Unix ms
	RuleID   string   `json:"ruleId"`
	RuleName string   `json:"ruleName"`
	Severity string   `json:"severity"`
	Kind     string   `json:"kind"`            // "aircraft" | "system"
	Event    string   `json:"event,omitempty"` // system event kind
	EventKey string   `json:"eventKey,omitempty"`
	ICAO     string   `json:"icao,omitempty"`
	Callsign string   `json:"callsign,omitempty"`
	Reg      string   `json:"reg,omitempty"`
	Type     string   `json:"type,omitempty"`
	Operator string   `json:"operator,omitempty"`
	Squawk   string   `json:"squawk,omitempty"`
	Lat      *float64 `json:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty"`
	AltFt    *float64 `json:"altFt,omitempty"`
	DistKm   *float64 `json:"distKm,omitempty"`
	GSKt     *float64 `json:"gsKt,omitempty"`     // ground speed
	VRFpm    *float64 `json:"vrFpm,omitempty"`    // vertical rate, ft/min
	TrackDeg *float64 `json:"trackDeg,omitempty"` // true track
	BrgDeg   *float64 `json:"brgDeg,omitempty"`   // bearing from the station
	RSSI     *float64 `json:"rssi,omitempty"`     // dBFS
	Value    *float64 `json:"value,omitempty"`    // system event value (e.g. record km), in Unit
	// Unit is the unit of Value and RefValue: "km", "ft", "kt", "dBFS",
	// "msg/s", "s" or "" (a count).
	Unit     string   `json:"unit,omitempty"`
	RefValue *float64 `json:"refValue,omitempty"` // comparison value (range collapse: the typical range)
	Text     string   `json:"text"`
	// Auto is true when Text comes from the built-in default template (the
	// rule has no message of its own). Its altitude / distance / record value
	// are then in fixed server units (ft, km, the record key's unit), and a
	// client may rebuild the text from the structured fields in its own units.
	Auto      bool   `json:"auto,omitempty"`
	Notify    bool   `json:"notify"`
	Sound     bool   `json:"sound"`
	Highlight string `json:"highlight,omitempty"`
	Webhook   bool   `json:"webhook,omitempty"` // a webhook POST was queued
	Logged    bool   `json:"logged,omitempty"`
}

// Highlight is a map highlight for an aircraft currently matching a rule
// with a highlight colour.
type Highlight struct {
	ICAO     string `json:"icao"`
	Color    string `json:"color"`
	RuleID   string `json:"ruleId"`
	Severity string `json:"severity"`
	Until    int64  `json:"until"` // Unix ms
}

// SystemEvent is a non-aircraft condition fed to HandleSystem.
type SystemEvent struct {
	Kind     string          `json:"kind"`          // Sys* constant
	Key      string          `json:"key,omitempty"` // record key / feed ID / ...
	Text     string          `json:"text,omitempty"`
	Value    *float64        `json:"value,omitempty"`
	Unit     string          `json:"unit,omitempty"`     // unit of Value / Ref; "" = derived from a record key's suffix
	Ref      *float64        `json:"refValue,omitempty"` // comparison value (e.g. the typical range)
	Aircraft *model.Aircraft `json:"-"`                  // optional: aircraft conditions in system rules test this
	T        time.Time       `json:"-"`                  // zero = now
}

// Stats are engine counters.
type Stats struct {
	Rules            int     `json:"rules"`
	EnabledRules     int     `json:"enabledRules"`
	Evaluations      uint64  `json:"evaluations"`
	LastEvalMicros   int64   `json:"lastEvalMicros"`
	LastEvalNanos    int64   `json:"lastEvalNanos"` // same, in ns (the rule pass usually takes < 1 µs per aircraft)
	LastEvalAircraft int     `json:"lastEvalAircraft"`
	Fired            uint64  `json:"fired"`
	Suppressed       uint64  `json:"suppressed"` // cooldown / storm guard
	States           int     `json:"states"`
	WebhookSent      uint64  `json:"webhookSent"`
	WebhookFailed    uint64  `json:"webhookFailed"`
	WebhookDropped   uint64  `json:"webhookDropped"`
	LastWebhookError string  `json:"lastWebhookError,omitempty"`
	Subscribers      int     `json:"subscribers"`
	RangeBaselineKm  float64 `json:"rangeBaselineKm"`
	RangeRecentKm    float64 `json:"rangeRecentKm"`
}

type stateKey struct {
	rule string
	icao uint32
}

type fireState struct {
	lastMatch int64
	lastFire  int64
}

type hl struct {
	color, rule, severity string
	rank                  int
	until                 int64
}

type ruleSet struct {
	all      []*compiled // sorted by ID
	aircraft []*compiled // enabled aircraft rules
	system   []*compiled // enabled system rules
	byID     map[string]*compiled
}

func newRuleSet(rules []Rule) *ruleSet {
	rs := &ruleSet{byID: map[string]*compiled{}}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	for _, r := range rules {
		c := compileRule(r)
		rs.all = append(rs.all, c)
		rs.byID[r.ID] = c
		if !r.Enabled {
			continue
		}
		if c.system {
			rs.system = append(rs.system, c)
		} else {
			rs.aircraft = append(rs.aircraft, c)
		}
	}
	return rs
}

// Engine evaluates rules. Safe for concurrent use; Evaluate is meant to be
// called from one goroutine at about 1 Hz.
type Engine struct {
	opts Options
	log  *slog.Logger
	st   *store.Store

	rules atomic.Pointer[ruleSet]
	watch atomic.Pointer[watchSet]
	cfgMu sync.Mutex // serialises rule / watch-list changes

	mu         sync.Mutex // everything below
	states     map[stateKey]fireState
	sysStates  map[string]fireState
	ruleLast   map[string]int64
	newAF      map[uint32]int64
	highlights map[uint32]hl
	history    []Alert
	histNext   int
	histLen    int
	nextID     int64
	subs       map[int]chan Alert
	subSeq     int
	stormStart int64
	stormCount int
	lastSweep  int64
	view       view
	stateFull  bool
	evals      uint64
	lastEvalUs int64
	lastEvalNs int64
	lastEvalN  int
	fired      uint64
	suppressed uint64
	mon        statusMonitor
	webhookCh  chan hookJob
	hookStats  hookStats
	hookStart  int64
	hookCount  int

	noHooks atomic.Bool // webhooks switched off globally (SetWebhooks)
}

// SetWebhooks switches the webhook action on or off for all rules (the
// integrator's global "allow webhooks" setting; default on).
func (e *Engine) SetWebhooks(on bool) { e.noHooks.Store(!on) }

// New returns an engine with the built-in default rules (call Load to read
// persisted rules and watch-lists).
func New(opts Options) *Engine {
	opts = opts.withDefaults()
	e := &Engine{opts: opts, log: opts.Logger, st: opts.Store,
		states: map[stateKey]fireState{}, sysStates: map[string]fireState{}, ruleLast: map[string]int64{},
		newAF: map[uint32]int64{}, highlights: map[uint32]hl{}, history: make([]Alert, opts.HistorySize),
		subs: map[int]chan Alert{}, webhookCh: make(chan hookJob, opts.WebhookQueue)}
	e.rules.Store(newRuleSet(DefaultRules()))
	e.watch.Store(&watchSet{lists: map[string]map[uint32]watchEntry{}})
	return e
}

// Reset forgets all runtime state (factory reset): the in-memory alert
// history, per-aircraft / system / rule cooldowns, new-airframe memory,
// highlights, the status monitor and the counters. Rules, watch-lists,
// subscribers and the alert id sequence are kept; call Load afterwards to
// re-read the (reset) rules.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.states = map[stateKey]fireState{}
	e.sysStates = map[string]fireState{}
	e.ruleLast = map[string]int64{}
	e.newAF = map[uint32]int64{}
	e.highlights = map[uint32]hl{}
	e.history = make([]Alert, e.opts.HistorySize)
	e.histNext, e.histLen = 0, 0
	e.stormStart, e.stormCount = 0, 0
	e.lastSweep = 0
	e.stateFull = false
	e.evals, e.lastEvalUs, e.lastEvalNs, e.lastEvalN = 0, 0, 0, 0
	e.fired, e.suppressed = 0, 0
	e.mon = statusMonitor{}
	e.hookStats = hookStats{}
	e.hookStart, e.hookCount = 0, 0
}

func sevRank(s string) int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	}
	return 1
}

// Evaluate tests every enabled aircraft rule against the live picture.
// Allocation-free for aircraft that do not match (and for continuing
// matches); only firing allocates.
func (e *Engine) Evaluate(aircraft []model.Aircraft, now time.Time) {
	// ntp.Now: time.Now on Windows advances only once per timer tick
	// (0.5-15.6 ms), so a pass of a few µs would always read 0.
	start := ntp.Now()
	rs := e.rules.Load()
	ws := e.watch.Load()
	nowMs := now.UnixMilli()
	e.mu.Lock()
	defer e.mu.Unlock()
	v := &e.view
	*v = view{watch: ws}
	if len(rs.aircraft) > 0 {
		nowS := now.Unix()
		for i := range aircraft {
			a := &aircraft[i]
			v.set(a)
			v.nowS = nowS
			v.newAF = false
			if v.icaoOK && len(e.newAF) > 0 {
				if t, ok := e.newAF[v.icao]; ok && nowMs-t <= e.opts.NewAirframeWindow.Milliseconds() {
					v.newAF = true
				}
			}
			for _, c := range rs.aircraft {
				if !c.skip(v) && c.root.eval(v) {
					e.onMatch(c, v, nowMs)
				}
			}
		}
	}
	*v = view{}
	if nowMs-e.lastSweep >= 30_000 {
		e.sweep(rs, nowMs)
	}
	e.evals++
	el := max(ntp.Now().Sub(start), 0)
	e.lastEvalUs, e.lastEvalNs = el.Microseconds(), el.Nanoseconds()
	e.lastEvalN = len(aircraft)
}

func (e *Engine) onMatch(c *compiled, v *view, nowMs int64) {
	r := &c.rule
	if r.Actions.Highlight != "" && v.icaoOK {
		h, ok := e.highlights[v.icao]
		rank := sevRank(r.Severity)
		if !ok || h.until < nowMs || rank >= h.rank {
			h.color, h.rule, h.severity, h.rank = r.Actions.Highlight, r.ID, r.Severity, rank
		}
		h.until = nowMs + e.opts.HighlightHold.Milliseconds()
		e.highlights[v.icao] = h
	}
	key := stateKey{rule: r.ID, icao: v.icao}
	if !v.icaoOK {
		key.icao = 0xFFFFFFFF
	}
	st, ok := e.states[key]
	if ok && nowMs-st.lastMatch <= e.opts.StaleAfter.Milliseconds() {
		st.lastMatch = nowMs // same episode: de-duplicated
		e.states[key] = st
		return
	}
	if !ok && len(e.states) >= e.opts.MaxStates {
		if !e.stateFull {
			e.stateFull = true
			e.log.Warn("alerts: de-duplication table full; new matches are ignored until entries expire", "max", e.opts.MaxStates)
		}
		e.suppressed++
		return
	}
	st.lastMatch = nowMs
	if !e.allowFire(r, st.lastFire, nowMs) {
		e.states[key] = st
		e.suppressed++
		return
	}
	st.lastFire = nowMs
	e.states[key] = st
	e.fire(c, v, nil, nowMs)
}

// allowFire applies the per-aircraft cooldown, the rule cooldown and the global storm guard.
func (e *Engine) allowFire(r *Rule, lastFire, nowMs int64) bool {
	if lastFire != 0 && nowMs-lastFire < int64(r.CooldownSec)*1000 {
		return false
	}
	if r.RuleCooldownSec > 0 {
		if t, ok := e.ruleLast[r.ID]; ok && nowMs-t < int64(r.RuleCooldownSec)*1000 {
			return false
		}
	}
	if nowMs-e.stormStart >= 60_000 {
		e.stormStart, e.stormCount = nowMs, 0
	}
	if e.stormCount >= e.opts.MaxAlertsPerMinute {
		return false
	}
	e.stormCount++
	e.ruleLast[r.ID] = nowMs
	return true
}

// HandleSystem evaluates the enabled system rules for one event.
func (e *Engine) HandleSystem(ev SystemEvent) {
	if ev.T.IsZero() {
		ev.T = time.Now()
	}
	ev.Kind = strings.ToLower(strings.TrimSpace(ev.Kind))
	rs := e.rules.Load()
	nowMs := ev.T.UnixMilli()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handleSystemLocked(rs, &ev, nowMs)
}

func (e *Engine) handleSystemLocked(rs *ruleSet, ev *SystemEvent, nowMs int64) {
	if len(rs.system) == 0 {
		return
	}
	v := view{watch: e.watch.Load(), sys: ev, sysKind: strings.ToUpper(ev.Kind)}
	v.sysKey = v.sysKind
	if ev.Key != "" {
		v.sysKey += ":" + strings.ToUpper(ev.Key)
	}
	if ev.Aircraft != nil {
		v.set(ev.Aircraft)
		if t, ok := e.newAF[v.icao]; ok && v.icaoOK && nowMs-t <= e.opts.NewAirframeWindow.Milliseconds() {
			v.newAF = true
		}
	}
	for _, c := range rs.system {
		if c.skip(&v) || !c.root.eval(&v) {
			continue
		}
		key := c.rule.ID + "|" + v.sysKey
		st := e.sysStates[key]
		if !e.allowFire(&c.rule, st.lastFire, nowMs) {
			e.suppressed++
			continue
		}
		st.lastFire, st.lastMatch = nowMs, nowMs
		e.sysStates[key] = st
		e.fire(c, &v, ev, nowMs)
	}
}

// NoteNewAirframe marks icao as seen for the first time ever (call from the
// tracker when it inserts a new row into the aircraft table).
func (e *Engine) NoteNewAirframe(icao uint32, now time.Time) {
	e.mu.Lock()
	if len(e.newAF) < e.opts.MaxStates {
		e.newAF[icao] = now.UnixMilli()
	}
	e.mu.Unlock()
}

func (e *Engine) sweep(rs *ruleSet, nowMs int64) {
	e.lastSweep = nowMs
	stale := e.opts.StaleAfter.Milliseconds()
	for k, st := range e.states {
		c := rs.byID[k.rule]
		cool := int64(0)
		if c != nil {
			cool = int64(c.rule.CooldownSec) * 1000
		}
		if c == nil || (nowMs-st.lastMatch > stale && nowMs-st.lastFire >= cool) {
			delete(e.states, k)
		}
	}
	if len(e.states) < e.opts.MaxStates {
		e.stateFull = false
	}
	for k, st := range e.sysStates {
		if nowMs-st.lastFire > MaxCooldownSec*1000 {
			delete(e.sysStates, k)
		}
	}
	for k, h := range e.highlights {
		if h.until < nowMs || rs.byID[h.rule] == nil {
			delete(e.highlights, k)
		}
	}
	win := e.opts.NewAirframeWindow.Milliseconds()
	for k, t := range e.newAF {
		if nowMs-t > win {
			delete(e.newAF, k)
		}
	}
	for k, t := range e.ruleLast {
		if c := rs.byID[k]; c == nil || nowMs-t > int64(c.rule.RuleCooldownSec)*1000+60_000 {
			delete(e.ruleLast, k)
		}
	}
}

// fire builds and dispatches an alert (e.mu held).
func (e *Engine) fire(c *compiled, v *view, ev *SystemEvent, nowMs int64) {
	r := &c.rule
	al := Alert{T: nowMs, RuleID: r.ID, RuleName: r.Name, Severity: r.Severity, Kind: "aircraft",
		Notify: r.Actions.Notify, Sound: r.Actions.Sound, Highlight: r.Actions.Highlight}
	if ev != nil {
		al.Kind, al.Event, al.EventKey, al.Value = "system", ev.Kind, ev.Key, finitePtr(ev.Value)
		al.RefValue = finitePtr(ev.Ref)
		al.Unit = eventUnit(ev)
	}
	if a := v.ac; a != nil {
		al.ICAO = strings.ToUpper(strings.TrimSpace(a.ICAO))
		al.Callsign = strings.TrimSpace(a.Callsign)
		al.Reg, al.Type, al.Operator, al.Squawk = a.Registration, a.TypeCode, a.Operator, a.Squawk
		if v.hasPos {
			al.Lat, al.Lon = finitePtr(&v.lat), finitePtr(&v.lon)
		}
		if !math.IsNaN(v.alt) {
			al.AltFt = finitePtr(&v.alt)
		}
		al.DistKm = finitePtr(a.DistKm)
		al.setMotion(a)
	}
	al.Text = e.expand(r, &al, v, ev)
	al.Auto = r.Message == ""
	e.dispatch(&al)
}

// setMotion copies the aircraft's numeric values that a client formats in
// its own units.
func (al *Alert) setMotion(a *model.Aircraft) {
	al.GSKt, al.VRFpm, al.TrackDeg = finitePtr(a.GS), finitePtr(a.VertRate), finitePtr(a.Track)
	al.BrgDeg, al.RSSI = finitePtr(a.Bearing), finitePtr(a.RSSI)
}

// eventUnit is the unit of a system event's value: the event's own, else
// derived from a record key ("max_dist_km" -> "km"), else "km" for the
// range events.
func eventUnit(ev *SystemEvent) string {
	if ev.Unit != "" {
		return ev.Unit
	}
	switch ev.Kind {
	case SysRangeCollapse, SysRangeRestored:
		return "km"
	case SysRecord:
		return RecordUnit(ev.Key)
	}
	return ""
}

// RecordUnit maps a record key to the unit of its value, from the key's
// suffix: _km, _ft, _kt, _dbfs, _s, _rate (msg/s); "" for counts.
func RecordUnit(key string) string {
	k := strings.ToLower(key)
	switch {
	case strings.HasSuffix(k, "_km"):
		return "km"
	case strings.HasSuffix(k, "_ft"):
		return "ft"
	case strings.HasSuffix(k, "_kt"):
		return "kt"
	case strings.HasSuffix(k, "_dbfs"):
		return "dBFS"
	case strings.HasSuffix(k, "_s"):
		return "s"
	case strings.HasSuffix(k, "_rate"):
		return "msg/s"
	}
	return ""
}

func finitePtr(p *float64) *float64 {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	x := *p
	return &x
}

// dispatch records and distributes an alert (e.mu held).
func (e *Engine) dispatch(al *Alert) {
	e.nextID++
	al.ID = e.nextID
	e.fired++
	c := e.rules.Load().byID[al.RuleID]
	if c != nil && c.rule.Actions.Log && e.st != nil {
		if b, err := json.Marshal(al); err == nil {
			icao, _ := parseHexICAO(al.ICAO)
			e.st.AddEvents([]store.Event{{T: al.T / 1000, Kind: "alert", ICAO: icao, Detail: string(b)}})
			al.Logged = true
		}
	}
	if c != nil && c.rule.Actions.Webhook != "" && !e.noHooks.Load() {
		al.Webhook = e.enqueueWebhook(c.rule.Actions.Webhook, *al, al.T)
	}
	e.history[e.histNext] = *al
	e.histNext = (e.histNext + 1) % len(e.history)
	if e.histLen < len(e.history) {
		e.histLen++
	}
	for _, ch := range e.subs {
		select {
		case ch <- *al:
		default: // slow subscriber: drop
		}
	}
	e.log.Info("alert", "rule", al.RuleID, "severity", al.Severity, "icao", al.ICAO, "text", al.Text)
}

// Subscribe returns a channel receiving every new alert (buffered; alerts
// are dropped for a subscriber that does not keep up) and a cancel func.
func (e *Engine) Subscribe(buf int) (<-chan Alert, func()) {
	if buf <= 0 {
		buf = 32
	}
	ch := make(chan Alert, buf)
	e.mu.Lock()
	e.subSeq++
	id := e.subSeq
	e.subs[id] = ch
	e.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			e.mu.Lock()
			delete(e.subs, id)
			e.mu.Unlock()
		})
	}
}

// Recent returns up to limit alerts with ID > afterID, newest first.
func (e *Engine) Recent(limit int, afterID int64) []Alert {
	if limit <= 0 || limit > len(e.history) {
		limit = len(e.history)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Alert, 0, min(limit, e.histLen))
	for i := 0; i < e.histLen && len(out) < limit; i++ {
		idx := (e.histNext - 1 - i + len(e.history)) % len(e.history)
		if e.history[idx].ID <= afterID {
			break
		}
		out = append(out, e.history[idx])
	}
	return out
}

// Highlights returns the active map highlights, sorted by ICAO.
func (e *Engine) Highlights(now time.Time) []Highlight {
	nowMs := now.UnixMilli()
	e.mu.Lock()
	out := make([]Highlight, 0, len(e.highlights))
	for icao, h := range e.highlights {
		if h.until >= nowMs {
			out = append(out, Highlight{ICAO: hex6(icao), Color: h.color, RuleID: h.rule, Severity: h.severity, Until: h.until})
		}
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ICAO < out[j].ICAO })
	return out
}

func hex6(v uint32) string {
	s := strings.ToUpper(strconv.FormatUint(uint64(v&0xFFFFFF), 16))
	return strings.Repeat("0", 6-len(s)) + s
}

// Stats returns engine counters.
func (e *Engine) Stats() Stats {
	rs := e.rules.Load()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Stats{Rules: len(rs.all), EnabledRules: len(rs.aircraft) + len(rs.system), Evaluations: e.evals,
		LastEvalMicros: e.lastEvalUs, LastEvalNanos: e.lastEvalNs, LastEvalAircraft: e.lastEvalN, Fired: e.fired, Suppressed: e.suppressed,
		States: len(e.states), Subscribers: len(e.subs),
		WebhookSent: e.hookStats.sent, WebhookFailed: e.hookStats.failed, WebhookDropped: e.hookStats.dropped,
		LastWebhookError: e.hookStats.lastErr}
	s.RangeBaselineKm, s.RangeRecentKm = e.mon.rangeBaseline(), e.mon.recentMax()
	return s
}

// TestMatch is one aircraft a rule matches in a test run.
type TestMatch struct {
	ICAO     string `json:"icao"`
	Callsign string `json:"callsign,omitempty"`
	Reg      string `json:"reg,omitempty"`
	Type     string `json:"type,omitempty"`
	Text     string `json:"text"`
}

// TestRule validates r and returns the aircraft of the given picture it
// matches right now (no cooldowns, nothing fired). For a system rule, ev
// (optional) is the event to test with. At most 500 matches are returned.
func (e *Engine) TestRule(r Rule, aircraft []model.Aircraft, ev *SystemEvent, now time.Time) ([]TestMatch, error) {
	if err := r.Normalize(); err != nil {
		return nil, err
	}
	if r.ID == "" {
		r.ID = "test"
	}
	c := compileRule(r)
	nowMs := now.UnixMilli()
	ws := e.watch.Load()
	e.mu.Lock()
	newAF := make(map[uint32]bool)
	for k, t := range e.newAF {
		if nowMs-t <= e.opts.NewAirframeWindow.Milliseconds() {
			newAF[k] = true
		}
	}
	e.mu.Unlock()
	out := []TestMatch{}
	if c.system {
		if ev == nil {
			return out, nil
		}
		v := view{watch: ws, sys: ev, sysKind: strings.ToUpper(ev.Kind)}
		v.sysKey = v.sysKind
		if ev.Key != "" {
			v.sysKey += ":" + strings.ToUpper(ev.Key)
		}
		if ev.Aircraft != nil {
			v.set(ev.Aircraft)
		}
		if c.root.eval(&v) {
			al := Alert{RuleName: r.Name, Kind: "system", Event: ev.Kind, EventKey: ev.Key}
			out = append(out, TestMatch{ICAO: "", Text: e.expand(&c.rule, &al, &v, ev)})
		}
		return out, nil
	}
	for i := range aircraft {
		var v view
		v.watch = ws
		v.set(&aircraft[i])
		v.nowS = now.Unix()
		v.newAF = v.icaoOK && newAF[v.icao]
		if c.skip(&v) || !c.root.eval(&v) {
			continue
		}
		a := &aircraft[i]
		al := Alert{RuleName: r.Name, ICAO: a.ICAO, Callsign: strings.TrimSpace(a.Callsign), Reg: a.Registration,
			Type: a.TypeCode, Squawk: a.Squawk, Operator: a.Operator, DistKm: finitePtr(a.DistKm)}
		if !math.IsNaN(v.alt) {
			al.AltFt = finitePtr(&v.alt)
		}
		out = append(out, TestMatch{ICAO: a.ICAO, Callsign: al.Callsign, Reg: a.Registration, Type: a.TypeCode,
			Text: e.expand(&c.rule, &al, &v, nil)})
		if len(out) >= 500 {
			break
		}
	}
	return out, nil
}
