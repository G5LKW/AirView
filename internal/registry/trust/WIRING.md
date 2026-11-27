# internal/registry/trust: wiring notes (H-mlatc, timing quarantine)

Keeps the copies of stations whose timing is not proven out of the MLAT engine and proves them on ADS-B
positions. Maths, thresholds and measurements: `doc.go`. Persistent state: station flags in
`internal/registry` (`timing.go`).

## 1. Construction (engine, aggregator mode, MLAT enabled)

```go
gate := trust.New(eng,                              // the MLAT engine (ingest.MLATSink + LateObserver)
    trust.RegistrySource{R: reg},                    // states from Station.Timing(), effective positions
    trust.ConfigFromModel(cfg.Aggregator.MLAT),      // aggregator.mlat.quarantine (zero = default)
    trust.RegistryPromoter(reg, log))                // ProveTiming (audited) on proof
ingOpts.MLAT = gate                                  // instead of eng; the gate forwards Observe/ObserveLate
mlatcOpts.Trust = gate                               // probe requests for quarantined mlat-clients
```

After an admin override call `gate.Refresh()` so it applies at once (otherwise within 10 s).

## 2. Registry side (`internal/registry`)

| Call | Use |
|---|---|
| `Station.Timing()` | `trusted` / `quarantined` / `excluded` |
| `Station.TimingOverride()` | admin override in force (`trusted`, `excluded`, `""`) |
| `Registry.SetTiming(ctx, actor, idx, mode)` | admin: `trusted`, `excluded`, `quarantine` (judge again), `auto` (clear overrides); audited `station.timing` |
| `Registry.ProveTiming(ctx, idx, detail)` | the gate's proof; audited `station.timing.proven` |
| `Registry.Quarantined()` | admin list |
| `Policy.NoTimingQuarantine` | new stations skip the quarantine (default false) |

New AVF and mlat-client stations get `FlagTimingQuarantine`; Beast stations (never MLAT) and stations that
existed before do not.

## 3. Views

- Owner (own station) and admin: `gate.Status(idx)` → `Report{state, since, samples, windows, good, bad, refs,
  lastSigmaNs, proven, reason}` while the station is judged. No peer identities.
- Admin: `gate.Stats()` (groups, filtered, dropped, copies removed, comparisons, windows, promoted,
  quarantined now) and the override buttons above.

## 4. Tests

```
go test ./internal/registry/trust -short                    # unit + benchmarks' code paths
go test ./internal/registry/trust -run Quarantine -v        # simnet: hostile stations, bootstrap (≈ 2 min)
go test ./internal/registry/trust -run '^$' -bench .        # hot path
```

## 5. Wired (integrator after the hardening wave)

internal/engine builds the gate in `newMLAT` (mlatwire.go) between the per-station MLAT tap and the engine, passes it to the
mlat-client listener (`Options.Trust`) and exposes it as `Engine.TrustGate()` (admin `NetworkStatus().trust`,
`GET /api/admin/mlat/quarantine`, `POST /api/admin/stations/{sid}/timing`, owner station detail `timing`).
Config: `aggregator.mlat.quarantine.{disabled, minRefs, windowSec, maxSigmaNs, goodWindows, maxBadFrac, minAgeSec}`;
`disabled` also sets `registry.Policy.NoTimingQuarantine`. `RegistrySource` falls back to the GNSS and then the declared
position when a station has no effective one (mlat-client handshakes set only the declared position; without the fallback
those stations could never be proven).
