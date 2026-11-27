package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// Manager runs every configured uplink of one installation and is the
// uplink branch of the frame tee (DESIGN Â§1): internal/engine adds it to
// the sink fan-out and registers HandleClockEvent with feed.Manager.
//
// It implements model.FrameSink. Frames that must never leave the station
// are dropped here, whatever the configuration: MLAT results (magic
// timestamp or MLAT flag), frames of feeds that are not forwardable
// (MLATOnly, NoForward, demo, hub), and the uplink's own injected results.
type Manager struct {
	o  Options
	id *Identity

	mu      sync.Mutex
	byID    map[string]*Client
	clients atomic.Pointer[[]*Client]
	closed  bool

	// Beast outputs ("also feed others", beast.go)
	beastByID map[string]*beastClient
	beasts    atomic.Pointer[[]*beastClient]
}

// NewManager loads (or creates) the identity and returns an idle manager;
// call Apply with the configured uplinks.
func NewManager(o Options) (*Manager, error) {
	o.normalize()
	id := o.Identity
	if id == nil {
		var err error
		id, err = LoadOrCreateIdentity(o.StateDir)
		if err != nil {
			return nil, err
		}
	}
	m := &Manager{o: o, id: id, byID: map[string]*Client{}, beastByID: map[string]*beastClient{}}
	m.clients.Store(&[]*Client{})
	m.beasts.Store(&[]*beastClient{})
	return m, nil
}

// Identity returns the feeder identity (FeederID and UUID are public; the
// key is not).
func (m *Manager) Identity() *Identity { return m.id }

// connKey are the settings that need a new connection (and client) when
// they change.
func connKey(u model.Uplink) model.Uplink {
	return model.Uplink{ID: u.ID, Host: u.Host, Port: u.Port, ServerPin: u.ServerPin, EnrolToken: u.EnrolToken,
		SpoolMB: u.SpoolMB, ShareGNSS: u.ShareGNSS, ReturnMLAT: u.ReturnMLAT, Disabled: u.Disabled}
}

// Apply makes the running uplinks match ups (normalized copies of
// Config.Uplinks or FeederConfig uplinks): new ones start, removed ones
// stop gracefully (their unsent data goes to the spool), changed
// connection settings restart the uplink, other changes apply in place.
func (m *Manager) Apply(ups []model.Uplink) {
	ups = model.CloneUplinks(ups)
	_ = model.NormalizeUplinks(ups)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	want := map[string]model.Uplink{}
	for _, u := range ups {
		want[u.ID] = u
	}
	var stop []*Client
	for id, c := range m.byID {
		u, ok := want[id]
		c.mu.Lock()
		old := c.cfg
		c.mu.Unlock()
		if !ok || !connKey(u).Equal(connKey(old)) {
			stop = append(stop, c)
			delete(m.byID, id)
		}
	}
	// stop first so a restarted uplink can reopen its spool
	var wg sync.WaitGroup
	for _, c := range stop {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			c.close()
		}(c)
	}
	wg.Wait()
	for _, u := range ups {
		if c, ok := m.byID[u.ID]; ok {
			c.update(u)
			continue
		}
		c, err := newClient(&m.o, m.id, u)
		if err != nil {
			m.o.Logf("uplink %s: %v", u.ID, err)
			continue
		}
		m.byID[u.ID] = c
		c.start()
	}
	list := make([]*Client, 0, len(m.byID))
	for _, u := range ups {
		if c := m.byID[u.ID]; c != nil {
			list = append(list, c)
		}
	}
	m.clients.Store(&list)
}

// Active reports whether any uplink is configured (the tee may skip the
// manager otherwise).
func (m *Manager) Active() bool { return len(*m.clients.Load()) > 0 || len(*m.beasts.Load()) > 0 }

// HandleFrame implements model.FrameSink.
func (m *Manager) HandleFrame(f model.RawFrame) {
	for _, c := range *m.clients.Load() {
		c.handleFrame(&f)
	}
	for _, b := range *m.beasts.Load() {
		b.handleFrame(&f)
	}
}

// HandleUpdate implements model.FrameSink (DECODED, from feeds listed in an
// uplink's Inputs only; MLAT results are never forwarded).
func (m *Manager) HandleUpdate(u model.DecodedUpdate) {
	for _, c := range *m.clients.Load() {
		c.handleUpdate(&u)
	}
}

// HandleClockEvent receives feed.Manager.OnClockEvent callbacks (call it
// from the same goroutine order as the frames: before the first frame of
// the new epoch).
func (m *Manager) HandleClockEvent(feedID string, ev feed.ClockEvent) {
	for _, c := range *m.clients.Load() {
		c.handleClockEvent(feedID, ev)
	}
}

// HandleGNSS receives a receiver's own GNSS position (Radarcape type '5').
// It is forwarded only by uplinks with ShareGNSS, never for alias receivers.
func (m *Manager) HandleGNSS(feedID string, lat, lon, altM float64, altRef string, t time.Time) {
	for _, c := range *m.clients.Load() {
		c.handleGNSS(feedID, lat, lon, altM, altRef, t)
	}
}

// Status returns the status of every uplink in configuration order.
func (m *Manager) Status() []Status {
	list := *m.clients.Load()
	out := make([]Status, 0, len(list))
	for _, c := range list {
		out = append(out, c.Status())
	}
	return out
}

// Client returns the running uplink with the given ID.
func (m *Manager) Client(id string) *Client {
	for _, c := range *m.clients.Load() {
		if c.uid == id {
			return c
		}
	}
	return nil
}

// ErrNoUplink is returned for an unknown uplink ID.
var ErrNoUplink = errors.New("no such uplink")

// Claim requests a claim code from uplink id (first uplink when id is "").
func (m *Manager) Claim(ctx context.Context, id string, rxNo int) (string, time.Time, error) {
	c := m.pick(id)
	if c == nil {
		return "", time.Time{}, ErrNoUplink
	}
	return c.Claim(ctx, rxNo)
}

// Retry reconnects uplink id now (also leaves the stopped state).
func (m *Manager) Retry(id string) error {
	if id != "" {
		if b := m.beast(id); b != nil {
			b.Retry()
			return nil
		}
	}
	c := m.pick(id)
	if c == nil {
		return ErrNoUplink
	}
	c.Retry()
	return nil
}

func (m *Manager) pick(id string) *Client {
	list := *m.clients.Load()
	if id == "" && len(list) > 0 {
		return list[0]
	}
	return m.Client(id)
}

// Flush closes every open batch of every uplink now.
func (m *Manager) Flush() {
	for _, c := range *m.clients.Load() {
		c.Flush()
	}
}

// Close stops every uplink gracefully.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	list := *m.clients.Load()
	m.clients.Store(&[]*Client{})
	m.byID = map[string]*Client{}
	bl := *m.beasts.Load()
	m.beasts.Store(&[]*beastClient{})
	m.beastByID = map[string]*beastClient{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range list {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			c.close()
		}(c)
	}
	for _, b := range bl {
		wg.Add(1)
		go func(b *beastClient) {
			defer wg.Done()
			b.close()
		}(b)
	}
	wg.Wait()
}

// StatusResponse is the GET body of the status handler.
type StatusResponse struct {
	FeederID string   `json:"feederId"`
	UUID     string   `json:"uuid"`
	Uplinks  []Status `json:"uplinks"`
	// Beast lists the Beast outputs ("also feed others"); omitted when none.
	Beast []BeastStatus `json:"beast,omitempty"`
}

// ActionRequest is the POST body of the status handler.
type ActionRequest struct {
	Action string `json:"action"` // "claim" | "retry"
	ID     string `json:"id"`     // uplink ID ("" = first)
	RxNo   int    `json:"rxNo"`   // claim: receiver (0 = first)
}

// ClaimResponse answers a claim action. The code is a bearer secret for the
// owner's eyes only; the server must mount this handler behind admin auth.
type ClaimResponse struct {
	Code      string `json:"code"`
	ExpiresMs int64  `json:"expiresMs"`
}

// ServeHTTP serves the uplink status (GET) and the claim / retry actions
// (POST). Mount it behind the admin/owner authorisation (for example at
// /api/uplinks in airview, /api/status/uplinks on the feeder page).
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		_ = json.NewEncoder(w).Encode(StatusResponse{FeederID: m.id.FeederID, UUID: m.id.UUID, Uplinks: m.Status(), Beast: m.BeastStatus()})
	case http.MethodPost:
		var req ActionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			httpErr(w, http.StatusBadRequest, "bad request body")
			return
		}
		switch req.Action {
		case "claim":
			code, exp, err := m.Claim(r.Context(), req.ID, req.RxNo)
			if err != nil {
				st := http.StatusConflict
				if errors.Is(err, ErrNoUplink) {
					st = http.StatusNotFound
				}
				httpErr(w, st, err.Error())
				return
			}
			_ = json.NewEncoder(w).Encode(ClaimResponse{Code: code, ExpiresMs: exp.UnixMilli()})
		case "retry":
			if err := m.Retry(req.ID); err != nil {
				httpErr(w, http.StatusNotFound, err.Error())
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			httpErr(w, http.StatusBadRequest, "unknown action")
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		httpErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

var _ model.FrameSink = (*Manager)(nil)
