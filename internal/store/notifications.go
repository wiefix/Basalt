package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// NotificationChannel is where events are sent.
type NotificationChannel struct {
	ID     int64
	Name   string
	Kind   string // webhook, slack, discord, telegram, email
	Config []byte // encrypted settings (kind-specific JSON)
	Events []string
	// The outcome of the last delivery.
	LastSentAt  *time.Time
	LastError   string
	LastErrorAt *time.Time
	CreatedAt   time.Time
}

const channelColumns = `id, name, kind, config, events, last_sent_at, last_error, last_error_at, created_at`

func scanChannel(row interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var events string
	var sent, errAt sql.NullInt64
	var created int64
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &events, &sent, &c.LastError, &errAt, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(events), &c.Events); err != nil {
		return nil, err
	}
	c.LastSentAt, c.LastErrorAt, c.CreatedAt = timePtr(sent), timePtr(errAt), time.Unix(created, 0)
	return &c, nil
}

func (db *DB) NotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NotificationChannel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) NotificationChannel(ctx context.Context, id int64) (*NotificationChannel, error) {
	return scanChannel(db.R.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = ?`, id))
}

// CreateNotificationChannel stores a channel; Config is sealed by the
// caller with the ID it gets, so the row is inserted first and the config
// set by SetNotificationChannel.
func (db *DB) CreateNotificationChannel(ctx context.Context, c *NotificationChannel) error {
	events, _ := json.Marshal(nonNilStrings(c.Events))
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO notification_channels (name, kind, config, events) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		c.Name, c.Kind, c.Config, string(events)).Scan(&c.ID, &created)
	if err != nil {
		return nameTaken(err)
	}
	c.CreatedAt = time.Unix(created, 0)
	return nil
}

// UpdateNotificationChannel changes a channel's name, settings and events
// (not its kind).
func (db *DB) UpdateNotificationChannel(ctx context.Context, c *NotificationChannel) error {
	events, _ := json.Marshal(nonNilStrings(c.Events))
	res, err := db.W.ExecContext(ctx, `UPDATE notification_channels SET name = ?, config = ?, events = ? WHERE id = ?`,
		c.Name, c.Config, string(events), c.ID)
	if err != nil {
		return nameTaken(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM notification_channels WHERE id = ?`, id)
}

// SetNotificationResult records the outcome of a delivery ("" for success).
func (db *DB) SetNotificationResult(ctx context.Context, id int64, errMsg string, at time.Time) error {
	if errMsg == "" {
		_, err := db.W.ExecContext(ctx, `UPDATE notification_channels SET last_sent_at = ?, last_error = '', last_error_at = NULL WHERE id = ?`,
			at.Unix(), id)
		return err
	}
	_, err := db.W.ExecContext(ctx, `UPDATE notification_channels SET last_error = ?, last_error_at = ? WHERE id = ?`,
		errMsg, at.Unix(), id)
	return err
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
