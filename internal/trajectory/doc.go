// Package trajectory estimates aircraft state and predicts short-term
// trajectories for the live map.
//
// Each aircraft has an Interacting Multiple Model filter (constant velocity +
// wind-aware coordinated turn, 5-state [x y vx vy omega] in a per-aircraft
// WGS84-scaled tangent plane that is re-centred as the aircraft moves), a
// vertical (altitude, rate) Kalman channel and an along-track speed /
// acceleration channel. Position, velocity-only, altitude, vertical-rate and
// turn-rate (BDS 5,0 track-angle rate, or g*tan(roll)/TAS) measurements are
// gated with a Mahalanobis test; late observations are shifted to the filter
// time with inflated noise; duplicates are dropped.
//
// Predictions continue the turn with an exponentially decaying rate (or until
// the selected heading, or along a detected holding racetrack), propagate the
// air vector under a constant wind, level off at the selected altitude (or a
// plausible ceiling / the ground), never let surface traffic fly, and carry a
// propagated covariance (1-sigma and error ellipse).
//
// Every real position scores the predictions made 5/15/30/60 s earlier,
// against a straight-line and a naive dead-reckoning baseline, so the UI can
// show calibrated, honest skill numbers. See WIRING.md for integration.
package trajectory
