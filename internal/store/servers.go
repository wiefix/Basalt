package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Server struct {
	ID            int64
	Name          string
	Enrolled      bool
	EnrollExpires *time.Time
	CertFP        string
	PrevCertFP    string
	CertExpires   *time.Time
	Hostname      string
	OS            string
	Arch          string
	Kernel        string
	Distro        string
	AgentVersion  string
	CPUCount      int
	MemTotal      uint64
	LastSeen      *time.Time
	LastIP        string
	PHPVersions   []string
	Isolated      bool
	Docker        string // Docker Engine version, "" if unavailable
	CreatedAt     time.Time
}

var ErrNameTaken = errors.New("name already in use")

const serverColumns = `id, name, enrolled_at IS NOT NULL, enroll_expires_at, COALESCE(cert_fp, ''),
	COALESCE(prev_cert_fp, ''), cert_expires_at, hostname, os, arch, kernel, distro, agent_version,
	cpu_count, mem_total, last_seen_at, last_ip, php_versions, isolated, docker, created_at`

func scanServer(row interface{ Scan(...any) error }) (*Server, error) {
	var s Server
	var enrollExp, certExp, lastSeen sql.NullInt64
	var created int64
	var phpVersions string
	err := row.Scan(&s.ID, &s.Name, &s.Enrolled, &enrollExp, &s.CertFP, &s.PrevCertFP, &certExp,
		&s.Hostname, &s.OS, &s.Arch, &s.Kernel, &s.Distro, &s.AgentVersion,
		&s.CPUCount, &s.MemTotal, &lastSeen, &s.LastIP, &phpVersions, &s.Isolated, &s.Docker, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(phpVersions), &s.PHPVersions); err != nil {
		return nil, err
	}
	s.EnrollExpires = timePtr(enrollExp)
	s.CertExpires = timePtr(certExp)
	s.LastSeen = timePtr(lastSeen)
	s.CreatedAt = time.Unix(created, 0)
	return &s, nil
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

// CreateServer registers a server awaiting enrollment.
func (db *DB) CreateServer(ctx context.Context, name string, tokenHash []byte, expires time.Time) (*Server, error) {
	s, err := scanServer(db.W.QueryRowContext(ctx,
		`INSERT INTO servers (name, enroll_token_hash, enroll_expires_at) VALUES (?, ?, ?)
		 RETURNING `+serverColumns, name, tokenHash, expires.Unix()))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: servers.name") {
		return nil, ErrNameTaken
	}
	return s, err
}

// ResetEnrollToken issues a new token for a server that has not enrolled yet.
func (db *DB) ResetEnrollToken(ctx context.Context, id int64, tokenHash []byte, expires time.Time) error {
	return db.execOne(ctx,
		`UPDATE servers SET enroll_token_hash = ?, enroll_expires_at = ? WHERE id = ? AND enrolled_at IS NULL`,
		tokenHash, expires.Unix(), id)
}

// ConsumeEnrollToken atomically redeems a valid token and returns the server ID.
func (db *DB) ConsumeEnrollToken(ctx context.Context, tokenHash []byte, now time.Time) (int64, error) {
	var id int64
	err := db.W.QueryRowContext(ctx,
		`UPDATE servers SET enroll_token_hash = NULL, enroll_expires_at = NULL
		 WHERE enroll_token_hash = ? AND enroll_expires_at > ? AND enrolled_at IS NULL
		 RETURNING id`, tokenHash, now.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// SetServerCert records a newly issued agent certificate. On first issue the
// server becomes enrolled; on renewal the old fingerprint stays accepted.
func (db *DB) SetServerCert(ctx context.Context, id int64, fp string, expires, now time.Time) error {
	return db.execOne(ctx,
		`UPDATE servers SET prev_cert_fp = cert_fp, cert_fp = ?, cert_expires_at = ?,
		 enrolled_at = COALESCE(enrolled_at, ?) WHERE id = ?`,
		fp, expires.Unix(), now.Unix(), id)
}

func (db *DB) UpdateServerHello(ctx context.Context, id int64, s Server) error {
	if s.PHPVersions == nil {
		s.PHPVersions = []string{}
	}
	php, _ := json.Marshal(s.PHPVersions)
	return db.execOne(ctx,
		`UPDATE servers SET hostname = ?, os = ?, arch = ?, kernel = ?, distro = ?, agent_version = ?,
		 cpu_count = ?, mem_total = ?, last_ip = ?, last_seen_at = ?, php_versions = ?, isolated = ?, docker = ?
		 WHERE id = ?`,
		s.Hostname, s.OS, s.Arch, s.Kernel, s.Distro, s.AgentVersion, s.CPUCount, s.MemTotal,
		s.LastIP, s.LastSeen.Unix(), string(php), s.Isolated, s.Docker, id)
}

func (db *DB) ServerByID(ctx context.Context, id int64) (*Server, error) {
	return scanServer(db.R.QueryRowContext(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = ?`, id))
}

func (db *DB) ListServers(ctx context.Context) ([]*Server, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+serverColumns+` FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Server
	for rows.Next() {
		s, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) DeleteServer(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM servers WHERE id = ?`, id)
}

type ServerMetrics struct {
	At        time.Time
	CPUPct    float64
	MemUsed   uint64
	MemTotal  uint64
	DiskUsed  uint64
	DiskTotal uint64
	Load1     float64
	Load5     float64
	Load15    float64
	Uptime    uint64
}

// InsertMetrics stores a sample and marks the server as seen.
func (db *DB) InsertMetrics(ctx context.Context, serverID int64, m ServerMetrics) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO server_metrics
		 (server_id, at, cpu_pct, mem_used, mem_total, disk_used, disk_total, load1, load5, load15, uptime)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		serverID, m.At.Unix(), m.CPUPct, m.MemUsed, m.MemTotal, m.DiskUsed, m.DiskTotal,
		m.Load1, m.Load5, m.Load15, m.Uptime); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE servers SET last_seen_at = ? WHERE id = ?`, m.At.Unix(), serverID); err != nil {
		return err
	}
	return tx.Commit()
}

// MetricsSince returns samples newer than since, oldest first.
func (db *DB) MetricsSince(ctx context.Context, serverID int64, since time.Time, limit int) ([]ServerMetrics, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT at, cpu_pct, mem_used, mem_total, disk_used, disk_total, load1, load5, load15, uptime
		 FROM server_metrics WHERE server_id = ? AND at > ? ORDER BY at LIMIT ?`,
		serverID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerMetrics
	for rows.Next() {
		var m ServerMetrics
		var at int64
		if err := rows.Scan(&at, &m.CPUPct, &m.MemUsed, &m.MemTotal, &m.DiskUsed, &m.DiskTotal,
			&m.Load1, &m.Load5, &m.Load15, &m.Uptime); err != nil {
			return nil, err
		}
		m.At = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (db *DB) DeleteMetricsBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM server_metrics WHERE at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetSettingIfAbsent stores value under key unless the key already exists.
func (db *DB) SetSettingIfAbsent(ctx context.Context, key, value string) error {
	_, err := db.W.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}
