package store

import (
	"context"
	"encoding/json"
	"time"
)

type AuditEntry struct {
	ID      int64
	At      time.Time
	ActorID *int64
	Action  string
	IP      string
	Details map[string]any
}

// Audit appends an entry to the audit log. details must never contain secrets.
func (db *DB) Audit(ctx context.Context, actorID *int64, action, ip string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = db.W.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, action, ip, details) VALUES (?, ?, ?, ?)`,
		actorID, action, ip, string(b))
	return err
}

// RecentAudit returns the newest entries first.
func (db *DB) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, at, actor_id, action, ip, details FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.ActorID, &e.Action, &e.IP, &details); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
