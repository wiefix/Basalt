package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/proxy"
)

type Site struct {
	ID         int64
	ServerID   int64
	ProjectID  int64
	Name       string
	Kind       string
	RouteID    int64
	Domains    []string
	TLS        proxy.TLSMode
	PHPVersion string
	PHP        php.Settings
	Usage      SiteUsage
	// QuotaMB is a soft limit on Usage (0: none); QuotaAlerted, that going
	// over it was reported.
	QuotaMB      int
	QuotaAlerted bool
	CreatedAt    time.Time
}

// SiteUsage is the disk space a site took when last measured.
type SiteUsage struct {
	DiskBytes int64
	DiskFiles int64
	DBBytes   int64
	At        *time.Time
}

// Total is files and databases together.
func (u SiteUsage) Total() int64 { return u.DiskBytes + u.DBBytes }

// CreateSite creates a site and the route that serves it, in one transaction.
func (db *DB) CreateSite(ctx context.Context, projectID, serverID int64, name string, domains []string, tls proxy.TLSMode, phpVersion string) (*Site, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	projectID = projectOrDefault(projectID)
	if err := checkRoom(ctx, tx, projectID, "sites"); err != nil {
		return nil, err
	}
	site := &Site{ServerID: serverID, ProjectID: projectID, Name: name, Kind: "static", Domains: domains, TLS: tls, PHPVersion: phpVersion}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO sites (project_id, server_id, name, kind, php_version) VALUES (?, ?, ?, 'static', ?) RETURNING id, created_at`,
		projectID, serverID, name, phpVersion).Scan(&site.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: sites.name") {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	site.CreatedAt = time.Unix(created, 0)

	err = tx.QueryRowContext(ctx,
		`INSERT INTO proxy_routes (server_id, kind, tls, site_id) VALUES (?, 'static', ?, ?) RETURNING id`,
		serverID, tls, site.ID).Scan(&site.RouteID)
	if err != nil {
		return nil, err
	}
	if err := insertDomains(ctx, tx, serverID, site.RouteID, domains); err != nil {
		return nil, err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return nil, err
	}
	return site, tx.Commit()
}

func insertDomains(ctx context.Context, tx *sql.Tx, serverID, routeID int64, domains []string) error {
	for _, d := range domains {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO proxy_route_domains (server_id, domain, route_id) VALUES (?, ?, ?)`, serverID, d, routeID)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return &ErrDomainTaken{Domain: d}
			}
			return err
		}
	}
	return nil
}

// SetSiteDomains replaces the domains a site answers to.
func (db *DB) SetSiteDomains(ctx context.Context, siteID int64, domains []string) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var serverID, routeID int64
	err = tx.QueryRowContext(ctx,
		`SELECT s.server_id, r.id FROM sites s JOIN proxy_routes r ON r.site_id = s.id WHERE s.id = ?`,
		siteID).Scan(&serverID, &routeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_route_domains WHERE route_id = ?`, routeID); err != nil {
		return err
	}
	if err := insertDomains(ctx, tx, serverID, routeID, domains); err != nil {
		return err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return err
	}
	return tx.Commit()
}

const siteQuery = `SELECT s.id, s.server_id, s.project_id, s.name, s.kind, s.php_version, s.php_settings,
	s.disk_bytes, s.disk_files, s.db_bytes, s.usage_at, s.quota_mb, s.quota_alerted,
	s.created_at, r.id, r.tls, COALESCE(d.domain, '')
	FROM sites s JOIN proxy_routes r ON r.site_id = s.id
	LEFT JOIN proxy_route_domains d ON d.route_id = r.id`

func (db *DB) scanSites(rows *sql.Rows) ([]*Site, error) {
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		var s Site
		var created int64
		var domain, settings string
		var usageAt sql.NullInt64
		if err := rows.Scan(&s.ID, &s.ServerID, &s.ProjectID, &s.Name, &s.Kind, &s.PHPVersion, &settings,
			&s.Usage.DiskBytes, &s.Usage.DiskFiles, &s.Usage.DBBytes, &usageAt, &s.QuotaMB, &s.QuotaAlerted,
			&created, &s.RouteID, &s.TLS, &domain); err != nil {
			return nil, err
		}
		s.Usage.At = timePtr(usageAt)
		if err := json.Unmarshal([]byte(settings), &s.PHP); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].ID == s.ID {
			out[n-1].Domains = append(out[n-1].Domains, domain)
			continue
		}
		s.CreatedAt = time.Unix(created, 0)
		s.Domains = []string{}
		if domain != "" {
			s.Domains = append(s.Domains, domain)
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

func (db *DB) Sites(ctx context.Context) ([]*Site, error) {
	rows, err := db.R.QueryContext(ctx, siteQuery+` ORDER BY s.name, s.id, d.domain`)
	if err != nil {
		return nil, err
	}
	return db.scanSites(rows)
}

func (db *DB) SitesOnServer(ctx context.Context, serverID int64) ([]*Site, error) {
	rows, err := db.R.QueryContext(ctx, siteQuery+` WHERE s.server_id = ? ORDER BY s.id, d.domain`, serverID)
	if err != nil {
		return nil, err
	}
	return db.scanSites(rows)
}

func (db *DB) SiteByID(ctx context.Context, id int64) (*Site, error) {
	rows, err := db.R.QueryContext(ctx, siteQuery+` WHERE s.id = ? ORDER BY d.domain`, id)
	if err != nil {
		return nil, err
	}
	sites, err := db.scanSites(rows)
	if err != nil {
		return nil, err
	}
	if len(sites) == 0 {
		return nil, ErrNotFound
	}
	return sites[0], nil
}

// DeleteSite removes the site and its route (by cascade).
func (db *DB) DeleteSite(ctx context.Context, id int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM databases WHERE site_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSiteHasDatabases
	}
	var serverID int64
	err = tx.QueryRowContext(ctx, `DELETE FROM sites WHERE id = ? RETURNING server_id`, id).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSiteUsage records a measurement of a site's disk space.
func (db *DB) SetSiteUsage(ctx context.Context, siteID int64, u SiteUsage) error {
	var at any
	if u.At != nil {
		at = u.At.Unix()
	}
	return db.execOne(ctx, `UPDATE sites SET disk_bytes = ?, disk_files = ?, db_bytes = ?, usage_at = ? WHERE id = ?`,
		u.DiskBytes, u.DiskFiles, u.DBBytes, at, siteID)
}

// SetSiteQuota sets a site's soft quota (0: none) and forgets an earlier
// report of going over it.
func (db *DB) SetSiteQuota(ctx context.Context, siteID int64, mb int) error {
	return db.execOne(ctx, `UPDATE sites SET quota_mb = ?, quota_alerted = 0 WHERE id = ?`, mb, siteID)
}

// SetSiteQuotaAlerted records whether going over the quota was reported.
func (db *DB) SetSiteQuotaAlerted(ctx context.Context, siteID int64, alerted bool) error {
	return db.execOne(ctx, `UPDATE sites SET quota_alerted = ? WHERE id = ?`, alerted, siteID)
}

// SetSitePHPSettings stores a site's PHP options.
func (db *DB) SetSitePHPSettings(ctx context.Context, siteID int64, st php.Settings) error {
	b, _ := json.Marshal(st)
	return db.execOne(ctx, `UPDATE sites SET php_settings = ? WHERE id = ?`, string(b), siteID)
}

// SetSitePHP switches a site between static ("") and a PHP version.
func (db *DB) SetSitePHP(ctx context.Context, siteID int64, version string) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var serverID int64
	err = tx.QueryRowContext(ctx, `UPDATE sites SET php_version = ? WHERE id = ? RETURNING server_id`, version, siteID).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return err
	}
	return tx.Commit()
}
