package alerts

import (
	"fmt"
	"sort"
	"time"

	"airview/internal/model"
)

// statusMonitor turns periodic model.Status snapshots into system events
// (feed down/up, NTP sync lost/regained, range collapse/restored).
// Memory is bounded: 24 hourly + 15 one-minute range bins, one entry per feed.
type statusMonitor struct {
	feeds       map[string]*feedState
	ntpInit     bool
	ntpSynced   bool
	hourBins    [24]float64 // max range per hour slot
	hourStamp   [24]int64   // hour number of each slot
	minBins     [15]float64
	minStamp    [15]int64
	collapsed   bool
	baselineMin int // hours of history needed (6)
}

type feedState struct {
	connected bool
	downSince int64 // Unix ms, 0 when connected
	reported  bool  // feed_down already emitted for this outage
	name      string
}

func (m *statusMonitor) record(nowMs int64, km float64, withPos int) {
	if withPos <= 0 || km <= 0 {
		return
	}
	h := nowMs / 3_600_000
	hi := int(h % 24)
	if m.hourStamp[hi] != h {
		m.hourStamp[hi], m.hourBins[hi] = h, 0
	}
	if km > m.hourBins[hi] {
		m.hourBins[hi] = km
	}
	mn := nowMs / 60_000
	mi := int(mn % 15)
	if m.minStamp[mi] != mn {
		m.minStamp[mi], m.minBins[mi] = mn, 0
	}
	if km > m.minBins[mi] {
		m.minBins[mi] = km
	}
}

// rangeBaseline is the median hourly maximum over the last 24 h (0 when
// fewer than 6 hours of data exist).
func (m *statusMonitor) rangeBaseline() float64 {
	var v []float64
	var newest int64
	for _, s := range m.hourStamp {
		newest = max(newest, s)
	}
	for i, s := range m.hourStamp {
		if s != 0 && newest-s < 24 && m.hourBins[i] > 0 {
			v = append(v, m.hourBins[i])
		}
	}
	if len(v) < 6 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

// recentMax is the maximum range of the last 15 minutes.
func (m *statusMonitor) recentMax() float64 {
	var newest int64
	for _, s := range m.minStamp {
		newest = max(newest, s)
	}
	best := 0.0
	for i, s := range m.minStamp {
		if s != 0 && newest-s < 15 {
			best = max(best, m.minBins[i])
		}
	}
	return best
}

// recentMinutes counts minutes with data in the 15-minute window.
func (m *statusMonitor) recentMinutes() int {
	var newest int64
	for _, s := range m.minStamp {
		newest = max(newest, s)
	}
	n := 0
	for i, s := range m.minStamp {
		if s != 0 && newest-s < 15 && m.minBins[i] > 0 {
			n++
		}
	}
	return n
}

// CheckStatus derives system events from a status snapshot (call every
// 5-10 s with the same model.Status the server sends as SSE "status"):
// feed_down (enabled feed disconnected longer than FeedDownGrace), feed_up
// (after a reported outage), ntp_unsync / ntp_sync (NTP enabled and its
// Synced flag flips), range_collapse / range_restored (15-minute maximum
// range below RangeCollapseFrac x the 24 h median hourly maximum, with at
// least 3 positioned aircraft; restored above 60 %).
func (e *Engine) CheckStatus(st model.Status, now time.Time) {
	nowMs := now.UnixMilli()
	var evs []SystemEvent
	e.mu.Lock()
	m := &e.mon
	if m.feeds == nil {
		m.feeds = map[string]*feedState{}
	}
	seen := make(map[string]bool, len(st.Feeds))
	for _, f := range st.Feeds {
		seen[f.ID] = true
		fs, ok := m.feeds[f.ID]
		if !ok {
			if len(m.feeds) >= 64 {
				continue
			}
			fs = &feedState{connected: true}
			m.feeds[f.ID] = fs
		}
		fs.name = f.Name
		up := f.Connected || !f.Enabled
		switch {
		case up && !fs.connected:
			if fs.reported {
				evs = append(evs, SystemEvent{Kind: SysFeedUp, Key: f.ID, Text: fmt.Sprintf("feed %s reconnected", label(f))})
			}
			fs.connected, fs.downSince, fs.reported = true, 0, false
		case !up && fs.connected:
			fs.connected, fs.downSince = false, nowMs
		case !up && !fs.reported && nowMs-fs.downSince >= e.opts.FeedDownGrace.Milliseconds():
			fs.reported = true
			downS := float64((nowMs - fs.downSince) / 1000)
			txt := fmt.Sprintf("feed %s down for %.0f s", label(f), downS)
			if f.LastError != "" {
				txt += ": " + f.LastError
			}
			evs = append(evs, SystemEvent{Kind: SysFeedDown, Key: f.ID, Text: txt, Value: &downS, Unit: "s"})
		}
	}
	for id := range m.feeds {
		if !seen[id] {
			delete(m.feeds, id)
		}
	}
	if st.NTP.Enabled {
		if m.ntpInit && m.ntpSynced && !st.NTP.Synced {
			evs = append(evs, SystemEvent{Kind: SysNTPUnsync, Key: st.NTP.Server, Text: "NTP not synchronised (" + st.NTP.Server + ")"})
		}
		if m.ntpInit && !m.ntpSynced && st.NTP.Synced {
			evs = append(evs, SystemEvent{Kind: SysNTPSync, Key: st.NTP.Server, Text: "NTP synchronised again"})
		}
		m.ntpInit, m.ntpSynced = true, st.NTP.Synced
	} else {
		m.ntpInit = false
	}
	m.record(nowMs, st.Counts.MaxRangeKm, st.Counts.WithPos)
	base, recent := m.rangeBaseline(), m.recentMax()
	if base >= 50 && m.recentMinutes() >= 10 && st.Counts.WithPos >= 3 {
		if !m.collapsed && recent < e.opts.RangeCollapseFrac*base {
			m.collapsed = true
			v, b := recent, base
			evs = append(evs, SystemEvent{Kind: SysRangeCollapse, Value: &v, Ref: &b, Unit: "km",
				Text: fmt.Sprintf("range collapsed: %.0f km in the last 15 min vs. %.0f km typical", recent, base)})
		} else if m.collapsed && recent >= 0.6*base {
			m.collapsed = false
			v, b := recent, base
			evs = append(evs, SystemEvent{Kind: SysRangeRestored, Value: &v, Ref: &b, Unit: "km",
				Text: fmt.Sprintf("range restored: %.0f km", recent)})
		}
	}
	rs := e.rules.Load()
	for i := range evs {
		evs[i].T = now
		e.handleSystemLocked(rs, &evs[i], nowMs)
	}
	e.mu.Unlock()
}

func label(f model.FeedStatus) string {
	if f.Name != "" {
		return f.Name
	}
	return f.ID
}
