package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// BackupCheck is one integrity check of a repository: a server's in a
// storage, or the panel's own (ServerID 0).
type BackupCheck struct {
	ID         int64
	StorageID  int64
	ServerID   int64
	Trigger    string
	Status     string
	ReadData   string
	Error      string
	Log        string
	ActorID    *int64
	CreatedAt  time.Time
	FinishedAt *time.Time
}

var ErrCheckInProgress = errors.New("this repository is being checked already")

// CreateBackupCheck records a running check, or returns ErrCheckInProgress.
func (db *DB) CreateBackupCheck(ctx context.Context, c *BackupCheck) error {
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO backup_checks (storage_id, server_id, trigger, status, read_data, actor_id)
		 VALUES (?, NULLIF(?, 0), ?, 'running', ?, ?) RETURNING id, created_at`,
		c.StorageID, c.ServerID, c.Trigger, c.ReadData, c.ActorID).Scan(&c.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrCheckInProgress
		}
		return err
	}
	c.Status, c.CreatedAt = "running", time.Unix(created, 0)
	return nil
}

// FinishBackupCheck records the outcome of a running check.
func (db *DB) FinishBackupCheck(ctx context.Context, id int64, ok bool, errMsg, log string) error {
	status := "failed"
	if ok {
		status = "succeeded"
	}
	return db.execOne(ctx,
		`UPDATE backup_checks SET status = ?, error = ?, log = ?, finished_at = unixepoch() WHERE id = ? AND status = 'running'`,
		status, errMsg, log, id)
}

// FailUnfinishedBackupChecks marks checks cut short by a restart as failed.
func (db *DB) FailUnfinishedBackupChecks(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx,
		`UPDATE backup_checks SET status = 'failed', error = 'interrupted by a panel restart', finished_at = unixepoch()
		 WHERE status = 'running'`)
	return err
}

const checkColumns = `id, storage_id, COALESCE(server_id, 0), trigger, status, read_data, error, log, actor_id, created_at, finished_at`

func scanCheck(row interface{ Scan(...any) error }) (*BackupCheck, error) {
	var c BackupCheck
	var created int64
	var finished sql.NullInt64
	err := row.Scan(&c.ID, &c.StorageID, &c.ServerID, &c.Trigger, &c.Status, &c.ReadData, &c.Error, &c.Log, &c.ActorID,
		&created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.FinishedAt = time.Unix(created, 0), timePtr(finished)
	return &c, nil
}

func (db *DB) BackupCheck(ctx context.Context, id int64) (*BackupCheck, error) {
	return scanCheck(db.R.QueryRowContext(ctx, `SELECT `+checkColumns+` FROM backup_checks WHERE id = ?`, id))
}

// CheckTarget is a repository holding backups, with its latest check.
type CheckTarget struct {
	StorageID  int64
	ServerID   int64        // 0: the panel's repository
	ServerName string       // "" for the panel
	Last       *BackupCheck // nil: never checked
}

// BackupCheckTargets lists the repositories that hold backups (a backup
// to them succeeded) in storages that still exist, with their latest
// check.
func (db *DB) BackupCheckTargets(ctx context.Context) ([]CheckTarget, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT t.storage_id, t.server_id, COALESCE(sv.name, ''),
		        (SELECT MAX(c.id) FROM backup_checks c WHERE c.storage_id = t.storage_id AND COALESCE(c.server_id, 0) = t.server_id)
		 FROM (SELECT DISTINCT storage_id, server_id FROM backups
		       WHERE kind = 'backup' AND status = 'succeeded' AND storage_id IS NOT NULL
		       UNION SELECT DISTINCT storage_id, 0 FROM panel_backups
		       WHERE status = 'succeeded' AND storage_id IS NOT NULL) t
		 JOIN backup_storages st ON st.id = t.storage_id
		 LEFT JOIN servers sv ON sv.id = t.server_id
		 ORDER BY st.name, t.server_id != 0, sv.name`)
	if err != nil {
		return nil, err
	}
	var out []CheckTarget
	var last []sql.NullInt64
	for rows.Next() {
		var t CheckTarget
		var id sql.NullInt64
		if err := rows.Scan(&t.StorageID, &t.ServerID, &t.ServerName, &id); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
		last = append(last, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, id := range last {
		if !id.Valid {
			continue
		}
		c, err := db.BackupCheck(ctx, id.Int64)
		if err != nil {
			return nil, err
		}
		c.Log = "" // listings do not need it
		out[i].Last = c
	}
	return out, nil
}

// DeleteBackupChecksBefore forgets finished checks older than before.
func (db *DB) DeleteBackupChecksBefore(ctx context.Context, before time.Time) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM backup_checks WHERE status != 'running' AND created_at < ?`, before.Unix())
	return err
}
