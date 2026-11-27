# Bistatic SSR self-location: feasibility study (WP-SSR-research)

Status: **experimental, off by default** (`ssr.enabled=false`). Advisory only. Never used to set or confirm a
station position automatically. Code: `internal/mlat/ssr` (estimator), `internal/mlat/ssr/ssrsim` (ground-truth
simulator), `internal/mlat/ssr/cmd/ssrsim` (the table in §6). Wiring: `internal/mlat/ssr/WIRING.md`. Date: 2026-11-14.

## 1. Question

Can a single 1090 MHz receiver find its own antenna position from the timing structure of secondary
surveillance radar (SSR) replies? It would have no peers, no GPS and no synchronised clock. It has no
1030 MHz receiver either, so it cannot hear the interrogations themselves. If it can, under what conditions,
and how well?

## 2. Short answer

**Yes in simulation, conditionally in reality.** If one or more rotating interrogators within about 250 km
elicit **Mode A/C replies** (legacy/conventional SSR, or Mode S radars that send Mode A/C interrogations
answered by Mode S transponders), or **DF11 all-call replies without II lockout**, and the receiver decodes
Mode A/C with its sample-clock timestamps and the ADS-B positions of the replying aircraft, then:

- 10 minutes give a horizontal error of about **5 m median and ≤ 15 m worst** (10 seeds, three radars, Beast-
  or RTL-class receiver). The interrogator sites need not be known: the method learns them, with their
  rotation periods.
- The reported σ_h is consistent: median error/σ_h is about 0.4-0.9.
- 30 minutes give about 2 m median and 4 m worst. One radar gives about 15 m median and 25 m worst. Three
  minutes give 13 m median and 34 m worst.
- Light traffic (40 aircraft) is marginal: 30 m median, 3 of 10 runs not converged (up to 126 m), and one
  accepted run 85 m off at 1.9σ.
- Height is weak without a height hint (errors of tens of metres, up to hundreds in light traffic). Pass
  DEM + AGL as the hint.

**Clear negative result:** where the only interrogators are Mode S radars with II/SI lockout (aircraft locked
out do not answer that radar's all-calls), and no Mode A/C interrogations reach Mode S transponders, there are
almost no common-interrogation pairs. Nothing is produced after 10 or 30 minutes (§5). Today's European en-route
environment is largely Mode S with lockout. Whether a given site works therefore depends on its local radar
environment, which the estimator's live counters reveal within minutes (§8).

The simulator encodes the same physical model the estimator assumes (§7). The numbers therefore validate the
algorithm and its error budget, **not** the real-world assumptions. A field trial with a surveyed receiver is
the open step.

## 3. Principle

A rotating SSR at R sends an interrogation at time t₀. Every transponder inside the main beam answers the
same interrogation after a fixed delay d: Mode A/C 3.0 µs ± 0.5 µs, Mode S all-call 128 µs ± 0.25 µs (ICAO
Annex 10 Vol IV). Aircraft K and J, both at positions known from their ADS-B, are heard by the receiver U at

    t_K = t₀ + ρ(R,A_K)/c + d + β_K + ρ(A_K,U)/c,   likewise t_J.

The difference Δ = t_J − t_K does not contain t₀ or U's clock offset. The two replies are less than about 1 ms
apart, so even a free-running 12 MHz crystal (25 ppm) contributes less than 0.03 ns:

    c·Δ − [ρ(R,A_J) − ρ(R,A_K)] − c(β_J − β_K) = ρ(U,A_J) − ρ(U,A_K)

This is a hyperbola for U with foci A_K and A_J: a TDOA measurement made by one receiver. It is "bistatic"
because the interrogator is the other end of the path.

**Why the radar site hardly matters.** Both aircraft are in one beam, so they lie on (almost) the same
radial from R. A site error δR changes the bracket by (e_J − e_K)·δR, where the unit vectors e from R differ
by at most the beam width (about 2.5°, 0.04 rad) plus the elevation difference. A 300 m site error costs
about 10 m per observation and averages down over many azimuths. The `sites-1km-off` row in §6 shows this:
claimed sites that are wrong by 1 km still give about 7 m.

**Nuisance parameters.** β is the per-airframe reply-delay offset, a fixed transponder property within the
Annex 10 tolerance. It is solved per airframe and reply class, with a prior at the tolerance. ADS-B position
latency (an uncompensated time-of-applicability error) is solved per airframe along its velocity. Without the
latency parameter, 40 % of airframes with 0-0.6 s of latency gave 15-24 m and err/σ ≈ 2-4. With it, about 6 m.

## 4. Method (implemented)

1. **Identify** Mode A/C replies. A reply is attributed to an aircraft when exactly one tracked aircraft has
   that code as its squawk, or as its barometric altitude in Gillham code within ±50 ft. Conspicuity codes and
   shared codes are dropped. In the baseline about 30 % of heard Mode A/C replies are attributed: many
   aircraft share a flight level, and FRUIT is not attributable. DF11 replies carry the address and the
   interrogator identifier.
2. **Dwells and train matching.** The replies of one airframe form a burst while the beam passes (about
   20-60 ms, 5-15 interrogations). For two overlapping bursts, the pairwise arrival differences cluster at Δ
   when both aircraft answered the same interrogations. Staggered PRFs make the cluster unique. With a
   constant PRF, a PRI alias that also fits inside the geometric window (|Δ| ≤ 2·separation/c) is detected and
   the pair rejected. Each cluster gives one observation (mean of N matches, N ≥ 4).
3. **Interrogators.** Each pair lies on a radial of its radar. In a gnomonic projection (great circles become
   straight lines) every pair is a bearing line through the site. A Hough vote, restricted to the part of each
   line whose expected miss distance is under 10 km, finds candidate sites; robust weighted least squares
   intersects their lines. The antenna **rotation** is then recovered from the pair times: the mean resultant
   length of azimuth − 360°·t/P per 60 s window, maximised over P = ±2..15 s. Sites without a coherent
   rotation (line crossings) and "ghost" sites whose pairs fit a busier site are discarded. Observations are
   assigned by beam geometry and beam phase. In simulation the learned sites end up about 100-700 m from
   the truth, with periods correct to better than 0.1 %.
4. **Solve.** A pseudo-Huber seed grid (300 km radius without a hint), then robust Gauss-Newton on the station,
   every site (prior from the line fit, at least 1.5 km σ, so the Δ data can move a poorly learned site), the
   airframe biases and the latencies. The height prior is 100 m ± 1 km without a hint, or DEM + AGL ± 20 m
   with one. Huber weights, 5σ outlier rounds, and a covariance scaled by max(1, χ²/dof).

## 5. The negative case: Mode S with lockout

A Mode S radar acquires an aircraft with all-calls and then locks it out for its II code. The aircraft stops
answering that radar's all-calls, and the radar uses selective roll-calls instead. Each roll-call is
addressed to one aircraft at its own time, so no two aircraft answer the same interrogation and there is
nothing to difference. Roll-call replies (DF4/5/20/21) are therefore useless here. All-call PRFs are also low
(about 50-100 Hz).

Simulated with 90 % lockout, 60 Hz all-call and no Mode A/C interrogations: **0-1 observations in 10 and in
30 minutes** (rows `modes-lockout`, `modes-30min`). The same radars without lockout work (row
`modes-nolockout`, about 8 m). The method needs Mode A/C interrogations or unlocked all-calls. No
algorithmic change can recover pairs that do not exist.

Other ways to fail in the field (not simulated, see §7): the receiver does not decode Mode A/C (readsb
needs `--modeac`); no interrogator within line of sight of both the aircraft and the receiver; very light
traffic.

## 6. Simulation results

`go run ./internal/mlat/ssr/cmd/ssrsim -seeds 10 -scenario all`. The receiver is at 52° N 1° W, 30 m HAE.
There are three conventional SSRs at 60, 140 and 220 km (rotation 4, 8 and 6 s; PRF 220-300 Hz with ±7 %
stagger; beam 2.4°; Mode A/C interleaved), and 150 aircraft in a 300 km region, 85 % of them with ADS-B
(σ 10 m, 2 Hz). 10 % squawk 7000. Transponder offsets are uniform ±0.4 µs (A/C) and ±0.15 µs (S); jitter
50/25 ns. Receiver time-of-arrival σ is 60 ns (Beast class). There are 300 FRUIT/s, reply garbling at the
receiver, a free-running receiver clock (+25 ppm, offset 1234 s), 10 min of data, no position hint, and the
interrogators are learned unless "sites" says otherwise. Ten seeds per row.

| scenario | OK runs | median err_h m | max err_h m | median σ_h m | median err_h/σ_h | radar err m (median) | obs used | rms ns | s/run |
|---|---|---|---|---|---|---|---|---|---|
| ideal | 10/10 | 1 | 3 | 4 | 0.37 | 3 | 2415 | 13 | 1.6 |
| baseline-sites | 10/10 | 5 | 8 | 7 | 0.69 | 6 | 1723 | 82 | 1.7 |
| baseline | 10/10 | 5 | 8 | 7 | 0.62 | 175 | 1723 | 82 | 1.8 |
| rtlsdr | 10/10 | 4 | 15 | 9 | 0.40 | 203 | 1664 | 121 | 1.6 |
| latency | 10/10 | 6 | 10 | 8 | 0.70 | 284 | 1577 | 81 | 1.5 |
| 30min | 10/10 | 2 | 4 | 3 | 0.68 | 89 | 4552 | 84 | 4.2 |
| 3min | 9/10 | 13 | 34 | 25 | 0.44 | 577 | 378 | 78 | 0.4 |
| one-radar | 10/10 | 15 | 25 | 21 | 0.80 | 229 | 854 | 80 | 0.6 |
| light-traffic | 7/10 | 29 | 126 | 30 | 0.72 | 621 | 251 | 83 | 0.4 |
| const-prf | 10/10 | 7 | 16 | 7 | 0.96 | 170 | 1746 | 81 | 1.4 |
| modes-lockout | 0/10 | NaN | NaN | NaN | NaN | NaN | NaN | NaN | 0.3 |
| modes-30min | 0/10 | NaN | NaN | NaN | NaN | NaN | NaN | NaN | 1.0 |
| modes-nolockout | 10/10 | 8 | 17 | 11 | 0.63 | 460 | 554 | 64 | 0.6 |
| sites-1km-off | 10/10 | 7 | 12 | 7 | 0.89 | 976 | 1723 | 85 | 1.2 |
| radar-height-300m | 10/10 | 3 | 8 | 7 | 0.39 | 6 | 1931 | 83 | 1.5 |
| heavy-fruit | 10/10 | 7 | 11 | 11 | 0.70 | 356 | 919 | 78 | 2.4 |

The columns: OK is the number of runs that passed the result gates. err_h is the horizontal error against
truth, and σ_h the estimator's reported 1σ. Radar err is the horizontal error of the learned or refined
interrogator sites. rms is the RMS of the Δ residuals.

Scenario notes. `rtlsdr`: ToA σ 150 ns, configured `timingSigmaACNs` 180. `latency`: 40 % of airframes with
0-0.6 s ADS-B latency. `one-radar`: only the 140 km SSR. `light-traffic`: 40 aircraft. `const-prf`: no stagger.
`heavy-fruit`: 3000 FRUIT/s, so 60 % of heard replies are garbled. `radar-height-300m`: sites configured, but
the true antennas are 300 m higher. `sites-1km-off`: sites configured 1 km wrong while claiming 50 m.

Benchmarks (i9-13900K, one core): `SolveObs` takes 0.26 s for 1,800 observations, 110 airframes and 3 learned
sites (budget 3 s). Ingest runs at 2.6 M replies/s, against a budget of 200 k/s; a busy receiver hears
about 3-5 k Mode A/C/s. Memory is at most 60,000 observations (about 10 MB) plus 120 s of positions.

Tests (`go test ./internal/mlat/ssr/...`):
- unit tests: Gillham code properties, track store, train matching (stagger, alias, two interrogators,
  unrelated trains), rotation recovery, gnomonic projection, beam assignment, configuration and env;
- model-level solves with known and learned sites, and rejection of scrambled geometry;
- simnet end-to-end runs: ideal, baseline learned (≤ 25 m and ≤ 4σ, three sites within 3 km), latency + RTL,
  Mode S without lockout, and the Mode S lockout negative;
- determinism, concurrency and memory bounds, and benchmarks.

## 7. What the simulation does not prove (honest limits)

- **Shared assumptions.** The simulator and the estimator use the same beam/reply model. These things are
  not modelled:
  - antenna sidelobes and SLS failures;
  - reflections, which produce delayed ghost replies (they would mostly become outliers, but are untested);
  - transponder delay varying with signal level;
  - interrogators with synchronised PRFs;
  - monopulse radars that interrogate only part of the dwell;
  - non-uniform antenna rotation beyond the 60 s phase windows;
  - multipath at the receiver;
  - decoder-specific Mode A/C time-of-arrival biases. These are constant per class and cancel in Δ, but
    leading-edge detection on weak, overlapped replies may not be constant.
- **Real radar environment unknown.** How many legacy Mode A/C interrogations, or unlocked all-calls, reach a
  typical AirView site is the decisive unknown. It differs by country and changes as SSRs are replaced. No
  third-party data was downloaded to check it.
- **Mode A/C decoding.** Feeders must enable Mode A/C. AirView's own Beast parser carries type '1' frames,
  but converting the payload to an identity code is a hook (HOOKS.md).
- **No field validation.** No live run was made: this work package has no access to a receiver with Mode A/C
  enabled and a surveyed antenna, and the user's own instance must not be touched.

## 8. Recommendation

1. Keep it **experimental and off by default**. Wire it (HOOKS.md) only as an owner/admin diagnostic:
   "pairs/min" counters first, an on-demand solve second.
2. **Field test.** On a station with a surveyed position, enable Mode A/C, run the estimator for 10-30 min,
   and compare. If `Stats().PairMatches` stays near zero after 10 minutes, the site is in the §5 regime and
   the feature should say so and stop.
3. Use a successful estimate only as a **search centre for the regular survey** (`internal/mlat/survey`),
   never as a position (DESIGN §19.3: AutoUseSurvey requires the survey's own σ_h ≤ 50 m and its suspect
   checks).
4. Possible later work: radar-site sharing between stations of one aggregator (learned sites with rotation
   periods are network-wide facts and not private); sidelobe/reflection outlier models; a field-data
   regression suite.
