package registry

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
)

// Actor is who performed an audited action.
type Actor struct {
	ID   int64  // account ID (0 = the system)
	Name string // user name at the time ("" = look up nothing; shown as #ID)
}

type actorNameKey struct{}

// WithActorName returns ctx carrying the account name of the person acting,
// for registry calls that take only an account ID (claims): their audit
// entries then record the name too (UX-08).
func WithActorName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, actorNameKey{}, name)
}

// ownerActor is the audit actor of an owner acting through an ID-only call.
func ownerActor(ctx context.Context, ownerID int64) Actor {
	name, _ := ctx.Value(actorNameKey{}).(string)
	return Actor{ID: ownerID, Name: name}
}

// SystemActor is the registry itself (automatic admission, retention).
var SystemActor = Actor{Name: "system"}

// AuditEntry is one audit log row.
//
// Detail is a short JSON object. Callers must not put coordinates, IP
// addresses, tokens or codes into it (DESIGN §8.4 "Logs"); station
// identities go into SID so that deletion can pseudonymise them.
type AuditEntry struct {
	ID      int64
	T       time.Time
	ActorID int64
	Actor   string
	Action  string
	SID     string
	Detail  string
}

const maxDetail = 2048

func insertAudit(ctx context.Context, tx *sql.Tx, now time.Time, a Actor, action, sid, detail string) error {
	if len(detail) > maxDetail {
		detail = detail[:maxDetail]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit(t, actor_id, actor, action, sid, detail) VALUES(?,?,?,?,?,?)`,
		now.Unix(), a.ID, a.Name, action, sid, detail)
	return err
}

// Audit records an action, for example an admin's exact-location reveal
// ("station.reveal") or a privacy setting change. See AuditEntry for what
// Detail may contain.
func (r *Registry) Audit(ctx context.Context, a Actor, action, sid, detail string) error {
	return r.write(ctx, func(tx *sql.Tx) error {
		return insertAudit(ctx, tx, r.now(), a, action, sid, detail)
	}, nil)
}

// AuditFilter selects audit entries. Zero fields do not filter.
type AuditFilter struct {
	SID     string
	ActorID int64
	// ActionPrefix matches actions starting with it ("station.").
	ActionPrefix string
	Since        time.Time
	// Before pages backwards by ID (0 = newest).
	Before int64
	Limit  int // default 100, max 1000
}

// AuditLog returns matching entries, newest first.
func (r *Registry) AuditLog(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	var where []string
	var args []any
	if f.SID != "" {
		where, args = append(where, "sid=?"), append(args, strings.ToLower(f.SID))
	}
	if f.ActorID != 0 {
		where, args = append(where, "actor_id=?"), append(args, f.ActorID)
	}
	if f.ActionPrefix != "" {
		where, args = append(where, "substr(action, 1, ?)=?"), append(args, len(f.ActionPrefix), f.ActionPrefix)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "t>=?"), append(args, f.Since.Unix())
	}
	if f.Before > 0 {
		where, args = append(where, "id<?"), append(args, f.Before)
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	f.Limit = min(f.Limit, 1000)
	q := `SELECT id, t, actor_id, actor, action, sid, detail FROM audit`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)
	rows, err := r.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.ActorID, &e.Actor, &e.Action, &e.SID, &e.Detail); err != nil {
			return nil, err
		}
		e.T = fromS(t)
		out = append(out, e)
	}
	return out, rows.Err()
}
