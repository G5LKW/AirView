# internal/geoid: wiring guide

`import "airview/internal/geoid"` (stdlib only). MSL <-> ellipsoid heights with the EGM96 geoid.

```go
hae, ok := geoid.MSLToHAE(lat, lon, hMSL)   // ok=false: no grid in this build; hae == hMSL then
msl, ok := geoid.HAEToMSL(lat, lon, hHAE)
n, ok := geoid.Undulation(lat, lon)          // N, metres (geoid above ellipsoid); 11 ns, no allocation
geoid.Available()                            // grid present?
```

- Station altitudes (`model.AltRefMSL`) must be converted before they reach `mlat/*`; `AltRefHAE` is used
  as is. GNSS heights from DF17 TC20-22 are HAE; the GNSS−baro field (§9.6) fallback adds N.
- Operator grid (finer resolution or a different model): `g, err := geoid.Load(path); geoid.SetDefault(g)`.
  `Load` accepts AVGEOID1 (`.avg`), NGA ASCII (`WW15MGH.GRD`) and GeographicLib PGM (`egm96-15.pgm`).
  The config key is requested in docs/aggregator/HOOKS.md (`aggregator.mlat.geoidFile`).
- The embedded file is `data/egm96-1deg.avg`, produced by `go run ./internal/geoid/cmd/geoidgen`; see
  `data/README.md`. **It is not yet in the tree**: until it is generated, `Available()` is false and callers
  must treat heights as HAE and flag them (DESIGN §10 survey resolves ambiguous altitudes).
