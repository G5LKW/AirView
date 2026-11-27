// Package stat is AirView's numerical toolbox: descriptive and circular
// statistics, correlation, regression (OLS, robust, ridge, logistic), time
// series smoothing and forecasting, small optimisers and forecast evaluation.
//
// It depends on the standard library only and consists of pure functions and
// small value-like structs. Nothing in the package logs, blocks or keeps
// global state, and nothing panics on empty or degenerate input.
//
// # Missing values
//
// NaN is the package-wide marker for "missing" (e.g. an hour without data).
// The handling is uniform:
//
//   - Scalar summaries (Mean, Std, Quantile, MAE, ...) skip NaN values and
//     return NaN when the result is undefined (no valid data, too few points).
//   - Paired statistics (Pearson, LinearRegression, MAE(actual, pred), ...)
//     use pairwise-complete observations: an index is used only when every
//     series involved is finite at that index.
//   - Matrix style fits (OLS, WLS, Ridge, Logistic) drop every row that
//     contains a non-finite value in X, y or the weights.
//   - Series transforms (ZScores, RollingMean, Diff, ...) return slices of
//     the input length and keep NaN where the input was missing or where the
//     result is undefined.
//   - Filters and smoothers (EWMA, Holt-Winters, Kalman) treat NaN as a time
//     step without a measurement: the state is propagated but not corrected.
//
// Infinities are not valid data. Everything that fits a model or keeps
// state (regressions, correlations, Holt-Winters, AR, Kalman, EWMA,
// Accumulator, SeasonalProfile, Describe, the forecast metrics) treats +-Inf
// like NaN, so that a stray infinity can neither poison a persisted struct
// nor turn every later result into NaN. The plain scalar summaries (Sum,
// Mean, Min, Max, Quantile, ...) follow IEEE arithmetic instead and may
// return an infinity when the input contains one.
//
// Functions that return structs report failure with an error (one of the
// Err* sentinels, test with errors.Is) or an ok flag instead of NaN.
// Structs that are meant to be persisted (Model, HoltWinters, EWMA,
// Accumulator, SeasonalProfile, LocalLevel, LocalTrend, ARModel) never
// contain NaN after a successful fit and can be passed to encoding/json.
//
// # Conventions
//
// Angles are degrees, bearings clockwise from north in [0, 360). Wind
// direction is the meteorological "from" direction; u is the eastward and v
// the northward component. Sample variance uses n-1; Pop* variants use n.
// Quantiles use linear interpolation between order statistics (type 7, the
// default of R and NumPy).
package stat

import (
	"errors"
	"math"
)

// Sentinel errors. Fit functions wrap or return these directly.
var (
	// ErrInsufficientData: not enough valid observations for the request.
	ErrInsufficientData = errors.New("stat: insufficient data")
	// ErrSingular: the linear system is singular or too ill-conditioned
	// (collinear features, constant regressor, ...).
	ErrSingular = errors.New("stat: singular or ill-conditioned system")
	// ErrDimension: slice lengths or matrix shapes do not match.
	ErrDimension = errors.New("stat: dimension mismatch")
	// ErrInvalidArgument: a parameter is outside its domain.
	ErrInvalidArgument = errors.New("stat: invalid argument")
	// ErrDegenerate: the data carries no information for the request
	// (zero variance, a single class, ...).
	ErrDegenerate = errors.New("stat: degenerate input")
)

// NaN returns the missing-value marker.
func NaN() float64 { return math.NaN() }

// IsFinite reports whether v is neither NaN nor infinite.
func IsFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func sq(v float64) float64 { return v * v }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
