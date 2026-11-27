# feed - API notes (from the package author)

PACKAGE airview/internal/feed (imports model and hub)

Manager (all methods concurrency-safe):
  m := feed.New(sink model.FrameSink)                 // or NewWithOptions(sink, feed.Options{DialTimeout 10s, IdleTimeout 60s, MinBackoff 1s, MaxBackoff 30s, JSONInterval 1s, Logger})
  m.Start(ctx)                                        // optional: ties lifetime to ctx (cancel == Close); non-blocking
  m.Apply(feeds []model.Feed)                         // live reconcile: start new/enabled, stop removed/disabled, restart changed; unchanged feeds untouched (counters kept). Feeds with empty/duplicate ID ignored. No-op after Close.
  m.Status() []model.FeedStatus                       // config order; Connected, Since, Frames, Bytes, MsgRate (EWMA, 1 Hz tick, alpha 0.3), LastFrame, LastError, GPSTime, Info (format, Radarcape status text, receiver GPS position / hub host / JSON dialect)
  m.RegisterType(typ string, f feed.Factory)          // e.g. model.FeedDemo; already-configured feeds of that type are (re)started; nil removes
  m.SetHubOptions(feed.HubOptions{Password, KeepHostFeedIDs})   // for type "hub" feeds (use Config.Remote.Password); change reconnects. Default: relayed frames get the local hub feed's ID; KeepHostFeedIDs keeps the host's feed IDs.
  m.OnStation(func(feedID string, st hub.Station))    // host receiver/antennas/feeds from a hub feed, after each login and on change
  m.OnReceiverPosition(func(feedID string, pos feed.ReceiverPosition{Lat, Lon, AltM}))  // Radarcape Beast type '5' frames
  m.Close()                                           // stops everything, waits; idempotent
Built-in types: "beast" (default port 10003), "avr" (30002), "sbs" (30003), "json", "hub" (30154). beast/avr sniff the first bytes of each connection and follow the actual format (Radarcape 10002 can be either).
Reconnect: exponential backoff 1s..30s with 50-100% jitter, reset after data arrived; TCP keepalive 30 s; read deadline = IdleTimeout ("no data for 1m0s" -> reconnect).

Plug-in types:
  type Factory func(cfg model.Feed) (Runner, error)
  type Runner interface { Run(ctx context.Context, s *feed.Session) error }   // RunnerFunc adapter; a Runner that returns/panics is restarted with backoff
  *Session implements model.FrameSink (fills FeedID/Recv/Msgs, applies MLATOnly, counts) plus: Feed(), SetConnected(addr), SetDisconnected(err), SetGPSTime(bool), SetInfo(string), AddBytes(n), ReportPosition(pos).
  Example: m.RegisterType(model.FeedDemo, func(cfg model.Feed)(feed.Runner,error){ return feed.RunnerFunc(func(ctx context.Context, s *feed.Session) error { s.SetConnected("simulator"); /* s.HandleFrame(...) */ <-ctx.Done(); return ctx.Err() }), nil })

RawFrame semantics produced: Data is a freshly allocated exact-size slice per frame (sink may retain it forever; the only per-frame allocation). Channel always -1 for beast/avr/sbs. Signal = 20*log10(byte/255) dBFS, NaN when byte==0/absent. GPSNs = ns since UTC midnight when GPS mode is known (status frame authoritative; else statistical detection over >=5 s and >=16 frames), else -1; Clock12MHz set in 12 MHz mode; both unset until the mode is known and for zero timestamps. MLAT=true for timestamps 0xFF004D4C4154..+10 or Feed.MLATOnly. Radarcape status frames are delivered as Kind=model.FrameStatus with the 14-byte payload in Data (Signal NaN): decode with feed.ParseBeastStatus(f.Data).
DecodedUpdate semantics: ICAO may carry feed.NonICAOFlag (1<<24) for "~" addresses; Msgs = new messages since last poll (JSON) or 1; JSON emits an update only for aircraft with new messages; stale positions (older than interval+1 s) are omitted.

Low-level building blocks (usable by internal/sim and tests):
  NewBeastParser(emit func(*BeastFrame)) *BeastParser; (*BeastParser).Write([]byte), Reset(); counters Frames, Skipped, Garbage, Truncated. BeastFrame{Type, Timestamp, Signal, Payload} valid only during callback. Zero-alloc.
  AppendBeastFrame(dst []byte, typ byte, ts uint64, sig byte, payload []byte) []byte   // encoder with escaping; typ constants BeastModeAC '1', BeastModeSShort '2', BeastModeSLong '3', BeastStatusType '4', BeastPosition '5'
  GPSTimestamp(secOfDay, nanos int) uint64; IsMLATTimestamp(ts) bool; BeastSignalDBFS(b byte) float64
  ParseBeastStatus(data []byte) (BeastStatus, bool): fields Settings, PPSOffset, GPS, BinaryOutput, DF17Only, AVRTimestamps, GPSTimestamps, FlowControlOff, FECDisabled, ModeAC, AntennaOK, Tracking, Sats3, UTC, FPGASync, TimestampBad, TrueUTCRange, GPSDetail; methods GPSLocked(), PPSOffsetNs(), String()
  ParseBeastPosition(p []byte) (ReceiverPosition, bool)
  Timestamper{}: SetStatus(BeastStatus), Observe(ts, recv) bool, Convert(ts) (gpsNs int64, clock uint64), Mode() TimestampMode (TimestampUnknown/Timestamp12MHz/TimestampGPS)
  BeastDecoder{Sink, FeedID, MLATOnly, Recv, OnStatus, OnPosition, OnMode, TS}.Handle(*BeastFrame)
  NewAVRParser(emit func(*AVRFrame)); AVRDecoder{Sink, FeedID, MLATOnly, Recv, OnMode, TS}.Handle
  ParseSBSLine(line []byte) (model.DecodedUpdate, bool)  // MSG,1..8, MSG,10 = Radarcape MLAT, MSG,9 FLARM rejected; SBSDecoder{Sink, FeedID, MLATOnly, Recv}.Write([]byte)
  JSONDecoder{Sink, FeedID, MLATOnly, Interval}.Decode(doc []byte, now time.Time) error; .Format is JSONReadsb or JSONRadarcape
  JSON feed addressing: Feed.URL naming a .json document is used as is; a base URL or Host[/Port] probes /aircraftlist.json, /data/aircraft.json, /tar1090/data/aircraft.json, /skyaware/data/aircraft.json, /dump1090-fa/data/aircraft.json, /dump1090/data/aircraft.json.

PACKAGE airview/internal/hub (imports model only). Wire protocol v1 is specified in hub/doc.go (text handshake "AIRVIEW-HUB 1 <hmac-sha256|open> <nonce>" / "AUTH <client-nonce> <HMAC-SHA256(password, server-nonce)>" / "OK <server-proof>" or "ERR auth|busy|throttled|proto ..."; then uint16-length-prefixed records: 1 Station JSON, 2 FeedDef, 3 Frame, 4 Update, 5 Heartbeat).

Server:
  s := hub.NewServer(hub.Options{Port (0=30154, -1=ephemeral), BindAddr, Password (""=open), MaxClients 8, LoginTimeout 10s, HeartbeatInterval 5s, WriteTimeout 10s, ClientBuffer 4 MiB, FlushInterval 10ms, FailLimit 5, Logger})
  err := s.Start(ctx)                 // binds; one-shot (to change port/enable state create a new Server); stops on ctx cancel or s.Close()
  s.Broadcast(f model.RawFrame); s.BroadcastUpdate(u model.DecodedUpdate)   // never block, do not retain f.Data, zero-alloc and lock-free when no client is connected; also HandleFrame/HandleUpdate so *Server is a model.FrameSink
  s.SetStation(rx model.Receiver, antennas []model.Antenna, feeds []hub.StationFeed)   // hub.StationFeeds(cfg.Feeds) builds the list; pushed to clients at login and on change
  s.SetPassword(pw)                   // live; disconnects logged-in clients when changed
  s.Status() model.HubStatus; s.Clients() []hub.ClientInfo; s.DroppedClients() uint64; s.Addr() net.Addr; s.Close() error
  Slow clients: dropped when their backlog exceeds ClientBuffer or a write exceeds WriteTimeout. Failed logins per IP: after FailLimit failures, refused for 1 s doubling to 60 s.
Client:
  c := hub.NewClient(hub.ClientOptions{Host, Port, Password, FeedID (override FeedID of delivered data; ""=keep host IDs), OnStation func(hub.Station), OnState func(connected bool, addr string, err error), OnBytes func(int), DialTimeout, IdleTimeout 60s, MinBackoff 1s, MaxBackoff 30s, Logger})
  c := hub.Dial(host, port, password) // shorthand without callbacks
  err := c.Run(ctx, sink model.FrameSink)   // blocks; reconnects with backoff+jitter until ctx is cancelled; frames keep original GPSNs/Clock12MHz/Signal/Channel/MLAT; Recv = host receive time translated to the local clock via heartbeats
  c.Connected(), c.Station() (hub.Station, bool), c.Counters(), c.Invalid(), c.Addr()
  hub.Station{App, Version, Proto, Receiver model.Receiver, Antennas []model.Antenna, Feeds []hub.StationFeed{ID, Name, Type, AntennaID, MLATOnly}}
  Errors (errors.Is): hub.ErrAuth, ErrBusy, ErrThrottled, ErrProtocol. Constants: hub.DefaultPort = 30154, hub.ProtocolVersion = 1.

Engine wiring sketch: fm := feed.New(tracker); fm.Start(ctx); fm.SetHubOptions(feed.HubOptions{Password: cfg.Remote.Password}); fm.OnStation(adoptHostSettings); fm.Apply(cfg.Feeds). Host side: tee the tracker sink into hubServer.Broadcast / BroadcastUpdate and call hubServer.SetStation(cfg.Receiver, cfg.Antennas, hub.StationFeeds(cfg.Feeds)) on every config change.

Network additions (WP-F, DESIGN §4.3 / §5.1; full contract and examples in internal/feed/WIRING.md). Standalone behaviour and the legacy GPSNs / Clock12MHz fields are unchanged.
  Beast extension messages: (*BeastParser).OnEscape func(typ byte, body []byte) (body aliases the parser buffer; nil for an unknown type, which is skipped and counted in Unknown, never a stream failure); counter Unknown next to Frames/Skipped/Garbage/Truncated.
  Escape types: BeastReceiverID 0xE3, BeastUUID 0xE4, BeastPing 'P', BeastOption 'W'; BeastUUIDLen 36; options BeastOptPing 'O', BeastOptSlowDown 'S'. IsBeastEscapeType(typ) bool.
  Encoders: AppendBeastEscape(dst, typ, body), AppendBeastPing(dst, v uint32), AppendBeastOption(dst, opt), AppendBeastUUID(dst, uuid), AppendBeastReceiverID(dst, id uint64).
  Parsers: ParseBeastPing(body) (uint32, bool), ParseBeastReceiverID(body) (uint64, bool), ParseBeastUUID(body) (string, bool), NormalizeUUID(s) (string, bool). Own pings: BeastPingValue(t) uint32, BeastPingRTT(v, now) (time.Duration, bool) (24-bit ms counter).
  RawFrame network fields filled by Beast and AVR feeds: HostNs (host UTC receive time), ClockCls (model.ClockClass; ClockNone = untimed), Epoch (never 0 when timed), Ticks (unwrapped receiver time in ClockCls.NominalHz() units: GPS classes = day*86400e9 + ns of day, 12 MHz = 48-bit unwrap).
  ClockTracker (one per receiver clock): Class, Epoch, Running, LastTicks, SetEpochBase(prev), SetClass(cls, detail), ModeChange(detail), SampleDrop(lost), Restart(), Stamp(raw, hostNs) (ticks, ok); tunables DefaultBackwardsTolerance (100 ms), DefaultStepConfirmFrames (3); a step needs 3 agreeing frames, so a single bad timestamp never costs an epoch (Glitches counter).
  ClockEvent{OldEpoch, NewEpoch, Reason model.ClockEventReason, Detail, OldClass, NewClass, StepNs, SamplesLost, HostNs}; Detail is one of ClockDetail* (start, reconnect, step, backwards, gps_lost, gps_regained, time_base, day_range, class_change, override, sample_drop). Delivered before the first frame of the new epoch.
  Timestamper additions: OnClockEvent, Class() (effective), DetectedClass(), SetClassOverride(cls, set), ClassOverride(), SetEpochBase, Epoch, LastTicks, Clock() *ClockTracker, Stamp(ts, hostNs) (cls, epoch, ticks, ok). GPSRegainStatuses = 3 good Radarcape status frames to regain a GPS class; OverrideApplies(override, detected) bool (Feed.ClockClass applies only with the same tick unit, or none).
  Manager: OnClockEvent(func(feedID string, ev ClockEvent)), Clock(feedID) (ClockState{Class, Detected, Epoch, Events, Last, Escapes, UnknownEscapes}, bool), Forwardable(feedID) bool (configured, not MLATOnly, not NoForward, not demo, not hub; same rule as model.Feed.Forwardable). ForwardableFrame(*RawFrame) (ok, timed bool): MLAT results and status frames never.
  Session: Clock() ClockState, Forwardable() bool, ReportClockEvent(ev) for custom runners that produce timed frames.
