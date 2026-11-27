// Package ssr is AirView's EXPERIMENTAL bistatic SSR self-location
// (DESIGN.md §14 phase 5, §19 decision 4 "research grade"): a single
// 1090 MHz receiver estimates its own antenna position from the timing of
// secondary-radar replies, with no peers, no GPS and no clock
// synchronisation. It is off by default (Config.Enabled), advisory only,
// and never used to locate a station automatically. The feasibility study,
// the simulation results and the open risks are in
// docs/aggregator/SSR-RESEARCH.md.
//
// # Principle
//
// A rotating SSR at R interrogates on 1030 MHz. Every transponder inside
// the beam answers the same interrogation after a fixed reply delay (Mode
// A/C 3 µs, Mode S all-call 128 µs; ICAO Annex 10 Vol IV). When two
// aircraft K and J at known positions (their ADS-B, decoded by the same
// receiver) answer the same interrogation, the receiver U hears them
// Δ = t_J − t_K apart:
//
//	c·Δ = ρ(R, A_J) − ρ(R, A_K) + ρ(U, A_J) − ρ(U, A_K) + c·(β_J − β_K)
//
// ρ is the optical path (geo.Atmosphere), β the per-airframe reply-delay
// offset. The interrogation time and the receiver clock cancel: only times
// less than a millisecond apart are differenced, so a free-running sample
// clock is enough. Each pair is a hyperbola for U with foci A_K, A_J. The
// two aircraft share a beam azimuth, so the radar site enters only through
// (e_J − e_K)·δR with |e_J − e_K| ≲ the beam width (~0.04): a radar site
// known to a few hundred metres costs metres at U.
//
// # Pipeline
//
//  1. Identification (TrackStore): Mode A/C replies are attributed to the
//     unique tracked aircraft whose squawk, or whose barometric altitude in
//     Gillham code (ModeCCode, ±50 ft), equals the reply code; shared and
//     conspicuity codes are dropped. Mode S DF11 replies carry the address
//     and the interrogator identifier (IID).
//  2. Dwells: the replies of one airframe (and IID) form bursts while the
//     beam passes (gap DwellGapS).
//  3. Train matching (MatchTrains): two overlapping dwells answered the same
//     interrogations when their arrival differences cluster to within the
//     reply jitter. A staggered PRF makes the pattern unique; a constant
//     PRF whose alias also fits within the geometric window is rejected.
//     Each cluster is one PairObs (mean Δ over N interrogations).
//  4. Interrogators (LocateInterrogators), unless Config.Sites lists them:
//     every pair lies on one radial of its radar, which is a straight line
//     in a gnomonic projection; a Hough vote over these bearing lines and a
//     robust intersection locate the sites. Each site's rotation is then
//     recovered from the pair times (EstimateRotation): learned sites
//     without a coherent rotation, and "ghost" sites whose pairs fit a
//     busier site, are dropped; observations are assigned by beam geometry
//     and beam phase (Assign).
//  5. Solve (SolveObs): a horizontal seed grid (pseudo-Huber, straight-line
//     ranges), then robust Gauss-Newton on U, every site (prior from the
//     line fit, floored at LearnedSiteMinSigmaM, or from the
//     configuration), the airframe biases β (prior BiasSigma*) and
//     per-airframe position latencies (prior LatencySigmaS, along the
//     ADS-B velocity), with a height prior, Huber weights, 5σ outlier
//     rounds and a covariance scaled by max(1, χ²/dof).
//
// # Use
//
//	e := ssr.New(cfg)                  // cfg.Enabled is the caller's gate
//	e.AddPosition(icao, ssr.Position{...})  // every decoded ADS-B position
//	e.AddReply(ssr.Reply{...})         // every Mode A/C and DF11 reply
//	e.Process(now)                     // about once a second
//	res := e.Solve(ssr.Hint{...})      // when wanted (≈0.3 s for 2k observations)
//	radars := e.LocateRadars(hint)     // interrogators from their beam sweeps (sweep.go)
//
// # Locating the interrogators from their beam sweeps
//
// LocateBySweep (sweep.go) is a second, independent method: the DF11
// all-call bursts of one interrogator code arrive while that radar's beam
// points at the replying aircraft, so their times and the aircraft
// positions give the radar site and rotation (a bearing-only fit). It
// needs no common-interrogation pairs, so it still works where Mode S
// lockout defeats the pair method, as long as some aircraft answer the
// same radar on several scans. See SSR-RESEARCH.md §9.
//
// Everything is in the receiver's own time base. See WIRING.md for the
// integration points and package ssrsim for the ground-truth simulator.
package ssr
