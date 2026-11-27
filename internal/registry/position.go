package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"airview/internal/geoid"
	"airview/internal/model"
)

// Feeder-announced positions (finding PRIV-1, H-mlatc).
//
// A feeder tells the server where it is: AVF/1 RECEIVER records and the
// mlat-client handshake carry a position. The owner controls that value,
// and every position change moves the station's MLAT geometry and the
// reference of its owner views (peer distance bands, coverage). Unlimited
// changes would let an owner probe the network (move, query, move back)
// and would let a wrong value take effect at once. Changes announced by a
// feeder therefore go through FeederPosition:
//
//   - the first position of a station is applied at once;
//   - changes below PositionPolicy.JitterM are ignored (GNSS noise,
//     rounding);
//   - moderate changes (below HoldBeyondM) are applied at most once per
//     MinInterval; a change inside the interval is kept pending and applied
//     when the interval has passed (next announcement or the hourly
//     ReleasePending);
//   - a move of HoldBeyondM or more is held pending a survey: it is applied
//     when the station's survey estimate (SetEstimated) lands near it, when
//     the owner or admin confirms it (ConfirmPendingPosition), or when the
//     feeder has kept announcing it for HoldMax (and the rate limit
//     allows).
//
// Positions the owner enters in the web UI (SetDeclared) are not limited;
// an owner-declared position is never replaced by a compat handshake.

// PositionPolicy tunes FeederPosition. Zero fields take the defaults.
type PositionPolicy struct {
	// JitterM: changes smaller than this are ignored (default 30 m).
	JitterM float64
	// MinInterval: at most one applied change per station per interval
	// (default 24 h).
	MinInterval time.Duration
	// HoldBeyondM: moves at least this large wait for the survey, a
	// confirmation or HoldMax (default 1000 m).
	HoldBeyondM float64
	// HoldMax releases a held move announced continuously this long
	// (default 7 days; < 0 = never).
	HoldMax time.Duration
	// SurveyMatchM: a survey estimate within max(SurveyMatchM, 3σ) of a held
	// position confirms it (default 250 m).
	SurveyMatchM float64
}

func (p PositionPolicy) normalized() PositionPolicy {
	if p.JitterM <= 0 {
		p.JitterM = 30
	}
	if p.MinInterval <= 0 {
		p.MinInterval = 24 * time.Hour
	}
	if p.HoldBeyondM <= 0 {
		p.HoldBeyondM = 1000
	}
	if p.HoldMax == 0 {
		p.HoldMax = 7 * 24 * time.Hour
	}
	if p.SurveyMatchM <= 0 {
		p.SurveyMatchM = 250
	}
	return p
}

// FeederPos is one position announced by a feeder.
type FeederPos struct {
	Lat, Lon, Alt float64
	// AltRef is model.AltRefMSL, AltRefHAE or AltRefAmbiguous (declared
	// positions; GNSS positions are HAE).
	AltRef string
	// GNSS: the receiver's own GNSS position (stored in Station.GNSS),
	// otherwise a declared position (Station.Declared).
	GNSS bool
	// SigmaM is the announced 1σ (0 = unknown).
	SigmaM float64
	// SetEffective also makes the position the effective one, unless a
	// survey result or a contradiction is in charge (the AVF RECEIVER
	// semantics). mlat-client handshakes only set the declared position.
	SetEffective bool
}

// Position decisions (FeederPosition).
const (
	PosFirst   = "first"   // the station had no position of this kind: applied
	PosApplied = "applied" // a change within the rate limit: applied
	PosIgnored = "ignored" // below JitterM (or equal to the pending one, still waiting)
	PosLimited = "limited" // a change inside MinInterval: pending
	PosHeld    = "held"    // a large move: pending a survey or confirmation
	PosOwner   = "owner"   // an owner-declared position is in force: not replaced
)

// PendingPos is a position waiting to be applied.
type PendingPos struct {
	FeederPos
	// Reason is PosLimited or PosHeld.
	Reason string
	// Since: first announcement of this position; Last: latest one.
	Since, Last time.Time
}

// feederPosRow is the station_feeder_pos row of a station.
type feederPosRow struct {
	lastChange       int64
	applied          Position
	appliedGNSS      bool
	hasPend          bool
	pend             PendingPos
	pendSince, pLast int64
	held             int64
}

func (r *Registry) loadFeederPos(ctx context.Context, sid string) (feederPosRow, error) {
	var row feederPosRow
	var aLat, aLon, aAlt, pLat, pLon, pAlt sql.NullFloat64
	var aG, pG, pEff int
	err := r.rdb.QueryRowContext(ctx, `SELECT last_change, applied_lat, applied_lon, applied_alt, applied_gnss,
 pend_lat, pend_lon, pend_alt, pend_ref, pend_gnss, pend_sigma, pend_eff, pend_since, pend_last, pend_reason, held
 FROM station_feeder_pos WHERE sid=?`, sid).Scan(&row.lastChange, &aLat, &aLon, &aAlt, &aG,
		&pLat, &pLon, &pAlt, &row.pend.AltRef, &pG, &row.pend.SigmaM, &pEff, &row.pendSince, &row.pLast, &row.pend.Reason, &row.held)
	if errors.Is(err, sql.ErrNoRows) {
		return feederPosRow{}, nil
	}
	if err != nil {
		return row, err
	}
	if aLat.Valid && aLon.Valid {
		row.applied = Position{Lat: aLat.Float64, Lon: aLon.Float64, Alt: aAlt.Float64, Valid: true}
	}
	row.appliedGNSS = aG != 0
	if pLat.Valid && pLon.Valid && row.pend.Reason != "" {
		row.hasPend = true
		row.pend.Lat, row.pend.Lon, row.pend.Alt = pLat.Float64, pLon.Float64, pAlt.Float64
		row.pend.GNSS, row.pend.SetEffective = pG != 0, pEff != 0
		row.pend.Since, row.pend.Last = fromS(row.pendSince), fromS(row.pLast)
	}
	return row, nil
}

func (r *Registry) saveFeederPos(ctx context.Context, tx *sql.Tx, sid string, row *feederPosRow) error {
	nf := func(v float64, ok bool) any {
		if !ok {
			return nil
		}
		return v
	}
	reason := ""
	if row.hasPend {
		reason = row.pend.Reason
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO station_feeder_pos(sid, last_change, applied_lat, applied_lon, applied_alt,
 applied_gnss, pend_lat, pend_lon, pend_alt, pend_ref, pend_gnss, pend_sigma, pend_eff, pend_since, pend_last, pend_reason, held)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(sid) DO UPDATE SET last_change=excluded.last_change, applied_lat=excluded.applied_lat,
 applied_lon=excluded.applied_lon, applied_alt=excluded.applied_alt, applied_gnss=excluded.applied_gnss,
 pend_lat=excluded.pend_lat, pend_lon=excluded.pend_lon, pend_alt=excluded.pend_alt, pend_ref=excluded.pend_ref,
 pend_gnss=excluded.pend_gnss, pend_sigma=excluded.pend_sigma, pend_eff=excluded.pend_eff,
 pend_since=excluded.pend_since, pend_last=excluded.pend_last, pend_reason=excluded.pend_reason, held=excluded.held`,
		sid, row.lastChange, nf(row.applied.Lat, row.applied.Valid), nf(row.applied.Lon, row.applied.Valid),
		nf(row.applied.Alt, row.applied.Valid), b2i(row.appliedGNSS),
		nf(row.pend.Lat, row.hasPend), nf(row.pend.Lon, row.hasPend), nf(row.pend.Alt, row.hasPend),
		row.pend.AltRef, b2i(row.pend.GNSS), row.pend.SigmaM, b2i(row.pend.SetEffective), row.pendSince, row.pLast, reason, row.held)
	return err
}

// distM is the distance between two positions (great circle plus height).
func distM(aLat, aLon, aAlt, bLat, bLon, bAlt float64) float64 {
	const R = 6371008.8
	p1, p2 := aLat*math.Pi/180, bLat*math.Pi/180
	dp, dl := p2-p1, (bLon-aLon)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	d := 2 * R * math.Asin(math.Min(1, math.Sqrt(h)))
	return math.Hypot(d, bAlt-aAlt)
}

// FeederPosition applies, limits or holds a position announced by the
// station's feeder (see the comment at the top of this file). It returns
// the station and the decision (Pos*).
func (r *Registry) FeederPosition(ctx context.Context, idx model.StationIdx, fp FeederPos) (Station, string, error) {
	if !validLatLon(fp.Lat, fp.Lon) || (fp.Lat == 0 && fp.Lon == 0) || math.IsNaN(fp.Alt) || math.IsInf(fp.Alt, 0) {
		st, _ := r.Station(idx)
		return st, "", fmt.Errorf("%w: position", ErrInvalid)
	}
	switch fp.AltRef {
	case model.AltRefMSL, model.AltRefHAE, model.AltRefAmbiguous, "":
	default:
		st, _ := r.Station(idx)
		return st, "", fmt.Errorf("%w: altRef %q", ErrInvalid, fp.AltRef)
	}
	r.posMu.Lock() // one decision at a time (rows are read, then written)
	defer r.posMu.Unlock()
	st, ok := r.Station(idx)
	if !ok {
		return Station{}, "", ErrStationNotFound
	}
	pol := r.Policy().Positions.normalized()
	now := r.now()
	row, err := r.loadFeederPos(ctx, st.SID)
	if err != nil {
		return st, "", err
	}
	cur := st.Declared
	if fp.GNSS {
		cur = st.GNSS
	}
	// An owner-declared position (one the feeder did not set) stays.
	if !fp.GNSS && !fp.SetEffective && cur.Valid &&
		(!row.applied.Valid || row.appliedGNSS || distM(cur.Lat, cur.Lon, cur.Alt, row.applied.Lat, row.applied.Lon, row.applied.Alt) > 1) {
		return st, PosOwner, nil
	}
	if !cur.Valid {
		st, err = r.applyFeederPos(ctx, st, &row, fp, now, "first")
		return st, PosFirst, err
	}
	d := distM(cur.Lat, cur.Lon, cur.Alt, fp.Lat, fp.Lon, fp.Alt)
	if d < pol.JitterM {
		if row.hasPend { // back at the current position: forget the pending move
			row.hasPend = false
			if err := r.write(ctx, func(tx *sql.Tx) error { return r.saveFeederPos(ctx, tx, st.SID, &row) }, nil); err != nil {
				return st, "", err
			}
		}
		return st, PosIgnored, nil
	}
	reason := PosLimited
	if d >= pol.HoldBeyondM {
		reason = PosHeld
	}
	intervalOK := now.Unix()-row.lastChange >= int64(pol.MinInterval/time.Second)
	samePend := row.hasPend && row.pend.GNSS == fp.GNSS &&
		distM(row.pend.Lat, row.pend.Lon, row.pend.Alt, fp.Lat, fp.Lon, fp.Alt) < pol.JitterM
	if reason == PosLimited && intervalOK {
		st, err = r.applyFeederPos(ctx, st, &row, fp, now, "rate")
		return st, PosApplied, err
	}
	if reason == PosHeld && samePend && intervalOK && pol.HoldMax > 0 && now.Sub(row.pend.Since) >= pol.HoldMax {
		st, err = r.applyFeederPos(ctx, st, &row, fp, now, "held-expired")
		return st, PosApplied, err
	}
	if !samePend {
		row.hasPend = true
		row.pend = PendingPos{FeederPos: fp, Reason: reason}
		row.pendSince = now.Unix()
		row.held++
	} else if reason == PosHeld {
		row.pend.Reason = PosHeld
	}
	row.pLast = now.Unix()
	err = r.write(ctx, func(tx *sql.Tx) error {
		if err := r.saveFeederPos(ctx, tx, st.SID, &row); err != nil {
			return err
		}
		if samePend {
			return nil
		}
		return insertAudit(ctx, tx, now, SystemActor, "station.position."+reason, st.SID,
			fmt.Sprintf(`{"gnss":%t,"moveM":%s}`, fp.GNSS, strconv.FormatFloat(math.Round(d), 'f', 0, 64)))
	}, nil)
	return st, reason, err
}

// applyFeederPos stores fp as the station's position (and, with
// SetEffective, as its effective one) and records it as the feeder's
// latest applied position. Caller holds posMu.
func (r *Registry) applyFeederPos(ctx context.Context, st Station, row *feederPosRow, fp FeederPos, now time.Time, why string) (Station, error) {
	next, err := r.Update(ctx, st.Idx, func(s *Station) error {
		setFeederPos(s, fp)
		return nil
	})
	if err != nil {
		return st, err
	}
	row.lastChange = now.Unix()
	row.applied = Position{Lat: fp.Lat, Lon: fp.Lon, Alt: fp.Alt, Valid: true}
	row.appliedGNSS = fp.GNSS
	row.hasPend = false
	err = r.write(ctx, func(tx *sql.Tx) error {
		if err := r.saveFeederPos(ctx, tx, st.SID, row); err != nil {
			return err
		}
		if why == "first" {
			return nil
		}
		return insertAudit(ctx, tx, now, SystemActor, "station.position.applied", st.SID,
			fmt.Sprintf(`{"gnss":%t,"why":%q}`, fp.GNSS, why))
	}, nil)
	return next, err
}

// setFeederPos is the position update itself (the semantics the engine
// used for RECEIVER records: GNSS beats declared, a survey result or a
// contradiction keeps the effective position).
func setFeederPos(s *Station, fp FeederPos) {
	p := Position{Lat: fp.Lat, Lon: fp.Lon, Alt: fp.Alt, Valid: true}
	hae := fp.Alt
	if fp.AltRef == model.AltRefMSL {
		if h, ok := geoid.MSLToHAE(fp.Lat, fp.Lon, fp.Alt); ok {
			hae = h
		}
	}
	if fp.GNSS {
		s.GNSS = Position{Lat: fp.Lat, Lon: fp.Lon, Alt: hae, Valid: true}
	} else {
		s.Declared, s.DeclaredAltRef = p, fp.AltRef
	}
	if !fp.SetEffective {
		return
	}
	switch s.LocStatus {
	case model.LocConfirmed, model.LocEstimated, model.LocSuspect, model.LocEstimating:
		if s.Located {
			return // a survey result (or a contradiction) is in charge (§10.1)
		}
	}
	src, status := model.LocSrcManual, model.LocDeclared
	if fp.GNSS {
		src, status = model.LocSrcGNSS, model.LocGNSS
	}
	if s.LocStatus == model.LocGNSS && !fp.GNSS && s.Located {
		return // GNSS beats a declared position
	}
	s.Located = true
	s.Effective = model.StationGeo{Lat: fp.Lat, Lon: fp.Lon, HAE: hae, SigmaM: fp.SigmaM, Source: src}
	s.LocStatus = status
}

// PendingPosition returns the feeder position waiting to be applied, if
// any (owner and admin views; exact coordinates).
func (r *Registry) PendingPosition(ctx context.Context, idx model.StationIdx) (PendingPos, bool, error) {
	st, ok := r.Station(idx)
	if !ok {
		return PendingPos{}, false, ErrStationNotFound
	}
	row, err := r.loadFeederPos(ctx, st.SID)
	if err != nil || !row.hasPend {
		return PendingPos{}, false, err
	}
	return row.pend, true, nil
}

// ConfirmPendingPosition applies the pending feeder position now (the
// owner or the admin confirmed it). It is audited; false when nothing was
// pending.
func (r *Registry) ConfirmPendingPosition(ctx context.Context, actor Actor, idx model.StationIdx) (Station, bool, error) {
	r.posMu.Lock()
	defer r.posMu.Unlock()
	st, ok := r.Station(idx)
	if !ok {
		return Station{}, false, ErrStationNotFound
	}
	row, err := r.loadFeederPos(ctx, st.SID)
	if err != nil || !row.hasPend {
		return st, false, err
	}
	st, err = r.applyFeederPos(ctx, st, &row, row.pend.FeederPos, r.now(), "confirmed")
	if err == nil {
		err = r.Audit(ctx, actor, "station.position.confirm", st.SID, "{}")
	}
	return st, err == nil, err
}

// releaseBySurvey applies a held feeder position that the station's survey
// estimate confirms (called by SetEstimated).
func (r *Registry) releaseBySurvey(ctx context.Context, st Station) error {
	if !st.Estimated.Valid {
		return nil
	}
	r.posMu.Lock()
	defer r.posMu.Unlock()
	row, err := r.loadFeederPos(ctx, st.SID)
	if err != nil || !row.hasPend {
		return err
	}
	pol := r.Policy().Positions.normalized()
	tol := math.Max(pol.SurveyMatchM, 3*st.EstSigmaM)
	p, e := row.pend, st.Estimated
	if distM(p.Lat, p.Lon, 0, e.Lat, e.Lon, 0) > tol {
		return nil
	}
	_, err = r.applyFeederPos(ctx, st, &row, p.FeederPos, r.now(), "survey")
	return err
}

// ReleasePending applies the pending feeder positions whose rate limit has
// passed (PosLimited) or whose hold expired (PosHeld after HoldMax). Run
// calls it with the retention job; it returns the number applied.
func (r *Registry) ReleasePending(ctx context.Context) (int, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT sid FROM station_feeder_pos WHERE pend_reason <> ''`)
	if err != nil {
		return 0, err
	}
	var sids []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return 0, err
		}
		sids = append(sids, s)
	}
	rows.Close()
	pol := r.Policy().Positions.normalized()
	n := 0
	for _, sid := range sids {
		st, ok := r.BySID(sid)
		if !ok {
			continue
		}
		r.posMu.Lock()
		row, err := r.loadFeederPos(ctx, sid)
		now := r.now()
		due := err == nil && row.hasPend && now.Unix()-row.lastChange >= int64(pol.MinInterval/time.Second) &&
			(row.pend.Reason == PosLimited || (pol.HoldMax > 0 && now.Sub(row.pend.Since) >= pol.HoldMax))
		if due {
			_, err = r.applyFeederPos(ctx, st, &row, row.pend.FeederPos, now, "released")
			if err == nil {
				n++
			}
		}
		r.posMu.Unlock()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- band refs

// FrozenBandRef returns the reference position of a viewer station's peer
// distance bands for a tag period (finding PRIV-1, internal/server
// bandref.go): the position passed with the first request of the period,
// persisted so a server restart does not refreeze it. Later calls of the
// same period get the stored position whatever they pass. On a storage
// error the passed position is returned (and frozen in memory).
func (r *Registry) FrozenBandRef(ctx context.Context, sid, period string, lat, lon float64) (float64, float64) {
	r.bandMu.Lock()
	defer r.bandMu.Unlock()
	if r.bandRefs == nil {
		r.bandRefs = map[string]bandRef{}
	}
	if b, ok := r.bandRefs[sid]; ok && b.period == period {
		return b.lat, b.lon
	}
	var b bandRef
	err := r.rdb.QueryRowContext(ctx, `SELECT period, lat, lon FROM band_refs WHERE sid=?`, sid).Scan(&b.period, &b.lat, &b.lon)
	if err == nil && b.period == period {
		r.cacheBandRef(sid, b)
		return b.lat, b.lon
	}
	b = bandRef{period: period, lat: lat, lon: lon}
	r.cacheBandRef(sid, b)
	_ = r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO band_refs(sid, period, lat, lon) VALUES(?,?,?,?)
 ON CONFLICT(sid) DO UPDATE SET period=excluded.period, lat=excluded.lat, lon=excluded.lon`, sid, period, lat, lon)
		return err
	}, nil)
	return lat, lon
}

type bandRef struct {
	period   string
	lat, lon float64
}

func (r *Registry) cacheBandRef(sid string, b bandRef) {
	if len(r.bandRefs) >= 200_000 {
		clear(r.bandRefs)
	}
	r.bandRefs[sid] = b
}

// pruneBandRefs drops frozen references of other periods than keep.
func (r *Registry) pruneBandRefs(ctx context.Context, keep string) error {
	return r.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM band_refs WHERE period <> ?`, keep)
		return err
	}, nil)
}

// TagPeriod names the peer-tag period of t (the ISO week, as PeerTag).
func TagPeriod(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return strconv.Itoa(y) + "W" + strconv.Itoa(w)
}
