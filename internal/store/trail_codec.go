package store

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
)

// Trail chunk codec (DB-EFFICIENCY-DESIGN §3.8).
//
// A sealed trail chunk (table trail_chunk) holds up to trailChunkMax points
// of one flight in time order, lossless: every column is stored exactly as
// the 1.0.0 positions table stored it (integers: lat/lon 1e-5 deg, alt ft,
// gs kt, track deg, rssi 0.1 dB, src code; NULL stays NULL).
//
// Blob layout:
//
//	byte 0      codec: 1 = varint payload, 2 = raw deflate of the payload
//	payload     uvarint n (points)
//	            byte    order mask: bit c set = column c is stored as
//	                    second-order deltas (delta of deltas), else as deltas
//	            col 0   t:   n zig-zag deltas (the first relative to 0)
//	            col 1   lat: n zig-zag deltas
//	            col 2   lon: n zig-zag deltas
//	            col 3-6 alt, gs, track, rssi_x10: NULL map + deltas of the
//	                    non-NULL values. NULL map = uvarint number of runs,
//	                    then the run lengths, alternating present / NULL and
//	                    starting with a (possibly empty) present run
//	            col 7   src: uvarint number of runs, then (zig-zag value,
//	                    uvarint length) per run
//
// Deltas are taken over the non-NULL values of a column only. The encoder
// picks first- or second-order deltas per column, whichever is shorter
// (smooth tracks make lat/lon/alt second-order deltas mostly one byte).

// TrailRow is one stored trail point in its stored (integer) form: the
// columns of the 1.0.0 positions table. The nullable columns are valid when
// their bit is set in Has.
type TrailRow struct {
	FlightID int64
	T        int64
	LatE5    int64
	LonE5    int64
	Alt      int64 // ft
	GS       int64 // kt
	Track    int64 // deg
	RSSIX10  int64 // 0.1 dBFS
	Has      uint8 // TrailHas* bits
	Src      int64
}

// Bits of TrailRow.Has.
const (
	TrailHasAlt uint8 = 1 << iota
	TrailHasGS
	TrailHasTrack
	TrailHasRSSI
)

// Position converts the stored row to the API row type.
func (r TrailRow) Position() Position {
	nan := math.NaN()
	p := Position{FlightID: r.FlightID, T: r.T, Lat: float64(r.LatE5) / 1e5, Lon: float64(r.LonE5) / 1e5,
		Alt: nan, GS: nan, Track: nan, RSSI: nan, Src: uint8(r.Src)}
	if r.Has&TrailHasAlt != 0 {
		p.Alt = float64(r.Alt)
	}
	if r.Has&TrailHasGS != 0 {
		p.GS = float64(r.GS)
	}
	if r.Has&TrailHasTrack != 0 {
		p.Track = float64(r.Track)
	}
	if r.Has&TrailHasRSSI != 0 {
		p.RSSI = float64(r.RSSIX10) / 10
	} else {
		// 1.0.0 scanned a NULL rssi_x10 into NaN and divided by 10: NaN.
		p.RSSI = nan
	}
	return p
}

// Values returns the row as the column values of the 1.0.0 positions table
// (flight_id, t, lat_e5, lon_e5, alt, gs, track, rssi_x10, src), NULL as nil.
func (r TrailRow) Values() []any {
	opt := func(bit uint8, v int64) any {
		if r.Has&bit == 0 {
			return nil
		}
		return v
	}
	return []any{r.FlightID, r.T, r.LatE5, r.LonE5, opt(TrailHasAlt, r.Alt), opt(TrailHasGS, r.GS),
		opt(TrailHasTrack, r.Track), opt(TrailHasRSSI, r.RSSIX10), r.Src}
}

// TrailColumns are the column names of Values (the 1.0.0 positions table).
var TrailColumns = []string{"flight_id", "t", "lat_e5", "lon_e5", "alt", "gs", "track", "rssi_x10", "src"}

// trailRowFromPosition converts a Position to its stored form exactly like
// the 1.0.0 INSERT did (rounding, NaN -> NULL). ok is false for a point
// 1.0.0 would not have stored.
func trailRowFromPosition(p *Position) (TrailRow, bool) {
	if p.FlightID <= 0 || !isNum(p.Lat) || !isNum(p.Lon) || math.Abs(p.Lat) > 90 || math.Abs(p.Lon) > 360 {
		return TrailRow{}, false
	}
	r := TrailRow{FlightID: p.FlightID, T: p.T, LatE5: int64(math.Round(p.Lat * 1e5)),
		LonE5: int64(math.Round(p.Lon * 1e5)), Src: int64(p.Src)}
	set := func(bit uint8, dst *int64, v float64) {
		if x, ok := ni(v).(int64); ok {
			*dst = x
			r.Has |= bit
		}
	}
	set(TrailHasAlt, &r.Alt, p.Alt)
	set(TrailHasGS, &r.GS, p.GS)
	set(TrailHasTrack, &r.Track, p.Track)
	set(TrailHasRSSI, &r.RSSIX10, p.RSSI*10)
	return r, true
}

// Codec bytes.
const (
	trailCodecVarint  = 1
	trailCodecDeflate = 2
)

// trailChunkMax is the number of points of a compacted chunk; sealing and
// merging split longer runs at this size too.
const trailChunkMax = 256

// trailMaxPoints bounds n when decoding (corrupt input must not allocate
// unbounded memory).
const trailMaxPoints = 1 << 16

// Deflate levels: sealing runs every few minutes on the writer, compaction
// once per flight.
const (
	trailLevelNone    = 0
	trailLevelSeal    = flate.BestSpeed
	trailLevelCompact = 6
)

var errTrailCorrupt = errors.New("store: corrupt trail chunk")

// column accessors (the 4 nullable columns).
var trailOptBits = [4]uint8{TrailHasAlt, TrailHasGS, TrailHasTrack, TrailHasRSSI}

func trailOpt(r *TrailRow, c int) *int64 {
	switch c {
	case 0:
		return &r.Alt
	case 1:
		return &r.GS
	case 2:
		return &r.Track
	}
	return &r.RSSIX10
}

func zz(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzz(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func uvlen(u uint64) int {
	n := 1
	for u >= 0x80 {
		u >>= 7
		n++
	}
	return n
}

// deltaCost returns the encoded size of vals as first-order (c1) and
// second-order (c2) zig-zag deltas.
func deltaCost(vals []int64) (c1, c2 int) {
	var prev, prevD int64
	for i, v := range vals {
		d := v - prev
		c1 += uvlen(zz(d))
		if i == 0 {
			c2 += uvlen(zz(d))
		} else {
			c2 += uvlen(zz(d - prevD))
		}
		prev, prevD = v, d
	}
	return
}

func appendDeltas(b []byte, vals []int64, second bool) []byte {
	var prev, prevD int64
	for i, v := range vals {
		d := v - prev
		x := d
		if second && i > 0 {
			x = d - prevD
		}
		b = binary.AppendUvarint(b, zz(x))
		prev, prevD = v, d
	}
	return b
}

// trailEncScratch holds the column buffers of one encoder call.
type trailEncScratch struct {
	col  []int64
	buf  []byte
	flat bytes.Buffer
}

var trailEncPool = sync.Pool{New: func() any { return &trailEncScratch{} }}

var trailFlatePools [10]sync.Pool // per level 1..9

func getFlateWriter(w io.Writer, level int) *flate.Writer {
	if fw, ok := trailFlatePools[level].Get().(*flate.Writer); ok {
		fw.Reset(w)
		return fw
	}
	fw, _ := flate.NewWriter(w, level)
	return fw
}

// encodeTrailChunk appends the blob of pts (sorted by T, unique T, one
// flight) to dst. level 0 stores the varint payload, 1-9 additionally tries
// raw deflate at that level and keeps it when it is smaller.
func encodeTrailChunk(dst []byte, pts []TrailRow, level int) []byte {
	sc := trailEncPool.Get().(*trailEncScratch)
	defer trailEncPool.Put(sc)
	p := sc.buf[:0]
	n := len(pts)
	p = binary.AppendUvarint(p, uint64(n))
	maskAt := len(p)
	p = append(p, 0)
	var mask byte

	col := sc.col[:0]
	emit := func(c int, vals []int64) {
		c1, c2 := deltaCost(vals)
		second := c2 < c1
		if second {
			mask |= 1 << c
		}
		p = appendDeltas(p, vals, second)
	}
	for c := 0; c < 3; c++ {
		col = col[:0]
		for i := range pts {
			switch c {
			case 0:
				col = append(col, pts[i].T)
			case 1:
				col = append(col, pts[i].LatE5)
			default:
				col = append(col, pts[i].LonE5)
			}
		}
		emit(c, col)
	}
	for c := 0; c < 4; c++ {
		bit := trailOptBits[c]
		// NULL map: runs alternating present / NULL, starting with present.
		nRuns, present := uint64(1), true
		for i := range pts {
			if has := pts[i].Has&bit != 0; has != present {
				nRuns++
				present = has
			}
		}
		p = binary.AppendUvarint(p, nRuns)
		present, run := true, uint64(0)
		for i := range pts {
			if has := pts[i].Has&bit != 0; has != present {
				p = binary.AppendUvarint(p, run)
				present, run = has, 0
			}
			run++
		}
		p = binary.AppendUvarint(p, run)
		col = col[:0]
		for i := range pts {
			if pts[i].Has&bit != 0 {
				col = append(col, *trailOpt(&pts[i], c))
			}
		}
		emit(3+c, col)
	}
	// src: run-length (value, length) pairs.
	var nRuns uint64
	for i := range pts {
		if i == 0 || pts[i].Src != pts[i-1].Src {
			nRuns++
		}
	}
	p = binary.AppendUvarint(p, nRuns)
	for i := 0; i < n; {
		j := i + 1
		for j < n && pts[j].Src == pts[i].Src {
			j++
		}
		p = binary.AppendUvarint(p, zz(pts[i].Src))
		p = binary.AppendUvarint(p, uint64(j-i))
		i = j
	}
	p[maskAt] = mask
	sc.col, sc.buf = col, p

	if level >= 1 && level <= 9 {
		sc.flat.Reset()
		fw := getFlateWriter(&sc.flat, level)
		_, _ = fw.Write(p)
		_ = fw.Close()
		trailFlatePools[level].Put(fw)
		if sc.flat.Len() < len(p) {
			dst = append(dst, trailCodecDeflate)
			return append(dst, sc.flat.Bytes()...)
		}
	}
	dst = append(dst, trailCodecVarint)
	return append(dst, p...)
}

// trailDecReader reads uvarints from a payload with bounds checks.
type trailDecReader struct {
	b   []byte
	err bool
}

func (r *trailDecReader) uv() uint64 {
	v, k := binary.Uvarint(r.b)
	if k <= 0 {
		r.err = true
		r.b = nil
		return 0
	}
	r.b = r.b[k:]
	return v
}

type trailDecScratch struct {
	inflated bytes.Buffer
	fr       io.ReadCloser
	br       bytes.Reader
}

var trailDecPool = sync.Pool{New: func() any { return &trailDecScratch{} }}

// decodeTrailChunk appends the points of blob to dst (FlightID set to
// flightID). It never panics on corrupt input; it returns errTrailCorrupt.
func decodeTrailChunk(dst []TrailRow, flightID int64, blob []byte) ([]TrailRow, error) {
	if len(blob) == 0 {
		return dst, errTrailCorrupt
	}
	payload := blob[1:]
	switch blob[0] {
	case trailCodecVarint:
	case trailCodecDeflate:
		sc := trailDecPool.Get().(*trailDecScratch)
		defer trailDecPool.Put(sc)
		sc.br.Reset(payload)
		if sc.fr == nil {
			sc.fr = flate.NewReader(&sc.br)
		} else if err := sc.fr.(flate.Resetter).Reset(&sc.br, nil); err != nil {
			return dst, errTrailCorrupt
		}
		sc.inflated.Reset()
		// A payload is at most ~45 bytes per point.
		if _, err := io.Copy(&sc.inflated, io.LimitReader(sc.fr, 64*trailMaxPoints)); err != nil {
			return dst, fmt.Errorf("%w: %v", errTrailCorrupt, err)
		}
		payload = sc.inflated.Bytes()
	default:
		return dst, errTrailCorrupt
	}
	return decodeTrailPayload(dst, flightID, payload)
}

func decodeTrailPayload(dst []TrailRow, flightID int64, payload []byte) ([]TrailRow, error) {
	r := trailDecReader{b: payload}
	n64 := r.uv()
	if r.err || n64 > trailMaxPoints || len(r.b) < 1 {
		return dst, errTrailCorrupt
	}
	n := int(n64)
	mask := r.b[0]
	r.b = r.b[1:]
	base := len(dst)
	dst = slices.Grow(dst, n)[:base+n]
	pts := dst[base:]
	clear(pts)
	for i := range pts {
		pts[i].FlightID = flightID
	}
	// The three dense columns.
	for c := 0; c < 3; c++ {
		second := mask&(1<<c) != 0
		var prev, prevD int64
		for k := range pts {
			d := unzz(r.uv())
			if second && k > 0 {
				d += prevD
			}
			prev += d
			prevD = d
			switch c {
			case 0:
				pts[k].T = prev
			case 1:
				pts[k].LatE5 = prev
			default:
				pts[k].LonE5 = prev
			}
		}
		if r.err {
			return dst[:base], errTrailCorrupt
		}
	}
	// The nullable columns: NULL map, then the values of the present rows.
	for c := 0; c < 4; c++ {
		bit := trailOptBits[c]
		nRuns := r.uv()
		if r.err || nRuns > uint64(n)+1 {
			return dst[:base], errTrailCorrupt
		}
		i, present := 0, true
		for k := uint64(0); k < nRuns; k++ {
			l := r.uv()
			if r.err || l > uint64(n-i) {
				return dst[:base], errTrailCorrupt
			}
			if present {
				for j := i; j < i+int(l); j++ {
					pts[j].Has |= bit
				}
			}
			i += int(l)
			present = !present
		}
		if i != n {
			return dst[:base], errTrailCorrupt
		}
		second := mask&(1<<(3+c)) != 0
		var prev, prevD int64
		k := 0
		for j := range pts {
			if pts[j].Has&bit == 0 {
				continue
			}
			d := unzz(r.uv())
			if second && k > 0 {
				d += prevD
			}
			prev += d
			prevD = d
			*trailOpt(&pts[j], c) = prev
			k++
		}
		if r.err {
			return dst[:base], errTrailCorrupt
		}
	}
	nRuns := r.uv()
	if r.err || nRuns > uint64(n) {
		return dst[:base], errTrailCorrupt
	}
	i := 0
	for k := uint64(0); k < nRuns; k++ {
		v := unzz(r.uv())
		l := r.uv()
		if r.err || l == 0 || l > uint64(n-i) {
			return dst[:base], errTrailCorrupt
		}
		for j := i; j < i+int(l); j++ {
			pts[j].Src = v
		}
		i += int(l)
	}
	if i != n || len(r.b) != 0 {
		return dst[:base], errTrailCorrupt
	}
	return dst, nil
}

// trailChunkBounds returns t0, t1 of sorted points.
func trailChunkBounds(pts []TrailRow) (t0, t1 int64) {
	return pts[0].T, pts[len(pts)-1].T
}
