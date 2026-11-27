# internal/config wiring

Owner: WP-CF-configfiles (plus the original config owner). Reference for users: `docs/CONFIG.md` (generated).

## Layers

Effective config = defaults < `config.json` (UI-managed, rewritten by AirView) < operator file (JSONC, read-only: `-config`, else `$AIRVIEW_CONFIG`, else `<data>/airview.jsonc`) < `AIRVIEW_*` environment < `-set path=value`.

```go
cfg, layers, warnings, err := config.LoadLayered(config.LoadOptions{
    Dir: dataDir, File: flagConfig, Environ: os.Environ(), Sets: flagSets,
})
// err != nil: config.json unreadable/unwritable, or the operator file is missing
// (explicitly named) / not valid JSONC. cfg is still usable. A headless server
// started with an explicit -config should treat err as fatal.

// PUT /api/config:
next, notes := config.ApplyUpdate(incoming, holder.Get())
eff, err := layers.Commit(dataDir, next) // saves config.json WITHOUT locked values, returns next + overrides
holder.Set(eff)

// Background mutation (mutateConfig): same, layers.Commit(dir, next).
// Factory reset: eff, err := layers.FactoryReset(dataDir)
// UI lock state: layers.Locked("http.port"), layers.Overrides() -> [{Path, Origin}]
```

A nil `*config.Layers` is valid everywhere (no overrides), so the engine can keep its current behaviour until it adopts `LoadLayered`.

`config.Load` / `LoadWithWarnings` / `Save` are unchanged. `config.json` now tolerates comments and trailing commas (with a warning; they are dropped on the next save) instead of being moved aside as corrupt.

## Checking and printing

`config.Check(opts)` never writes anything and returns a `Report` with `Issues` (`Severity`, `Source` file/env/-set, `Pos` line:col, `Path`, `Msg`). The CLI front end is `internal/cli.RunConfig` (`airview config check|print|example|env`).

## Generated files

`go generate ./internal/config` (runs `./gen`) regenerates, from the doc comments in `internal/model`:

- `fielddocs_gen.go` (doc map),
- `airview.example.jsonc` (every setting, its default and its variable; parses to exactly the defaults),
- `docs/CONFIG.md`.

The package tests fail while any of them is stale, and when a setting has no documentation (add a doc comment in `internal/model` or an entry in `docOverrides` in `render.go`).

## Network settings (DESIGN §1)

`Validate` accepts `mode: "aggregator"` (not `"feeder"`), runs `Aggregator.Normalize` and `NormalizeUplinks`, checks `uplinks[].inputs` against feed IDs, `feeds[].clockClass` against `model.ParseClockClass`. `Clone` deep-copies uplinks and the aggregator lists. `Redact`/`MergeSecrets` cover `uplinks[].enrolToken` (matched by uplink id), `aggregator.outputToken` and `aggregator.tlsKey`.

## Phase 4-5 blocks (WP-WIRE-45)

- `ssr` (top level, every mode; `json:"ssr,omitzero"`, so a default config.json does not carry it):
  `model.SSRConfig`, normalised in `Validate` (`SSRConfig.Normalize`: ranges, sites, octal conspicuity codes,
  `modeAcCodeFormat`). Variables are the generic `AIRVIEW_SSR_*` names of the layer machinery
  (`AIRVIEW_SSR_SITES_0_LAT`, ...); the ssr package's own `ApplyEnv` is not used (its `SITES` string form and
  `MAX_SIGMA_H_M` spelling differ). The engine maps it by field name (`ssrConfigFromModel`, tested).
- `aggregator.mlat.latencyCal` (`model.AggMLATLatencyCal`, `Aggregator.normalizeLatencyCal`).
- `TestCheckCoversEveryTunable` covers `ssr.*`; `TestSSRBlockLayers` and `TestLatencyCalBlock` cover the lists and
  the cross-checks.
