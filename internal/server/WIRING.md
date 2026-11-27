# internal/server — aggregator-mode wiring (WP-H)

Design: `docs/aggregator/DESIGN.md` §8 (privacy), §11 (views), §20.1 (headless operation).

Standalone mode is unchanged: every client sees every answer as before; only mutating
routes need the admin (Route.Admin). Everything below applies in aggregator mode.

## Files

| File | Contents |
|---|---|
| `access.go` | `Access` classes, `StationSel`, the route gate (scope check, public rate limit, station selection), per-viewer cache keys. |
| `routes_net.go` | The network route table (public, owner, admin, uplinks, `/healthz`, `/data/*.json`). |
| `api_view.go` | Per-viewer projections of the legacy live routes; the shared public snapshot (1 s); island guard. |
| `api_public.go` | `/api/public/*`, `/data/aircraft.json`, `/data/receiver.json`. |
| `api_join.go` | `GET /api/public/join`: the feeder page's facts (public host, open listener ports, admission, registration, backup days for consent text C8, privacy-notice link). Never echoes a request header. |
| `api_mlat.go`, `api_mlat_longterm.go` | MLAT routes (public quality and cells, owner Timing & MLAT, admin console, CRLB cache). |
| `mlat_history.go`, `mlat_history_store.go` | Once-a-minute MLAT console history (24 h network ring, 2 h per station) and its persistence: `<data>/mlat-history.gob.gz` is written every 10 minutes and at shutdown and read back at start (station rings by SID, attached at the first sample), so a restart keeps the charts. |
| `api_me.go` | `/api/me/*`: own stations, privacy settings, Feed Check, claims, export, account deletion. |
| `api_admin.go` | `/api/admin/*`, `/api/uplinks`. |
| `sse_net.go` | Live stream frames per viewer class; public stream admission. |
| `ratelimit.go` | Token bucket per client address for the public scope. |
| `health.go` | `GET /healthz`. |
| `net.go` | `Network` (services installed by cmd/airview), `LiveLink` / `LinkSource`, `BeastLinks`. |
| `api_share.go`, `sharestore.go` | Share links (WP-H2): owner create / list / revoke, admin list / revoke, the public `/api/share/{token}` views; store `<data>/sharelinks.json` (SHA-256 of the secret only). |
| `api_verify.go` | ADS-B verification alerts: counters, the aircraft suspect now (planner class `suspect`), the last 200 transitions; watcher every 10 s. |
| `api_improve.go` | CRLB coverage improvement: where one more station helps MLAT most (candidate sites, gain per cell). |
| `federation.go`, `modeac.go` | Federation status (admin) and public overlay; Mode A/C targets in the pictures and `/api/admin/mlat/modeac`. |
| `../privacy/` | Leak scanner, strip helpers, island guard; `publicdto` public types incl. tar1090 shapes. |

## Scopes (route lint: `TestRouteAccessLint`)

Every route declares `Access` and `Station`:

| Access | Aggregator mode |
|---|---|
| `AccessPublic` | Same answer for everyone; no station data (publicdto, reference data). |
| `AccessViewer` | Open; the handler projects per viewer (public: publicdto shapes; owner: tracker owner projection with other stations stripped; admin: full). |
| `AccessOwner` | Owner session (or admin). `{sid}` routes resolve only own stations (else 404). |
| `AccessAdmin` | Admin session with second factor. All legacy single-station routes (stats, antenna, coverage, timing, terrain, RF, weather, history, DB, ...) are admin-only: their data is the host station's. |
| `AccessSession` | The handler checks the session (account routes of `auth_routes.go`). |

Response cache keys include `model.Viewer.CacheKey()` (scope, owner, station) for every
non-public route in aggregator mode. Public scope is rate-limited per client address
(`aggregator.limits.publicRps` / `publicBurst`, default 1/s burst 10; `429` + `Retry-After`).
Behind a reverse proxy, list it in `aggregator.trustedProxies`, otherwise every client
shares the proxy's bucket. Live streams: one public stream per address, at most
`aggregator.limits.maxPublicSse`; owners and admins use the regular slots.

## How to wire it (cmd/airview / engine, WP-E)

```go
srv := server.New(eng, opts)
srv.SetAccountService(auth.NewService(reg.Accounts(), reg.Sessions(), ...)) // already wired
srv.SetNetwork(server.Network{
    Registry: reg,                                    // *registry.Registry
    Links:    []server.LinkSource{server.BeastLinks(beastSrv), avfSrv, mlatcSrv},
    Beast:    beastSrv,                               // *beastin.Server (admin view)
    Uplinks:  uplinkManager,                          // *uplink.Manager (standalone too)
})
```

`SetNetwork` may be called at any time (it replaces the services atomically and drops the
response cache). Missing services make their routes answer `503`; `/healthz` reports
`degraded` in aggregator mode without a registry.

Ingest listeners (avfsrv, mlatc) implement `LinkSource`:
`LiveLinks() []server.LiveLink` with proto, station index, UUID / feeder ID, remote IP
(memory only), connected since, msg/s, pos/s, kB/s, RTT, clock class, MLAT synced. The IP
is used for Feed Check (exact IPv4 or same IPv6 /64), compat claims and the admin link view
only.

The tracker must run in network mode (`tr.SetNetwork(reg.Resolver(), lanes)`) for owner and
admin projections (`mine`, `stations`, geometry of the selected station). Until then owners
get the merged picture with every receiver field stripped.

## Privacy tests

- `TestGoldenPublicCrawl`: every GET route anonymously, both live streams, `/data/*.json`, over
  a fixture (4 stations at known exact positions, addresses, UUIDs, feeder IDs, the demo
  receiver); fails on any coordinate (3 decimals, also scaled integers), any IPv4, the fixture
  IPv6 (/64), hidden SIDs, UUIDs, feeder IDs, ns timestamps and the keys of
  `privacy.ForbiddenPublicKeys`.
- `TestGoldenOwnerCrawl`: every owner-visible route as owner A; nothing of the other stations,
  no addresses; foreign `{sid}` answers 404.
- `publicdto.TestNoForbiddenFields`: no public type can even hold a forbidden member.

## Phase 4-5 web and API (WP-W45-web)

Nothing needs wiring outside this package: every route below works with what `engine.Engine`
already exposes.

- **Share links** (DESIGN §8.2 "My map", §14 WP-H2). `POST /api/me/shares` needs
  `acknowledge: true` (the location warning `shareLocationWarning`); scopes `map` and `map+stats`;
  1..365 days (default 30); at most 20 active links per account, 16 stations per link. The secret
  (24 random bytes, base64url) is returned once; `<data>/sharelinks.json` (0600, atomic writes) keeps
  its SHA-256. A link resolves only while it is active **and** every listed station is still owned by
  the link's account (unclaim, transfer, deletion end it; account deletion revokes all). Public views:
  `/api/share/{token}` (label, scope, `stationCount`, expiry, network name), `/aircraft` (the tracker's
  owner projection filtered to `Mine`, converted with `publicdto.FromSnapshot`, guard = public island
  guard OR — with `coarseLow`, default on — any aircraft below 1000 ft AGL / on the ground), `/stats`
  (`entries`: name only when the station publishes it, else "Station n"; `publicdto.StationStats`).
  Without network mode in the tracker the shared picture is empty, never the full map. Audit:
  `share.create`, `share.revoke`. Public rate limit applies.
- **Verification alerts**: `GET /api/admin/mlat/verification`. Suspect = `plan.PrioSuspect` of the
  live ADS-B aircraft (10 minutes after the last contradiction; hold-out aircraft show as hold-out).
  Admin only (a suspicion is a quality signal, not a public accusation).
- **Coverage improvement**: `GET /api/admin/mlat/crlb/improve?alt=&step=&sigmaNs=`. Rebuilds the CRLB
  station set from the registry (approved, located, timed; σ from the clock report when synced, else the
  class prior) and ranks candidate cells by the area-weighted log gain of σ_h within 400 km. Cached 10
  minutes per parameter set and station set; uses `heavySem`. 100 stations: 0.15 s
  (`BenchmarkImprove100`).
- **Admin station table**: `GET /api/admin/stations` takes `sort` (name, sid, kind, admission, online,
  lastSeen, trust, created, owner) and `dir`, applied before `offset`/`limit` (stable, ties by index).
- **Bundle adjustment**: `GET /api/admin/mlat/survey` `bundle.last.stations[]` now carries `sid` and
  `name` (admin).
- Tests: `wave4_routes_test.go` (share links incl. leak scan, persistence and limits, verification
  transitions, improvement analysis against geometry, sorted paging); the golden crawls cover the new
  routes (public: 404 / 401; owner: `/api/me/shares`).

## Network routes

<!-- BEGIN GENERATED ROUTES (go test ./internal/server -run TestServerWiringDoc) -->
| Method | Path | Access | Station | Mode | Summary |
|---|---|---|---|---|---|
| GET | `/healthz` | public | - | both | Health check for load balancers, service managers and monitors |
| GET | `/api/public/status` | public | - | both | Public application status: version, mode, network name, live counters and which public surfaces are enabled |
| GET | `/api/public/aircraft` | public | - | both | The merged live picture without receiver data (`publicdto.Snapshot`) |
| GET | `/api/public/aircraft/{icao}` | public | - | both | One aircraft of the public picture: live state, live trail without signal levels, reference data |
| GET | `/api/public/trails` | public | - | both | Live trails of the public picture (signal column null) |
| GET | `/api/public/events` | public | - | both | The public live stream (Server-Sent Events): `aircraft` (`publicdto.Snapshot`), `status` (`publicdto.Status`) and public `notice` events |
| POST | `/api/events/view` | public | - | both | Change the viewport of a live stream opened with `bbox` or `view=1` (no new keyframe) |
| GET | `/api/public/stations` | public | - | both | Stations that opted in to a public pin or public statistics (`publicdto.StationList`) |
| GET | `/api/public/summary` | public | - | both | The network landing page in numbers (`publicdto.Summary`): station counts, aircraft, MLAT aircraft, message rate |
| GET | `/api/public/stats` | public | - | both | The network in numbers: approved and online stations and the live counters |
| GET | `/api/public/mlat` | public | - | both | The network's empirical MLAT accuracy over the last hour (hold-out aircraft against their ADS-B reference) and its 1° cells (`publicdto.MLATCoverage`) |
| GET | `/api/public/join` | public | - | both | What a new feeder needs to join this network: host name, open listener ports (AVF/1, Beast, mlat-client), admission and registration policy, the backup retention named in the deletion notice and the privacy-notice link |
| GET | `/data/aircraft.json` | public | - | both | The public picture in the tar1090 / readsb `aircraft.json` shape |
| GET | `/data/receiver.json` | public | - | both | tar1090 `receiver.json` |
| GET | `/api/me` | owner | - | aggregator | The logged-in account and its stations (exact data) |
| GET | `/api/me/stations` | owner | - | aggregator | The own stations with exact location, privacy settings, admission and live state |
| GET | `/api/me/stations/{sid}` | owner | {sid} | aggregator | One own station: exact data, location estimates, the public pin preview (exactly as others see it), live links and statistics |
| POST | `/api/me/stations/{sid}` | owner | {sid} | aggregator | Change an own station's name, privacy (pin precision, show name, public statistics) or declared position |
| PUT | `/api/me/stations/{sid}/privacy` | owner | {sid} | aggregator | Set an own station's public pin precision, name display and public statistics |
| PUT | `/api/me/stations/{sid}/name` | owner | {sid} | aggregator | Rename an own station |
| PUT | `/api/me/stations/{sid}/location` | owner | {sid} | aggregator | Declare an own station's antenna position (cross-checked by the survey); `{"clear":true}` removes it |
| POST | `/api/me/stations/{sid}/location/confirm` | owner | {sid} | aggregator | Use a location estimate (survey, GNSS) as the station's position for MLAT (owner confirmation, DESIGN §10.1) |
| POST | `/api/me/stations/{sid}/unclaim` | owner | {sid} | aggregator | Release an own station (it keeps feeding; it can be claimed again) |
| GET | `/api/me/stations/{sid}/stats` | owner | {sid} | aggregator | Live statistics of an own station: frames, positions, aircraft this minute, late frames, live links, last seen |
| GET | `/api/me/stations/{sid}/mlat` | owner | {sid} | aggregator | Timing and MLAT of an own station: position source used for MLAT, clock state (class, anchor state, σθ, drift, short-frame offset, jitter, steps, events), sync peers, MLAT contributions and residuals, timed frames per minute and result eligibility |
| GET | `/api/me/stations/{sid}/mlat/longterm` | owner | {sid} | aggregator | History of an own station's MLAT participation over days, one point per hour: timed frames, groups, fixes and outliers per minute, synced peers, synced share, σθ, residual RMS and drift |
| GET | `/api/me/stations/{sid}/connections` | owner | {sid} | aggregator | Connection log of an own station (newest first; addresses only as daily-salted hashes) |
| GET | `/api/me/aircraft` | owner | ?station= | aggregator | The live picture for an owner: `mine` and `stations` (own SIDs) per aircraft, geometry for the selected own station |
| GET | `/data/me/aircraft.json` | owner | - | aggregator | tar1090 `aircraft.json` of the aircraft the own stations heard in the last 60 s (session-scoped) |
| GET | `/api/me/check` | public | - | aggregator | Feed Check: the live feeder links coming from the visitor's own address (exact IPv4, or the same IPv6 /64) |
| POST | `/api/me/claim` | owner | - | aggregator | Claim a station: a native feeder with its claim code, a compat feeder with its full UUID from the feeder's own address |
| GET | `/api/me/export` | owner | - | aggregator | Export the account's data (JSON files in a zip) |
| DELETE | `/api/me/account` | owner | - | aggregator | Delete the own account and its stations' data now; the session ends |
| GET | `/api/admin/stations` | admin | - | aggregator | The station registry with exact positions and owners: `{"stations":[...],"total":n,"counts":{...}}` |
| GET | `/api/admin/stations/{sid}` | admin | {sid} | aggregator | One station with exact data, estimates, live links (with addresses) and statistics |
| POST | `/api/admin/stations/{sid}` | admin (mutating) | {sid} | aggregator | Change a station: admission, owner, name, trust, anchor, never-anchor flag |
| PUT | `/api/admin/stations/{sid}` | admin (mutating) | {sid} | aggregator | Edit a station (same body as the POST) |
| POST | `/api/admin/stations/{sid}/admission` | admin (mutating) | {sid} | aggregator | Approve, hold, deny or revoke a station |
| POST | `/api/admin/stations/{sid}/reveal` | admin (mutating) | {sid} | aggregator | A station's exact position and current address (audited `station.reveal`) |
| DELETE | `/api/admin/stations/{sid}` | admin (mutating) | {sid} | aggregator | Delete a station and its data (queued; survives restarts) |
| POST | `/api/admin/feeders/{id}/revoke` | admin (mutating) | - | aggregator | Revoke a native feeder key (all its stations) |
| GET | `/api/admin/tokens` | admin | - | aggregator | Enrol tokens (without secrets) |
| POST | `/api/admin/tokens` | admin (mutating) | - | aggregator | Mint an enrol token; the secret is shown once |
| DELETE | `/api/admin/tokens/{id}` | admin (mutating) | - | aggregator | Revoke an enrol token |
| GET | `/api/admin/audit` | admin | - | aggregator | Audit log, newest first |
| POST | `/api/admin/retention` | admin (mutating) | - | aggregator | Run the retention jobs now; returns their report |
| GET | `/api/admin/links` | admin | - | aggregator | Every live feeder link of every listener, with addresses |
| GET | `/api/admin/ingest/beast` | admin | - | aggregator | The Beast listener (30004): totals and every link |
| POST | `/api/admin/ingest/beast/{link}/disconnect` | admin (mutating) | - | aggregator | Close one Beast link |
| GET | `/api/admin/network` | admin | - | aggregator | Operator view of the aggregator: listeners (ports, addresses, errors), ingest statistics, tracker lock statistics, lanes, flight_station rows |
| GET | `/api/admin/mlat` | admin | - | aggregator | MLAT engine counters: every stage (groups, clusters, solves, publishes, drops), clock filter, reference store, tracker, planner, k_atm |
| GET | `/api/admin/mlat/quality` | admin | - | aggregator | MLAT self-validation: hold-out error (fix and track), σ calibration, NEES, week-on-week alarm |
| GET | `/api/admin/mlat/history` | admin | - | aggregator | MLAT console history, one sample per minute for up to 24 h: hold-out error (fix and track N, P50, P95, cross-track P95, share within 1σ over the preceding 10 minutes), stage rates per minute (groups, clusters, solves, accepted, rejected, published, results, drops), clock filter (stations, synced, components, epoch cost), worker load |
| GET | `/api/admin/mlat/longterm` | admin | - | aggregator | MLAT network history over days, one point per hour from the persisted minute tables: frames, groups, fixes and outliers per minute, synced stations, and the hold-out error per hour (N, P50, P95 from the summed error histograms of every cell) |
| GET | `/api/admin/mlat/stations` | admin | - | aggregator | Per-station clock quality and MLAT residuals for every station the MLAT engine knows (class, anchor state, synced, σθ, drift, β short, jitter, steps, peers, contributions, used / excluded / outlier %, residual bias, σ scale, bias flag, timed frames per minute) |
| GET | `/api/admin/mlat/sync` | admin | - | aggregator | Clock-sync matrix: per station and peer (SIDs) co-observations in the last 30 s, RMS residual, relative frequency, same clock domain |
| GET | `/api/admin/mlat/crlb` | admin | - | aggregator | Expected horizontal MLAT σ (Cramér-Rao bound) at 3000 ft, FL100 and FL300 on a 0.25° grid from the located stations and their timing |
| GET | `/api/admin/stations/{sid}/mlat` | admin | {sid} | aggregator | Timing and MLAT of any station (as the owner view, peers by SID) |
| GET | `/api/admin/aircraft/{icao}` | admin | - | aggregator | One aircraft with the full receiver list (every station that heard it in the last 60 s) |
| GET | `/api/uplinks` | admin | - | both | Uplinks of this station to aggregators: feeder ID, UUID and per-uplink state (no secrets) |
| POST | `/api/uplinks` | admin (mutating) | - | both | Uplink actions: `{"action":"claim","id":"...","rxNo":0}` returns a claim code (shown once), `{"action":"retry","id":"..."}` restarts a stopped uplink |
| GET | `/api/aircraft` | viewer | ?station= | both | Live aircraft table (`model.LiveSnapshot`), rebuilt once per second |
| GET | `/api/events` | viewer | ?station= | both | Live stream (Server-Sent Events) |
<!-- END GENERATED ROUTES -->
