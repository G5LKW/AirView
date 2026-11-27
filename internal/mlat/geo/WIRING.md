# internal/mlat/geo: wiring guide

`import "airview/internal/mlat/geo"` (stdlib only). WGS-84 geometry and the radio propagation model of the
MLAT engine (DESIGN §9.1). Everything is a value type or a pure function: no allocation, safe for
concurrent use. Units: ECEF metres (`geo.Vec3`), degrees, metres above the ellipsoid (HAE).

## Coordinates

| Need | Call |
|---|---|
| lat/lon/HAE -> ECEF | `geo.ToECEF(lat, lon, h)` or `geo.LLH{...}.ECEF()` (13 ns) |
| ECEF -> lat/lon/HAE | `geo.FromECEF(v)` (closed form, Vermeille 2002; 49 ns; round trip < 1 µm) |
| local ENU frame | `f := geo.NewFrame(origin)`; `f.ToENU(p)`, `f.FromENU(e)`, `f.Rotate/Unrotate(d)` |
| azimuth/elevation/range | `f.AzElRange(p)` |
| covariance ECEF <-> ENU | `f.CovToENU(c)`, `f.CovToECEF(c)` (3×3 row-major) |
| cheap height for models | `geo.GeocentricHeight(v)` (< 0.1 m from geodetic h below 15 km) |

Station heights must be HAE: convert MSL with `internal/geoid` (`geoid.MSLToHAE`).

## Propagation (solver, clock filter, survey)

```go
atm := geo.DefaultAtmosphere()               // N0 315, H 7 km, k_atm 1
atm = geo.Atmosphere{N0: n0FromMETAR, H: geo.DefaultScaleHeight, K: kAtm}.Normalize()
rho := atm.Range(rx, ac)                     // optical path, m: geometric range + tropospheric excess
rho, grad := atm.RangeGrad(rx, ac)           // + d rho / d ac (ECEF), for analytic Jacobians
ns := atm.PropagationNs(rx, ac)              // rho / c in ns
ex := atm.Excess(rx, ac)                     // rho - |ac - rx|
```

- Model: the exponential profile N(h) = K·N0·e^(−h/H) integrated along an optimised trial ray (parabola + cubic,
  two Newton steps on the optical length, 8-point Gauss-Legendre). Fermat's principle makes the optical length
  insensitive to small path errors. `raytrace_test.go` checks it against an RK4 ray trace through the same
  atmosphere: **≤ 2 mm at 400-420 km** for N0 280-456 and H 6-8 km (the acceptance limit is 1 m), and the
  gradient to 1e-7. About 380 ns per call, zero allocations.
- `Atmosphere` is a small value: keep one per network/component and replace it when k_atm or N0 changes
  (no locking; publish it in the clock snapshot or behind an `atomic.Pointer`).
- `StraightExcess(D, h1, h2)` is the closed form of RESEARCH §2.2 (no bending, no earth curvature). It is
  metres to tens of metres short for long, low paths; use it only for steep/short geometry.
- `PropagationNsDist(dist, h1, h2)` is the distance-only form for `internal/latency` (see HOOKS.md).
- k_atm (mlat/quality, hourly regression) must stay within `geo.KAtmMin..KAtmMax`; `Normalize` clamps.

## Line of sight (fix rejection "outside the radio horizon of any member", §9.4)

`geo.LineOfSight(rx, ac, marginM)` (4/3 earth), `geo.Clearance(rx, ac, k)` (minimum ray height in m; negative =
blocked), `geo.RadioHorizon(h1, h2, k)`. Terrain is not considered.
