# internal/trajectory — wiring guide

Aircraft state estimation (IMM: constant velocity + wind-aware coordinated
turn, vertical and air-speed channels) and short-term prediction for the map.
Imports only `internal/model` (and `internal/geo` in tests). Everything is
in-memory, bounded, and safe for concurrent use. No DB writes.

## 1. Lifecycle (engine)

```go
tj := trajectory.NewManager(trajectory.Config{Now: ntpNow}) // zero fields = defaults
go tj.Run(ctx)                    // GCs stale aircraft every 10 s until ctx is done
tj.SetReceiver(lat, lon)          // whenever the station location is known/changes (ClearReceiver() when unknown)
tj.SetRangeFunc(func(brg, altFt float64) float64 { return coverageKm(brg, altFt) }) // optional (exit time)
tj.SetRunways(rp)                 // optional trajectory.RunwayProvider (approach detection)
tj.SetWind(wp)                    // optional trajectory.WindProvider (e.g. internal/met profile)
```

Providers must be cheap and concurrency-safe. `RunwaysNear(lat, lon, radiusKm, dst)`
appends to `dst` (a stack buffer of 16 is passed; do not retain it). `WindAt` is
queried at most once a minute per aircraft, only when frames carry no wind.

Defaults worth knowing: `MaxCoast` 60 s (hard stop for predicted positions),
`CoastAfter` 4 s (from then on `coast:true`), `StaleAfter` 180 s, `MaxTracks` 2000.

## 2. Feeding (tracker hook, on every decoded message)

Call `tj.Observe(icao, &obs)` in the tracker right after it applies a decoded
DF17/18 position, airborne velocity, altitude or Comm-B (BDS 4,0 / 5,0 / 6,0)
message, and for MLAT results. Build `obs` on the stack, set only what that
message carried, and set the matching `Has` bit (helpers `SetPos`, `SetVel`,
`SetAlt`, `SetVRate`). Allocation-free once the aircraft is known (~3.5 µs per
call on a desktop i9; budget ~10x that on a Pi 4 ≈ 4 % of one core at 1000 msg/s).

| Obs field | Has bit | From |
|---|---|---|
| `T` | – | receiver timestamp of the frame (GPS/MLAT time when available), else arrival time. **Must be monotone per feed;** late frames ≤ `OOSMaxLag` (2 s) are used, older ones dropped |
| `Lat, Lon, Src` | `HasPos` | decoded CPR position; `Src = trajectory.SourceFromString(model.Src*)` |
| `NACp` | `HasNACp` | latest NACp (sets position noise: EPU/2.45) |
| `AccuracyM` | `HasAccuracy` | explicit 1-sigma (m), e.g. from the MLAT solver; overrides NACp/Src |
| `GSKt, TrackDeg` | `HasVel` | airborne velocity (TC19) ground speed + true track, or surface movement |
| `NACv` | `HasNACv` | latest NACv |
| `VRateFpm` | `HasVRate` | TC19 / BDS 6,0 vertical rate |
| `AltFt`, `AltQ100` | `HasAlt` | barometric altitude; `AltQ100=true` for Gillham (100 ft) coding |
| `RollDeg` | `HasRoll` | BDS 5,0 roll (+ right wing down) |
| `TrackRateDps` | `HasTrackRate` | BDS 5,0 track angle rate |
| `TASKt` | `HasTAS` | BDS 5,0 TAS |
| `MagHeadingDeg` | `HasMagHeading` | BDS 6,0 magnetic heading |
| `SelAltFt` | `HasSelAlt` | TC29 / BDS 4,0 selected altitude (MCP/FCU or FMS) |
| `SelHeadingDeg` | `HasSelHeading` | TC29 selected heading |
| `NavModes` | `HasNavModes` | `trajectory.NavModesFromStrings(ac.NavModes)` |
| `OnGround` | `HasGround` | surface position / ground flag |
| `WindDirDeg, WindKt` | `HasWind` | derived wind (direction FROM, deg true) when the tracker has it |

`icao` is the 24-bit address as `uint32`; set bit 24 for non-ICAO/TIS-B track
addresses (`hex` output then reads `~ABCDEF`). Call `tj.Drop(icao)` when the
tracker forgets an aircraft (optional; GC does it after `StaleAfter`).

## 3. HTTP endpoints (server)

All responses are JSON with finite numbers only (no NaN/Inf). Times are Unix ms.
ICAO path params are 6 hex digits (optionally `~` prefixed); parse with
`strconv.ParseUint(hex, 16, 32)` (+ `1<<24` for `~`).

| Path | Params | Response | Notes |
|---|---|---|---|
| `GET /api/predict/live` | – | `{"now":ms,"aircraft":[]trajectory.Predicted}` | `tj.Predicted(now, buf[:0])`; reuse `buf` (1 Hz). Also push it in the SSE `aircraft` event as `"predicted"` if convenient |
| `GET /api/predict/{icao}` | `at` (ms, optional, default now) | `trajectory.Prediction` / 404 | `tj.Predict(icao, at)`; 404 when unknown or older than `MaxCoast` |
| `GET /api/predict/{icao}/path` | `horizon` s (default 120, max 900), `step` s (default 10) | `trajectory.Path` / 404 | `tj.PathFor(icao, now, h, s)` |
| `GET /api/predict/{icao}/intent` | – | `trajectory.Intent` / 404 | phase, holding, approach, CPA to receiver, exit time, level-off |
| `GET /api/predict/proximity` | `km` (search, 40), `horizon` s (120, ≤600), `hkm` (9.26), `vft` (1000), `max` (200) | `{"now":ms,"pairs":[]trajectory.ProximityPair}` | `tj.Proximity(now, opt)`; ~5 ms for 1000 aircraft on desktop → call at most every 5 s and cache |
| `GET /api/predict/stats` | – | `trajectory.Stats` | `tj.Stats(now)`; allocates, call at UI rate (≤ 1/5 s) |
| `GET /api/aircraft/{icao}` (existing) | `smooth=1` | trail replaced by `trajectory.SmoothTrail(trail, trajectory.SmoothOptions{})` | RTS smoother for noisy MLAT trails |

## 4. UI

* **Coasting icons:** for each `Predicted` with `coast:true`, draw the aircraft
  at the predicted `lat/lon/track` with a glow; opacity ∝ `conf` (0..1, decays
  with age, about e^(-age/30), and with `sigH`). `sigH` (m, 1-sigma incl. the
  manoeuvre floor, see §5) can be drawn as a faint circle. Entries vanish after `MaxCoast` (60 s) automatically. Non-coasting
  entries (`coast:false`) can be ignored (the live position is better) or used
  to animate smoothly between 1 Hz updates.
* **Predicted path:** on selecting an aircraft, fetch `/path?horizon=300&step=10`
  and draw `points` as a dashed polyline; draw `ell` (a, b in m, `dir` = major
  axis direction deg true) at 60/120/300 s, growing. `plan` explains the shape
  (`decay` = turn rolls out, `capture` = turning to selected heading, `hold` =
  racetrack, `ground` = taxiing stops).
* **Aircraft details panel (intent):** phase badge; "Holding (right, 60 s legs,
  2.0 laps)"; "Approach RWY 27 — 17.0 km, ETA 4:24, on glide path +0 ft";
  "Closest to station in 3:50 at 10.0 km"; "Leaves coverage in 12:07";
  "Levels off at FL180 in 0:45 (selected)"; "Top of descent near" when
  `descentPending`.
* **Proximity:** list/markers of `pairs` sorted by `severity` (≤1 = both
  thresholds violated). Purely informational, never an ATC/TCAS function —
  label it so. Highlight pairs where either aircraft reports nav mode `tcas`.
* **Prediction quality (stats page):** table per horizon (5/15/30/60 s) from
  `horizons` where `phase=="all"`: "60 s prediction median error 0.03 km
  (p90 …)"; columns imm / cv / dr. Show `skill` (`skillPct` = % better than
  naive dead reckoning) per phase (all, straight, turn, vertical), and
  `calib95` (ideal 0.95: fraction of real positions inside the predicted 95 %
  ellipse). Show `counters` (rejected outliers, out-of-order, duplicates, resets).

## 5. Notes / verified facts

* NACp → 95 % EPU and NACv → 95 % velocity bound tables (DO-260B code tables),
  checked against https://mode-s.org/1090mhz/content/ads-b/7-uncertainty.html
  and 14 CFR 91.227(c)(1) (NACp < 0.05 NM = code 8, NACv < 10 m/s = code 1),
  https://www.law.cornell.edu/cfr/text/14/91.227 .
* g = 9.80665 m/s² (CGPM 1901 conventional standard gravity, NIST SP 330 App. 1).
* Selected heading is treated as magnetic and converted to a selected track
  with the learned (track − magnetic heading) offset (declination + drift).
* Scoring uses only positions with 1-sigma ≤ `ScoreMaxSigmaM` (60 m), so MLAT
  noise does not pollute the statistics.
* **Reported uncertainty includes a manoeuvre floor** (`sigH` / `ell` of
  `Predict`, `Predicted`, `PathFor`; the tracker's `predSigmaM`). The filter
  covariance alone assumes the aircraft keeps doing what it did and is far too
  confident for an aircraft that turns while out of coverage. On top of it
  (root-sum-square) comes the displacement of a turn hypothesis started at the
  last filter update: a turn at `CoastTurnDps` (3°/s, standard rate), limited
  to `CoastBankDeg` (25°) of bank at the current speed and to at least
  `CoastMinAccel` (1.2 m/s²) lateral acceleration - along track
  R·(θ − sin θ), across R·(1 − cos θ), θ = ω·t, R = v/ω; ≈ a·t²/2 early on.
  Half of it for intent-constrained plans (selected-heading capture, holding),
  none on the ground. When positions stop but velocity reports keep updating
  the filter, the turn is observed (the hypothesis restarts at each update),
  and instead 1.5 m/s of velocity error integrated over the time since the
  last accepted position is added (≈ 25 m per axis after 17 s). Typical 1-sigma: ~17 m 2 s after the last position,
  ~250 m after 10 s, ~0.9 km after 20 s, ~3.4 km after 40 s (100-450 kt).
  Measured (`TestCoastSigmaCoversTurns`, aircraft at 100-300 kt that start a
  3°/s turn the moment their data stops): 47 % / 42 % / 39 % of the true
  positions inside the 1-sigma circle at 10 / 20 / 40 s of coasting (fast
  aircraft cannot fly 3°/s at 25° bank and fall outside); straight flight
  100 %. Before: ~0 %. Self-verification scoring (`Stats`, `calib95`) and
  `SmoothTrail` use the raw filter covariance and are unaffected.
* UI wording: `sigH` is a 1-sigma radius of a deliberately pessimistic turn
  hypothesis, not a 95 % bound; label the circle "1σ (if it turned)" or
  similar and keep "Prediction is a display aid".
