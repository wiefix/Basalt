package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Registry holds the credentials used to pull images from one registry.
type Registry struct {
	ID        int64
	Host      string
	Username  string
	Secret    []byte // encrypted password or token
	CreatedAt time.Time
	UpdatedAt time.Time
}

const registryColumns = `id, host, username, secret, created_at, updated_at`

func scanRegistry(row interface{ Scan(...any) error }) (*Registry, error) {
	var r Registry
	var created, updated int64
	err := row.Scan(&r.ID, &r.Host, &r.Username, &r.Secret, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &r, nil
}

func (db *DB) CreateRegistry(ctx context.Context, host, username string, secret []byte) (*Registry, error) {
	r, err := scanRegistry(db.W.QueryRowContext(ctx,
		`INSERT INTO registries (host, username, secret) VALUES (?, ?, ?) RETURNING `+registryColumns,
		host, username, secret))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: registries.host") {
		return nil, ErrNameTaken
	}
	return r, err
}

// UpdateRegistry replaces the credentials of a registry.
func (db *DB) UpdateRegistry(ctx context.Context, id int64, username string, secret []byte) error {
	return db.execOne(ctx, `UPDATE registries SET username = ?, secret = ?, updated_at = unixepoch() WHERE id = ?`,
		username, secret, id)
}

func (db *DB) DeleteRegistry(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM registries WHERE id = ?`, id)
}

func (db *DB) RegistryByID(ctx context.Context, id int64) (*Registry, error) {
	return scanRegistry(db.R.QueryRowContext(ctx, `SELECT `+registryColumns+` FROM registries WHERE id = ?`, id))
}

func (db *DB) RegistryByHost(ctx context.Context, host string) (*Registry, error) {
	return scanRegistry(db.R.QueryRowContext(ctx, `SELECT `+registryColumns+` FROM registries WHERE host = ?`, host))
}

func (db *DB) Registries(ctx context.Context) ([]*Registry, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+registryColumns+` FROM registries ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Registry
	for rows.Next() {
		r, err := scanRegistry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
