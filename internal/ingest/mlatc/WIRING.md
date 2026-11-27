# internal/ingest/mlatc: wiring notes (WP-B3, DESIGN §5.2)

Compat server for unmodified **mlat-client** feeders on TCP **31090**. Timed receptions go into the station-aware
ingest (`internal/ingest`) and from there to the MLAT engine; results come back per contributing station.
Protocol description: `docs/aggregator/MLATC.md`. Maths and conventions: `doc.go`.

## 1. Construction (engine, aggregator mode, `aggregator.mlatPort > 0`)

```go
opt := mlatc.OptionsFromConfig(cfg.Aggregator)    // addr ":<mlatPort>", limits, handshake timeout, DisableResults, MOTD
opt.ServerKey = reg.IngestKey()                   // persistent HMAC key for UUID-less clients
opt.Now       = ntp.Now                           // server clock (GPS day resolution, rate accounting)
opt.Logger    = logger.With("component", "mlatc") // never logs raw IPs
opt.Gate      = func() bool { return accounts.AdminReady(ctx) } // DESIGN §7
opt.Hooks     = mlatc.Hooks{OnSeen: planner.Seen, OnLost: planner.Lost, OnRates: planner.Rates, ...} // optional
opt.Stats     = clockFilter                        // optional mlatc.StatsSource ("stats" messages)
opt.Trust     = gate                               // optional mlatc.TrustSource (internal/registry/trust): probe requests
// opt.SplitSync: SplitSyncAuto (default, offered with per-identity fallback) | SplitSyncOn | SplitSyncOff
srv, err := mlatc.New(opt, regAdapter, mlatc.FromIngest(ing))
go srv.ListenAndServe(ctx)                         // returns nil after ctx cancel / srv.Close()
srv.SetPolicy(mlatc.PolicyFromConfig(next.Aggregator)) // on ApplyConfig
srv.Close()                                        // disconnects, waits until every client is detached
```

Results (MLAT engine, WP-M): for each fix, for each **contributing** station `st` that is an mlatc station,
`srv.SendResult(st, mlatc.Result{MLATFix: fix, Callsign: cs, Squawk: sq})` (or `srv.SendResults(st, fixes)` —
the same signature as `avfsrv.Server.SendResults`). It returns false when the station has no client, the
client asked for no results, results are disabled, or the client's output queue is full.

## 2. Registry contract (`mlatc.Registry`)

`Attach(ctx, Identity) (Attachment, error)` / `Detach(Identity, Attachment)` (exactly once per admitted attach,
after the client's last frames were delivered). Suggested adapter over `internal/registry`:

| Identity | registry call |
|---|---|
| `UUID != ""` | `Admit{Kind: mlatc, UUID, Name: User, ClockCls, PrivacyOff}` |
| `UUID == ""` | `Admit{Kind: mlatc, IdentKey: reg.UnidentifiedKey(RemoteIP, port) [+ "/<Slot>" when Slot > 0], ...}` |
| `Located` | `SetDeclared(idx, {Lat, Lon, Alt}, model.AltRefAmbiguous)` when the owner has not set a position |
| `Privacy` / `PrivacyOff` | name and pin per DESIGN §19.2 (`PrivacyOff` = region pin + name for new stations) |

`Attachment{Station, FeedID, Admission (approved|pending), EpochBase, NoTimestamps}`. Denied/revoked → the
client gets `{"deny":[Reason]}` and a cool-down. `ConnOpen/ConnClose` and `Seen` can be called from
`Hooks.OnAttach/OnDetach`.

## 3. Sink contract (`mlatc.Sink`, `FromIngest(*ingest.Ingest)`)

- `Submit(frames)`: timed frames of one station (Station, FeedID, Epoch, ClockCls, Ticks in the AirView
  convention, HostNs = server arrival time, GPSNs −1, Signal NaN, Channel −1). Never map-only.
- `ClockEvent(st, ev)`: before the first frame of every epoch (connection start: `restart`; then
  `mode_change`, `jump`, `backwards`, `sample_drop`, `restart`).
- `SetTimestamps(st, !NoTimestamps)` at attach.
- `OpenSession({Proto: "mlatc", Controller: conn, Selective: true})`: the ingest's full/selective policy and
  `ing.SetInterest(st, ...)` reach the client as start_sending/stop_sending; FLOW is a no-op;
  `Disconnect("rate_limited", ..., n)` cools the client down n seconds.

## 4. Observability (admin scope: contains addresses and UUIDs)

`srv.Stats()` (clients, lines, frames, syncs, mlats, drops by cause, epochs, results, close reasons),
`srv.Links()` (per client: station, UUID, user, clock type/class, compression, mode, seen/requested sizes,
rates, results, IP), `srv.Online(st)`, `srv.Disconnect(st, coolDown)`, `srv.StartSending/StopSending`.
`server.LinkSource` for Feed Check can be built from `Links()` (proto "mlatc").

## 4b. rate_report, split sync, offline sessions

- `srv.ADSBRate(st, addr)` / `srv.ADSBRates(st, dst)`: the client's last rate_report (ADS-B position msgs/s per
  aircraft) — for the planner's sync-set selection (`Hooks.OnRates` delivers each report as it arrives).
- `LinkStats`: `Results` / `ResultsDrop` (owner "results received"), `SplitSync`, `SSyncs`, `ADSBAircraft`,
  `ADSBRate`. `Stats`: `SSyncs`, `SplitFallbacks`. `srv.SplitSyncFallbacks()` lists identities answered without
  split_sync.
- `srv.Offline(handshake, ip, now)` → `Lines(data, now)`, `Poll(now)`, `Close()`: a session without a socket
  (capture replay, `mlatcsim`).

## 4c. UDP transport and Mode A/C requests (phase 5, WP-MLATC-udp)

- **Enable:** `opt.UDP = cfg.Aggregator.MLAT.UDP` (`OptionsFromConfig` maps it). `srv.Serve(ctx, ln)` then opens
  the datagram socket itself on the TCP listener's address and port (`opt.UDPAddr` overrides); a bind error is
  logged, kept in `srv.UDPStatus()` / `Stats.UDPError`, and clients stay on TCP. `srv.ServeUDP(pc)` serves a
  socket of your own instead. `srv.Close()` closes it. The firewall must open UDP on the same port.
- **Reply:** a client offering `udp_transport: 2` gets `[host, port, key]`. Behind NAT / Docker port mapping set
  `opt.UDPHost` (public name or address) and, if the external port differs, `opt.UDPPort`; otherwise the host is
  the local address the client's TCP connection reached.
- **Source check:** datagrams must come from the TCP client's address; `opt.UDPAnySource = true` lifts it (CGNAT
  with several egress addresses). `opt.UDPReaders` (default min(4, GOMAXPROCS)) goroutines read the socket.
- **Observability:** `Stats.UDPActive`, `UDPPackets`, `UDPBytes`, `UDPUnknownKey`, `UDPBadSource`, `UDPBad`,
  `UDPDup`, `UDPLost`, `UDPLate`, `ModeACFrames`; `LinkStats.UDP`, `UDPPackets`, `UDPLost`, `UDPDup`, `UDPBad`,
  `ModeAC`, `ModeACFrames`.
- **Offline / replay:** with `Options.UDP` an Offline session that offers `udp_transport: 2` gets a key
  (`off.UDPKey()`); `off.Datagram(b, now)` processes a recorded datagram.
- **Client side (tests, simulators):** `AppendUDPHeader`, `AppendUDPMLAT` (7/14-byte frames, 2-byte Mode A/C padded),
  `AppendUDPSSync`, `AppendUDPSync`, `AppendUDPAbsSync`, `AppendUDPRebase`.
- **Mode A/C (engine, DESIGN §9.9):** after each replan, for every mlatc station
  `srv.SetModeACAll(st, planner.ModeACEligible(st))` (or per code `StartSendingModeAC` / `StopSendingModeAC`).
  The return value says whether the client receives the request (`"modeac": true` in its handshake, or
  `opt.ModeACAllClients`). Replies arrive in the ingest as `model.FrameModeAC` frames with the station's ticks.

## 5. Tests and tools

```
go test ./internal/ingest/mlatc                         # unit, interop (scripted client), simnet truth, ingest e2e
go test ./internal/ingest/mlatc -run '^$' -bench .      # per-line cost, result encoding
go test ./internal/ingest/mlatc -run '^$' -fuzz FuzzParser -fuzztime 60s
go test ./internal/ingest/mlatc -run '^$' -fuzz FuzzZlib2  -fuzztime 60s
go test ./internal/ingest/mlatc -run '^$' -fuzz FuzzDatagram -fuzztime 60s
go test ./internal/ingest/mlatc -run 'UDP|ModeAC' -v              # UDP interop (scripted client over real sockets), Mode A/C
go run ./internal/ingest/mlatc/cmd/mlatc-load -serve 127.0.0.1:18606 -duration 80s -results 1
go run ./internal/ingest/mlatc/cmd/mlatc-load -load  127.0.0.1:18606 -clients 1000 -rate 300 -duration 60s
# UDP transport: add -udp on both sides. Measured (WP-MLATC-udp, loopback, separate processes, 60 s):
# 1,000 clients x 300 frames/s -> 300k frames/s delivered, 0.16-0.27 CPU cores, 73 MB heap, 0 datagrams lost or bad
go test ./internal/ingest/mlatc/mlatcsim -run TestMixedHoldout -v    # AVF vs mlat-client hold-out (virtual time)
MLATCSIM_SEEDS="31 32 33" MLATCSIM_DUR=12m go test ./internal/ingest/mlatc/mlatcsim -run TestMixedHoldout -v
MLATCSIM_WORLD=sdr ...                                              # the RTL-SDR world of the WP-E2 report
MLATCSIM_E2E=1 go test ./internal/ingest/mlatc/mlatcsim -run TestRealTimeDiagnostic -v  # real time, real engine
```

`mlatcsim` (package `internal/ingest/mlatc/mlatcsim`) is test tooling: one simnet world through the real ingest,
the real mlatc server (Offline sessions with an emulated client: pairs disjoint/overlapping or ssync) and the
real MLAT engine in virtual time; optional `Wrap` puts a gate (the timing quarantine) between ingest and engine.

Numbers: see the WP-B3 section of `docs/aggregator/PROGRESS.md`.
