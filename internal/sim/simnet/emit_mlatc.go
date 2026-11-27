package simnet

import (
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"

	"airview/internal/model"
)

// MLATClientOptions configures an MLATClientEmitter.
type MLATClientOptions struct {
	// SyncWindowS is the largest gap between the even and odd DF17
	// airborne positions of one aircraft that are paired into a sync
	// message (0 = 1 s).
	SyncWindowS float64
	// SendAll sends an mlat message for every Mode S frame of every
	// aircraft. Otherwise only aircraft in the sending set get them: by
	// default every aircraft this receiver has not heard ADS-B positions
	// from (Mode S only), plus those added with StartSending.
	SendAll bool
	// SeenEveryS is the seen/lost report interval (0 = 15 s); aircraft are
	// lost after LostAfterS without frames (0 = 60 s).
	SeenEveryS float64
	LostAfterS float64
	// RateEveryS is the rate_report interval (0 = 30 s).
	RateEveryS float64
	// HeartbeatS is the heartbeat interval (0 = 30 s).
	HeartbeatS float64
	// Privacy is the handshake privacy flag.
	Privacy bool
	FlushMs float64
}

// MLATClientEmitter produces an mlat-client-like message stream for one
// receiver (protocol v3 shape, uncompressed, one JSON object per line):
//
//	{"version":3,...handshake...}
//	{"input_connected":{}}
//	{"sync":{"et":T,"em":"8d..","ot":T,"om":"8d.."}}   even/odd DF17 position pair
//	{"mlat":{"t":T,"m":"5d..."}}                       one Mode S frame
//	{"seen":["4840d6",...]} / {"lost":[...]}
//	{"rate_report":{"4840d6":2.0,...}}
//	{"clock_reset":{"reason":"sample_drop","epoch":N,"frequency":F,"mode":"..."}}
//	{"heartbeat":{"client_time":S}}
//
// Timestamps T are the receiver's unwrapped ticks of the current epoch (12
// MHz / 20 MHz counter, GPS classes nanoseconds). The exact conventions of
// the real client are fixed from captures by the mlatc ingest work package;
// this stream is shaped so tests can exercise that ingest without a GPL
// client.
type MLATClientEmitter struct {
	emitBase
	opt                        MLATClientOptions
	epoch                      uint32
	started                    bool
	lastPos                    map[uint32]*posPair
	ac                         map[uint32]*mlatcAC
	extra                      map[uint32]bool
	nextSeen, nextRate, nextHB float64
	line                       []byte

	Syncs, MLATs uint64
}

type posPair struct {
	t     [2]uint64
	at    [2]float64
	msg   [2][]byte
	valid [2]bool
}

type mlatcAC struct {
	last     float64
	adsb     bool
	seenSent bool
	posCount int
}

// NewMLATClientEmitter creates the emitter for receiver rx and attaches it.
func (n *Network) NewMLATClientEmitter(rx int, out ByteSink, opt MLATClientOptions) *MLATClientEmitter {
	if !(opt.SyncWindowS > 0) {
		opt.SyncWindowS = 1
	}
	if !(opt.SeenEveryS > 0) {
		opt.SeenEveryS = 15
	}
	if !(opt.LostAfterS > 0) {
		opt.LostAfterS = 60
	}
	if !(opt.RateEveryS > 0) {
		opt.RateEveryS = 30
	}
	if !(opt.HeartbeatS > 0) {
		opt.HeartbeatS = 30
	}
	e := &MLATClientEmitter{opt: opt, lastPos: map[uint32]*posPair{}, ac: map[uint32]*mlatcAC{}, extra: map[uint32]bool{}}
	e.init(n, rx, out, opt.FlushMs)
	h := max(n.horizon, 0)
	e.nextSeen, e.nextRate, e.nextHB = h+opt.SeenEveryS, h+opt.RateEveryS, h+opt.HeartbeatS
	n.Attach(rx, e)
	return e
}

// StartSending adds aircraft to the sending set (server start_sending).
func (e *MLATClientEmitter) StartSending(icaos ...uint32) {
	for _, a := range icaos {
		e.extra[a] = true
	}
}

// StopSending removes aircraft from the sending set (server stop_sending).
func (e *MLATClientEmitter) StopSending(icaos ...uint32) {
	for _, a := range icaos {
		e.extra[a] = false
	}
}

// Handshake returns the handshake object this emitter sends.
func (e *MLATClientEmitter) Handshake() map[string]any {
	sp := &e.n.cfg.Receivers[e.rx]
	return map[string]any{
		"version":              3,
		"client_version":       "airview-simnet",
		"compress":             []string{"none"},
		"selective_traffic":    true,
		"heartbeat":            true,
		"return_results":       true,
		"return_result_format": "ecef",
		"udp_transport":        false,
		"uuid":                 sp.UUID,
		"user":                 sp.Name,
		"lat":                  sp.declaredLat,
		"lon":                  sp.declaredLon,
		"alt":                  sp.declaredAlt,
		"clock_type":           sp.Clock.Device.MLATClientType(),
		"clock_frequency":      sp.Clock.Class.NominalHz(),
		"privacy":              e.opt.Privacy,
	}
}

func (e *MLATClientEmitter) send(h int64, v any) {
	h = e.host(h)
	if !e.up(h) {
		e.Dropped++
		return
	}
	e.begin(h)
	if e.fresh {
		e.fresh = false
		e.started = true
		b, _ := json.Marshal(e.Handshake())
		e.buf = append(e.buf, b...)
		e.buf = append(e.buf, '\n')
		e.buf = append(e.buf, `{"input_connected":{}}`+"\n"...)
		// the server knows nothing about this connection's aircraft yet
		for _, a := range e.ac {
			a.seenSent = false
		}
	}
	b, _ := json.Marshal(v)
	e.buf = append(e.buf, b...)
	e.buf = append(e.buf, '\n')
}

func icaoHex(a uint32) string {
	s := strconv.FormatUint(uint64(a), 16)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

func (e *MLATClientEmitter) hostAt(s float64) int64 {
	cs := &e.n.cfg.Receivers[e.rx].Clock
	return e.n.UnixNs(s) + int64((cs.HostDelayMs+cs.HostOffsetMs)*1e6)
}

func (e *MLATClientEmitter) timeDriven(t float64) {
	for e.nextSeen <= t {
		s := e.nextSeen
		e.nextSeen += e.opt.SeenEveryS
		var seen, lost []string
		keys := make([]uint32, 0, len(e.ac))
		for k := range e.ac {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			a := e.ac[k]
			switch {
			case s-a.last > e.opt.LostAfterS:
				if a.seenSent {
					lost = append(lost, icaoHex(k))
				}
				delete(e.ac, k)
				delete(e.lastPos, k)
			case !a.seenSent:
				a.seenSent = true
				seen = append(seen, icaoHex(k))
			}
		}
		if len(seen) > 0 {
			e.send(e.hostAt(s), map[string]any{"seen": seen})
		}
		if len(lost) > 0 {
			e.send(e.hostAt(s), map[string]any{"lost": lost})
		}
	}
	for e.nextRate <= t {
		s := e.nextRate
		e.nextRate += e.opt.RateEveryS
		rates := map[string]float64{}
		for k, a := range e.ac {
			if a.adsb && a.posCount > 0 {
				rates[icaoHex(k)] = float64(a.posCount) / e.opt.RateEveryS
			}
			a.posCount = 0
		}
		if len(rates) > 0 {
			e.send(e.hostAt(s), map[string]any{"rate_report": rates})
		}
	}
	for e.nextHB <= t {
		s := e.nextHB
		e.nextHB += e.opt.HeartbeatS
		e.send(e.hostAt(s), map[string]any{"heartbeat": map[string]any{"client_time": float64(e.hostAt(s)) / 1e9}})
	}
}

// Observe implements RxSink.
func (e *MLATClientEmitter) Observe(o *Observation) {
	e.timeDriven(o.ArrivalS)
	if e.started && o.Epoch != e.epoch {
		reason := string(model.ClockEvRestart)
		for _, ev := range e.n.ClockEvents(e.rx) {
			if ev.NewEpoch == o.Epoch {
				reason = string(ev.Reason)
			}
		}
		e.send(o.HostNs, map[string]any{"clock_reset": map[string]any{
			"reason": reason, "epoch": o.Epoch, "frequency": o.Class.NominalHz(), "mode": o.Class.String()}})
		clear(e.lastPos)
	}
	e.epoch = o.Epoch
	a := e.ac[o.ICAO]
	if a == nil {
		a = &mlatcAC{}
		e.ac[o.ICAO] = a
	}
	a.last = o.ArrivalS
	d := o.Data
	df := d[0] >> 3
	if df == 17 && len(d) == 14 {
		tc := d[4] >> 3
		if (tc >= 9 && tc <= 18) || (tc >= 20 && tc <= 22) {
			a.adsb = true
			a.posCount++
			odd := int((d[6] >> 2) & 1)
			p := e.lastPos[o.ICAO]
			if p == nil {
				p = &posPair{}
				e.lastPos[o.ICAO] = p
			}
			p.t[odd], p.at[odd], p.valid[odd] = o.Ticks, o.ArrivalS, true
			p.msg[odd] = d
			if p.valid[1-odd] && o.ArrivalS-p.at[1-odd] <= e.opt.SyncWindowS {
				e.send(o.HostNs, map[string]any{"sync": map[string]any{
					"et": p.t[0], "em": hex.EncodeToString(p.msg[0]),
					"ot": p.t[1], "om": hex.EncodeToString(p.msg[1])}})
				e.Syncs++
				p.valid[1-odd] = false // each message pairs once
				p.valid[odd] = false
			}
			return
		}
	}
	switch df {
	case 0, 4, 5, 11, 16, 20, 21:
	default:
		return
	}
	send := e.opt.SendAll
	if !send {
		if v, ok := e.extra[o.ICAO]; ok {
			send = v
		} else {
			send = !a.adsb
		}
	}
	if send {
		e.send(o.HostNs, map[string]any{"mlat": map[string]any{"t": o.Ticks, "m": hex.EncodeToString(d)}})
		e.MLATs++
	}
}

// Advance implements RxSink.
func (e *MLATClientEmitter) Advance(horizonS float64) {
	e.timeDriven(horizonS)
	e.advance(horizonS)
}
