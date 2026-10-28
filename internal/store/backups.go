package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// StorageConfig is where a backup storage is, without its secrets.
type StorageConfig struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	User      string `json:"user,omitempty"`
	Path      string `json:"path"`
}

type BackupStorage struct {
	ID             int64
	Name           string
	Kind           string // s3, sftp, local
	Config         StorageConfig
	SecretSealed   []byte // S3 secret key or SSH private key
	SSHPublicKey   string
	PasswordSealed []byte
	CreatedAt      time.Time
	Policies       int // policies using it
}

var (
	ErrStorageInUse       = errors.New("the storage is used by backup policies")
	ErrBackupInProgress   = errors.New("a backup or restore of this resource is already running")
	errStorageWithoutSeal = errors.New("storage secrets missing")
)

// Sealer encrypts a new storage's secret and password, which are bound to
// its ID.
type Sealer func(id int64) (secret, password []byte)

// CreateBackupStorage stores a storage; seal encrypts its secrets once its
// ID is known.
func (db *DB) CreateBackupStorage(ctx context.Context, st *BackupStorage, seal Sealer) error {
	cfg, _ := json.Marshal(st.Config)
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO backup_storages (name, kind, config, ssh_public_key) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		st.Name, st.Kind, string(cfg), st.SSHPublicKey).Scan(&st.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrNameTaken
		}
		return err
	}
	st.SecretSealed, st.PasswordSealed = seal(st.ID)
	if st.PasswordSealed == nil {
		return errStorageWithoutSeal
	}
	if _, err := tx.ExecContext(ctx, `UPDATE backup_storages SET secret = ?, password = ? WHERE id = ?`,
		st.SecretSealed, st.PasswordSealed, st.ID); err != nil {
		return err
	}
	st.CreatedAt = time.Unix(created, 0)
	return tx.Commit()
}

// UpdateBackupStorage changes a storage's name, location and (if not nil)
// secret. The kind and password never change.
func (db *DB) UpdateBackupStorage(ctx context.Context, st *BackupStorage) error {
	cfg, _ := json.Marshal(st.Config)
	err := db.execOne(ctx, `UPDATE backup_storages SET name = ?, config = ?, secret = COALESCE(?, secret) WHERE id = ?`,
		st.Name, string(cfg), st.SecretSealed, st.ID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrNameTaken
	}
	return err
}

const storageQuery = `SELECT s.id, s.name, s.kind, s.config, s.secret, s.ssh_public_key, s.password, s.created_at,
	(SELECT COUNT(*) FROM backup_policies p WHERE p.storage_id = s.id) FROM backup_storages s`

func scanStorage(row interface{ Scan(...any) error }) (*BackupStorage, error) {
	var st BackupStorage
	var cfg string
	var created int64
	err := row.Scan(&st.ID, &st.Name, &st.Kind, &cfg, &st.SecretSealed, &st.SSHPublicKey, &st.PasswordSealed, &created, &st.Policies)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st.CreatedAt = time.Unix(created, 0)
	return &st, json.Unmarshal([]byte(cfg), &st.Config)
}

func (db *DB) BackupStorages(ctx context.Context) ([]*BackupStorage, error) {
	rows, err := db.R.QueryContext(ctx, storageQuery+` ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BackupStorage
	for rows.Next() {
		st, err := scanStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (db *DB) BackupStorageByID(ctx context.Context, id int64) (*BackupStorage, error) {
	return scanStorage(db.R.QueryRowContext(ctx, storageQuery+` WHERE s.id = ?`, id))
}

// DeleteBackupStorage removes a storage no policy uses. The backups in it
// are left alone.
func (db *DB) DeleteBackupStorage(ctx context.Context, id int64) error {
	err := db.execOne(ctx, `DELETE FROM backup_storages WHERE id = ?`, id)
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		return ErrStorageInUse
	}
	return err
}

// Schedules.
const (
	ScheduleManual = "manual"
	ScheduleHourly = "hourly"
	ScheduleDaily  = "daily"
)

// BackupPolicy says where and when a site or app is backed up.
type BackupPolicy struct {
	ID                                           int64
	SiteID                                       int64 // or AppID
	AppID                                        int64
	ServerID                                     int64 // of the resource
	StorageID                                    int64
	Schedule                                     string
	AtMinute                                     int // daily: minute of the day (UTC); hourly: minute of the hour
	KeepLast, KeepDaily, KeepWeekly, KeepMonthly int
	UpdatedAt                                    time.Time
	// LastScheduled is when the last scheduled backup started (zero if never).
	LastScheduled time.Time
}

const policyQuery = `SELECT p.id, COALESCE(p.site_id, 0), COALESCE(p.app_id, 0), COALESCE(s.server_id, a.server_id),
	p.storage_id, p.schedule, p.at_minute, p.keep_last, p.keep_daily, p.keep_weekly, p.keep_monthly, p.updated_at,
	COALESCE((SELECT MAX(b.created_at) FROM backups b WHERE b.kind = 'backup' AND b.trigger = 'schedule'
		AND (b.site_id = p.site_id OR b.app_id = p.app_id)), 0)
	FROM backup_policies p LEFT JOIN sites s ON s.id = p.site_id LEFT JOIN apps a ON a.id = p.app_id`

func scanPolicy(row interface{ Scan(...any) error }) (*BackupPolicy, error) {
	var p BackupPolicy
	var updated, last int64
	err := row.Scan(&p.ID, &p.SiteID, &p.AppID, &p.ServerID, &p.StorageID, &p.Schedule, &p.AtMinute,
		&p.KeepLast, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &updated, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.UpdatedAt = time.Unix(updated, 0)
	if last > 0 {
		p.LastScheduled = time.Unix(last, 0)
	}
	return &p, nil
}

// BackupTarget is what a policy or run belongs to: a site or an app.
type BackupTarget struct {
	SiteID int64
	AppID  int64
}

// where selects rows of the target in a table with site_id and app_id.
func (t BackupTarget) where(alias string) (string, int64) {
	if t.SiteID > 0 {
		return alias + "site_id = ?", t.SiteID
	}
	return alias + "app_id = ?", t.AppID
}

// BackupPolicy returns the target's policy, or ErrNotFound.
func (db *DB) BackupPolicy(ctx context.Context, t BackupTarget) (*BackupPolicy, error) {
	cond, id := t.where("p.")
	return scanPolicy(db.R.QueryRowContext(ctx, policyQuery+` WHERE `+cond, id))
}

// ScheduledBackupPolicies lists the policies that run on a schedule.
func (db *DB) ScheduledBackupPolicies(ctx context.Context) ([]*BackupPolicy, error) {
	rows, err := db.R.QueryContext(ctx, policyQuery+` WHERE p.schedule != 'manual' ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BackupPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetBackupPolicy creates or replaces the policy of p's site or app.
func (db *DB) SetBackupPolicy(ctx context.Context, p *BackupPolicy) error {
	key := "site_id"
	if p.SiteID == 0 {
		key = "app_id"
	}
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO backup_policies (site_id, app_id, storage_id, schedule, at_minute, keep_last, keep_daily, keep_weekly, keep_monthly)
		 VALUES (NULLIF(?, 0), NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (`+key+`) DO UPDATE SET storage_id = excluded.storage_id, schedule = excluded.schedule,
		 at_minute = excluded.at_minute, keep_last = excluded.keep_last, keep_daily = excluded.keep_daily,
		 keep_weekly = excluded.keep_weekly, keep_monthly = excluded.keep_monthly, updated_at = unixepoch()`,
		p.SiteID, p.AppID, p.StorageID, p.Schedule, p.AtMinute, p.KeepLast, p.KeepDaily, p.KeepWeekly, p.KeepMonthly)
	return err
}

func (db *DB) DeleteBackupPolicy(ctx context.Context, t BackupTarget) error {
	cond, id := t.where("")
	return db.execOne(ctx, `DELETE FROM backup_policies WHERE `+cond, id)
}

// Backup is one backup or restore run.
type Backup struct {
	ID         int64
	SiteID     int64
	AppID      int64
	ServerID   int64
	StorageID  int64 // 0 if the storage was deleted
	Kind       string
	Trigger    string
	Status     string
	Error      string
	Log        string
	Snapshots  json.RawMessage
	Bytes      uint64
	DataAdded  uint64
	ActorID    *int64
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// CreateBackup records a running backup or restore, or returns
// ErrBackupInProgress.
func (db *DB) CreateBackup(ctx context.Context, b *Backup) error {
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO backups (site_id, app_id, server_id, storage_id, kind, trigger, status, actor_id)
		 VALUES (NULLIF(?, 0), NULLIF(?, 0), ?, NULLIF(?, 0), ?, ?, 'running', ?) RETURNING id, created_at`,
		b.SiteID, b.AppID, b.ServerID, b.StorageID, b.Kind, b.Trigger, b.ActorID).Scan(&b.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrBackupInProgress
		}
		return err
	}
	b.Status, b.CreatedAt = "running", time.Unix(created, 0)
	return nil
}

// FinishBackup records the outcome of a running backup or restore.
func (db *DB) FinishBackup(ctx context.Context, id int64, ok bool, errMsg, log string, snapshots any, bytes, added uint64) error {
	status := "failed"
	if ok {
		status = "succeeded"
	}
	snaps, err := json.Marshal(snapshots)
	if err != nil || string(snaps) == "null" {
		snaps = []byte("[]")
	}
	return db.execOne(ctx,
		`UPDATE backups SET status = ?, error = ?, log = ?, snapshots = ?, bytes = ?, data_added = ?, finished_at = unixepoch()
		 WHERE id = ? AND status = 'running'`, status, errMsg, log, string(snaps), bytes, added, id)
}

// FailUnfinishedBackups marks runs cut short by a panel restart as failed.
func (db *DB) FailUnfinishedBackups(ctx context.Context) (int64, error) {
	res, err := db.W.ExecContext(ctx,
		`UPDATE backups SET status = 'failed', error = 'interrupted by a panel restart', finished_at = unixepoch()
		 WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const backupColumns = `id, COALESCE(site_id, 0), COALESCE(app_id, 0), server_id, COALESCE(storage_id, 0), kind, trigger,
	status, error, log, snapshots, bytes, data_added, actor_id, created_at, finished_at`

func scanBackup(row interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var snaps string
	var created int64
	var finished sql.NullInt64
	err := row.Scan(&b.ID, &b.SiteID, &b.AppID, &b.ServerID, &b.StorageID, &b.Kind, &b.Trigger, &b.Status, &b.Error,
		&b.Log, &snaps, &b.Bytes, &b.DataAdded, &b.ActorID, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Snapshots = json.RawMessage(snaps)
	b.CreatedAt, b.FinishedAt = time.Unix(created, 0), timePtr(finished)
	return &b, nil
}

// Backups returns the target's runs, newest first.
func (db *DB) Backups(ctx context.Context, t BackupTarget, limit int) ([]*Backup, error) {
	cond, tid := t.where("")
	rows, err := db.R.QueryContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE `+cond+` ORDER BY id DESC LIMIT ?`, tid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (db *DB) BackupByID(ctx context.Context, t BackupTarget, id int64) (*Backup, error) {
	cond, tid := t.where("")
	return scanBackup(db.R.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE `+cond+` AND id = ?`, tid, id))
}

// LastPrune returns when a repository last had its space freed.
func (db *DB) LastPrune(ctx context.Context, storageID, serverID int64) (time.Time, error) {
	var at int64
	err := db.R.QueryRowContext(ctx, `SELECT pruned_at FROM backup_prunes WHERE storage_id = ? AND server_id = ?`,
		storageID, serverID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return time.Unix(at, 0), err
}

func (db *DB) SetPruned(ctx context.Context, storageID, serverID int64, at time.Time) error {
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO backup_prunes (storage_id, server_id, pruned_at) VALUES (?, ?, ?)
		 ON CONFLICT DO UPDATE SET pruned_at = excluded.pruned_at`, storageID, serverID, at.Unix())
	return err
}

// SetServerRestic stores the restic version a server reported.
func (db *DB) SetServerRestic(ctx context.Context, serverID int64, version string) error {
	return db.execOne(ctx, `UPDATE servers SET restic = ? WHERE id = ?`, version, serverID)
}

// SetServerRailpack records the Railpack version a server builds with.
func (db *DB) SetServerRailpack(ctx context.Context, serverID int64, version string) error {
	return db.execOne(ctx, `UPDATE servers SET railpack = ? WHERE id = ?`, version, serverID)
}

// ServerRestic returns the restic version of a server ("" if none).
func (db *DB) ServerRestic(ctx context.Context, serverID int64) (string, error) {
	var v string
	err := db.R.QueryRowContext(ctx, `SELECT restic FROM servers WHERE id = ?`, serverID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}
