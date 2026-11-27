# weather - API notes (from the package author)

## package weather  (import "airview/internal/weather"; depends only on internal/model)
Units: degC, hPa, km/h, mm, %, metres, snowfall cm; timestamps Unix ms UTC.

type Num float64 — NaN = missing; MarshalJSON -> null, UnmarshalJSON null -> NaN. var NaN. Methods: Valid() bool, Float() float64, Or(def) float64, Ptr() *float64 (nil when missing; for SQL NULL).

type Surface struct{ TempC, RH, DewPointC, ApparentC, PrecipMM, RainMM, ShowersMM, SnowfallCM, WeatherCode, Cloud, CloudLow, CloudMid, CloudHigh, PressureMSL, SurfacePressure, WindKmh, WindDir, GustKmh, VisibilityM, IsDay, CAPE Num }  (json: tempC, rh, dewPointC, apparentC, precipMm, rainMm, showersMm, snowCm, weatherCode, cloud, cloudLow, cloudMid, cloudHigh, pressureMsl, surfacePressure, windKmh, windDir, gustKmh, visibilityM, isDay, cape)
type Current struct{ T int64; IntervalSec int; Surface }  — Time()
type Level struct{ HPa float64 `hpa`; TempC `t`, RH `rh`, HeightM `z` (geopotential m AMSL), WindKmh `ws`, WindDir `wd` Num }
type HourPoint struct{ T int64; Surface; PrecipProb, BoundaryLayerM, LiftedIndex, CIN, FreezingLevelM, ShortwaveWm2, SoilTempC, PWATmm, SnowDepthM, Temp80C, Temp120C, Temp180C, Wind80Kmh, Wind100Kmh(ERA5 only), Wind120Kmh, Wind180Kmh, WindDir80, WindDir100(ERA5 only), WindDir120, WindDir180 Num; Levels []Level }  — Levels ordered ground-up (1000 hPa first), nil for ERA5 hours; Time(); HasProfile() (>=2 levels with T, RH, Z).
type Bundle struct{ Source("open-meteo"), Kind, Model("best_match" default), Attribution, AttributionURL string; Lat, Lon float64; GridLat, GridLon, ElevationM Num; Fetched int64; Cost float64 (weighted API calls); Current *Current (nil for history); Hourly []HourPoint (ascending); LevelsHPa []int }  — immutable once published. Methods: FetchedTime(), Index(t) int, At(t) (HourPoint, bool), Split(now) (past, future []HourPoint).
Kinds: KindForecast, KindHistoricalForecast, KindArchive, KindMixed ("archive+historical-forecast").
Consts: Source, Attribution ("Weather data by Open-Meteo.com (CC BY 4.0)"), AttributionURL, RadarAttribution, RadarAttributionURL. func WeatherCodeText(code int) string (WMO code -> English).
var DefaultLevels = []int{1000,975,950,925,900,850,800,700,600,500,400,300,250,200}.

Client:
 NewClient(Options) *Client — zero Options OK. Options{HTTPClient, UserAgent (default "AirView/<model.Version>"), Timeout (25s), HistoryTimeout (90s), Retries (2; <0 none), Budget, Logger, ForecastURL/HistoricalForecastURL/ArchiveURL/ElevationURL/RainViewerURL overrides}. Safe for concurrent use.
 (c) Forecast(ctx, lat, lon, ForecastOptions) (*Bundle, error) — ONE request: current + hourly + profiles. ForecastOptions{PastDays 0..92, ForecastDays 0..16, NoCurrent, NoLevels, Levels []int, Model string}; DefaultForecastOptions() = {PastDays:2, ForecastDays:7} = 21 current + 109 hourly vars, 13.0 weighted calls. Zero options = current only.
 (c) HistoryEach(ctx, lat, lon, from, to time.Time, HistoryOptions, fn func(*Bundle) error) error — UTC days inclusive, chunked oldest first (default 14 days/chunk), paced. (c) History(...) (*Bundle, error) = collected. HistoryOptions{Source: HistoryAuto|HistoryModel|HistoryReanalysis, NoLevels, Levels, ChunkDays 1..31, Model}. HistoryAuto: model archive where it has data (profiles where available), ERA5 otherwise, per hour; tell hours apart by Levels==nil / HasProfile().
 (c) Elevation(ctx, lat, lon) (float64 metres AMSL, error) — 1 call.
 (c) RadarFrames(ctx) (*Radar, error).
 (c) Usage() Usage{Minute, Hour, Day float64; Budget; ServerRemaining}; (c) UserAgent().
 Errors: ErrBadCoordinate, ErrBudget, *APIError{Status, Reason, RetryAfter}.RateLimited(). Budget{PerMinute, PerHour, PerDay}; DefaultBudget = {300, 2500, 5000} (half the free tier); FreeTierPerMinute=600, PerHour=5000, PerDay=10000, PerMonth=300000.

Radar: type Radar struct{ Fetched, Generated int64; Host string; Past, Nowcast []RadarFrame; TileSize(256), MaxZoom(7) int; Attribution, AttributionURL }; RadarFrame{ T int64; Path string; URL string } — URL is a ready XYZ template "https://tilecache.rainviewer.com/v2/radar/<id>/256/{z}/{x}/{y}/2/1_1.png". (r) Latest() (RadarFrame, bool); (r) TileURL(frame, size 256|512, color, smooth, snow bool) string. Consts RadarMaxZoom=7, RadarTileSize=256, RadarColorScheme=2. Use as a MapLibre raster source with maxzoom 7 (the server answers zoom 8+ with a placeholder image, not an error).

Poller:
 NewPoller(cfg model.WeatherConfig, rx model.Receiver, PollerOptions{Client, Forecast *ForecastOptions, Logger}) *Poller
 (p) Run(ctx) error — blocks until ctx is cancelled, returns nil. First fetch immediately when Enabled && rx.Located (valid coords); then every IntervalMin (default 15, clamped 5..360) +-10% jitter; exponential back-off from 30 s up to the interval on failure, honours 429 Retry-After and ErrBudget. RainViewer index every ~5 min when Enabled && RadarOverlay (independent of location).
 (p) Latest() *Bundle (nil until fetched; read-only); (p) Radar() *Radar; (p) OnUpdate(func(*Bundle)); (p) OnRadar(func(*Radar)) (only on change); (p) Refresh(ctx) (*Bundle, error) — manual fetch, returns the current bundle if <20 s old; errors ErrDisabled, ErrNotLocated, ErrSuperseded, ErrBudget, *APIError; (p) Reconfigure(cfg, rx) live (location change drops the bundle and refetches); (p) Seed(*Bundle) bool — install a persisted forecast bundle at start-up to avoid a request; (p) Status() PollerStatus{Enabled, Located, IntervalMin, LastAttempt, LastOK, LastError, Failures, NextPoll, RadarEnabled, RadarLastOK, RadarError, Usage}; (p) Client() *Client (share it for History/Elevation so the budget is common).
 Example:
   p := weather.NewPoller(cfg.Weather, cfg.Receiver, weather.PollerOptions{})
   p.OnUpdate(func(b *weather.Bundle){ past, fc := b.Split(time.Now()); /* store past -> weather_obs, fc -> weather_fc */ })
   go p.Run(ctx);  elev, err := p.Client().Elevation(ctx, lat, lon)

## package ntp  (import "airview/internal/ntp")
Sign convention: offset = true UTC - host clock; UTC ~= time.Now().Add(offset).
 Query(ctx, server string) (*Response, error); type Client struct{ Timeout (default 3 s); Version (3|4, default 4) }; (c) Query(ctx, server). server = host | host:port | IPv4 | IPv6 | [v6]:port, default port 123. NormalizeServer(s) (string, error).
 type Response struct{ Server, Addr string; Time time.Time (T4, host clock); Offset, Delay time.Duration; Stratum int; RefID string ("GPS"/"PPS" text for stratum 1, dotted IPv4 for 2+); RefIDRaw uint32; Leap, Version, Poll, Precision int; PrecisionDur, RootDelay, RootDispersion, RootDistance time.Duration; ReferenceTime, ReceiveTime, TransmitTime time.Time }
 Errors (errors.Is): ErrNoServer, ErrTimeout, ErrUnsynchronized (LI=3, stratum>=16, root distance>=16 s), ErrInvalidResponse (mode != 4, bad version, zero timestamps, T3<T2), ErrClockStepped, ErrKissOfDeath / *KissError{Code "RATE"|"DENY"|"RSTR"|...}. Replies whose origin timestamp does not echo the request nonce are ignored (keeps waiting until timeout).
 Wire helpers: type Packet (Leap, Version, Mode, Stratum, Poll, Precision, RootDelay, RootDispersion Short, RefID, Reference, Origin, Receive, Transmit Timestamp); (p) Marshal() [48]byte; ParsePacket([]byte) (Packet, error); Timestamp / TimestampFromTime(t) / (ts) Time(pivot) (era-safe past 2036); Short / ShortFromDuration / (s) Duration(); FormatRefID(stratum, id); Now() time.Time (precise wall clock: GetSystemTimePreciseAsFileTime on Windows).
 Sampler (implements model.Clock):
   NewSampler(cfg model.NTPConfig, Options{Burst (default 4, max 8), BurstGap (250 ms), Timeout (1.5 s/query), HistorySize (8192 = ~6 days at 64 s), Logger, Query func override}) *Sampler
   (s) Run(ctx) error — blocks, nil on cancel; idles while !Enabled or Server=="". Every IntervalSec (default 64, clamped 8..3600, +-5% jitter) takes a burst and keeps the minimum-delay reply.
   (s) Offset() time.Duration — median of last 8 samples; 0 when disabled/unknown/older than max(3 intervals, 15 min). (s) Synced() bool — good sample within 3 intervals. (s) Now() time.Time.
   (s) Status() model.NTPStatus; (s) OnSample(func(Sample)); (s) Reconfigure(model.NTPConfig) live (server/enabled change resets sync state, keeps history); (s) SampleNow(ctx) (Sample, error); (s) Latest() (Sample, bool); (s) History(since time.Time) []Sample (oldest first); (s) Seed([]Sample) (preload ring from ntp_samples; never affects Offset/Synced); (s) Stats(window) Stats{Count, SpanSec, Offset Mean/Min/Max/Std ms, Delay Mean/Min/Max ms, DriftPPM *float64, Polls, Failures, Steps}; (s) Config().
   type Sample struct{ T int64 (Unix ms); Server, Addr string; OffsetMs, DelayMs float64; Stratum int; RefID string; Leap int; RootDelayMs, RootDispMs, SmoothedMs, JitterMs float64; Sent, Replies int } — maps to ntp_samples(t, server, offset_ms, delay_ms, stratum). JitterMs = RMS of successive offset differences over the last 16 samples.
   Example: s := ntp.NewSampler(cfg.NTP, ntp.Options{}); s.OnSample(func(x ntp.Sample){ /* store */ }); go s.Run(ctx); var clk model.Clock = s; status.NTP = s.Status()
