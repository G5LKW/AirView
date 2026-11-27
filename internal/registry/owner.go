package registry

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"airview/internal/model"
)

// ---------------------------------------------------------------- estimates

// Estimate is one self-location result (location_estimates, DESIGN §6.6).
type Estimate struct {
	ID     int64
	Idx    model.StationIdx
	T      time.Time
	Method string // "survey", "locateme", "gnss", "declared-check", …
	Lat    float64
	Lon    float64
	HAE    float64
	SigmaE float64 // 1-sigma east / north / up, metres
	SigmaN float64
	SigmaU float64
	N      int    // observations used
	Status string // e.g. "stable", "unstable", "suspect"
}

// AddEstimate stores a location estimate and keeps only the latest
// Retention.EstimatesPerMethod of its method for the station.
func (r *Registry) AddEstimate(ctx context.Context, e Estimate) (int64, error) {
	if !validLatLon(e.Lat, e.Lon) || e.Method == "" || len(e.Method) > 32 {
		return 0, fmt.Errorf("%w: estimate", ErrInvalid)
	}
	if st, ok := r.Station(e.Idx); !ok || st.Deleting {
		return 0, ErrStationNotFound
	}
	if e.T.IsZero() {
		e.T = r.now()
	}
	keep := r.Policy().Retention.EstimatesPerMethod
	var id int64
	err := r.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO location_estimates(idx, t, method, lat, lon, hae, sigma_e, sigma_n, sigma_u, n, status)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.Idx, e.T.Unix(), e.Method, e.Lat, e.Lon, e.HAE, e.SigmaE, e.SigmaN, e.SigmaU, e.N, e.Status)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM location_estimates WHERE id IN (
 SELECT id FROM location_estimates WHERE idx=? AND method=? ORDER BY t DESC, id DESC LIMIT -1 OFFSET ?)`, e.Idx, e.Method, keep)
		return err
	}, nil)
	return id, err
}

// Estimates returns a station's estimates, newest first.
func (r *Registry) Estimates(ctx context.Context, idx model.StationIdx) ([]Estimate, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT id, idx, t, method, lat, lon, hae, sigma_e, sigma_n, sigma_u, n, status
FROM location_estimates WHERE idx=? ORDER BY t DESC, id DESC`, idx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Estimate
	for rows.Next() {
		var e Estimate
		var t int64
		if err := rows.Scan(&e.ID, &e.Idx, &t, &e.Method, &e.Lat, &e.Lon, &e.HAE, &e.SigmaE, &e.SigmaN, &e.SigmaU, &e.N, &e.Status); err != nil {
			return nil, err
		}
		e.T = fromS(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- owners

// OwnerStations returns the stations an account owns, ordered by idx.
func (r *Registry) OwnerStations(ownerID int64) []Station {
	if ownerID == 0 {
		return nil
	}
	return r.List(Filter{OwnerID: ownerID})
}

// Owns reports whether ownerID owns the station sid.
func (r *Registry) Owns(ownerID int64, sid string) bool {
	st, ok := r.BySID(sid)
	return ok && ownerID != 0 && st.OwnerID == ownerID
}

// DeleteOwner erases an owner (GDPR erasure, DESIGN §8.5 "Owner delete:
// immediate"): with deleteStations the owned stations are queued for
// deletion (their statistics, locations, logs and keys are removed by the
// deletion job), otherwise they are released (unclaimed). Then the account
// and its sessions are deleted and its audit entries pseudonymised.
func (r *Registry) DeleteOwner(ctx context.Context, actor Actor, ownerID int64, deleteStations bool) error {
	if _, err := r.Accounts().ByID(ctx, ownerID); err != nil {
		return err
	}
	if deleteStations {
		for _, st := range r.OwnerStations(ownerID) {
			if st.Idx == model.HostStation {
				continue // the host station is the operator's
			}
			if err := r.DeleteStation(ctx, actor, st.Idx, "owner erasure"); err != nil {
				return err
			}
		}
	}
	return r.Accounts().Delete(ctx, ownerID)
}

// ---------------------------------------------------------------- export

// exportStation is the owner-facing JSON of a station (own data, exact).
type exportStation struct {
	SID            string    `json:"sid"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	FeederID       string    `json:"feederId,omitempty"`
	RxNo           int       `json:"rxNo"`
	UUID           string    `json:"uuid,omitempty"`
	ClockClass     string    `json:"clockClass"`
	Declared       *posJSON  `json:"declared,omitempty"`
	DeclaredAltRef string    `json:"declaredAltRef,omitempty"`
	GNSS           *posJSON  `json:"gnss,omitempty"`
	Estimated      *posJSON  `json:"estimated,omitempty"`
	EstSigmaM      float64   `json:"estimatedSigmaM,omitempty"`
	Effective      *posJSON  `json:"effective,omitempty"`
	EffSigmaM      float64   `json:"effectiveSigmaM,omitempty"`
	EffSource      string    `json:"effectiveSource,omitempty"`
	LocStatus      string    `json:"locationStatus"`
	Pin            string    `json:"pin"`
	PublicPin      *posJSON  `json:"publicPin,omitempty"`
	ShowName       bool      `json:"showName"`
	PublicStats    bool      `json:"publicStats"`
	Admission      string    `json:"admission"`
	Created        time.Time `json:"created"`
	LastSeen       time.Time `json:"lastSeen,omitzero"`
}

type posJSON struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	Alt float64 `json:"alt"`
}

func pj(p Position) *posJSON {
	if !p.Valid {
		return nil
	}
	return &posJSON{p.Lat, p.Lon, p.Alt}
}

func toExport(st Station) exportStation {
	e := exportStation{SID: st.SID, Name: st.Name, Kind: st.Kind, FeederID: st.FeederID, RxNo: st.RxNo, UUID: st.UUID,
		ClockClass: st.ClockCls.String(), Declared: pj(st.Declared), DeclaredAltRef: st.DeclaredAltRef,
		GNSS: pj(st.GNSS), Estimated: pj(st.Estimated), LocStatus: st.LocStatus, Pin: st.Pin,
		ShowName: st.ShowName, PublicStats: st.PublicStats, Admission: st.Admission,
		Created: st.Created, LastSeen: st.LastSeen}
	if st.Estimated.Valid {
		e.EstSigmaM = st.EstSigmaM
	}
	if st.Located {
		e.Effective = &posJSON{st.Effective.Lat, st.Effective.Lon, st.Effective.HAE}
		e.EffSigmaM, e.EffSource = st.Effective.SigmaM, st.Effective.Source
	}
	if st.HasPublic {
		e.PublicPin = &posJSON{Lat: st.Public.Lat, Lon: st.Public.Lon}
	}
	return e
}

// ExportOwner writes an owner's data as a ZIP of JSON files (GDPR access /
// portability, DESIGN §8.5): the account (without secrets), sessions
// (without identifiers), the owned stations with exact locations, their
// location estimates and connection log, and the audit entries about the
// account or its stations. Options.ExportExtra may add files (statistics).
func (r *Registry) ExportOwner(ctx context.Context, ownerID int64, w io.Writer) error {
	acct, err := r.Accounts().ByID(ctx, ownerID)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	add := func(name string, v any) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: r.now()})
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	now := r.now().UTC()
	if err := add("README.json", map[string]any{
		"exported": now,
		"contents": "Your AirView network account and station data. Coordinates are exact; keep this file private.",
	}); err != nil {
		return err
	}
	if err := add("account.json", map[string]any{
		"id": acct.ID, "username": acct.Username, "role": acct.Role.String(), "created": acct.Created,
		"lastLogin": acct.LastLogin, "totpEnabled": acct.TOTPEnabled(), "disabled": acct.Disabled,
	}); err != nil {
		return err
	}
	sess, err := r.Sessions().List(ctx, ownerID)
	if err != nil {
		return err
	}
	type sj struct {
		Scope    string    `json:"scope"`
		Created  time.Time `json:"created"`
		LastSeen time.Time `json:"lastSeen"`
		Expires  time.Time `json:"expires"`
		MFA      bool      `json:"mfa"`
	}
	sl := []sj{}
	for _, s := range sess {
		sl = append(sl, sj{s.Scope.String(), s.Created, s.LastSeen, s.Expires, s.MFA})
	}
	if err := add("sessions.json", sl); err != nil {
		return err
	}
	stations := r.OwnerStations(ownerID)
	es := []exportStation{}
	ests := map[string][]Estimate{}
	conns := map[string][]ConnEntry{}
	for _, st := range stations {
		es = append(es, toExport(st))
		e, err := r.Estimates(ctx, st.Idx)
		if err != nil {
			return err
		}
		ests[st.SID] = e
		// History starts at the current owner's assignment (a re-claimed
		// station must not export the previous owner's connections).
		c, err := r.ConnLog(ctx, st.Idx, st.OwnedSince, 100000)
		if err != nil {
			return err
		}
		conns[st.SID] = c
	}
	if err := add("stations.json", es); err != nil {
		return err
	}
	if err := add("location_estimates.json", ests); err != nil {
		return err
	}
	if err := add("connections.json", conns); err != nil {
		return err
	}
	var audit []AuditEntry
	seen := map[int64]bool{}
	collect := func(f AuditFilter) error {
		for {
			f.Limit = 1000
			list, err := r.AuditLog(ctx, f)
			if err != nil {
				return err
			}
			for _, e := range list {
				if !seen[e.ID] {
					seen[e.ID] = true
					audit = append(audit, e)
				}
			}
			if len(list) < 1000 {
				return nil
			}
			f.Before = list[len(list)-1].ID
		}
	}
	if err := collect(AuditFilter{ActorID: ownerID}); err != nil {
		return err
	}
	since := map[string]time.Time{}
	for _, st := range stations {
		since[st.SID] = st.OwnedSince
		if err := collect(AuditFilter{SID: st.SID}); err != nil {
			return err
		}
	}
	audit = ownerAudit(audit, ownerID, since)
	if err := add("audit.json", audit); err != nil {
		return err
	}
	if r.opts.ExportExtra != nil {
		if err := r.opts.ExportExtra(ctx, ownerID, stations, add); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ownerAudit keeps the audit entries an owner may see in their export: the
// owner's own actions, and the entries about their stations from the
// current assignment on (since by SID). Other actors (admins, a previous
// owner) are not identified: their account id, name and the detail
// (which can carry account ids) are removed.
func ownerAudit(in []AuditEntry, ownerID int64, since map[string]time.Time) []AuditEntry {
	out := []AuditEntry{}
	for _, e := range in {
		if e.ActorID == ownerID {
			out = append(out, e)
			continue
		}
		t, ok := since[e.SID]
		if !ok || e.T.Before(t) {
			continue
		}
		e.ActorID, e.Actor, e.Detail = 0, "", ""
		out = append(out, e)
	}
	return out
}
