# 3.4 MLAT explained

Most aircraft broadcast their own GPS position (ADS-B). Many do not: older transponders and many military, business and general-aviation aircraft send only Mode S replies with their address and altitude. **Multilateration (MLAT)** locates them anyway, from the tiny differences in the times at which several receivers heard the same transmission. This part explains how AirView's own MLAT engine does that, what it needs from the stations, and how accurate it is.

All accuracy figures in this part come from simulations (`docs/aggregator/MLAT-RESULTS.md`, measured 2026-11-14). **There is no real-world measurement yet.** Every network measures its own accuracy continuously with the hold-out method (3.4.6), and the MLAT console shows the result (3.2.7).

---

## 3.4.1 The principle

A radio signal travels at the speed of light, about 30 cm per nanosecond. If two receivers hear the same transmission and their clocks agree, the difference between their reception times says how much closer the aircraft is to one than to the other: every 1 µs of difference is 300 m of difference in distance. All points with the same difference lie on a curved surface (a hyperboloid) between the two receivers.

- With **4 receivers** (3 independent time differences) the surfaces meet in one point: the aircraft's position in three dimensions.
- With **3 receivers** plus the aircraft's reported altitude, the position is also determined.
- With **2 receivers** a known track can be kept going for a while, but not started.

Two things decide the accuracy:

1. **Clocks.** The receivers' clocks must agree to within tens of nanoseconds. Real clocks drift: an RTL-SDR dongle's oscillator is typically off by 30-100 parts per million and changes with temperature.
2. **Geometry.** Receivers spread around the aircraft give sharp intersections; receivers in a line, or an aircraft far outside the receiver area, give long, thin uncertainty regions. This is described by the Cramér-Rao bound (CRLB), the best accuracy the geometry permits, which the MLAT console shows as "Expected accuracy".

---

## 3.4.2 Synchronising the clocks

AirView synchronises all receiver clocks continuously from aircraft whose positions are known, mostly ADS-B aircraft:

- When an ADS-B aircraft is heard by two stations, the network knows where the aircraft was, so it knows how long the signal took to reach each receiver. The difference between expected and measured reception times is the offset between the two receivers' clocks.
- A **clock filter** combines thousands of such comparisons every 0.5 s and estimates, for every station, its offset, its rate (drift) and its noise. Stations that share aircraft form a **clock component**; large components are split above `aggregator.mlat.maxComponentStations` (200 stations).
- **Anchors** are stations with GPS-disciplined timestamps (for example a Radarcape with GPS). They tie network time to UTC. A new GPS station must prove itself for 10 minutes before it becomes an anchor, and an anchor whose timestamps disagree with the others is demoted within about a second.
- **Clock steps** (a dongle that drops samples, a USB stall) are detected and re-synchronised automatically: in the simulations a silent 22 µs step was detected after 1.0 s and re-synced after 1.5 s; a 1 µs fault on a GPS anchor whose status bits still said "locked" was demoted after about 0.5-1 s, and network time moved by less than 2 ns.

| Clock class (shown in the station tables) | Typical source | Synced clock error P50 / P95 against the anchors (simulation) |
|---|---|---|
| GPS (UTC), GPS (GPS time) | Radarcape with GPS | about 1-11 ns / 5-29 ns |
| 12 MHz hardware | Mode-S Beast, Radarcape without GPS | about 6-11 ns / 18-26 ns |
| SDR 12 MHz (and high-rate) | RTL-SDR, Airspy via readsb or `rtl_tcp` | about 50 ns / 160-195 ns |
| SBS 20 MHz | Kinetic SBS receivers | about 40-47 ns / 135-150 ns |
| No timestamps | SBS / AVR text inputs, or *No timestamps* chosen | not used for MLAT |

### The timing quarantine

A new station is not trusted at once. Its timing is compared with the network for a few minutes (3 minutes after its first comparison by default) before its receptions are used for positions. A station whose timing does not fit, for example because its declared position is kilometres off, stays in quarantine ("timing inconsistent with the network") until the cause is fixed. The admin can override this per station (Stations tab: *Timing: trust now*, *judge again*, *exclude from MLAT*, *automatic*).

---

## 3.4.3 Station positions

A 30 m error in a receiver's position is a 100 ns error in every one of its timestamps. MLAT therefore needs each antenna's position to within a few tens of metres, and AirView checks every declared position:

- **Survey ("reverse MLAT").** From aircraft whose positions are known, the network solves for the receiver's own position. In the simulations a GPS or Beast receiver without a declared position was located within 3 minutes to 0.6-3.7 m (reported σ 3.5-5.4 m), and an RTL-SDR to 8 m (σ 9 m).
- **Checks.** Declared positions are re-surveyed every 6 hours (`aggregator.mlat.surveyIntervalH`). A station more than max(100 m, 3σ) from its survey is marked **suspect** and excluded. In the simulations a position declared 300 m off was flagged after 3-5 minutes; a correct one was never flagged.
- **Automatic use.** A surveyed position with σ ≤ 50 m that passes the checks is used for MLAT straight away (`aggregator.mlat.autoUseSurvey`); the owner is told and can override it.
- **Bundle adjustment.** Once an hour the network solves all station positions and clocks together. In the simulation, stations declared 350 m to 10 km off were corrected to 2-5 m (mean error 2,192 m before, 4.7 m after), while honest declarations and anchors were kept.
- **Height.** The survey also tells whether a declared altitude was above mean sea level or above the ellipsoid (in the UK they differ by roughly 45-55 m), but that needs a vertical uncertainty below about 15 m, so it is not always decided.

Surveyed positions are visible only to the station's owner and the admins.

---

## 3.4.4 From receptions to a position

1. **Grouping.** Every reception of a Mode S transmission is sent to the aggregator with its timestamp. Copies of the same transmission from different stations are grouped. The group is closed after an adaptive wait (the 99th percentile of the stations' network delay, clamped to 150-600 ms).
2. **Planning.** Groups of aircraft that need MLAT (no usable ADS-B position, or a hold-out aircraft) with enough synced stations are solved.
3. **Solving.** A least-squares solver finds the position that best fits the measured time differences, with the reported pressure altitude as an extra measurement when only 3 stations heard the transmission. A 6-station solution takes about 13-16 µs of CPU.
4. **Tracking.** A tracker combines successive solutions and raw time differences into a smooth track with velocity, and rejects solutions that contradict the track. Results get a grade; only displayable grades reach the map.
5. **Integrity checks.** χ² tests and receiver autonomous integrity monitoring (RAIM) remove stations whose timing does not fit a solution; a station that repeatedly contradicts ADS-B positions gets its own position re-surveyed.
6. **Height.** Mode S reports pressure altitude, not geometric height. AirView learns the difference between the two (GNSS height minus barometric altitude, which depends on the weather) from ADS-B aircraft that report both, by area and altitude band, and converts.

Positions reach the map as aircraft with the source "MLAT". Public viewers see the position, not which stations produced it.

---

## 3.4.5 ADS-B verification

The same timing that locates Mode S aircraft also checks ADS-B aircraft: if the time differences do not match the position an aircraft broadcasts, the position is wrong (a faulty transponder, or spoofing). The engine counts confirmed and contradicted positions; an aircraft that keeps being contradicted is marked **suspect** and multilaterated for 10 minutes. The admin sees this in the MLAT console's **ADS-B verification** card.

---

## 3.4.6 How AirView measures its own accuracy: the hold-out

A network cannot know the true position of an aircraft that does not broadcast it. To measure its accuracy anyway, AirView picks 10 % of the ADS-B aircraft with a good position report (`aggregator.mlat.holdout` = 0.1; NACp ≥ 9, chosen by a key that changes every 10 minutes) and solves them **as if they had no ADS-B**: no ADS-B position, velocity or GNSS height is used for them, and they are hidden from the clock filter's references. Their MLAT positions are then compared with what they broadcast.

The MLAT console shows these figures as **Hold-out track P50 / P95** and over time in the **Hold-out accuracy** chart. Two cautions:

- The ADS-B position itself lags by up to a few hundred milliseconds in many aircraft, so the comparison overstates the MLAT error. In the simulations the live hold-out read 32-50 m P50 while the same tracks were 4-7 m from the truth. AirView therefore also shows an estimate from the error across the track only (cross-track), which this lag does not affect.
- The hold-out needs some minutes of traffic before it has samples; the console shows "no hold-out samples yet" until then.

The public **Network** page shows the same measurement to everyone as **MLAT accuracy** (median and P95 estimated from the cross-track error, the comparison with ADS-B, fixes per minute), computed only from areas served by at least 5 stations. The screenshot is from the demonstration network about 10 minutes after the start.

![The public Network page with the measured MLAT accuracy](img/network/public-network.png)

---

## 3.4.7 Results for the feeders

Every second, each MLAT aircraft's latest position goes back to the stations that contributed to its track in the last 60 seconds and sent at least 10 timed frames per minute while synced (the minimum-contribution rule; `aggregator.mlat.returnResults`). Results carry no information about which other stations took part. The feeder offers them locally (3.3.4); they are marked as MLAT and are never forwarded to another network.

---

## 3.4.8 Accuracy in numbers (simulation)

The test suite (`internal/mlat`, `TestMLATResults`) runs simulated networks through the real ingest, clock filter, solver and tracker and compares every published position with the simulated truth. Common conditions: 30 % of aircraft Mode S-only, 10 % hold-out, aircraft position-report latency 0-300 ms, network latency 20-80 ms, realistic clock impairments (RTL-SDR 30-100 ppm with thermal drift and 300-500 ns jitter; GPS 15-45 ns jitter), 1-2 % multipath and rare gross outliers. Each run simulates 16 minutes and is evaluated over the last 4; "inside" means aircraft inside the network radius.

**Horizontal error of published positions, inside the network area** (latest run, 2026-11-14):

| Network | P50 | P95 | P99 | Within the reported 1σ / 2σ |
|---|---|---|---|---|
| 8 GPS Radarcapes, 150 km | **5.3 m** | **17.5 m** | 47 m | 64 % / 93 % |
| 4 GPS + 20 RTL-SDR, 150 km | **11.5 m** | **106 m** | 692 m | 63 % / 89 % |
| 20 RTL-SDR only, 150 km | **31.9 m** | 157 m | 468 m | 73 % / 93 % |
| 20 mixed (3 GPS, 2 Radarcape, 15 SDR) | 7.6 m | 30.2 m | 66 m | 79 % / 97 % |
| 100 stations, typical community mix, 300 km | 5.4 m | 51.8 m | 91 m | 70 % / 94 % |
| 300 stations, community mix, 450 km (earlier run) | 3.6 m | 13.5 m | 36 m | 57 % / 94 % |
| 20 mixed, heavy multipath and clock faults | 14.9 m | 81.6 m | 201 m | 48 % / 81 % |
| 30 stations along a coast (half the area over sea) | 10.5 m | 54.5 m | 324 m | 71 % / 93 % |

The "community mix" is 62 % RTL-SDR, 10 % Airspy, 10 % Beast, 4 % Radarcape without GPS, 10 % GPS Radarcape and 4 % SBS.

**Design targets (DESIGN §13.2) and result:**

| Target | Measured | Result |
|---|---|---|
| 8 GPS stations: P50 < 20 m, P95 < 60 m | 5.3 m / 17.5 m | pass |
| 4 GPS + 20 SDR: P50 < 80 m, P95 < 250 m | 11.5 m / 106 m | pass |
| SDR only: P50 < 200 m | 31.9 m | pass |
| Mode S-only aircraft in the 4-station footprint that get positions | 100 % | pass |
| Results published within 0.6 s (P95) at 50 ms network jitter | 0.40-0.51 s | pass |

**Other figures:**

- **Availability.** Of the Mode S-only aircraft heard by at least 4 stations, every one got positions; 99.6-100 % of those aircraft-seconds were covered.
- **Wrong positions.** More than 1 km off: 0.002-0.5 % of all published positions in normal networks, 3.5 % under heavy multipath, almost all of them outside the receiver area. No "ghost" aircraft were published.
- **Height.** Vertical P95 37.6 m in the 8-GPS network. Mode S-only aircraft at flight levels where no ADS-B aircraft reports GNSS height can be wrong by up to about 380 m in height (the simulation has cold air aloft and no weather source); the horizontal position is barely affected.
- **Latency** from transmission to the map: 0.39-0.51 s (P95) at 20-80 ms network latency, 0.62 s at 300 ms.
- **Live runs.** The `airview` binary with simulated feeders over real sockets: all timed stations left the quarantine about 6 minutes after the start; about 13 minutes after the start the public MLAT quality was P50 / P95 43 / 306 m (the cross-track estimate said 29 / 346 m), with about 5,500 positions per minute.

### Honest limits

- **Simulated, not real.** Real dongles have temperature jumps, USB stalls and antenna problems the models may miss. Until real networks confirm these numbers, treat them as an upper bound.
- **SDR-only networks** are the weak spot: P95 of roughly 100-350 m depending on the network, and more frequent clock steps. That is in the same "tens to hundreds of metres" class as other community MLAT networks, not clearly better.
- **Reported uncertainty** is optimistic in very large and in harsh networks: the 1σ ellipses on the map contain 48-57 % of the true positions there instead of the intended 60-75 %.
- **Outside the receiver area** the error grows quickly: P99 of 0.4-2.2 km.
- **Height** at flight levels without ADS-B GNSS reports, as above.

### What makes a network accurate

1. **GPS-timed anchors.** A few GPS Radarcapes spread over the area make the largest difference (5 m P50 with 8 GPS stations against 32 m with SDR only).
2. **Correct antenna positions.** Enter your position to 10 m and say whether the height is MSL or HAE; or leave it blank and let the network survey it.
3. **Density and spread.** Four or more stations around each area; the **Where to add a station** card (admin) shows where one more receiver helps most.
4. **Timestamps on.** Stations that send *No timestamps* feed the map but cannot help MLAT.

---

## 3.4.9 Cost

Measured CPU and memory of ingest plus the MLAT engine (simulation, one process):

| Network | CPU | Memory | Clock filter step (every 0.5 s) |
|---|---|---|---|
| 8 GPS stations | 0.03 cores | 48 MB | 0.1 ms |
| 100 stations, 600 aircraft | 0.23 cores | 145 MB | 13 ms |
| 300 stations, 1,500 aircraft | about 1 core | 412 MB | 77 ms |
| 1,000 stations, 10,000 aircraft (projection, not measured) | 3-4 cores | 1.5-2 GB | partitioned, about 80 ms per component |
