// Package trust keeps stations whose timing is not proven out of the MLAT
// engine (H-mlatc; DESIGN §9.6 "trust/quarantine of new stations").
//
// With open admission anyone can connect a station. A station with wrong
// timestamps — broken hardware, a wrong position, or on purpose — takes part
// in every MLAT fix it shares and drags the published quality down
// (measured below: hold-out P95 37 m → 166 m with three hostile stations
// next to fifteen honest ones). New stations therefore start in timing
// quarantine (registry.FlagTimingQuarantine): the Gate, which sits between
// the ingest grouper and the engine, removes their copies from every group
// and judges their timing on the ADS-B position messages they share with
// trusted stations. A station whose timing agrees with the network is
// promoted (Registry.ProveTiming, audited) and joins MLAT; one that does not
// stays out until the admin decides (Registry.SetTiming).
//
// # The test
//
// For a DF17 airborne position heard by a quarantined station Q and a
// reference station R, the aircraft position P is decoded from the message
// itself (CPR local decode around R, cross-checked around Q; barometric
// altitude plus the geoid, or the GNSS height), and
//
//	y = (t_Q − t_R) − (|P − Q| − |P − R|) / c
//
// with t the receivers' ticks in nanoseconds relative to a per-epoch base.
// For honest clocks y is the clocks' offset plus their rate difference times
// time, plus jitter: a line. Per (Q, R) pair the gate collects y in windows
// (at most 64 comparisons or 30 s, at least 8), fits the line robustly
// (Theil–Sen: the median of the pairwise slopes, then the median
// intercept), and takes the residual scale σ = 1.4826 · MAD. A window is
// good when σ ≤ max(1.5 µs, 3·√(jitter_Q² + jitter_R²)) (class priors) and
// at most 40 % of its residuals are gross (> 5× the threshold).
//
// Measured on simnet: honest RTL-SDR pairs σ 300-500 ns, Beast/GPS pairs
// 40-100 ns, all windows good; random timing (±50 µs) σ 30-38 µs and a
// subtle ±4 µs station with a wandering rate σ 2.6-3 µs, all windows bad.
// The robust fit matters: simnet (like real receivers) produces a few
// percent of gross timing errors, which a least-squares fit spreads over
// every residual (a GPS-GPS pair then looked like 1-3 µs).
//
// Position errors show up as a geometry-dependent y that no line fits, so a
// station with a wrong position stays out as well (the survey is the tool
// for that); errors of the aircraft's own position (its position latency,
// the GNSS−baro difference) are common to many windows and stay well below
// the threshold.
//
// # Promotion
//
// A quarantined station is proven when it has Config.GoodWindows (8) good
// windows against at least Config.MinRefs (2) references with more good
// than bad windows, at most Config.MaxBadFrac (20 %) of the trusted
// references it was compared with show as many bad windows as good ones,
// and its first comparison is Config.MinAge (3 min) old. Simnet: honest
// new stations are proven after about 3 minutes, hostile ones never.
//
// Bootstrap: a new network has no trusted station. While fewer than
// MinRefs trusted, located, timed stations were active in the last
// RefActiveFor (2 min), quarantined stations are judged against each other
// as well; only trusted references can mark a station inconsistent, so a
// hostile station cannot keep honest ones out during the bootstrap.
// Colluding hostile stations (consistent with each other but not with the
// truth) could prove each other only in the bootstrap phase of an empty
// network; the admin's overrides and the survey remain the answer there.
//
// The bootstrap also applies locally (finding SCALE-06: a second regional
// cluster with no aircraft in common with the trusted region was never
// judged, so never promoted): a quarantined station that has been judged
// for RefActiveFor without sharing one position message with a trusted
// station is "isolated" and is compared with the quarantined stations it
// hears with, under the same rule (only trusted references can mark it
// inconsistent). Its Report says NoTrustedRef ("no trusted reference in
// range"). A quarantined station that does hear trusted stations is judged
// against them only.
//
// # Cost
//
// The hot path (a group without quarantined copies) is one atomic load and
// one table read per copy: 22 ns per 5-copy group, 0 allocations
// (BenchmarkGateTrustedGroup). Groups with quarantined copies are copied
// once (30 ns). Judging decodes one message per DF17 group with a
// quarantined copy and fits a 64-sample window in about 50 µs; with a few
// hundred stations in quarantine at once that stays within a few percent of
// one core.
//
// # Wiring
//
// See WIRING.md: the engine wraps the MLAT sink (ingest.Options.MLAT =
// trust.New(eng, trust.RegistrySource{R: reg}, cfg, trust.RegistryPromoter(reg, log))),
// passes the gate to the mlat-client server (mlatc.Options.Trust) and
// exposes Gate.Status / Stats and Registry.SetTiming to the owner and
// admin views.
package trust
