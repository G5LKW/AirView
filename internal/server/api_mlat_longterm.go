package server

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"airview/internal/model"
	"airview/internal/store"
)

// Long-term MLAT history (days to weeks) from the minute / hour tables the
// engine writes (internal/engine/mlatmetrics.go): mlat_minute (per station
// SID and the network row SID "") and mlat_quality_hour (hold-out error
// histograms per hour × 1° cell × altitude band). The RAM rings of
// mlat_history.go cover the last 24 h (network) / 2 h (station) at full
// detail; these routes cover up to 90 days at one point per hour.
//
// Aggregation of the minutes of one hour (bucket b, minutes present m_b):
//
//   - counters (frames, groups, fixes, outliers) are summed and reported
//     per minute of data: rate = Σ / m_b, so a partially covered hour is not
//     diluted by minutes without data;
//   - peers: maximum (a gauge);
//   - synced share: Σ SyncedSecs / (60 · m_known) over the minutes with a
//     known value;
//   - σθ, residual RMS, drift: mean over the minutes with a known value.
//
// Hold-out error per hour: the histograms of every cell and band of that
// hour are added bin by bin and the percentiles are read from the sum
// (store.MLATErrPercentile, geometric interpolation inside 0.1-decade
// bins, i.e. ≤ ±12 % resolution); N is the summed count. Cells are not
// returned here (admin error maps use /api/admin/mlat/quality?cells=1).
//
// Scopes: the network series is admin-only (it includes the hold-out
// statistics per hour); owners get only their own station's series. Nothing
// is public.

const longtermMaxDays = 90

type mlatLongtermDTO struct {
	Running     bool       `json:"running"`
	BucketSec   int        `json:"bucketSec"`
	From        int64      `json:"from"` // Unix ms
	To          int64      `json:"to"`
	T           []int64    `json:"t"` // bucket start, Unix ms
	Minutes     []int      `json:"minutes"`
	FramesPM    []float64  `json:"framesPerMin"`
	GroupsPM    []float64  `json:"groupsPerMin"`
	FixesPM     []float64  `json:"fixesPerMin"`
	OutliersPM  []float64  `json:"outliersPerMin"`
	Peers       []int64    `json:"peers"`
	SyncedShare []*float64 `json:"syncedShare"` // 0..1
	SigmaNs     []*float64 `json:"sigmaNs"`
	ResidRMSNs  []*float64 `json:"residRmsNs"`
	DriftPPM    []*float64 `json:"driftPpm"`
	// network only: hold-out error per hour
	HoldN   []int64    `json:"holdN,omitempty"`
	HoldP50 []*float64 `json:"holdP50M,omitempty"`
	HoldP95 []*float64 `json:"holdP95M,omitempty"`
}

func longtermDays(r *http.Request) (int, error) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > longtermMaxDays {
			return 0, badRequest("days: 1..%d", longtermMaxDays)
		}
		days = n
	}
	return days, nil
}

// meanAcc averages the known (non-NaN) values.
type meanAcc struct {
	sum float64
	n   int
}

func (a *meanAcc) add(v float64) {
	if !math.IsNaN(v) && !math.IsInf(v, 0) {
		a.sum += v
		a.n++
	}
}

func (a *meanAcc) get(dec float64) *float64 {
	if a.n == 0 {
		return nil
	}
	v := math.Round(a.sum/float64(a.n)*dec) / dec
	return &v
}

type ltBucket struct {
	minutes                         int
	frames, groups, fixes, outliers int64
	peers                           int64
	synced                          meanAcc // SyncedSecs per minute
	sigma, resid, drift             meanAcc
	holdN                           int64
	hist                            []uint32
}

// aggregateMinutes buckets mlat_minute rows (T in Unix seconds) by bucket
// seconds.
func aggregateMinutes(rows []store.MLATMinute, bucket int64) map[int64]*ltBucket {
	out := map[int64]*ltBucket{}
	for _, r := range rows {
		k := r.T - r.T%bucket
		b := out[k]
		if b == nil {
			b = &ltBucket{}
			out[k] = b
		}
		b.minutes++
		b.frames += r.Frames
		b.groups += r.Groups
		b.fixes += r.Fixes
		b.outliers += r.Outliers
		b.peers = max(b.peers, r.Peers)
		b.synced.add(r.SyncedSecs)
		b.sigma.add(r.SigmaNs)
		b.resid.add(r.ResidRMSNs)
		b.drift.add(r.DriftPPM)
	}
	return out
}

func longtermDTO(bk map[int64]*ltBucket, bucket int64, from, to time.Time, hold bool) mlatLongtermDTO {
	keys := make([]int64, 0, len(bk))
	for k := range bk {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	d := mlatLongtermDTO{Running: true, BucketSec: int(bucket), From: from.UnixMilli(), To: to.UnixMilli(),
		T: []int64{}, Minutes: []int{}, FramesPM: []float64{}, GroupsPM: []float64{}, FixesPM: []float64{}, OutliersPM: []float64{},
		Peers: []int64{}, SyncedShare: []*float64{}, SigmaNs: []*float64{}, ResidRMSNs: []*float64{}, DriftPPM: []*float64{}}
	if hold {
		d.HoldN, d.HoldP50, d.HoldP95 = []int64{}, []*float64{}, []*float64{}
	}
	pm := func(v int64, m int) float64 {
		if m == 0 {
			return 0
		}
		return math.Round(float64(v)/float64(m)*100) / 100
	}
	for _, k := range keys {
		b := bk[k]
		d.T = append(d.T, k*1000)
		d.Minutes = append(d.Minutes, b.minutes)
		d.FramesPM = append(d.FramesPM, pm(b.frames, b.minutes))
		d.GroupsPM = append(d.GroupsPM, pm(b.groups, b.minutes))
		d.FixesPM = append(d.FixesPM, pm(b.fixes, b.minutes))
		d.OutliersPM = append(d.OutliersPM, pm(b.outliers, b.minutes))
		d.Peers = append(d.Peers, b.peers)
		var share *float64
		if b.synced.n > 0 {
			v := math.Round(min(1, b.synced.sum/(60*float64(b.synced.n)))*1000) / 1000
			share = &v
		}
		d.SyncedShare = append(d.SyncedShare, share)
		d.SigmaNs = append(d.SigmaNs, b.sigma.get(10))
		d.ResidRMSNs = append(d.ResidRMSNs, b.resid.get(10))
		d.DriftPPM = append(d.DriftPPM, b.drift.get(1e4))
		if hold {
			d.HoldN = append(d.HoldN, b.holdN)
			var p50, p95 *float64
			if b.holdN > 0 {
				a := rnd1(store.MLATErrPercentile(b.hist, 0.5))
				c := rnd1(store.MLATErrPercentile(b.hist, 0.95))
				p50, p95 = &a, &c
			}
			d.HoldP50 = append(d.HoldP50, p50)
			d.HoldP95 = append(d.HoldP95, p95)
		}
	}
	return d
}

// longterm reads the series of one SID ("" = network, with hold-out).
func (s *Server) longterm(ctx context.Context, sid string, days int) (any, error) {
	if s.mlatSrc() == nil {
		return mlatOff, nil
	}
	st := s.eng.Store()
	if st == nil {
		return mlatOff, nil
	}
	to := time.Now()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := st.MLATMinutes(ctx, sid, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	const bucket = 3600
	bk := aggregateMinutes(rows, bucket)
	hold := sid == ""
	if hold {
		qs, err := st.MLATQualityHours(ctx, from.Unix()-bucket, to.Unix(), -1)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			k := q.T - q.T%bucket
			b := bk[k]
			if b == nil {
				b = &ltBucket{}
				bk[k] = b
			}
			b.holdN += q.N
			if len(b.hist) < len(q.Hist) {
				b.hist = append(b.hist, make([]uint32, len(q.Hist)-len(b.hist))...)
			}
			for i, c := range q.Hist {
				b.hist[i] += c
			}
		}
	}
	return longtermDTO(bk, bucket, from, to, hold), nil
}

func apiAdminMLATLongterm(s *Server, r *http.Request) (any, error) {
	days, err := longtermDays(r)
	if err != nil {
		return nil, err
	}
	return s.longterm(r.Context(), "", days)
}

func apiMeStationMLATLongterm(s *Server, r *http.Request) (any, error) {
	days, err := longtermDays(r)
	if err != nil {
		return nil, err
	}
	st, _ := selectedStation(r)
	if st.SID == "" {
		return nil, badRequest("no station")
	}
	out, err := s.longterm(r.Context(), st.SID, days)
	if err != nil {
		return nil, err
	}
	// owner view: coarse drift (finding PRIV-2)
	if d, ok := out.(mlatLongtermDTO); ok && s.viewerOf(r).Scope != model.ScopeAdmin {
		coarsenDrift(d.DriftPPM)
		return d, nil
	}
	return out, nil
}
