# rfstat - wiring guide for the integration engineer

`import "airview/internal/rfstat"` - advanced RF / protocol statistics computed online from the frame stream.
Imports only `model`-level stable packages (`geo`, `modes`, `store`). Registers its own tables
(`rfstat_hour`, `rfstat_iid_hour`, migration `"rfstat"` v1) from `init()`.

## 1. Lifecycle (engine)

```go
rs := rfstat.New(rfstat.Config{
    Antennas:   antennaIDs,            // cfg.Antennas[].ID in order; Frame.Antenna indexes this (max 8)
    StationLat: cfg.Receiver.Lat,      // for GNSS interference map cell centres (optional)
    StationLon: cfg.Receiver.Lon,
    // defaults: MaxAircraft 4096, AircraftTimeout 5m, PdWindow 10s, PdMaxGap 30s,
    // FadeWindow 10s, DecodeSNRdB 10, History 48 hours, Logger slog.Default()
})
go rs.Run(ctx, st)   // st = *store.Store (implements rfstat.Writer) or nil; returns ctx.Err() on cancel
```

- `Run` calls `Tick(time.Now())` every second and writes one row per closed clock hour (no per-frame DB writes).
  If you prefer to drive the clock from the tracker, calling `rs.Tick(now)` additionally is harmless (idempotent).
- On config change of antennas/station: create a new `Stats` (state is in memory only; hourly rows persist).
- Use **one `Stats` per physical receiver**. The Radarcape EVO 2CH is one stream, so one instance. If several
  receivers feed the same instance, channel occupancy / RSSI counts include duplicates (reception
  probability and timing are de-duplicated: same squitter type within 50 ms / same GPS ns is ignored).
- Memory: ~0.4 MB fixed + `MaxAircraft` x ~0.5 kB. Observe: ~80 ns, 0 allocations (benchmarked).

## 2. Per-frame hook (tracker)

Call `rs.Observe(f)` for **every** Mode S / Mode A/C frame after decoding, including frames that failed CRC
(they feed the garble/interference statistics). Do not pass MLAT frames (`RawFrame.MLAT`), status frames or
`DecodedUpdate`s. Build the frame with `f := rfstat.NewFrame(raw.Recv)` (sets every optional field to unknown),
then fill:

| Field | Source |
|---|---|
| `GPSNs` | `RawFrame.GPSNs` (-1 if none) |
| `Signal` | `RawFrame.Signal` (dBFS, NaN unknown) |
| `Feed` | small int per feed (index in cfg.Feeds, <= 15) |
| `Antenna` | `rs.AntennaIndex(feed.AntennaID)` - compute once per feed, not per frame; -1 = combined |
| `DF` | `m.DF` (24 for 24-31); `rfstat.DFModeAC` (32) for 2-byte Mode A/C frames |
| `CRC` | `modes.CRCOK`/`CRCInterrogator` -> `CRCOK`; `CRCFixed1/2` -> `CRCFixed`; `CRCBad` -> `CRCBad`; `CRCAddressParity` -> `CRCAddrAccepted` if the tracker accepted the address (known aircraft), else `CRCAddrRejected` |
| `ICAO` | `m.AddrKey()` (0 for bad/rejected frames) |
| `Kind` | see table below |
| `TC` | `m.TC` for DF17/18 |
| `BDS` | DF20/21: the BDS the tracker settled on (`modes.DisambiguateBDS`, ok=true) as `uint8(bds)` (0x40 = BDS 4,0); 0 = unknown |
| `IID` | DF11: `m.IID` (raw 0..79: 0-15 = II, 16-79 = SI 0-63) |
| `RA` | DF16 `m.ACASRA().Active()`, DF20/21 BDS 3,0 RA, or TC28 RA broadcast |
| `DistKm`, `BearingDeg`, `ElevDeg`, `AltFt`, `TrackDeg` | tracker's current geometry of this aircraft (also for non-position frames) |
| `Lat`, `Lon` | only on position frames: the decoded position (rfstat runs its own > 1200 kt jump test) |
| `Version`, `NIC`, `NACp`, `SIL`, `Category` | aircraft's last known values (TC31 version / NIC suppl.; NIC from `m.NICRc`) |
| `OnGround` | aircraft on ground |
| `GNSSDeltaFt` | TC19 `m.GNSSDelta` (GNSS minus baro) when present |
| `GNSSAlt` | true for TC 20-22 positions |
| `Implausible` | tracker rejected this frame's position as a jump |

Kind mapping: TC9-18/20-22 even/odd -> `KindPosEven`/`KindPosOdd`; TC5-8 -> `KindSurfEven`/`KindSurfOdd`;
TC19 -> `KindVelocity`; TC1-4 -> `KindIdent`; TC28 -> `KindAircraftStatus`; TC29 -> `KindTargetState`;
TC31 -> `KindOpStatus`; DF4 -> `KindSurvAlt`; DF5 -> `KindSurvIdent`; DF20 -> `KindCommBAlt`; DF21 -> `KindCommBIdent`;
DF11 -> `KindAllCall`; DF0 -> `KindACASShort`; DF16 -> `KindACASLong`; Mode A/C -> `KindModeAC`; else `KindOther`.

Rate: per frame (1,000-3,000/s). Locking: one mutex, held ~80 ns per frame.

## 3. HTTP endpoints (server)

All GET, JSON, no auth needed. `Report()` costs ~0.3 ms and ~300 kB of garbage: cache it for 1-2 s and share
it between the endpoints below.

| Path | Response |
|---|---|
| `GET /api/rf/report` | `rfstat.Report` (everything) |
| `GET /api/rf/health` | `rfstat.Health` (= `Report().Health`) - also suitable for `/api/status` badges |
| `GET /api/rf/signal` | `rfstat.SignalStats` |
| `GET /api/rf/detection` | `rfstat.DetectionStats` |
| `GET /api/rf/channel` | `rfstat.ChannelStats` |
| `GET /api/rf/interrogators` | `rfstat.InterrogationStats` |
| `GET /api/rf/timing` | `rfstat.TimingStats` |
| `GET /api/rf/census` | `rfstat.CensusStats` |
| `GET /api/rf/history?from=<ms>&to=<ms>` | `[]rfstat.HourSummary` oldest first: `rfstat.LoadHistory(ctx, st.DB(), from, to)` (default last 7 days); without a store use `rs.History()` (last 48 h in memory) |

JSON conventions: times Unix ms, ICAO 6-hex strings, every float is `rfstat.Num` -> unknown is `null`
(never NaN). Probabilities/fractions are 0..1; `*Pct` fields are percent.

## 4. UI (suggest a "RF" page, or sections of `stats` / `antenna` / `timing`)

1. **Health header**: big score (0-100, `-1` = no data) + status colour, the 7 `items` as cards (score,
   status, text), `advice` bullet list, `blockedSectors` drawn as red wedges on a small polar plot.
2. **Signal**: RSSI histogram (`signal.all.hist`) with vertical markers at `decodeFloorDbfs`,
   `noiseFloorEstDbfs` (dashed, labelled "estimate, assumes N dB decode SNR"), `p50Dbfs`, -3 dBFS
   (overload zone shaded). Small multiples / box plots for `perAntenna`, `perRing` (p05/p50/p95 vs km),
   `perElev`. KPI tiles: dynamic range, SNR margin, strong %, near-clip %.
3. **Fading & multipath**: K-factor and std-dev histograms, `kByElev` bars, `worst` table;
   multipath: line chart `devDb` vs `elevDeg`, headline "reflector height ~X m, lobe spacing Y deg"
   only when `detected`. **Aspect**: polar/half-circle chart of `meanDb` vs `fromDeg` (0 = nose-on, 180 = tail-on).
4. **Detection**: Pd vs distance (3 lines pos/vel/ident, x = (lo+hi)/2) with horizontal 50 %/90 % lines and the
   `range50Km`/`range90Km` markers; Pd vs elevation, vs RSSI; polar chart of Pd vs bearing.
   Table of `gaps` bands: p50/p95/p99 and `pUpdate` at 2/4/5/8/12 s. Per-aircraft table `aircraft`.
5. **Channel**: frames/s by DF (stacked bar), `recent` 5-minute sparkline of frames, bad and occupancy %,
   KPI tiles occupancy / peak occupancy / CRC bad / fixed / rejected / overlap model / implied channel rate;
   scatter `vsLoad` (`failFrac` vs `meanFps`, with `modelFailFrac` as line); `bursts` list.
6. **Interrogators**: table of `codes` (code, replies, rate, last 10 min, aircraft replying, replying %, lockout
   badge) with a 60-minute sparkline from `timeline`; note "II 0 includes DF11 acquisition squitters".
   KPIs: active radars, surveillance reply rate p50/p90/max (+`topSurv`), Comm-B mix donut (`commB`),
   ACAS DF0/DF16 rates, RA reports; ADS-B vs Mode S surveillance split bar.
7. **Timing** (only if `available`): interval histograms per type with the nominal window shaded
   (`nominalLo..nominalHi`), mean/std/chi2/uniformOk, even/odd alternation %, `nonCompliant` table.
   Show `note` (no TOA ranging with one receiver).
8. **Census**: ADS-B version donut, NIC/NACp/SIL bars, poor-integrity %, emitter categories, TC28/29/31 and
   GNSS-altitude shares, GNSS-baro delta histogram + `gnssDeltaByAlt` table; **GNSS interference index**
   24 h line (`gnssSeries`), map layer of `gnssCells` (polar cells 10 deg x 50 km around the station,
   centre lat/lon given, colour by `index`), `degradedList`.
9. **History**: hourly lines from `/api/rf/history` (fps, occupancy, CRC bad, Pd, 90 % range, health).

## 5. Definitions & verified facts

- Nominal rates (airborne): position 2/s, velocity 2/s, identification 0.2/s - verified at
  https://mode-s.org/1090mhz/content/ads-b/1-basics.html (ADS-B v2 transmission-rate table).
  Position/velocity interval 0.4-0.6 s (+-0.1 s jitter around 0.5 s): MIT Lincoln Lab (Bayliss 1994) and an
  ITU-R report state the bounds; the **uniform** distribution is the DO-260 design but was not verifiable from a
  free primary source - the `uniformOk` chi-square test therefore reports, it does not judge compliance.
  Identification 4.8-5.2 s is from memory of DO-260B, not verified online.
- Air time: 8 us preamble + 56/112 us data at 1 bit/us (mode-s.org introduction) -> 64/120 us; Mode A/C 20.3 us.
- Poor integrity = NACp < 8 or NIC < 7 or SIL < 3 (14 CFR 91.227(c)(1): NACp < 0.05 NM, NIC < 0.2 NM, SIL <= 1e-7;
  verified at https://www.law.cornell.edu/cfr/text/14/91.227).
- Reception probability: per aircraft 10 s windows on a fixed grid; expected = window x nominal rate for every
  type the aircraft has transmitted at least once, airborne DF17 only; the first window of a track is skipped
  (it starts on a received frame) and silent gaps > 30 s end the track (never-heard aircraft cannot be counted).
- Update probability `pUpdate(T)` = time-weighted share with position age <= T = sum(min(gap,T)) / sum(gap).
- Noise floor estimate = 1st RSSI percentile - `DecodeSNRdB` (assumption, labelled as such).
- Rician K: moment method on range-normalised power (`dBFS + 20 log10 km`) per 10 s window, needs >= 8 frames.
- Multipath: residual vs elevation (0.05 deg bins, -1..14 deg), detrended by a +-1 deg moving average,
  periodogram in sin(elev) for reflector heights 4-60 m; lambda = c / 1090 MHz = 0.275 m.
- GNSS degraded = NACp < 5 after having reported >= 8, or NIC < 5 after >= 7. Index = % of ADS-B aircraft with a
  recent position that are degraded, per minute (24 h ring) and per polar cell (window).
- Health thresholds: strong (>= -3 dBFS) > 1 % warn / > 5 % bad; SNR margin < 6 dB with no strong signals -> gain
  low; excess failures (observed - Poisson overlap) > 5 % warn / > 15 % bad; blocked sector = Pd < 50 % of the
  median sector; GNSS index >= 5 % warn / >= 20 % bad; update target P(age <= 5 s) >= 95 % within 150 km.
- Sliding window: `Report()` = current clock hour + the not yet elapsed share of the previous hour (~1 h).
  Hourly summaries are exact clock hours.

## 6. Structured health / advice messages (`code` + `data`)

Every `health.items[]` entry carries, next to the unchanged `text`, a stable `code` and a `data` object with
the numbers printed in `text` under unit-suffixed names, so the UI can re-format them with its unit settings.
`health.advice` (strings) is unchanged; `health.adviceItems[]` = `{code, text, data}` in the same order.
Unknown numbers are `null`. Suffixes: `Pct` percent (0..100), `Dbfs` signal level, `Db` dB step, `Km`, `Deg`,
`Hz` (per second), `S` seconds, `MHz`; bare names are counts.

| code | data fields |
|---|---|
| `gain.no-data` | - |
| `gain.ok` | `p50Dbfs`, `floorDbfs`, `strongPct`, `nearDb` (3: "within 3 dB of full scale") |
| `gain.too-high` | as `gain.ok` + `stepMinDb` 3, `stepMaxDb` 6 |
| `gain.high` | as `gain.ok` + `stepMinDb` 2, `stepMaxDb` 3 |
| `gain.low` | as `gain.ok` |
| `interference.no-data` | - |
| `interference.ok` / `.elevated` / `.likely` | `failPct`, `rejectedPct`, `overlapPct`, `rateHz` (frames/s), `occupancyPct`, `bursts` (only when > 0) |
| `detection.no-data` | - |
| `detection.measured` (status from the score: ok, warn or bad) | `posPct`, `velPct`, `identPct`, `pd90Pct` 90, `range90Km`, `pd50Pct` 50, `range50Km` |
| `sectors.no-data` / `sectors.clear` | - |
| `sectors.blocked` | `fromDeg1`, `toDeg1`, `fromDeg2`, `toDeg2`, ... for every printed sector (count the keys), plus `fromDeg`, `toDeg` = the first one (also `blockedSectors[]`) |
| `updates.no-data` | `distKm` 150 |
| `updates.ok` / `updates.low` | `distKm` 150, `ageS` 5, `freshPct`, `targetPct` |
| `gnss.no-data` | - |
| `gnss.ok` / `.elevated` / `.interference` | `degradedPct`, `jumpAircraft` |
| `timing.unavailable` | - |
| `timing.measured` (status from the score) | `alternationPct`, `nonCompliant` |
| `advice.gain-reduce` | `stepMinDb` 3, `stepMaxDb` 6, `strongPct`, `nearDb` 3 |
| `advice.gain-slightly-high` / `advice.gain-low` | `stepMinDb` 2, `stepMaxDb` 3 |
| `advice.interference` | `freqMHz` 1090 |
| `advice.sectors-blocked` | as `sectors.blocked`: `fromDegN` / `toDegN` per sector, `fromDeg` / `toDeg` = first |
| `advice.gnss` / `advice.healthy` | - |

`health_data_test.go` checks that every item / advice produced by the tests has a documented code and that each
data value is the number printed in the text.
