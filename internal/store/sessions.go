package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type Session struct {
	TokenHash  []byte
	UserID     int64
	MFAPending bool
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

func (db *DB) CreateSession(ctx context.Context, s Session) error {
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, mfa_pending, ip, user_agent, created_at, last_seen_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.TokenHash, s.UserID, s.MFAPending, s.IP, s.UserAgent,
		s.CreatedAt.Unix(), s.LastSeenAt.Unix(), s.ExpiresAt.Unix())
	return err
}

// SessionByTokenHash returns an unexpired session, or ErrNotFound.
func (db *DB) SessionByTokenHash(ctx context.Context, hash []byte, now time.Time) (*Session, error) {
	var s Session
	var created, seen, expires int64
	err := db.R.QueryRowContext(ctx,
		`SELECT token_hash, user_id, mfa_pending, ip, user_agent, created_at, last_seen_at, expires_at
		 FROM sessions WHERE token_hash = ? AND expires_at > ?`, hash, now.Unix()).
		Scan(&s.TokenHash, &s.UserID, &s.MFAPending, &s.IP, &s.UserAgent, &created, &seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(expires, 0)
	return &s, nil
}

func (db *DB) TouchSession(ctx context.Context, hash []byte, now, expires time.Time) error {
	_, err := db.W.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`,
		now.Unix(), expires.Unix(), hash)
	return err
}

func (db *DB) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash)
	return err
}

// DeleteUserSessions removes all sessions of a user except keep (may be nil).
func (db *DB) DeleteUserSessions(ctx context.Context, userID int64, keep []byte) error {
	_, err := db.W.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND token_hash IS NOT ?`, userID, keep)
	return err
}

func (db *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SessionID identifies a session in lists: the start of its token's hash
// (neither the token nor the full hash is shown).
func SessionID(hash []byte) string { return hex.EncodeToString(hash[:8]) }

// UserSessions lists a user's signed-in sessions, newest activity first.
func (db *DB) UserSessions(ctx context.Context, userID int64, now time.Time) ([]*Session, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT token_hash, user_id, mfa_pending, ip, user_agent, created_at, last_seen_at, expires_at
		 FROM sessions WHERE user_id = ? AND expires_at > ? AND mfa_pending = 0 ORDER BY last_seen_at DESC`, userID, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var s Session
		var created, seen, expires int64
		if err := rows.Scan(&s.TokenHash, &s.UserID, &s.MFAPending, &s.IP, &s.UserAgent, &created, &seen, &expires); err != nil {
			return nil, err
		}
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(expires, 0)
		out = append(out, &s)
	}
	return out, rows.Err()
}

// DeleteUserSessionByID ends one of a user's sessions by its SessionID.
func (db *DB) DeleteUserSessionByID(ctx context.Context, userID int64, id string) error {
	return db.execOne(ctx, `DELETE FROM sessions WHERE user_id = ? AND lower(hex(substr(token_hash, 1, 8))) = ?`, userID, id)
}
