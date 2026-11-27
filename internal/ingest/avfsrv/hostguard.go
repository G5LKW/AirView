package avfsrv

import (
	"math"
	"sync/atomic"
	"time"

	"airview/internal/model"
)

// Host-time plausibility (finding ROB-1, negative fake host offsets).
//
// AVF/1 frames carry the feeder's host time of reception; the ingest maps
// it into the server clock with the session's host-clock offset (feeder
// minus server, from PING/PONG) and groups copies by that corrected time.
// The clock filter's server-time check compares a GPS station's clock with
// the corrected time of the earliest copy of its groups. A feeder that
// answers PINGs honestly but stamps its frames earlier than it received
// them would make its copies the earliest ones and so pull the reference
// of that check (and its groups' times) towards its own claims.
//
// Physics bounds the claim: a frame reaches the server no earlier than half
// the round trip after its reception. With L = arrival − (host − offset)
// the implied one-way latency, the smallest L over a stretch of live
// traffic is RTT/2 plus the feeder's smallest batching delay (most batches
// contain a frame received just before the batch was sent). When even that
// minimum exceeds RTT/2 + slack, the host times are earlier than possible,
// and the offset handed to the ingest is reduced by the excess, so the
// corrected times move back to where the link allows them. Backfill and
// late frames are not used (they are legitimately old). Honest feeders are
// unaffected: their minimum latency stays far below the slack.
//
// Positive fake offsets (claims of later reception) never win the earliest
// copy, and the ingest clamps receive times to the arrival time.

const (
	hgBucket  = 10 * time.Second // bucket length of the minimum
	hgBuckets = 3                // buckets kept: the minimum covers 20-30 s
	hgMinN    = 20               // frames needed in a bucket
)

// hostGuard tracks the minimum implied latency per bucket. observe runs on
// the worker goroutine, hostOffset on the monitor goroutine.
type hostGuard struct {
	off    atomic.Int64 // raw ping offset (ns) used for L; set by the monitor
	offSet atomic.Bool
	id     [hgBuckets]atomic.Int64
	min    [hgBuckets]atomic.Int64
	n      [hgBuckets]atomic.Int64
	adj    atomic.Int64 // correction in force (ns, ≥ 0)
	warned atomic.Bool
}

// observe records the frames of one FRAMES record read at arrival time at.
func (h *hostGuard) observe(raw []model.RawFrame, at int64) {
	if !h.offSet.Load() || at == 0 {
		return
	}
	off := h.off.Load()
	m := int64(math.MaxInt64)
	k := int64(0)
	for i := range raw {
		f := &raw[i]
		if f.Late || f.HostNs == 0 {
			continue
		}
		k++
		if l := at - f.HostNs + off; l < m {
			m = l
		}
	}
	if k == 0 {
		return
	}
	id := at / int64(hgBucket)
	b := id % hgBuckets
	if h.id[b].Load() != id {
		h.min[b].Store(m)
		h.n[b].Store(k)
		h.id[b].Store(id)
		return
	}
	if m < h.min[b].Load() {
		h.min[b].Store(m)
	}
	h.n[b].Add(k)
}

// minLatency returns the smallest implied latency over the recent buckets
// (ok false without enough frames).
func (h *hostGuard) minLatency(now int64) (int64, bool) {
	cur := now / int64(hgBucket)
	m, ok := int64(math.MaxInt64), false
	for b := 0; b < hgBuckets; b++ {
		id := h.id[b].Load()
		if id < cur-hgBuckets+1 || id > cur || h.n[b].Load() < hgMinN {
			continue
		}
		ok = true
		m = min(m, h.min[b].Load())
	}
	return m, ok
}

// hostOffset returns the offset to hand to the ingest for the ping offset
// off and round trip rtt (monitor goroutine).
func (c *conn) hostOffset(off, rtt time.Duration) time.Duration {
	return c.hg.offset(c, off, rtt)
}

func (h *hostGuard) offset(c *conn, off, rtt time.Duration) time.Duration {
	h.off.Store(int64(off))
	h.offSet.Store(true)
	slack := c.s.o.HostLatencySlack
	if slack < 0 {
		h.adj.Store(0)
		return off
	}
	minL, ok := h.minLatency(c.s.o.Now().UnixNano())
	if !ok {
		return off - time.Duration(h.adj.Load()) // keep the last correction while there is no data
	}
	excess := minL - int64(rtt/2) - int64(slack)
	if excess <= 0 {
		h.adj.Store(0)
		return off
	}
	if h.adj.Swap(excess) == 0 {
		c.s.st.hostClamped.Add(1)
	}
	if !h.warned.Swap(true) {
		c.s.o.Logger.Warn("avfsrv: feeder host times claim frames older than the link allows; offset corrected",
			"feeder", c.feeder, "excess_ms", excess/1e6, "rtt_ms", rtt.Milliseconds())
	}
	return off - time.Duration(excess)
}
