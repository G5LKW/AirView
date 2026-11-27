package uplink

import (
	"time"

	"airview/internal/avf"
)

// rec is one seq-carrying record held by the uplink: the record type, the
// body after the seq varint, and the host time of its oldest content (data
// age).
type rec struct {
	typ    avf.RecordType
	late   bool // backfill: frames carry the late bit when sent
	hostNs int64
	body   []byte
	frames int // FRAMES: number of frames (status counters)
	// rxNo / epoch the record refers to (RECEIVER, CLOCK_EVENT: the new
	// epoch, FRAMES, CLOCK_PAIR); oldEpoch is CLOCK_EVENT's previous epoch.
	rxNo     int
	epoch    uint32
	oldEpoch uint32

	seq     uint64 // seq in the current seq space once sent (0 = unsent)
	origSeq uint64 // seq it had when it was last sent (BACKFILL markers)

	marked bool // FRAMES body already re-encoded with the late bit

	fromSpool bool
	spoolEnd  spoolPos
}

func (r *rec) size() int { return len(r.body) + 48 }

// backfillable reports whether a record type may be replayed late (AVF
// §5.14): FRAMES, DECODED and MINUTE_STATS. Old RECEIVER / CLOCK_EVENT
// records are superseded by the fresh ones of each session.
func backfillable(t avf.RecordType) bool {
	return t == avf.TFrames || t == avf.TDecoded || t == avf.TMinuteStats
}

// deque is a FIFO of records with byte accounting.
type deque struct {
	items []*rec
	head  int
	bytes int
}

func (d *deque) len() int { return len(d.items) - d.head }

func (d *deque) front() *rec {
	if d.len() == 0 {
		return nil
	}
	return d.items[d.head]
}

func (d *deque) push(r *rec) {
	d.items = append(d.items, r)
	d.bytes += r.size()
}

func (d *deque) popFront() *rec {
	if d.len() == 0 {
		return nil
	}
	r := d.items[d.head]
	d.items[d.head] = nil
	d.head++
	d.bytes -= r.size()
	if d.head > 1024 && d.head*2 > len(d.items) {
		n := copy(d.items, d.items[d.head:])
		clear(d.items[n:])
		d.items = d.items[:n]
		d.head = 0
	}
	return r
}

// pushFront puts rs (in order) before the current front.
func (d *deque) pushFront(rs []*rec) {
	if len(rs) == 0 {
		return
	}
	cur := d.items[d.head:]
	n := make([]*rec, 0, len(rs)+len(cur))
	n = append(n, rs...)
	n = append(n, cur...)
	d.items, d.head = n, 0
	for _, r := range rs {
		d.bytes += r.size()
	}
}

// take removes and returns every record.
func (d *deque) take() []*rec {
	out := d.items[d.head:]
	d.items, d.head, d.bytes = nil, 0, 0
	return out
}

// queue holds the uplink's outgoing data (AVF §5.2, §9.1):
//
//   - inflight: records sent in the current seq space and not yet ACKed;
//   - live: records not sent yet, in order;
//   - back: records waiting to be backfilled (unacknowledged data older
//     than the resume window at a reconnect);
//   - sp: the optional disk spool (written only while disconnected).
//
// inflight + live form the RAM ring, bounded by RingAge and RingBytes.
type queue struct {
	o         *Options
	connected bool
	inflight  deque
	live      deque
	back      deque
	sp        *spool
	newest    int64 // newest data host time seen
	lastSeq   uint64

	dropped, droppedBytes, spilled uint64
}

func newQueue(o *Options, sp *spool) *queue { return &queue{o: o, sp: sp} }

// push appends a new live record.
func (q *queue) push(r *rec) {
	// only received data advances the ring clock (MINUTE_STATS carry the
	// end of their minute, RECEIVER the wall clock)
	if (r.typ == avf.TFrames || r.typ == avf.TDecoded) && r.hostNs > q.newest {
		q.newest = r.hostNs
	}
	q.live.push(r)
	q.enforce()
}

func (q *queue) ringBytes() int { return q.inflight.bytes + q.live.bytes }

func (q *queue) oldestRing() *rec {
	if r := q.inflight.front(); r != nil {
		return r
	}
	return q.live.front()
}

// enforce applies the RAM limits.
//
// While disconnected, the inflight records (sent, not yet acknowledged) are
// kept as long as they fit in RingBytes on their own, whatever their age:
// only the next WELCOME tells which of them the server holds. Spilling them
// to the spool would drop their seq, and the server would get them a
// second time as backfill after every outage longer than RingAge (C3).
func (q *queue) enforce() {
	age := q.o.RingAge.Nanoseconds()
	for {
		keepInflight := !q.connected && q.inflight.bytes <= q.o.RingBytes
		r := q.oldestRing()
		if keepInflight {
			r = q.live.front()
		}
		if r == nil || (q.ringBytes() <= q.o.RingBytes && q.newest-r.hostNs <= age) {
			break
		}
		if !keepInflight && q.inflight.len() > 0 {
			q.inflight.popFront()
		} else {
			q.live.popFront()
		}
		q.evict(r)
	}
	for q.back.bytes > q.o.BackfillBytes {
		q.evict(q.back.popFront())
	}
}

// evict disposes of a record that no longer fits in memory: to the spool
// while disconnected, else it is lost.
func (q *queue) evict(r *rec) {
	switch {
	case r.fromSpool:
		// still on disk; it is read again after the next reconnect
	case !q.connected && q.sp != nil && backfillable(r.typ):
		r.late = true
		if q.sp.Append(r) == nil {
			q.spilled++
			return
		}
		fallthrough
	default:
		q.dropped++
		q.droppedBytes += uint64(len(r.body))
	}
}

// connect prepares the queue for a new session after WELCOME. ackSeq is
// the server's watermark of this seq space; trusted is false when it cannot
// belong to this seq space (it is then ignored). Every record the server
// does not hold is renumbered from ackSeq+1 when sent: records younger than
// the resume window are resent live, older ones become backfill.
func (q *queue) connect(ackSeq uint64, trusted bool, now time.Time) {
	q.connected = true
	if q.sp != nil {
		_ = q.sp.CloseWriter()
	}
	var toLive, toBack []*rec
	for _, r := range q.inflight.take() {
		if trusted && r.seq != 0 && r.seq <= ackSeq {
			// the server holds it (spool positions are acknowledged in
			// order, so a later position also covers skipped records)
			if r.fromSpool {
				q.sp.Ack(r.spoolEnd, now)
			}
			continue
		}
		r.origSeq, r.seq = r.seq, 0
		switch {
		case r.fromSpool: // re-read from disk after the rewind
		case r.late:
			toBack = append(toBack, r)
		default:
			toLive = append(toLive, r)
		}
	}
	if q.sp != nil {
		q.sp.Rewind() // unacknowledged spool records are read again
	}
	q.back.pushFront(toBack)
	q.live.pushFront(toLive)
	cut := now.UnixNano() - q.o.ResumeWindow.Nanoseconds()
	keep := q.live.take()
	for _, r := range keep {
		switch {
		case r.hostNs >= cut:
			q.live.push(r)
		case backfillable(r.typ):
			r.late = true
			q.back.push(r)
		}
	}
	q.enforce()
}

// disconnect marks the end of a session. Inflight records keep their seq
// so the next WELCOME.ackSeq can be matched against them.
func (q *queue) disconnect() { q.connected = false }

// resetSeqs forgets every seq (a new seq space starts).
func (q *queue) resetSeqs() {
	for _, r := range q.inflight.items[q.inflight.head:] {
		r.seq = 0
	}
	q.lastSeq = 0
}

// sent records that r went out with seq.
func (q *queue) sent(r *rec, seq uint64) {
	r.seq = seq
	if seq > q.lastSeq {
		q.lastSeq = seq
	}
	q.inflight.push(r)
}

// ack trims every inflight record up to seq.
func (q *queue) ack(seq uint64, now time.Time) {
	for r := q.inflight.front(); r != nil && r.seq <= seq; r = q.inflight.front() {
		q.inflight.popFront()
		if r.fromSpool {
			q.sp.Ack(r.spoolEnd, now)
		}
	}
}

// nextBackfill returns the next record to backfill: spool first (oldest
// data), then the in-memory backfill queue.
func (q *queue) nextBackfill() *rec {
	if q.sp != nil {
		if r, ok := q.sp.Next(); ok {
			return r
		}
	}
	return q.back.popFront()
}

// hasBackfill reports whether backfill data is waiting.
func (q *queue) hasBackfill() bool {
	return q.back.len() > 0 || (q.sp != nil && !q.sp.Empty())
}

// close spills everything unsent to the spool (when there is one) so a
// restart can backfill it, and closes the spool. Records already sent and
// not yet acknowledged (inflight; usually the last second or so) are not
// spilled: the server most likely holds them, and a backfilled copy under
// a new seq would be counted twice (C3). Spool records among them stay on
// disk and are read again.
func (q *queue) close(now time.Time) {
	if q.sp == nil {
		return
	}
	q.connected = false
	for _, r := range q.inflight.take() {
		if r.seq == 0 && !r.fromSpool && backfillable(r.typ) { // never actually sent
			r.late = true
			if q.sp.Append(r) == nil {
				q.spilled++
			}
		}
	}
	for _, d := range []*deque{&q.live, &q.back} {
		for _, r := range d.take() {
			if !r.fromSpool && backfillable(r.typ) {
				r.late = true
				if q.sp.Append(r) == nil {
					q.spilled++
				}
			}
		}
	}
	_ = q.sp.Close(now)
}
