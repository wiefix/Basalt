package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
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
	// AgentURL is where agents reach it once it takes over (it reports
	// it; "" until then).
	AgentURL  string
	CreatedAt time.Time
}

const standbyColumns = `id, name, COALESCE(server_id, 0), cert_fp, prev_cert_fp, cert_expires_at, enrolled_at,
	last_seen_at, last_ip, agent_url, created_at`

func scanStandby(r interface{ Scan(...any) error }) (*Standby, error) {
	var s Standby
	var exp, enrolled, seen sql.NullInt64
	var created int64
	if err := r.Scan(&s.ID, &s.Name, &s.ServerID, &s.CertFP, &s.PrevCertFP, &exp, &enrolled, &seen, &s.LastIP, &s.AgentURL, &created); err != nil {
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

// SetStandbyAgentURL records where agents reach a standby once it takes
// over; changed reports whether it differs from what was recorded.
func (db *DB) SetStandbyAgentURL(ctx context.Context, id int64, url string) (changed bool, err error) {
	res, err := db.W.ExecContext(ctx, `UPDATE standby_panels SET agent_url = ? WHERE id = ? AND agent_url != ?`, url, id, url)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
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

const settingPanelEpoch = "panel_epoch"

// PanelEpoch is how many times a standby took over this panel's database
// (0 for the panel it started as).
func (db *DB) PanelEpoch(ctx context.Context) (int64, error) {
	v, err := db.Setting(ctx, settingPanelEpoch)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// SettingPanelServer is the server whose agent runs on the panel's own
// host (it does not vote on whether the panel is gone).
const SettingPanelServer = "panel_server_id"

// SettingPromoted records a promotion the panel has not announced yet
// (JSON Promotion).
const SettingPromoted = "panel_promoted"

// Promotion is how this database became the panel's.
type Promotion struct {
	Epoch int64     `json:"epoch"`
	At    time.Time `json:"at"`
	Auto  bool      `json:"auto"` // the standby took over by itself
}

// PanelServer is the server on the panel's host (0: not known).
func (db *DB) PanelServer(ctx context.Context) int64 {
	v, err := db.Setting(ctx, SettingPanelServer)
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseInt(v, 10, 64)
	return id
}

// Promote makes this database, a standby's copy, the panel's: the epoch
// goes up by one, the standby (self) is no longer listed and its host's
// server becomes the panel's. It returns the new epoch.
func (db *DB) Promote(ctx context.Context, self int64, auto bool, now time.Time) (int64, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var v string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingPanelEpoch).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	epoch, _ := strconv.ParseInt(v, 10, 64)
	epoch++
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		settingPanelEpoch, strconv.FormatInt(epoch, 10)); err != nil {
		return 0, err
	}
	var host sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT server_id FROM standby_panels WHERE id = ?`, self).Scan(&host); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	b, _ := json.Marshal(Promotion{Epoch: epoch, At: now.UTC(), Auto: auto})
	set := map[string]string{SettingPromoted: string(b)}
	if host.Valid {
		set[SettingPanelServer] = strconv.FormatInt(host.Int64, 10)
	}
	for k, v := range set {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM standby_panels WHERE id = ?`, self); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

// GuessPanelServer records, for a panel installed before it was recorded,
// the server on the panel's host: the one agent that connects from the
// loopback, if there is exactly one.
func (db *DB) GuessPanelServer(ctx context.Context) error {
	if db.PanelServer(ctx) != 0 {
		return nil
	}
	rows, err := db.R.QueryContext(ctx, `SELECT id FROM servers WHERE last_ip IN ('127.0.0.1', '::1')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != 1 {
		return err
	}
	return db.SetSetting(ctx, SettingPanelServer, strconv.FormatInt(ids[0], 10))
}

// TakePromotion returns the promotion not announced yet, and forgets it
// (nil: none).
func (db *DB) TakePromotion(ctx context.Context) (*Promotion, error) {
	v, err := db.Setting(ctx, SettingPromoted)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := db.deleteSetting(ctx, SettingPromoted); err != nil {
		return nil, err
	}
	var p Promotion
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		return nil, err
	}
	return &p, nil
}
