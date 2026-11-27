# stat - API notes (from the package author)

import "airview/internal/stat"   (stdlib only; pure functions + small structs; no logging, no globals, no panics, including zero-value and inconsistently deserialised structs)

Changes from the author's reference are marked [CHANGED]. No signature changed.

CONVENTIONS
- NaN = missing. Scalar summaries skip NaN and return NaN if undefined. Paired stats use pairwise-complete (finite) points. Matrix fits (OLS/WLS/Ridge/Logistic) drop rows with any NaN/Inf in X, y, w (or w<=0). Series transforms return len(x) slices and keep NaN. Filters (EWMA, HoltWinters, Kalman) treat NaN as "no measurement".
- [CHANGED] +-Inf policy: everything that fits a model or keeps state (regressions, correlations, Autocorr, Holt-Winters, AR, Kalman, EWMA, Accumulator, SeasonalProfile, Describe, ZScores, SeasonalNaive, forecast metrics) treats +-Inf like NaN. Plain scalar summaries (Sum, Mean, Min, Max, Quantile, Rolling*, ...) follow IEEE and may return Inf.
- Errors (errors.Is): ErrInsufficientData, ErrSingular, ErrDimension, ErrInvalidArgument, ErrDegenerate.
- Angles in degrees, bearings [0,360); wind dir = meteorological FROM; u east, v north. Sample variance n-1 (Pop* = n). Quantiles type 7 (R/NumPy default).
- JSON-safe (never NaN/Inf after success, json tags): Model, HoltWinters, EWMA, Accumulator, SeasonalProfile, LocalLevel, LocalTrend, ARModel, LinearFit, RobustLine, OLSResult, CorrResult, ResponseBin, Summary, CircularStats, WindMean, Metrics, BacktestResult, BacktestComparison, CVResult, ChangePoint. OptResult is JSON-safe except when F = +Inf [CHANGED: now documented; Converged is false then]. NOT JSON-safe: LagCorr.R from CrossCorr (NaN where undefined).
- helpers: NaN() float64; IsFinite(v) bool; const MADScale=1.4826...; HoursPerDay=24; HoursPerWeek=168.

1 DESCRIPTIVE (descriptive.go)
Count(x) int; DropNaN(x) []float64; Sum(x) (0 if empty); Mean; Variance; PopVariance; Std; PopStd; Min; Max -> float64 (NaN if undefined; Variance/Std need n>=2)
MinMax(x) (lo, hi float64, ok bool)
Median(x); Quantile(x, p); Quantiles(x, ps...) []float64; QuantileSorted(sorted, p)  (p outside [0,1] -> NaN; input not modified) [CHANGED: no NaN next to an infinity; median{1,2,+Inf} = 2]
MAD(x) (raw); RobustStd(x)=MADScale*MAD
WeightedMean(x, w); WeightedQuantile(x, w, p)  (w>0 finite only; inverse weighted ECDF, averages at exact ties; equals R type 2 on integer weights)
TrimmedMean(x, frac)  (drops floor(frac*n) per end; frac>=0.5 -> median) [CHANGED: rounding guard, 0.29*100 trims 29]
ZScores(x) []float64 (all NaN if n<2; 0 if std=0) [CHANGED: +-Inf treated as missing]; RobustZScores(x) []float64 ((x-median)/(MADScale*MAD), falls back to mean-abs-dev if MAD=0)
Describe(x) Summary{N, Mean, Std, Min, Q1, Median, Q3, Max}  (zeros when N=0) [CHANGED: finite values only]
type Number (all int/uint/float kinds)
HistQuantile[T Number](counts []T, lo, width, p) float64   bin i=[lo+i*width, lo+(i+1)*width), linear inside bin; p=0 -> lower edge of first occupied bin, p=1 -> upper edge of last; works directly on the []uint32 range_hour dist_hist (lo=0,width=5)
HistQuantileEdges[T](counts []T, edges []float64, p) (len(edges)=len(counts)+1); HistMean[T](counts, lo, width)
Histogram(x, lo, width, bins) (counts []float64, under, over int)

ROLLING / STREAMING (rolling.go)
RollingApply(x, window, minValid, f func(valid []float64) float64) []float64   trailing window, truncated at start; f gets scratch slice of non-NaN values
RollingMean, RollingSum (O(n)); RollingStd, RollingMedian, RollingMin, RollingMax (O(n*w)); RollingMeanCentered(x, window)
Lag(x, k) (out[i]=x[i-k]); Diff(x, lag); FillLinear(x, maxGap) (interior gaps <= maxGap; 0 = any)
type Window (ring buffer): NewWindow(size); Push(v); Reset(); Len(); Valid(); Full(); Values(dst) []float64; Mean(); Std(); Min(); Max(); Median() [CHANGED: zero value no longer panics, behaves like NewWindow(1)]
AlphaFromHalfLife(halfLife) = 1-2^(-1/h)
type EWMA{Alpha, Mean, Var float64; N int64}: NewEWMA(alpha); NewEWMAHalfLife(h); Add(v) float64; AddDT(v, dt) float64 (irregular sampling); Value() (NaN before first); Std() [CHANGED: +-Inf samples ignored like NaN]
EWMASeries(x, alpha) []float64
type Accumulator{N int64; Avg, M2, Lo, Hi, Last float64} (zero value usable, Welford): Add(v); AddAll(x); Merge(b Accumulator); Reset(); Mean(); Sum(); Variance(); PopVariance(); Std(); Min(); Max() [CHANGED: +-Inf samples ignored like NaN]

2 CIRCULAR (circular.go)
NormalizeDeg(a) [0,360) [CHANGED: never -0]; AngleDiffDeg(a, b) = a-b wrapped to (-180,180]; AbsAngleDiffDeg(a, b)
CircularSummary(deg, w []float64 /*nil=equal*/) (CircularStats{N, MeanDeg, R, Variance(=1-R), StdDeg, Defined}, ok)
CircularMeanDeg(deg) (mean, r float64, ok bool) (ok=false if empty or R~0); ResultantLength(deg); CircularVariance(deg); CircularStdDeg(deg)
WindToUV(speed, dirDeg) (u, v) = (-s sin d, -s cos d); UVToWind(u, v) (speed, dirDeg) (calm -> dir 0)
VectorMeanWind(speed, dirDeg []float64) (WindMean{N, Speed, DirDeg, U, V, ScalarSpeed, Steadiness}, ok)

3 ASSOCIATION (assoc.go)
type CorrResult{R float64; N, DF int; T, P float64}  (two-sided p from t dist; |R|=1 -> T=+-1e300, P=0)
Pearson(x, y) (CorrResult, error) (n>=3, df=n-2; ErrDegenerate if constant); Spearman(x, y) (average ranks); Ranks(x) []float64; Covariance(x, y) float64
PartialCorr(x, y, z) (CorrResult, error)  (controls for z; df=n-3, n>=4)
type LagCorr{Lag int; R float64; N int}
CrossCorr(x, y, maxLag) []LagCorr  lags -maxLag..+maxLag; at lag k pairs x[t] with y[t+k] (positive lag = y follows x)
BestLag(x, y, maxLag, minN) (LagCorr, error)  max |R|, ties -> smallest |lag|
Autocorr(x, maxLag) []float64 (r[0]=1; nil if no variance) [CHANGED: +-Inf skipped like NaN]
type BinOptions{Bins int /*10*/; Lo, Hi float64 /*data range unless Lo<Hi*/; EqualCount bool; Quantiles []float64 /*{.1,.5,.9}*/; MinCount int /*1*/}
type ResponseBin{Lo, Hi float64; N int; XMean, YMean, YStd float64; YQ []float64}
BinnedResponse(x, y, opt *BinOptions) ([]ResponseBin, error)  (empty bins omitted)
special.go: RegIncBeta(a, b, x); StudentTCDF(t, df); StudentTTwoSidedP(t, df); StudentTQuantile(p, df); FDistSF(f, d1, d2); NormalCDF(x); NormalQuantile(p); Sigmoid(x); Logit(p)   (verified to ~1e-9 against numerical integration; accuracy degrades to ~1e-7 only for df > ~1e9)

4 REGRESSION (regress.go, model.go)
type LinearFit{Slope, Intercept, R2, SlopeSE, InterceptSE, T, P, ResidStd float64; N int; XMean, SXX float64}
LinearRegression(x, y) (LinearFit, error) (n>=3; ErrSingular if x constant)
  .Predict(x); .PredictInterval(x, level) (yhat, lo, hi) (new observation, t dist); .ConfidenceInterval(x, level) (mean response)   [invalid level -> 0.95 everywhere]
type RobustLine{Slope, Intercept float64; N int; ResidStd float64}; TheilSen(x, y) (RobustLine, error) (all pairs up to n=700, else 250k deterministic sampled pairs); .Predict(x)
type OLSResult{Coef []float64; Intercept bool; SE, T, P []float64; R2, AdjR2, ResidStd, SSE, SST, F, FP float64; N, DF int; Cov []float64 /*unscaled (X'X)^-1*/}   (index 0 = intercept when Intercept)
OLS(X [][]float64 /*rows=obs*/, y, intercept bool) (*OLSResult, error)  Householder QR; ErrInsufficientData if rows<=coefs; ErrSingular if collinear
WLS(X, y, w, intercept) (*OLSResult, error)  minimises sum w*e^2 (precision weights)
  .NumFeatures(); .Predict(x []float64) float64; .PredictInterval(x, level) (yhat, lo, hi) [CHANGED: NaN bounds instead of a panic when Cov is missing or short]
type Model{Kind string /*KindLinear|KindLogistic*/; Features []string; Mean, Scale, Coef []float64; Intercept, Lambda float64; N int; DF, ResidStd, R2, LogLoss float64; Iter int; Converged bool; Cov []float64 /*(p+1)x(p+1) cov of (Intercept, Coef), omitempty*/}
  eta = Intercept + sum Coef[j]*(x[j]-Mean[j])/Scale[j]
  .Predict(x) (linear: eta; logistic: probability; NaN features imputed with training mean; wrong length -> NaN); .Linear(x); .PredictMap(map[string]float64); .PredictInterval(x, level) (pred, lo, hi) (linear: prediction interval; logistic: CI of probability); .RawCoef() (coef []float64, intercept float64) in original units; .NumFeatures(); .Validate() error
ParseModel(json []byte) (*Model, error)   (json.Marshal(model) to save)
Ridge(X, y, names []string /*nil -> x0..*/, lambda) (*Model, error)  minimises (1/n)*SSE + lambda*|b|^2 on standardised features (population sd), unpenalised intercept, Householder QR of the augmented design; lambda=0 = OLS; constant features get coef 0; DF = n-1-effective df
RidgeWeighted(X, y, w, names, lambda)  (relative row weights, e.g. exponential forgetting) [doc: Cov assumes precision weights; coefficient uncertainty optimistic by ~n/n_eff under strong forgetting]
KFold(n, k) [][]int (interleaved); BlockFold(n, k) [][]int (contiguous blocks = leave-one-block-out for time series); LogSpace(lo, hi, n) []float64
type CVResult{Lambdas, MSE []float64 /*-1 = no fit*/; Best, BestMSE float64; N int}
RidgeCV(X, y, names, lambdas, folds [][]int) (*Model /*refit on all rows with best lambda*/, CVResult, error)   (each fold standardised and fitted on its training rows only)
CrossValidate(n, folds, fit func(train []int) (predict func(i int) float64, err error), loss func(i int, pred float64) float64) (meanLoss float64, error)
type LogisticOptions{Lambda float64; MaxIter int /*50*/; Tol float64 /*1e-8*/; Weights []float64}
Logistic(X, y /*in [0,1]*/, names, opt *LogisticOptions) (*Model, error)  IRLS/Newton with step halving; minimises mean log-loss + Lambda/2*|b|^2; ErrDegenerate if one class; Converged=false on separable data with Lambda=0

5 TIME SERIES
seasonal.go
HourOfDay(unixSec int64, utcOffsetSec int) int (0..23); HourOfWeek(unixSec, utcOffsetSec) int (0..167, 0 = Monday 00:00 local)
type ProfileOptions{HalfLife float64 /*samples; 0 = none*/; Mean bool /*default weighted median*/}
type SeasonalProfile{Period int; Level, Spread, Weight []float64; Count []int}  (slot defined when Weight>0)
NewSeasonalProfile(period); FitSeasonalProfile(y, startSlot, period, opt) (*SeasonalProfile, error) (y[i] in slot (startSlot+i) mod period); FitSeasonalProfileSlots(y, slots []int, period, opt)
  .At(slot) (v, ok); .Update(slot, v, alpha) (online forgetting) [CHANGED: no-op instead of a panic on inconsistent slices]; .Filled() ([]float64, ok) (gaps interpolated around the cycle); .Forecast(startSlot, h) []float64; .Deseasonalize(y, startSlot) []float64
SeasonalNaive(y, period, h) ([]float64, error)  (last finite value at same phase, else last finite value) [CHANGED: skips +-Inf]
smoothing.go
type HWParams{Alpha, Beta, Gamma, Phi float64}
type HoltWinters{HWParams; Period int; HasTrend bool; Level, Trend float64; Season []float64; N, NObs int; Sigma float64}
  recursion with e = y-(Level+Phi*Trend+Season[phase]): Level+=Phi*Trend+Alpha*e; Trend=Phi*Trend+Alpha*Beta*e; Season[phase]+=Gamma*(1-Alpha)*e; NaN/Inf y -> e=0
  .Update(y) (pred, resid); .PredictNext(); .Phase() int; .Forecast(h) []float64; .ForecastStd(h) []float64 (grows with h: Sigma*sqrt(1+sum c_j^2)); .ForecastInterval(h, level) (mean, lo, hi []float64) [CHANGED: negative N cannot index out of range]
  Season[i] applies to sample index i mod Period, index 0 = first fitted sample (caller maps to hour-of-week via the slot of y[0])
type HWOptions{NoTrend, Damped bool; Params *HWParams /*fixed, skip optimisation*/; MaxIter int /*300*/}
type HWFit{Model *HoltWinters; Fitted, Residuals []float64; SSE float64; NObs int; Opt OptResult}   (Fitted is in-sample; use Backtest for honest errors)
FitHoltWinters(y, period, opt *HWOptions) (*HWFit, error)  period<=1 = non-seasonal; needs >=2 full seasons and >=3 finite samples; coarse grid then bounded Nelder-Mead on one-step SSE [CHANGED: initial level/trend/season from first <=8 cycles are estimated jointly by backfitting, so gaps in those cycles no longer bias the fit; identical on complete data]
FitHolt(y, opt) = FitHoltWinters(y, 0, opt)   (NoTrend + period 0 = simple exponential smoothing)
ar.go
type ARModel{Coef []float64; Const, Mean, Sigma2 float64; N int}  x[t]=Const+sum Coef[j-1]*x[t-j]
FitARYuleWalker(x, p) (*ARModel, error) (Levinson-Durbin, gap tolerant); FitARLeastSquares(x, p) [CHANGED: +-Inf treated as missing in both; Mean is always finite]
  .Forecast(history, h) []float64; .ForecastStd(h) []float64
AnomalyScores(resid) []float64 (= RobustZScores); RollingAnomalyScores(x, window, minValid) []float64 (vs preceding window, sample itself excluded, capped +-1e6)
type ChangePoint{Index int; Before, After, Shift, Score float64}; DetectChangePoint(x, minSeg) (ChangePoint, ok)  (single mean shift; Score > ~4 significant)
CUSUM(z, k, h) (pos, neg []float64, alarms []int)
kalman.go
type LocalLevel{Q, R, Level, P float64; N int}: NewLocalLevel(q, r); Update(z) float64; Step(z, dt) float64
type LocalTrend{QLevel, QTrend, R, Level, Trend, P00, P01, P11 float64; N int}: NewLocalTrend(qLevel, qTrend, r); Update(z) (level, trend); Step(z, dt) (level, trend); Forecast(h float64) (mean, std)   (Trend = tendency per time step; std grows with h) [CHANGED: Forecast(+Inf) -> NaN, NaN]
KalmanSmoothLevel(y, q, r) []float64; KalmanSmoothTrend(y, qLevel, qTrend, r) (level, trend []float64)  (filter + RTS smoother; NaN bridged; NaN before first valid sample)

6 OPTIMISATION (optimize.go)
type OptResult{X []float64; F float64; Iter, Evals, Restarts int; Converged bool}   (F = +Inf and Converged = false when the objective was NaN/Inf everywhere)
type NMOptions{MaxIter int /*400*n total*/; TolF /*1e-10*/, TolX /*1e-8*/ float64; Step, Lower, Upper []float64; Restarts int /*default 1; negative = none*/}
NelderMead(f func([]float64) float64, x0 []float64, opt *NMOptions) (OptResult, error)  bounds by clamping; NaN objective = +Inf; result returned even if not converged [CHANGED: Converged false when best F is +Inf]
GoldenSection(f func(float64) float64, a, b, tol float64, maxIter int) (OptResult, error)  (X has one element) [CHANGED: Converged false when F is +Inf]
GridSearch(f, lo, hi []float64, steps /*>=2, default 5*/, refine int) (OptResult, error)  coarse-to-fine; max 2e6 evals per pass

7 EVALUATION (eval.go)
MAE, MSE, RMSE, Bias (mean of pred-actual), MAPE (%; skips actual==0), SMAPE (%; 0..200)  : func(actual, pred []float64) float64
type Metrics{N int; MAE, RMSE, Bias, MAPE float64; NPct int; SMAPE, Corr float64}; Evaluate(actual, pred) Metrics (zeros when N=0)
SkillScore(score, baseline) = 1-score/baseline (NaN if baseline 0 and score != 0)
type Forecaster func(history []float64, h int) ([]float64, error)   (history is read-only); SeasonalNaiveForecaster(period) Forecaster
type BacktestOptions{Horizon /*1*/, MinTrain /*len/2*/, Step /*Horizon*/, Window /*0 = expanding*/, MaxOrigins int}
type BacktestResult{Origins []int; Failures int; Overall Metrics; ByHorizon []Metrics}
Backtest(y, f Forecaster, opt *BacktestOptions) (*BacktestResult, error)  rolling origin: f(y[:o:o], Horizon) scored against y[o:o+Horizon] (capacity capped at the origin, so the future is unreachable)
type BacktestComparison{Model, Baseline *BacktestResult; N int; Skill /*RMSE*/, SkillMAE float64; ByHorizon []float64}   (skills over the N points both forecasters scored)
CompareBacktest(y, model, baseline Forecaster, opt) (*BacktestComparison, error)

8 LINEAR ALGEBRA (linalg.go)
type Matrix{Rows, Cols int; Data []float64 /*row-major*/}: NewMatrix(r, c); MatrixFromRows(rows); At(i,j); Set(i,j,v); Row(i) (view); Clone(); T(); Mul(b) (*Matrix, error); MulVec(x) ([]float64, error)
Cholesky(a) (L *Matrix, error); CholeskySolve(l, b); CholeskyInverse(l); SolveSPD(a, b)
Solve(a, b) (Gaussian elimination, partial pivoting) [CHANGED: rows equilibrated, pivot test relative to its column, so independent of the units of equations/unknowns; NaN/Inf in a -> ErrSingular]
type QR: NewQR(a) (*QR, error) (Rows>=Cols); FullRank() bool; Solve(b) ([]float64, error); NormalInverse() (*Matrix /*(A'A)^-1*/, error); LeastSquares(a, b)

TEST FILES
D:\AirView\internal\stat\reference_test.go        independent verification (29 TestRef* tests)
D:\AirView\internal\stat\reference_data_test.go   refData map generated by the Node.js reference program (Simpson integration, normal equations, Newton logistic, textbook Holt-Winters, dense penalised LS for Kalman); data regenerated in Go with the same 32-bit LCG
