package server

import (
	"math"
	"net/http"
	"time"

	"airview/internal/analytics"
	"airview/internal/briefing"
	"airview/internal/store"
)

// briefingHistoryDays is how many earlier days the briefing compares with
// (four of each weekday).
const briefingHistoryDays = 28

// apiBriefingDaily writes the daily briefing for one station-local day:
// `date=YYYY-MM-DD`, `date=today` (the day so far) or nothing (the last
// complete day).
func apiBriefingDaily(s *Server, r *http.Request) (any, error) {
	ant, err := qAnt(r)
	if err != nil {
		return nil, err
	}
	a := s.eng.Analyzer()
	now, off := a.Now()
	todayStart := floorDay(now.Unix()+off)*86400 - off
	start := todayStart - 86400 // default: yesterday
	switch v := qStr(r, "date"); v {
	case "", "yesterday":
	case "today":
		start = todayStart
	default:
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			return nil, badRequest("date: want YYYY-MM-DD, today or yesterday")
		}
		start = d.Unix() - off
		if start > todayStart {
			return nil, badRequest("date: the day has not started yet")
		}
	}
	end := start + 86400
	partial := start == todayStart

	daily, err := a.Daily(r.Context(), analytics.Range{From: start - briefingHistoryDays*86400, To: end}, ant)
	if err != nil {
		return nil, err
	}
	in := briefing.Input{Partial: partial, UTCOffsetSec: daily.UTCOffsetSec, Now: now}
	date := time.Unix(start+off, 0).UTC().Format("2006-01-02")
	in.Day = analytics.DayTotals{T: start * 1000, Date: date, MaxKm: analytics.Num(math.NaN())}
	for _, d := range daily.Days {
		if d.Date == date {
			in.Day = d
		} else {
			in.History = append(in.History, d)
		}
	}
	if in.Day.Flights > 0 {
		if in.Top, err = a.Top(r.Context(), analytics.Range{From: start, To: end}, 5); err != nil {
			return nil, err
		}
	}
	// The day's events (newest first); a day with more than 2000 events
	// (mostly catches) keeps the newest ones.
	ev, err := s.eng.Store().Events(r.Context(), store.EventQuery{Since: start, Until: end, Limit: 2000})
	if err != nil {
		return nil, err
	}
	in.Events = ev
	return briefing.Build(in), nil
}

func floorDay(sec int64) int64 {
	d := sec / 86400
	if sec%86400 < 0 {
		d--
	}
	return d
}
