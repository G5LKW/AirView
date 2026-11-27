package uplink

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/avf"
	"airview/internal/feed"
	"airview/internal/model"
)

// Client runs one uplink (one model.Uplink) to one aggregator: it encodes
// the local frames into AVF/1 records, buffers them (RAM ring + optional
// disk spool), keeps the connection up and backfills after outages.
//
// All methods are safe for concurrent use. Handle* methods never block on
// the network.
type Client struct {
	o   *Options
	id  *Identity
	uid string // uplink ID (immutable)
	dir string // per-uplink state dir ("" = memory only)

	inputs atomic.Pointer[[]string]

	mu       sync.Mutex
	cfg      model.Uplink
	q        *queue
	spoolMB  int
	rx       map[string]*rxState
	rxByNo   map[int]*rxState
	st       persisted
	side     []*rec
	tmpl     *selFilter // filter settings applied to new receivers
	flow     flowState
	sess     *session
	seqEpoch string
	rotate   bool
	resume   string
	resumeAt time.Time
	claims   []claimWait
	closing  bool
	stat     statusState

	known sync.Map // feedID -> struct{} (receiver created)

	wake   chan struct{}
	retry  chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	ctr counters
}

type flowState struct {
	paused  bool
	batchMs int
	slow    float64
}

type claimWait struct {
	rxNo int
	ch   chan claimResult
}

type claimResult struct {
	code avf.ClaimCode
	err  error
}

// counters are the status counters (atomics: updated on hot paths).
type counters struct {
	framesIn, framesQueued, framesSent, framesLate atomic.Uint64
	mlatRefused, filtered, untimed                 atomic.Uint64
	recordsSent, bytesWire                         atomic.Uint64
	resultsIn, resultsInjected                     atomic.Uint64
	decodedIn                                      atomic.Uint64
	sessions                                       atomic.Uint64
}

// rxState is the encoder state of one local receiver.
type rxState struct {
	feedID string
	rxNo   int
	class  model.ClockClass
	epoch  uint32

	fb         frameBatch
	open       bool
	openedWall time.Time

	lastTicks  uint64
	lastHostNs int64
	nextPair   time.Time
	statusAt   time.Time

	stats minuteStats
	filt  *selFilter

	dec      []avf.DecodedUpdate
	decFirst int64
	decWall  time.Time

	info      ReceiverInfo
	infoAt    time.Time
	inputAt   time.Time
	connected bool
	gnssAt    time.Time

	feedback   *avf.Feedback
	framesSent uint64

	// classes remembers the clock class of recent epochs, so a RECEIVER
	// can announce an older epoch whose data is still queued.
	classes map[uint32]model.ClockClass
}

// noteClass records the class of epoch (bounded to the most recent few).
func (rx *rxState) noteClass(epoch uint32, class model.ClockClass) {
	if epoch == 0 {
		return
	}
	if rx.classes == nil {
		rx.classes = map[uint32]model.ClockClass{}
	}
	if len(rx.classes) >= 16 {
		clear(rx.classes)
	}
	rx.classes[epoch] = class
}

// newClient creates (but does not start) the client of cfg.
func newClient(o *Options, id *Identity, cfg model.Uplink) (*Client, error) {
	c := &Client{o: o, id: id, uid: cfg.ID, cfg: cfg.Clone(), rx: map[string]*rxState{}, rxByNo: map[int]*rxState{},
		tmpl: newSelFilter(), flow: flowState{batchMs: DefaultBatchMs, slow: 1}, seqEpoch: avf.NewSeqEpoch(),
		wake: make(chan struct{}, 1), retry: make(chan struct{}, 1), done: make(chan struct{})}
	if o.StateDir != "" && cfg.ID != "" {
		c.dir = filepath.Join(o.StateDir, "uplinks", cfg.ID)
	}
	c.st = loadState(c.dir)
	c.setInputs(cfg.Inputs)
	var sp *spool
	c.spoolMB = model.ResolveSpoolMB(cfg.SpoolMB, runtime.GOOS, o.SDRoot())
	if c.spoolMB > 0 && c.dir != "" {
		var err error
		sp, err = openSpool(filepath.Join(c.dir, "spool"), int64(c.spoolMB)<<20, o.SegmentBytes)
		if err != nil {
			o.Logf("uplink %s: spool disabled: %v", cfg.ID, err)
			sp = nil
		}
	}
	if sp == nil {
		c.spoolMB = 0
	}
	c.q = newQueue(o, sp)
	if sp != nil && sp.Bytes() > 0 {
		// Spooled data of an earlier run: announce its receivers (from the
		// persisted numbers) so the backfill can go out before the inputs
		// deliver anything.
		for feedID, n := range c.st.Rx {
			if n < 1 || n > 255 || c.rxByNo[n] != nil {
				continue
			}
			info := o.Source.Receiver(feedID)
			rx := &rxState{feedID: feedID, rxNo: n, filt: c.tmpl.clone(), info: info, infoAt: o.Now(),
				inputAt: o.Now().Add(-time.Hour), connected: info.Connected}
			c.rx[feedID], c.rxByNo[n] = rx, rx
			c.known.Store(feedID, struct{}{})
		}
	}
	c.stat.state = StateIdle
	c.stat.since = o.Now()
	return c, nil
}

func (c *Client) setInputs(in []string) {
	cp := slices.Clone(in)
	c.inputs.Store(&cp)
}

// start runs the connection loop and the housekeeping ticker.
func (c *Client) start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.housekeeping(ctx)
	go c.run(ctx)
}

// notify wakes the session writer.
func (c *Client) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Retry cuts a backoff short and leaves the "stopped" state.
func (c *Client) Retry() {
	select {
	case c.retry <- struct{}{}:
	default:
	}
}

// update applies a changed uplink configuration that does not need a new
// connection (inputs, location, privacy, timestamps, results).
func (c *Client) update(cfg model.Uplink) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tsChanged := c.cfg.Timestamps != cfg.Timestamps
	c.cfg = cfg.Clone()
	c.setInputs(cfg.Inputs)
	for _, rx := range c.rx {
		if tsChanged {
			c.closeBatchLocked(rx)
			rx.epoch, rx.class = 0, model.ClockNone
		}
		c.pushReceiverLocked(rx)
	}
}

// --- ingest ----------------------------------------------------------------

// accept applies the per-feed forwarding rule.
func (c *Client) accept(feedID string, decoded bool) bool {
	if IsInjectedFeed(feedID) || !c.o.Source.Forwardable(feedID) {
		return false
	}
	in := *c.inputs.Load()
	if len(in) == 0 {
		return !decoded
	}
	return slices.Contains(in, feedID)
}

// ensureRx makes sure the receiver of feedID exists; the source is asked
// for its description outside the lock.
func (c *Client) ensureRx(feedID string) {
	if _, ok := c.known.Load(feedID); ok {
		return
	}
	info := c.o.Source.Receiver(feedID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.rx[feedID]; ok {
		return
	}
	if c.closing {
		return
	}
	n := info.RxNo
	if n < 1 || n > 255 || c.rxByNo[n] != nil {
		used := map[int]bool{}
		for k := range c.rxByNo {
			used[k] = true
		}
		var ok bool
		before := len(c.st.Rx)
		n, ok = c.st.assignRx(feedID, used)
		if !ok {
			c.o.Logf("uplink %s: no free receiver number for feed %q", c.cfg.ID, feedID)
			return
		}
		if len(c.st.Rx) != before {
			if err := saveState(c.dir, c.st); err != nil {
				c.o.Logf("uplink %s: state: %v", c.cfg.ID, err)
			}
		}
	}
	now := c.o.Now()
	rx := &rxState{feedID: feedID, rxNo: n, filt: c.tmpl.clone(), info: info, infoAt: now, inputAt: now.Add(-time.Hour),
		connected: info.Connected}
	c.rx[feedID] = rx
	c.rxByNo[n] = rx
	c.known.Store(feedID, struct{}{})
	c.pushReceiverLocked(rx)
}

// handleFrame encodes one frame (hot path).
func (c *Client) handleFrame(f *model.RawFrame) {
	if f.MLAT || feed.IsMLATTimestamp(f.Clock12MHz) {
		c.ctr.mlatRefused.Add(1)
		return
	}
	if f.Late {
		// backfilled data of another link is never re-forwarded as live
		return
	}
	if f.Kind == model.FrameStatus {
		if c.accept(f.FeedID, false) {
			c.handleStatus(f)
		}
		return
	}
	if !wireKind(f.Kind, len(f.Data)) || !c.accept(f.FeedID, false) {
		return
	}
	c.ensureRx(f.FeedID)
	hostNs := f.HostNs
	if hostNs == 0 {
		hostNs = f.Recv.UnixNano()
	}
	c.ctr.framesIn.Add(1)

	c.mu.Lock()
	defer c.mu.Unlock()
	rx := c.rx[f.FeedID]
	if rx == nil || c.closing {
		return
	}
	if hostNs <= 0 {
		hostNs = c.o.Now().UnixNano()
	}
	// Never timed: MLAT stamps (above), zero timestamps, unknown class,
	// unconfirmed steps (feed marks those ClockNone), or Timestamps off.
	timed := c.cfg.Timestamps && f.ClockCls.Timed() && f.Ticks != 0 && f.Epoch != 0 &&
		!(f.ClockCls.NominalHz() == 12e6 && f.Clock12MHz == 0) // all-zero 48-bit Beast timestamp
	if timed && (f.Epoch != rx.epoch || f.ClockCls != rx.class) {
		reason := model.ClockEvRestart
		if rx.epoch != 0 && f.ClockCls != rx.class {
			reason = model.ClockEvModeChange
		}
		c.epochChangeLocked(rx, f.Epoch, f.ClockCls, reason, 0, hostNs)
	}
	if !timed {
		c.ctr.untimed.Add(1)
	}
	if ms, done := rx.stats.add(f.Data, f.Signal, hostNs, rx.rxNo); done {
		// The finished minute's frames go out before its MINUTE_STATS, so
		// the server has seen all of them when it merges the record
		// (selective-mode merge, avfsrv minutes.go).
		c.closeBatchLocked(rx)
		c.pushStatsLocked(rx, ms)
	}
	rx.filt.observe(hostNs)
	if rx.filt.active() && !rx.filt.pass(f.Data, hostNs) {
		c.ctr.filtered.Add(1)
		return
	}
	var ticks uint64
	if timed {
		ticks = f.Ticks
	}
	batchNs := int64(c.flow.batchMs) * 1e6
	if rx.open && (!rx.fb.canAppend(ticks, timed, len(f.Data)) || hostNs-rx.fb.firstHost >= batchNs || hostNs < rx.fb.firstHost-batchNs) {
		c.closeBatchLocked(rx)
	}
	if !rx.open {
		ep := rx.epoch
		if !c.cfg.Timestamps {
			ep = 0
		}
		rx.fb.reset(rx.rxNo, ep)
		rx.open = true
		rx.openedWall = time.Now()
	}
	rx.fb.add(timed, f.Channel, ticks, hostNs, sigByte(f.Signal), f.Data)
	if timed {
		rx.lastTicks, rx.lastHostNs = ticks, hostNs
	}
}

// handleStatus turns a Radarcape status frame into CLOCK_STATUS (1 Hz).
func (c *Client) handleStatus(f *model.RawFrame) {
	st, ok := feed.ParseBeastStatus(f.Data)
	if !ok {
		return
	}
	c.ensureRx(f.FeedID)
	c.mu.Lock()
	defer c.mu.Unlock()
	rx := c.rx[f.FeedID]
	if rx == nil || !c.cfg.Timestamps {
		return
	}
	now := c.o.Now()
	if now.Sub(rx.statusAt) < 900*time.Millisecond {
		return
	}
	rx.statusAt = now
	hostNs := f.HostNs
	if hostNs == 0 {
		hostNs = now.UnixNano()
	}
	raw := f.Data
	if len(raw) > avf.MaxClockStatusRaw {
		raw = raw[:avf.MaxClockStatusRaw]
	}
	p, err := avf.AppendClockStatus(nil, &avf.ClockStatus{RxNo: uint8(rx.rxNo), HostNs: hostNs, Settings: st.Settings,
		GPSStatus: st.GPS, PPSOffsetTicks: int64(st.PPSOffset), Raw: raw})
	if err != nil {
		return
	}
	body := stripSeq0(p)
	c.pushSideLocked(&rec{typ: avf.TClockStatus, hostNs: hostNs, body: body})
}

// handleClockEvent records a new receiver clock epoch (before the first
// frame of that epoch).
func (c *Client) handleClockEvent(feedID string, ev feed.ClockEvent) {
	if !c.accept(feedID, false) {
		return
	}
	c.ensureRx(feedID)
	c.mu.Lock()
	defer c.mu.Unlock()
	rx := c.rx[feedID]
	if rx == nil || !c.cfg.Timestamps || (ev.NewEpoch == rx.epoch && ev.NewClass == rx.class) {
		return
	}
	reason := ev.Reason
	if !reason.Valid() {
		reason = model.ClockEvRestart
	}
	c.epochChangeLocked(rx, ev.NewEpoch, ev.NewClass, reason, ev.SamplesLost, ev.HostNs)
}

func (c *Client) epochChangeLocked(rx *rxState, epoch uint32, class model.ClockClass, reason model.ClockEventReason, lost uint64, hostNs int64) {
	c.closeBatchLocked(rx)
	old := rx.epoch
	rx.epoch, rx.class = epoch, class
	rx.noteClass(epoch, class)
	rx.lastTicks = 0
	if hostNs == 0 {
		hostNs = c.o.Now().UnixNano()
	}
	if old != 0 {
		body, _ := avf.AppendJSON(nil, avf.ClockEvent{RxNo: rx.rxNo, OldEpoch: old, NewEpoch: epoch,
			Reason: reason, SamplesLost: int64(lost), HostNs: hostNs})
		c.q.push(&rec{typ: avf.TClockEvent, hostNs: hostNs, body: body, rxNo: rx.rxNo, epoch: epoch, oldEpoch: old})
	}
	c.pushReceiverLocked(rx)
}

// handleUpdate forwards an untimed decoded update (DECODED, map only).
func (c *Client) handleUpdate(u *model.DecodedUpdate) {
	if u.MLAT || u.Src != "" || u.Late || !c.accept(u.FeedID, true) {
		if u.MLAT {
			c.ctr.mlatRefused.Add(1)
		}
		return
	}
	c.ensureRx(u.FeedID)
	c.mu.Lock()
	defer c.mu.Unlock()
	rx := c.rx[u.FeedID]
	if rx == nil || c.closing {
		return
	}
	c.ctr.decodedIn.Add(1)
	t := u.Recv.UnixNano()
	if u.Recv.IsZero() {
		t = c.o.Now().UnixNano()
	}
	it := avf.DecodedUpdate{ICAO: avf.FormatICAO(u.ICAO&0xFFFFFF, u.ICAO&(1<<24) != 0), TMs: t / 1e6, CS: u.Callsign,
		Sq: u.Squawk, Lat: u.Lat, Lon: u.Lon, AltBaro: iptr(u.AltBaro), AltGeom: iptr(u.AltGeom), GS: u.GS, Trk: u.Track,
		VR: iptr(u.VertRate), Gnd: u.OnGround, Sig: u.Signal, N: u.Msgs}
	if len(rx.dec) == 0 {
		rx.decFirst, rx.decWall = t, time.Now()
	}
	rx.dec = append(rx.dec, it)
	if len(rx.dec) >= 200 || t-rx.decFirst >= 1e9 {
		c.flushDecodedLocked(rx)
	}
}

// handleGNSS forwards the receiver's own GNSS position (Radarcape type '5')
// when ShareGNSS is set and the receiver is not an alias; at most every 10 s.
func (c *Client) handleGNSS(feedID string, lat, lon, altM float64, altRef string, t time.Time) {
	if !c.accept(feedID, false) {
		return
	}
	c.ensureRx(feedID)
	c.mu.Lock()
	defer c.mu.Unlock()
	rx := c.rx[feedID]
	if rx == nil || !c.cfg.ShareGNSS || rx.info.AliasGNSS || t.Sub(rx.gnssAt) < 10*time.Second {
		return
	}
	rx.gnssAt = t
	if altRef == "" {
		altRef = model.AltRefHAE
	}
	body, _ := avf.AppendJSON(nil, avf.GNSSPos{RxNo: rx.rxNo, Lat: lat, Lon: lon, AltM: altM, AltRef: altRef, TMs: t.UnixMilli()})
	c.pushSideLocked(&rec{typ: avf.TGNSSPos, hostNs: t.UnixNano(), body: body})
}

// --- record builders (c.mu held) ---------------------------------------------

func (c *Client) closeBatchLocked(rx *rxState) {
	if !rx.open {
		return
	}
	rx.open = false
	n := rx.fb.n()
	if n == 0 {
		return
	}
	body, err := rx.fb.body()
	if err != nil {
		c.o.Logf("uplink %s: FRAMES: %v", c.uid, err)
		return
	}
	c.q.push(&rec{typ: avf.TFrames, hostNs: rx.fb.firstHost, body: body, frames: n, rxNo: rx.rxNo, epoch: rx.fb.r.EpochID})
	c.ctr.framesQueued.Add(uint64(n))
	c.notify()
}

func (c *Client) flushDecodedLocked(rx *rxState) {
	if len(rx.dec) == 0 {
		return
	}
	p, err := avf.AppendDecoded(nil, 0, uint8(rx.rxNo), rx.dec)
	clear(rx.dec)
	rx.dec = rx.dec[:0]
	if err != nil {
		return
	}
	body := stripSeq0(p)
	c.q.push(&rec{typ: avf.TDecoded, hostNs: rx.decFirst, body: body})
	c.notify()
}

func (c *Client) pushStatsLocked(rx *rxState, ms avf.MinuteStats) {
	ms.RxNo = rx.rxNo
	body, err := avf.AppendJSON(nil, ms)
	if err != nil {
		return
	}
	c.q.push(&rec{typ: avf.TMinuteStats, hostNs: (ms.Minute + 60) * 1e9, body: body})
	c.notify()
}

// receiverRecordLocked builds the RECEIVER record of rx from the
// configuration, announcing its current epoch and class.
func (c *Client) receiverRecordLocked(rx *rxState) *rec {
	return c.receiverRecordEpochLocked(rx, rx.epoch, rx.class)
}

// receiverRecordEpochLocked builds a RECEIVER record announcing epoch with
// class (used at the start of a session whose queued data still belongs to
// an older epoch: the queued CLOCK_EVENTs then lead to the current one).
func (c *Client) receiverRecordEpochLocked(rx *rxState, epoch uint32, class model.ClockClass) *rec {
	r := avf.Receiver{RxNo: rx.rxNo, Name: rx.info.Name, Decoder: rx.info.Decoder, Channels: max(1, rx.info.Channels),
		Antennas: rx.info.Antennas, Timestamps: c.cfg.Timestamps,
		Privacy: avf.Privacy{Pin: c.cfg.Pin, PublicStats: c.cfg.PublicStats, ShowName: c.cfg.ShowName}}
	if r.Channels > 15 {
		r.Channels = 15
	}
	if r.Privacy.Pin == "" {
		r.Privacy.Pin = model.PinHidden
	}
	cls := class
	if !c.cfg.Timestamps {
		cls = model.ClockNone
	}
	r.ClockClass = cls.String()
	r.NominalHz = cls.NominalHz()
	if cls.Timed() {
		r.EpochID = epoch
	}
	if c.cfg.SendLocation && rx.info.Location != nil {
		l := *rx.info.Location
		r.Location = &l
	}
	body, _ := avf.AppendJSON(nil, r)
	return &rec{typ: avf.TReceiver, hostNs: c.o.Now().UnixNano(), body: body, rxNo: rx.rxNo, epoch: r.EpochID}
}

func (c *Client) pushReceiverLocked(rx *rxState) {
	c.closeBatchLocked(rx)
	c.q.push(c.receiverRecordLocked(rx))
	c.notify()
}

// pushSideLocked queues an ephemeral record (CLOCK_PAIR, CLOCK_STATUS,
// INPUT_STATUS, GNSS_POS). They are not kept across sessions.
func (c *Client) pushSideLocked(r *rec) {
	if c.sess == nil {
		return
	}
	if len(c.side) >= 512 {
		c.side = c.side[1:]
	}
	c.side = append(c.side, r)
	c.notify()
}

// --- housekeeping ------------------------------------------------------------

// housekeeping closes stale batches and emits the periodic records.
func (c *Client) housekeeping(ctx context.Context) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var slow time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.flushStale(false)
		if now := time.Now(); now.Sub(slow) >= time.Second {
			slow = now
			c.periodic()
		}
	}
}

// flushStale closes FRAMES batches open for at least batchMs (all with
// force) and DECODED batches open for 1 s.
func (c *Client) flushStale(force bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	bd := time.Duration(c.flow.batchMs) * time.Millisecond
	for _, rx := range c.rx {
		if rx.open && (force || now.Sub(rx.openedWall) >= bd) {
			c.closeBatchLocked(rx)
		}
		if len(rx.dec) > 0 && (force || now.Sub(rx.decWall) >= time.Second) {
			c.flushDecodedLocked(rx)
		}
	}
}

// Flush closes every open batch immediately (tests and shutdown).
func (c *Client) Flush() { c.flushStale(true) }

// periodic runs once a second: CLOCK_PAIR every 5 s, receiver descriptions
// every 60 s, INPUT_STATUS on change and every 60 s, finished minutes.
func (c *Client) periodic() {
	now := c.o.Now()
	c.mu.Lock()
	var refresh []string
	for id, rx := range c.rx {
		if now.Sub(rx.infoAt) >= 60*time.Second {
			refresh = append(refresh, id)
		}
	}
	c.mu.Unlock()
	infos := make(map[string]ReceiverInfo, len(refresh))
	for _, id := range refresh {
		infos[id] = c.o.Source.Receiver(id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, rx := range c.rx {
		if inf, ok := infos[id]; ok {
			rx.infoAt = now
			changed := !sameInfo(rx.info, inf)
			rx.info = inf
			if changed {
				c.pushReceiverLocked(rx)
			}
		}
		if c.cfg.Timestamps && rx.class.Timed() && rx.lastTicks != 0 && !now.Before(rx.nextPair) {
			rx.nextPair = now.Add(5 * time.Second)
			body := stripSeq0(avf.AppendClockPair(nil, &avf.ClockPair{RxNo: uint8(rx.rxNo), EpochID: rx.epoch, Ticks: rx.lastTicks,
				HostUTCNs: rx.lastHostNs, HostErrEstNs: c.o.HostErrNs()}))
			c.pushSideLocked(&rec{typ: avf.TClockPair, hostNs: rx.lastHostNs, body: body, rxNo: rx.rxNo, epoch: rx.epoch})
		}
		if rx.info.Connected != rx.connected || now.Sub(rx.inputAt) >= 60*time.Second {
			rx.connected = rx.info.Connected
			rx.inputAt = now
			body, _ := avf.AppendJSON(nil, avf.InputStatus{RxNo: rx.rxNo, Connected: rx.info.Connected, FPS: rx.info.FPS,
				CRCErrors: rx.info.CRCErrors, Type: rx.info.InputType, ClockClass: rx.class.String(), SinceMs: rx.info.SinceMs})
			c.pushSideLocked(&rec{typ: avf.TInputStatus, hostNs: now.UnixNano(), body: body})
		}
		// a minute ends without traffic in the next one
		if rx.stats.minute != 0 && now.Unix() >= rx.stats.minute+65 && now.Unix() < rx.stats.minute+3600 {
			c.closeBatchLocked(rx)
			c.pushStatsLocked(rx, rx.stats.finish(rx.rxNo))
		}
	}
}

func sameInfo(a, b ReceiverInfo) bool {
	if a.Name != b.Name || a.Decoder != b.Decoder || a.Channels != b.Channels || a.AliasGNSS != b.AliasGNSS ||
		(a.Location == nil) != (b.Location == nil) || !slices.Equal(a.Antennas, b.Antennas) {
		return false
	}
	return a.Location == nil || *a.Location == *b.Location
}

// --- connection loop ---------------------------------------------------------

// stopCodes stop reconnecting until the configuration changes or Retry.
var stopCodes = map[string]bool{"proto_version": true, "enrol_required": true, "enrol_invalid": true, "revoked": true, "cloned_feeder": true}

// ServerError is a refusal or close by the server (WELCOME denied, GOODBYE).
type ServerError struct {
	Code        string
	Reason      string
	ReconnectIn int
	Welcome     bool // from WELCOME (else GOODBYE)
}

func (e *ServerError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("server: %s (%s)", e.Code, e.Reason)
	}
	return "server: " + e.Code
}

func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	bo := newBackoff(c.o.BackoffMin, c.o.BackoffMax)
	for ctx.Err() == nil {
		c.mu.Lock()
		disabled := c.cfg.Disabled
		c.mu.Unlock()
		if disabled {
			c.setState(StateDisabled, "", nil)
			select {
			case <-ctx.Done():
				return
			case <-c.retry:
				continue
			}
		}
		c.setState(StateConnecting, "", nil)
		up, err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if up >= backoffResetAfter {
			bo.reset()
		}
		delay := bo.next()
		var se *ServerError
		stop := false
		if errors.As(err, &se) {
			switch {
			case se.Code == "enrol_required" && c.dropEnrolled():
				delay, stop = c.o.BackoffMin, false // the token was withheld: send it again
			case stopCodes[se.Code]:
				stop = true
			case se.ReconnectIn > 0:
				delay = time.Duration(se.ReconnectIn) * time.Second
			case se.Welcome && (se.Code == "denied" || se.Code == "limit_stations"):
				delay = max(delay, 10*time.Minute)
			}
		}
		if stop {
			c.setState(StateStopped, "", err)
			c.o.Logf("uplink %s: stopped: %v", c.cfg.ID, err)
			select {
			case <-ctx.Done():
				return
			case <-c.retry:
				bo.reset()
				continue
			}
		}
		next := time.Now().Add(delay)
		c.setState(StateBackoff, "", err)
		c.mu.Lock()
		c.stat.nextRetry = next
		c.mu.Unlock()
		tm := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			tm.Stop()
			return
		case <-c.retry:
			tm.Stop()
		case <-tm.C:
		}
	}
}

// dropEnrolled forgets that the current server accepted our token (it asked
// for one); it reports whether a token is configured to send.
func (c *Client) dropEnrolled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	sid := c.stat.serverID
	if c.cfg.EnrolToken == "" || c.st.Enrolled[sid] == "" {
		return false
	}
	delete(c.st.Enrolled, sid)
	_ = saveState(c.dir, c.st)
	return true
}

func (c *Client) setState(s, detail string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stat.state != s {
		c.stat.state = s
		c.stat.since = c.o.Now()
	}
	if err != nil {
		c.stat.lastErr = err.Error()
		c.stat.lastErrAt = c.o.Now()
		var se *ServerError
		if errors.As(err, &se) {
			c.stat.errCode = se.Code
		} else {
			c.stat.errCode = ""
		}
	}
	if s != StateBackoff {
		c.stat.nextRetry = time.Time{}
	}
}

// close stops the client gracefully: open batches are queued, the session
// gets up to 2 s to have them acknowledged, then GOODBYE; whatever is left
// goes to the spool.
func (c *Client) close() {
	c.flushStale(true)
	c.mu.Lock()
	// The minute in progress is sent as it stands; the next run (or the
	// restarted uplink) sends the rest of that minute, and the server adds
	// the two records up (avfsrv minutes.go).
	for _, rx := range c.rx {
		if rx.stats.minute != 0 && rx.stats.frames > 0 {
			c.pushStatsLocked(rx, rx.stats.finish(rx.rxNo))
		}
	}
	c.closing = true
	for _, w := range c.claims {
		w.ch <- claimResult{err: errors.New("uplink closed")}
	}
	c.claims = nil
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		<-c.done
	}
	c.mu.Lock()
	c.q.close(c.o.Now())
	c.mu.Unlock()
}

// Claim asks the aggregator for a claim code for receiver rxNo (AVF §5.15).
// The code is a bearer secret: show it to the owner, never log it.
func (c *Client) Claim(ctx context.Context, rxNo int) (code string, expires time.Time, err error) {
	c.mu.Lock()
	s := c.sess
	if s == nil {
		c.mu.Unlock()
		return "", time.Time{}, errors.New("uplink is not connected")
	}
	if !slices.Contains(s.feats, avf.FeatClaim) {
		c.mu.Unlock()
		return "", time.Time{}, errors.New("server does not support claim codes")
	}
	if rxNo == 0 {
		for n := range c.rxByNo {
			if rxNo == 0 || n < rxNo {
				rxNo = n
			}
		}
		if rxNo == 0 {
			rxNo = 1
		}
	}
	ch := make(chan claimResult, 1)
	c.claims = append(c.claims, claimWait{rxNo: rxNo, ch: ch})
	conn := s.conn
	c.mu.Unlock()
	if err := conn.WriteJSON(avf.TClaim, &avf.Claim{RxNo: rxNo}); err != nil {
		c.mu.Lock()
		c.claims = slices.DeleteFunc(c.claims, func(w claimWait) bool { return w.ch == ch })
		c.mu.Unlock()
		return "", time.Time{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", time.Time{}, r.err
		}
		return r.code.Code, time.UnixMilli(r.code.ExpiresMs), nil
	case <-tctx.Done():
		c.mu.Lock()
		c.claims = slices.DeleteFunc(c.claims, func(w claimWait) bool { return w.ch == ch })
		c.mu.Unlock()
		return "", time.Time{}, tctx.Err()
	}
}
