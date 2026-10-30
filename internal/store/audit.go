package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type AuditEntry struct {
	ID      int64
	At      time.Time
	ActorID *int64
	// The actor's current name and email ("" when deleted or a system action).
	ActorName  string
	ActorEmail string
	Action     string
	IP         string
	Details    map[string]any
}

// SetAuditKey sets the key of the audit log's hash chain (derived from the
// master key, which is not in the database: whoever can only write the
// database cannot forge entries). Without it the chain is unkeyed.
func (db *DB) SetAuditKey(key []byte) {
	k := append([]byte(nil), key...)
	db.auditKey.Store(&k)
}

func (db *DB) auditKeyBytes() []byte {
	if k := db.auditKey.Load(); k != nil {
		return *k
	}
	return nil
}

// auditHash chains an entry to the previous one (prev is nil for the first
// entry of the chain). Every field is length-prefixed, so no two different
// entries hash the same input.
func auditHash(key, prev []byte, id, at, actor int64, action, ip, details string) []byte {
	mac := hmac.New(sha256.New, key)
	var n [8]byte
	put := func(v uint64) {
		binary.BigEndian.PutUint64(n[:], v)
		mac.Write(n[:])
	}
	put(uint64(len(prev)))
	mac.Write(prev)
	put(uint64(id))
	put(uint64(at))
	put(uint64(actor))
	for _, s := range []string{action, ip, details} {
		put(uint64(len(s)))
		mac.Write([]byte(s))
	}
	return mac.Sum(nil)
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
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last int64
	var prev []byte
	err = tx.QueryRowContext(ctx, `SELECT id, hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&last, &prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id, at := last+1, time.Now().Unix()
	var actor int64
	if actorID != nil {
		actor = *actorID
	}
	hash := auditHash(db.auditKeyBytes(), prev, id, at, actor, action, ip, string(b))
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (id, at, actor_id, action, ip, details, hash) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, at, actorID, action, ip, string(b), hash); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditQuery selects entries of the audit log, newest first.
type AuditQuery struct {
	Before  int64  // only entries with a smaller ID (0: from the newest)
	ActorID int64  // only this user's (0: anyone's)
	System  bool   // only entries without a user (scheduled and automatic actions)
	Action  string // only actions with this prefix, e.g. "app." ("" for all)
	// Project: only entries about this project's resources (0: all).
	Project int64
	Limit   int
}

// AuditEntries lists entries of the audit log, newest first.
func (db *DB) AuditEntries(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	where := []string{"1"}
	var args []any
	if q.Before > 0 {
		where = append(where, "a.id < ?")
		args = append(args, q.Before)
	}
	switch {
	case q.System:
		where = append(where, "a.actor_id IS NULL")
	case q.ActorID > 0:
		where = append(where, "a.actor_id = ?")
		args = append(args, q.ActorID)
	}
	if q.Project > 0 {
		where = append(where, "json_extract(a.details, '$.project_id') = ?")
		args = append(args, q.Project)
	}
	if q.Action != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Action)
		where = append(where, `a.action LIKE ? ESCAPE '\'`)
		args = append(args, esc+"%")
	}
	args = append(args, max(q.Limit, 1))
	rows, err := db.R.QueryContext(ctx,
		`SELECT a.id, a.at, a.actor_id, COALESCE(u.name, ''), COALESCE(u.email, ''), a.action, a.ip, a.details
		 FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id
		 WHERE `+strings.Join(where, " AND ")+` ORDER BY a.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.ActorID, &e.ActorName, &e.ActorEmail, &e.Action, &e.IP, &details); err != nil {
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

// RecentAudit returns the newest entries first.
func (db *DB) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	return db.AuditEntries(ctx, AuditQuery{Limit: limit})
}

// AuditActions lists the distinct actions in the log (project > 0: in the
// entries about that project).
func (db *DB) AuditActions(ctx context.Context, project int64) ([]string, error) {
	q, args := `SELECT DISTINCT action FROM audit_log ORDER BY action`, []any{}
	if project > 0 {
		q = `SELECT DISTINCT action FROM audit_log WHERE json_extract(details, '$.project_id') = ? ORDER BY action`
		args = append(args, project)
	}
	rows, err := db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditCheck is the outcome of verifying the audit log's chain.
type AuditCheck struct {
	Entries int // in the log
	// Legacy entries were written before the chain existed (the first ones);
	// they are not protected.
	Legacy int
	// BrokenAt is the first entry that does not match its hash (changed,
	// or an entry before it was removed or changed); 0 if the chain holds.
	BrokenAt int64
	LastID   int64
}

// VerifyAudit recomputes the hash chain of the whole log.
func (db *DB) VerifyAudit(ctx context.Context) (AuditCheck, error) {
	var c AuditCheck
	tx, err := db.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, at, COALESCE(actor_id, 0), action, ip, details, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	key := db.auditKeyBytes()
	var prev []byte
	chained := false
	for rows.Next() {
		var id, at, actor int64
		var action, ip, details string
		var hash []byte
		if err := rows.Scan(&id, &at, &actor, &action, &ip, &details, &hash); err != nil {
			return c, err
		}
		c.Entries++
		c.LastID = id
		if c.BrokenAt != 0 {
			continue
		}
		if hash == nil && !chained {
			c.Legacy++
			continue
		}
		chained = true
		want := auditHash(key, prev, id, at, actor, action, ip, details)
		if !hmac.Equal(hash, want) {
			c.BrokenAt = id
			continue
		}
		prev = hash
	}
	return c, rows.Err()
}

// AuditHead is the newest signed entry of the log: its number and hash
// (0 and nil when nothing is signed yet).
func (db *DB) AuditHead(ctx context.Context) (int64, []byte, error) {
	var id int64
	var hash []byte
	err := db.R.QueryRowContext(ctx, `SELECT id, hash FROM audit_log WHERE hash IS NOT NULL ORDER BY id DESC LIMIT 1`).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return id, hash, err
}

// AuditHash is the hash of entry id; ErrNotFound if the log has no such
// entry.
func (db *DB) AuditHash(ctx context.Context, id int64) ([]byte, error) {
	var hash []byte
	err := db.R.QueryRowContext(ctx, `SELECT hash FROM audit_log WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return hash, err
}
