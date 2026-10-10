package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Settings of panel backups.
const (
	settingPanelPolicy = "panel_backup_policy"
	settingRecoveryKey = "recovery_key"
	settingRecoveryAt  = "recovery_key_set_at"
	settingPanelPrune  = "panel_backup_pruned_at"
)

// SetSetting stores value under key.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (db *DB) deleteSetting(ctx context.Context, key string) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// RecoveryKey returns the master key encrypted with the recovery
// passphrase and when it was set, or ErrNotFound.
func (db *DB) RecoveryKey(ctx context.Context) ([]byte, time.Time, error) {
	v, err := db.Setting(ctx, settingRecoveryKey)
	if err != nil {
		return nil, time.Time{}, err
	}
	var at time.Time
	if s, err := db.Setting(ctx, settingRecoveryAt); err == nil {
		at, _ = time.Parse(time.RFC3339, s)
	}
	return []byte(v), at, nil
}

func (db *DB) SetRecoveryKey(ctx context.Context, wrapped []byte, at time.Time) error {
	if err := db.SetSetting(ctx, settingRecoveryKey, string(wrapped)); err != nil {
		return err
	}
	return db.SetSetting(ctx, settingRecoveryAt, at.UTC().Format(time.RFC3339))
}

// panelPolicy is how the panel's policy is stored.
type panelPolicy struct {
	StorageID   int64     `json:"storage_id"`
	Schedule    string    `json:"schedule"`
	AtMinute    int       `json:"at_minute"`
	KeepLast    int       `json:"keep_last"`
	KeepDaily   int       `json:"keep_daily"`
	KeepWeekly  int       `json:"keep_weekly"`
	KeepMonthly int       `json:"keep_monthly"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PanelBackupPolicy returns the policy for backups of the panel, or
// ErrNotFound. LastScheduled is filled from the history.
func (db *DB) PanelBackupPolicy(ctx context.Context) (*BackupPolicy, error) {
	v, err := db.Setting(ctx, settingPanelPolicy)
	if err != nil {
		return nil, err
	}
	var p panelPolicy
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return nil, err
	}
	out := &BackupPolicy{StorageID: p.StorageID, Schedule: p.Schedule, AtMinute: p.AtMinute, KeepLast: p.KeepLast,
		KeepDaily: p.KeepDaily, KeepWeekly: p.KeepWeekly, KeepMonthly: p.KeepMonthly, UpdatedAt: p.UpdatedAt}
	var last int64
	if err := db.R.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(created_at), 0) FROM panel_backups WHERE trigger = 'schedule'`).Scan(&last); err != nil {
		return nil, err
	}
	if last > 0 {
		out.LastScheduled = time.Unix(last, 0)
	}
	return out, nil
}

func (db *DB) SetPanelBackupPolicy(ctx context.Context, p *BackupPolicy, now time.Time) error {
	b, _ := json.Marshal(panelPolicy{StorageID: p.StorageID, Schedule: p.Schedule, AtMinute: p.AtMinute,
		KeepLast: p.KeepLast, KeepDaily: p.KeepDaily, KeepWeekly: p.KeepWeekly, KeepMonthly: p.KeepMonthly, UpdatedAt: now.UTC()})
	return db.SetSetting(ctx, settingPanelPolicy, string(b))
}

func (db *DB) DeletePanelBackupPolicy(ctx context.Context) error {
	return db.deleteSetting(ctx, settingPanelPolicy)
}

// PanelBackup is one backup of the panel.
type PanelBackup struct {
	ID         int64
	StorageID  int64
	Trigger    string
	Status     string
	Error      string
	Log        string
	Snapshot   string
	Bytes      uint64
	ActorID    *int64
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// CreatePanelBackup records a running backup of the panel, or returns
// ErrBackupInProgress.
func (db *DB) CreatePanelBackup(ctx context.Context, b *PanelBackup) error {
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO panel_backups (storage_id, trigger, status, actor_id) VALUES (?, ?, 'running', ?) RETURNING id, created_at`,
		b.StorageID, b.Trigger, b.ActorID).Scan(&b.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrBackupInProgress
		}
		return err
	}
	b.Status, b.CreatedAt = "running", time.Unix(created, 0)
	return nil
}

func (db *DB) FinishPanelBackup(ctx context.Context, id int64, ok bool, errMsg, log, snapshot string, bytes uint64) error {
	status := "failed"
	if ok {
		status = "succeeded"
	}
	return db.execOne(ctx,
		`UPDATE panel_backups SET status = ?, error = ?, log = ?, snapshot = ?, bytes = ?, finished_at = unixepoch()
		 WHERE id = ? AND status = 'running'`, status, errMsg, log, snapshot, bytes, id)
}

const panelBackupColumns = `id, COALESCE(storage_id, 0), trigger, status, error, log, snapshot, bytes, actor_id, created_at, finished_at`

func scanPanelBackup(row interface{ Scan(...any) error }) (*PanelBackup, error) {
	var b PanelBackup
	var created int64
	var finished sql.NullInt64
	err := row.Scan(&b.ID, &b.StorageID, &b.Trigger, &b.Status, &b.Error, &b.Log, &b.Snapshot, &b.Bytes, &b.ActorID, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.CreatedAt, b.FinishedAt = time.Unix(created, 0), timePtr(finished)
	return &b, nil
}

func (db *DB) PanelBackups(ctx context.Context, limit int) ([]*PanelBackup, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+panelBackupColumns+` FROM panel_backups ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PanelBackup
	for rows.Next() {
		b, err := scanPanelBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (db *DB) PanelBackupByID(ctx context.Context, id int64) (*PanelBackup, error) {
	return scanPanelBackup(db.R.QueryRowContext(ctx, `SELECT `+panelBackupColumns+` FROM panel_backups WHERE id = ?`, id))
}

// FailUnfinishedPanelBackups marks runs cut short by a restart as failed.
func (db *DB) FailUnfinishedPanelBackups(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx,
		`UPDATE panel_backups SET status = 'failed', error = 'interrupted by a panel restart', finished_at = unixepoch()
		 WHERE status = 'running'`)
	return err
}

func (db *DB) PanelLastPrune(ctx context.Context) time.Time {
	v, err := db.Setting(ctx, settingPanelPrune)
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

func (db *DB) SetPanelPruned(ctx context.Context, at time.Time) error {
	return db.SetSetting(ctx, settingPanelPrune, at.UTC().Format(time.RFC3339))
}

// SnapshotTo writes a consistent copy of the database to path (which must
// not exist), for backups.
func (db *DB) SnapshotTo(ctx context.Context, path string) error {
	_, err := db.W.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
