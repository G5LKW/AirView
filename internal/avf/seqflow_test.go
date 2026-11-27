package avf

import (
	"testing"
	"time"
)

func TestSeqEpoch(t *testing.T) {
	a, b := NewSeqEpoch(), NewSeqEpoch()
	if !ValidSeqEpoch(a) || a == b {
		t.Fatal(a, b)
	}
	for _, s := range []string{"", "0123456789ABCDEF", "0123456789abcde", "0123456789abcdeg"} {
		if ValidSeqEpoch(s) {
			t.Error(s)
		}
	}
}

func TestWatermarkStore(t *testing.T) {
	var w WatermarkStore
	now := time.Unix(1000, 0)
	if w.Begin("f1", "e1") != 0 {
		t.Fatal("unknown epoch must start at 0")
	}
	w.Update("f1", "e1", 10)
	w.End("f1", "e1", 12, now)
	if w.Begin("f1", "e1") != 12 {
		t.Fatal("watermark lost")
	}
	w.End("f1", "e1", 12, now)
	w.Sweep(now.Add(9 * time.Minute))
	if w.Len() != 1 {
		t.Fatal("swept too early")
	}
	w.Sweep(now.Add(11 * time.Minute))
	if w.Len() != 0 {
		t.Fatal("not swept")
	}
	// Active epochs are never swept; per-feeder bound.
	w.Begin("f2", "active")
	for i := 0; i < 20; i++ {
		e := string(rune('a' + i))
		w.Begin("f2", e)
		w.End("f2", e, 1, now.Add(time.Duration(i)*time.Second))
	}
	if w.Len() > 8 {
		t.Fatal("per-feeder bound", w.Len())
	}
	w.Sweep(now.Add(time.Hour))
	if w.Len() != 1 || w.Begin("f2", "active") != 0 {
		t.Fatal("active epoch swept")
	}
}

func TestSendRingResume(t *testing.T) {
	now := time.Unix(10_000, 0)
	r := NewSendRing("", 0)
	if !ValidSeqEpoch(r.Epoch()) {
		t.Fatal("epoch")
	}
	hold := func(at time.Time) uint64 {
		s := r.NextSeq()
		r.Hold(TFrames, s, AppendSeq(nil, s), at)
		return s
	}
	for i := 0; i < 10; i++ {
		hold(now.Add(time.Duration(i) * time.Second))
	}
	r.Ack(4)
	if n, _ := r.Held(); n != 6 {
		t.Fatal("ack trim", n)
	}
	// Resume at ackSeq 6, everything young: replay 7..10.
	p := r.Resume(6, now.Add(12*time.Second), 0)
	if p.NewEpoch || len(p.Replay) != 4 || p.Replay[0].Seq != 7 || p.Replay[3].Seq != 10 {
		t.Fatalf("%+v", p)
	}
	if s, b, _ := SplitSeq(p.Replay[0].Payload); s != 7 || len(b) != 0 {
		t.Fatal("payload")
	}
	// ackSeq above what was sent: new epoch.
	if p := r.Resume(99, now, 0); !p.NewEpoch {
		t.Fatal("ackSeq > last must need a new epoch")
	}
	old := r.Epoch()
	if r.Reset() == old || r.LastSeq() != 0 || r.NextSeq() != 1 {
		t.Fatal("reset")
	}
	// Oldest unacked record outside the resume window: new epoch, all stale.
	r = NewSendRing("", 0)
	for i := 0; i < 5; i++ {
		hold(now.Add(time.Duration(i) * 20 * time.Second))
	}
	p = r.Resume(1, now.Add(100*time.Second), ResumeWindow)
	if !p.NewEpoch || len(p.Stale) != 4 || len(p.Replay) != 0 {
		t.Fatalf("%+v", p)
	}
	// Everything acknowledged: nothing to do.
	r = NewSendRing("", 0)
	hold(now)
	if p := r.Resume(1, now, 0); p.NewEpoch || len(p.Replay) != 0 {
		t.Fatalf("%+v", p)
	}
	// Records dropped for space: ackSeq+1 missing -> new epoch.
	r = NewSendRing("", 10)
	for i := 0; i < 20; i++ {
		s := r.NextSeq()
		r.Hold(TFrames, s, []byte{1, 2, 3, 4}, now)
	}
	if r.Dropped() == 0 {
		t.Fatal("no drops")
	}
	if p := r.Resume(0, now, 0); !p.NewEpoch {
		t.Fatal("gap must need a new epoch")
	}
}

func TestFlowState(t *testing.T) {
	f := NewFlowState(50 * time.Millisecond)
	if f.Paused() || f.SlowFactor() != 1 || !f.Allowed(TFrames) {
		t.Fatal("initial")
	}
	ch, err := f.Apply(Flow{BatchMs: 200, Pause: true, SlowFactor: 0.5})
	if err != nil || !ch || f.Batch() != 200*time.Millisecond || f.SlowFactor() != 0.5 {
		t.Fatal(err)
	}
	if f.Allowed(TFrames) || f.Allowed(TDecoded) || !f.Allowed(TClockPair) || !f.Allowed(TInputStatus) || !f.Allowed(TPong) {
		t.Fatal("pause filter")
	}
	ch, _ = f.Apply(Flow{})
	if ch || f.Paused() || f.SlowFactor() != 1 || f.Batch() != 200*time.Millisecond {
		t.Fatal("resume")
	}
	if _, err := f.Apply(Flow{SlowFactor: -1}); err == nil {
		t.Fatal("bad flow applied")
	}
}

func TestTokenBucketAndBackfillRate(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewTokenBucket(1000, 500)
	if !b.Take(500, now) || b.Take(1, now) {
		t.Fatal("burst")
	}
	if !b.Take(400, now.Add(500*time.Millisecond)) || b.Take(200, now.Add(500*time.Millisecond)) {
		t.Fatal("refill")
	}
	if r := BackfillRate(30000, 1000); r != 10000 {
		t.Fatal(r) // 25 % of 40 kB/s total
	}
	if r := BackfillRate(0, 2000); r != 2000 {
		t.Fatal(r)
	}
}

func TestFlowMonitor(t *testing.T) {
	m := FlowMonitor{MaxFPS: 6000, MaxBytesPerSec: 200_000}
	t0 := time.Unix(0, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	if m.Observe(at(0), 0.6, 100, 1000) != FlowNone || m.Observe(at(4), 0.6, 100, 1000) != FlowNone {
		t.Fatal("too early")
	}
	if m.Observe(at(5), 0.6, 100, 1000) != FlowSend {
		t.Fatal("FLOW after 5 s")
	}
	if m.Observe(at(6), 0.7, 100, 1000) != FlowNone || m.Observe(at(10), 0.7, 100, 1000) != FlowSelective {
		t.Fatal("selective after another 5 s")
	}
	if m.Observe(at(11), 0.1, 100, 1000) != FlowNone {
		t.Fatal("recovered")
	}
	if m.Observe(at(20), 0.1, 9000, 1000) != FlowSendOverLimits {
		t.Fatal("over limits: FLOW first")
	}
	if m.Observe(at(25), 0.1, 9000, 1000) != FlowNone {
		t.Fatal("grace")
	}
	if m.Observe(at(30), 0.1, 100, 300_000) != FlowDisconnect {
		t.Fatal("disconnect after grace")
	}
}

func TestBackoff(t *testing.T) {
	var b Backoff
	prev := time.Duration(0)
	for i := 0; i < 10; i++ {
		d := b.Next()
		base := time.Second << i
		if base > time.Minute {
			base = time.Minute
		}
		if d < base*8/10 || d > base*12/10 {
			t.Fatalf("step %d: %v", i, d)
		}
		prev = d
	}
	_ = prev
	b.Override(300 * time.Second)
	if b.Next() != 300*time.Second {
		t.Fatal("override")
	}
	b.SessionEnded(10 * time.Second)
	if d := b.Next(); d < 48*time.Second {
		t.Fatal("short session must not reset", d)
	}
	b.SessionEnded(2 * time.Minute)
	if d := b.Next(); d > 1200*time.Millisecond {
		t.Fatal("reset", d)
	}
}
