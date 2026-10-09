package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/sftpd"
)

type SFTPUser struct {
	ID           int64
	SiteID       int64
	Username     string
	PasswordHash string
	PublicKeys   []string
	CreatedAt    time.Time
}

var ErrUsernameTaken = errors.New("username already in use")

func (db *DB) CreateSFTPUser(ctx context.Context, siteID int64, username, passwordHash string, keys []string) (*SFTPUser, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	keysJSON, _ := json.Marshal(keys)
	u := &SFTPUser{SiteID: siteID, Username: username, PasswordHash: passwordHash, PublicKeys: keys}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO sftp_users (site_id, username, password_hash, public_keys) VALUES (?, ?, ?, ?)
		 RETURNING id, created_at`, siteID, username, passwordHash, string(keysJSON)).Scan(&u.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: sftp_users.username") {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	if err := bumpSFTPRevision(ctx, tx, siteID); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

func (db *DB) DeleteSFTPUser(ctx context.Context, siteID, id int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM sftp_users WHERE id = ? AND site_id = ?`, id, siteID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := bumpSFTPRevision(ctx, tx, siteID); err != nil {
		return err
	}
	return tx.Commit()
}

// BumpSFTPRevisionForServer marks the server's SFTP state as changed, e.g.
// after a site (and with it its accounts) was deleted.
func (db *DB) BumpSFTPRevisionForServer(ctx context.Context, serverID int64) error {
	_, err := db.W.ExecContext(ctx, `UPDATE servers SET sftp_revision = sftp_revision + 1 WHERE id = ?`, serverID)
	return err
}

func bumpSFTPRevision(ctx context.Context, tx *sql.Tx, siteID int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE servers SET sftp_revision = sftp_revision + 1
		 WHERE id = (SELECT server_id FROM sites WHERE id = ?)`, siteID)
	return err
}

func scanSFTPUsers(rows *sql.Rows) ([]*SFTPUser, error) {
	defer rows.Close()
	var out []*SFTPUser
	for rows.Next() {
		var u SFTPUser
		var keys string
		var created int64
		if err := rows.Scan(&u.ID, &u.SiteID, &u.Username, &u.PasswordHash, &keys, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(keys), &u.PublicKeys); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(created, 0)
		out = append(out, &u)
	}
	return out, rows.Err()
}

const sftpColumns = `u.id, u.site_id, u.username, u.password_hash, u.public_keys, u.created_at`

func (db *DB) SFTPUsers(ctx context.Context, siteID int64) ([]*SFTPUser, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT `+sftpColumns+` FROM sftp_users u WHERE u.site_id = ? ORDER BY u.username`, siteID)
	if err != nil {
		return nil, err
	}
	return scanSFTPUsers(rows)
}

// DesiredSFTP assembles every SFTP account on a server.
func (db *DB) DesiredSFTP(ctx context.Context, serverID int64) (rev int64, users []sftpd.User, err error) {
	if err := db.R.QueryRowContext(ctx, `SELECT sftp_revision FROM servers WHERE id = ?`, serverID).Scan(&rev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, ErrNotFound
		}
		return 0, nil, err
	}
	rows, err := db.R.QueryContext(ctx,
		`SELECT `+sftpColumns+` FROM sftp_users u JOIN sites s ON s.id = u.site_id
		 WHERE s.server_id = ? ORDER BY u.id`, serverID)
	if err != nil {
		return 0, nil, err
	}
	list, err := scanSFTPUsers(rows)
	if err != nil {
		return 0, nil, err
	}
	users = make([]sftpd.User, len(list))
	for i, u := range list {
		users[i] = sftpd.User{Name: u.Username, SiteID: u.SiteID, PasswordHash: u.PasswordHash, PublicKeys: u.PublicKeys}
	}
	return rev, users, nil
}

func (db *DB) SetServerSFTP(ctx context.Context, serverID int64, port int, fingerprint string) error {
	_, err := db.W.ExecContext(ctx, `UPDATE servers SET sftp_port = ?, sftp_fingerprint = ? WHERE id = ?`,
		port, fingerprint, serverID)
	return err
}

func (db *DB) ServerSFTP(ctx context.Context, serverID int64) (port int, fingerprint string, err error) {
	err = db.R.QueryRowContext(ctx, `SELECT sftp_port, sftp_fingerprint FROM servers WHERE id = ?`, serverID).
		Scan(&port, &fingerprint)
	return port, fingerprint, err
}
