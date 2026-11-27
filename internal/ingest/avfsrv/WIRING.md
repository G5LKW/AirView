# internal/ingest/avfsrv: wiring notes (WP-I, DESIGN §4, AVF.md)

The native feeder listener (AVF/1 over TLS 1.3, TCP 30160). Aggregator mode only, and only when
`aggregator.avfPort > 0`. It uses `internal/avf` for everything on the wire, `internal/registry` for
admission, claims and the connection log, and `internal/ingest` for the data.

## 1. Construction (engine, WP-E)

```go
tlsCfg, pin, err := avfsrv.TLSFromConfig(dataDir, cfg.Aggregator) // operator cert (reloaded) or self-signed in <data>/avf-tls/
sid, err := avfsrv.LoadOrCreateServerID(dataDir)                  // <data>/avf-tls/server-id (see HOOKS: move into stations.db)
o := avfsrv.OptionsFromConfig(cfg.Aggregator)                     // ":<avfPort>", Policy (admission, limits, mlat, returnResults, name)
o.TLSConfig, o.ServerID = tlsCfg, sid
o.Resolver = reg.Resolver()              // notices approvals / revocations of connected stations (every 5 s)
o.Gate     = func() bool { return accounts.AdminReady(ctx) }   // DESIGN §7: closed until the admin exists
o.Hooks    = avfsrv.Hooks{OnReceiver: ..., OnGNSS: ..., OnMinuteStats: ..., OnInputStatus: ..., OnOnline: ...}
o.Now, o.Logger = ntp.Now, log.With("component", "avfsrv")
o.Policy.MOTD = "..."                    // optional
srv, err := avfsrv.New(o, reg, ing)      // reg: *registry.Registry (avfsrv.Registry), ing: *ingest.Ingest
go srv.ListenAndServe(ctx)
srv.SetPolicy(avfsrv.PolicyFromConfig(next.Aggregator))   // on ApplyConfig (a changed port needs a restart)
srv.Close()                              // GOODBYE shutting_down (reconnectIn 15) to every feeder, waits for the data
```

Show `pin` (and the serverId) on the admin page and in the install one-liner.

## 2. Session sequence

1. Accept: gate, `MaxUnauthPerIP` / `MaxConnsPerIP` / `MaxConnsPerSubnet` (/24, /48) / `MaxSessions`
   (`AllowCIDRs` exempt) - refused connections are closed before any TLS work and counted in
   `Stats().Reasons`.
2. `avf.ServerHandshake` (TLS 1.3, ALPN, Ed25519 signature bound to the exporter).
3. Rate-limit cool-down (`rate_limited`, remaining seconds), clone rule (`registry.CloneGuard`: other IP ->
   `cloned_feeder`; same IP -> the old session gets GOODBYE `replaced` and is waited for, so its
   watermark is final).
4. Feeder admission before any receiver is known: revoked feeder -> `revoked`; known approved feeder ->
   approved; else `open` -> approved, `approve` -> pending, `invite` -> `enrol_required` / `enrol_invalid`
   (`registry.CheckToken`, not consumed) / approved.
5. WELCOME: status, fresh resume token (10 min, memory), `ackSeq` from the shared `avf.WatermarkStore`,
   `mode` from `ing.DesiredMode()`, `mlat`, `returnResults`, MOTD.
6. Receivers of the previous session of the same seqEpoch are re-admitted at once (records replayed before
   their RECEIVER, AVF.md §5.2 strategy (a)); every new RECEIVER -> `registry.Admit` (consumes the invite
   token once; the feeder is approved for further receivers), per-IP station limit, `ConnOpen`, ingest
   `AddStation`, `SetTimestamps`, `Hooks.OnReceiver`. A denied receiver gets a NOTICE with the registry's
   code; when nothing else is attached and the code is final (revoked, enrol_*, limit_stations) the session
   ends with that GOODBYE. Pending receivers get NOTICE `pending` and later `approved`.
7. Reader: seq rule (`avf.Sequencer`: duplicates dropped, gap = `protocol_error`), copies each seq record
   into a pooled buffer and queues it (64 records, 4 MiB; a full queue back-pressures the TCP stream).
   CLAIM (10 s per session) -> `registry.MintClaim` -> CLAIM_CODE; GOODBYE -> session ends without the
   resume grace.
8. Worker (in order): RECEIVER, FRAMES (normalised: host time, ticks of the receiver's class unless
   `tsInvalid` or `timestamps:false`, epoch, channel, dBFS from the Beast signal byte, late bit inside
   BACKFILL; frames with the MLAT magic timestamp are refused; budget priority essential for selective
   stations, normal otherwise) -> `ing.Submit`; CLOCK_EVENT / CLOCK_PAIR / CLOCK_STATUS -> the ingest's
   clock observer; DECODED -> `ing.HandleUpdate` (map only, late inside BACKFILL); MINUTE_STATS,
   INPUT_STATUS, GNSS_POS (gnss feature) -> hooks. ACK reports what the worker has *processed*.
9. Monitor (1 s): PING/PONG offset -> `SetHostOffset`, wire bytes, queue fill, watermark; every 5 s
   `Seen` and admission changes (approved -> NOTICE, revoked/denied -> GOODBYE).
10. End: watermark stored (10 min), receiver table saved with it, `ConnClose` (bytes shared per station),
    clone guard released, ingest session closed with a 30 s grace unless the client said GOODBYE.

FLOW and INTEREST come from the ingest session (`ingest.Controller` is implemented by the connection);
INTEREST is sent only with the `selective` feature.

## 3. Engine API

| Call | Use |
|---|---|
| `srv.SendResults(st, fixes)` | MLAT_RESULTS (≤ 1000 per record) to the station's feeder; the caller applies the contribution rule; `false` = no session / results not wanted |
| `srv.SendFeedback(st, avf.Feedback{...})` | FEEDBACK (own station only; `RxNo` is filled in) |
| `srv.Notice(st, code, level, text)` | NOTICE (e.g. `location_suspect`, `clock_class_downgraded`) |
| `srv.Disconnect(feederID, code, reason, reconnectIn)` | admin kick / revocation |
| `srv.Sessions()` | admin view; contains the raw remote IP: **admin scope only** |
| `srv.Stats()` | counters and refusal reasons |

## 4. Tests and the live run

```
go test ./internal/ingest/avfsrv          # handshake/admission/limits/resume/flow/claims against a real stations.db
go build -o <scratch>/avfsrv-load ./internal/ingest/avfsrv/cmd/avfsrv-load
avfsrv-load -serve 127.0.0.1:18521 -data <dir> -duration 75s [-heapprofile heap.pprof]
avfsrv-load -load  127.0.0.1:18521 -data <dir> -feeders 1000 -aircraft 300 -duration 55s
```

Measured (i9-13900K, Windows 11, loopback, server and 1,000 simulated feeders in separate processes, real
registry + ingest + tracker + lanes, open admission): 1,000 sessions, 0 handshake failures,
~520 k frames/s (2.3 k unique transmissions/s, ~220 stations per transmission), server CPU 0.9-1.3 cores,
0 dropped / too old / shed frames, live heap 0.7-1.2 GB. The heap profile shows ~0.7 MB per session in
`compress/flate` writers of the server→client stream (`internal/avf`), the bulk of the memory; see HOOKS.md.

## H-mlatc additions

- `SessionInfo.Results` (all receivers of the connection) next to `SessionStation.Results`.
- `Options.HostLatencySlack` (default 300 ms, < 0 = off): host-time plausibility against negative fake host
  offsets (finding ROB-1, `hostguard.go`). Visible as `SessionInfo.HostClampMs` and `Stats.HostClamped`.

## WP-I2: selective-mode MINUTE_STATS merge (WP-UPLINK-multi)

In selective mode (DESIGN §4.4) and under FLOW, a feeder sends only part of its frames. Counters built from the
frames that reach the aggregator (lane stats, message and position rates, the DF mix) then undercount the
station. MINUTE_STATS carries exact per-minute counters, counted at the feeder before any filtering. `minutes.go`
merges them with what the server received:

```go
o.Hooks.OnStationMinute = func(m avfsrv.StationMinute) {
    // upsert by (m.Station, m.Minute): a later call for the same minute carries the sum of all parts
    c := m.Merged()          // per field max(exact, received): frames, modeAC, positions, DF[0..24]
    add := m.Shed()          // exact - received (>= 0); add to counters built from the received frames
    _ = m.Exact.ICAOs        // unique aircraft (lower bound when m.Records > 1), m.Exact.CRCFail, m.Exact.RSSI
    _ = m.Selective          // some of the minute arrived in selective mode
    _ = m.Late               // inside BACKFILL or more than 2 minutes old
}
```

- **Counting:** only when `OnStationMinute` is set. The worker counts the frames of each FRAMES record it hands
  to the ingest (late ones included). Frames are binned by feeder host time, per station, in a window of 16
  minutes shared by every connection of the station. The window survives reconnects and is dropped 21 minutes
  after the last use (janitor). Cost: `BenchmarkCountFrames` is about 3.6 ns per frame, with 0 allocations and one
  lock per FRAMES record. Memory is about 4 KB per station (about 4 MB for 1,000 stations).
- **Order:** the feeder (internal/uplink) sends a minute's frames before its MINUTE_STATS, and the worker is in
  order, so `Received` is complete when the record arrives. `OnStationMinute` is called from the worker before
  `OnMinuteStats` (which is unchanged).
- **Parts:** a feeder that restarts or reconnects inside a minute sends two records. They are added up (DF,
  Mode A/C, positions, CRC failures, RSSI; ICAOs = the larger value) and re-published with `Records` = 2.
- **Out of window:** records for minutes older than the window (backfill after a long outage) have
  `ReceivedKnown=false`. `Merged()` is then the exact counters and `Shed()` is zero. Such records are not
  merged with each other. A record far in the future (a bad feeder clock) does not disturb the window.
- **Stats:** `Stats.StationMinutes` counts the published records.
- **Tests:** `TestSelectiveMinuteStatsMergeEndToEnd` runs a real `internal/uplink` feeder against this server in
  selective mode: 72,000 frames in a minute, 15 % sent. Exact equals truth; Received equals an independent replay
  of the selective filter; Merged equals truth. Also `TestStationMinutesWindow` and `TestMinuteCountsArithmetic`.
- **Consumer:** the engine's per-station statistics (requested in HOOKS.md).
