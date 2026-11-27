package server

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"

	"airview/internal/privacy/publicdto"
)

// Public live stream deltas (SO-1).
//
// A full public picture is about 600 bytes per aircraft; sent every second
// to every viewer it does not scale (10k aircraft x 1000 viewers would be
// gigabytes per second). A public stream opened with ?delta=1 therefore
// gets one keyframe (event "aircraft": the public snapshot plus "seq") and
// then, once per new picture, an event "delta":
//
//	{"seq":S,"base":S-1,"now":ms,"counts":{...},
//	 "upd":[{"icao":"abc123","lat":52.1,...}, ...],   // new aircraft in full, others only changed members
//	 "del":["abc124", ...]}                           // aircraft that left the picture
//
// A member that disappeared from an aircraft is sent as null. Positions
// and other fast-changing values are quantised (publicQuantise) so that
// small jitter does not make an aircraft "changed". Each delta and each
// keyframe is encoded once and the same bytes go to every delta client; a
// client that missed a frame (full queue) gets a keyframe again. Streams
// without ?delta=1 keep receiving full "aircraft" events (unchanged API).

// deltaEncoder holds the last public picture sent as a delta / keyframe.
// It is used by the live hub goroutine only (and by firstFrames under the
// hub's delta mutex).
type deltaEncoder struct {
	seq  uint64
	gen  uint64 // pubCache generation of the last picture
	prev map[string]map[string]string
	keep publicdto.Snapshot // the picture of seq (for keyframes)

	delta    []byte // encoded delta of seq (nil = none)
	keyframe []byte // encoded keyframe of seq (lazily)

	// Viewport subscriptions (sse_view.go) build their frames from the
	// pieces of the shared encoding instead of encoding aircraft again:
	// raw is every aircraft of seq in full (quantised), frag the aircraft's
	// fragment of the delta of seq (only the changed members; new aircraft
	// in full; absent = unchanged), counts the encoded counts and idx the
	// positions of seq's aircraft.
	raw    map[string][]byte
	frag   map[string][]byte
	counts []byte
	idx    viewIndex
}

// advance moves the encoder to a new public picture of generation gen and
// returns the delta frame (nil when gen is not new). The first picture
// produces no delta (clients start from a keyframe).
func (d *deltaEncoder) advance(snap publicdto.Snapshot, gen uint64) []byte {
	if d.prev != nil && gen == d.gen {
		return nil
	}
	snap = publicQuantise(snap)
	cur := make(map[string]map[string]string, len(snap.Aircraft))
	raw := make(map[string][]byte, len(snap.Aircraft))
	order := make([]string, 0, len(snap.Aircraft))
	for i := range snap.Aircraft {
		m, b := aircraftMembers(&snap.Aircraft[i])
		if m == nil {
			continue
		}
		icao := snap.Aircraft[i].ICAO
		cur[icao] = m
		raw[icao] = b
		order = append(order, icao)
	}
	first := d.prev == nil
	var out []byte
	frag := make(map[string][]byte)
	if !first {
		out = encodeDelta(d.seq+1, d.seq, &snap, order, cur, d.prev, frag)
	}
	d.raw, d.frag = raw, frag
	d.counts, _ = json.Marshal(snap.Counts)
	d.idx.build(&snap)
	d.seq++
	d.gen = gen
	d.prev = cur
	d.keep = snap
	d.delta = out
	d.keyframe = nil
	return out
}

// keyframeFrame returns the keyframe of the current seq (encoded once).
func (d *deltaEncoder) keyframeFrame() []byte {
	if d.prev == nil {
		return nil
	}
	if d.keyframe == nil {
		kf := struct {
			publicdto.Snapshot
			Seq uint64 `json:"seq"`
		}{d.keep, d.seq}
		b, err := marshal(&kf)
		if err != nil {
			return nil
		}
		d.keyframe = sseFrame("aircraft", b)
	}
	return d.keyframe
}

// aircraftMembers splits one aircraft's JSON into its members (raw values)
// and returns the JSON too.
func aircraftMembers(a *publicdto.Aircraft) (map[string]string, []byte) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil, nil
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		m[k] = string(v)
	}
	return m, b
}

// encodeDelta writes the delta event from prev to cur. When frags is not
// nil it receives every aircraft's object of "upd" (sharing the bytes of
// the encoding).
func encodeDelta(seq, base uint64, snap *publicdto.Snapshot, order []string, cur, prev map[string]map[string]string, frags map[string][]byte) []byte {
	type span struct{ from, to int }
	var spans map[string]span
	if frags != nil {
		spans = make(map[string]span)
	}
	var b bytes.Buffer
	b.Grow(64 + 48*len(order))
	b.WriteString(`{"seq":`)
	b.WriteString(strconv.FormatUint(seq, 10))
	b.WriteString(`,"base":`)
	b.WriteString(strconv.FormatUint(base, 10))
	b.WriteString(`,"now":`)
	b.WriteString(strconv.FormatInt(snap.Now, 10))
	if c, err := json.Marshal(snap.Counts); err == nil {
		b.WriteString(`,"counts":`)
		b.Write(c)
	}
	b.WriteString(`,"upd":[`)
	keys := make([]string, 0, 64)
	n := 0
	for _, icao := range order {
		m := cur[icao]
		old := prev[icao]
		keys = keys[:0]
		for k, v := range m {
			if k == "icao" {
				continue
			}
			if ov, ok := old[k]; !ok || ov != v {
				keys = append(keys, k)
			}
		}
		for k := range old {
			if _, ok := m[k]; !ok {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 && old != nil {
			continue
		}
		sort.Strings(keys)
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		from := b.Len()
		b.WriteString(`{"icao":`)
		b.WriteString(m["icao"])
		for _, k := range keys {
			b.WriteString(`,"`)
			b.WriteString(k) // member names are plain identifiers
			b.WriteString(`":`)
			if v, ok := m[k]; ok {
				b.WriteString(v)
			} else {
				b.WriteString("null")
			}
		}
		b.WriteByte('}')
		if spans != nil {
			spans[icao] = span{from, b.Len()}
		}
	}
	b.WriteString(`],"del":[`)
	gone := make([]string, 0)
	for icao := range prev {
		if _, ok := cur[icao]; !ok {
			gone = append(gone, icao)
		}
	}
	sort.Strings(gone)
	for i, icao := range gone {
		if i > 0 {
			b.WriteByte(',')
		}
		q, _ := json.Marshal(icao)
		b.Write(q)
	}
	b.WriteString("]}")
	data := b.Bytes()
	for icao, sp := range spans {
		frags[icao] = data[sp.from:sp.to:sp.to]
	}
	return sseFrame("delta", data)
}

// publicQuantise rounds the fast-changing values of a public picture to
// what a map display needs, so that jitter below that does not count as a
// change: positions to 1e-5° (about 1 m), altitudes to 25 ft, speeds and
// rates to 1 unit, angles to 0.1°, ages to 1 s, lastSeen to 5 s. The input
// is not modified.
func publicQuantise(s publicdto.Snapshot) publicdto.Snapshot {
	out := s
	out.Aircraft = make([]publicdto.Aircraft, len(s.Aircraft))
	copy(out.Aircraft, s.Aircraft)
	for i := range out.Aircraft {
		a := &out.Aircraft[i]
		a.Lat, a.Lon = qp(a.Lat, 1e5), qp(a.Lon, 1e5)
		a.LastRealLat, a.LastRealLon = qp(a.LastRealLat, 1e5), qp(a.LastRealLon, 1e5)
		a.AltBaro, a.AltGeom, a.SelAlt = qStep(a.AltBaro, 25), qStep(a.AltGeom, 25), qStep(a.SelAlt, 25)
		a.VertRate = qStep(a.VertRate, 64)
		a.GS, a.IAS, a.TAS = qp(a.GS, 1), qp(a.IAS, 1), qp(a.TAS, 1)
		a.Track, a.MagHeading, a.SelHeading = qp(a.Track, 10), qp(a.MagHeading, 10), qp(a.SelHeading, 10)
		a.Roll, a.TrackRate = qp(a.Roll, 10), qp(a.TrackRate, 100)
		a.Mach = qp(a.Mach, 1000)
		a.PosAge, a.CoastS = qp(a.PosAge, 1), qp(a.CoastS, 1)
		a.PredSigmaM, a.MLATSigmaM = qStep(a.PredSigmaM, 10), qStep(a.MLATSigmaM, 10)
		a.WindDir, a.WindSpd, a.OAT = qp(a.WindDir, 1), qp(a.WindSpd, 1), qp(a.OAT, 1)
		// "Last seen" moves with every message: 5 s steps are enough for
		// the age shown and the staleness check, and it then changes in
		// one delta out of five instead of every one.
		a.LastSeen -= a.LastSeen % 5000
	}
	return out
}

// qp rounds *v to 1/scale (a new pointer; nil and non-finite stay as they are).
func qp(v *float64, scale float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return v
	}
	r := math.Round(*v*scale) / scale
	return &r
}

// qStep rounds *v to a multiple of step.
func qStep(v *float64, step float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return v
	}
	r := math.Round(*v/step) * step
	return &r
}
