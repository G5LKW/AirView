# internal/latency — wiring guide

`import "airview/internal/latency"` (imports model, modes, geo, ntp, store; stdlib otherwise).
Stage model, verified sources and estimator maths: see `doc.go`.

## 1. Tracker hook (owner: tracker team)

The T bit is already decoded: `modes.Message.CPRTime` (bit 21 of TC 5-8 / 9-18 / 20-22; left false for
TIS-B/ADS-R, where bit 21 is the IMF). `latency.TFlagFromME(m.ME())` is the same check for raw ME values.

Keep one `latency.Obs` per Tracker (no allocation), fill it in `HandleFrame` (`internal/tracker/ingest.go`)
and call `Observe` **after** the message is fully handled, so `Handled` covers AirView's processing and the
position decoded from *this* message is known:

```go
// fields: lat *latency.Analyzer (nil = disabled); obs latency.Obs
func (t *Tracker) observeLatency(f *model.RawFrame, m *modes.Message, ac *aircraft, posLat, posLon float64, gotPos bool) {
    if t.lat == nil { return }
    o := &t.obs
    o.Reset()                    // NaN floats, GPSNs -1
    o.SetFrame(f)                // RecvHost=f.Recv, GPSNs, FeedID, MLAT
    o.SetMessage(m)              // DF, ICAO=AddrKey, TC, IsPosition (DF17/18 with FCPR), TFlag=CPRTime, CPROdd
    if gotPos {                  // position decoded from THIS message by handleCPR (global or local)
        o.Lat, o.Lon = posLat, posLon
        if m.Has(modes.FAltBaro) { o.AltFt = float64(m.AltBaro) } else if m.Has(modes.FAltGNSS) { o.AltFt = float64(m.AltGNSS) }
    }
    if ac != nil && finite(ac.gs) && finite(ac.track) && fresh(ac.velMs, tMs, 10_000) {
        o.GS, o.Track = ac.gs, ac.track // latest TC19 velocity (<= 10 s old)
    }
    o.Handled = time.Now()       // or ntp.Now() - must be the same clock as f.Recv
    t.lat.Observe(o)
}
```

* Call it for every decoded DF17/18 frame (positions and others) and also for other DFs if you want
  receiver-delay statistics from all traffic (only `GPSNs`, `RecvHost`, `Handled`, `FeedID`, `DF` matter
  then). Not for duplicates suppressed by `dedupe` (they would double-count) and not for CRC failures.
* `handleCPR(ac, m, tMs, …)` (`position.go`) decodes the pair/local position: return (or stash in `ac`) the
  lat/lon it produced for this message, and pass `gotPos=false` when it did not produce one.
* MLAT frames (`f.MLAT`) may be passed — they are counted and excluded.
* `Observe` takes ~210 ns and 0 allocations (`BenchmarkObserve`); it takes its own mutex, so it may be
  called with the tracker lock held (it never calls back).
* **One reference instant for receiver delays: the START of the frame.** A Radarcape GPS stamp marks
  the end of the frame; this package subtracts the frame duration (64 µs short / 120 µs long / 20.3 µs
  Mode A/C, `FrameStampOffsetNs(kind, df)`), and the tracker's per-feed latency (`sampleLatency`,
  `GET /api/timing/latency` `feeds[]`) does the same since 2026-11. Both figures therefore agree for
  the same frames (tracker test `TestFeedLatencyMatchesChainReceiver`). The JSON says so explicitly:
  `tracker.FeedLatency.ref`, `latency.FeedClock.ref` and the `receiver` entry of `Summary.stages[]`
  (`StageStats.ref`) are `"startOfFrame"` (`latency.RefStartOfFrame`), or `"endOfFrame"` when
  `Options.NoFrameEndCorrection` is set (then only this package's figures change).

## 2. Engine setup (owner: engine team)

```go
e.lat = latency.New(latency.Options{})           // defaults: 15 min window, 24 h per-feed history, 4096 aircraft
e.lat.SetClock(e.ntp, latency.SamplerInfo(e.ntp)) // *ntp.Sampler is a model.Clock
applyRx := func(cfg model.Config) {
    if !cfg.Receiver.Located { e.lat.SetReceiver(math.NaN(), 0, 0); return }
    h := cfg.Receiver.GroundElevM + mastHeight(cfg)          // antenna feed point, m AMSL (e.g. max Antennas[].HeightAGLM)
    e.lat.SetReceiver(cfg.Receiver.Lat, cfg.Receiver.Lon, h)
}
applyRx(cfg)                                      // and again on every config change
tracker.Options{…, Latency: e.lat}               // hand it to the tracker
```

Persistence (optional; table `latency_minute` is registered by the package's `init` via
`store.RegisterMigration("latency", 1, …)`): once a minute in the engine scheduler

```go
rows := e.lat.DrainMinutes()
_ = latency.Persist(ctx, e.store, rows, cfg.Storage.MinuteRetentionDays)
```

At start-up `rows, _ := latency.LoadMinutes(ctx, e.store.DB(), time.Now().Add(-24*time.Hour).Unix(), nil, 0)`
then `e.lat.Seed(rows)` restores the per-feed charts. `Reset()` on factory reset. The receiver position
reported by Beast type-5 frames can be used too (its altitude is the GPS antenna height).

## 3. HTTP endpoints (owner: server team)

All JSON; every float is a `latency.Num` (`null` instead of NaN/Inf). Times: Unix ms; durations: ms.

**Note:** `GET /api/timing/latency` already exists (tracker per-feed `{now, clockOffsetMs, clockSynced,
feeds:[tracker.FeedLatency]}`). Recommended non-breaking change: keep those fields and add
`"chain": latency.Summary` and `"histograms": []latency.Histogram`.

| Method & path | Result |
|---|---|
| `GET /api/timing/latency` | existing fields + `chain` (`Summary`) + `histograms` (`Histograms()`) |
| `GET /api/timing/latency/aircraft` | `{ "aircraft": [AircraftRow…] }` (`Aircraft()`, most positions first) |
| `GET /api/timing/latency/aircraft/{icao}` | `{ "row": AircraftRow, "waterfall": Waterfall }` from `AircraftDetail(icao)`; 404 `{"error":"unknown aircraft"}`. `{icao}` via `modes.ParseICAO`. |
| `GET /api/timing/latency/feeds/{id}?since=ms` | `{ "feed": id, "series": [FeedMinute…] }` (`FeedSeries`; `FeedIDs()` lists ids) |
| `GET /api/timing/ntpcheck` | `NTPCheck` |

Shapes (JSON keys):

* `Summary`: `now, windowSec, receiverSet, clockSynced, clockOffsetMs, hostOffsetMs, hostOffsetCorrected,
  aircraft, aircraftT1, shareT1, msgShareT1, stages[], counts{}, feeds[] (FeedClock, without minima),
  findings[]`.
* **Host clock without NTP** (`clockSynced:false`): the host clock error is estimated from the
  receiver's GPS stamps (minimum of host receive − GPS arrival over 10 s intervals, last minute) and
  reported as `hostOffsetMs` (host − GPS UTC incl. the few-ms receiver floor; `null` when synced or
  not yet estimated). Beyond 100 ms (`hostOffsetCorrected:true`) the host-side stages (`receiver`,
  `airToSystem`, `endToEnd`) are corrected by it and are RELATIVE (floor ≈ 0), the T=1 `transponder`
  stage stays exact (GPS only), and finding `hostclock` (level `bad`) says "Host clock offset
  unknown/large (no NTP): T=1 latency not measurable …" with the estimate - no receiver-fault finding.
  An estimate of ≈ −1 s is ambiguous (host 1 s behind or legacy +1 s receiver stamps): T=1 is then
  kept out and the finding says why. Until the first estimate (20 GPS frames) host-side stages are
  not recorded. UI: show `hostOffsetMs` next to the clock badge and mark the receiver / end-to-end
  cards "relative" when `hostOffsetCorrected`.
* `StageStats`: `key` (`propagation | receiver | processing | transponder | ageDelta | airToSystem |
  endToEnd`), `label, description, count, minMs, p50Ms, p95Ms, p99Ms, maxMs, meanMs, uncertaintyMs, exact`,
  `ref` (only on `receiver`: `"startOfFrame"` / `"endOfFrame"`).
* `counts`: `frames, gpsFrames, noGps, mlatExcluded, positions, positionsRanged, positionsT1,
  positionsT0, negativeDelay, absurdDelay, oneSecondAhead, gpsTimeNotUtc, legacyRange, processingGated,
  rangeGated, t1OutOfSpec, t1UnexpectedTc, agePairs, ageTurnsSkipped, ageStaleRepeats, agePairsGated,
  frameEndCorrUs, receiverDelayMinMs, receiverDelayMaxMs`.
* `Histogram`: `key, label, unit:"ms", scale:"log"|"linear", count, bins:[{lo, hi, n}]` — log bins are
  signed (negative values have negative edges; the centre bin is −0.0001..+0.0001 ms). Extra keys
  `aircraftTransponder` (per-aircraft mean T=1 latency, 10 ms bins −200..200) and `aircraftAgeJitter`.
* `AircraftRow`: `icao, firstSeen, lastSeen, n, positions, t1, t0, tFlag, lastTc, xpdrN, xpdrMeanMs,
  xpdrStdMs, xpdrMinMs, xpdrMaxMs, agePairs, ageJitterMs, ageModelMs, ageCheckMs, positionAgeMs,
  ageExact, propMeanUs, propLastUs, slantKm, flags[]`. Flags: `t1-not-synchronised` (T=1 but values
  spread over the ±200 ms ambiguity window), `t1-out-of-tolerance` (|mean| > 150 ms), `t1-outliers`,
  `t1-unexpected-tc`, `age-over-0.6s-est`, `age-jitter-high` (σ > 100 ms; a 200 ms register gives 58 ms),
  `stale-positions` (same position repeated while moving).
* `Waterfall`: `icao, t, df, tc, tFlag, cprOdd, feed, exact, slantKm, elevDeg, events[], stages[],
  totalMs, totalUncMs, notes[]`; `events`: `{key: applicability|transmit|arrival|received|handled, label,
  utc (RFC 3339 ns), atMs (relative to applicability), uncMs}`; `stages`: `{key: positionAge|propagation|
  receiver|processing, label, ns, ms, uncMs, method: exact|estimated|assumed|measured|gated|computed|unavailable}`.
* `FeedMinute`: `t, n, minMs, p50Ms, p95Ms, maxMs, meanMs, floorMs, jitterMs, neg, absurd`.
* `NTPCheck`: `ntp{enabled, synced, server, stratum, refId, leap, offsetMs, delayMs, jitterMs, rootDelayMs,
  rootDispMs, lastOk, lastError}, quality: good|degraded|bad|unknown, offsetUncertaintyMs, issues[],
  feeds[FeedClock{feedId, samples, negative, absurd, lastMs, floorMs, jitterMs, driftUsPerMin, intervals,
  status: ok|warn|bad|unknown, issues[], minima[{t, ms}], ref}], explanation`.
* `Finding`: `{level: info|warn|bad, key, text}`.

## 4. Timing page UI (owner: web team, `web/static/js/pages/timing.js`)

1. **Stage waterfall** (top, for a selected aircraft — default: the one with the most positions, T=1
   preferred): a horizontal timeline from `events[0]` (time of applicability) to `handled`, one coloured
   segment per `stages[]` entry (position age · propagation · receiver→host · processing). Use a
   log-ish scale or broken axis — propagation (µs) and receiver (ms) differ by 1000×; print each
   segment's value with ± `uncMs`, hatch `estimated`/`assumed` segments, show `notes[]` beneath.
2. **Stage cards**: one per `chain.stages[]` — p50 large, min / p95 / p99 small, count, ± uncertainty,
   tooltip = `description`.
3. **Histograms** (ECharts bar, x = bin centre on a log axis for `scale:"log"`): receiver, propagation,
   transponder (T=1), ageDelta; plus the per-aircraft `aircraftTransponder` histogram with the ±150 ms
   tolerance shaded.
4. **Per-feed receiver delay chart**: line chart of `p50Ms` with a `minMs..p95Ms` band and `floorMs` as
   a separate line, 24 h, from `/api/timing/latency/feeds/{id}`; negative values in red.
5. **NTP/GPS consistency panel** from `/api/timing/ntpcheck`: server, stratum, refid, offset ± uncertainty,
   root dispersion; quality badge; the `minima` (10 s minima) sparkline with floor, jitter and drift
   numbers; `issues[]` list; `explanation` as help text.
6. **Aircraft table** (`/api/timing/latency/aircraft`): ICAO, msgs, T-bit, transponder latency (T=1) or
   estimated age (T=0, marked "est."), age jitter, propagation, flags as chips; row click loads the waterfall.
7. **Findings** list (plain language, from `chain.findings`).
8. **Explanation text**: ADS-B positions with T=1 apply to an exact 0.2 s UTC epoch (even/odd = the CPR
   format bit), so with the Radarcape's GPS arrival time the transponder's own delay is measured exactly;
   about 1 % of aircraft use it. For T=0 only the *variation* of the position age is observable from the
   aircraft's own reports; the shown mean is a model estimate. Limits for context: 14 CFR 91.227 —
   ≤ 2.0 s total, ≤ 0.6 s uncompensated; DO-260B uncompensated budget −200…+400 ms.

Poll the endpoints every 5–10 s while the page is visible; nothing here needs SSE.
