package uplink

import "time"

// beastReducer thins a Beast output the way beast_reduce_out is documented
// to behave (readsb README: "removes duplicates within the reduce interval;
// data that does not affect the position is sent at 4x the interval"):
// per aircraft address, downlink format, ADS-B type code and CPR format, at
// most one frame per interval for position and velocity data and one per
// four intervals for everything else. Our own implementation from that
// description; see docs/aggregator/CLEANROOM.md.
type beastReducer struct {
	base    time.Duration
	last    map[uint64]int64 // key -> host ns last passed
	pruneAt int64
}

func (r *beastReducer) init(base time.Duration) {
	r.base = base
	r.last = make(map[uint64]int64, 1024)
}

// reset forgets every aircraft (new connection).
func (r *beastReducer) reset() {
	clear(r.last)
	r.pruneAt = 0
}

// reduceKey classifies a Mode S / Mode A/C payload. pos reports position or
// velocity data (sent at the base interval).
func reduceKey(d []byte) (key uint64, pos bool) {
	addr, df := frameAddr(d)
	key = addr<<16 | uint64(df)<<8
	if (df == 17 || df == 18) && len(d) == 14 {
		tc := d[4] >> 3
		key |= uint64(tc) << 1
		switch {
		case tc >= 5 && tc <= 18, tc >= 20 && tc <= 22:
			key |= uint64(d[6]>>2) & 1 // even and odd CPR frames are both needed
			pos = true
		case tc == 19:
			pos = true
		}
	}
	return key, pos
}

// pass reports whether a frame received at hostNs is sent. slowLevel doubles
// the interval per level (aggregator 'W' 'S' requests).
func (r *beastReducer) pass(d []byte, hostNs int64, slowLevel int) bool {
	k, pos := reduceKey(d)
	iv := int64(r.base) << uint(slowLevel)
	if !pos {
		iv *= 4
	}
	if t, ok := r.last[k]; ok && hostNs >= t && hostNs-t < iv {
		return false
	}
	r.last[k] = hostNs
	if hostNs >= r.pruneAt {
		r.pruneAt = hostNs + 30e9
		keep := 4 * (int64(r.base) << 3)
		for key, t := range r.last {
			if hostNs-t > keep || t > hostNs+60e9 {
				delete(r.last, key)
			}
		}
	}
	return true
}
