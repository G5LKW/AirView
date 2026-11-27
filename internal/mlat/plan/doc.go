// Package plan is the MLAT planner (docs/aggregator/DESIGN.md §9.1
// "mlat/plan", §4.4 traffic modes, §9.3 candidates): it decides which
// aircraft each station must send in full (its syncSet and mlatSet, carried
// to AVF feeders as INTEREST and to mlat-clients as start/stop_sending),
// which stations are asked for each MLAT aircraft, and how often each
// aircraft is solved.
//
// # Inputs
//
//   - ObserveGroup for every timed group (who hears which aircraft, the
//     aircraft's frame rate, ADS-B positions from DF17/18 results);
//   - NoteHearer for visibility reported without timed groups (mlat-client
//     seen/lost), NoteADSB for the reference track's verdict on ADS-B
//     quality (NACp ≥ 8, NIC ≥ 7), MarkSuspect for ADS-B positions that
//     failed TDOA verification;
//   - Stations (positions, synced, anchor, timing sigma) and optionally
//     Positions (MLAT track / ADS-B estimates) and HoldOut (quality set).
//
// # Priorities (Prio)
//
// An aircraft without a good ADS-B position within ADSBGoodAge (10 s) is an
// MLAT candidate (DESIGN §9.3): PrioAcquire until the tracker has it better
// than TrackedSigmaM, then PrioTracked. ADS-B aircraft are candidates only in
// the hold-out set (PrioHoldOut) or while suspect (PrioSuspect). Non-ADS-B
// aircraft therefore come first everywhere: in station caps, in churn
// budgets and in solve admission.
//
// # Solve admission (Admit, Candidate: group.Selector)
//
// A token bucket per aircraft (4 / 2 / 1 / 0.5 solves per second by class,
// burst 3) and a global bucket (GlobalSolveHz) with per-class reserves
// (0, 10, 30, 50 %): under overload suspects back off first and Mode S-only
// acquisitions last. A refused cluster becomes a TDOA-only tracker update.
//
// # Station plan per MLAT aircraft: greedy D-optimal design
//
// For the unknowns (position p, c·transmit time b) a station j contributes
// the Fisher information h_j h_jᵀ/σ_j² with h_j = [u_jᵀ, 1] (u_j the unit
// vector from the station to the aircraft) and σ_j its timing sigma in
// metres. By the matrix determinant lemma adding station k multiplies
// det J by 1 + h_kᵀ J⁻¹ h_k/σ_k², so the greedy choice maximises that term
// and J⁻¹ is updated by Sherman–Morrison in O(16). log det is submodular, so
// greedy selection is within 1 − 1/e of the best set of each size. Stations
// are added up to StationsPerAircraft (12), always at least
// MinStationsPerAircraft (6; RAIM needs redundancy) when available, and
// beyond that only while the expected horizontal CRLB
// σ_h² = tr(P_pp) − upᵀ P_pp up is above TargetSigmaM. Without a position
// estimate the hearers' centroid lifted to 9 km stands in for the aircraft:
// the geometry ranking is insensitive to tens of km of error.
//
// # syncSet per station: lazy greedy coverage
//
// A sync aircraft heard by station s and by peers j links s to each j (the
// clock filter observes θ_s − θ_j). The value of a set S is
// F(S) = Σ_j w_j·H(c_j(S)), with c_j the number of chosen aircraft shared
// with peer j, H(c) = 1 + 1/2 + … + 1/c the harmonic number (diminishing
// returns per peer), and peer weights 3 (anchor), 2 (synced), 1 (located),
// 0 (unlocated). Aircraft a adds Σ_{j hears a} w_j/(1 + c_j), scaled down for
// aircraft sending under 2 frames/s. H is concave, so F is monotone
// submodular and lazy greedy evaluation (a max heap of stale upper bounds)
// gives exactly the greedy order at a fraction of the cost.
// Current members get a SwapMargin (1.25) bonus so sets do not flap.
// Between SyncSetMin and SyncSetMax (15-30) aircraft are chosen, within half
// of the station's StationFPSBudget.
//
// # mlatSet per station and deltas
//
// The per-aircraft plans give (station, aircraft) requests; each station
// takes them by priority and D-optimal gain up to MLATSetMax and its frame
// budget. Changes are sent as deltas: removals of aircraft the station no
// longer hears are always sent, other removals wait MinDwell (30 s), and at
// most MaxChurn adds plus removals go out per replan (MLAT adds first). The
// first message to a station is a reset carrying its whole sets.
// Removals that compete for the churn budget are taken in address order,
// and stations that heard nothing are emptied in station order, so a
// replan never depends on map iteration (bit-reproducible engine runs,
// H-track).
//
// # Mode A/C (phase 5, off)
//
// ModeACEligible lists the anchors that have at least MinPeers anchor peers
// within MaxBaselineKm (150 km), for group.ModeAC.
//
// Clean room: written from the maths above and the DESIGN; no code of
// mlat-server or other GPL/AGPL software was consulted.
package plan
