package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
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
	Hash       []byte // nil: written before the chain existed
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
	After   int64  // only entries with a larger ID
	Oldest  bool   // oldest first (for exports)
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
	if q.After > 0 {
		where = append(where, "a.id > ?")
		args = append(args, q.After)
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
	order := "DESC"
	if q.Oldest {
		order = "ASC"
	}
	rows, err := db.R.QueryContext(ctx,
		`SELECT a.id, a.at, a.actor_id, COALESCE(u.name, ''), COALESCE(u.email, ''), a.action, a.ip, a.details, a.hash
		 FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id
		 WHERE `+strings.Join(where, " AND ")+` ORDER BY a.id `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&e.ID, &at, &e.ActorID, &e.ActorName, &e.ActorEmail, &e.Action, &e.IP, &details, &e.Hash); err != nil {
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
	// Pruned entries were removed after the retention period: the chain
	// starts at FirstID (0 when nothing was removed).
	Pruned   int
	PrunedAt time.Time
	FirstID  int64
}

// VerifyAudit recomputes the hash chain of the whole log.
func (db *DB) VerifyAudit(ctx context.Context) (AuditCheck, error) {
	var c AuditCheck
	tx, err := db.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	key := db.auditKeyBytes()
	base, err := auditBaseTx(ctx, tx)
	if err != nil {
		return c, err
	}
	var prev []byte
	chained := false
	if base != nil {
		c.Pruned, c.PrunedAt, c.FirstID = base.pruned, base.prunedAt, base.firstID
		prev, chained = base.prevHash, base.prevHash != nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, at, COALESCE(actor_id, 0), action, ip, details, hash FROM audit_log ORDER BY id`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	first := true
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
		// After pruning the log starts exactly at its base, which must be
		// signed: removing more entries, or keeping an older base, shows.
		if first && base != nil && (id != base.firstID || !hmac.Equal(base.mac, auditBaseMAC(key, base.firstID, base.prevHash))) {
			c.BrokenAt = id
			continue
		}
		first = false
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

// auditBase is where the chain starts after old entries were pruned.
type auditBase struct {
	firstID  int64
	prevHash []byte
	pruned   int
	prunedAt time.Time
	mac      []byte
}

// auditBaseMAC signs a base: the first entry kept and the hash it chains to.
func auditBaseMAC(key []byte, firstID int64, prev []byte) []byte {
	return auditHash(key, prev, firstID, 0, 0, "audit.base", "", "")
}

func auditBaseTx(ctx context.Context, q querier) (*auditBase, error) {
	var b auditBase
	var at int64
	err := q.QueryRowContext(ctx, `SELECT first_id, prev_hash, pruned, pruned_at, mac FROM audit_log_base WHERE id = 1`).
		Scan(&b.firstID, &b.prevHash, &b.pruned, &at, &b.mac)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.prunedAt = time.Unix(at, 0)
	return &b, nil
}

// AuditFirstID is the oldest entry in the log (0 when it is empty).
func (db *DB) AuditFirstID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := db.R.QueryRowContext(ctx, `SELECT MIN(id) FROM audit_log`).Scan(&id)
	return id.Int64, err
}

// auditPruneBatch is how many entries one transaction removes, so pruning
// a large log does not hold the write lock for long (a variable for tests).
var auditPruneBatch = 10_000

// auditNoDelete is the trigger of migration 0029 that keeps entries from
// being removed; pruning lifts it inside its transaction.
const auditNoDelete = `CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'the audit log is append-only');
END`

// PruneAudit removes the entries written before cutoff, never the newest
// one (the next entry chains to it), and moves the chain's base to the
// oldest entry kept. It returns how many were removed and the last one
// removed.
func (db *DB) PruneAudit(ctx context.Context, cutoff time.Time) (int, int64, error) {
	total, through := 0, int64(0)
	for {
		n, last, err := db.pruneAuditBatch(ctx, cutoff)
		if err != nil {
			return total, through, err
		}
		total += n
		if n == 0 {
			return total, through, nil
		}
		through = last
	}
}

func (db *DB) pruneAuditBatch(ctx context.Context, cutoff time.Time) (int, int64, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	// The newest entry to remove: written before the cutoff, not the last
	// one, and at most a batch from the start.
	var cut sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(id) FROM (SELECT id FROM audit_log
		  WHERE at < ? AND id < (SELECT MAX(id) FROM audit_log) ORDER BY id LIMIT ?)`,
		cutoff.Unix(), auditPruneBatch).Scan(&cut); err != nil {
		return 0, 0, err
	}
	if !cut.Valid {
		return 0, 0, nil
	}
	var prev []byte
	var first int64
	if err := tx.QueryRowContext(ctx, `SELECT hash FROM audit_log WHERE id = ?`, cut.Int64).Scan(&prev); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT MIN(id) FROM audit_log WHERE id > ?`, cut.Int64).Scan(&first); err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER audit_log_no_delete`); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE id <= ?`, cut.Int64)
	if err != nil {
		return 0, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, auditNoDelete); err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log_base (id, first_id, prev_hash, pruned, pruned_at, mac) VALUES (1, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET first_id = excluded.first_id, prev_hash = excluded.prev_hash,
		   pruned = audit_log_base.pruned + excluded.pruned, pruned_at = excluded.pruned_at, mac = excluded.mac`,
		first, prev, n, time.Now().Unix(), auditBaseMAC(db.auditKeyBytes(), first, prev)); err != nil {
		return 0, 0, err
	}
	return int(n), cut.Int64, tx.Commit()
}

// settingAuditRetention is how many days audit entries are kept (absent
// or 0: forever).
const settingAuditRetention = "audit_retention_days"

// AuditRetention is how many days audit entries are kept (0: forever).
func (db *DB) AuditRetention(ctx context.Context) (int, error) {
	v, err := db.Setting(ctx, settingAuditRetention)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// SetAuditRetention sets how many days audit entries are kept (0: forever).
func (db *DB) SetAuditRetention(ctx context.Context, days int) error {
	return db.SetSetting(ctx, settingAuditRetention, strconv.Itoa(days))
}
