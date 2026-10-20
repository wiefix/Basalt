package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wiefix/basalt/internal/proxy"
)

type ProxyRoute struct {
	proxy.Route
	ServerID  int64
	CreatedAt time.Time
}

// ErrDomainTaken is returned when a domain is already routed on the server.
type ErrDomainTaken struct{ Domain string }

func (e *ErrDomainTaken) Error() string {
	return fmt.Sprintf("domain %s is already used on this server", e.Domain)
}

// CreateRoute adds a validated route and bumps the server's proxy revision.
func (db *DB) CreateRoute(ctx context.Context, serverID int64, r proxy.Route) (*ProxyRoute, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id, created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO proxy_routes (server_id, kind, root, upstream, tls, hsts) VALUES (?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		serverID, r.Kind, r.Root, r.Upstream, r.TLS, r.HSTS).Scan(&id, &created)
	if err != nil {
		return nil, err
	}
	if err := insertDomains(ctx, tx, serverID, id, r.Domains); err != nil {
		return nil, err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r.ID = id
	return &ProxyRoute{Route: r, ServerID: serverID, CreatedAt: time.Unix(created, 0)}, nil
}

func (db *DB) DeleteRoute(ctx context.Context, serverID, routeID int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Site and app routes are removed together with their site or app.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM proxy_routes WHERE id = ? AND server_id = ? AND site_id IS NULL AND app_id IS NULL`, routeID, serverID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return err
	}
	return tx.Commit()
}

func bumpProxyRevision(ctx context.Context, tx *sql.Tx, serverID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE servers SET proxy_revision = proxy_revision + 1 WHERE id = ?`, serverID)
	return err
}

// Routes returns a server's routes, ordered by ID, with sorted domains.
func (db *DB) Routes(ctx context.Context, serverID int64) ([]*ProxyRoute, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT r.id, r.kind, r.root, r.upstream, r.tls, r.hsts, r.created_at, COALESCE(r.site_id, 0), COALESCE(r.app_id, 0),
		        COALESCE(s.php_version, '') != '',
		        (r.app_id IS NOT NULL AND (SELECT count(*) FROM proxy_routes o WHERE o.app_id = r.app_id) > 1) OR
		        (r.site_id IS NOT NULL AND (SELECT count(*) FROM proxy_routes o WHERE o.site_id = r.site_id) > 1), d.domain
		 FROM proxy_routes r JOIN proxy_route_domains d ON d.route_id = r.id
		 LEFT JOIN sites s ON s.id = r.site_id
		 WHERE r.server_id = ? ORDER BY r.id, d.domain`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProxyRoute
	for rows.Next() {
		var r ProxyRoute
		var created int64
		var domain string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Root, &r.Upstream, &r.TLS, &r.HSTS, &created, &r.SiteID, &r.AppID, &r.PHP, &r.DNSChallenge, &domain); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].ID == r.ID {
			out[n-1].Domains = append(out[n-1].Domains, domain)
			continue
		}
		r.ServerID, r.CreatedAt, r.Domains = serverID, time.Unix(created, 0), []string{domain}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DesiredProxy assembles the full proxy state for a server.
func (db *DB) DesiredProxy(ctx context.Context, serverID int64) (proxy.Desired, error) {
	var d proxy.Desired
	err := db.R.QueryRowContext(ctx, `SELECT proxy_revision FROM servers WHERE id = ?`, serverID).Scan(&d.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	routes, err := db.Routes(ctx, serverID)
	if err != nil {
		return d, err
	}
	d.Routes = make([]proxy.Route, len(routes))
	for i, r := range routes {
		d.Routes[i] = r.Route
	}
	d.ACMEEmail, err = db.ACMEEmail(ctx)
	return d, err
}

// ACMEEmail is the contact address for certificate authorities: the oldest
// owner's email.
func (db *DB) ACMEEmail(ctx context.Context) (string, error) {
	var email string
	err := db.R.QueryRowContext(ctx,
		`SELECT email FROM users WHERE role = 'owner' AND disabled_at IS NULL ORDER BY id LIMIT 1`).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return email, err
}

// SetProxyStatus records the result of delivering revision rev.
func (db *DB) SetProxyStatus(ctx context.Context, serverID, rev int64, applyErr string) error {
	if applyErr != "" {
		_, err := db.W.ExecContext(ctx, `UPDATE servers SET proxy_error = ? WHERE id = ?`, applyErr, serverID)
		return err
	}
	_, err := db.W.ExecContext(ctx,
		`UPDATE servers SET proxy_applied_revision = MAX(proxy_applied_revision, ?), proxy_error = '' WHERE id = ?`,
		rev, serverID)
	return err
}

type ProxyStatus struct {
	Revision        int64
	AppliedRevision int64
	Error           string
}

func (db *DB) ProxyStatus(ctx context.Context, serverID int64) (ProxyStatus, error) {
	var s ProxyStatus
	err := db.R.QueryRowContext(ctx,
		`SELECT proxy_revision, proxy_applied_revision, proxy_error FROM servers WHERE id = ?`, serverID).
		Scan(&s.Revision, &s.AppliedRevision, &s.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}
