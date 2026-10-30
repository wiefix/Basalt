package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// A site or an app can be served by several servers: one route per server
// (the primary's on the owner's server_id), each following a health check
// once there is more than one, so that DNS answers only with the servers
// that work.

// Placement is a server a site or app is served by: its route there and
// the health check that route follows. A site's replica also records when
// its files last matched the primary's.
type Placement struct {
	ServerID    int64
	RouteID     int64
	Primary     bool
	HealthCheck int64  // 0: none
	CheckStatus string // of HealthCheck
	SyncedAt    *time.Time
	SyncError   string
}

// AppPlacement is a server an app runs on.
type AppPlacement = Placement

// placements lists the routes of the site or app (owner "site" or "app")
// id, the primary first.
func placements(ctx context.Context, q queryer, owner string, id int64) ([]Placement, error) {
	table, col := owner+"s", owner+"_id"
	rows, err := q.QueryContext(ctx,
		`SELECT r.server_id, r.id, r.server_id = o.server_id, COALESCE(r.health_check_id, 0), COALESCE(h.status, ''),
		        r.synced_at, r.sync_error
		 FROM proxy_routes r JOIN `+table+` o ON o.id = r.`+col+`
		 LEFT JOIN health_checks h ON h.id = r.health_check_id
		 WHERE r.`+col+` = ? ORDER BY r.server_id != o.server_id, r.server_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Placement
	for rows.Next() {
		var p Placement
		var synced sql.NullInt64
		if err := rows.Scan(&p.ServerID, &p.RouteID, &p.Primary, &p.HealthCheck, &p.CheckStatus, &synced, &p.SyncError); err != nil {
			return nil, err
		}
		p.SyncedAt = timePtr(synced)
		out = append(out, p)
	}
	return out, rows.Err()
}

func appRoutes(ctx context.Context, q queryer, appID int64) ([]Placement, error) {
	return placements(ctx, q, "app", appID)
}

func siteRoutes(ctx context.Context, q queryer, siteID int64) ([]Placement, error) {
	return placements(ctx, q, "site", siteID)
}

// AppPlacements lists the servers an app runs on, the primary first.
func (db *DB) AppPlacements(ctx context.Context, appID int64) ([]Placement, error) {
	return appRoutes(ctx, db.R, appID)
}

// SitePlacements lists the servers serving a site, the primary first.
func (db *DB) SitePlacements(ctx context.Context, siteID int64) ([]Placement, error) {
	return siteRoutes(ctx, db.R, siteID)
}

// dropRoutes removes the health checks of the routes of a deleted site or
// app (the routes go by cascade) and bumps the revisions; it returns the
// servers of the routes.
func dropRoutes(ctx context.Context, tx *sql.Tx, routes []Placement) ([]int64, error) {
	var servers []int64
	checks := false
	for _, r := range routes {
		servers = append(servers, r.ServerID)
		if r.HealthCheck != 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM health_checks WHERE id = ?`, r.HealthCheck); err != nil {
				return nil, err
			}
			checks = true
		}
		if err := bumpProxyRevision(ctx, tx, r.ServerID); err != nil {
			return nil, err
		}
	}
	if checks {
		if err := bumpHealthRevision(ctx, tx); err != nil {
			return nil, err
		}
	}
	return servers, nil
}

// ErrReplicaExists: the app already runs (or the site is served) there.
var ErrReplicaExists = errors.New("the app already runs on this server")

// AddAppReplica places an app on one more server: a route there with the
// app's domains, following check (created here). primaryCheck, when not
// nil, is created for the primary route too (the first replica).
func (db *DB) AddAppReplica(ctx context.Context, appID, serverID int64, check, primaryCheck *HealthCheck) error {
	return db.addReplica(ctx, "app", appID, serverID, check, primaryCheck)
}

// AddSiteReplica has one more server serve a site, as AddAppReplica.
func (db *DB) AddSiteReplica(ctx context.Context, siteID, serverID int64, check, primaryCheck *HealthCheck) error {
	return db.addReplica(ctx, "site", siteID, serverID, check, primaryCheck)
}

func (db *DB) addReplica(ctx context.Context, owner string, id, serverID int64, check, primaryCheck *HealthCheck) error {
	table, col := owner+"s", owner+"_id"
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var primaryRoute, projectID int64
	var tls, kind string
	err = tx.QueryRowContext(ctx,
		`SELECT r.id, r.tls, r.kind, o.project_id FROM `+table+` o JOIN proxy_routes r ON r.`+col+` = o.id AND r.server_id = o.server_id WHERE o.id = ?`,
		id).Scan(&primaryRoute, &tls, &kind, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// The checks of its placements belong to the owner's project.
	check.ProjectID = projectID
	if err := insertHealthCheck(ctx, tx, check); err != nil {
		return err
	}
	if primaryCheck != nil {
		primaryCheck.ProjectID = projectID
		if err := insertHealthCheck(ctx, tx, primaryCheck); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_routes SET health_check_id = ? WHERE id = ?`, primaryCheck.ID, primaryRoute); err != nil {
			return err
		}
	}
	var routeID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO proxy_routes (server_id, kind, tls, `+col+`, health_check_id) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		serverID, kind, tls, id, check.ID).Scan(&routeID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrReplicaExists
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proxy_route_domains (server_id, domain, route_id, service) SELECT ?, domain, ?, service FROM proxy_route_domains WHERE route_id = ?`,
		serverID, routeID, primaryRoute); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return errors.New("one of the " + owner + "'s domains is already served by something else on that server")
		}
		return err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return err
	}
	if err := bumpHealthRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveAppReplica takes an app off a server other than its primary one,
// with the health check of that placement. Once no replica is left, the
// primary's check goes too.
func (db *DB) RemoveAppReplica(ctx context.Context, appID, serverID int64) error {
	return db.removeReplica(ctx, "app", appID, serverID)
}

// RemoveSiteReplica stops a server other than the primary serving a site,
// as RemoveAppReplica.
func (db *DB) RemoveSiteReplica(ctx context.Context, siteID, serverID int64) error {
	return db.removeReplica(ctx, "site", siteID, serverID)
}

func (db *DB) removeReplica(ctx context.Context, owner string, id, serverID int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	routes, err := placements(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	var drop []int64 // health checks
	found := false
	for _, r := range routes {
		if r.ServerID != serverID || r.Primary {
			continue
		}
		found = true
		if err := dropServiceChecks(ctx, tx, r.RouteID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_routes WHERE id = ?`, r.RouteID); err != nil {
			return err
		}
		if r.HealthCheck != 0 {
			drop = append(drop, r.HealthCheck)
		}
	}
	if !found {
		return ErrNotFound
	}
	if len(routes) == 2 && routes[0].Primary {
		if routes[0].HealthCheck != 0 {
			drop = append(drop, routes[0].HealthCheck)
		}
		if err := dropServiceChecks(ctx, tx, routes[0].RouteID); err != nil {
			return err
		}
	}
	for _, id := range drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM health_checks WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return err
	}
	if err := bumpHealthRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSiteSync records how the last copy of a site's files to a replica
// went: errMsg "" means the files matched at at.
func (db *DB) SetSiteSync(ctx context.Context, siteID, serverID int64, at time.Time, errMsg string) error {
	if errMsg != "" {
		return db.execOne(ctx, `UPDATE proxy_routes SET sync_error = ? WHERE site_id = ? AND server_id = ?`, errMsg, siteID, serverID)
	}
	return db.execOne(ctx, `UPDATE proxy_routes SET synced_at = ?, sync_error = '' WHERE site_id = ? AND server_id = ?`,
		at.Unix(), siteID, serverID)
}

// ReplicatedSites lists the sites served by more than one server.
func (db *DB) ReplicatedSites(ctx context.Context) ([]*Site, error) {
	rows, err := db.R.QueryContext(ctx, siteQuery+
		` WHERE s.id IN (SELECT site_id FROM proxy_routes WHERE site_id IS NOT NULL GROUP BY site_id HAVING COUNT(*) > 1)
		 ORDER BY s.id, d.domain`)
	if err != nil {
		return nil, err
	}
	return db.scanSites(rows)
}

// SiteReplicasOn lists the sites a server serves as a replica.
func (db *DB) SiteReplicasOn(ctx context.Context, serverID int64) ([]*Site, error) {
	rows, err := db.R.QueryContext(ctx, siteQuery+
		` WHERE s.server_id != ? AND s.id IN (SELECT site_id FROM proxy_routes WHERE server_id = ?) ORDER BY s.id, d.domain`,
		serverID, serverID)
	if err != nil {
		return nil, err
	}
	return db.scanSites(rows)
}

// SetSitePrimary makes a server already serving a copy of a site its
// primary: the site's files and databases are its from now on (the former
// primary keeps its route, as a replica).
func (db *DB) SetSitePrimary(ctx context.Context, siteID, serverID int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	routes, err := siteRoutes(ctx, tx, siteID)
	if err != nil {
		return err
	}
	var old int64
	found := false
	for _, r := range routes {
		if r.Primary {
			old = r.ServerID
		}
		if r.ServerID == serverID && !r.Primary {
			found = true
		}
	}
	if !found {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sites SET server_id = ? WHERE id = ?`, serverID, siteID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE databases SET server_id = ? WHERE site_id = ?`, serverID, siteID); err != nil {
		return err
	}
	// The former primary's copy of the files is copied over from now on.
	if _, err := tx.ExecContext(ctx, `UPDATE proxy_routes SET synced_at = NULL, sync_error = '' WHERE site_id = ?`, siteID); err != nil {
		return err
	}
	for _, id := range []int64{old, serverID} {
		if err := bumpProxyRevision(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE servers SET sftp_revision = sftp_revision + 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ServiceCheck is the health check a server's copy of another service of a
// compose app (one with domains of its own) follows, as the route's own
// check does for the published service.
type ServiceCheck struct {
	ServerID    int64
	RouteID     int64
	Service     string
	HealthCheck int64
	CheckStatus string
	HostHeader  string
}

func serviceChecks(ctx context.Context, q queryer, appID int64) ([]ServiceCheck, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT r.server_id, r.id, c.service, c.health_check_id, h.status, h.host_header
		 FROM proxy_route_checks c JOIN proxy_routes r ON r.id = c.route_id JOIN health_checks h ON h.id = c.health_check_id
		 WHERE r.app_id = ? ORDER BY r.server_id, c.service`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceCheck{}
	for rows.Next() {
		var c ServiceCheck
		if err := rows.Scan(&c.ServerID, &c.RouteID, &c.Service, &c.HealthCheck, &c.CheckStatus, &c.HostHeader); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppServiceChecks lists the checks of an app's other services, on every
// server it runs on.
func (db *DB) AppServiceChecks(ctx context.Context, appID int64) ([]ServiceCheck, error) {
	return serviceChecks(ctx, db.R, appID)
}

// SetServiceChecks makes the checks of an app's other services those of
// want (route ID → service → check, created here). A check already there
// stays, with its status, unless want asks another host; the others go.
// It reports whether anything changed.
func (db *DB) SetServiceChecks(ctx context.Context, appID int64, want map[int64]map[string]*HealthCheck) (bool, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM apps WHERE id = ?`, appID).Scan(&projectID); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	routes, err := appRoutes(ctx, tx, appID)
	if err != nil {
		return false, err
	}
	have, err := serviceChecks(ctx, tx, appID)
	if err != nil {
		return false, err
	}
	kept := map[int64]map[string]bool{}
	changed := false
	for _, c := range have {
		if w := want[c.RouteID][c.Service]; w != nil && w.HostHeader == c.HostHeader {
			if kept[c.RouteID] == nil {
				kept[c.RouteID] = map[string]bool{}
			}
			kept[c.RouteID][c.Service] = true
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM health_checks WHERE id = ?`, c.HealthCheck); err != nil {
			return false, err
		}
		changed = true
	}
	for _, r := range routes {
		for service, check := range want[r.RouteID] {
			if kept[r.RouteID][service] {
				continue
			}
			check.ProjectID = projectID
			if err := insertHealthCheck(ctx, tx, check); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_route_checks (route_id, service, health_check_id) VALUES (?, ?, ?)`,
				r.RouteID, service, check.ID); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := bumpHealthRevision(ctx, tx); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// dropServiceChecks deletes the checks of the other services on a route
// about to go (the rows would go by cascade, the checks would stay).
func dropServiceChecks(ctx context.Context, tx *sql.Tx, routeID int64) error {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM health_checks WHERE id IN (SELECT health_check_id FROM proxy_route_checks WHERE route_id = ?)`, routeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return bumpHealthRevision(ctx, tx)
	}
	return nil
}
