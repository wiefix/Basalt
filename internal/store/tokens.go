package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// APIToken is a user's credential for the API (scripts, CI).
type APIToken struct {
	ID        int64
	UserID    int64
	UserEmail string // in listings
	Name      string
	Prefix    string
	Role      Role // the most the token may do (its user's role caps it)
	// ProjectID limits the token to one project's resources (0: none).
	ProjectID  int64
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	LastUsedIP string
	CreatedAt  time.Time
}

// Expired reports whether the token no longer works at now.
func (t *APIToken) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// MaxAPITokens is how many tokens one user may have.
const MaxAPITokens = 50

var ErrTooManyTokens = errors.New("too many API tokens; delete some first")

const tokenColumns = `t.id, t.user_id, u.email, t.name, t.prefix, t.role, COALESCE(t.project_id, 0), t.expires_at, t.last_used_at, t.last_used_ip, t.created_at`

func scanAPIToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var expires, used sql.NullInt64
	var created int64
	err := row.Scan(&t.ID, &t.UserID, &t.UserEmail, &t.Name, &t.Prefix, &t.Role, &t.ProjectID, &expires, &used, &t.LastUsedIP, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.ExpiresAt, t.LastUsedAt, t.CreatedAt = timePtr(expires), timePtr(used), time.Unix(created, 0)
	return &t, nil
}

// CreateAPIToken stores a token by its hash.
func (db *DB) CreateAPIToken(ctx context.Context, t *APIToken, hash []byte) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE user_id = ?`, t.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxAPITokens {
		return ErrTooManyTokens
	}
	var expires any
	if t.ExpiresAt != nil {
		expires = t.ExpiresAt.Unix()
	}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO api_tokens (user_id, name, token_hash, prefix, role, expires_at, project_id) VALUES (?, ?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`, t.UserID, t.Name, hash, t.Prefix, t.Role, expires, nullID(t.ProjectID)).Scan(&t.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: api_tokens.user_id, api_tokens.name") {
			return ErrNameTaken
		}
		return err
	}
	t.CreatedAt = time.Unix(created, 0)
	return tx.Commit()
}

// APITokens lists a user's tokens (userID 0: everyone's), newest first.
func (db *DB) APITokens(ctx context.Context, userID int64) ([]*APIToken, error) {
	q := `SELECT ` + tokenColumns + ` FROM api_tokens t JOIN users u ON u.id = t.user_id`
	var args []any
	if userID > 0 {
		q += ` WHERE t.user_id = ?`
		args = append(args, userID)
	}
	rows, err := db.R.QueryContext(ctx, q+` ORDER BY t.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) APIToken(ctx context.Context, id int64) (*APIToken, error) {
	return scanAPIToken(db.R.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE t.id = ?`, id))
}

// APITokenByHash returns the token with this hash if it has not expired.
func (db *DB) APITokenByHash(ctx context.Context, hash []byte, now time.Time) (*APIToken, error) {
	return scanAPIToken(db.R.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.token_hash = ? AND (t.expires_at IS NULL OR t.expires_at > ?)`, hash, now.Unix()))
}

// TouchAPIToken records a use of a token.
func (db *DB) TouchAPIToken(ctx context.Context, id int64, ip string, at time.Time) error {
	_, err := db.W.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, at.Unix(), ip, id)
	return err
}

func (db *DB) DeleteAPIToken(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
}

// DeleteExpiredAPITokens forgets tokens that expired before before.
func (db *DB) DeleteExpiredAPITokens(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at <= ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
