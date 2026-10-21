package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Standby is a standby panel: a copy of this panel that can take over.
type Standby struct {
	ID          int64
	Name        string
	ServerID    int64 // the server of its host's agent (0: none)
	CertFP      string
	PrevCertFP  string
	CertExpires *time.Time
	EnrolledAt  *time.Time
	LastSeen    *time.Time
	LastIP      string
	CreatedAt   time.Time
}

const standbyColumns = `id, name, COALESCE(server_id, 0), cert_fp, prev_cert_fp, cert_expires_at, enrolled_at,
	last_seen_at, last_ip, created_at`

func scanStandby(r interface{ Scan(...any) error }) (*Standby, error) {
	var s Standby
	var exp, enrolled, seen sql.NullInt64
	var created int64
	if err := r.Scan(&s.ID, &s.Name, &s.ServerID, &s.CertFP, &s.PrevCertFP, &exp, &enrolled, &seen, &s.LastIP, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.CertExpires, s.EnrolledAt, s.LastSeen = nullTime(exp), nullTime(enrolled), nullTime(seen)
	s.CreatedAt = time.Unix(created, 0)
	return &s, nil
}

func nullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

// CreateStandby records a standby panel waiting to enroll with the token
// whose hash is tokenHash.
func (db *DB) CreateStandby(ctx context.Context, name string, serverID int64, tokenHash []byte, expires time.Time) (*Standby, error) {
	var sid any
	if serverID > 0 {
		sid = serverID
	}
	s, err := scanStandby(db.W.QueryRowContext(ctx,
		`INSERT INTO standby_panels (name, server_id, enroll_token_hash, enroll_expires_at) VALUES (?, ?, ?, ?)
		 RETURNING `+standbyColumns, name, sid, tokenHash, expires.Unix()))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: standby_panels.name") {
		return nil, ErrNameTaken
	}
	return s, err
}

// ConsumeStandbyToken redeems a valid enrollment token once.
func (db *DB) ConsumeStandbyToken(ctx context.Context, tokenHash []byte, now time.Time) (int64, error) {
	var id int64
	err := db.W.QueryRowContext(ctx,
		`UPDATE standby_panels SET enroll_token_hash = NULL, enroll_expires_at = NULL
		 WHERE enroll_token_hash = ? AND enroll_expires_at > ? AND enrolled_at IS NULL
		 RETURNING id`, tokenHash, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// SetStandbyCert records a newly issued certificate; the previous one stays
// accepted (renewal).
func (db *DB) SetStandbyCert(ctx context.Context, id int64, fp string, expires, now time.Time) error {
	return db.execOne(ctx,
		`UPDATE standby_panels SET prev_cert_fp = cert_fp, cert_fp = ?, cert_expires_at = ?,
		 enrolled_at = COALESCE(enrolled_at, ?) WHERE id = ?`, fp, expires.Unix(), now.Unix(), id)
}

// SetStandbySeen records when and from where a standby last connected.
func (db *DB) SetStandbySeen(ctx context.Context, id int64, ip string, at time.Time) error {
	return db.execOne(ctx, `UPDATE standby_panels SET last_seen_at = ?, last_ip = ? WHERE id = ?`, at.Unix(), ip, id)
}

func (db *DB) StandbyByID(ctx context.Context, id int64) (*Standby, error) {
	return scanStandby(db.R.QueryRowContext(ctx, `SELECT `+standbyColumns+` FROM standby_panels WHERE id = ?`, id))
}

func (db *DB) Standbys(ctx context.Context) ([]*Standby, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+standbyColumns+` FROM standby_panels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Standby{}
	for rows.Next() {
		s, err := scanStandby(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteStandby forgets a standby panel: its certificate is no longer
// accepted.
func (db *DB) DeleteStandby(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM standby_panels WHERE id = ?`, id)
}
