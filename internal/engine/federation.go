package engine

// Federation (DESIGN §12.1 phase 5; internal/federation WIRING.md): links
// to other AirView networks over AVFED/1 and federation-safe outbound
// feeds. Aggregator mode only. The node is rebuilt whenever the
// aggregator.federation block changes (links re-establish within seconds).
// Peers receive at most the public map: the node projects the unprojected
// tracker snapshot itself, coarsens or drops island-guard aircraft and has
// no wire fields for feeder identities, positions, receivers or addresses.

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"airview/internal/federation"
	"airview/internal/model"
	"airview/internal/privacy/publicdto"
	"airview/internal/tracker"
)

// fedState is the running federation node of a netCore.
type fedState struct {
	mu   sync.Mutex // serialises rebuilds
	node atomic.Pointer[federation.Node]
	key  string // JSON of the running config ("" = none)
	err  string
	id   string // node ID of the running node
	pin  string
}

// SetFederationGuard installs the public map's island-guard predicate for
// the federation export (the server calls it once; fn returns nil while
// the guard is off).
func (e *Engine) SetFederationGuard(fn func() publicdto.Guard) {
	if fn != nil {
		e.fedGuard.Store(&fn)
	}
}

// Federation returns the running federation node (nil = off, standalone
// mode, or not started).
func (e *Engine) Federation() *federation.Node {
	if nc := e.net; nc != nil {
		return nc.fed.node.Load()
	}
	return nil
}

// FederationError is the last start error of the federation node ("" =
// none).
func (e *Engine) FederationError() string {
	if nc := e.net; nc != nil {
		nc.fed.mu.Lock()
		defer nc.fed.mu.Unlock()
		return nc.fed.err
	}
	return ""
}

// syncFederation makes the node match cfg.Aggregator.Federation. Caller
// holds opMu; rt is running.
func (e *Engine) syncFederation(rt *runtimeState, nc *netCore, cfg model.Config) {
	fs := &nc.fed
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fc := federation.FromModel(cfg.Aggregator.Federation)
	key := ""
	if fc.Enabled {
		b, _ := json.Marshal(struct {
			F    federation.Config
			Host string
		}{fc, cfg.Aggregator.PublicHost})
		key = string(b)
	}
	if key == fs.key && (key == "" || fs.node.Load() != nil) {
		return
	}
	if old := fs.node.Swap(nil); old != nil {
		_ = old.Close()
		nc.log.Info("federation stopped")
	}
	fs.key, fs.err, fs.id, fs.pin = key, "", "", ""
	if key == "" {
		return
	}
	fail := func(err error) {
		fs.err = err.Error()
		nc.log.Warn("federation cannot start", "err", err)
		e.Notify("warn", "Federation cannot start: "+err.Error())
	}
	fc.Normalize()
	if probs := fc.Check("aggregator.federation"); len(probs) > 0 {
		fail(fmt.Errorf("%s", probs[0]))
		return
	}
	var hosts []string
	if h := cfg.Aggregator.PublicHost; h != "" {
		hosts = []string{h}
	}
	m, err := federation.LoadMaterial(e.dir, &fc, hosts)
	if err != nil {
		fail(err)
		return
	}
	tr := e.tr
	opts := federation.Options{
		Identity:    m.Identity,
		Certificate: m.Certificate,
		Version:     e.opts.Version,
		Source:      tr.Snapshot,
		Guard: func(a *model.Aircraft) bool {
			if p := e.fedGuard.Load(); p != nil {
				if g := (*p)(); g != nil {
					return g(a)
				}
			}
			return false
		},
		Logf: func(format string, args ...any) {
			nc.log.Info(fmt.Sprintf(format, args...), "component", "federation")
		},
	}
	if fc.Inject {
		opts.Sink = tr
	}
	node, err := federation.New(fc, opts)
	if err != nil {
		fail(err)
		return
	}
	if err := node.Start(rt.ctx); err != nil {
		_ = node.Close()
		fail(err)
		return
	}
	fs.node.Store(node)
	fs.id, fs.pin = node.ID(), m.Pin
	nc.log.Info("federation started", "nodeId", node.ID(), "listen", fc.Listen, "peers", len(fc.Peers), "feeds", len(fc.Feeds))
}

// closeFederation stops the node (engine stop).
func (nc *netCore) closeFederation() {
	nc.fed.mu.Lock()
	defer nc.fed.mu.Unlock()
	if n := nc.fed.node.Swap(nil); n != nil {
		_ = n.Close()
	}
	nc.fed.key = ""
}

// fedHandler tees the ingest's first copies into the federation node's
// Beast out-feeds (only while one wants frames).
type fedHandler struct {
	tracker.NetHandler
	fed *fedState
}

func (h fedHandler) Handle(f model.RawFrame) tracker.Result {
	r := h.NetHandler.Handle(f)
	if n := h.fed.node.Load(); n != nil && n.WantsFrames() {
		n.HandleFrame(f)
	}
	return r
}

// publishFederationMLAT hands one own MLAT result to the node.
func (nc *netCore) publishFederationMLAT(f *model.MLATFix) {
	if n := nc.fed.node.Load(); n != nil {
		n.PublishMLAT([]model.MLATFix{*f})
	}
}
