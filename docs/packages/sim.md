# sim - API notes (from the package author)

package sim (import "airview/internal/sim"; depends only on internal/model)

CONFIG
type Config struct {
  Aircraft int               // peak simultaneous aircraft (default 60, max 5000); diurnal curve scales it down to ~20 % at night
  Receiver model.Receiver    // used when Located and |lat| <= 70; else DefaultLat/DefaultLon/DefaultGroundElevM (50.05, 8.60, 110 m)
  Antennas []model.Antenna   // max 4; empty = model.DefaultConfig().Antennas (vertical ch0, yagi ch1); Channel < 0 becomes the index
  FeedID string              // stamped on RawFrame.FeedID (default "demo")
  Seed uint64                // 0 = random; same Seed + Start + config gives the identical frame sequence
  Start time.Time            // virtual UTC start; zero = live (time.Now, virtual clock follows the wall clock at Speed 1)
  Speed float64              // pacing of Run/ServeBeast in virtual s per wall s (default 1; e.g. 600)
  RangeKm float64            // world radius (default 460, clamped 50..1500)
  MLATFraction, GNSSAltFraction, MilitaryFraction float64 // defaults 0.08 / 0.04 / 0.07; negative = none
  EmergencyPerHour float64   // per aircraft and flight hour, default 0.0004; negative = never
  NoDiurnal bool             // constant peak density
  PerfectReception bool      // every transmission received on every antenna at its mean level (decoder tests)
  NoStatusFrames bool        // suppress the once-per-second FrameStatus
  MagVarDeg float64          // 0 = crude estimate from longitude
}
func FromModel(c model.Config, feedID string) Config   // takes receiver, antennas and Demo.Aircraft
func New(cfg Config) *Sim                              // never fails; seeds traffic already in flight

LIFECYCLE (all methods are safe for concurrent use; all outputs of one Sim share one simulation, 100 ms steps)
func (s *Sim) Run(ctx, sink model.FrameSink) error
  Paced by Config.Speed; blocks until ctx is cancelled, then returns nil. Frames arrive in time order from one goroutine. No frame reaches the sink after Run returns. Only HandleFrame is called, never HandleUpdate.
func (s *Sim) Advance(ctx, d time.Duration, sink model.FrameSink) error
  Deterministic, as fast as the CPU allows, ignores the wall clock; sink may be nil; also feeds attached Run sinks and Beast clients. Returns ctx.Err() if cancelled.
func (s *Sim) CatchUp(ctx, sink) error
  Advances until the virtual clock reaches the wall clock, then the sim is live. Use: New with Start in the past, CatchUp to pre-fill statistics, then Run.
func (s *Sim) ServeBeast(ctx, listenAddr string) error
  TCP server, combined stream (BeastOptions{}); blocks until ctx is cancelled (nil) or the listener fails. The simulation runs while it is active.
func (s *Sim) ServeBeastOn(ctx, ln net.Listener, opt BeastOptions) error
  Same on an existing listener (closed on return). Returns an error for an unknown AntennaID.
type BeastOptions struct { AntennaID string; NoMLAT bool; NoPosition bool }
  AntennaID "" = one frame per transmission, taken from the best channel; otherwise only that antenna's frames.
func AppendBeastFrame(dst []byte, f *model.RawFrame) []byte   // encodes any RawFrame as Beast '1'/'2'/'3'/'4'

RawFrame SEMANTICS
  Recv = virtual UTC time of reception (wall clock when live).
  GPSNs = ns since UTC midnight. Signal = dBFS, about -0.5 to -37. Channel = receiving antenna's RF channel. FeedID from Config.
  A transmission heard on two antennas is delivered twice (same Data, different Channel and Signal).
  MLAT results: DF18 CF2 frames (TC18 even + odd position, TC19 velocity) with MLAT=true, GPSNs=-1, Signal=NaN, Channel=-1.
  Status: Kind=FrameStatus, 14 bytes, once per virtual second (byte0 settings 0x11, byte1 PPS offset, byte2 GPS status 0x9F).
  Data is read-only and stays valid indefinitely.

BEAST STREAM
  0x1A, type, body with every 0x1A doubled.
  '2'/'3': 6-byte big-endian timestamp = secondOfDay<<30 | ns, then signal byte = round(255*10^(dBFS/20)) (min 1), then 7 or 14 bytes.
  '4': status every second. '5': receiver position every 10 s (21 bytes, float32 LE lat/lon/alt at offsets 4/8/12).
  MLAT results: timestamp 0xFF004D4C4154, signal 0.

INSPECTION
func (s *Sim) Truth() []Truth                               // at Now()
func (s *Sim) TruthAt(t time.Time) []Truth                  // exact within one step either side of Now
func (s *Sim) TruthOf(icao uint32, t time.Time) (Truth, bool)
  Calling TruthOf(icao, f.Recv) inside HandleFrame returns exactly the state the frame was encoded from.
type Truth struct {
  ICAO uint32; Callsign, Squawk, Category string
  Kind string   // overflight|arrival|departure|ga|helicopter|military
  Phase string
  ADSB, MLAT, GNSSAlt bool; ADSBVersion int; Emergency string
  Lat, Lon, AltBaro, AltGeom float64; OnGround bool; GS, Track, VertRate float64
  TAS, IAS, Mach, TrueHeading, MagHeading, Roll, TrackRate, SelAlt float64
  WindDir, WindKt, OAT float64
  DistKm, Bearing, Elevation, EIRPdBm float64
}
func (s *Sim) Now() time.Time; Seed() uint64; Config() Config; Station() model.Receiver
func (s *Sim) Airports() []Airport     // Airport{Lat, Lon, ElevFt, RunwayHeading}
func (s *Sim) Stats() Stats            // Now, Aircraft, Spawned, Transmitted, Received, BeastClients, ClockSkipped, TrafficDensity, KFactor, QNH, ISADeviation, SurfaceWindDir, SurfaceWindKt
func (s *Sim) TriggerEmergency() (icao uint32, ok bool)     // forces squawk 7700 on one airborne aircraft now

USAGE
  // demo mode
  s := sim.New(sim.FromModel(cfg, feed.ID)); go s.Run(ctx, tracker)
  // end-to-end through the real Beast client
  ln, _ := net.Listen("tcp", "127.0.0.1:0"); go s.ServeBeastOn(ctx, ln, sim.BeastOptions{})
  // deterministic pre-fill or test
  s := sim.New(sim.Config{Seed: 1, Start: t0, Aircraft: 100}); s.Advance(ctx, 6*time.Hour, sink); truth := s.Truth()
