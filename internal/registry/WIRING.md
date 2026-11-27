# internal/registry — wiring notes

Owner: WP-R (phase 1, wave 1A). Design: `docs/aggregator/DESIGN.md` §3, §6.6, §7, §8, §19, §20.

The registry is aggregator-mode only. Standalone mode never opens it.

## Files

| File | Contents |
|---|---|
| `registry.go` | `Open` / `OpenDir` (`<data>/stations.db`), `Options`, `Policy` (`PolicyFromConfig(model.Aggregator)`), `SetPolicy`, `Close`. One writer connection (BEGIN IMMEDIATE) + a read-only pool, WAL, `auto_vacuum=INCREMENTAL` on new files. |
| `schema.go` | Versioned migrations in `registry_meta`. Reserves idx 1 for the host station. |
| `keys.go` | Server secret (32 random bytes in `registry_keys`, created once) and HKDF purpose keys; `UnidentifiedKey(ip, port)`, `PeerTag(viewerSID, peerSID, t)`, the public-pin PRF. |
| `station.go` | `Station`, `Flags`, `Position`; in-memory mirror (lock-free `Resolver()`), `BySID`/`ByFeeder`/`ByUUID`/`ByIdentKey`/`List`/`Count`; `Update` and the `Set*` helpers; `Seen` + `FlushSeen` (batched last-seen). |
| `public.go` | Public pin points (§8.3): keyed point in the cell, stored in `station_public`, kept while the station stays in its cell, rural guard (`RuralGuardMin` = 3 per 1° cell, neighbours refreshed). |
| `admit.go` | `Admit` (identity + admission policy), `EnsureHost`, `SetAdmission`, `Feeder`, `RevokeFeeder`. |
| `tokens.go` | Enrol tokens: `MintToken`, `CheckToken`, `RevokeToken`, `Tokens` (stored as HMAC). |
| `claims.go` | `MintClaim`, `RedeemClaim`, `ClaimCompat`, `AssignOwner`, `Unclaim`; rate limits 5/10 min per client and 20/day per account. |
| `accounts.go` | `Accounts()` = `auth.AccountStore`, `Sessions()` = `auth.SessionStore` on `stations.db`. |
| `owner.go` | `AddEstimate`/`Estimates` (latest N per method), `OwnerStations`, `Owns`, `ExportOwner` (JSON zip), `DeleteOwner` (GDPR erasure). |
| `connlog.go` | `ConnOpen`/`ConnClose`/`ConnLog`, `IPHash` (HMAC with a random per-UTC-day salt; old salts deleted). |
| `audit.go` | `Actor`, `Audit`, `AuditLog`. |
| `retention.go` | `DeleteStation` (queued, survives restarts), `ProcessDeletions`, `RunRetention`, `DefaultRetentionRules`, `Run`. |
| `clone.go` | `CloneGuard`: the AVF "cloned_feeder" rule, in memory. |

## Tables in stations.db owned here

`registry_meta, registry_keys, feeders, stations, station_public, location_estimates, accounts, sessions, claims, enrol_tokens, conn_log, conn_salt, audit, deletions`. Every other table in the file (the namespaced statistics of internal/store) is handled generically:

- **Deletion:** rows with `ant = 's<idx>'` or `ant` in `['s<idx>/', 's<idx>0')`, rows with `sid = <sid>`, and rows with `feed` in `['st/<sid>/', 'st/<sid>0')` are deleted in batches of 5,000 (rowid tables) when a station is deleted.
- **Retention:** `DefaultRetentionRules`: `stats_minute`, `station_minute`, `station_clock_minute`, `latency_minute`, `receiver_status` (column `t`, Unix s) keep `MinuteDays`; `stats_hour`, `range_hour` (`t`) and `coverage`, `signal_hist`, `elev_hist` (`day`) keep `HourMonths`. A rule applies only when the table and column exist.

## How to wire it

Engine / cmd (WP-E):

```go
reg, err := registry.OpenDir(dataDir, registry.Options{Policy: registry.PolicyFromConfig(cfg.Aggregator), Logger: log})
defer reg.Close()
go reg.Run(ctx)                                   // last-seen flush, deletions, retention
svc := auth.NewService(reg.Accounts(), reg.Sessions(), auth.Options{Issuer: cfg.Aggregator.Name, Policy: ...})
reg.EnsureHost(ctx, "host", adminAccountID)       // station #1
// on config change:
reg.SetPolicy(registry.PolicyFromConfig(next.Aggregator))
```

`airview admin init` / `admin reset --data <dir>` can now open the registry from the data directory in a separate process and use `auth.NewService(reg.Accounts(), reg.Sessions(), …)`: SQLite WAL allows it while the server runs (the server re-reads accounts from the database on every login, nothing is cached).

Ingest (avfsrv / beastin / mlatc):

1. Native: after the HELLO_C signature check, `guard.Acquire(feederID, ip)` (`CloneRejected` → WELCOME denied `cloned_feeder`; `CloneReplace` → GOODBYE `replaced` to the old session), then `reg.Admit(ctx, AdmitRequest{Kind: avf, FeederPub, RxNo, UUID, EnrolToken, ClockCls})` per RECEIVER. `res.Status` / `res.Code` / `res.Reason` go into WELCOME unchanged. `Allowed()` false → close.
2. Compat: `Admit` with `UUID` (and `RelaySub` only for `TrustedRelays`) or `IdentKey: reg.UnidentifiedKey(ip, 0)` when the link has no UUID; `PrivacyOff` for mlat-client `privacy: false`.
3. Pending stations (`Status == "pending"`, `StationMeta.Admission == pending`) feed their own lane only — never the public map or MLAT.
4. `reg.ConnOpen(idx, ip, proto)` at session start and `reg.ConnClose(id, bytes)` at the end; `reg.Seen(idx, t)` freely (memory only, ~12 ns).
5. CLAIM record → `reg.MintClaim(idx)` → CLAIM_CODE `{code, expiresMs}`; `ErrNotClaimable` → NOTICE.
6. Per-IP / per-subnet limits and `PerIPStations` need raw IPs and stay in ingest.
7. Hot paths use `reg.Resolver()` (`model.StationResolver`: lock-free, allocation-free, ~10-30 ns).

Server (WP-H):

- `/claim`: `reg.RedeemClaim(ownerID, code, s.remoteIP(r))`; compat: `reg.ClaimCompat(ownerID, uuid, sameIP, s.remoteIP(r))` where `sameIP` = the visitor matches the station's live link IP (`auth.SameClient`). `ErrRateLimited` (`*RateLimitError.RetryAfter`) → 429. The error messages are user-safe.
- Owner pages: `reg.OwnerStations(ownerID)`, `reg.Owns(ownerID, sid)`, `SetPrivacy`, `SetName`, `SetDeclared`, `Unclaim`, `ExportOwner(ownerID, w)` (`Content-Type: application/zip`), `DeleteOwner(actor, ownerID, true)`.
- Admin: `List(Filter)`, `SetAdmission`, `AssignOwner`, `RevokeFeeder`, `DeleteStation`, `MintToken` (show the secret once), `Tokens`, `RevokeToken`, `AuditLog`, `RunRetention`. Call `reg.Audit(actor, "station.reveal", sid, "")` whenever an admin view shows exact data (§8.2).
- Public DTOs: only `reg.PublicPoint(idx)` (approved stations with a non-hidden pin) and `StationMeta`; never `Station`.
- Sync peers for owners: `reg.PeerTag(viewerSID, peerSID, now)`.

Store (WP-D): pass `Options.ExportExtra` to add the owner's statistics to the export.

Survey / self-location (WP-SU): `SetEstimated`, `AddEstimate`, `SetEffective(geo, located, status)`, `SetDeclared`, `SetGNSS`. The registry stores; the resolution policy (§10.1, `AutoUseSurvey` with σ_h ≤ `AutoUseMaxSigmaM`) is decided by the caller.

## Measured (i9-13900K, Windows, NVMe; `TestScale5000`)

5,000 stations: admit 0.16 ms/station, reconnect admission 35 µs, `Resolver.Info` 10-30 ns, flush 5,000 last-seen 8 ms, retention pass 13 ms, reopen 21 ms, offline purge + deletion of 1,000 stations 2.1 s.

## Integrator additions (after Wave 1A)

- `reg.Beast()` returns `*BeastRegistry`, the `beastin.Registry` adapter (beast.go): UUID -> `Admit{UUID}`, trusted-relay sub-station -> `RelaySub` = 0xE3 ID as 16 hex digits, UUID-less -> `IdentKey = Identity.Key()`. Admission follows `Admit` (under `invite`, UUID stations are held pending for the admin, UUID-less links are refused). Attach writes the conn log and `Seen`; Detach closes the conn-log row. `EpochBase` is always 0 and there is no link_guess.
- `reg.IngestKey()` is the persistent key for `beastin.Options.ServerKey`.
- cmd/airview opens the registry in aggregator mode (`openRegistry` in cmd/airview/accounts.go): persistent accounts/sessions, `Run`, `SetPolicy` on every config change, closed on shutdown. `airview admin init|reset` (cmd/airview/admin.go) use the same file. `EnsureHost` and `Resolver()` are not wired yet (WP-E).

## H-mlatc additions (timing quarantine, feeder positions, band references)

- **Timing trust** (`timing.go`): `Station.Timing()` (`trusted` | `quarantined` | `excluded`), `TimingOverride()`,
  `SetTiming(ctx, actor, idx, "trusted"|"excluded"|"quarantine"|"auto")` (admin, audited `station.timing`),
  `ProveTiming(ctx, idx, detail)` (the gate, audited `station.timing.proven`), `Quarantined()`, `TimingState(idx)`.
  New AVF / mlat-client stations get `FlagTimingQuarantine` unless `Policy.NoTimingQuarantine`. The gate itself is
  `internal/registry/trust` (WIRING.md there).
- **Feeder positions** (`position.go`, PRIV-1): `FeederPosition(ctx, idx, FeederPos{Lat, Lon, Alt, AltRef, GNSS,
  SigmaM, SetEffective})` → `(Station, decision, error)` with decision `first` | `applied` | `ignored` | `limited` |
  `held` | `owner`. The AVF RECEIVER path (`internal/engine` `applyReceiverLocation`) should call it with
  `SetEffective: true` (same semantics as today's update: GNSS beats declared, a survey result keeps the effective
  position); the mlat-client adapter already does (declared only, `AltRefAmbiguous`). `PendingPosition`,
  `ConfirmPendingPosition(ctx, actor, idx)` (owner/admin), `ReleasePending(ctx)` (hourly from `Run`).
  `SetEstimated` releases a held move the survey confirms. Tunables: `Policy.Positions` (`JitterM` 30 m,
  `MinInterval` 24 h, `HoldBeyondM` 1000 m, `HoldMax` 7 days, `SurveyMatchM` 250 m).
- **Band references**: `FrozenBandRef(ctx, sid, period, lat, lon)` and `TagPeriod(t)` replace the in-memory
  `bandRefTable` of `internal/server/bandref.go` (persisted in `band_refs`, pruned hourly to the current period).
- Schema migration 3 adds `station_feeder_pos` and `band_refs` (both keyed by `sid`: the deletion job removes
  their rows generically).
