# internal/avf — wiring notes (WP-P)

AVF/1 codec and session layer (docs/aggregator/AVF.md is normative). Imports only the
standard library and `internal/model`. Consumers: `internal/uplink` (client, WP-U),
`internal/ingest/avfsrv` (server, WP-I), `cmd/airview-feeder` (identity file, pin display).

## 1. Server (avfsrv)

```go
cert, pin, err := avf.LoadOrCreateSelfSigned(filepath.Join(dataDir, "avf-tls"), []string{cfg.Aggregator.PublicHost})
// or, with Aggregator.TLSCert/TLSKey: r, err := avf.NewCertReloader(certFile, keyFile); get := r.GetCertificate
tlsCfg := avf.ServerTLSConfig(avf.StaticCertificate(cert))   // TLS 1.3 only, ALPN avf/1 mandatory
// show `pin` in the admin UI and the install one-liner

p, err := avf.ServerHandshake(ctx, nc, tlsCfg, avf.ServerConfig{
    ServerID: serverID, Name: netName, Admission: cfg.Aggregator.Admission,
    HandshakeTimeout: time.Duration(limits.HandshakeTimeoutSec) * time.Second,
})
// err: *avf.HandshakeError (a denied WELCOME bad_hello/bad_signature/proto_version was already sent) or a
// TLS/timeout error (closed silently). The handshake deadline stays armed until Accept/Deny.
// p.FeederID, p.Pub, p.Body (verified; UUID normalised), p.Features (intersection)
ack := wm.Begin(p.FeederID, p.Body.SeqEpoch)                // avf.WatermarkStore (one per server)
c, err := p.Accept(avf.Welcome{Status: avf.StatusApproved /*or Pending*/, SessionID: sid, ResumeToken: tok,
    AckSeq: ack, Config: avf.WelcomeConfig{BatchMs: 50, Mode: avf.ModeFull, MLAT: true, ReturnResults: true}})
// or p.Deny(avf.CodeEnrolRequired, "…", 0)

seq := avf.NewSequencer(ack)
go c.RunServerKeepalive(ctx, seq.Watermark)                // PING every 5 s, ACK ≥ 1/s while records arrive
for {
    rec, err := c.ReadRecord()                              // payload valid until the next call
    // err: avf.ErrIdle (60 s silence), errors.Is(err, avf.ErrProtocol) -> c.Goodbye(avf.CodeProtocolError, …)
    if rec.Type.HasSeq() { s, _, _ := avf.SplitSeq(rec.Payload); fresh, err := seq.Accept(s) /* false = duplicate, drop */ }
    switch rec.Type { case avf.TFrames: var f avf.Frames; f.Decode(rec.Payload) … }
}
wm.End(p.FeederID, p.Body.SeqEpoch, seq.Watermark(), time.Now()); go-routine: wm.Sweep(now) every minute
rtt, offset, ok := c.Clock()                                // min-RTT PONG estimate (feeder − server host clock)
```

Flow: feed `avf.FlowMonitor{MaxFPS, MaxBytesPerSec}` once per second with queue fill and rates;
`FlowSend`/`FlowSendOverLimits` -> `c.WriteJSON(avf.TFlow, &avf.Flow{…})`, `FlowSelective` -> INTEREST
mode selective, `FlowDisconnect` -> `c.Goodbye(avf.CodeRateLimited, "", 300)`.
MLAT results: `c.WriteMLATResults(fixes)` (no station identities by construction).

## 2. Client (uplink)

```go
id, created, err := avf.LoadOrCreateIdentity(filepath.Join(dataDir, avf.IdentityFile)) // 0600
tlsCfg, err := avf.ClientTLSConfig(up.Host, up.ServerPin, nil)  // pin "" = WebPKI; mismatch -> avf.ErrPinMismatch
ring := avf.NewSendRing("", maxBytes)                            // seqEpoch kept across reconnects
c, res, err := avf.Dial(ctx, net.JoinHostPort(up.Host, port), tlsCfg, avf.ClientConfig{
    Identity: id, ServerID: knownServerID /*optional*/,
    Body: avf.HelloBody{Software: avf.SoftwareFeeder, Version: v, UUID: uuid, SeqEpoch: ring.Epoch(),
        ResumeToken: tok, EnrolToken: up.EnrolToken}})
// *avf.DeniedError -> show Welcome.Reason, wait de.RetryAfter(), avf.ActionFor(code) says stop/back-off
plan := ring.Resume(res.Welcome.AckSeq, time.Now(), 0)
// plan.NewEpoch: close, ring.Reset(), reconnect at once with the new seqEpoch and send plan.Stale as backfill.
// else: send plan.Replay unchanged (c.WriteRecord(h.Type, h.Payload)), THEN the RECEIVER records (new seqs),
//       then live data. (See HOOKS: RECEIVER-first contradicts gap-free replay.)
s := ring.NextSeq(); p, _ := avf.AppendFrames(buf[:0], &avf.Frames{Seq: s, …}); ring.Hold(avf.TFrames, s, p, now); c.WriteRecord(avf.TFrames, p)
// reader goroutine: c.ReadRecord(); PING is answered automatically; ACK -> ring.Ack(v);
// FLOW -> flow.Apply(f) (+ c.SetBatch(flow.Batch()) if changed); GOODBYE -> backoff.Override(reconnectIn)
// avf.ErrIdle = 30 s without PING/ACK -> reconnect.
var bo avf.Backoff // 1 s doubling to 60 s ±20 %; bo.SessionEnded(upFor) resets after 60 s
```

Backfill share: `avf.NewTokenBucket(avf.BackfillRate(liveBps, floor), burst)`; backfill records carry new seqs,
the late bit on every frame, wrapped in BACKFILL begin/end.

## 3. Numbers (i9-13900K, one core, `go test -bench . -cpu 1`)

| Path | frames/s per core |
|---|---|
| FRAMES encode (64 frames/record) | ~134 M |
| FRAMES decode | ~75 M |
| Client send: encode + framing + DEFLATE level 1 + flush/4 records | ~5.1 M (≈18 wire bytes per frame with random ME/CRC bytes) |
| Server receive: inflate + guards + framing + decode | ~9.6 M |

Live loopback run (`go test -c`, `AVF_LIVE_ADDR=127.0.0.1:18511 AVF_LIVE_FEEDERS=500`): 500 TLS sessions,
~950 k frames/s aggregate in one process with server and clients, no loss, seq/ACK exact.

## 4. Behaviour notes
- FLOW `batchMs` 0 / absent keeps the current interval; `slowFactor` 0 / absent means 1.
- FRAMES host times are carried in µs: decode gives `baseHostNs + 1000·Σ dHostUs`; keep `BaseHostNs` a multiple of
  1000 for exact round trips. `Frame.Channel` is the wire value (channel + 1, 0 = unknown).
- The ratio guard counts wire bytes below flate's read-ahead (≤ 4 KiB early), checked per 1 MiB of output.
- Go's TLS server cannot send `no_application_protocol` when a client offers no ALPN at all; the handshake is
  aborted from `GetConfigForClient` instead (the client sees a different alert). Clients offering other protocols
  get `no_application_protocol`.
- `PendingSession` keeps the handshake deadline armed until `Accept`/`Deny`; do registry work quickly.
