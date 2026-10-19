package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// NotificationChannel is where events are sent.
type NotificationChannel struct {
	ID int64
	// ProjectID: the project whose events it receives (0: the whole panel).
	ProjectID int64
	Name      string
	Kind      string // webhook, slack, discord, telegram, email
	Config    []byte // encrypted settings (kind-specific JSON)
	Events    []string
	// The outcome of the last delivery.
	LastSentAt  *time.Time
	LastError   string
	LastErrorAt *time.Time
	CreatedAt   time.Time
}

const channelColumns = `id, COALESCE(project_id, 0), name, kind, config, events, last_sent_at, last_error, last_error_at, created_at`

func scanChannel(row interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var events string
	var sent, errAt sql.NullInt64
	var created int64
	err := row.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.Config, &events, &sent, &c.LastError, &errAt, &created)
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
		`INSERT INTO notification_channels (project_id, name, kind, config, events) VALUES (?, ?, ?, ?, ?) RETURNING id, created_at`,
		nullID(c.ProjectID), c.Name, c.Kind, c.Config, string(events)).Scan(&c.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return ErrNoProject
		}
		return nameTaken(err)
	}
	c.CreatedAt = time.Unix(created, 0)
	return nil
}

// UpdateNotificationChannel changes a channel's name, settings and events
// (not its kind or project).
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

// QueuedNotification is a message waiting for delivery through a channel.
type QueuedNotification struct {
	ID        int64
	ChannelID int64
	Message   []byte // JSON
	Attempts  int
	LastError string
}

// QueueNotification stores message for each channel, held until next (the
// caller delivers it at once). A channel with perChannel messages waiting
// already gets none: a channel that keeps failing must not grow the queue
// without end. It returns the rows stored.
func (db *DB) QueueNotification(ctx context.Context, channels []int64, message []byte, next time.Time, perChannel int) ([]QueuedNotification, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var out []QueuedNotification
	for _, ch := range channels {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_queue WHERE channel_id = ?`, ch).Scan(&n); err != nil {
			return nil, err
		}
		if n >= perChannel {
			continue
		}
		q := QueuedNotification{ChannelID: ch, Message: message}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO notification_queue (channel_id, message, next_at) VALUES (?, ?, ?) RETURNING id`,
			ch, string(message), next.Unix()).Scan(&q.ID); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, tx.Commit()
}

// ClaimDueNotifications returns up to limit messages due by now and holds
// them until until, so that one attempt runs at a time.
func (db *DB) ClaimDueNotifications(ctx context.Context, now, until time.Time, limit int) ([]QueuedNotification, error) {
	rows, err := db.W.QueryContext(ctx,
		`UPDATE notification_queue SET next_at = ?
		 WHERE id IN (SELECT id FROM notification_queue WHERE next_at <= ? ORDER BY next_at, id LIMIT ?)
		 RETURNING id, channel_id, message, attempts, last_error`, until.Unix(), now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedNotification
	for rows.Next() {
		var q QueuedNotification
		var msg string
		if err := rows.Scan(&q.ID, &q.ChannelID, &msg, &q.Attempts, &q.LastError); err != nil {
			return nil, err
		}
		q.Message = []byte(msg)
		out = append(out, q)
	}
	return out, rows.Err()
}

// RetryNotification records a failed attempt; the next one is not before next.
func (db *DB) RetryNotification(ctx context.Context, id int64, attempts int, next time.Time, errMsg string) error {
	_, err := db.W.ExecContext(ctx, `UPDATE notification_queue SET attempts = ?, next_at = ?, last_error = ? WHERE id = ?`,
		attempts, next.Unix(), errMsg, id)
	return err
}

// DropNotification removes a delivered (or given up) message.
func (db *DB) DropNotification(ctx context.Context, id int64) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM notification_queue WHERE id = ?`, id)
	return err
}

// QueuedNotifications counts the messages waiting per channel.
func (db *DB) QueuedNotifications(ctx context.Context) (map[int64]int, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT channel_id, COUNT(*) FROM notification_queue GROUP BY channel_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
