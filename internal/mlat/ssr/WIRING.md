# internal/mlat/ssr: wiring guide (EXPERIMENTAL, off by default)

`import "airview/internal/mlat/ssr"`: stdlib plus `internal/mlat/geo` and `internal/mlat/linalg`. This is bistatic SSR
self-location: one receiver estimates its own position from secondary-radar reply timing. It is research-grade
and advisory only. Nothing in AirView may use its result to set, confirm or replace a station position
automatically. The feasibility report is `docs/aggregator/SSR-RESEARCH.md`. The model is in `doc.go`.

## 1. Feeding it (standalone/feeder receiver, or per station on an aggregator)

```go
cfg := ssr.DefaultConfig()            // Enabled=false
_ = cfg.ApplyEnv(os.LookupEnv)        // AIRVIEW_SSR_* (see ssr.EnvKeys())
cfg.Normalize()
if err := cfg.Validate(); err != nil { /* config check output */ }
if !cfg.Enabled { return }            // do not construct or feed it
e := ssr.New(cfg)
e.SetAtmosphere(currentAtmosphere)    // optional (geo.Atmosphere, k_atm/N0)
```

Use **one time base** for both inputs: the receiver's own timestamp in seconds. The 12 MHz Beast counter divided
by 12e6, unwrapped, works. For GPS-stamped receivers use seconds of day, unwrapped across midnight. The method
only differences replies less than about 1 ms apart, so a free-running clock is fine. Do not use host
wall-clock time, because its jitter is about 1 ms.

| Input | Call | Source |
|---|---|---|
| ADS-B airborne position decoded by this receiver | `e.AddPosition(icao, ssr.Position{T, P: ECEF, SigmaM: from NACp (0 = 20 m), Squawk: latest Mode A or -1, AltBaroFt: latest barometric altitude or NaN})` | tracker / decoder, every position message |
| Mode A/C reply (Beast type `'1'`) | `e.AddReply(ssr.Reply{T, Class: ssr.ClassModeAC, Code: octal identity 0..0o7777})` | `internal/feed` frames of kind `model.FrameModeAC` |
| DF11 all-call reply with a valid address | `e.AddReply(ssr.Reply{T, Class: ssr.ClassModeS, ICAO, IID})` | Mode S decoder. IID is the PI field XOR the CRC (II/SI), 0 when unknown |
| about once a second | `e.Process(now)` | `now` is in the same time base |

`Reply.Code` is the 12-bit identity value, A4A2A1 B4B2B1 C4C2C1 D4D2D1, as an octal number (squawk 7700 →
`0o7700`). The Beast Mode A/C payload stores the pulses in its own bit order, so the decoder that owns
`model.FrameModeAC` must convert it. See HOOKS.md.

Mode A/C must be enabled at the receiver (readsb/dump1090 `--modeac`; Beast DIP/flag "Mode A/C"). Without it,
only DF11 replies are available, and under II lockout those are too few (SSR-RESEARCH.md §5).

## 2. Solving

```go
res := e.Solve(ssr.Hint{Valid: true, Pos: declaredOrLocateMe, RadiusM: 50e3,
	HeightKnown: true, HeightM: demPlusGeoidPlusAGL, HeightSigmaM: 20})
// res.Experimental == true always; res.OK, res.Reason, res.LLH, res.SigmaHM,
// res.CovENU, res.Radars (learned/configured sites with rotation periods)
```

Solving takes about 0.26 s for about 1,800 observations on one core (`BenchmarkSolve`). The cost grows with
the number of airframes: the dense normal matrix holds up to 3 + 3·sites + 2·MaxAircraft (600) parameters.
Run it on demand, or at most once every few minutes, off the hot path. Ingest handles about 2.6 M replies/s
(`BenchmarkIngest`). Memory is bounded by `MaxObs` (60,000 observations, about 10 MB) plus 120 s of positions.

Present the result to the station owner/admin only, labelled "experimental SSR estimate". Never show it on
public maps (DESIGN §8: station positions are private). It may be offered as a **search hint** for the survey
(`survey` "Locate Me" centre), never as a position.

## 3. Configuration (file keys requested in HOOKS.md)

Everything has a default (zero = default). The env names are `AIRVIEW_SSR_<KEY>` (`ssr.EnvKeys()`).

| Key | Default | Meaning |
|---|---|---|
| `enabled` / `ENABLED` | false | Master switch (experimental) |
| `sites` / `SITES` | none | Known interrogators. Env form: `name,lat,lon,heightM;...`. Empty = learn them from the data |
| `dwellGapS` | 0.05 | Silence that ends a reply burst |
| `processLagS` | 2 | Replies are processed this far behind the newest input |
| `maxDwellS` | 0.5 | Maximum burst length |
| `matchTolNs` | 400 | Half width of the train-match window |
| `minMatches` | 4 | Common interrogations per pair dwell |
| `ambiguityRatio` | 0.6 | PRI-alias rejection |
| `maxPairSepM` | 250000 | Maximum pair separation |
| `timingSigmaACNs` / `timingSigmaSNs` | 120 / 60 | Per-reply timing noise (raise `timingSigmaACNs` to about 180 for RTL-SDR) |
| `trackSigmaM`, `floorSigmaM` | 20, 5 | Position noise when not given; error floor |
| `biasSigmaACNs` / `biasSigmaSNs` | 300 / 150 | Reply-delay offset priors |
| `latencySigmaS`, `noLatency` | 0.3, false | Per-airframe ADS-B latency parameters |
| `maxObs`, `maxAircraft` | 60000, 600 | Memory and solve bounds |
| `minObs`, `minAircraft`, `minRadarObs` | 50, 10, 20 | Result gates |
| `radarRegionM`, `radarCellM`, `minLineSepM`, `maxLineSigmaM` | 500 km, 4 km, 15 km, 10 km | Site learning (Hough) |
| `minRadarLines`, `minRadarSpreadDeg`, `maxRadars`, `radarNmsM` | 25, 10, 16, 30 km | Site learning |
| `beamwidthDeg`, `assignMinTolM`, `phaseTolDeg` | 3, 3000, 2 | Beam gating |
| `rotWindowS`, `minPeriodS`, `maxPeriodS`, `minCoherence` | 60, 2, 15, 0.8 | Rotation check |
| `radarHeightM`, `radarHeightSigmaM`, `learnedSiteMinSigmaM` | 60, 300, 1500 | Learned-site priors |
| `searchRadiusM`, `seedMaxObs`, `heightSigmaM` | 300 km, 4000, 1000 | Seed grid and height prior without a hint |
| `huberK`, `outlierZ`, `maxIter`, `maxSigmaHM` | 2.5, 5, 25, 500 | Robust solve and gate |

## 4. Tools

- `go run ./internal/mlat/ssr/cmd/ssrsim -seeds 10 -scenario all [-v]` prints the feasibility table of
  SSR-RESEARCH.md §6.
- `ssrsim.Evaluate(scenario, cfg, hint)` runs one end-to-end simulation against ground truth.
- `e.Stats()` returns the counters: replies, resolved/ambiguous Mode A/C, dwells, pair matches. Run the
  estimator on a live receiver for 10 minutes and read `PairMatches` before trusting a solve. A real site where
  it stays near zero is the negative case in SSR-RESEARCH.md §5.
