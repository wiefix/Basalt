package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleMember: 2, RoleAdmin: 3, RoleOwner: 4}

// AtLeast reports whether r grants at least the privileges of min.
func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] && roleRank[min] > 0 }

func (r Role) Valid() bool { return roleRank[r] > 0 }

type User struct {
	ID           int64
	Email        string
	Name         string
	PasswordHash string
	Role         Role
	TOTPSecret   []byte // encrypted; nil when 2FA is disabled
	TOTPPending  []byte // encrypted; enrolment in progress
	TOTPLastStep int64
	Disabled     bool
	CreatedAt    time.Time
}

func (u *User) TOTPEnabled() bool { return u.TOTPSecret != nil }

var ErrEmailTaken = errors.New("email already in use")

const userColumns = `id, email, name, password_hash, role, totp_secret, totp_pending,
	totp_last_step, disabled_at IS NOT NULL, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created int64
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Role,
		&u.TOTPSecret, &u.TOTPPending, &u.TOTPLastStep, &u.Disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateFirstOwner creates the initial owner account. It fails if any user
// already exists, so concurrent setup requests cannot both succeed.
func (db *DB) CreateFirstOwner(ctx context.Context, email, name, passwordHash string) (*User, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrSetupDone
	}
	u, err := scanUser(tx.QueryRowContext(ctx,
		`INSERT INTO users (email, name, password_hash, role) VALUES (?, ?, ?, 'owner')
		 RETURNING `+userColumns, email, name, passwordHash))
	if err != nil {
		return nil, fmt.Errorf("create owner: %w", err)
	}
	return u, tx.Commit()
}

var ErrSetupDone = errors.New("setup already completed")

func (db *DB) CreateUser(ctx context.Context, email, name, passwordHash string, role Role) (*User, error) {
	u, err := scanUser(db.W.QueryRowContext(ctx,
		`INSERT INTO users (email, name, password_hash, role) VALUES (?, ?, ?, ?)
		 RETURNING `+userColumns, email, name, passwordHash, role))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: users.email") {
		return nil, ErrEmailTaken
	}
	return u, err
}

func (db *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(db.R.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

func (db *DB) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(db.R.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, email))
}

func (db *DB) UpdatePasswordHash(ctx context.Context, userID int64, hash string) error {
	return db.execOne(ctx, `UPDATE users SET password_hash = ?, updated_at = unixepoch() WHERE id = ?`, hash, userID)
}

func (db *DB) SetTOTPPending(ctx context.Context, userID int64, encSecret []byte) error {
	return db.execOne(ctx, `UPDATE users SET totp_pending = ?, updated_at = unixepoch() WHERE id = ?`, encSecret, userID)
}

// EnableTOTP promotes the pending secret and replaces all recovery codes.
func (db *DB) EnableTOTP(ctx context.Context, userID int64, encSecret []byte, step int64, recoveryHashes [][]byte) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET totp_secret = ?, totp_pending = NULL, totp_last_step = ?, updated_at = unixepoch()
		 WHERE id = ?`, encSecret, step, userID); err != nil {
		return err
	}
	if err := replaceRecoveryCodes(ctx, tx, userID, recoveryHashes); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DisableTOTP(ctx context.Context, userID int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET totp_secret = NULL, totp_pending = NULL, totp_last_step = 0, updated_at = unixepoch()
		 WHERE id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// AdvanceTOTPStep records a used TOTP step. It returns false if a code for
// this or a later step was already used (a concurrent replay).
func (db *DB) AdvanceTOTPStep(ctx context.Context, userID, step int64) (bool, error) {
	res, err := db.W.ExecContext(ctx,
		`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, userID int64, hashes [][]byte) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
			return err
		}
	}
	return nil
}

// UseRecoveryCode marks a code as used. It returns false if the code is
// unknown or already used.
func (db *DB) UseRecoveryCode(ctx context.Context, userID int64, codeHash []byte) (bool, error) {
	res, err := db.W.ExecContext(ctx,
		`UPDATE recovery_codes SET used_at = unixepoch()
		 WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`, userID, codeHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (db *DB) RemainingRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	var n int
	err := db.R.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

func (db *DB) execOne(ctx context.Context, query string, args ...any) error {
	res, err := db.W.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
