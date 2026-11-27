package uplink

import (
	"math"
	"strconv"

	"airview/internal/avf"
	"airview/internal/modes"
)

// RSSI histogram of MINUTE_STATS: 1 dB bins from rssiLo to 0 dBFS.
const (
	rssiLo   = -50
	rssiBins = -rssiLo
)

// minuteStats counts one receiver's frames per minute exactly, before any
// selective filtering (AVF §5.12).
type minuteStats struct {
	minute    int64 // Unix second of the minute start (0 = nothing counted)
	df        [26]uint64
	modeAC    uint64
	icaos     map[uint32]struct{}
	positions uint64
	crcFail   uint64
	rssi      [rssiBins]uint64
	frames    uint64
}

// add counts one frame. It returns a finished minute (and true) when the
// frame starts a new minute.
func (m *minuteStats) add(data []byte, sigDBFS float64, hostNs int64, rxNo int) (avf.MinuteStats, bool) {
	mn := floorMinute(hostNs)
	var out avf.MinuteStats
	done := false
	if m.minute != 0 && mn != m.minute {
		if mn < m.minute {
			// a frame from an earlier minute (late input): count it in the
			// current minute rather than reopening a finished one
			mn = m.minute
		} else {
			out, done = m.finish(rxNo), true
		}
	}
	if m.minute == 0 {
		m.minute = mn
	}
	m.frames++
	if !math.IsNaN(sigDBFS) && !math.IsInf(sigDBFS, 0) {
		i := int(math.Floor(sigDBFS)) - rssiLo
		m.rssi[max(0, min(i, rssiBins-1))]++
	}
	if len(data) == 2 {
		m.modeAC++
		return out, done
	}
	if len(data) != 7 && len(data) != 14 {
		return out, done
	}
	df := data[0] >> 3
	if df > 24 {
		df = 24
	}
	m.df[df]++
	switch df {
	case 17, 18:
		if len(data) != 14 {
			return out, done
		}
		if modes.Residual(data) != 0 {
			m.crcFail++
			return out, done
		}
		m.addICAO(data)
		tc := data[4] >> 3
		if (tc >= 5 && tc <= 18) || (tc >= 20 && tc <= 22) {
			m.positions++
		}
	case 11:
		if modes.Residual(data)&^0x7F == 0 {
			m.addICAO(data)
		}
	}
	return out, done
}

func (m *minuteStats) addICAO(data []byte) {
	if m.icaos == nil {
		m.icaos = make(map[uint32]struct{}, 256)
	}
	m.icaos[uint32(data[1])<<16|uint32(data[2])<<8|uint32(data[3])] = struct{}{}
}

// finish returns the current minute's record and resets the counters.
func (m *minuteStats) finish(rxNo int) avf.MinuteStats {
	out := avf.MinuteStats{RxNo: rxNo, Minute: m.minute, DF: map[string]uint64{}, ModeAC: m.modeAC,
		ICAOs: uint64(len(m.icaos)), Positions: m.positions, CRCFail: m.crcFail,
		RSSI: &avf.RSSIHist{Lo: rssiLo, Counts: append([]uint64(nil), m.rssi[:]...)}}
	for i, n := range m.df {
		if n > 0 {
			out.DF[strconv.Itoa(i)] = n
		}
	}
	icaos := m.icaos
	clear(icaos)
	*m = minuteStats{icaos: icaos}
	return out
}

func floorMinute(hostNs int64) int64 {
	s := hostNs / 1e9
	if hostNs < 0 && hostNs%1e9 != 0 {
		s--
	}
	return s - ((s%60)+60)%60
}
