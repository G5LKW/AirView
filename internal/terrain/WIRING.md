# internal/terrain — wiring guide

Terrain elevation (AWS Terrain Tiles, Terrarium PNG) and 1090 MHz radio
line-of-sight analysis: horizon panorama, theoretical coverage rings per
altitude, coverage floor map layer, single-path LOS with Fresnel/diffraction,
per-aircraft visibility, site advice. Imports only model, geo, radio.

## 1. Construction (engine start-up)

```go
prov := terrain.NewProvider(terrain.ProviderOptions{
    Dir:       filepath.Join(dataDir, "terrain"), // tiles in Dir/terrarium/{z}/{x}/{y}.png
    MaxDiskMB: 1024,       // default 1024; a 450 km z9 set is ~30-40 MB
    MemTiles:  64,         // decoded tiles in RAM (256 KiB each) -> 16 MiB, fine on a Pi 4
    Offline:   false,      // true = cache only (e.g. a config switch "no internet")
    Logger:    log,
})                          // UserAgent defaults to "AirView/<model.Version>"
svc := terrain.NewService(terrain.ServiceOptions{
    Dir:      filepath.Join(dataDir, "terrain"), // polar_<key>.bin.gz result cache (6 kept)
    Provider: prov,
    Logger:   log,
})
go svc.Run(ctx)   // returns when ctx is cancelled; cancels a running job
```

Demo / sim mode without network: `ServiceOptions{Source: terrain.ConeSource{...}}`
or `terrain.FlatSource{}` instead of `Provider`.

## 2. Hooks

| When | Call | Notes |
|---|---|---|
| Start-up, after config load, **only if `Receiver.Located`** | `svc.Submit(terrain.RequestFromConfig(cfg.Receiver, cfg.Antennas, 0), false)` | Background job: tiles (0-50 %), terrain sampling (50-90 %), coverage (90-100 %). First run on a Pi: download time + ~10-30 s CPU; later runs load the polar terrain from disk and take ~1-3 s. |
| Config saved (`PUT /api/config`) | same `Submit(...)` | No-op if position, antenna heights / RF chain, k and parameters are unchanged (`Request.Key()`). A changed request cancels a running job and starts over. |
| Optional: weather k-factor changed by > 0.05 | `Submit(RequestFromConfig(rx, ants, k), false)` | Every k change recomputes the coverage (cheap when the polar cache exists: only panoramas, ~0.1 s per antenna). Use 0 (= 4/3) unless the UI offers "coverage follows refraction". |
| Tracker, once per aircraft per second (or per position update, throttled to 1 Hz) | `v := svc.Visible("", lat, lon, altFt)` | ~100 ns, zero allocation, nil-safe (`v.Known=false` until the first analysis). `antID ""` = best antenna. Store `v.Visible`, `v.MarginDeg`, `v.FloorFt` on the aircraft (e.g. `terrainVisible`, `terrainMarginDeg`). |
| Tracker/predict, when a predicted path exists (≤ 1 Hz per aircraft) | `f := svc.Forecast("", []terrain.PathPoint{{TMs, Lat, Lon, AltFt}, ...})` | First point = now. `f.LostAtMs` / `f.AcquiredAtMs` (0 = not within the path). Use to label "expected to be lost behind terrain in 45 s" and to suppress "signal lost" alarms when loss was predicted. |
| Analytics: comparing measured coverage | `a := svc.Analysis()`; `a.FloorGrid("", 0)` | Cells of 0.05° coincide with the store heat-map cells (`HeatAligned=true`), so `heat.min_alt` can be compared with the theoretical floor per cell (cells with measured < theoretical = anomalous propagation / ducting). |

Altitudes are **feet AMSL** (barometric altitude is close enough; prefer
geometric altitude when available). Distances km, angles degrees, elevations m.

`Service.Analysis()` returns an immutable `*Analysis` (atomic pointer); keep
the pointer for a batch of calls. `Analysis.Antenna(id)` / `.Panorama(id)`
give direct access (`Panorama.Visible`, `.MarginDeg`, `.Forecast`, `.FloorM`).

Lower-level API (no Service needed): `terrain.LOS(src, LOSInput)`,
`terrain.BuildProfile(src, lat1, lon1, lat2, lon2, n, k)`,
`terrain.SamplePolar` + `terrain.NewPanorama`, `KnifeEdgeLossDB(v)`,
`FresnelParameter`, `FresnelRadiusM`, `Provider.ElevationM(lat, lon)`
(cache only, never blocks on network), `Provider.Prefetch(ctx, lat, lon, radiusKm, z, progress)`,
`ChooseZoom(radiusKm, lat, maxTiles, minZ, maxZ)`.

## 3. HTTP endpoints

Register everything with `svc.Mount(mux, adminMiddleware)` (Go 1.22
method patterns) **before** `mux.Handle("/", web.Handler())`, or iterate
`svc.Routes()` (`Admin=true` routes must be wrapped with the admin-auth
middleware). Errors are `{"error":"..."}`; `503` = no analysis yet,
`404` = unknown antenna, `400` = bad parameters. JSON never contains NaN/Inf.

| Method, path | Params | Response |
|---|---|---|
| `GET /api/terrain/status` | – | `StatusResponse{job: JobStatus{state idle/running/done/error/cancelled, phase tiles/terrain/coverage, progress 0..1, phaseProgress, tiles: PrefetchProgress, message, error, key, startedMs, finishedMs, fromCache}, available, key, computedMs, dataComplete, provider: ProviderStats, attribution, attributionUrl}` — poll every 1 s while `job.state=="running"`, else every 30 s or on demand. |
| `GET /api/terrain/coverage` | `ant` (whose advice goes into `advice`; default first, 404 unknown) | the full `Analysis` (site, ground, k, radius, per antenna: `horizon`, `los[]`, `radio[]` (`AltRange{altFt, altM, smoothKm, rangeKm[] per bearing, capped, minKm, maxKm, meanKm, areaKm2}`), `advice`, attribution). ~0.3-1 MB at 0.5° step: fetch once per `key`. |
| `GET /api/terrain/coverage.geojson` | `ant` (default first antenna), `kind=los\|radio` | GeoJSON `FeatureCollection`, one `Polygon` per altitude, highest first; properties `kind, antennaId, altFt, altM, areaKm2, meanKm, maxKm, minKm, smoothKm, capped, color`. |
| `GET /api/terrain/horizon` | `ant` | `Horizon{antennaM, k, stepDeg, radiusKm, smoothDipDeg, smoothHorizonKm, mean/maxSkylineDeg, maxSkylineBrgDeg, bearingDeg[], elevationDeg[], distanceKm[], terrainM[], nearestObstacleKm[], nearestObstacleDeg[], noDataFraction}` |
| `GET /api/terrain/advice` | `ant` (default first antenna; 404 unknown) | `SiteAdvice{antennaId, altitudesFt, refAltFt, heights[{extraM, antennaAglM, areaKm2[], meanRangeKm[], areaGainPct[]}], km2PerMetre[], sectors[36]{fromDeg, toDeg, maxSkylineDeg, limitDistKm, limitLat/Lon, rangeKm, smoothKm, blockedPct, nearestObstacle...}, worstSectors[], summary[] (ready-to-show English sentences), summaryItems[]{code, text, data} (same order; codes terrain.height-gain {heightM, pct, altFt, areaKm2, km2PerM}, terrain.sector-blocked {fromDeg, toDeg, elevDeg, distKm, altFt, pct, obstacleKm?, obstacleBrgDeg?}, terrain.no-blockage {})}` |
| `GET /api/terrain/floor` | `ant` (empty = best of all), `cell` deg (default 0.05 = heat cells) | `FloorGrid{cellDeg, south, west, north, east, rows, cols, floorFt[] (row 0 = south, -1 = not visible / outside), heatAligned}` |
| `GET /api/terrain/floor-image` | `ant`, `w` px (default 1024, max 2048) | `{url, width, height, coordinates[4][lon,lat] (TL,TR,BR,BL), legend[{maxFt,color,label}], key}` → MapLibre `image` source |
| `GET /api/terrain/floor.png` | `ant`, `w`, `k` (cache buster) | PNG (Web-Mercator rows, alpha 150, transparent = > 60 000 ft / outside) |
| `GET /api/terrain/visible` | `lat, lon, altFt`, `ant` | `Visibility{known, visible, marginDeg, elevationDeg, skylineDeg, floorFt, distKm, bearingDeg, beyondRange, antennaId}` |
| `GET /api/terrain/los` | `lat, lon, altFt`, `ant`, `profile=0` to omit the profile, `k` effective earth factor (clamped 0.5..10, default the analysis' k) | `LOSResult{clear, rxAntennaM, groundKm, slantKm, bearingDeg, elevationDeg, obstruction{distKm, lat, lon, terrainM, aboveRayM, elevationDeg}\|null, minClearanceM, fresnelRatio, fresnel60Clear, diffractionDb, edges[], fsplDb, gasDb, totalLossDb, noDataSamples, profile[{distKm, lat, lon, terrainM, bulgeM, rayM, fresnelM}]}` — downloads the path tiles on demand (≤ 60 s). |
| `GET /api/terrain/elevation` | `lat, lon`, `z` (default 12) | `{lat, lon, elevM, ok, zoom}` (`ok=false` = no data, sea level) |
| `POST /api/terrain/recompute` (admin) | – | `202 {started, job}` — re-runs the last request ignoring caches (also resets the network back-off). `409` if nothing was submitted. |
| `POST /api/terrain/cancel` (admin) | – | `JobStatus` |

## 4. UI

* **Map layer "Theoretical coverage"** (toggle per antenna, `kind` LOS / radio):
  `coverage.geojson` rings as outlines coloured by `properties.color`, label
  `altFt`; legend from `FloorLegend` colours. Overlay the measured
  max-range polygon from the stats for comparison.
* **Map layer "Coverage floor"**: `floor-image` as a MapLibre image source
  (opacity slider), legend from `legend[]`; hover can query `/visible`.
* **Horizon panel** (antenna page): polar or unrolled chart of
  `elevationDeg[]` vs `bearingDeg[]` with `smoothDipDeg` as a reference
  line, tooltip `distanceKm`/`terrainM`; overlay the measured
  lowest-elevation-per-bearing from stats if available.
* **Site advice card**: `summary[]` sentences, table of `heights[]`
  (+0/+2/+5/+10 m → area per altitude and `areaGainPct`), the 5
  `worstSectors` with `blockedPct`, distance and position of the limiting
  ridge (clickable to fly to it), `km2PerMetre`.
* **Aircraft detail**: "Terrain: visible, +0.8° above the horizon" /
  "below terrain horizon (floor 4 200 ft)", forecast "lost behind terrain in
  ~45 s"; a "Path profile" button opens `/api/terrain/los?...` as an
  elevation chart (terrain + bulge filled, ray line, Fresnel zone band,
  obstruction marker, loss figures).
* **Status / settings**: progress bar from `/status` while running (phase,
  tiles done/total, bytes), "Recompute" and "Cancel" buttons (admin), cache
  size from `provider.diskBytes`, an "incomplete terrain data" warning when
  `dataComplete=false` (offline: missing tiles read as sea level).
* **Attribution (required)**: show `attribution` with a link to
  `attributionUrl` wherever terrain-derived layers are visible (map
  attribution control) and in the About page (full text `terrain.Attribution`).

## 5. Data source facts (verified 2026-11-12 with real requests)

* `https://s3.amazonaws.com/elevation-tiles-prod/terrarium/{z}/{x}/{y}.png`
  → HTTP 200 `image/png`, 256×256, ~15-140 kB per tile (e.g. z10/544/339 115 581 B);
  z16 → 404 (max zoom 15). No API key. Decoding `(R·256 + G + B/256) − 32768`
  checked against real tiles: Zugspitze 2933 m (true 2962), Bodensee 390 m
  (≈ 395), Mont Blanc 4779 m (4806), North Sea → 0 (bathymetry clamped).
* AWS registry `registry.opendata.aws/terrain-tiles`: buckets
  `arn:aws:s3:::elevation-tiles-prod` (+ `-eu`), licence =
  `https://github.com/tilezen/joerd/blob/master/docs/attribution.md`
  (required-attribution list reproduced in `terrain.Attribution`).
* Skadi HGT also available: `.../elevation-tiles-prod/skadi/N50/N50E010.hgt.gz`
  (200, 8.6 MB per 1°×1°) — not used: much larger than Terrarium at z9-z11.
* Copernicus DEM on AWS (`copernicus-dem-30m` / `copernicus-dem-90m` buckets,
  COG GeoTIFF, e.g. `Copernicus_DSM_COG_30_N50_00_E010_00_DEM.tif` 3.7 MB /
  GLO-30 29.8 MB) — free under the Copernicus DEM licence; not used because it
  needs a GeoTIFF/DEFLATE-predictor decoder outside the stdlib. A
  `Source` implementation can be added later without API changes.

## 6. Resource budget (Pi 4)

* Disk: tiles ≤ `MaxDiskMB` (LRU, never re-downloaded); polar cache
  ~1-4 MB per site (gzip float32, 6 files kept).
* RAM: decoded tiles `MemTiles × 256 KiB` (16 MiB default); polar terrain
  rays × samples × 4 B, held only while the job runs (default 720 × ~2300 at z9/50° N ≈ 7 MB; hard cap 3600 rays × 4000
  samples — keep `BearingStepDeg` ≥ 0.5 on a Pi); each
  antenna panorama ≤ 720 × 512 × 12 B ≈ 4.4 MB.
* CPU: Visible ~100 ns, Forecast O(points), NewPanorama ~40 ms (desktop),
  terrain sampling dominated by tile decoding (once per tile).

## Memory (Raspberry Pi)

* Decoded DEM tiles in RAM are bounded by `ProviderOptions.MemTiles` (default 64 = 16 MiB) and hard-capped by
  `ProviderOptions.MaxMemMB` (default `DefaultMaxMemMB` = 32 MiB = 128 tiles).
* After every analysis job the Service drops the decoded tiles (`ServiceOptions.KeepMemTiles`, default 0;
  `Provider.TrimMemory(keep)` does it by hand). The polar terrain and the advice panoramas are job locals.
  Measured (`TestAnalysisHeapRelease`, 120 km / zoom 10): live heap after a job 19.3 MiB keeping the tiles,
  3.3 MiB with the release.
* `ServiceOptions.OnJobDone func(JobStatus)` runs after each job, after that release: the job runner (engine)
  may call `debug.FreeOSMemory()` there. The library never forces a GC itself.
