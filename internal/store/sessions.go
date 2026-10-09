package store

import (
	"context"
	"database/sql"
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
