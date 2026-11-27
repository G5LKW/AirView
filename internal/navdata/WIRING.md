# internal/navdata — wiring notes for the integration engineer

Free aviation reference data: OurAirports airports / runways / navaids / countries, VRS
standing-data callsign routes (bulk), optional online route lookup (adsbdb.com), optional
aircraft photo metadata (planespotters.net), airport association and runway/approach helpers.

Imports only `model`, `geo`, `store`. Owns the tables `navdata_*` (registered with
`store.RegisterMigration("navdata", …)` in `init()`); everything is read through `store.DB()`,
written through `store.Write` (bulk imports go into `*_stage` tables in 10 000-row transactions
and are swapped in atomically, so a failed/cancelled import keeps the previous data).

## 1. Construction (engine)

```go
nd := navdata.New(st, navdata.Options{
    UserAgent: model.AppName + "/" + model.Version,   // default anyway
    Routes:    navdata.RouteOptions{Online: cfg.NavData.RoutesOnline},          // default false
    Photos:    navdata.PhotoOptions{Enabled: cfg.NavData.Photos, Contact: cfg.NavData.PhotoContact}, // default false
})
// on config change:
nd.SetOptions(newOpts)
// on shutdown: nd.CancelImport()
```

Suggested config fields (add to `model.Config` by whoever owns it; none exist yet):
`navData.routesOnline bool` (default **false** — sends callsigns to a third party),
`navData.photos bool` (default **false**), `navData.photoContact string` (contact URL or e-mail,
required by planespotters.net). Nothing is downloaded automatically: imports run **only on user request**.

## 2. Hooks

| When | Call | Notes |
|---|---|---|
| Engine start, receiver located, and after every successful import / receiver move | `region, err := nd.LoadRegion(ctx, rxLat, rxLon, 800, navdata.SizeSmall)` | Immutable in-memory index (`*Region` implements `navdata.RunwayIndex`). Swap it into an `atomic.Pointer[navdata.Region]` used by tracker/predictor. ~10–30 k airports for 800 km in Europe: a few MB. Empty region when nothing imported. |
| Trajectory predictor / tracker, per aircraft update when below ~10 000 ft (or at 1 Hz per aircraft) | `ends = region.RunwaysNear(lat, lon, 30, ends[:0])` then `fit, ok := navdata.MatchApproach(ends, lat, lon, altFt, trackDeg)` | Allocation-free with a reused `ends` slice. `ok` = established on final for `fit.End.Runway` at `fit.End.AirportCode`; use it to bend the prediction onto the extended centreline. NaN altitude/track is allowed. |
| Phase label (optional, aircraft details / stats) | `navdata.ClassifyPhase(altFt, heightAGLFt, vrFpm, gsKt, onGround, onApproach)` | `heightAGLFt` = alt − nearest airport elevation (`region.NearestAirport`), NaN if unknown. |
| Tracker, when a flight session ends (gap > 30 min) — not per frame | `dep, ok1 := navdata.GuessDeparture(points, region)`; `arr, ok2 := navdata.GuessArrival(points, region)` | `points []navdata.TrackPoint` in time order (first/last ~6 positions are enough; NaN for unknown alt/gs/track). Store `dep.Airport.Code()` / `arr.Airport.Code()` with the flight (e.g. in a flights detail JSON or a later column). Confidence 0..1 and a reason string are included. |

## 3. HTTP endpoints (server)

All JSON. Errors: map `navdata.ErrBadInput` → 400, `ErrNoStore` → 503, `ErrImportRunning` → 409,
`ErrDisabled` / `ErrContactMissing` → 409 (with the message), `ErrRateLimited` → 429, others → 502/500.
Mutating endpoints are **admin only**.

| Method & path | Params | Calls | Response |
|---|---|---|---|
| `GET /api/navdata/datasets` | – | `nd.Datasets(ctx)` | `[]navdata.DatasetInfo` (rows, last import time, source with **licence + attribution**) |
| `GET /api/navdata/sources` | – | `navdata.Sources()` | `[]navdata.Source` |
| `POST /api/navdata/import` (admin) | body `{"datasets":["airports","runways",…],"force":false}` (empty = all) | `nd.StartImport(engineCtx, navdata.ImportOptions{Datasets: …, Force: …})` | `202 {"started":true}`; 409 if running |
| `GET /api/navdata/import` | – | `nd.ImportStatus()` | `navdata.ImportStatus` (`running`, `progress{dataset,stage,bytesRead,bytesTotal,records,index,count}`, `fraction`, `result`, `error`). Poll every 1 s while running (or push the progress callback as SSE `event: navdata`). |
| `DELETE /api/navdata/import` (admin) | – | `nd.CancelImport()` | `{"cancelled":true}` |
| `GET /api/navdata/airports` | `bbox=S,W,N,E` (W>E crosses the antimeridian), `minSize=large\|medium\|small\|other\|all` (`navdata.ParseSize`), `limit`, `runways=1`, `format=geojson` | `nd.AirportsInBBox(ctx, navdata.AirportQuery{…})`; with `format=geojson`: `navdata.AirportOverlay(as, navdata.OverlayOptions{})` | `[]navdata.Airport` or GeoJSON `FeatureCollection` |
| `GET /api/navdata/airports/near` | `lat,lon,radiusKm(≤5000),minSize,limit,runways` | `nd.AirportsNear(...)` | `[]navdata.Airport` with `distKm`, nearest first |
| `GET /api/navdata/airports/nearest` | `lat,lon,minSize,maxKm` | `nd.NearestAirport(...)` | `navdata.Airport` or 404 |
| `GET /api/navdata/airport/{code}` | ICAO/IATA/ident/GPS code | `nd.AirportByCode(ctx, code, true)` + `navdata.RunwayEnds(&a, nil)` | `{"airport": Airport (with runways), "ends": []RunwayEndInfo}` or 404 |
| `GET /api/navdata/search` | `q`, `limit` | `nd.SearchAirports` | `[]navdata.Airport` |
| `GET /api/navdata/navaids` | `bbox`, `types=VOR,NDB…`, `limit` | `nd.NavaidsInBBox` | `[]navdata.Navaid` |
| `GET /api/navdata/route/{callsign}` | optional `lat,lon` (current position → plausibility) | `nd.Route(ctx, cs, &navdata.Point{…})` | `navdata.RouteInfo` (`found`, `source` "standing-data"/"adsbdb", `airports[]` with resolved `Airport`, `path` [[lon,lat]…] great circle with unwrapped longitudes, `distanceKm`, `check{plausible,doubtful,crossTrackKm,alongPct,reason}`) |
| `DELETE /api/navdata/route-cache` (admin) | – | `nd.ClearRouteCache` | `{"ok":true}` |
| `GET /api/navdata/photo/{icao}` | optional `reg` | `nd.Photo(ctx, icao, reg)` | `navdata.Photo` (`found`, `thumbnail{src,width,height}`, `large`, `link`, `photographer`, `attribution`) |

The route and photo endpoints should be called **lazily** (when the user opens an aircraft's
details), never for every aircraft in the live picture. Both are cached in SQLite (routes 24 h /
12 h negative; photos 7 d / 2 d negative) and paced (≥ 2 s resp. 1 s between requests, daily
budget 1000 / 2000, honour 429 Retry-After). The standing-data route table is local and free to
query for every aircraft if desired (one indexed lookup).

## 4. UI

- **Settings → Reference data**: table from `/api/navdata/datasets` (dataset, rows, imported date,
  source link, licence). "Download / update" button (+ per-dataset checkboxes, "force") →
  `POST /api/navdata/import`, progress bar from `fraction` and `progress.stage/records`, cancel
  button. Approx. sizes from `Source.approxBytes` (airports ≈ 13 MB, runways ≈ 4 MB, navaids
  ≈ 1.5 MB, routes zip ≈ 7 MB). Show the attribution lines of the sources somewhere visible
  (About box or map attribution): "Airport data: OurAirports", "Routes: Virtual Radar Server standing data".
- Two **opt-in** switches with a privacy note: "Look up unknown routes online (sends the callsign to
  adsbdb.com)" and "Show aircraft photos from planespotters.net (sends the ICAO address; requires
  a contact URL or e-mail, which is sent in the User-Agent as planespotters.net requires)".
- **Map overlay "Airports"**: on `moveend` fetch `/api/navdata/airports?bbox=…&runways=1&format=geojson&minSize=`
  depending on zoom (≤6 large, ≤8 medium, else small). Render `kind=airport` points (label `code`),
  `kind=runway` polygons (fill), `kind=centerline` lines (dashed, 10 NM). Optional navaid layer
  from `/api/navdata/navaids`.
- **Aircraft details panel**: route line "EGLL → KJFK" from `/api/navdata/route/{callsign}?lat&lon`
  with a ⚠ "doubtful route" badge when `check.doubtful`; draw `path` as a thin great-circle line on
  the map while the aircraft is selected. Photo: show `thumbnail.src` **only together with**
  `attribution` (photographer credit) and make the image link to `link`. Never show a photo when
  `found` is false.
- Flight history: departure/arrival guesses (code + confidence) next to each flight.

## 5. Verified external facts (November 2026, real requests)

- `https://davidmegginson.github.io/ourairports-data/{airports,runways,navaids,countries}.csv`
  answer 200; header rows match the parser (`airports.csv` now includes `icao_code`; runways
  `le_/he_` columns incl. `le_heading_degT`, `le_displaced_threshold_ft`; navaids `frequency_khz`,
  `usageType`, `associated_airport`; countries `code,name,continent`). The repository licence
  (GitHub API) is The Unlicense = public-domain dedication. (`https://ourairports.com/data/`
  served an expired TLS certificate during the check; the GitHub mirror is the download source.)
- `https://github.com/vradarserver/standing-data/archive/refs/heads/main.zip` → 302 → 200
  `application/zip`; licence CC0 1.0 (GitHub API); routes live in
  `routes/schema-01/<X>/<AIRLINE>-all.csv` with header `Callsign,Code,Number,AirlineCode,AirportCodes`
  (UTF-8 BOM) and `AirportCodes` like `EGLL-KJFK`.
- `https://api.adsbdb.com/v0/callsign/{CS}` → 200 `{"response":{"flightroute":{callsign, airline{icao…},
  origin{icao_code,iata_code,…}, destination{…}}}}` or 404 `{"response":"unknown callsign"}`. The adsbdb
  README states the route data "may not be copied, published, or incorporated into other databases
  without the explicit permission of David J Taylor" — hence online answers are only kept in an
  expiring cache table, never merged into `navdata_route`, and the lookup is off by default.
- `https://api.planespotters.net/pub/photos/hex/{HEX}` → 403 with a plain User-Agent ("Server
  User-Agent strings must include a contact URL or email…"), 200 with
  `AirView/x (+https://example.com/contact)`; fields `photos[].thumbnail{src,size{width,height}}`,
  `thumbnail_large`, `link`, `photographer`.

Live download tests: `AIRVIEW_LIVE_TESTS=1 go test ./internal/navdata -run TestLive`.
