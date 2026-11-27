package avfsrv

import (
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"airview/internal/model"
)

// feedGuard plays 30 s of live traffic through a guard: frames received at
// true time t, stamped with host = t + feederOff − lie, batched every
// 200 ms, arriving rtt/2 after the batch was sent.
func feedGuard(c *conn, now *time.Time, feederOff, lie, rtt time.Duration, late bool) {
	rng := rand.New(rand.NewPCG(1, 2))
	for b := 0; b < 150; b++ {
		*now = now.Add(200 * time.Millisecond)
		sent := now.UnixNano()
		raw := make([]model.RawFrame, 30)
		for i := range raw {
			recv := sent - int64(rng.Float64()*200e6)
			raw[i] = model.RawFrame{HostNs: recv + int64(feederOff) - int64(lie), Late: late}
		}
		c.hg.observe(raw, sent+int64(rtt/2))
		if b%5 == 0 {
			c.hostOffset(feederOff, rtt)
		}
	}
}

func guardConn(slack time.Duration, now *time.Time) *conn {
	s := &Server{o: Options{HostLatencySlack: slack, Now: func() time.Time { return *now },
		Logger: slog.New(slog.DiscardHandler)}}
	return &conn{s: s, feeder: "f1"}
}

func TestHostGuardHonestFeederUnchanged(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := guardConn(300*time.Millisecond, &now)
	off := 1500 * time.Millisecond // feeder host clock 1.5 s ahead: legitimate, PONG knows it
	feedGuard(c, &now, off, 0, 80*time.Millisecond, false)
	if got := c.hostOffset(off, 80*time.Millisecond); got != off {
		t.Fatalf("honest feeder: offset %v, want %v", got, off)
	}
	if c.s.st.hostClamped.Load() != 0 {
		t.Fatal("honest feeder counted as clamped")
	}
}

func TestHostGuardNegativeFakeOffset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := guardConn(300*time.Millisecond, &now)
	off, rtt, lie := 200*time.Millisecond, 60*time.Millisecond, 2*time.Second
	feedGuard(c, &now, off, lie, rtt, false)
	got := c.hostOffset(off, rtt)
	// corrected time of the earliest-looking frame: host − got must not be
	// earlier than arrival − rtt/2 − slack.
	wantMax := off - lie + rtt/2 + 300*time.Millisecond // ≥ this much removed
	if got > off-lie+400*time.Millisecond || got < off-lie {
		t.Fatalf("offset %v: the %v lie is not undone (want about %v)", got, lie, wantMax)
	}
	if c.s.st.hostClamped.Load() != 1 {
		t.Fatalf("clamped sessions %d", c.s.st.hostClamped.Load())
	}
	// A frame received at true time T now maps to at least T − slack.
	T := now.UnixNano()
	host := T + int64(off) - int64(lie)
	if corr := host - int64(got); corr < T-int64(300*time.Millisecond)-int64(rtt) {
		t.Fatalf("corrected time %d ms before the truth", (T-corr)/1e6)
	}
	// The feeder stops lying: the correction is released.
	feedGuard(c, &now, off, 0, rtt, false)
	if got := c.hostOffset(off, rtt); got != off {
		t.Fatalf("after the lie stopped: offset %v, want %v", got, off)
	}
}

func TestHostGuardIgnoresBackfillAndCanBeOff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := guardConn(300*time.Millisecond, &now)
	feedGuard(c, &now, 0, 30*time.Second, 50*time.Millisecond, true) // spooled frames are old for real
	if got := c.hostOffset(0, 50*time.Millisecond); got != 0 {
		t.Fatalf("backfill moved the offset to %v", got)
	}
	c2 := guardConn(-1, &now)
	feedGuard(c2, &now, 0, 2*time.Second, 50*time.Millisecond, false)
	if got := c2.hostOffset(0, 50*time.Millisecond); got != 0 {
		t.Fatalf("guard off but offset %v", got)
	}
}
