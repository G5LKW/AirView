package feed

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"airview/internal/model"
)

// liveGPSBurst is a Radarcape GPS burst stamped around the current time, so
// the ticks resolve to today, plus readsb-style extension messages.
func liveGPSBurst(now time.Time) []byte {
	now = now.UTC()
	sod := now.Hour()*3600 + now.Minute()*60 + now.Second()
	var b []byte
	b = AppendBeastUUID(b, "0f8c1e2a-7d3b-4c55-9a1a-1a2b3c4d5e6f")
	b = AppendBeastFrame(b, BeastStatusType, GPSTimestamp(sod, 0), 0, statusPayload(0x10, 0, gpsUTCLocked))
	b = AppendBeastFrame(b, BeastModeSLong, GPSTimestamp(sod, 1000), 200, long1)
	b = append(b, 0x1A, 0xE9) // unknown escape: skipped, link stays up
	b = AppendBeastPing(b, 7)
	b = AppendBeastFrame(b, BeastModeSShort, GPSTimestamp(sod, 2000), 100, short1)
	return b
}

func TestManagerClockEventsAndReconnect(t *testing.T) {
	src := newTCPSource(t, func(conn net.Conn, n int) {
		conn.Write(liveGPSBurst(time.Now()))
		if n == 1 {
			time.Sleep(50 * time.Millisecond)
			conn.Close() // force a reconnect
			return
		}
		io.Copy(io.Discard, conn)
	})
	sink := &safeSink{}
	m := testManager(t, sink)
	var (
		mu     sync.Mutex
		events []ClockEvent
		ids    []string
	)
	m.OnClockEvent(func(id string, ev ClockEvent) {
		mu.Lock()
		events = append(events, ev)
		ids = append(ids, id)
		mu.Unlock()
	})
	rc := model.Feed{ID: "rc", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: src.port()}
	m.Apply([]model.Feed{rc})

	waitFor(t, "two connections", func() bool { return sink.nFrames() >= 6 })
	waitFor(t, "two epochs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 2
	})
	mu.Lock()
	e0, e1 := events[0], events[1]
	if ids[0] != "rc" || e0.Reason != model.ClockEvRestart || e0.Detail != ClockDetailStart || e0.NewClass != model.ClockGPSUTC {
		t.Errorf("first event %+v", e0)
	}
	if e1.Reason != model.ClockEvRestart || e1.Detail != ClockDetailReconnect || e1.OldEpoch != e0.NewEpoch || e1.NewEpoch != e0.NewEpoch+1 {
		t.Errorf("reconnect event %+v after %+v", e1, e0)
	}
	mu.Unlock()

	f := sink.frame(1)
	if d := int64(f.Ticks) - f.HostNs; f.ClockCls != model.ClockGPSUTC || f.Epoch != e0.NewEpoch || f.HostNs == 0 || d < -3e9 || d > 3e9 {
		t.Errorf("frame %+v", f)
	}
	cs, ok := m.Clock("rc")
	if !ok || cs.Class != model.ClockGPSUTC || cs.Detected != model.ClockGPSUTC || cs.Epoch != e1.NewEpoch ||
		cs.Events < 2 || cs.Escapes < 4 || cs.UnknownEscapes < 2 {
		t.Errorf("clock state %+v", cs)
	}
	if _, ok := m.Clock("nope"); ok {
		t.Error("clock state for an unknown feed")
	}
	st, _ := status(m, "rc")
	if !strings.Contains(st.Info, "clock gps_utc") {
		t.Errorf("info %q", st.Info)
	}
}

func TestManagerClockOverrideAndForwarding(t *testing.T) {
	src := newTCPSource(t, func(conn net.Conn, n int) {
		now := time.Now().UTC()
		sod := now.Hour()*3600 + now.Minute()*60 + now.Second()
		var b []byte
		// Old firmware: GPS state not reported, detected gps_emul.
		b = AppendBeastFrame(b, BeastStatusType, GPSTimestamp(sod, 0), 0, statusPayload(0x10|0x80, 0, 0))
		b = AppendBeastFrame(b, BeastModeSLong, GPSTimestamp(sod, 1000), 200, long1)
		conn.Write(b)
		io.Copy(io.Discard, conn)
	})
	sink := &safeSink{}
	m := testManager(t, sink)
	rc := model.Feed{ID: "rc", Enabled: true, Type: model.FeedBeast, Host: "127.0.0.1", Port: src.port(), ClockClass: "gps_utc"}
	mlat := model.Feed{ID: "mlat", Enabled: false, Type: model.FeedBeast, Host: "127.0.0.1", Port: 1, MLATOnly: true}
	priv := model.Feed{ID: "priv", Enabled: false, Type: model.FeedBeast, Host: "127.0.0.1", Port: 1, NoForward: true}
	demo := model.Feed{ID: "demo", Enabled: false, Type: model.FeedDemo}
	relay := model.Feed{ID: "relay", Enabled: false, Type: model.FeedHub, Host: "127.0.0.1", Port: 1}
	m.Apply([]model.Feed{rc, mlat, priv, demo, relay})

	waitFor(t, "frames", func() bool { return sink.nFrames() >= 2 })
	if f := sink.frame(1); f.ClockCls != model.ClockGPSUTC {
		t.Errorf("override not applied: %v", f.ClockCls)
	}
	if cs, _ := m.Clock("rc"); cs.Detected != model.ClockGPSEmul || cs.Class != model.ClockGPSUTC {
		t.Errorf("clock state %+v", cs)
	}

	if !m.Forwardable("rc") || m.Forwardable("mlat") || m.Forwardable("priv") || m.Forwardable("unknown") ||
		m.Forwardable("demo") || m.Forwardable("relay") {
		t.Error("Forwardable wrong")
	}
	// Reconfiguring updates the forwarding table.
	priv.NoForward = false
	m.Apply([]model.Feed{rc, mlat, priv})
	if !m.Forwardable("priv") {
		t.Error("Forwardable not updated by Apply")
	}

	f := sink.frame(1)
	if ok, timed := ForwardableFrame(&f); !ok || !timed {
		t.Error("timed frame not forwardable")
	}
	f.MLAT = true
	if ok, _ := ForwardableFrame(&f); ok {
		t.Error("MLAT frame forwardable")
	}
	st := sink.frame(0)
	if ok, _ := ForwardableFrame(&st); ok {
		t.Error("status frame forwardable as a Mode S frame")
	}
	u := model.RawFrame{Kind: model.FrameModeSShort}
	if ok, timed := ForwardableFrame(&u); !ok || timed {
		t.Error("untimed frame must be forwardable only as untimed")
	}
}
