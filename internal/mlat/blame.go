package mlat

import (
	"sync"
	"time"

	"airview/internal/model"
)

// ADS-B verification attribution (HOOKS: "WP-SU-survey -> internal/mlat:
// one wrong station must not suspend the sky"). A station whose position
// is only declared can be wrong by hundreds of metres; every TDOA check it
// takes part in then contradicts the (correct) ADS-B position, and the
// engine would suspend the reference aircraft around it. verifyADSB
// therefore (a) never uses a declared position the survey already flagged
// suspect, and (b) when a check is contradicted, retries it without each
// declared-only station: when dropping one makes the contradiction go away,
// the contradiction is charged to that station instead of the aircraft.
// A station charged blameLimit times within blameWindow is handed to the
// survey (SurveyRequester) at most once per blameSurveyEvery.

const (
	blameWindow      = 10 * time.Minute
	blameLimit       = 5
	blameSurveyEvery = time.Hour
	// blameMaxTries bounds the leave-one-out retries per contradiction.
	blameMaxTries = 4
)

type blameRec struct {
	since     int64 // start of the current window
	n         int
	requested int64 // last survey request
}

type blameTable struct {
	mu sync.Mutex
	m  map[model.StationIdx]*blameRec
}

// charge records one contradiction attributed to st and reports whether a
// survey should be requested now.
func (b *blameTable) charge(st model.StationIdx, nowNs int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[model.StationIdx]*blameRec{}
	}
	r := b.m[st]
	if r == nil {
		r = &blameRec{since: nowNs}
		b.m[st] = r
	}
	if nowNs-r.since > int64(blameWindow) {
		r.since, r.n = nowNs, 0
	}
	r.n++
	if r.n >= blameLimit && (r.requested == 0 || nowNs-r.requested >= int64(blameSurveyEvery)) {
		r.requested = nowNs
		r.n = 0
		return true
	}
	return false
}

// expire forgets stations without a recent charge or request.
func (b *blameTable) expire(beforeNs int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for st, r := range b.m {
		if r.since < beforeNs && r.requested < beforeNs {
			delete(b.m, st)
		}
	}
}

// blameStation charges a contradiction to st (see above).
func (e *Engine) blameStation(st model.StationIdx, nowNs int64) {
	e.ctr.verifyBlamed.Add(1)
	if e.blame.charge(st, nowNs) {
		e.o.Logger.Info("mlat: station contradicts ADS-B positions; survey requested", "station", st)
		if e.o.Survey != nil {
			e.o.Survey.RequestSurvey(st, "ADS-B verification")
		}
	}
}
