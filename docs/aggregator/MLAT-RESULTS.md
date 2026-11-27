# MLAT results: DESIGN §13.2 gates measured end to end

Measured 2026-11-14 on the development machine: i9-13900K (32 threads), Windows 11, Go 1.27.3. Every number in this document comes from a simulation. **There is no real-world measurement yet.** The live hold-out of a real network is the next step (§9.8).

## 0d. Re-run after Wave-B integration (2026-11-14)

Code: everything of §0c plus Wave-A (phase-4 quality wiring: projected k_atm, phase-4 ADS-B verification with station attribution, clock partitions, Mode A/C, bundle adjustment) and Wave-B (SSR wiring, latency-calibration mapping: off by default and so not active in the harness). Run: `MLAT_RESULTS=row1,row2,row3,mix20,harsh20,coast30,step,anchorfault` (347 s) and `MLAT_RESULTS=mix100` (204 s); mix300 not re-run. Every scenario passed.

| Scenario | H P50 / P95 / P99 (m) | V P95 (m) | 1σ / 2σ | Everywhere H P95 | > 1 km (all) | > 5σ + 50 m (all) | Hold-out track vs truth P50 / P95 | Coast share | Clock steps |
|---|---|---|---|---|---|---|---|---|---|
| row1 | 5.3 / 17.5 / 47 | 37.6 | 64.3 / 93.0 % | 198 | 0.051 % | 0.35 % | 6.5 / 18.9 | 0.7 % | 0 |
| row2 | 11.5 / 106 / 692 | 74.6 | 62.7 / 89.0 % | 303 | 0.43 % | 0.33 % | 6.5 / 15.3 | 0.6 % | 1 |
| row3 | 31.9 / 157 / 468 | 81.2 | 73.4 / 93.4 % | 426 | 0.034 % | 1.82 % | 24.7 / 56.3 | 0.2 % | 1 |
| mix20 | 7.6 / 30.2 / 66 | 8.3 | 78.5 / 96.8 % | 216 | 0.063 % | 0.00 % | 7.2 / 20.4 | 0.7 % | 0 |
| mix100 | 5.4 / 51.8 / 91 | 78.4 | 70.0 / 94.3 % | 85 | 0.002 % | 0.01 % | 3.9 / 9.7 | 0.2 % | 2 |
| harsh20 | **14.9 / 81.6 / 201** | 5.5 | 48.1 / 80.7 % | 839 | **3.52 %** | **2.54 %** | 17.0 / 76.4 | 1.0 % | 9 |
| coast30 | 10.5 / 54.5 / 324 | 7.0 | 70.6 / 92.9 % | 417 | 0.50 % | 0.40 % | 6.3 / 14.0 | 0.3 % | 0 |
| step | 7.5 / 59.7 / 360 | 136 | 81.5 / 96.2 % | 300 | 0.024 % | 0.36 % | 5.2 / 14.9 | 0.5 % | 1 |
| anchorfault | 7.7 / 44.4 / 108 | 6.7 | 65.9 / 94.0 % | 221 | 0.062 % | 0.80 % | 14.1 / 341 | 0.7 % | 1 |

**Gates (§13.2):** row 1 5.3 / 17.5 m, 1σ 64.3 %, vertical P95 37.6 m (pass); row 2 11.5 / 106 m (pass); row 3 P50 31.9 m (pass); step detected +1.0 s, re-synced +1.45 s (pass); anchor fault demoted +0.45 s, network-time shift 0.5 ns (pass). Every gated row and mix100 are identical to §0c. **harsh20 is worse** than §0c (H P95 66.3 → 81.6 m, > 1 km 2.99 → 3.52 %, > 5σ + 50 m 1.94 → 2.54 %). harsh20 has no gate and was not re-run after Wave-A, so the change cannot yet be pinned on one Wave-A change. The candidates are the phase-4 verification and blame path and the projected k_atm, which only act under its heavy outliers and multipath. It is filed as an open item. mix100 vertical P95 78.4 m is still above 50 m, unchanged.

**Live smoke** (the `airview` binary on 127.0.0.1:18747, AVF 18748, Beast 18749, mlat-client 18750 incl. UDP, federation 18751; `latencyCal.enabled`; `airview-netsim -seed 7 -mix gps`: 10 AVF incl. 1 unlocated, 3 Beast, 3 mlat-clients, 200 aircraft, 150 km; plus one `airview-feeder`). All 10 timed stations left the timing quarantine about 6 minutes after start. About 13 minutes after start: 64,255 published fixes, 5,487 fixes per minute, hold-out fixes 2,145 and tracks 2,223, public MLAT quality P50 / P95 43 / 306 m (the estimate says 29 / 346 m), 339,897 results sent (88 k to the AVF and 34 k to the mlat-client feeders by minute 6). `latencyCal` was reported in the MLAT stats as "insufficient" with 0 airframes, as expected without learning solves. There were no ERROR lines.
## 0c. Re-run after the hardening-wave integration (2026-11-14)

Code: everything of §0b plus the integrator's wiring (HOOKS.md, "Integrator (after the hardening wave)"): the `aggregator.mlat.clock` block mapped onto the clock filter, Mode S altitude replies (DF0/4/16/20) fed to the reference store, the Taylor update in `geo.Atmosphere.path`, and the timing quarantine in the engine (it sits outside the harness, so the scenarios below do not exercise it). Run: `MLAT_RESULTS=row1,row2,row3,mix20,harsh20,coast30,step,anchorfault` and `MLAT_RESULTS=mix100` (mix300 not re-run). Every scenario passed.

| Scenario | H P50 / P95 / P99 (m) | V P95 (m) | 1σ / 2σ | Everywhere H P95 | > 1 km (all) | > 5σ + 50 m (all) | Hold-out track vs truth P50 / P95 | Coast share | Clock steps |
|---|---|---|---|---|---|---|---|---|---|
| row1 | 5.3 / 17.5 / 47 | **37.6** | 64.3 / 93.0 % | 198 | 0.051 % | 0.35 % | 6.5 / 18.9 | 0.7 % | 0 |
| row2 | 11.5 / 106 / 692 | 74.6 | 62.7 / 89.0 % | 303 | 0.43 % | 0.33 % | 6.5 / 15.3 | 0.6 % | 1 |
| row3 | 31.9 / 157 / 468 | 81.2 | 73.4 / 93.4 % | 426 | 0.034 % | 1.82 % | 24.7 / 56.3 | 0.2 % | 1 |
| mix20 | 7.6 / 30.2 / 66 | 8.3 | 78.5 / 96.8 % | 216 | 0.063 % | 0.00 % | 7.2 / 20.4 | 0.7 % | 0 |
| mix100 | 5.4 / 51.8 / 91 | **78.4** | 70.0 / 94.3 % | 85 | 0.002 % | 0.01 % | 3.9 / 9.7 | 0.2 % | 2 |
| harsh20 | 13.2 / 66.3 / 167 | 5.3 | 46.4 / 78.7 % | 753 | 2.99 % | 1.94 % | 16.6 / 69.0 | 0.8 % | 9 |
| coast30 | 10.5 / 54.5 / 324 | 7.0 | 70.6 / 92.9 % | 417 | 0.50 % | 0.40 % | 6.3 / 14.0 | 0.3 % | 0 |
| step | 7.5 / 59.7 / 360 | 136.5 | 81.5 / 96.2 % | 300 | 0.024 % | 0.36 % | 5.2 / 14.9 | 0.5 % | 1 |
| anchorfault | 7.7 / 44.4 / 108 | 6.7 | 65.9 / 94.0 % | 221 | 0.062 % | 0.80 % | 14.1 / 341 | 0.7 % | 1 |

**Gates (§13.2):** row 1 5.3 / 17.5 m, 1σ 64.3 % (pass); row 2 11.5 / 106 m (pass); row 3 P50 31.9 m (pass); step detected +1.0 s, re-synced +1.5 s (pass); anchor fault demoted +0.5 s, network-time shift 0.5 ns (pass). Horizontal figures are within ±3 % of §0b in every row.

**Vertical (WP-GATES request):** row 1 P95 75 → **37.6 m (gate < 50 m: pass)**; mix100 275 → **78.4 m (still above 50 m)**: the level-flight tail (P95 140 m) comes from Mode S-only aircraft in altitude bands where no ADS-B aircraft reports a GNSS height, and the simulator has no weather. The improvement comes from the Mode S altitude replies (the reference store took 165 k replies in the live run) and H-clock's field changes.

**SDR-only seed sweep, 30 simulated minutes** (`MLAT_SEEDS="12 13 14 15 16 17 18 19" MLAT_MIN=30 MLAT_EVAL=26`, row-3 network, last 4 minutes evaluated), as built and with the two new switches:

| Seed | Default: steps, P50 / P95, 1σ | `ingestGate` on | `adaptiveFlush` on |
|---|---|---|---|
| 12 | 1, 26.4 / 200 m, 76.6 % | 1, 27.0 / 200 m, 76.1 % | 1, 25.9 / 207 m, 80.1 % |
| 13 | 1, 29.2 / 137 m, 80.3 % | 1, 29.2 / 134 m, 79.5 % | 1, 24.4 / 135 m, 79.4 % |
| 14 | 0, 23.0 / 273 m, 82.1 % | 0, 22.4 / 256 m, 83.7 % | 0, 23.4 / 243 m, 84.4 % |
| 15 | 0, 28.5 / 134 m, 71.9 % | 0, 27.5 / 131 m, 74.8 % | 0, 27.9 / 128 m, 72.2 % |
| 16 | 1, 27.4 / 95 m, 92.7 % | 1, 27.0 / 92 m, 92.8 % | 1, 34.8 / 109 m, 78.1 % |
| 17 | 1, 21.1 / 126 m, 87.4 % | 1, 21.1 / 128 m, 86.5 % | 1, 21.1 / 130 m, 87.8 % |
| 18 | 0, 28.9 / 123 m, 82.3 % | 0, 29.5 / 126 m, 82.0 % | 0, 29.9 / 115 m, 81.6 % |
| 19 | 0, 24.5 / 341 m, 85.0 % | 0, 24.0 / 338 m, 85.7 % | 0, 24.3 / 343 m, 85.9 % |

All seeds stay stable for 30 minutes (≤ 1 clock step each). Neither switch changes the result beyond seed noise, so both stay off by default; the harness does not measure the result latency the adaptive flush is meant to reduce.

**Live smoke run** (the `airview` binary on 127.0.0.1:18710, AVF 18711, Beast 18712, mlat-client 18713; `airview-netsim -seed 7`: 12 AVF + 3 Beast + 4 mlat-clients, 250 aircraft, 150 km, plus one `airview-feeder` on the host stream). Timing quarantine: 16 timed stations proven 3 minutes after their first comparison (`minAgeSec` default); the `airview-feeder`, whose declared position was deliberately 70 km off, stayed quarantined ("timing inconsistent with the network", residual σ 246 µs). About 4 minutes later the hold-out reported fixes n 192, P50 / P95 100 / 321 m, tracks n 169, P50 / P95 41 / 82 m; the cross-track estimates were fix 102 / 364 m and track 33 / 80 m. Results reached all 12 AVF feeders and all 4 mlat-clients.

**Open defect found live (filed in HOOKS.md):** when the stations feed the engine from its first second (a restart with proven stations, or `quarantine.disabled`), no hold-out sample is scored for at least 5-10 minutes (four runs, `holdoutFixes` and `holdoutTracks` stayed 0) although fixes are published and results delivered. A fresh start, where the quarantine holds all copies back for 3 minutes, scored 270 fix and 759 track samples within 4 minutes. Not root-caused. Neither the Mode S altitude replies (off: still 0) nor the sync warm-up (20 s vs 60 s: still 0) is the cause.

## 0b. Re-run after H-track (2026-11-14): run-away tracks, wrong branches, publication timing

H-track changed the engine root, the tracker, the solver and the planner (internal/mlat/doc.go "Verification of tracks", "Publication"; track doc.go §8; solve doc.go "Cost"). The suite was run twice: all 10 scenarios with the code before the last change (the publication time bases, see below), and the 8 small scenarios again with the final code. The final values (mix100 and mix300 from the first run):

| Scenario | H P50 | H P95 | H P99 | 1σ / 2σ | Everywhere P95 | > 1 km (all) | > 5σ + 50 m (all) | Hold-out track vs truth P50 / P95 | Coast share |
|---|---|---|---|---|---|---|---|---|---|
| row1 | 5.4 | 17.2 | 44.7 | 63.1 / 92.8 % | 191 | **0.043 %** (0.31) | **0.44 %** (1.54) | 6.7 / 18.5 | 0.7 % (24.6) |
| row2 | 11.4 | 107 | 667 | 64.6 / 89.7 % | 358 | 0.38 % (0.47) | 0.31 % (0.32) | 6.1 / 15.0 | 0.5 % |
| row3 | 31.1 | 152 | 477 | 74.6 / 93.3 % | 456 | **0.028 %** (2.25) | 2.79 % (6.33) | 22.0 / 53.8 | 0.2 % |
| mix20 | 7.6 | 30.1 | 66.1 | 78.5 / 96.9 % | 216 | **0.063 %** (0.76) | 0.003 % (0.70) | 7.2 / 20.4 | 0.7 % |
| mix100 * | 5.3 | 53.3 | 94.7 | 71.1 / 94.7 % | 90.2 | **0.001 %** (0.20) | 0.28 % (0.47) | 3.8 / 10.2 | 29.1 % |
| mix300 * | 3.7 | 13.5 | 34.4 | 57.1 / 93.6 % | 37.8 | 0.23 % (0.22) | 0.30 % (0.30) | 3.3 / 8.2 | 31.1 % |
| harsh20 | 13.2 | 67.2 | 190 | 46.6 / 79.6 % | 735 | 2.90 % (2.95) | 1.63 % (1.67) | 16.1 / 65.8 | 0.8 % |
| coast30 | 10.7 | 53.6 | 327 | 70.2 / 92.8 % | 364 | 0.37 % (0.68) | 0.45 % (0.76) | 6.4 / 14.4 | 0.3 % |
| step | 7.4 | 58.8 | 350 | 79.4 / 96.4 % | 275 | 0.022 % (0.03) | 0.33 % (0.33) | 4.9 / 15.3 | 0.5 % |
| anchorfault | 7.7 | 44.4 | 108 | 65.9 / 94.0 % | 221 | 0.062 % (0.43) | 0.80 % (1.15) | 14.1 / **341** | 0.7 % |

\* first run (before the publication time-base change). Values in brackets: the §0 run. H = published horizontal error inside the radius.

**Gates (§13.2):** row 1 5.4 / 17.2 m / 1σ 63.1 % (pass); row 2 11.4 / 107 m (pass); row 3 P50 31.1 m (pass); step: detected +1.0 s, re-synced +1.5 s (pass); anchor fault: demoted +0.5 s, network-time shift 0.5 ns (pass). `go test ./internal/mlat/...` passes, `TestHotPathAllocs` at 0 allocations. Row 3 also profited from the clock filter's own changes in the same wave (1 clock step instead of 410); its improvement is not attributed to H-track.

**What changed the numbers:**
- **False positions far from the truth** dropped by 5-80× in most rows (`> 1 km` row 1 0.31 → 0.043 %, mix20 0.76 → 0.063 %, mix100 0.20 → 0.001 %): wrong 3+alt and 4-station branches are no longer confirmed by default, nor do two minimal fixes of the same stations "verify" them, and right tracks are no longer replaced by wrong ones on one DOF-1 contradiction. harsh20 is unchanged (its tail is multipath and clock steps outside the radius).
- **Coast share** 18-31 % → < 1 %: the map now shows measurement updates; before, a coasting publication at the 1 Hz tick (server time) blocked the next second of updates (event time, ≈ 0.5 s behind), so most map positions were predictions.
- **anchorfault hold-out track P95 341 m** (P50 14 m): one hold-out aircraft far outside the radius dominates the 1,017 samples; not investigated further.

**Live run-away hold-out tracks** (the Wave 2B open item; reproduction `MLAT_HTRACK=1 MLAT_HTRACK_LIVE=1 MLAT_HTRACK_MIN=20 go test ./internal/mlat -run HoldoutTrackNetsim -v`: netsim seed 7, 12 AVF + 3 map-only Beast links, no GPS status, relative time throughout, deterministic):

| | Hold-out track vs truth P50 / P95 / P99 | Hold-out fix vs truth P95 | Live statistic: track / fix P95 (vs ADS-B) |
|---|---|---|---|
| before | 105 m / **165.8 km** / 166.8 km | 272 m | **123 km** / 337 m |
| after | 70 m / **390 m** / 460 m | 272 m | **430 m** / 321 m |

**Live run, real time, real sockets** (`MLAT_LIVE=1 MLAT_LIVE_MIN=16 go test ./internal/mlat -run LiveNetsim -v`: an in-process aggregator, `internal/engine` with its AVF/Beast listeners on 127.0.0.1:28701-28703 and a throw-away data directory, fed by `simfeed` exactly like `airview-netsim -seed 7 -avf-feeders 12 -beast-feeders 3 -aircraft 250 -radius 150`; 16 minutes):

| Code | Hold-out fixes (n, P50 / P95) | Hold-out tracks (n, P50 / P95 / P99) | Cross-track estimate fix / track P95 |
|---|---|---|---|
| before H-track (12 min) | 812, 78 / 277 m | **25**, 90 m / 372 m (one 123 km sample in another run) | |
| H-track without the time-base fix | 2,380, 86 / 498 m | **4**, 78 m / 123 km | |
| final | 882, 133 / 549 m | **2,254**, 64 / 549 / 636 m | 330 / 537 m |

**Acceptance: hold-out track P95 549 m against fix P95 549 m (ratio 1.0, gate ≤ 2), live.** The live track statistic only exists since the time-base fix: before, almost no hold-out track estimate was ever scored (4-25 samples in 12-16 minutes) because the coasting publications kept blocking the measurement updates. The Wave 2B "4.5 km" figure was the long-term hourly statistic of the same failure modes (wrong branches on far aircraft, a few samples dominating). Honest caveat: this is one 16-minute run of one simulated network; its fixes (P95 549 m) are worse than the deterministic reproduction's because the live grouping, network latency and relative-time offsets differ.

**Honest hold-out figure.** The hold-out reports and `PublicQuality` now carry `EstimateFromCross`: horizontal P50/P95 estimated from the cross-track errors alone, which the reference's airframe latency does not enter (unit test: MLAT P50/P95 31.5 / 79.8 m, against the lagging reference 43.4 / 94.8 m, estimate 31.5 / 76.3 m). Row 1: live hold-out vs ADS-B P50 39.4 m while the same tracks are 6.7 m from the truth; the estimate is the figure to publish. `LatencyFeedback` stays off (it would let the MLAT fixes calibrate their own reference).

**Other H-track numbers:** solver 6 stations without altitude 15.9 µs (was 28-30 µs, gate ≤ 20 µs), 6 + alt 13.2 µs, 6 SDR + alt 14.5 µs (solve/doc.go "Cost"); 3+alt ambiguity with strictly the same three stations resolved within 10 s 36.9 → 43.8 %, 60 s 87 %, with a 4th station on 20 % unchanged 96.9 % (gate > 95 %); 2-station maintenance of manoeuvring traffic: errors inside the 99.9 % region 93.8 / 92.4 % → 96.9 / 97.9 % (track doc.go §8); runs with the synchronous engine are bit-reproducible (`TestReproducible`, 12 simulated minutes).

The sections below §0 still show the earlier runs.

## 0. Re-run after the phase-2 review fixes (2026-11-14)

The whole suite (all 10 scenarios) was re-run after the fixes for review findings M1-M6, ROB-1 to ROB-6 and PRIV-1 to PRIV-3 (PROGRESS.md, "Fixer: phase-2 review findings"). The changes that touch these numbers:

- **Frame reference (M1, M2):** hw12 (Beast / Radarcape 12 MHz) now uses one fixed 64 µs point for short and long frames, and sbs20 an end-of-frame table (64 / 120 µs, A/C 24 µs). simnet's Beast, Radarcape 12 MHz and SBS devices stamp the same way. Before, simnet and the engine shared the same wrong assumption, so the suite could not see the error; the reviewer's overlay runs, which mismatched the two, measured P95 +20 to +170 m and P99 +230 to +330 m on 12-station Beast or SBS networks. With engine and simulator now on the interop convention, the numbers below are what to expect when real receivers follow it. `TestFrameRefInterop` (internal/ingest/mlatc) checks the convention for every tick base.
- **Refractivity above the geoid (M5)**, **height-dependent fallback σ of the GNSS−baro field (M3)**, **field σ not capped at 30 m (M4)**.
- **Clock filter:** server-time check for first/self-asserted anchors and k·1 s leaps (ROB-1), failure attribution and the 20 % blacklist cap (ROB-2), re-probation after a transient demotion (ROB-5).

The other sections of this document still show the earlier run. Values of the re-run, inside the radius (published positions vs truth, metres):

| Scenario | H P50 | H P95 | H P99 | 1σ / 2σ | Everywhere P95 | > 1 km (all) | > 5σ + 50 m (all) | Clock steps | Within 3σ |
|---|---|---|---|---|---|---|---|---|---|
| row1 | 5.5 (5.5) | 17.8 (17.9) | 41.4 (42.9) | 63.7 / 92.8 % | 238 (241) | 0.31 % (0.38) | 1.54 % (1.57) | 0 | 88.0 % |
| row2 | 11.6 (11.6) | 107 (107) | 809 (809) | 64.7 / 89.1 % | 356 (349) | 0.47 % (0.47) | 0.32 % (0.32) | 1 | 93.5 % |
| row3 | 67.4 (67.5) | 222 (244) | 468 (626) | 20.2 / 45.4 % | 648 (575) | 2.25 % (1.42) | 6.33 % (5.49) | 410 (336) | 57.2 % |
| mix20 | 7.7 (7.4) | 31.4 (29.8) | 77.6 (80.0) | 78.5 / 96.7 % | 240 (218) | 0.76 % (0.72) | 0.70 % (0.68) | 0 | 93.9 % |
| mix100 | 5.3 (5.7) | 51.5 (52.1) | 94.4 (94.7) | 71.6 / 94.9 % | 89.6 (90.0) | 0.20 % (0.20) | 0.47 % (0.47) | 2 | 92.8 % |
| mix300 | 3.6 (3.7) | 13.5 (13.5) | 35.8 (35.3) | 57.3 / 93.5 % | 36.7 (37.4) | 0.22 % (0.28) | 0.30 % (0.35) | 8 (7) | 85.3 % |
| harsh20 | 13.3 (13.6) | 68.4 (71.9) | 202 (196) | 46.5 / 79.5 % | 741 (843) | 2.95 % (3.68) | 1.67 % (2.95) | 9 | 93.2 % |
| coast30 | 10.8 (10.7) | 55.7 (55.1) | 330 (326) | 70.3 / 92.7 % | 406 (394) | 0.68 % (0.83) | 0.76 % (0.83) | 0 | 98.5 % |
| step | 7.6 (7.9) | 60.3 (58.5) | 354 (342) | 79.3 / 96.4 % | 275 (272) | 0.03 % (0.03) | 0.33 % (0.35) | 1 | 95.5 % |
| anchorfault | 8.2 (8.1) | 50.9 (51.6) | 302 (299) | 63.7 / 92.0 % | 244 (240) | 0.43 % (0.44) | 1.15 % (1.11) | 1 | 97.9 % |

Values in brackets are the earlier run. Clock |rel| P50 / P95 of the device classes M1/M2 touch: mix100 Beast 5.3 / 15.6 ns (5.6 / 18.2), Radarcape 12 MHz 8.5 / 18.0 (9.7 / 19.7), SBS 46.8 / 133 (40.7 / 135); mix300 Beast 10.9 / 24.6 (11.2 / 25.9), SBS 42.3 / 139 (43.1 / 150). Mix300 vertical P95 25.8 m, availability 302/302 aircraft, CPU 0.93 cores, epoch 69.8 ms, heap 414 MB. `step`: detected after 1.0 s, re-synced after 1.5 s. `anchorfault`: demoted after 0.5 s, network-time shift 0.5 ns.

**Gates (§13.2):** row 1 5.5 / 17.8 m / 1σ 63.7 % (pass); row 2 11.6 / 107 m and 26/26 Mode S-only aircraft (pass); row 3 P50 67.4 m (pass); SDR-only P95 222 m (within the 500 m target). The in-suite gate tests (`internal/mlat/gates2_test.go`, clock `gates_test.go`) pass.

**Honest reading:** most rows move within run-to-run noise. Row 3 (SDR only, relative time) improved inside the radius (P95 244 → 222 m, P99 626 → 468 m) but got worse everywhere (P95 575 → 648 m, false > 1 km 1.42 → 2.25 %) with more clock steps (336 → 410). This is the existing SDR-only clock weakness of §7 item 2, now with a different random path; it is not attributed to one fix. In one full-suite run under heavy parallel CPU load, `internal/engine TestMLATEndToEnd` (a wall-clock test) failed once with 291 clock steps (relative time, P50 4.6 km); it passed alone (P50 46 m, 0 steps) before and after. It is load-sensitive and is listed as open.

## How the numbers were produced

**Pipeline under test.** `simnet` receivers produce Beast-equivalent frames. The frames are delivered with network latency into the real `internal/ingest` grouper (virtual clock), then into the real `mlat.Engine` (clock filter, reference store, planner, assembler, solver, tracker, quality). The engine never sees the truth. Stations get their declared positions (σ 5 m) and declared clock classes, as the registry would provide them.

**Truth comparison.** The truth is applied outside the engine:
- **Positions:** each published position (tracker output, displayable grades A-C, hold-out aircraft excluded) is compared with the simulator's true position at the published transmit time. The comparison is made in a local ENU frame: the horizontal error is the east/north distance, the vertical error is the up distance.
- **Clocks:** each synced station's network-time conversion of its noise-free local time is compared with true time. The component's common mode is removed: the median of its anchors, or of its free clocks when it has no anchor. This is the same method as the clock package's own harness.

**Suite.** `internal/mlat/results_test.go`, test `TestMLATResults`. It is opt-in because the 300-station run simulates for 20 minutes:

```
MLAT_RESULTS=all MLAT_RESULTS_OUT=results.jsonl go test -run TestMLATResults -v -timeout 6h ./internal/mlat
```

You can also pass a comma-separated list of scenario names instead of `all`. The suite uses three hooks added to the shared test harness (`harness_test.go`): `obsHook` for fault injection, `pubHook` for every published fix with its truth, and `engWall` for CPU. The harness's `[64]bool` station bookkeeping undercounted networks larger than 64 stations; it is now sized to the network.

**Common settings.**
- 30 % of aircraft are Mode S-only (no ADS-B position).
- 10 % hold-out.
- Airframe position latency 0-300 ms.
- Network latency uniform 20-80 ms, except in `harsh20`.
- Each run simulates 16 minutes and is evaluated over the last 4 minutes, after the 10-minute GPS anchor probation of §9.2.5. `anchorfault` is the exception: 22 minutes, evaluated over the last 4.
- "Inside" means the true aircraft position is within the network radius.

**Default impairments in every scenario.** These come from simnet's `DefaultClockSpec` and `DefaultReception`.
- **RTL-SDR:** 30-100 ppm, 0.01 ppm/s thermal ramps, 300-500 ns jitter, 2.4 MS/s quantisation reported in 12 MHz ticks, ±200 ns short/long reference error, silent and declared sample drops.
- **GPS Radarcape:** 15-45 ns PPS jitter, 50-300 ns cable bias.
- **Multipath:** 1-2 % of frames (mean 100 ns, four times as often below 3° elevation).
- **Gross outliers:** 0.01 % of frames (±5 µs).

### Scenarios

| Name | Network |
|---|---|
| `row1` | §13.2 row 1: 8 GPS Radarcapes, 150 km, seed 11 |
| `row2` | §13.2 row 2: 4 GPS + 20 RTL-SDR, 150 km, seed 12 |
| `row3` | §13.2 row 3: 20 RTL-SDR only, 150 km, seed 13 |
| `mix20` | 3 GPS Radarcapes + 2 Radarcapes on their free 12 MHz clock + 15 RTL-SDR; 150 km; 250 aircraft |
| `mix100` | 100 stations, simnet `DefaultMix`: 62 % RTL-SDR, 10 % Airspy, 10 % Beast, 4 % Radarcape 12 MHz, 10 % GPS Radarcape, 4 % SBS. 300 km; 600 aircraft |
| `mix300` | 300 stations, `DefaultMix`; 450 km; 1,500 aircraft |
| `harsh20` | `mix20` layout with heavy impairments: multipath 8 % (mean 250 ns); outliers 0.2 % (±5 µs); SDR silent clock steps 2/h (±30 µs), silent drops 0.5/h and declared drops 1/h; network latency 20-300 ms |
| `coast30` | 30 stations, `DefaultMix`, all on the southern half of a 150 km disc (a coastline). Aircraft over the "sea" half are outside the receiver hull but inside the radius |
| `step` | `mix20` layout (seed 26) with a silent 22 µs step on one RTL-SDR at t = 800 s |
| `anchorfault` | 8 GPS + 2 Beast + 12 RTL-SDR; from t = 900 s, +1 µs on every timestamp of one GPS anchor while its status bits stay "locked" |

## 1. Position accuracy (published positions vs truth)

### Horizontal error

All values are in metres. The "inside radius" columns cover aircraft within the network radius; "everywhere" includes aircraft outside it.

| Scenario | n | P50 | P95 | P99 | 1σ | 2σ | Everywhere P50 | Everywhere P95 | Everywhere P99 |
|---|---|---|---|---|---|---|---|---|---|
| row1 (8 GPS) | 12,989 | **5.5** | **17.9** | 42.9 | 63.1 % | 92.7 % | 15.4 | 241 | 877 |
| row2 (4 GPS + 20 SDR) | 15,559 | **11.6** | **107** | 809 | 64.5 % | 89.1 % | 30.3 | 349 | 739 |
| row3 (20 SDR) | 36,460 | **67.5** | 244 | 626 | 21.2 % | 45.3 % | 95.4 | 575 | 1,152 |
| mix20 | 14,119 | 7.4 | 29.8 | 80.0 | 80.6 % | 96.9 % | 18.5 | 218 | 592 |
| mix100 | 48,223 | 5.7 | 52.1 | 94.7 | 67.5 % | 92.6 % | 8.7 | 90.0 | 405 |
| mix300 | 143,811 | 3.7 | 13.5 | 35.3 | 55.1 % | 93.1 % | 4.7 | 37.4 | 374 |
| harsh20 | 20,317 | 13.6 | 71.9 | 196 | 45.4 % | 78.8 % | 43.3 | 843 | 2,212 |
| coast30 | 17,536 | 10.7 | 55.1 | 326 | 70.4 % | 92.4 % | 26.5 | 394 | 830 |
| step | 13,454 | 7.9 | 58.5 | 342 | 77.8 % | 95.6 % | 26.1 | 272 | 482 |
| anchorfault | 12,513 | 8.1 | 51.6 | 299 | 64.0 % | 92.1 % | 25.3 | 240 | 551 |

### Solver fixes and hold-out tracks

The second table covers the per-cluster solver fixes before tracking, and the hold-out aircraft. Hold-out aircraft are ADS-B aircraft solved as if they had no ADS-B.

| Scenario | Fixes P50 / P95 / P99 (m) | Hold-out track vs truth P50 / P95 (m) | Live hold-out vs ADS-B reference P50 / P95 (m) |
|---|---|---|---|
| row1 | 11.5 / 48.0 / 200 | 6.7 / 18.3 | 41.4 / 154 |
| row2 | 28.6 / 132 / 228 | 6.0 / 14.7 | 50.3 / 179 |
| row3 | 119 / 347 / 580 | 104 / 209 | 50.3 / 291 |
| mix20 | 17.4 / 95.4 / 193 | 6.7 / 18.8 | 50.3 / 217 |
| mix100 | 9.8 / 89.3 / 161 | 4.0 / 10.1 | 32.4 / 81.9 |
| mix300 | 6.0 / 26.2 / 67.8 | 3.5 / 8.3 | 35.7 / 86.0 |
| harsh20 | 45.6 / 265 / 517 | 16.2 / 86.1 | 121 / 1,259 |

### Vertical error

The published height is the solver/tracker height. The Mode S altitude row is converted to height above the ellipsoid through the GNSS−baro field.

| Scenario | P50 | P95 | P99 | Level flight P95 | Climbing/descending P95 |
|---|---|---|---|---|---|
| row1 | 2.4 | 75.2 | 379 | 178 | 44.7 |
| row2 | 2.3 | 74.2 | 99.1 | | |
| mix20 | 1.7 | 8.3 | 40.7 | 11.7 | 7.7 |
| mix100 | 3.2 | 275 | 377 | | |
| mix300 | 2.0 | 24.1 | 49.4 | | |
| harsh20 | 1.7 | 6.1 | 72.3 | | |

**Vertical tail: diagnosed, not fixed.** The tail is in level flight and is per aircraft. In row 1, two Mode S-only aircraft at FL320 and FL350 are 364 m and 377 m too high for their whole track.
- The GNSS−baro field has no learned cell in that altitude band: no ADS-B aircraft there reported GNSS height.
- `Field.Offset` therefore returns the fallback: geoid only, +44 m, σ 150 m, because no `ref.Weather` source is wired.
- The simulated truth is GNSS−baro ≈ −340 m (cold air aloft).
- The σ is honest (2.5σ), and the horizontal solution barely changes. The height is still wrong by 1,200 ft.

This is filed in HOOKS.md for WP-RF: borrow from neighbouring altitude bands of the same column, and add a weather source.

### Gates

| Gate (§13.2) | Measured | Result |
|---|---|---|
| Row 1: P50 < 20 m, P95 < 60 m, 1σ 60-75 % | 5.5 m / 17.9 m / 63.1 % | pass |
| Row 2: P50 < 80 m, P95 < 250 m | 11.6 m / 107 m | pass |
| Row 2: ≥ 90 % of Mode S-only aircraft in the 4-station footprint get fixes | 26/26 (100 %) | pass |
| Row 3: P50 < 200 m | 67.5 m | pass, with the weaknesses in §6 |
| §9.8 targets for SDR-only networks, P95 500 m | 244 m | within target |

## 2. Availability (aircraft without ADS-B positions)

The aircraft counted here are Mode S-only aircraft inside the radius.
- **"≥ 4-station aircraft"** means aircraft with at least 20 seconds in which one of their transmissions was heard by at least 4 stations.
- **"Seconds covered"** counts the aircraft-seconds with such a transmission that have a published position within ±2 s. A coasting prediction (at most 5 s old) counts as a position.
- **"All seconds"** includes the seconds with fewer than 4 stations.

| Scenario | ≥ 4-station aircraft with fixes | ≥ 4-station seconds covered | All Mode S-only seconds covered |
|---|---|---|---|
| row1 | 21/21 | 4,525/4,525 (100 %) | 99.9 % |
| row2 | 26/26 | 5,872/5,876 (99.9 %) | 99.9 % |
| row3 | 35/35 | 7,724/7,749 (99.7 %) | 99.7 % |
| mix20 | 28/28 | 5,957/5,983 (99.6 %) | 96.1 % |
| mix100 | 102/102 | 22,295/22,295 (100 %) | 98.1 % |
| mix300 | 302/302 | 67,969/67,969 (100 %) | 99.5 % |
| harsh20 | 18/18 | 4,027/4,027 (100 %) | 89.6 % |
| coast30 | 31/31 | 6,845/6,845 (100 %) | 100 % |
| anchorfault | 31/31 | 5,562/5,569 (99.9 %) | 89.0 % |

The 3+altitude solver and 2-station track maintenance give positions in some seconds with fewer than 4 stations. Without them, the "all seconds" column would be lower.

## 3. False fixes

- **"> 1 km"** and **"> 5σ + 50 m"** are shares of published positions, out of every published position.
- **"Inside"** gives the same two shares for aircraft inside the radius only.
- **"Ghost"** counts published addresses that the simulation never had.
- **"No truth"** counts published positions whose aircraft had already left the simulated world, so they could not be scored.

| Scenario | > 1 km | > 5σ + 50 m | Inside: > 1 km / > 5σ + 50 m | Ghost | No truth |
|---|---|---|---|---|---|
| row1 | 0.38 % | 1.57 % | 0.000 % / 0.000 % | 0 | 9 |
| row2 | 0.47 % | 0.32 % | 0.67 % / 0.67 % | 0 | 4 |
| row3 | 1.42 % | 5.49 % | 0.25 % / 3.91 % | 0 | 2 |
| mix20 | 0.72 % | 0.68 % | 0.000 % / 0.000 % | 0 | 0 |
| mix100 | 0.20 % | 0.47 % | 0.000 % / 0.023 % | 0 | 39 |
| mix300 | 0.28 % | 0.35 % | 0.22 % / 0.25 % | 0 | 35 |
| harsh20 | 3.68 % | 2.95 % | 0.28 % / 0.38 % | 0 | 12 |
| coast30 | 0.83 % | 0.83 % | 0.45 % / 0.45 % | 0 | 7 |
| step | 0.03 % | 0.35 % | 0.000 % / 0.71 % | 0 | 15 |
| anchorfault | 0.44 % | 1.11 % | 0.90 % / 0.82 % | 0 | 9 |

The address filter produced **no ghost tracks**. The tails sit outside the receiver hull: aircraft beyond the radius, where the geometry is poor and grade C still reaches the map. Under heavy impairments (`harsh20`), 3.7 % of all published positions are more than 1 km off, nearly all outside the radius.

## 4. Clock synchronisation

The table shows |relative clock error| of synced stations: P50 / P95 in ns, against their component's anchors (or the free-clock median). Samples are taken every 5 s over the evaluation window.

| Scenario | GPS | Radarcape 12 MHz | Beast | Airspy | SBS | RTL-SDR | Within 3σ | Steps |
|---|---|---|---|---|---|---|---|---|
| row1 | 2.3 / 8.3 | | | | | | 88.5 % | 0 |
| row2 | 2.4 / 11.2 | | | | | 46.9 / 175 | 93.5 % | 1 |
| row3 | | | | | | 142 / 1,154 | 65.6 % | 336 |
| mix20 | 4.4 / 7.8 | 10.1 / 20.2 | | | | 52.6 / 195 | 92.8 % | 0 |
| mix100 | 11.3 / 15.3 | 9.7 / 19.7 | 5.6 / 18.2 | 13.0 / 40.1 | 40.7 / 135 | 49.6 / 168 | 92.0 % | 2 |
| mix300 | 9.0 / 28.5 | 10.8 / 25.6 | 11.2 / 25.9 | 16.4 / 49.0 | 43.1 / 150 | 53.4 / 185 | 83.8 % | 7 |
| harsh20 | 1.2 / 9.7 | 15.4 / 32.0 | | | | 60.3 / 182 | 94.2 % | 9 |
| coast30 | 0.6 / 4.7 | | 10.0 / 20.5 | 18.2 / 54.3 | | 49.2 / 163 | 99.0 % | 0 |

**Network time vs UTC.** In anchored networks, |network time − UTC| is 130-250 ns and stable within about 1 ns. This offset is the mean cable and receiver delay of the anchors (simnet: 50-300 ns per GPS unit). No receiver can observe its own cable delay without a calibration, so this is expected and not an error of the filter. It cancels out of the positions.

**Step recovery** (`step`: silent 22 µs step on one RTL-SDR, full pipeline):
- detected after **1.0 s**;
- re-synced (synced and within 3σ of the truth) after **1.5 s**;
- 0 of 58 synced epochs beyond 3σ in the 30 s after the step;
- published fixes beyond 3σ: 151/2,703 (5.6 %) in the 30 s after the step, compared with 222/2,759 (8.0 %) in the 30 s before.

Gate: detected < 5 s, re-synced < 30 s, no fix error > 3σ during recovery. **Pass**, with this reading: fixes beyond 3σ did not increase during the recovery. The harness cannot attribute a fix to the stepped station, so the gate is read as "no increase".

**Anchor fault** (`anchorfault`: +1 µs on a GPS anchor with good status bits, full pipeline):
- demoted after **1.0 s**;
- network-time shift 0.4 ns, at most 1.8 ns at any time after the fault.

Gate: demoted < 10 min, shift < 20 ns. **Pass.**

Clock-package gates (filter only, `go test -run TestGate ./internal/mlat/clock`):

| Gate | Result |
|---|---|
| Silent 22 µs step | detected +0.5 s, re-synced +1.0 s; 2 of 180 epochs beyond 3σ, 0 beyond 4σ |
| 1 µs anchor fault | demoted +0.5 s, shift 0.3 ns |
| False GPS claim | the self-asserted fake anchor was demoted on revalidation as soon as genuine anchors became eligible (420 s) |
| β_short (2 µs on 5 SDRs) | estimates 1,988-2,014 ns against the 2,000 ns truth (within 100 ns); GPS stations at −5 to −8 ns |

All four pass.

## 5. Latency, CPU and memory

### Latency

Latency is measured from transmission to publication (`Fixes()`), in virtual time with 10 ms resolution. Only measurement updates count; coasting outputs are excluded. In every run, 18-30 % of published positions were coasting outputs.

| Scenario | Network latency | P50 | P95 | P99 |
|---|---|---|---|---|
| row1 | 20-80 ms | 0.505 s | 0.513 s | 0.515 s |
| mix20 | 20-80 ms | 0.501 s | 0.513 s | 0.516 s |
| mix100 | 20-80 ms | 0.387 s | 0.398 s | 0.404 s |
| mix300 | 20-80 ms | 0.466 s | 0.473 s | 0.478 s |
| harsh20 | 20-300 ms | 0.613 s | 0.620 s | 0.629 s |

Gate: result p95 < 0.6 s at 50 ms network jitter. **Pass.** With 300 ms network latency (`harsh20`), p95 is 0.62 s. The narrow spread shows that latency is set by the grouper's adaptive flush (p99 station latency, clamped to 150-600 ms), not by processing.

### CPU and memory

- **Ingest + engine (cores):** wall time of the ingest and engine in synchronous mode, one goroutine, per simulated second.
- **Engine µs/group:** the engine's own processing time per group.
- **Clock epoch:** the mean wall time of one clock filter step (every 0.5 s).
- **Heap:** the heap retained by ingest + engine (heap before minus after releasing them), with the process total in brackets.

| Scenario | Groups/s | Clusters/s | Ingest + engine (cores) | Engine µs/group | Clock epoch | Heap |
|---|---|---|---|---|---|---|
| row1 | 1,552 | 581 | 0.028 | 14.1 | 0.1 ms | 48 MB (of 49) |
| mix20 | 1,582 | 468 | 0.038 | 16.5 | 0.4 ms | 56 MB |
| mix100 | 3,820 | 1,280 | 0.23 | 29.0 | 12.9 ms | 145 MB |
| mix300 | 9,816 | 2,997 | **0.99** | 32.4 | **76.9 ms** | **412 MB** (of 414) |

**Live binary** (`internal/mlat/cmd/mlatsim`, built under the scratchpad, asynchronous workers, listener on 127.0.0.1:18640). The run used 300 stations, 1,500 aircraft, 450 km and 12 simulated minutes, in 10 min 42 s wall. It was stopped when it finished.
- Engine CPU was 308 s for 720 s simulated (**0.43 cores**, 32 workers), 43.7 µs per group, 0 drops.
- The last clock epoch took 68.7 ms.
- Process peak working set was **1.83 GB**, and the process used 1,362 s CPU in total. Most of both is simnet itself: 426 million receiver frames for 1,500 aircraft × 300 receivers.
- `/stats` and `/public` answered during the run.
- Accuracy is not comparable with the table above: anchors became eligible only at 11 minutes, so most of the run was in relative time.

**Hot-path gates** (`go test -bench`, idle machine unless stated):

| Item | Gate | Measured | Result |
|---|---|---|---|
| Grouper | ≤ 300 ns per copy, 0 allocs | 150-172 ns, 0 allocs | pass |
| Lane credit | ≤ 120 ns | 114 ns | pass |
| AVF encode | ≤ 400 ns per frame | 446 ns per 64-frame record (7 ns per frame) | pass |
| Engine hot path | 0 allocs per group | 0 allocs (`TestHotPathAllocs`) | pass |
| Solver, 6 stations with altitude | ≤ 20 µs | 16.8 µs | pass |
| Solver, 6 stations SDR with altitude | ≤ 20 µs | 20.2-20.8 µs | borderline |
| Solver, 6 stations without altitude | ≤ 20 µs | **26.8 µs** | **fail** (known since WP-SV; flat vertical/τ valley) |
| Clock epoch, 200 stations | ≤ 80 ms | 71.8 ms median (idle); **92.8 ms** while three other simulations loaded the machine | pass when idle, little margin |

At 300 stations, `mix300` measured 76.9 ms per epoch in the full pipeline, with partitioned components.

**Scale projection (§20: 1,000 feeders, 10k aircraft).** Both the engine cost (µs per group) and the clock filter cost grow faster than linearly with stations:
- At 300 stations, one synchronous core is fully used (0.99), and the asynchronous engine needs 0.43 cores.
- Linear extrapolation of groups/s gives about 3-4 cores for the engine at 1,000 stations. The partitioned clock filter keeps each component's epoch near the 80 ms budget, but runs several components per 0.5 s.
- Ingest + engine memory is about 1.4 MB per station at 5 aircraft per station, so about 1.5-2 GB at 1,000 stations / 10k aircraft.

**These are projections. A 1,000-station MLAT run was not executed** (simnet alone would need more than 1 hour of wall time per simulated 16 minutes on this machine).

## 6. Where we stand against mlat-server (honest comparison)

We could not run mlat-server here. It is AGPL-3.0, has no clean-room relevance, and there is no real-world capture to run it on. The comparison is therefore against what RESEARCH.md §1.9 / §4.9 records as publicly reported.

**Better (in simulation):**
- **Accuracy with anchors and density.** Published P50 is 4-12 m and P95 13-107 m for anchored networks of 20-300 stations. The publicly reported mlat-server figures are "tens to hundreds of metres", and Flightradar24 reports 10-100 m typical with a worst case of about 1 km. Our gain comes from the GPS anchors (mlat-server does not exploit absolute UTC), the joint clock filter and the raw-TOA tracker.
- **Latency.** 0.40-0.51 s p95 at 20-80 ms network latency, compared with mlat-server's fixed 0.9 s (wiedehopf) or 2.5 s (mutability) grouping delay.
- **Clock pairs.** Published mlat-server pair quality is RTL-SDR "well under 1 µs", Beast about 100 ns and GPS tens of ns. Ours: RTL-SDR P50 about 50 ns / P95 about 185 ns, Beast and Radarcape 12 MHz 6-11 ns / 18-26 ns, GPS 1-11 ns / 5-29 ns, all against the anchors.
- **Clock faults.** Steps and anchor faults are detected within 1 s and do not disturb network time (below 2 ns).
- **Self-measurement.** Integrity and self-measurement exist (RAIM, χ² gates, hold-out). mlat-server has neither.

**Same or worse:**
- **SDR-only networks** (`row3`) are our weak spot:
  - P50 is 67.5 m, which passes the gate, but P95 is 244 m and P99 626 m;
  - σ calibration is far too optimistic (1σ 21 %, 2σ 45 %);
  - the clock filter logged 336 steps in 16 minutes (WIRING/HOOKS items carried since Wave 2B), and only 65.6 % of clock samples are within 3σ.

  That is in the same "tens to hundreds of metres" class as mlat-server. It is not clearly better.
- **σ calibration in large and harsh networks** is optimistic: 1σ coverage is 55 % at 300 stations and 45 % in `harsh20`, against the 60-75 % target. The map ellipses are too small there. Clock σ is optimistic too: 84-94 % of samples are within 3σ, against an expected 99.7 %.
- **Outside the receiver hull**, the published tail reaches 0.4-2.2 km at P99. Under heavy multipath and outliers, 3.7 % of all positions are more than 1 km off. mlat-server shows similar "ragged" tracks at the edges. We are not demonstrably better there.
- **Height** at flight levels without ADS-B GNSS data is off by up to about 380 m (section 1). mlat-server treats pressure altitude as geometric, which is also wrong, by a similar order.
- **The live hold-out statistic** (vs the ADS-B reference) reads 32-50 m P50, while the same tracks are 4-7 m from the truth. The reference's own airframe position latency dominates. A published live P50 therefore overstates our error. That is the safe direction, but it is not a measure of the engine.
- **Simulated, not real.** simnet's clocks, multipath and traffic are models. Real dongles have temperature jumps, USB stalls and antenna problems the models may miss. The mlat-server figures are from real networks. Until the live hold-out of a real network confirms them, our numbers are an upper bound on what to expect.

## 7. Defects and open items found by this suite (filed in HOOKS.md)

1. **WP-RF:** the GNSS−baro field falls back to geoid-only for altitude bands without learned cells. The result is a vertical bias of about 360 m at FL320-350 in row 1, and the mix100 vertical P95 of 275 m.
2. **WP-K:** SDR-only clock robustness: 336 steps in 16 minutes in row 3, and optimistic clock σ (84-94 % within 3σ). The 200-station epoch has little margin (71.8 ms idle, 92.8 ms under load).
3. **WP-M / WP-TR:** published σ is optimistic for SDR-only (1σ 21 %), harsh (45 %) and 300-station (55 %) networks. Grade C positions outside the receiver hull reach 1-2 km.
4. **WP-SV:** a 6-station cluster without altitude takes 26.8 µs, against a 20 µs budget (already known).

Fixed in this work package (test-only): the end-to-end harness counted at most 64 stations per transmission. Footprint statistics of networks with more than 64 stations were undercounted.

## 8. Self-survey (WP-SU-survey, 2026-11-14)

`internal/mlat/survey` on simnet through the real ingest grouper, clock filter and reference store (`SURVEY_GATES=1 go test ./internal/mlat/survey -run TestGate -v -timeout 90m`, fixed seeds; 6 GPS + 10 RTL-SDR peers within 150 km, 250 aircraft, 12 minutes of network warm-up before the targets are registered). Errors are horizontal against the true antenna position; σ_h is the reported 1σ semi-major axis.

| Gate (§13.2 / WP) | Target | Result |
|---|---|---|
| GPS target < 25 m in 5 min; σ ≤ 25 m | Radarcape GPS, unlocated | done after 3 min: error 0.6 m, σ_h 3.5 m, stable, auto-use |
| 12 MHz σ ≤ 500 m; SDR < 300 m in 30 min | Beast hw12, unlocated | done after 3 min: 3.7 m, σ_h 5.4 m |
| | RTL-SDR sdr12 (Locate Me hint 20 km off) | done after 3 min: 8.1 m, σ_h 8.9 m (jitter learned 357 ns) |
| Declared 300 m off → suspect within 15 min | GPS / Beast declared 300 m off | flagged after 3 / 5 min (289 m / 294 m measured, limit 100 m) |
| | GPS declared correctly | not flagged (4 m) |
| MSL/HAE resolved within 30 min | GPS, MSL altitude declared "ambiguous", seeds 14/15/16 | MSL after 3 min (16) and 11 min (15); undecided after 30 min (14: free height 17 m low, σ_v 10 m); never wrong |

Calibration on synthetic surveys (`TestCalibrationSynth`, 30 GPS + 30 SDR): the horizontal error was inside the reported 95 % region in 60/60; the vertical inside 2σ in 27/30 (GPS) and 24/30 (SDR, somewhat optimistic). On simnet the reported σ_h was 1.1-6× the actual error. Vertical without a DEM or declared height uses the peers' median height (σ 200 m), so unlocated stations report σ_v of 60-180 m.

Limits found: the MSL/HAE test needs σ_v ≲ 15 m for the 47 m geoid separation; the reference aircraft's vertical σ (≈ 18 m from the GNSS−baro field) and their along-track errors keep a 30-minute GPS survey at σ_v 6-12 m, so one run in three stays undecided. A declared station 300 m off made the engine's TDOA verification suspend many reference aircraft (filed in HOOKS.md), which cut the observations of nearby surveys about 4× (TestGateDeclared300m). Costs: 40 ns per group without a target (0 allocations); an evaluation of 15 000 observations 0.33 s.

## 9. Network bundle adjustment (WP-SU2-bundle, 2026-11-14)

`internal/mlat/survey` `Bundle`/`Bundler` on simnet through the real ingest grouper and reference store (`BUNDLE_GATES=1 go test ./internal/mlat/survey -run TestBundleSimnet -v -timeout 60m`, seed 31): 6 GPS + 8 RTL-SDR anchors, 9 subjects, 250 aircraft, 3 min warm-up, 30 min collected, one hourly run. Deliberately wrong declarations of 350 m to 10 km. Errors are horizontal against the true antenna position.

| Station | Prior | Declared error | Adjusted error | σ_h | Verdict |
|---|---|---|---|---|---|
| Beast | declared | 0 | 2.4 m | 3.0 m | kept (2 m from the data) |
| RTL-SDR | declared | 0 | 11.1 m | 10.2 m | kept (11 m) |
| Beast | declared | 2000 m | 2.3 m | 3.1 m | released, outlier "declared" |
| RTL-SDR | declared | 800 m | 4.8 m | 10.4 m | released, outlier "declared" |
| Radarcape 12 MHz (MSL) | declared | 10 km | 2.4 m | 3.1 m | released, outlier "declared" |
| Airspy | declared | 350 m | 5.0 m | 10.0 m | released, outlier "declared" |
| Radarcape GPS | none (grid seed) | - | 2.3 m | 3.1 m | determined |
| RTL-SDR | none (grid seed) | - | 7.7 m | 10.2 m | determined |
| Beast | Locate Me hint | - | 2.9 m | 3.1 m | determined |

**Acceptance: the mean horizontal error of the declared stations goes from 2192 m (as declared) to 4.7 m after the adjustment** (8-minute default run: 4.9 m). No anchor and no honest declaration was flagged; variance factor 0.98; 19,120 frames, 244k receptions, 2.9 s compute. Synthetic network (`TestBundleSynthWrongDeclared`, 17 stations incl. 4 GPS anchors, wrong declarations 300 m-12 km): 2191 m -> 1.4 m; a wrong anchor (400 m) is released and flagged "anchor", a station with 3 µs jitter is flagged "noisy" (`TestBundleWrongAnchorAndNoisy`); two unlocated stations with only one declared peer are solved to 0.8 / 2.3 m (`TestBundleTwoUnlocatedOnly`).

Limits: vertical errors of free-running SDR stations reach 80-120 m (σ_v 35-62 m, i.e. 2-3σ: their level-dependent timing biases go into the height); GPS/hw12 classes stay within 11 m. Cost: `BenchmarkBundleMax` (64 stations, 30 min, 66k frames, 2.8M receptions) ~27 s and ~0.5 GB transient allocation per run on 8 workers (scratch reuse cut it from 35 s / 1.4 GB); `Bundler.Observe` 100 ns per group, 0 allocations. Not yet wired into the engine (HOOKS.md).
