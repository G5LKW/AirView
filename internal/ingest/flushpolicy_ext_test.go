package ingest_test

import (
	"airview/internal/ingest"
	"airview/internal/mlat/clock"
	"airview/internal/mlat/group"
)

// The engine's adaptive flush estimator and the clock filter's gate view
// plug into the ingest options as they are.
var (
	_ ingest.FlushPolicy   = (*group.FlushEstimator)(nil)
	_ ingest.HearerNoter   = (*group.FlushEstimator)(nil)
	_ ingest.ClockKindView = clock.GateView{}
)
