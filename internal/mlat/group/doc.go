// Package group turns the ingest's timed groups into MLAT clusters: sets of
// times of arrival (TOAs) of one transmission at distinct stations
// (docs/aggregator/DESIGN.md §9.3).
//
// The ingest groups copies by payload within a time window (internal/ingest,
// DESIGN §6.2). A group can therefore mix several transmissions with an
// identical payload: DF4/DF5/DF20/DF21 replies to successive interrogations
// (ACAS pairs 0.74 ms apart, several radars, repeated surveillance replies
// whose altitude and flight status do not change), and repeated DF11 or
// identical DF17 frames. Only the station that hears both copies can tell
// them apart by its own clock; copies of different stations can only be told
// apart in network time. This package does that, decides what each cluster
// is good for, fuses co-located stations, keeps late copies, estimates the
// adaptive flush delay and associates Mode A/C replies.
//
// # Data flow
//
//	ingest sweeper ── Observe(*ingest.Group) ──► Assembler ──► Sink.Cluster(*Cluster)
//	               └─ ObserveLate(*ingest.Group) ─┘    ▲   (solver, tracker; internal/mlat)
//	                     Clock (clock snapshot) ───────┤
//	                     Sites (station positions) ────┤
//	                     Hints (tracker / ADS-B ref) ──┤
//	                     Selector (internal/mlat/plan) ┘
//
// # Conversion to network time
//
// Each timed copy (ingest.Copy.Timed, not SameStation, not MapOnly) is
// converted by the Clock to network time t_k with the clock-state variance
// and the per-frame jitter variance, v_k = var_clock,k + var_jitter,k
// (DESIGN §9.2: s_k = τ + ρ_k/c + θ_k + β_short + n_k, solved for τ + ρ_k/c).
// Copies of unsynced stations, or of stations without an MLAT position, are
// dropped. Network times are carried as Time (exact Unix nanoseconds plus a
// fraction): a float64 of Unix nanoseconds resolves only 256 ns.
//
// # Baseline gate (eq. B)
//
// For one transmission from position p at instant τ,
//
//	t_k = τ + |p − r_k|/c + e_k,   so   t_k − t_i = (|p − r_k| − |p − r_i|)/c + (e_k − e_i).
//
// By the triangle inequality | |p − r_k| − |p − r_i| | ≤ |r_k − r_i| = d_ik,
// whatever p is. The true baseline differs from the one computed from the
// stations' MLAT positions by at most the position errors, and refraction
// excess and multipath add up to a few hundred metres at low elevation.
// With e_k − e_i ~ N(0, v_i + v_k − 2 cov_ik) (cov_ik from CrossCov when the
// clock exposes it, else 0), every pair of copies of one transmission
// satisfies, with probability ≥ 1 − 6·10⁻⁵ per pair at n = 4,
//
//	|t_k − t_i| ≤ (d_ik + SlackM + 3(s_i + s_k))/c + n·sqrt(v_i + v_k − 2 cov_ik)      (B)
//
// where s are the stations' position sigmas and SlackM is DESIGN's 1 km.
// Copies of the same station are never compatible.
//
// # Hint gate (eq. H)
//
// With a predicted position p̂ (1-sigma σp, isotropic) the emission instant
// can be estimated per copy, ê_k = t_k − ρ̂_k/c with ρ̂_k = |p̂ − r_k|. For the
// true p = p̂ + δ, a second-order expansion of |p − r_k| about p̂ gives
//
//	|p − r_k| = ρ̂_k + u_kᵀδ + R_k,   |R_k| ≤ |δ|² / (2 (ρ̂_k − |δ|))
//
// (u_k the unit vector from r_k to p̂; the Hessian of the range has norm
// 1/range). Hence
//
//	ê_k − ê_i = ((u_k − u_i)ᵀδ + R_k − R_i)/c + (e_k − e_i)     (τ cancels).
//
// (u_k − u_i)ᵀδ ~ N(0, σp²‖u_k − u_i‖²) is independent of the timing noise,
// so with |δ| ≤ nσp in the remainder bound q = (nσp)²/(2 max(ρ̂ − nσp, 1 km)),
//
//	|ê_k − ê_i| ≤ n·sqrt(σp²‖u_k − u_i‖²/c² + v_ik) + (q_i + q_k + HintSlackM + 3(s_i + s_k))/c   (H)
//
// Two stations looking at the aircraft from similar directions have
// ‖u_k − u_i‖ ≈ 0, so for them the gate is the timing noise plus a few
// hundred metres instead of their whole baseline. A 1 km hint separates
// interrogation replies 0.74 ms apart on every pair; without a hint, pairs
// with baselines over ~220 km cannot separate them at all (d/c > 0.74 ms).
// Both gates are necessary conditions, so a pair is compatible only when (B)
// and (H) hold. Hints with σp > HintMaxSigmaM are ignored. A wrong hint (a
// stale track, the wrong branch of a 3+altitude ambiguity) fragments true
// clusters; when the best hint clique is less than half the best baseline
// clique, the hint is dropped for the group and the cluster is flagged
// HintConflict so the tracker can re-acquire.
//
// # Clustering: maximum consistent sets
//
// Copies are vertices; a pair passing the gates is an edge. Every set of
// copies of one transmission is a clique, so the largest set that can be one
// transmission is a maximum clique. The assembler finds it exactly (Bron–
// Kerbosch with Tomita pivoting and a size bound, on 64-bit sets; ties go to
// the clique with the smallest Σ(|Δt|/gate)²), removes it, and repeats on
// the rest for up to MaxParts clusters per group. DESIGN §9.3 describes a
// greedy alternative: sort, split at gaps over 2 ms, grow from the newest
// copy. The gap split is implied (no edge spans a gap larger than every gate)
// and the greedy growth is order dependent: one copy of the other reply,
// admitted early, can lock out several copies of the right one. The exact
// search does not have this failure mode and costs one branch per level on
// the near-complete graphs of real groups; it is bounded by SearchBudget
// nodes, after which the best clique so far (or the greedy one) is used.
//
// # Co-located stations
//
// Stations closer than ColocatedM (1 km) add almost no geometry; DESIGN
// counts them once for degrees of freedom. Members are grouped into sites by
// single linkage (union-find) and every site is also fused into one
// measurement with inverse-variance weights w_k = v_k⁻¹ / Σ v⁻¹:
//
//	r̄ = Σ w_k r_k,   t̄ = Σ w_k t_k.
//
// Writing r_k = r̄ + δ_k (so Σ w_k δ_k = 0) and expanding the range to the
// aircraft about r̄,
//
//	ρ_k = ρ(r̄) − uᵀδ_k + (|δ_k|² − (uᵀδ_k)²)/(2ρ) + O(|δ|³/ρ²),
//
// the first-order term of Σ w_k ρ_k vanishes exactly because r̄ is the
// centroid with the same weights, so t̄ is the arrival time at r̄ up to the
// bias b ≤ Σ w_k |δ_k|² / (2ρ). Its variance is
//
//	var(t̄) = Σ w_k² v_k + 2 Σ_{k<l} w_k w_l cov_kl + (b/c)²
//
// with ρ bounded below by FuseMinRangeM (20 km: b ≤ 6 m for a 1 km pair).
// The members stay in the cluster with their weights and site numbers, so a
// solver that prefers the full covariance can use them directly.
//
// # Use
//
// A cluster with ≥ MinSites sites (4) is UseSolve; with ≥ MinSitesAlt (3)
// and an altitude (from the frame's AC13 / ES AC12 field, else from Hints)
// UseSolveAlt; with 2 sites UseTrack (DESIGN: "2-station clusters go to the
// tracker only"). A solvable cluster is offered to Selector.Admit, the
// planner's per-aircraft rate limit and global solve budget; when refused it
// is downgraded to UseTrack, not dropped.
//
// # Late copies
//
// Clusters are retained for LateHoldNs in a bounded ring. Copies delivered
// after the flush join the retained cluster they are consistent with (a
// clique search seeded with the retained members), and the grown cluster is
// re-emitted with Late set and the original Seq.
//
// # Flush delay
//
// FlushEstimator gives the per-aircraft flush delay of DESIGN §9.3 (p99
// delivery latency of the usual hearers, clamped to 150-600 ms) for the
// ingest; see docs/aggregator/HOOKS.md for the ingest side.
//
// # Mode A/C
//
// ModeAC associates Mode A/C replies by (code, time) among anchors within
// 150 km (DESIGN §9.9, phase 5, off by default).
//
// # Performance
//
// Observe allocates nothing. See WIRING.md for the measured numbers.
//
// Clean room: written from the maths above and the DESIGN; no code of
// mlat-server or other GPL/AGPL software was consulted.
package group
