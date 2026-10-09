package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Database struct {
	ID        int64
	SiteID    int64
	ServerID  int64
	Name      string
	Username  string
	CreatedAt time.Time
}

var ErrSiteHasDatabases = errors.New("site still has databases")

func (db *DB) CreateDatabaseRecord(ctx context.Context, siteID, serverID int64, name, username string) (*Database, error) {
	d := &Database{SiteID: siteID, ServerID: serverID, Name: name, Username: username}
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO databases (site_id, server_id, name, username) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		siteID, serverID, name, username).Scan(&d.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	return d, nil
}

func (db *DB) Databases(ctx context.Context, siteID int64) ([]*Database, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, site_id, server_id, name, username, created_at FROM databases WHERE site_id = ? ORDER BY name`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Database
	for rows.Next() {
		var d Database
		var created int64
		if err := rows.Scan(&d.ID, &d.SiteID, &d.ServerID, &d.Name, &d.Username, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = time.Unix(created, 0)
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (db *DB) DatabaseByID(ctx context.Context, siteID, id int64) (*Database, error) {
	list, err := db.Databases(ctx, siteID)
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		if d.ID == id {
			return d, nil
		}
	}
	return nil, ErrNotFound
}

func (db *DB) DeleteDatabaseRecord(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM databases WHERE id = ?`, id)
}

func (db *DB) SetServerMariaDB(ctx context.Context, serverID int64, version string) error {
	_, err := db.W.ExecContext(ctx, `UPDATE servers SET mariadb = ? WHERE id = ?`, version, serverID)
	return err
}

func (db *DB) ServerMariaDB(ctx context.Context, serverID int64) (string, error) {
	var v string
	err := db.R.QueryRowContext(ctx, `SELECT mariadb FROM servers WHERE id = ?`, serverID).Scan(&v)
	return v, err
}
