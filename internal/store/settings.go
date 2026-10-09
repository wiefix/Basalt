package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
)

const settingInstanceID = "instance_id"

// InstanceID returns the unique ID of this installation, generating it on first use.
func (db *DB) InstanceID(ctx context.Context) (string, error) {
	id := rand.Text()
	// INSERT OR IGNORE keeps the first ID ever written.
	if _, err := db.W.ExecContext(ctx,
		`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, settingInstanceID, id); err != nil {
		return "", fmt.Errorf("store instance id: %w", err)
	}
	return db.Setting(ctx, settingInstanceID)
}

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Setting returns the value stored under key, or ErrNotFound.
func (db *DB) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := db.R.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read setting %q: %w", key, err)
	}
	return v, nil
}
