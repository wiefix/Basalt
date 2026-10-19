package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

const settingHealthRevision = "health_revision"

// ErrNoHealthCheck: a DNS record refers to a health check that does not exist.
var ErrNoHealthCheck = errors.New("no such health check")

// Health check statuses.
const (
	HealthUnknown = "unknown"
	HealthUp      = "up"
	HealthDown    = "down"
)

// HealthCheck is a probe run from every server (see agentproto.HealthCheck).
type HealthCheck struct {
	ID          int64
	ProjectID   int64
	Name        string
	Kind        string
	Host        string
	Port        int
	Path        string
	HostHeader  string
	Insecure    bool
	Interval    int // seconds
	Timeout     int // seconds
	Status      string
	StatusSince *time.Time
	CreatedAt   time.Time
}

type HealthEvent struct {
	ID     int64
	Status string
	Detail string
	At     time.Time
}

const healthColumns = `id, project_id, name, kind, host, port, path, host_header, insecure, interval_s, timeout_s, status, status_since, created_at`

func scanHealthCheck(row interface{ Scan(...any) error }) (*HealthCheck, error) {
	var c HealthCheck
	var since sql.NullInt64
	var created int64
	err := row.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.Host, &c.Port, &c.Path, &c.HostHeader, &c.Insecure,
		&c.Interval, &c.Timeout, &c.Status, &since, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if since.Valid {
		t := time.Unix(since.Int64, 0)
		c.StatusSince = &t
	}
	c.CreatedAt = time.Unix(created, 0)
	return &c, nil
}

func (db *DB) HealthChecks(ctx context.Context) ([]*HealthCheck, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+healthColumns+` FROM health_checks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*HealthCheck{}
	for rows.Next() {
		c, err := scanHealthCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) HealthCheck(ctx context.Context, id int64) (*HealthCheck, error) {
	return scanHealthCheck(db.R.QueryRowContext(ctx, `SELECT `+healthColumns+` FROM health_checks WHERE id = ?`, id))
}

// HealthRevision numbers the set of checks agents run.
func (db *DB) HealthRevision(ctx context.Context) (int64, error) {
	var v string
	err := db.R.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingHealthRevision).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n, nil
}

func bumpHealthRevision(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, '1')
		 ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT), updated_at = unixepoch()`,
		settingHealthRevision)
	return err
}

// healthWrite runs a change to the checks and bumps the revision.
func (db *DB) healthWrite(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	if err := bumpHealthRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nameTaken(err error) error {
	if err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed: health_checks.name") ||
		strings.Contains(err.Error(), "UNIQUE constraint failed: index 'notification_channels_name'")) {
		return ErrNameTaken
	}
	return err
}

func (db *DB) CreateHealthCheck(ctx context.Context, c *HealthCheck) error {
	return db.healthWrite(ctx, func(tx *sql.Tx) error {
		// Checks of an app's placements (insertHealthCheck alone) come
		// with the app and do not count.
		if err := checkRoom(ctx, tx, projectOrDefault(c.ProjectID), "health_checks"); err != nil {
			return err
		}
		return insertHealthCheck(ctx, tx, c)
	})
}

func insertHealthCheck(ctx context.Context, tx *sql.Tx, c *HealthCheck) error {
	c.ProjectID = projectOrDefault(c.ProjectID)
	if err := projectExists(ctx, tx, c.ProjectID); err != nil {
		return err
	}
	var created int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO health_checks (project_id, name, kind, host, port, path, host_header, insecure, interval_s, timeout_s)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
		c.ProjectID, c.Name, c.Kind, c.Host, c.Port, c.Path, c.HostHeader, c.Insecure, c.Interval, c.Timeout).Scan(&c.ID, &created)
	if err != nil {
		return nameTaken(err)
	}
	c.Status, c.CreatedAt = HealthUnknown, time.Unix(created, 0)
	return nil
}

// UpdateHealthCheck changes a check; its status starts over. It reports
// whether DNS records follow the check (their zones then change).
func (db *DB) UpdateHealthCheck(ctx context.Context, c *HealthCheck) (dns bool, err error) {
	err = db.healthWrite(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE health_checks SET name = ?, kind = ?, host = ?, port = ?, path = ?, host_header = ?, insecure = ?,
			 interval_s = ?, timeout_s = ?, status = 'unknown', status_since = NULL WHERE id = ?`,
			c.Name, c.Kind, c.Host, c.Port, c.Path, c.HostHeader, c.Insecure, c.Interval, c.Timeout, c.ID)
		if err != nil {
			return nameTaken(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		c.Status, c.StatusSince = HealthUnknown, nil
		dns, err = bumpDNSIfFollowed(ctx, tx, c.ID)
		return err
	})
	return dns, err
}

// DeleteHealthCheck deletes a check; records that followed it are served
// unconditionally from then on. It reports whether there were any.
func (db *DB) DeleteHealthCheck(ctx context.Context, id int64) (dns bool, err error) {
	err = db.healthWrite(ctx, func(tx *sql.Tx) error {
		if dns, err = bumpDNSIfFollowed(ctx, tx, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM health_checks WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	return dns, err
}

// SetHealthStatus records a status change of a check. It reports whether
// DNS records follow the check (their zones then change).
func (db *DB) SetHealthStatus(ctx context.Context, id int64, status, detail string, at time.Time) (dns bool, err error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE health_checks SET status = ?, status_since = ? WHERE id = ?`, status, at.Unix(), id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO health_events (check_id, status, detail, at) VALUES (?, ?, ?, ?)`,
		id, status, detail, at.Unix()); err != nil {
		return false, err
	}
	if dns, err = bumpDNSIfFollowed(ctx, tx, id); err != nil {
		return false, err
	}
	return dns, tx.Commit()
}

// HealthEvents lists the latest status changes of a check, newest first.
func (db *DB) HealthEvents(ctx context.Context, id int64, limit int) ([]HealthEvent, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, status, detail, at FROM health_events WHERE check_id = ? ORDER BY at DESC, id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HealthEvent{}
	for rows.Next() {
		var e HealthEvent
		var at int64
		if err := rows.Scan(&e.ID, &e.Status, &e.Detail, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteHealthEventsBefore forgets old status changes.
func (db *DB) DeleteHealthEventsBefore(ctx context.Context, before time.Time) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM health_events WHERE at < ?`, before.Unix())
	return err
}
