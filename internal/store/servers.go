package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/docker"
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
	// DockerSecurity is how the daemon isolates containers; nil if unknown.
	DockerSecurity *docker.Security
	Restic         string // restic version, "" if backups cannot run
	Railpack       string // Railpack version, "" if apps cannot be built without a Dockerfile
	// DetectedIPs are public addresses on the host's interfaces; PublicIPs
	// are set by an admin and replace them when not empty.
	DetectedIPs []string
	PublicIPs   []string
	// HTTPSPort is the port its proxy serves HTTPS on.
	HTTPSPort int
	// PeerPort is the port of its agent's tunnel (0: off); PeerError, why
	// it could not listen.
	PeerPort  int
	PeerError string
	// Updatable: its agent can update Basalt there.
	Updatable bool
	MariaDB   string // version of the local MariaDB, "" if absent
	PHPLimits bool   // sites' PHP runs as systemd services with limits
	CreatedAt time.Time
}

// DNSAddrs are the addresses automatic DNS records point to: the ones an
// admin set, else the public ones on the host, else the public address
// the agent connects from (a server behind 1:1 NAT).
func (s *Server) DNSAddrs() []netip.Addr {
	parse := func(list []string) []netip.Addr {
		var out []netip.Addr
		for _, v := range list {
			if a, err := netip.ParseAddr(v); err == nil {
				out = append(out, a.Unmap())
			}
		}
		return out
	}
	if len(s.PublicIPs) > 0 {
		return parse(s.PublicIPs)
	}
	if len(s.DetectedIPs) > 0 {
		return parse(s.DetectedIPs)
	}
	if a, err := netip.ParseAddr(s.LastIP); err == nil && dns.PublicAddr(a) {
		return []netip.Addr{a.Unmap()}
	}
	return nil
}

var ErrNameTaken = errors.New("name already in use")

const serverColumns = `id, name, enrolled_at IS NOT NULL, enroll_expires_at, COALESCE(cert_fp, ''),
	COALESCE(prev_cert_fp, ''), cert_expires_at, hostname, os, arch, kernel, distro, agent_version,
	cpu_count, mem_total, last_seen_at, last_ip, php_versions, isolated, docker, docker_security, restic, railpack,
	detected_ips, public_ips, https_port, updatable, mariadb, php_limits, peer_port, peer_error, created_at`

func scanServer(row interface{ Scan(...any) error }) (*Server, error) {
	var s Server
	var enrollExp, certExp, lastSeen sql.NullInt64
	var created int64
	var phpVersions, dockerSec, detected, public string
	err := row.Scan(&s.ID, &s.Name, &s.Enrolled, &enrollExp, &s.CertFP, &s.PrevCertFP, &certExp,
		&s.Hostname, &s.OS, &s.Arch, &s.Kernel, &s.Distro, &s.AgentVersion,
		&s.CPUCount, &s.MemTotal, &lastSeen, &s.LastIP, &phpVersions, &s.Isolated, &s.Docker, &dockerSec, &s.Restic, &s.Railpack,
		&detected, &public, &s.HTTPSPort, &s.Updatable, &s.MariaDB, &s.PHPLimits, &s.PeerPort, &s.PeerError, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(phpVersions), &s.PHPVersions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(detected), &s.DetectedIPs); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(public), &s.PublicIPs); err != nil {
		return nil, err
	}
	if dockerSec != "" {
		s.DockerSecurity = &docker.Security{}
		if err := json.Unmarshal([]byte(dockerSec), s.DockerSecurity); err != nil {
			return nil, err
		}
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
	if s.DetectedIPs == nil {
		s.DetectedIPs = []string{}
	}
	detected, _ := json.Marshal(s.DetectedIPs)
	sec := ""
	if s.DockerSecurity != nil {
		b, _ := json.Marshal(s.DockerSecurity)
		sec = string(b)
	}
	return db.execOne(ctx,
		`UPDATE servers SET hostname = ?, os = ?, arch = ?, kernel = ?, distro = ?, agent_version = ?,
		 cpu_count = ?, mem_total = ?, last_ip = ?, last_seen_at = ?, php_versions = ?, isolated = ?, docker = ?,
		 docker_security = ?, detected_ips = ?, https_port = ?, updatable = ?, php_limits = ?, peer_port = ?, peer_error = ? WHERE id = ?`,
		s.Hostname, s.OS, s.Arch, s.Kernel, s.Distro, s.AgentVersion, s.CPUCount, s.MemTotal,
		s.LastIP, s.LastSeen.Unix(), string(php), s.Isolated, s.Docker, sec, string(detected), s.HTTPSPort, s.Updatable, s.PHPLimits,
		s.PeerPort, s.PeerError, id)
}

// SetServerPublicIPs sets the addresses automatic DNS records use for the
// server (empty: the detected ones).
func (db *DB) SetServerPublicIPs(ctx context.Context, id int64, ips []string) error {
	if ips == nil {
		ips = []string{}
	}
	b, _ := json.Marshal(ips)
	return db.execOne(ctx, `UPDATE servers SET public_ips = ? WHERE id = ?`, string(b), id)
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
	n, _ := res.RowsAffected()
	res, err = db.W.ExecContext(ctx, `DELETE FROM app_metrics WHERE at < ?`, before.Unix())
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// AppMetrics is one sample of an app's container usage.
type AppMetrics struct {
	AppID    int64
	At       time.Time
	CPUPct   float64
	MemUsed  uint64
	MemLimit uint64
	NetRx    uint64
	NetTx    uint64
}

// InsertAppMetrics stores samples reported by a server; samples of apps
// that are not on that server are ignored.
func (db *DB) InsertAppMetrics(ctx context.Context, serverID int64, samples []AppMetrics) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range samples {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO app_metrics (app_id, at, cpu_pct, mem_used, mem_limit, net_rx, net_tx)
			 SELECT id, ?, ?, ?, ?, ?, ? FROM apps WHERE id = ? AND server_id = ?`,
			m.At.Unix(), m.CPUPct, m.MemUsed, m.MemLimit, m.NetRx, m.NetTx, m.AppID, serverID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AppMetricsSince returns an app's samples newer than since, oldest first.
func (db *DB) AppMetricsSince(ctx context.Context, appID int64, since time.Time, limit int) ([]AppMetrics, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT at, cpu_pct, mem_used, mem_limit, net_rx, net_tx FROM app_metrics
		 WHERE app_id = ? AND at > ? ORDER BY at LIMIT ?`, appID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppMetrics
	for rows.Next() {
		m := AppMetrics{AppID: appID}
		var at int64
		if err := rows.Scan(&at, &m.CPUPct, &m.MemUsed, &m.MemLimit, &m.NetRx, &m.NetTx); err != nil {
			return nil, err
		}
		m.At = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetSettingIfAbsent stores value under key unless the key already exists.
func (db *DB) SetSettingIfAbsent(ctx context.Context, key, value string) error {
	_, err := db.W.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}

// DBTLS returns the certificate (DER) a server presents on its public
// database ports and its key, sealed; both nil when none was issued.
func (db *DB) DBTLS(ctx context.Context, serverID int64) (cert, sealedKey []byte, err error) {
	err = db.R.QueryRowContext(ctx, `SELECT db_tls_cert, db_tls_key FROM servers WHERE id = ?`, serverID).Scan(&cert, &sealedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return cert, sealedKey, err
}

// SetDBTLS stores a server's certificate for its public database ports.
func (db *DB) SetDBTLS(ctx context.Context, serverID int64, cert, sealedKey []byte) error {
	return db.execOne(ctx, `UPDATE servers SET db_tls_cert = ?, db_tls_key = ? WHERE id = ?`, cert, sealedKey, serverID)
}
