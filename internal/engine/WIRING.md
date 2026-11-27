# internal/engine: wiring notes (WP-E, DESIGN §1, §2, §6, §20.1)

The engine owns every lifecycle. Standalone mode is unchanged; everything below is
opt-in through `mode: "aggregator"` or `uplinks[]`.

## Modes

| Stored mode | Effective mode | What runs |
|---|---|---|
| `standalone` | standalone | today's station; uplinks when `uplinks[]` has enabled entries |
| `aggregator` | aggregator (fixed at `Start`) | registry, accounts, ingest core, lanes, host station #1, Beast 30004, SBS-in bridge, uplinks |
| `remote-data` / `remote-web` | unchanged | no uplinks, no network |

- `-demo` (`Options.Demo`) always forces standalone, whatever the file says.
- Switching to or from aggregator mode is saved but needs a restart: `RestartRequired()` turns
  true and `Mode()` keeps reporting the mode the engine started in.
- `Options.ListenHost` (`-listen-host`, `$AIRVIEW_LISTEN_HOST`) is the bind address of the
  feeder listeners (`""` = every interface). The SBS-in bridge always binds `127.0.0.1`.

## Aggregator start-up order (network.go)

1. `OpenRegistry` (`<data>/stations.db`) and `auth.NewService` on its stores
   (`AccountOptions(cfg.Aggregator)`). A registry that cannot be opened is a start error.
2. The tracker is built with `Aggregator.Lanes.MaxRangeKm` / `Limits.MaxAircraft`, then
   `tracker.NewLanes(LaneOptionsFromConfig)` + `SetNetwork(reg.Resolver(), lanes)`.
3. With `aggregator.mlat.enabled` (default on) the MLAT engine is built first (`mlatwire.go`,
   `mlat.New` with the whole `aggregator.mlat` block; Updates = the ingest, Results = the current
   AVF/1 and mlat-client listeners, PlanOut = ingest interest + mlat-client start/stop_sending) and
   wired into the ingest options (`MLAT`, `Clock = eng.ClockView()`, `ClockObserver`,
   `LearnExclude` = the hold-out set). A change of `aggregator.mlat` sets `RestartRequired`.
   `ingest.New` (Handler = tracker, Resolver = registry, Now = `ntp.Now`) and `Run`; the host
   station gets its own ingest session (`Proto "host"`). If the core cannot be built the engine
   falls back to its minimal grouper (`netingest.go`, map and lanes only, no MLAT groups) and the
   MLAT engine is closed. After the core runs, `eng.Run` and the station sync loop start: every 5 s
   the approved stations of the registry are mapped (`mlatStationInfo`: GNSS, confirmed, declared
   with MSL→HAE through the geoid, ambiguous mlat-client heights with σ + |N|, suspect +
   discrepancy) and pushed with `SetStation` when changed; stations that are gone are removed.
4. `reg.EnsureHost` (idx 1, kind host, approved). Name = `receiver.name` (default `host`),
   location = the configured receiver (MSL converted to HAE with the geoid when available) or the
   receiver's own GNSS fix; status `declared` / `gnss`; `Anchor` = located.
5. The maintenance loop (every 2 s until the admin exists, then every 15 s): admin gate, host
   owner (`EnsureHost(name, adminID)`), host location/name, host last-seen, retiring idle
   per-station runtimes.

## The sink tee (engine.go `tee`)

`feed.Manager` → `tee{tracker | hostSink, hub, uplinks}`:

- standalone: `tracker.HandleFrame/HandleUpdate` (dedup in the tracker, as before);
- aggregator: `hostSink` stamps `Station = 1` and `FeedID = st/<hostSID>/<n>` (n = 1-based
  position in `feeds[]`, 0 for unconfigured sources) and calls the ingest;
- the hub and the uplinks get the frame with its local feed ID. The uplink manager drops MLAT
  results, demo, hub and `noForward` feeds itself.
- Feed clock events go to the uplinks (`HandleClockEvent`) and, in aggregator mode, to
  `ingest.ClockEvent(HostStation, …)`. Radarcape GNSS fixes go to `uplink.HandleGNSS` (HAE).

## Listeners (runtime part; restarted by a factory reset, follow ApplyConfig)

| Listener | Config | Notes |
|---|---|---|
| Beast compat | `aggregator.beastPort` | `beastin.OptionsFromConfig`, `ServerKey = reg.IngestKey()`, `Gate` = admin ready, `Budget` = ingest budget, registry adapter `reg.Beast()`. Port change = new listener; `SetPolicy` otherwise. |
| SBS-in bridge | `aggregator.sbsInLocalPort` | 127.0.0.1 only; BaseStation positions → `HandleUpdate` with `MLAT`, `Src = mlat-ext`, station 0. |
| AVF/1 | `aggregator.avfPort` | `avfsrv.New(opts, reg, ingestCore)`: TLS from `aggregator.tlsCert/tlsKey` or the self-signed certificate in `<data>/avf-tls/` (`TLSFromConfig`), network ID in `<data>/avf-tls/server-id`, `Gate` = admin ready, clone guard and seq watermarks kept across listener restarts, `SetPolicy(PolicyFromConfig)` on ApplyConfig; a port or certificate-path change restarts it. The SPKI pin is in `NetworkStatus().AVFPin` and the log. RECEIVER locations become the station's declared (or GNSS) effective position unless a survey result is in charge (`onAVFReceiver`). |
| mlat-client | `aggregator.mlatPort` | Only while the MLAT engine and the ingest core run. `mlatc.OptionsFromConfig`, `ServerKey = reg.IngestKey()`, `Gate` = admin ready, `Stats` = the MLAT engine, `Hooks.OnSeen = eng.NoteSeen`, registry adapter `reg.MLATC()` (UUID or IP key, handshake position stored as declared with `altRef ambiguous` when the station has none), sink `mlatc.FromIngest(core)`. Port change = new listener; `SetPolicy` otherwise. |

Feeder listeners accept connections only after an admin with a confirmed authenticator exists
(DESIGN §7); until then cmd/airview prints the one-time setup token on stdout.

Shutdown order (`Stop`): listeners (they wait until their last frames reached the ingest) →
feeds → ingest flush → hub → loops → tracker + lanes flush → uplinks (spool) → network loops,
host session, station runtimes → registry → database.

## Accessors for the server and cmd

`Aggregator()`, `Registry()`, `AVFIngest()` (server + pin), `MLAT()` (the MLAT engine, nil when off), `MLATCIngest()`, `Accounts()`, `Lanes()`, `NetIngest()`, `BeastIngest()` (admin
scope: links carry IPs), `Stations()` (per-station runtime LRU), `AdminReady()`, `HostSID()`,
`Uplinks()` (an `http.Handler`, nil until an uplink was configured), `NetworkStatus()` (operator
view: listeners, counters, tracker lock statistics; admin scope), `SetFlightStationSink(fn)`.

cmd/airview installs the server side with `srv.SetAccountService(eng.Accounts())` and
`srv.SetNetwork(server.Network{Registry, Beast, Links, Uplinks})`, using adapters that ask the
engine on each call (the Beast listener is replaced on a port change; the uplink manager is
created on first use).

## Per-station runtimes (stations.go, DESIGN §6.5)

`StationRuntimes`: LRU (32) of per-station component sets, created by `Acquire(meta, now)`,
retired after 30 minutes without lane activity or when evicted. Components come from
`Register(name, StationFactory)`; none is registered yet (the analytics/latency/rfstat per-station
constructors are pending, HOOKS).

## Uplinks (uplinks.go)

Created on first need (`uplinks[]` with an enabled entry, mode standalone or aggregator) with
`StateDir = <data>/uplink`, `Source` = `feed.Manager.Forwardable` + receiver descriptions built
from the feed list (stable `RxNo` = feed position + 1, name, model, antennas of the feed,
location from the configured receiver or its GNSS), `Inject` = the tracker, tunables from
`uplinkTuning`. Kept for the engine's life (identity and spool stay open); `Apply(nil)` when no
uplink is configured; `Close` on Stop.

## MLAT metrics (mlatmetrics.go, WP-E2)

With the MLAT engine running, a tap in front of it (`ingest.Options.MLAT = mlatTap{eng}`) counts each station's timed
copies and groups (one atomic add per copy, 0 allocations), and a recorder goroutine writes once a minute (samples at
:00.2, :10.2, …):

| Table | DB | Rows |
|---|---|---|
| `mlat_minute` | airview.db | stations active in the minute + the network row (sid "") |
| `station_clock_minute` | stations.db | every station the clock filter knows |
| `mlat_quality_hour` | airview.db | hold-out error histograms (published hold-out fixes vs the ADS-B reference) |

Retention: the hourly `store.Maintain` pass (`aggregator.storage` tiers); station deletion: `DeleteExtra`. Definitions
and the derivation of the per-station fix/outlier counts: package documentation "MLAT metrics". The recorder is the
only consumer of `mlat.Engine.Fixes()`.

## MLAT configuration (WP-E2)

`aggregator.mlat.enabled` / `returnResults` / `holdout` (and the sub-blocks) come from config.json, the operator file,
`AIRVIEW_AGGREGATOR_MLAT_*` and `-set`; `airview config check` warns about MLAT without timed listeners, a
mlat-client port without the engine, the SBS-in bridge beside the own MLAT and `holdout: 0`. `returnResults` off is live;
every other change of `aggregator.mlat` sets `RestartRequired`. The SBS-in bridge follows `sbsInLocalPort` live (0 = off).

## Not wired yet (see HOOKS.md)

- AVF hooks other than OnReceiver (GNSS_POS, MINUTE_STATS, INPUT_STATUS, OnOnline) are not used yet.
- Lane statistics go to `airview.db` (keyed `ant = s<idx>`) until internal/store can open
  `stations.db` with the station migration subset; `flight_station` rows are dropped until
  internal/store provides the writer (install it with `SetFlightStationSink`).
- (Mode A/C and the survey are wired: see "Phase 4-5 (WP-WIRE-45)" and "Phase 3 (WP-W3-wire)" below.)
- Exact per-station fix/outlier totals from the MLAT engine (HOOKS WP-E2 -> internal/mlat); until then they are derived.

## Tools

`go run ./internal/engine/cmd/airview-netsim -beast HOST:PORT -avf HOST:PORT -pin sha256/... -host-listen ADDR`
drives an aggregator with a real-time simnet network: a host Beast stream, compat Beast feeders
and native AVF feeders (one identity each), and with `-mlatc ADDR -mlatc-feeders N` mlat-clients (reference timestamp
convention). `-mix gps` makes the timed feeders GPS Radarcapes. The feeders live in `internal/engine/simfeed`, which the
end-to-end test `TestMLATEndToEnd` uses as well.

## Integrator after the hardening wave (H-clock, H-mlatc, H-track wiring)

- Timing quarantine: `trust.New(eng, trust.RegistrySource{R: reg}, trust.ConfigFromModel(cfg.Aggregator.MLAT), trust.RegistryPromoter(reg, log))` sits between the MLAT tap and the engine; `mlatc.Options.Trust` gets the same gate; `Engine.TrustGate()`; `NetworkStatus().Trust`.
- mlat-client `rate_report` (`Hooks.OnRates`): aircraft with a non-zero ADS-B position rate go to the planner as heard by the station (`eng.NoteSeen`).
- AVF RECEIVER positions go through `registry.FeederPosition` (`aggregator.positions`: jitter, interval, hold of large moves; PRIV-1); GNSS positions are converted to HAE first.
- `mlatStationInfo`: an ambiguous mlat-client altitude (MSL or HAE) uses the midpoint with σ = hypot(30, |N|/2).
- `mlat.Options.Notifier` = `posNotices` (mlatnotice.go): survey-related position-source changes are kept per station for the owner view (`Engine.MLATPositionNotice`, station detail `mlatPosition.lastChange`) and audited as `station.mlat.position` (no coordinates).
- `mlat.Options.Weather` = a `ref.BundleWeather` fed from the host's weather poller (`onWeather`); stations far from the host have no bundle yet.
- Not wired (measured or carried, see HOOKS.md): the adaptive flush estimator (`ingest.Options.Flush`), short frames in relative components; the gate view is a switch (`aggregator.mlat.engine.ingestGate`, default off).

## Phase 3 (WP-W3-wire): survey, location, Locate Me v2, self-update

- **Survey** (surveywire.go): `survey.New` beside the MLAT engine, `survey.Tee` in front of the timing quarantine, stations from the registry sync loop, results to the engine (AutoUseSurvey: `aggregator.mlat.autoUseSurvey` / `autoUseMaxSigmaM`), the registry (estimates, declared-suspect status) and the audit log (owner notices). `Engine.Survey()` for the server routes.
- **Location follows the survey** (surveyloc.go, `reconcileSurveyLocations`, run in the MLAT station sync loop): when the MLAT engine uses a survey position for a station without a position of its own, the registry's effective position becomes that position with source/status `estimated` (audited `station.location.survey`, no coordinates); when the engine stops using it (owner rejection, later unstable or suspect survey, AutoUseSurvey off) the station is unlocated again; a running survey of an unlocated station shows `estimating`. Declared, GNSS, confirmed and suspect stations are never touched. Idempotent.
- **Locate Me v2 seeds the survey** (surveyloc.go `runSurveyHints`): every 10 minutes, at most 10 stations per pass and once an hour per station, the station's Locate Me v2 estimate (r95 <= 60 km) becomes `survey.Station.Hint`.
- **Locate Me v2** standalone (locatewire.go) and aggregator recorder for unlocated stations (network.go); owner/admin route `GET /api/me/stations/{sid}/locate`.
- **Server**: `/api/me/stations/{sid}/notices`, `/api/me/notices` (owner notices from audit rows), `/api/admin/mlat/surveys` (survey console + location quality), `/api/update` (+ `/check`, `/install`) for the self-update of airview.
- **Self-update**: `cmd/airview/updatectl.go` (web control, headless only installs), `cmd/airview-feeder/updateapi.go` (loopback `/api/update*` and the `/update` page), Pi installer `--update` (deploy/feeder/pi).
- **Acceptance tests**: `TestSurveyAutoUseEndToEnd` (`AIRVIEW_E2E_SURVEY=1`; real sockets, 6 located GPS feeders + 1 unlocated: surveyed, auto-used and located "estimated" in ~2.3 min, 4.5 m from the truth with σh 10.6 m), `TestSelfUpdateE2E` in cmd/airview (`AIRVIEW_E2E_UPDATE=1`: 1.0.0 -> 2.0.0 committed, broken 3.0.0 rolled back). `airview-netsim -unlocated N` makes the last N AVF feeders declare no position.
## Phase 4-5 (WP-WIRE-45): feature map, configuration and tests

Every phase 4-5 library is reachable from the products through the config file, `AIRVIEW_*` variables, `-set`
and `airview config check` (docs/CONFIG.md, generated). Defaults leave standalone stations and feeders unchanged
(`TestWire45StandaloneDefaults`: no uplink manager, no SSR, unsharded tracker, no new key in config.json).

| Feature | Settings | Where it runs | End-to-end test (real sockets) |
|---|---|---|---|
| Federation (peers, out-feeds) | `aggregator.federation.*` (`AIRVIEW_AGGREGATOR_FEDERATION_*`, indexed peers/feeds); `airview federation id` | federation.go `syncFederation` (aggregator only; rebuilt on change) | `TestWire45Federation`: two aggregators, A's picture reaches B (overlay only, B's tracker stays empty), A's Beast out-feed serves bytes, switching off closes the node |
| Multi-uplink (AVF/1 + Beast "also feed others") | `uplinks[].type` avf/beast, `format`, `uuid`, `reduceMs` | uplinks.go `syncUplinks` (`uplink.SplitUplinks` / `Manager.ApplyAll`); feeder `applyLocked` | `TestWire45BeastOutput` (station -> third-party Beast sink, removal stops it); feeder `TestFeederRTLTCPEndToEnd` (AVF + Beast at once) |
| RTL-SDR via the user's own rtl_tcp | feed / input type `rtltcp`, `feeds[].sdr` / `receivers[].input.sdr` (gain, ppm, biasTee, sampleRate, spawn, rtltcpPath, device, acceptUnverifiedAp); feeder `setup -input-type rtltcp -sdr-*`, `detect -sdr` | `sdrfeed.Factory` registered in engine.go and the feeder | `TestWire45RTLTCPStationFeed` (station: demodulated frames -> tracker position), `TestFeederRTLTCPEndToEnd` |
| WinUSB | none: the transport exists but there is no clean-room RTL2832U/tuner driver (internal/sdr/winusb/doc.go), so dongles are used through rtl_tcp; `airview-feeder detect -sdr` lists RTL-SDR USB devices and whether the WinUSB driver that rtl_tcp needs is bound | cmd/airview-feeder/detectsdr.go | `TestDetectSDR` |
| Mode A/C MLAT on/off | `aggregator.mlat.modeAc`, `aggregator.mlat.engine.modeAcCodeFormat`, `modeAcJitterNs`, `aggregator.mlatc.modeAcAllClients` | mlatwire.go (`ingest.Options.ModeAC` -> `ObserveModeACFrames`, `ModeACOut`) | `TestWire45ModeACOnOff` (on: AVF Mode A/C replies reach the engine; off: stage disabled, nothing routed) |
| Quality: k_atm, ADS-B verification | `aggregator.mlat.engine.applyKAtm` (applied only when the estimator is Ready), `verifyEverySec` (-1 = off) | internal/mlat options.go | internal/mlat tests |
| Quality: latency calibration | `aggregator.mlat.latencyCal.*` (enabled default false, minGain, zEnable, zDisable, treatFraction, minCalSamples, minArmSamples, minArmAircraft, windowH, priorSigmaS, calibratedSigmaS) | mapped by internal/mlat (`latencyCalParams`, internal/mlat/latcal.go): hold-out fixes score the A/B arms, `Decide` once a minute, report in `Stats.latencyCal`; no learning solves yet (planner budget, HOOKS), so the decision stays "insufficient" and nothing is applied | `TestLatencyCalBlock`, coverage test |
| Bundle adjustment schedule | `aggregator.mlat.survey.bundle.*` (`disabled`, `everyMin`, `spanMin`, ...) | bundlewire.go | config tests; integrator live smoke |
| Sharded tracker selection | `aggregator.limits.trackerShards` (0 = auto, 1..256) | the tracker splits itself on `SetNetwork` (aggregator only) | `TestWire45TrackerShards` (aggregator 4 shards; standalone ignores it) |
| mlat-client UDP listener | `aggregator.mlat.udp`, `aggregator.mlatc.udp*` | mlatwire.go `syncMLATC` (listener restart on change) | integrator live smoke |
| SSR self-location (experimental, off) | top-level `ssr.*` (`AIRVIEW_SSR_*`, `ssr.sites[]`, `conspicuityCodes`, `feedId`, `solveEveryMin`, `modeAcCodeFormat`, every ssr.Config tunable) | ssrwire.go (below) | `TestWire45SSREngine` (enabled at run time, paced Beast stream from ssrsim, solve on request: 18 m from the truth, receiver position untouched, off stops it); `TestSSRWiringBaseline` (decoder vs direct reference: 3.5 m vs 5.6 m) |

### Experimental SSR self-location (ssrwire.go)

- `ssr.enabled` (default false) builds an `ssr.Estimator`; `syncSSR` runs at start, after every configuration change
  and after a factory reset; a changed block rebuilds it, off closes it. The feed tee pays one atomic load per frame
  while it is off.
- One receiver clock: frames of `ssr.feedId` (else of the first feed that delivers timestamped frames). Receiver
  time in seconds from the GPS ns of day (unwrapped across midnight), the 48-bit 12 MHz counter (unwrapped) or the
  network ticks; a jump of more than 60 s restarts the estimator (receiver restart).
- Decoding (our own, with `internal/modes`): DF17/18 airborne positions (global CPR from an even/odd pair within
  10 s, then local relative to the aircraft's last position, 20 km jump guard), barometric altitude plus the TC19
  GNSS-baro difference when known, NACp -> sigma, Mode A from DF5/21 and ES; Mode A/C replies (`pulses` or `nibbles`
  payload) as octal identities; DF11 with the II/SI code. Per-aircraft state bounded to 4,096 entries, pruned after
  120 s. Cost: `BenchmarkSSRObserve` 104 ns per frame, 0 allocations (budget 1 us).
- `Process` once a second, `Solve` every `ssr.solveEveryMin` (default 10 min) and on `Engine.SSRSolveNow()`; the
  search centre is the configured receiver position when there is one.
- `Engine.SSRStatus()` (JSON-safe DTO: counters, estimator stats, last result with interrogators) is for the owner /
  admin only. The result is never written to the receiver position, the registry or anything public
  (DESIGN §8, §19.3). Routes: HOOKS "WP-WIRE-45 -> internal/server". The Beast feed decides GPS vs 12 MHz
  timestamps only after 5 s of wall time, so a recorded stream must be replayed paced (the test does).
- Not in airview-feeder: the feeder's stations are located by the aggregator's survey; the estimator runs in
  AirView (standalone, and the host feeds of an aggregator).
