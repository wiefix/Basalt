package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Replication of site databases between servers: the sealed replication
// password of a source, and how each replica copies from each source.

// ReplPassword returns a server's sealed replication password (nil: none).
func (db *DB) ReplPassword(ctx context.Context, serverID int64) ([]byte, error) {
	var b []byte
	err := db.R.QueryRowContext(ctx, `SELECT repl_password FROM servers WHERE id = ?`, serverID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// SetReplPassword stores a server's sealed replication password unless it
// has one already (two callers agree on the first).
func (db *DB) SetReplPassword(ctx context.Context, serverID int64, sealed []byte) error {
	_, err := db.W.ExecContext(ctx, `UPDATE servers SET repl_password = ? WHERE id = ? AND repl_password IS NULL`, sealed, serverID)
	return err
}

// DBReplication is how a replica copies databases from a source.
type DBReplication struct {
	ReplicaID int64
	SourceID  int64
	Running   bool
	Lag       int64 // seconds behind (-1: unknown)
	Error     string
	Databases []string
	UpdatedAt time.Time
}

// SetDBReplication records the state of a replica's copying from a source.
func (db *DB) SetDBReplication(ctx context.Context, r DBReplication) error {
	dbs, _ := json.Marshal(r.Databases)
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO db_replication (replica_id, source_id, running, lag, error, databases, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (replica_id, source_id) DO UPDATE SET running = excluded.running, lag = excluded.lag,
		 error = excluded.error, databases = excluded.databases, updated_at = excluded.updated_at`,
		r.ReplicaID, r.SourceID, r.Running, r.Lag, r.Error, string(dbs), r.UpdatedAt.Unix())
	return err
}

// DeleteDBReplication forgets a replica's copying from the sources not in
// keep.
func (db *DB) DeleteDBReplication(ctx context.Context, replicaID int64, keep []int64) error {
	rows, err := db.DBReplications(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ReplicaID != replicaID || containsID(keep, r.SourceID) {
			continue
		}
		if _, err := db.W.ExecContext(ctx, `DELETE FROM db_replication WHERE replica_id = ? AND source_id = ?`, replicaID, r.SourceID); err != nil {
			return err
		}
	}
	return nil
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// DBReplications lists every replica's copying.
func (db *DB) DBReplications(ctx context.Context) ([]DBReplication, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT replica_id, source_id, running, lag, error, databases, updated_at FROM db_replication ORDER BY replica_id, source_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DBReplication
	for rows.Next() {
		var r DBReplication
		var dbs string
		var at int64
		if err := rows.Scan(&r.ReplicaID, &r.SourceID, &r.Running, &r.Lag, &r.Error, &dbs, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(dbs), &r.Databases); err != nil {
			return nil, err
		}
		r.UpdatedAt = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
