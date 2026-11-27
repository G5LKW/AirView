package avf

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

// Sequence numbers, ACK and resume (AVF.md §5.2).
//
// Every seq-carrying record of a client's seqEpoch has a seq exactly one
// above the previous one (the first is 1). The server keeps a watermark per
// (feederId, seqEpoch) — the highest seq processed without gaps — drops
// duplicates at or below it, treats a gap inside a connection as a protocol
// error, ACKs it at least once per second and reports it as WELCOME.ackSeq
// when the same seqEpoch reconnects.

// NewSeqEpoch returns a fresh random seqEpoch (16 lower-case hex digits).
func NewSeqEpoch() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("avf: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ValidSeqEpoch reports whether s is 16 lower-case hex digits.
func ValidSeqEpoch(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- server

// Sequencer applies the server's seq rule to one session. Accept is called
// from the session's reader; Watermark may be read concurrently (for ACKs).
type Sequencer struct {
	wm atomic.Uint64
}

// NewSequencer starts at watermark start (WELCOME.ackSeq).
func NewSequencer(start uint64) *Sequencer {
	s := &Sequencer{}
	s.wm.Store(start)
	return s
}

// Accept checks the seq of a received record. It returns true when the
// record is new and must be processed (the watermark advances), false for
// a duplicate (seq ≤ watermark, to be dropped), and a protocol error for a
// gap (seq > watermark+1).
func (s *Sequencer) Accept(seq uint64) (bool, error) {
	wm := s.wm.Load()
	switch {
	case seq <= wm:
		return false, nil
	case seq == wm+1:
		s.wm.Store(seq)
		return true, nil
	}
	return false, protoErr("seq gap: got %d after %d", seq, wm)
}

// Watermark returns the highest seq processed without gaps.
func (s *Sequencer) Watermark() uint64 { return s.wm.Load() }

// WatermarkStore keeps watermarks per (feederId, seqEpoch) across sessions
// for at least WatermarkTTL after a session ends. It is safe for concurrent
// use. At most MaxEpochsPerFeeder inactive epochs are kept per feeder (the
// oldest are evicted first).
type WatermarkStore struct {
	// TTL is the retention after a session ends (default WatermarkTTL).
	TTL time.Duration
	// MaxEpochsPerFeeder bounds memory per feeder (default 8).
	MaxEpochsPerFeeder int

	mu sync.Mutex
	m  map[string]map[string]*wmEntry
}

type wmEntry struct {
	seq    uint64
	active int
	ended  time.Time
}

// Begin marks the epoch active (a session starts) and returns its stored
// watermark (0 for an unknown epoch), to be sent as WELCOME.ackSeq.
func (w *WatermarkStore) Begin(feederID, seqEpoch string) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]map[string]*wmEntry{}
	}
	f := w.m[feederID]
	if f == nil {
		f = map[string]*wmEntry{}
		w.m[feederID] = f
	}
	e := f[seqEpoch]
	if e == nil {
		e = &wmEntry{}
		f[seqEpoch] = e
	}
	e.active++
	return e.seq
}

// Update stores the current watermark of an active epoch (call it on every
// ACK, or at least when the session ends).
func (w *WatermarkStore) Update(feederID, seqEpoch string, seq uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.m[feederID][seqEpoch]; e != nil && seq > e.seq {
		e.seq = seq
	}
}

// End stores the final watermark and starts the retention period.
func (w *WatermarkStore) End(feederID, seqEpoch string, seq uint64, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.m[feederID]
	e := f[seqEpoch]
	if e == nil {
		return
	}
	if seq > e.seq {
		e.seq = seq
	}
	if e.active > 0 {
		e.active--
	}
	e.ended = now
	max := w.MaxEpochsPerFeeder
	if max <= 0 {
		max = 8
	}
	for len(f) > max {
		var oldK string
		var oldT time.Time
		for k, v := range f {
			if v.active == 0 && (oldK == "" || v.ended.Before(oldT)) {
				oldK, oldT = k, v.ended
			}
		}
		if oldK == "" {
			break
		}
		delete(f, oldK)
	}
}

// Sweep removes inactive epochs whose retention expired. Call it every
// minute or so.
func (w *WatermarkStore) Sweep(now time.Time) {
	ttl := w.TTL
	if ttl <= 0 {
		ttl = WatermarkTTL
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for fk, f := range w.m {
		for k, e := range f {
			if e.active == 0 && now.Sub(e.ended) > ttl {
				delete(f, k)
			}
		}
		if len(f) == 0 {
			delete(w.m, fk)
		}
	}
}

// Len returns the number of stored epochs.
func (w *WatermarkStore) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, f := range w.m {
		n += len(f)
	}
	return n
}

// ---------------------------------------------------------------- client

// HeldRecord is a sent, not yet acknowledged seq-carrying record.
type HeldRecord struct {
	Seq     uint64
	Type    RecordType
	Payload []byte    // complete payload (starts with the seq)
	At      time.Time // when the data was produced (resume-window age)
}

// SendRing is the client's in-memory store of unacknowledged records of
// one seqEpoch. It assigns seqs, trims on ACK and plans the retransmission
// after a reconnect. It is safe for concurrent use.
type SendRing struct {
	// MaxBytes bounds the held payload bytes; the oldest records are
	// dropped beyond it (0 = unbounded).
	MaxBytes int

	mu      sync.Mutex
	epoch   string
	next    uint64 // last assigned seq
	q       []HeldRecord
	head    int
	bytes   int
	dropped uint64
}

// NewSendRing returns an empty ring for seqEpoch (a fresh one when "").
func NewSendRing(seqEpoch string, maxBytes int) *SendRing {
	if seqEpoch == "" {
		seqEpoch = NewSeqEpoch()
	}
	return &SendRing{epoch: seqEpoch, MaxBytes: maxBytes}
}

// Epoch returns the ring's seqEpoch.
func (r *SendRing) Epoch() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epoch
}

// NextSeq assigns the next seq.
func (r *SendRing) NextSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return r.next
}

// LastSeq returns the highest assigned seq.
func (r *SendRing) LastSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// Hold stores a copy of a sent record until it is acknowledged.
func (r *SendRing) Hold(t RecordType, seq uint64, payload []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := append([]byte(nil), payload...)
	r.q = append(r.q, HeldRecord{Seq: seq, Type: t, Payload: p, At: at})
	r.bytes += len(p)
	for r.MaxBytes > 0 && r.bytes > r.MaxBytes && r.head < len(r.q) {
		r.popLocked()
		r.dropped++
	}
	r.compactLocked()
}

func (r *SendRing) popLocked() {
	r.bytes -= len(r.q[r.head].Payload)
	r.q[r.head] = HeldRecord{}
	r.head++
}

func (r *SendRing) compactLocked() {
	if r.head > 0 && r.head >= len(r.q)/2 {
		n := copy(r.q, r.q[r.head:])
		clear(r.q[n:])
		r.q = r.q[:n]
		r.head = 0
	}
}

// Ack trims every held record with seq ≤ seq.
func (r *SendRing) Ack(seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.head < len(r.q) && r.q[r.head].Seq <= seq {
		r.popLocked()
	}
	r.compactLocked()
}

// Held returns the number of held records and their payload bytes.
func (r *SendRing) Held() (records, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.q) - r.head, r.bytes
}

// Dropped returns how many unacknowledged records were dropped for space.
func (r *SendRing) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// ResumePlan is the retransmission plan after a WELCOME.
type ResumePlan struct {
	// Replay are the held records to send unchanged, in order, before any
	// new data (their seqs continue ackSeq without a gap).
	Replay []HeldRecord
	// Stale are unacknowledged records that can no longer be replayed live
	// (older than the resume window, or the seq space cannot continue);
	// they MAY be re-encoded as backfill (new seqs, late bit set).
	Stale []HeldRecord
	// NewEpoch is set when this seqEpoch cannot continue: ackSeq is above
	// the highest seq sent, ackSeq+1 is no longer held, or the oldest
	// unacknowledged records left the resume window (replaying only the
	// younger ones would leave a gap). The client must close this
	// connection and reconnect with a fresh seqEpoch (Reset), sending
	// Stale as backfill there.
	NewEpoch bool
}

// Resume trims the ring to ackSeq and plans the retransmission at now with
// the given resume window (ResumeWindow when 0). On NewEpoch the ring's
// unacknowledged records are moved to Stale and the ring is emptied.
func (r *SendRing) Resume(ackSeq uint64, now time.Time, window time.Duration) ResumePlan {
	if window <= 0 {
		window = ResumeWindow
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var plan ResumePlan
	if ackSeq > r.next {
		plan.NewEpoch = true
		plan.Stale = r.takeAllLocked()
		return plan
	}
	for r.head < len(r.q) && r.q[r.head].Seq <= ackSeq {
		r.popLocked()
	}
	held := r.q[r.head:]
	if len(held) == 0 {
		if ackSeq < r.next { // records lost (dropped for space)
			plan.NewEpoch = true
		}
		r.compactLocked()
		return plan
	}
	cut := now.Add(-window)
	if held[0].Seq != ackSeq+1 || held[0].At.Before(cut) {
		plan.NewEpoch = true
		plan.Stale = r.takeAllLocked()
		return plan
	}
	plan.Replay = append([]HeldRecord(nil), held...)
	return plan
}

func (r *SendRing) takeAllLocked() []HeldRecord {
	out := append([]HeldRecord(nil), r.q[r.head:]...)
	clear(r.q)
	r.q, r.head, r.bytes = r.q[:0], 0, 0
	return out
}

// Reset starts a fresh seqEpoch (seq restarts at 1) and drops held records.
func (r *SendRing) Reset() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch = NewSeqEpoch()
	r.next = 0
	r.takeAllLocked()
	return r.epoch
}
