package engine

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"airview/internal/feed"
	"airview/internal/model"
)

// sbsBridge is the localhost SBS-in bridge (DESIGN §5.3): an operator's
// separate, unmodified mlat-server can point its BaseStation (SBS) output
// at 127.0.0.1:<aggregator.sbsInLocalPort>, and its positions enter the map
// as MLAT positions with Src = model.SrcMLATExt. AirView never runs, links
// or embeds mlat-server; this is a plain TCP text input.
//
// The listener binds the loopback address only. A few concurrent
// connections are accepted (one mlat-server normally keeps one open); each
// is read until it closes.
type sbsBridge struct {
	ln  net.Listener
	in  NetIngest
	log *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	done  chan struct{}
	once  sync.Once

	updates, ignored atomic.Uint64
}

const sbsBridgeMaxConns = 8

// startSBSBridge listens on addr and serves until ctx ends or Close.
func startSBSBridge(ctx context.Context, addr string, in NetIngest, log *slog.Logger) (*sbsBridge, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	b := &sbsBridge{ln: ln, in: in, log: log, conns: map[net.Conn]struct{}{}, done: make(chan struct{})}
	b.wg.Add(1)
	go b.accept()
	go func() {
		select {
		case <-ctx.Done():
			b.Close()
		case <-b.done:
		}
	}()
	return b, nil
}

// Addr returns the bound address.
func (b *sbsBridge) Addr() string { return b.ln.Addr().String() }

func (b *sbsBridge) accept() {
	defer b.wg.Done()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-b.done:
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		b.mu.Lock()
		if len(b.conns) >= sbsBridgeMaxConns {
			b.mu.Unlock()
			_ = c.Close()
			continue
		}
		b.conns[c] = struct{}{}
		b.mu.Unlock()
		b.wg.Add(1)
		go b.serve(c)
	}
}

// bridgeSink marks every update as an external MLAT position.
type bridgeSink struct{ b *sbsBridge }

func (s bridgeSink) HandleFrame(model.RawFrame) {}

func (s bridgeSink) HandleUpdate(u model.DecodedUpdate) {
	if u.Lat == nil || u.Lon == nil {
		// Only positions are useful from an MLAT server; identity and
		// altitude come from the stations' own frames.
		s.b.ignored.Add(1)
		return
	}
	u.MLAT = true
	u.Src = model.SrcMLATExt
	u.Station = model.LocalStation
	u.FeedID = bridgeFeedID
	s.b.updates.Add(1)
	s.b.in.HandleUpdate(u)
}

func (b *sbsBridge) serve(c net.Conn) {
	defer b.wg.Done()
	defer func() {
		b.mu.Lock()
		delete(b.conns, c)
		b.mu.Unlock()
		_ = c.Close()
	}()
	b.log.Info("SBS-in bridge: mlat-server connected")
	dec := &feed.SBSDecoder{Sink: bridgeSink{b}, FeedID: bridgeFeedID, MLATOnly: true}
	buf := make([]byte, 16<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			dec.Recv = time.Now()
			dec.Write(buf[:n])
		}
		if err != nil {
			b.log.Info("SBS-in bridge: mlat-server disconnected")
			return
		}
	}
}

// Close stops the listener and every connection and waits for them.
func (b *sbsBridge) Close() {
	b.once.Do(func() {
		close(b.done)
		_ = b.ln.Close()
		b.mu.Lock()
		for c := range b.conns {
			_ = c.Close()
		}
		b.mu.Unlock()
	})
	b.wg.Wait()
}

// Stats returns the bridge counters (updates passed on, lines ignored).
func (b *sbsBridge) Stats() (updates, ignored uint64) {
	return b.updates.Load(), b.ignored.Load()
}
