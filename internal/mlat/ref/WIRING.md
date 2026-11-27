# internal/mlat/ref: wiring guide

`import "airview/internal/mlat/ref"`. ADS-B reference tracks, transmit-instant interpolation, and the
GNSS−baro field (DESIGN §9.1, §9.2.2, §9.4 altitude row, §9.6). It depends on `internal/modes`, `internal/weather` (types only),
`internal/mlat/geo` and `internal/geoid` only. The maths, the defaults and the measured numbers are in
`doc.go`. A `*ref.Store` is safe for concurrent use: tracks are sharded by address over 64 mutexes, and nothing
allocates on the hot paths.

## Owner: the engine (WP-M, `internal/mlat` root)

```go
refs := ref.New(ref.Options{}, wx)          // one per engine; wx implements ref.Weather (nil = geoid-only fallback)

// for every group the grouper hands over (ingest.MLATSink.Observe), and for
// ADS-B frames of single stations too, once per transmission (not per copy):
var m modes.Message                          // decode once, share with the clock/solver
if modes.Decode(g.Data, &m) == nil {
    refs.Observe(&m, txNs, txSigmaNs)        // DF17 / DF18 CF0/1 with clean CRC; DF0/4/16/20 altitude replies of tracked aircraft; others Ignored
}
refs.Expire(nowNs)                           // every ~10 s: drops tracks idle for 120 s, prunes the field
```

`txNs` is the transmit instant in network time (Unix ns; UTC when anchored). The best value is
`TransmitInstant` from the synced copies; the fallback is the grouper's `FirstNs` minus about 1 ms of propagation,
with `txSigmaNs` ≈ 5e6 (host-latency uncertainty). T=1 epoch tagging needs τ within ~±50 ms of UTC; T=0 tags are
only as good as τ.

## Clock filter (WP-K, §9.2.2): sync observations

```go
var st ref.State
if refs.At(g.Key, tauNs, tauSigmaNs, &st) && st.Usable() {
    // st.Pos (ECEF), st.Cov (ECEF 3x3 m²: smoother + NACp bias + latency + τ uncertainty) = Σ_pos of §9.2.2
    // st.PosAgeS, st.NACp, st.NIC, st.Flags for diagnostics
}
```

- Works for **any** message of a reference aircraft: DF17/18 and DF11/DF0/4/5/20/21 alike (`g.Key` is the
  tracker key = `modes.Message.AddrKey()`).
- τ: `refs.TransmitInstant(key, baseNs, arrivals, &atm, &st)` iterates t_i − ρ_i(p(τ))/c over the copies whose
  network time is known (synced stations; `Arrival.DtNs` relative to `baseNs` to keep ns precision) and also
  fills `st`. The clock filter eliminates τ by differencing, so for sync it only needs τ to ~1 ms; use
  `FirstNs`-based τ while nothing is synced.
- Blacklist (§9.2.4, aircraft biased across ≥ 3 station pairs): `refs.Suspend(key, nowNs+10*60e9)`.
- `refs.CheckHeard(key, tauNs, stationsECEF)` once per group (cheap) rejects references below the radio horizon
  of a receiver that heard them.

## Hold-out / quality (WP-M, §9.6) and position latency

- Hold-out truth: `refs.At(key, tau, 0, &st)`; compare the MLAT fix with `st.Pos`. For T=0 airframes the
  reported positions lag by the unknown latency (prior 0 ± 150 ms). **Feed the fix back:**
  `refs.ObserveFix(key, tauNs, fixECEF, fixCovECEF)` learns the airframe's latency (simnet: P95 66 m → 5 m).
  The TDOA solution of an ADS-B message from ≥ 4 GPS anchors is an equally good fix.
- `refs.SetLatency(key, L, σ)` restores a persisted estimate (`refs.Info(key).LatencyS`).
- ADS-B verification (`adsbSuspect`): compare the TDOA position with `st.Pos` and call `Suspend` on failure.

## Solver (WP-SV, §9.4): altitude row

```go
hae, sigma, src := refs.HAEFromBaro(key, lat, lon, baroFt, tNs)   // own GNSS−baro of an ADS-B aircraft, else the field
hae, sigma, src := refs.Field().BaroToHAE(lat, lon, baroFt, tNs)  // Mode S-only aircraft
```

`sigma` is the 1σ of the conversion (field cells with data: a few m; fallback 60 m with weather, 150 m without).
Add the altitude quantisation (25 ft/√12) and the age × vertical-rate inflation of §9.4 yourself.

## Mode S altitude replies (H-clock)

Pass the DF0/4/16/20 replies of every group to `refs.Observe` as well (same call, once per transmission, with
the group's τ): for an aircraft with a reference track they keep its pressure altitude current (pairing with
TC 19 and TC 20-22), add a height-only node to its vertical channel at most every `AltReplyMinS` (2 s), and
are gated against the track (address-parity frames). `Stats.AltReplies`, `AltReplyRejected`, `GNSSPairs`.
The engine feeds only DF17/18 so far (HOOKS H-clock -> internal/mlat).

## Weather (fallback of the field)

`ref.Weather` is `SurfaceAt(lat, lon, tNs) (qnhHPa, tempC, ok)`; the optional `ref.WeatherProfile`
(`MSLHeightOfPressure`) gives the analysed height of an isobaric surface. `ref.NewBundleWeather()` implements
both from `internal/weather` Open-Meteo bundles (pressure_msl as QNH, 2 m temperature, pressure-level
geopotential heights; nearest bundle within 250 km, hours interpolated; 0 allocations per query):

```go
wx := ref.NewBundleWeather()
refs := ref.New(opts, wx)                    // or refs.Field().SetWeather(wx)
poller.OnUpdate(wx.Set)                      // host station weather; add bundles for other station areas
wx.Set(bundle)                               // e.g. client.Forecast(ctx, lat, lon, opts) per 1° area with stations
```

Measured (TestFieldWithWeatherBundles, ISA+12 K column, FL350): geoid-only fallback error 502 m (σ 487),
QNH/temperature 61 m (σ 260), profile 0.1 m (σ 68). internal/weather has no METAR source (HOOKS H-clock ->
internal/weather).

## Tunables

`ref.Options` / `ref.FieldOptions` (all zero = documented defaults). Not yet in the config file: see HOOKS.md
(`aggregator.mlat.ref` request).

## Diagnostics

`refs.Stats()` (tracks, accepted/rejected/jumps, field cells), `refs.Info(key)` (per aircraft: NACp, NIC,
latency, flags as names) for the admin MLAT console.
