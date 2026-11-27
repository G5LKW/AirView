package mlatc

import "airview/internal/mlat/plan"

// The MLAT planner drives mlat-client stations through the server.
var _ plan.SendingController = (*Server)(nil)
