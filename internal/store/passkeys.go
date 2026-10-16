package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Passkey is a user's WebAuthn credential.
type Passkey struct {
	ID             int64
	UserID         int64
	CredentialID   []byte
	PublicKey      []byte
	Alg            int
	SignCount      uint32
	RPID           string
	Name           string
	AAGUID         []byte
	BackupEligible bool
	BackedUp       bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

// MaxPasskeys is how many passkeys one user may have.
const MaxPasskeys = 20

var (
	ErrTooManyPasskeys = errors.New("too many passkeys; remove some first")
	ErrPasskeyExists   = errors.New("this passkey is registered already")
)

const passkeyColumns = `id, user_id, credential_id, public_key, alg, sign_count, rp_id, name, aaguid, backup_eligible,
	backed_up, created_at, last_used_at`

func scanPasskey(row interface{ Scan(...any) error }) (*Passkey, error) {
	var p Passkey
	var created int64
	var used sql.NullInt64
	err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.PublicKey, &p.Alg, &p.SignCount, &p.RPID, &p.Name, &p.AAGUID,
		&p.BackupEligible, &p.BackedUp, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.LastUsedAt = time.Unix(created, 0), timePtr(used)
	return &p, nil
}

// WebAuthnHandle returns the user's passkey handle, creating it on first use.
func (db *DB) WebAuthnHandle(ctx context.Context, userID int64) ([]byte, error) {
	h := make([]byte, 32)
	rand.Read(h)
	if _, err := db.W.ExecContext(ctx, `UPDATE users SET webauthn_handle = ? WHERE id = ? AND webauthn_handle IS NULL`, h, userID); err != nil {
		return nil, err
	}
	var out []byte
	err := db.R.QueryRowContext(ctx, `SELECT webauthn_handle FROM users WHERE id = ?`, userID).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// UserByWebAuthnHandle finds the user a passkey names.
func (db *DB) UserByWebAuthnHandle(ctx context.Context, handle []byte) (*User, error) {
	return scanUser(db.R.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE webauthn_handle = ?`, handle))
}

// CreatePasskey stores a new passkey.
func (db *DB) CreatePasskey(ctx context.Context, p *Passkey) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM passkeys WHERE user_id = ?`, p.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPasskeys {
		return ErrTooManyPasskeys
	}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO passkeys (user_id, credential_id, public_key, alg, sign_count, rp_id, name, aaguid, backup_eligible, backed_up)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
		p.UserID, p.CredentialID, p.PublicKey, p.Alg, p.SignCount, p.RPID, p.Name, p.AAGUID, p.BackupEligible, p.BackedUp).Scan(&p.ID, &created)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "UNIQUE constraint failed: passkeys.credential_id"):
			return ErrPasskeyExists
		case strings.Contains(err.Error(), "UNIQUE constraint failed"):
			return ErrNameTaken
		}
		return err
	}
	p.CreatedAt = time.Unix(created, 0)
	return tx.Commit()
}

// Passkeys lists a user's passkeys, oldest first.
func (db *DB) Passkeys(ctx context.Context, userID int64) ([]*Passkey, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Passkey{}
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (db *DB) PasskeyByCredentialID(ctx context.Context, credID []byte) (*Passkey, error) {
	return scanPasskey(db.R.QueryRowContext(ctx, `SELECT `+passkeyColumns+` FROM passkeys WHERE credential_id = ?`, credID))
}

// PasskeyUsed records a sign-in with a passkey; the counter only grows.
func (db *DB) PasskeyUsed(ctx context.Context, id int64, signCount uint32, backedUp bool, at time.Time) error {
	return db.execOne(ctx, `UPDATE passkeys SET sign_count = MAX(sign_count, ?), backed_up = ?, last_used_at = ? WHERE id = ?`,
		signCount, backedUp, at.Unix(), id)
}

// RenamePasskey renames one of a user's passkeys.
func (db *DB) RenamePasskey(ctx context.Context, userID, id int64, name string) error {
	err := db.execOne(ctx, `UPDATE passkeys SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrNameTaken
	}
	return err
}

// DeletePasskey removes one of a user's passkeys.
func (db *DB) DeletePasskey(ctx context.Context, userID, id int64) error {
	return db.execOne(ctx, `DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
}

// DeleteUserPasskeys removes all of a user's passkeys.
func (db *DB) DeleteUserPasskeys(ctx context.Context, userID int64) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM passkeys WHERE user_id = ?`, userID)
	return err
}

// CountPasskeys returns how many passkeys each user has.
func (db *DB) CountPasskeys(ctx context.Context) (map[int64]int, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT user_id, count(*) FROM passkeys GROUP BY user_id`)
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
