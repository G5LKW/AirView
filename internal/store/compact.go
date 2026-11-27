package store

import (
	"encoding/binary"
	"math"
)

// Compact BLOB encodings for stations.db (DESIGN §20.2).
//
// range_hour is the largest hourly table of an aggregator: every row
// carries a 120-bin distance histogram (480 bytes raw) and 36 sector maxima
// (144 bytes raw), ~880 bytes per row, which over 13 months is ~17 MB per
// feeder. stations.db therefore stores both BLOBs in a compact form;
// airview.db keeps the raw little-endian arrays (unchanged format).
//
// The decoders (DecodeDistHist, DecodeSectorMax) accept both forms: a
// compact BLOB starts with the byte compactMagic and its length is never a
// multiple of 4 (a zero pad byte is appended when needed); a raw array's
// length always is.
//
// Histogram: magic, uvarint(bins), then tokens: a count c > 0 is uvarint(c<<1), a
// run of k zero bins uvarint(k<<1 | 1).
// Sector maxima: magic, uvarint(sectors), then uvarint(round(km*10)) per sector
// (0.1 km resolution; negative or NaN as 0).

// EncodeDistHistCompact packs a histogram in the compact form.
func EncodeDistHistCompact(h []uint32) []byte {
	if len(h) == 0 {
		return nil
	}
	b := binary.AppendUvarint(append(make([]byte, 0, 3+len(h)), compactMagic), uint64(len(h)))
	for i := 0; i < len(h); {
		if h[i] == 0 {
			k := 0
			for i < len(h) && h[i] == 0 {
				k++
				i++
			}
			b = binary.AppendUvarint(b, uint64(k)<<1|1)
			continue
		}
		b = binary.AppendUvarint(b, uint64(h[i])<<1)
		i++
	}
	return padCompact(b)
}

// EncodeSectorMaxCompact packs sector maxima in the compact form.
func EncodeSectorMaxCompact(s []float32) []byte {
	if len(s) == 0 {
		return nil
	}
	b := binary.AppendUvarint(append(make([]byte, 0, 3+2*len(s)), compactMagic), uint64(len(s)))
	for _, v := range s {
		var q uint64
		if f := float64(v); f > 0 && !math.IsInf(f, 0) {
			q = uint64(math.Round(math.Min(f, 1e9) * 10))
		}
		b = binary.AppendUvarint(b, q)
	}
	return padCompact(b)
}

func padCompact(b []byte) []byte {
	if len(b)%4 == 0 {
		b = append(b, 0)
	}
	return b
}

// compactMagic is the first byte of a compact BLOB.
const compactMagic = 0xC7

func isCompact(b []byte) bool { return len(b)%4 != 0 && len(b) > 1 && b[0] == compactMagic }

func decodeDistHistCompact(b []byte) []uint32 {
	n, k := binary.Uvarint(b[1:])
	if k <= 0 || n == 0 || n > 1<<16 {
		return nil
	}
	k++
	b = b[k:]
	h := make([]uint32, n)
	for i := 0; i < int(n) && len(b) > 0; {
		v, k := binary.Uvarint(b)
		if k <= 0 {
			break
		}
		b = b[k:]
		if v&1 == 1 {
			i += int(v >> 1)
			continue
		}
		h[i] = uint32(min(v>>1, math.MaxUint32))
		i++
	}
	return h
}

func decodeSectorMaxCompact(b []byte) []float32 {
	n, k := binary.Uvarint(b[1:])
	if k <= 0 || n == 0 || n > 1<<16 {
		return nil
	}
	k++
	b = b[k:]
	s := make([]float32, n)
	for i := range s {
		v, k := binary.Uvarint(b)
		if k <= 0 {
			break
		}
		b = b[k:]
		s[i] = float32(float64(v) / 10)
	}
	return s
}

// rangeHourEncoders returns the BLOB encoders of range_hour for a database
// kind (Batch.AddRangeHours): stations.db packs both BLOBs; airview.db
// (since migration "range" 2) packs the distance histogram, which is
// lossless, and keeps the sector maxima as exact float32 (the compact form
// rounds them to 0.1 km).
func rangeHourEncoders(k Kind) (hist func([]uint32) []byte, sect func([]float32) []byte) {
	if k == KindStations {
		return EncodeDistHistCompact, EncodeSectorMaxCompact
	}
	return EncodeDistHistCompact, EncodeSectorMax
}
