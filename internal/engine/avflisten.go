package engine

import (
	"context"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"airview/internal/avf"
	"airview/internal/geoid"
	"airview/internal/ingest/avfsrv"
	"airview/internal/model"
	"airview/internal/ntp"
	"airview/internal/registry"
)

// AVF/1 listener (DESIGN §4; internal/ingest/avfsrv, WP-I). Part of the
// runtime like the Beast listener: bound to Options.ListenHost, gated on the
// admin account, feeding the ingest core. The TLS certificate is the
// operator's (aggregator.tlsCert/tlsKey) or the self-signed one in
// <data>/avf-tls/, and the network's server ID lives in
// <data>/avf-tls/server-id; both survive restarts and backups.

// avfState is the running AVF listener.
type avfState struct {
	srv *avfsrv.Server
	at  string // listen address
	pin string // SPKI pin of the server certificate
	tls [2]string
}

func (e *Engine) syncAVF(rt *runtimeState, nc *netCore, cfg model.Config) {
	port := cfg.Aggregator.AVFPort
	addr := ""
	if port > 0 && nc.core != nil {
		addr = e.listenAddr(port)
	}
	tlsKey := [2]string{cfg.Aggregator.TLSCert, cfg.Aggregator.TLSKey}
	nc.mu.Lock()
	cur := nc.avf
	nc.mu.Unlock()
	if cur != nil && cur.at == addr && cur.tls == tlsKey {
		cur.srv.SetPolicy(avfsrv.PolicyFromConfig(cfg.Aggregator))
		return
	}
	if cur != nil {
		nc.mu.Lock()
		nc.avf = nil
		nc.mu.Unlock()
		_ = cur.srv.Close()
		nc.log.Info("AVF/1 listener stopped")
	}
	if addr == "" {
		nc.mu.Lock()
		nc.avfErr = ""
		nc.mu.Unlock()
		return
	}
	tc, pin, err := avfsrv.TLSFromConfig(e.dir, cfg.Aggregator)
	var sid string
	if err == nil {
		sid, err = e.avfServerID(nc)
	}
	var srv *avfsrv.Server
	if err == nil {
		o := avfsrv.OptionsFromConfig(cfg.Aggregator)
		o.Addr = addr
		o.TLSConfig = tc
		o.ServerID = sid
		o.Resolver = nc.reg.Resolver()
		o.Gate = nc.ready.Load
		o.CloneGuard = nc.clones
		o.Watermarks = nc.watermarks
		o.Now = ntp.Now
		o.Logger = e.log.With("component", "avfsrv")
		o.Hooks = avfsrv.Hooks{OnReceiver: nc.onAVFReceiver, OnGNSS: nc.onAVFGNSS, OnStationMinute: nc.minutes.add}
		srv, err = avfsrv.New(o, nc.reg, nc.core)
	}
	var ln net.Listener
	if err == nil {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		if srv != nil {
			_ = srv.Close()
		}
		e.listenerFailed(nc, &nc.avfErr, "AVF/1", port, err)
		return
	}
	st := &avfState{srv: srv, at: addr, pin: pin, tls: tlsKey}
	nc.mu.Lock()
	nc.avf, nc.avfErr = st, ""
	nc.mu.Unlock()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := srv.Serve(rt.ctx, ln); err != nil && rt.ctx.Err() == nil {
			nc.log.Error("AVF/1 listener stopped", "err", err)
		}
	}()
	nc.log.Info("AVF/1 listener", "addr", ln.Addr().String(), "pin", pin, "gated", !nc.ready.Load())
}

// AVFIngest returns the running AVF/1 listener (nil when off or in
// standalone mode) and its SPKI pin.
func (e *Engine) AVFIngest() (*avfsrv.Server, string) {
	if e.net == nil {
		return nil, ""
	}
	e.net.mu.Lock()
	defer e.net.mu.Unlock()
	if e.net.avf == nil {
		return nil, ""
	}
	return e.net.avf.srv, e.net.avf.pin
}

// rxLocKey is the last location applied from a station's RECEIVER record.
type rxLocKey struct {
	lat, lon, alt float64
	ref, src      string
}

// avfLocations de-duplicates RECEIVER locations so that a reconnecting
// feeder does not rewrite the registry every time.
type avfLocations struct {
	mu   sync.Mutex
	last map[model.StationIdx]rxLocKey
	gnss map[model.StationIdx]gnssSeen
}

// gnssSeen is the last GNSS fix written to the registry for a station.
type gnssSeen struct {
	p registry.Position
	t time.Time
}

// gnssRewriteEvery bounds how often an unchanged-looking GNSS fix of one
// station is written to the registry; gnssMoveM is the movement that
// writes at once.
const (
	gnssRewriteEvery = 30 * time.Minute
	gnssMoveM        = 5.0
)

// onAVFGNSS stores the receiver's own GNSS fix (GNSS_POS, DESIGN §10.1) as
// the station's GNSS position. Writes are de-duplicated: only a move of more
// than gnssMoveM or gnssRewriteEvery after the last write.
func (nc *netCore) onAVFGNSS(st model.StationIdx, g avf.GNSSPos) {
	if math.IsNaN(g.Lat) || math.IsNaN(g.Lon) || g.Lat < -90 || g.Lat > 90 || g.Lon < -180 || g.Lon > 180 || (g.Lat == 0 && g.Lon == 0) {
		return
	}
	hae := g.AltM
	if g.AltRef == model.AltRefMSL {
		if h, ok := geoid.MSLToHAE(g.Lat, g.Lon, g.AltM); ok {
			hae = h
		}
	}
	p := registry.Position{Lat: g.Lat, Lon: g.Lon, Alt: hae, Valid: true}
	now := time.Now()
	nc.rxLoc.mu.Lock()
	if nc.rxLoc.gnss == nil {
		nc.rxLoc.gnss = map[model.StationIdx]gnssSeen{}
	}
	last, ok := nc.rxLoc.gnss[st]
	moved := !ok || approxDistM(last.p, p) > gnssMoveM
	if !moved && now.Sub(last.t) < gnssRewriteEvery {
		nc.rxLoc.mu.Unlock()
		return
	}
	nc.rxLoc.gnss[st] = gnssSeen{p: p, t: now}
	nc.rxLoc.mu.Unlock()
	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		ctx, cancel := context.WithTimeout(nc.ctx, 20*time.Second)
		defer cancel()
		if _, err := nc.reg.SetGNSS(ctx, st, p); err != nil && nc.ctx.Err() == nil {
			nc.log.Warn("station GNSS position", "station", uint32(st), "err", err)
		}
	}()
}

// approxDistM is the 3-D distance of two nearby positions (equirectangular).
func approxDistM(a, b registry.Position) float64 {
	const r = 6371e3
	dLat := (b.Lat - a.Lat) * math.Pi / 180
	dLon := (b.Lon - a.Lon) * math.Pi / 180 * math.Cos((a.Lat+b.Lat)/2*math.Pi/180)
	return math.Sqrt((dLat*r)*(dLat*r) + (dLon*r)*(dLon*r) + (b.Alt-a.Alt)*(b.Alt-a.Alt))
}

// onAVFReceiver applies the location a native feeder announces (DESIGN
// §10.1): its GNSS or declared position becomes the station's effective
// position unless a better one (a confirmed survey) is in use. Called on a
// connection goroutine: the registry write runs in the background.
func (nc *netCore) onAVFReceiver(st model.StationIdx, r avf.Receiver) {
	// The first RECEIVER record of a session may precede the first frame
	// (class still unknown); later records carry the receiver's clock
	// class, which the MLAT engine takes from the registry.
	if c := r.Class(); c.Timed() {
		if cur, ok := nc.reg.Station(st); ok && cur.ClockCls != c {
			nc.wg.Add(1)
			go func() {
				defer nc.wg.Done()
				ctx, cancel := context.WithTimeout(nc.ctx, 20*time.Second)
				defer cancel()
				if _, err := nc.reg.SetClockClass(ctx, st, c); err != nil {
					nc.log.Warn("station clock class from RECEIVER", "station", uint32(st), "err", err)
				}
			}()
		}
	}
	loc := r.Location
	if loc == nil || math.IsNaN(loc.Lat) || math.IsNaN(loc.Lon) || loc.Lat < -90 || loc.Lat > 90 || loc.Lon < -180 || loc.Lon > 180 {
		return
	}
	key := rxLocKey{lat: loc.Lat, lon: loc.Lon, alt: loc.Alt, ref: loc.AltRef, src: loc.Source}
	nc.rxLoc.mu.Lock()
	if nc.rxLoc.last == nil {
		nc.rxLoc.last = map[model.StationIdx]rxLocKey{}
	}
	same := nc.rxLoc.last[st] == key
	nc.rxLoc.last[st] = key
	nc.rxLoc.mu.Unlock()
	if same {
		return
	}
	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		ctx, cancel := context.WithTimeout(nc.ctx, 20*time.Second)
		defer cancel()
		if err := applyReceiverLocation(ctx, nc.reg, st, *loc); err != nil {
			nc.log.Warn("station location from RECEIVER", "station", uint32(st), "err", err)
		}
	}()
}

// applyReceiverLocation stores a RECEIVER location in the registry through
// registry.FeederPosition (finding PRIV-1): small changes are ignored, at
// most one change per station and interval is applied, and large moves are
// held until the survey or the owner confirms them (aggregator.positions).
// A survey result or a GNSS position in charge keeps precedence (§10.1).
func applyReceiverLocation(ctx context.Context, reg *registry.Registry, st model.StationIdx, loc avf.Location) error {
	alt, ref := loc.Alt, loc.AltRef
	gnss := loc.Source == model.LocSrcGNSS
	if gnss && ref == model.AltRefMSL {
		// GNSS positions are stored as HAE.
		if h, ok := geoid.MSLToHAE(loc.Lat, loc.Lon, loc.Alt); ok {
			alt, ref = h, model.AltRefHAE
		}
	}
	_, _, err := reg.FeederPosition(ctx, st, registry.FeederPos{Lat: loc.Lat, Lon: loc.Lon, Alt: alt, AltRef: ref,
		GNSS: gnss, SigmaM: loc.SigmaM, SetEffective: true})
	return err
}

// avfServerID returns the network's AVF server ID, kept in the registry
// (stations.db, AVF.md §3.2). A value an earlier version kept in
// <data>/avf-tls/server-id is adopted once, so feeders see no change.
func (e *Engine) avfServerID(nc *netCore) (string, error) {
	legacy := ""
	if b, err := os.ReadFile(filepath.Join(e.dir, filepath.FromSlash(avfsrv.ServerIDFile))); err == nil {
		legacy = strings.TrimSpace(string(b))
	}
	ctx, cancel := context.WithTimeout(nc.ctx, 15*time.Second)
	defer cancel()
	return nc.reg.ServerID(ctx, legacy)
}
