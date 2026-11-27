package simnet

import (
	"bufio"
	"encoding/json"
	"io"
	"math"

	"airview/internal/model"
	"airview/internal/sim"
)

// ReceiverTruth is the true and the declared description of a receiver.
type ReceiverTruth struct {
	Index  int
	ID     string
	UUID   string
	Name   string
	RxNo   int
	Device Device
	Class  model.ClockClass // nominal class
	// True antenna position (height above the ellipsoid) and its ECEF.
	Lat, Lon, HAE float64
	Pos           Vec3
	// Declared position as the owner entered it; DeclaredAlt is relative
	// to DeclaredAltRef ("hae" or "msl").
	DeclaredLat, DeclaredLon, DeclaredAlt float64
	DeclaredAltRef                        string
	// DeclaredErrorM is the horizontal distance between the declared and
	// the true position.
	DeclaredErrorM float64
}

// Receivers returns the truth of every receiver.
func (n *Network) Receivers() []ReceiverTruth {
	out := make([]ReceiverTruth, len(n.rx))
	for i := range n.rx {
		out[i] = n.Receiver(i)
	}
	return out
}

// Receiver returns the truth of receiver i.
func (n *Network) Receiver(i int) ReceiverTruth {
	r := n.rx[i]
	s := r.spec
	return ReceiverTruth{
		Index: i, ID: s.ID, UUID: s.UUID, Name: s.Name, RxNo: s.RxNo,
		Device: s.Clock.Device, Class: s.Clock.Class,
		Lat: s.Lat, Lon: s.Lon, HAE: s.GroundHAE + s.AntennaAGL, Pos: r.pos,
		DeclaredLat: s.declaredLat, DeclaredLon: s.declaredLon, DeclaredAlt: s.declaredAlt,
		DeclaredAltRef: s.DeclaredAltRef,
		DeclaredErrorM: groundDistance(s.Lat, s.Lon, s.declaredLat, s.declaredLon),
	}
}

// ClockAt returns the true clock state of receiver rx at t (seconds since
// Start). ok is false when t is outside the kept history (about 12 s back
// from NowS) or in the future; the nearest state is returned then.
func (n *Network) ClockAt(rx int, t float64) (ClockTruth, bool) {
	return n.rx[rx].clk.truth(t)
}

// ClockEvents returns every epoch change of receiver rx so far. The slice
// must not be modified.
func (n *Network) ClockEvents(rx int) []ClockEvent { return n.rx[rx].clk.events }

// Aircraft returns the truth of every aircraft at NowS.
func (n *Network) Aircraft() []AircraftTruth {
	sn := n.w.latest()
	out := make([]AircraftTruth, len(sn.ac))
	for i := range sn.ac {
		out[i] = n.acTruth(&sn.ac[i], sn.truth[sn.ac[i].truth], sn.tS)
	}
	return out
}

// acTruth builds the truth record of a; rec is the simulator record of the
// snapshot a's state belongs to (an interpolated state carries the truth
// index of its own snapshot, which may differ from the nearest one).
func (n *Network) acTruth(a *acState, rec sim.Truth, t float64) AircraftTruth {
	at := AircraftTruth{ICAO: a.icao, AtS: t, Pos: a.pos, Vel: a.vel, Lat: a.lat, Lon: a.lon, HAE: a.hae, Sim: rec}
	if af := n.w.air[a.icao]; af != nil {
		at.PosLatencyS, at.NACp = af.latencyS, af.nacp
	}
	return at
}

// AircraftAt returns the truth of one aircraft at t (seconds since Start),
// interpolated between the 100 ms world states (exact to well below a metre
// for t within the last ~2 s; extrapolated outside). The Sim record is the
// one of the nearest state.
func (n *Network) AircraftAt(icao uint32, t float64) (AircraftTruth, bool) {
	st, ok := n.w.stateAt(icao, t)
	if !ok {
		return AircraftTruth{}, false
	}
	var best *snapshot
	for i := n.w.nsnap - 1; i >= 0 && i >= n.w.nsnap-snapRing; i-- {
		sn := n.w.snap(i)
		if _, ok := sn.idx[icao]; ok && (best == nil || math.Abs(sn.tS-t) < math.Abs(best.tS-t)) {
			best = sn
		}
	}
	return n.acTruth(&st, best.truth[best.ac[best.idx[icao]].truth], t), true
}

// TOA returns the true time of arrival (seconds since Start) of an
// observation's stamped reference point, i.e. ArrivalS + (RefNs +
// MultipathNs + OutlierNs)·1e-9: the instant the receiver's clock stamped.
func (o *Observation) TOA() float64 {
	return o.ArrivalS + (o.RefNs+o.MultipathNs+o.OutlierNs)*1e-9
}

// TickError returns the reported ticks minus the ticks a noise-free clock
// would have given for the observation (the clock state at TOA), in ticks:
// the white jitter (NoiseNs) plus the quantisation. It is computed without
// the float precision loss of absolute GPS tick values.
func (n *Network) TickError(o *Observation) float64 {
	ct, _ := n.ClockAt(o.Rx, o.TOA())
	if o.Class.GPS() {
		return float64(int64(o.Ticks)-n.startNs) - ct.LocalS*1e9
	}
	return float64(o.Ticks) - ct.Ticks
}

// TruthLog is a Sink that writes a JSON-lines truth log: once per PeriodS
// (default 1 s) one "aircraft" record per aircraft and one "clock" record per
// receiver, every clock "event", and, with Observations, one "obs" record
// per observation. The first line is a "receivers" record.
type TruthLog struct {
	N            *Network
	W            io.Writer
	PeriodS      float64
	Observations bool

	bw      *bufio.Writer
	next    float64
	evIdx   []int
	started bool
	Err     error
}

type truthLine struct {
	Type string  `json:"type"`
	T    float64 `json:"t"`
	Data any     `json:"data"`
}

func (l *TruthLog) emit(typ string, t float64, v any) {
	if l.Err != nil {
		return
	}
	b, err := json.Marshal(truthLine{Type: typ, T: t, Data: v})
	if err != nil {
		l.Err = err
		return
	}
	b = append(b, '\n')
	_, l.Err = l.bw.Write(b)
}

func (l *TruthLog) start() {
	l.started = true
	l.bw = bufio.NewWriterSize(l.W, 64<<10)
	if !(l.PeriodS > 0) {
		l.PeriodS = 1
	}
	l.next = math.Ceil(math.Max(l.N.horizon, 0))
	l.evIdx = make([]int, l.N.NumReceivers())
	l.emit("receivers", 0, l.N.Receivers())
}

// Observe implements Sink.
func (l *TruthLog) Observe(obs []Observation) {
	if !l.started {
		l.start()
	}
	if !l.Observations {
		return
	}
	for i := range obs {
		o := &obs[i]
		l.emit("obs", o.ArrivalS, struct {
			Rx      int     `json:"rx"`
			Seq     uint64  `json:"seq"`
			ICAO    string  `json:"icao"`
			TxS     float64 `json:"txS"`
			RangeM  float64 `json:"rangeM"`
			ExcessM float64 `json:"excessM"`
			MPNs    float64 `json:"multipathNs,omitempty"`
			OutNs   float64 `json:"outlierNs,omitempty"`
			OffNs   float64 `json:"clockOffsetNs"`
			NoiseNs float64 `json:"noiseNs"`
			Epoch   uint32  `json:"epoch"`
			Ticks   uint64  `json:"ticks"`
			HostNs  int64   `json:"hostNs"`
			Msg     []byte  `json:"msg"`
		}{o.Rx, o.Seq, icaoHex(o.ICAO), o.Tx.TxS, o.RangeM, o.ExcessM, o.MultipathNs, o.OutlierNs,
			o.ClockOffsetNs, o.NoiseNs, o.Epoch, o.Ticks, o.HostNs, o.Data})
	}
}

// Advance implements Sink.
func (l *TruthLog) Advance(h float64) {
	if !l.started {
		l.start()
	}
	for i := range l.evIdx {
		ev := l.N.ClockEvents(i)
		for ; l.evIdx[i] < len(ev); l.evIdx[i]++ {
			l.emit("event", ev[l.evIdx[i]].AtS, ev[l.evIdx[i]])
		}
	}
	for l.next <= h {
		t := l.next
		l.next += l.PeriodS
		for _, a := range l.N.Aircraft() {
			if at, ok := l.N.AircraftAt(a.ICAO, t); ok {
				l.emit("aircraft", t, struct {
					ICAO    string  `json:"icao"`
					Lat     float64 `json:"lat"`
					Lon     float64 `json:"lon"`
					HAE     float64 `json:"hae"`
					ECEF    Vec3    `json:"ecef"`
					Vel     Vec3    `json:"vel"`
					Lat0S   float64 `json:"posLatencyS"`
					NACp    uint8   `json:"nacp"`
					ADSB    bool    `json:"adsb"`
					Kind    string  `json:"kind"`
					Call    string  `json:"callsign"`
					AltBaro float64 `json:"altBaroFt"`
				}{icaoHex(a.ICAO), at.Lat, at.Lon, at.HAE, at.Pos, at.Vel, at.PosLatencyS, at.NACp, at.Sim.ADSB, at.Sim.Kind, at.Sim.Callsign, at.Sim.AltBaro})
			}
		}
		for i := 0; i < l.N.NumReceivers(); i++ {
			if ct, ok := l.N.ClockAt(i, t); ok {
				l.emit("clock", t, ct)
			}
		}
	}
	if l.Err == nil {
		l.Err = l.bw.Flush()
	}
}
