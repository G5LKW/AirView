package tracker

import (
	"airview/internal/model"
)

// Batched lane credits (SO-5, DESIGN §20.2).
//
// Lanes.Credit takes the station's shard mutex for every copy. At 1,000
// feeders the ingest workers credit several hundred thousand copies per
// second; each lock is a contended atomic on a cache line that moves
// between cores, and the lane's own state moves with it. A CreditBatch is a
// per-worker buffer: Credit / CreditChannel only copy the arguments (the
// Result is a plain value), and Flush applies the buffered copies shard by
// shard, taking each shard mutex once for all of its copies.
//
// The copies of one station keep their order (a stable counting sort by
// shard), so the lane sees exactly the sequence Lanes.Credit would have
// seen, only later: by at most one batch (CreditBatchCap copies or until the
// worker calls Flush). That delay is harmless as long as it stays well
// below a minute: a copy older than its lane's current minute is counted
// in its own minute (the late-minute path), exactly as a slow copy is.
// Workers flush at the end of every read (one network read holds the copies
// of one feeder, so a batch usually holds one station and costs one lock),
// and before they block.
//
// A CreditBatch is not safe for concurrent use: one per worker goroutine.
// It never allocates after NewCreditBatch.
//
// Measured (BenchmarkLaneCreditContention, 32 workers on a 24-core
// i9-13900K, ns per credit of wall time, medians of 3; "read" = copies of
// one feeder per network read, "affinity" = each feeder read by one worker
// with 10 % of credits to other stations):
//
//	                         256 shards (now)        16 shards (phase 0)
//	feeders / pattern / read direct  batched         direct  batched
//	1000 / affinity / 1        8.2     8.7             33.5    35.8
//	1000 / affinity / 16       6.8     7.4             24.9    21.7
//	1000 / affinity / 64       6.5     6.8             23.3    19.8
//	1000 / random   / 16       5.9     5.5             18.7    16.2
//	1000 / random   / 64       4.4     4.5             14.4    10.8
//	 100 / random   / 64       9.3     8.7
//
// With 16 shards batching saved 10-25 % at reads of 16+ copies. With the
// 256 shards of SO-5 the shard mutex is no longer where the time goes (the
// profile is dominated by cache misses on the lane's aircraft entries and
// by the shared heardBy sets of the aircraft): batching is within ±8 % of
// the direct call, slower by the cost of copying the Result when every
// read holds a single copy. Raising the shard count from 16 to 256 was
// the improvement (3.4x to 4x per credit under contention); CreditBatch
// is kept as the tool for hosts with many more cores than shards.

// CreditBatchCap is the number of copies a CreditBatch buffers before it
// flushes by itself.
const CreditBatchCap = 128

type batchCredit struct {
	r      Result
	sig    float64
	st     model.StationIdx
	ch     int16
	chOnly bool
}

// CreditBatch buffers lane credits of one worker (see above).
type CreditBatch struct {
	ls    *Lanes
	n     int
	mixed bool  // the batch spans more than one shard
	maxT  int64 // newest data time buffered
	ents  [CreditBatchCap]batchCredit
	order [CreditBatchCap]uint8
	cnt   [laneShards + 1]uint16
}

// NewCreditBatch returns an empty batch that credits to ls.
func (ls *Lanes) NewCreditBatch() *CreditBatch { return &CreditBatch{ls: ls} }

// Len is the number of buffered copies.
func (b *CreditBatch) Len() int { return b.n }

// Credit buffers one copy (Lanes.Credit semantics).
func (b *CreditBatch) Credit(st model.StationIdx, ch int, sig float64, r *Result) {
	if r == nil || r.Status == ResultNone || r.Status == ResultLate {
		return
	}
	b.add(st, ch, sig, r, false)
}

// CreditChannel buffers one diversity copy (Lanes.CreditChannel semantics).
func (b *CreditBatch) CreditChannel(st model.StationIdx, ch int, sig float64, r *Result) {
	if r == nil || r.Status == ResultNone || r.Status == ResultLate || ch < 0 || ch >= laneChannels {
		return
	}
	b.add(st, ch, sig, r, true)
}

func (b *CreditBatch) add(st model.StationIdx, ch int, sig float64, r *Result, chOnly bool) {
	if b.n == CreditBatchCap {
		b.Flush()
	}
	if ch < -1 || ch > 1<<14 {
		ch = -1 // out of range for every lane: no channel statistics
	}
	e := &b.ents[b.n]
	e.r, e.sig, e.st, e.ch, e.chOnly = *r, sig, st, int16(ch), chOnly
	if b.n > 0 && !b.mixed && b.ents[0].st%laneShards != st%laneShards {
		b.mixed = true
	}
	if r.TMs > b.maxT {
		b.maxT = r.TMs
	}
	b.n++
}

// Flush applies the buffered copies to their lanes.
func (b *CreditBatch) Flush() {
	n := b.n
	if n == 0 {
		return
	}
	ls := b.ls
	ls.observe(b.maxT)
	if !b.mixed {
		sh := &ls.shards[b.ents[0].st%laneShards]
		sh.mu.Lock()
		for i := 0; i < n; i++ {
			e := &b.ents[i]
			ls.laneLocked(sh, e.st, e.r.TMs).credit(ls, int(e.ch), e.sig, &e.r, e.chOnly)
		}
		sh.mu.Unlock()
	} else {
		// Stable counting sort of the entries by shard.
		clear(b.cnt[:])
		for i := 0; i < n; i++ {
			b.cnt[b.ents[i].st%laneShards+1]++
		}
		for s := 1; s <= laneShards; s++ {
			b.cnt[s] += b.cnt[s-1]
		}
		for i := 0; i < n; i++ {
			s := b.ents[i].st % laneShards
			b.order[b.cnt[s]] = uint8(i)
			b.cnt[s]++
		}
		for i := 0; i < n; {
			s := b.ents[b.order[i]].st % laneShards
			sh := &ls.shards[s]
			sh.mu.Lock()
			for ; i < n && b.ents[b.order[i]].st%laneShards == s; i++ {
				e := &b.ents[b.order[i]]
				ls.laneLocked(sh, e.st, e.r.TMs).credit(ls, int(e.ch), e.sig, &e.r, e.chOnly)
			}
			sh.mu.Unlock()
		}
	}
	for i := 0; i < n; i++ {
		b.ents[i].r.heard = nil // do not keep aircraft alive
	}
	b.n, b.mixed, b.maxT = 0, false, 0
}
