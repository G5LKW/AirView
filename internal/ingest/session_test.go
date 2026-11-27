package ingest

import (
	"sync"
	"testing"
	"time"

	"airview/internal/model"
)

type fakeCtl struct {
	mu        sync.Mutex
	flows     []FlowCmd
	interests map[model.StationIdx][]InterestCmd
	discon    []string
}

func newFakeCtl() *fakeCtl { return &fakeCtl{interests: map[model.StationIdx][]InterestCmd{}} }

func (c *fakeCtl) SendFlow(f FlowCmd) error {
	c.mu.Lock()
	c.flows = append(c.flows, f)
	c.mu.Unlock()
	return nil
}

func (c *fakeCtl) SendInterest(st model.StationIdx, in InterestCmd) error {
	c.mu.Lock()
	c.interests[st] = append(c.interests[st], in)
	c.mu.Unlock()
	return nil
}

func (c *fakeCtl) Disconnect(code, reason string, sec int) {
	c.mu.Lock()
	c.discon = append(c.discon, code)
	c.mu.Unlock()
}

func TestSessionModePolicyAndInterest(t *testing.T) {
	res := newFakeRes()
	res.set(4, model.AdmPending)
	x := newTester(t, func(o *Options) {
		o.Resolver = res
		o.Limits = model.DefaultAggregator().Limits
		o.Limits.SelectiveAbove = 2
	})
	ctl := newFakeCtl()
	s := x.ing.OpenSession(SessionInfo{Proto: "avf", Label: "f1", Controller: ctl, Selective: true})
	if m := s.AddStation(2); m != ModeFull {
		t.Fatalf("first station mode %s", m)
	}
	s.AddStation(3)
	s.AddStation(4) // pending: not counted
	now := time.Unix(0, t0)
	x.ing.Tick(now)
	if x.ing.DesiredMode() != ModeFull || len(ctl.interests) != 0 {
		t.Fatalf("2 approved stations must stay full (mode %s, %d interests)", x.ing.DesiredMode(), len(ctl.interests))
	}
	// interest sets are kept while full and sent with the switch
	if err := x.ing.SetInterest(2, InterestCmd{SyncAdd: []uint32{0x4840D6, 0x3C6586}, MLATAdd: []uint32{1}}); err != nil {
		t.Fatal(err)
	}
	s2 := x.ing.OpenSession(SessionInfo{Proto: "avf", Label: "f2", Controller: newFakeCtl(), Selective: true})
	s2.AddStation(5)
	now = now.Add(time.Second)
	x.ing.Tick(now)
	if x.ing.DesiredMode() != ModeSelective {
		t.Fatal("3 approved stations > 2 must switch to selective")
	}
	in := ctl.interests[2]
	if len(in) != 1 || in[0].Mode != ModeSelective || !in[0].Reset || len(in[0].SyncAdd) != 2 || len(in[0].MLATAdd) != 1 || in[0].ReduceMs != 500 {
		t.Fatalf("station 2 interest %+v", in)
	}
	// deltas are forwarded in selective mode
	x.ing.SetInterest(2, InterestCmd{SyncDel: []uint32{0x3C6586}})
	if in := ctl.interests[2]; len(in) != 2 || len(in[1].SyncDel) != 1 || in[1].Mode != ModeSelective {
		t.Fatalf("delta %+v", in)
	}
	// session 2 ends with a resume grace: still counted
	s2.Close(30 * time.Second)
	now = now.Add(time.Second)
	x.ing.Tick(now)
	if x.ing.DesiredMode() != ModeSelective || !x.ing.Online(5) {
		t.Fatal("grace period not honoured")
	}
	now = now.Add(31 * time.Second)
	x.ing.Tick(now)
	if x.ing.DesiredMode() != ModeFull || x.ing.Online(5) {
		t.Fatal("grace did not expire")
	}
	if in := ctl.interests[2]; in[len(in)-1].Mode != ModeFull {
		t.Fatalf("back to full: %+v", in[len(in)-1])
	}
	if ss := x.ing.Sessions(); len(ss) != 1 || len(ss[0].Stations) != 3 {
		t.Fatalf("sessions %+v", ss)
	}
	s.Close(0)
}

func TestSessionFlowEscalation(t *testing.T) {
	x := newTester(t, func(o *Options) {
		o.Limits = model.DefaultAggregator().Limits
		o.Limits.MaxFPSPerStation = 100
		o.FlowGrace = 3 * time.Second
	})
	ctl := newFakeCtl()
	s := x.ing.OpenSession(SessionInfo{Proto: "avf", Controller: ctl, Selective: true})
	s.AddStation(2)
	now := time.Unix(0, t0)
	x.ing.Tick(now)
	feed := func(n int) {
		for i := 0; i < n; i++ {
			x.ing.st.get(2, true).frames.Add(1)
		}
	}
	for k := 1; k <= 5; k++ {
		feed(500) // 500 fps > 100
		now = now.Add(time.Second)
		x.ing.Tick(now)
	}
	if len(ctl.flows) == 0 || ctl.flows[0].SlowFactor > 0.21 {
		t.Fatalf("flows %+v", ctl.flows)
	}
	if len(ctl.discon) != 1 || ctl.discon[0] != "rate_limited" {
		t.Fatalf("disconnects %v", ctl.discon)
	}
	// queue high for 5 s -> FLOW, another 5 s -> selective
	ctl2 := newFakeCtl()
	s2 := x.ing.OpenSession(SessionInfo{Proto: "avf", Controller: ctl2, Selective: true})
	s2.AddStation(3)
	s2.SetQueueFill(0.8)
	for k := 0; k < 12; k++ {
		now = now.Add(time.Second)
		x.ing.Tick(now)
	}
	if len(ctl2.flows) != 1 || ctl2.flows[0].BatchMs != 200 {
		t.Fatalf("queue FLOW %+v", ctl2.flows)
	}
	if in := ctl2.interests[3]; len(in) != 1 || in[0].Mode != ModeSelective {
		t.Fatalf("forced selective %+v", in)
	}
	// calm for 30 s lifts the FLOW
	s2.SetQueueFill(0.1)
	for k := 0; k < 32; k++ {
		now = now.Add(time.Second)
		x.ing.Tick(now)
	}
	if n := len(ctl2.flows); n != 2 || ctl2.flows[1].SlowFactor != 1 || ctl2.flows[1].BatchMs != 50 {
		t.Fatalf("recovery %+v", ctl2.flows)
	}
}

func TestSessionHostOffset(t *testing.T) {
	x := newTester(t, nil)
	s := x.ing.OpenSession(SessionInfo{Proto: "avf"})
	s.AddStation(2)
	s.SetHostOffset(1500*time.Millisecond, 40*time.Millisecond)
	s.AddStation(3) // added later: inherits the offset
	for _, st := range []model.StationIdx{2, 3} {
		ti, ok := x.ing.Timing(st)
		if !ok || ti.OffsetSrc != "ping" || ti.OffsetNs != int64(1500*time.Millisecond) || ti.RTTNs != int64(40*time.Millisecond) {
			t.Errorf("station %d timing %+v", st, ti)
		}
	}
	// a frame stamped by the feeder's clock (1.5 s ahead) gets a server Recv
	f := tframe(2, 0, t0+int64(1500*time.Millisecond), df11(5))
	x.ing.frame(&f, false, t0+int64(20*time.Millisecond))
	if d := f.Recv.UnixNano() - t0; d != 0 {
		t.Errorf("Recv off by %v", time.Duration(d))
	}
	s.Close(0)
}

func TestModePolicyCountsSessionlessStations(t *testing.T) {
	x := newTester(t, func(o *Options) {
		o.Limits = model.DefaultAggregator().Limits
		o.Limits.SelectiveAbove = 2
	})
	ctl := newFakeCtl()
	s := x.ing.OpenSession(SessionInfo{Proto: "avf", Controller: ctl, Selective: true})
	s.AddStation(2)
	// two Beast stations deliver frames without a session
	for _, st := range []model.StationIdx{8, 9} {
		x.send(tframe(st, -1, t0, df11(uint32(st))), true)
	}
	x.ing.Tick(time.Unix(0, t0+int64(time.Second)))
	if x.ing.DesiredMode() != ModeSelective || len(ctl.interests[2]) != 1 {
		t.Fatalf("3 active approved stations > 2: mode %s, interests %v", x.ing.DesiredMode(), ctl.interests)
	}
	x.ing.Tick(time.Unix(0, t0+int64(2*time.Minute)))
	if x.ing.DesiredMode() != ModeFull {
		t.Fatal("silent Beast stations still counted after a minute")
	}
	s.Close(0)
}
