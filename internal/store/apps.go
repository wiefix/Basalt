package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

type App struct {
	ID        int64
	ServerID  int64
	ProjectID int64
	Name      string
	Source    string // "image", "git", "compose", "database" or "mail"
	Image     string // for source "image"
	Git       GitSettings
	// Deploy key (encrypted OpenSSH private key and its public line) and
	// webhook secret (encrypted) of a git app; nil until generated.
	DeployKey       []byte
	DeployKeyPublic string
	WebhookSecret   []byte
	AutoDeploy      bool

	// Compose apps: the encrypted compose file, a summary of it for
	// display, and the service published on the app's domains and Port.
	ComposeSealed []byte
	Summary       ComposeSummary
	Service       string

	// Databases: engine and major version, and the apps whose networks the
	// database joins (so they reach it by its name).
	Engine        string
	EngineVersion string
	Links         []int64

	// Hostname is the mail server's host name (source "mail").
	Hostname string

	// Template is the catalog template a compose app was installed from
	// ("" for none) and TemplateRevision its revision then.
	Template         string
	TemplateRevision int

	Port      int
	EnvSealed []byte // encrypted JSON object; nil when empty
	Volumes   []agentproto.AppVolume
	MemoryMB  int
	CPUs      float64
	ReadOnly  bool  // read-only root filesystem (image and git apps)
	RouteID   int64 // the route on the primary server
	Domains   []string
	// Replicas are the other servers the app runs on (image, git and compose
	// apps without volumes).
	Replicas  []int64
	TLS       proxy.TLSMode
	CreatedAt time.Time
}

// GitSettings locate the Dockerfile an app is built from.
type GitSettings struct {
	URL        string
	Branch     string
	Dockerfile string // path in the repository
	Context    string // build context directory in the repository
}

const (
	SourceImage    = "image"
	SourceGit      = "git"
	SourceCompose  = "compose"
	SourceDatabase = "database"
	SourceMail     = "mail"
)

// HasServices reports whether the app runs as named services (compose
// apps, databases and the mail server) rather than one anonymous container.
func (a *App) HasServices() bool {
	return a.Source == SourceCompose || a.Source == SourceDatabase || a.Source == SourceMail
}

// ComposeSummary describes a compose file without its secrets.
type ComposeSummary struct {
	Services []ComposeService `json:"services"`
	Volumes  []string         `json:"volumes"`
}

type ComposeService struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Ports []int  `json:"ports,omitempty"` // container ports mentioned in the file
	Job   bool   `json:"job,omitempty"`
}

// AppSettings are the fields an app is deployed with, other than env.
// Source is fixed when the app is created.
type AppSettings struct {
	// ProjectID is the project of a new app (0: the default one); updates
	// leave it alone.
	ProjectID     int64
	Source        string
	Engine        string // databases
	EngineVersion string
	Hostname      string // mail server
	// Template and TemplateRevision: see App.
	Template         string
	TemplateRevision int
	Image            string
	Git              GitSettings
	Port             int
	Volumes          []agentproto.AppVolume
	MemoryMB         int
	CPUs             float64
	ReadOnly         bool
}

var (
	ErrDeployInProgress = errors.New("a deployment of this app is already running")
	ErrMailServerExists = errors.New("a mail server already exists")
)

// CreateApp creates an app and the route that serves it (without domains
// the route is inert), in one transaction.
func (db *DB) CreateApp(ctx context.Context, serverID int64, name string, st AppSettings, domains []string, tls proxy.TLSMode) (*App, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	vols, err := marshalVolumes(st.Volumes)
	if err != nil {
		return nil, err
	}
	if st.Source == "" {
		st.Source = SourceImage
	}
	st.ProjectID = projectOrDefault(st.ProjectID)
	if err := projectExists(ctx, tx, st.ProjectID); err != nil {
		return nil, err
	}
	if what := appKind(st.Source); what != "" {
		if err := checkRoom(ctx, tx, st.ProjectID, what); err != nil {
			return nil, err
		}
	}
	a := &App{ServerID: serverID, ProjectID: st.ProjectID, Name: name, Source: st.Source, Image: st.Image, Git: st.Git, Port: st.Port,
		Volumes: st.Volumes, MemoryMB: st.MemoryMB, CPUs: st.CPUs, Domains: domains, TLS: tls,
		Engine: st.Engine, EngineVersion: st.EngineVersion, Links: []int64{}, ReadOnly: st.ReadOnly, Hostname: st.Hostname,
		Template: st.Template, TemplateRevision: st.TemplateRevision}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO apps (project_id, server_id, name, source, image, git_url, git_branch, git_dockerfile, git_context,
		 port, volumes, memory_mb, cpus, engine, engine_version, read_only, hostname, template, template_revision)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		st.ProjectID, serverID, name, st.Source, st.Image, st.Git.URL, st.Git.Branch, st.Git.Dockerfile, st.Git.Context,
		st.Port, vols, st.MemoryMB, st.CPUs, st.Engine, st.EngineVersion, st.ReadOnly, st.Hostname, st.Template, st.TemplateRevision).Scan(&a.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: apps.name") {
			return nil, ErrNameTaken
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed: apps.source") {
			return nil, ErrMailServerExists
		}
		return nil, err
	}
	a.CreatedAt = time.Unix(created, 0)
	err = tx.QueryRowContext(ctx,
		`INSERT INTO proxy_routes (server_id, kind, tls, app_id) VALUES (?, 'reverse_proxy', ?, ?) RETURNING id`,
		serverID, tls, a.ID).Scan(&a.RouteID)
	if err != nil {
		return nil, err
	}
	if err := insertDomains(ctx, tx, serverID, a.RouteID, domains); err != nil {
		return nil, err
	}
	if err := bumpProxyRevision(ctx, tx, serverID); err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

func marshalVolumes(v []agentproto.AppVolume) (string, error) {
	if v == nil {
		v = []agentproto.AppVolume{}
	}
	b, err := json.Marshal(v)
	return string(b), err
}

const appQuery = `SELECT a.id, a.server_id, a.project_id, a.name, a.source, a.image, a.git_url, a.git_branch, a.git_dockerfile,
	a.git_context, a.deploy_key, a.deploy_key_public, a.webhook_secret, a.auto_deploy,
	a.compose, a.compose_summary, a.service, a.engine, a.engine_version, a.hostname, a.template, a.template_revision,
	(SELECT COALESCE(json_group_array(l.app_id), '[]') FROM (SELECT app_id FROM app_links WHERE database_id = a.id ORDER BY app_id) l),
	(SELECT COALESCE(json_group_array(p.server_id), '[]') FROM (SELECT server_id FROM proxy_routes
	 WHERE app_id = a.id AND server_id != a.server_id ORDER BY server_id) p),
	a.port, a.env, a.volumes, a.memory_mb, a.cpus, a.read_only, a.created_at, r.id, r.tls, COALESCE(d.domain, '')
	FROM apps a JOIN proxy_routes r ON r.app_id = a.id AND r.server_id = a.server_id
	LEFT JOIN proxy_route_domains d ON d.route_id = r.id`

func scanApps(rows *sql.Rows) ([]*App, error) {
	defer rows.Close()
	var out []*App
	for rows.Next() {
		var a App
		var created int64
		var vols, domain, summary, links, replicas string
		if err := rows.Scan(&a.ID, &a.ServerID, &a.ProjectID, &a.Name, &a.Source, &a.Image, &a.Git.URL, &a.Git.Branch,
			&a.Git.Dockerfile, &a.Git.Context, &a.DeployKey, &a.DeployKeyPublic, &a.WebhookSecret, &a.AutoDeploy,
			&a.ComposeSealed, &summary, &a.Service, &a.Engine, &a.EngineVersion, &a.Hostname, &a.Template, &a.TemplateRevision, &links, &replicas, &a.Port, &a.EnvSealed, &vols, &a.MemoryMB, &a.CPUs, &a.ReadOnly, &created, &a.RouteID, &a.TLS, &domain); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].ID == a.ID {
			out[n-1].Domains = append(out[n-1].Domains, domain)
			continue
		}
		if err := json.Unmarshal([]byte(vols), &a.Volumes); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(summary), &a.Summary); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(links), &a.Links); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(replicas), &a.Replicas); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		a.Domains = []string{}
		if domain != "" {
			a.Domains = append(a.Domains, domain)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (db *DB) Apps(ctx context.Context) ([]*App, error) {
	rows, err := db.R.QueryContext(ctx, appQuery+` ORDER BY a.name, a.id, d.domain`)
	if err != nil {
		return nil, err
	}
	return scanApps(rows)
}

func (db *DB) AppByID(ctx context.Context, id int64) (*App, error) {
	rows, err := db.R.QueryContext(ctx, appQuery+` WHERE a.id = ? ORDER BY d.domain`, id)
	if err != nil {
		return nil, err
	}
	apps, err := scanApps(rows)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, ErrNotFound
	}
	return apps[0], nil
}

// AppIDsOnServer lists the IDs of all apps placed on a server, replicas
// included.
func (db *DB) AppIDsOnServer(ctx context.Context, serverID int64) ([]int64, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id FROM apps WHERE server_id = ?
		 UNION SELECT app_id FROM proxy_routes WHERE server_id = ? AND app_id IS NOT NULL ORDER BY 1`, serverID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UpdateAppSettings changes what the next deployment runs.
func (db *DB) UpdateAppSettings(ctx context.Context, id int64, st AppSettings) error {
	vols, err := marshalVolumes(st.Volumes)
	if err != nil {
		return err
	}
	return db.execOne(ctx,
		`UPDATE apps SET image = ?, git_url = ?, git_branch = ?, git_dockerfile = ?, git_context = ?,
		 port = ?, volumes = ?, memory_mb = ?, cpus = ?, read_only = ? WHERE id = ?`,
		st.Image, st.Git.URL, st.Git.Branch, st.Git.Dockerfile, st.Git.Context, st.Port, vols, st.MemoryMB, st.CPUs, st.ReadOnly, id)
}

// SetAppCompose stores a compose app's file (encrypted), its summary and
// the service published on the app's port.
func (db *DB) SetAppCompose(ctx context.Context, id int64, sealed []byte, sum ComposeSummary, service string, port int) error {
	b, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	return db.execOne(ctx, `UPDATE apps SET compose = ?, compose_summary = ?, service = ?, port = ? WHERE id = ?`,
		sealed, string(b), service, port, id)
}

// LinkApp makes a database reachable from an app.
func (db *DB) LinkApp(ctx context.Context, databaseID, appID int64) error {
	_, err := db.W.ExecContext(ctx, `INSERT OR IGNORE INTO app_links (database_id, app_id) VALUES (?, ?)`, databaseID, appID)
	return err
}

// UnlinkApp undoes LinkApp.
func (db *DB) UnlinkApp(ctx context.Context, databaseID, appID int64) error {
	return db.execOne(ctx, `DELETE FROM app_links WHERE database_id = ? AND app_id = ?`, databaseID, appID)
}

// LinkedDatabaseNames lists the names of the databases linked to an app:
// host names on its network.
func (db *DB) LinkedDatabaseNames(ctx context.Context, appID int64) ([]string, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT a.name FROM app_links l JOIN apps a ON a.id = l.database_id WHERE l.app_id = ? ORDER BY a.name`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetAppDeployKey stores a new deploy key (private half encrypted).
func (db *DB) SetAppDeployKey(ctx context.Context, id int64, sealed []byte, public string) error {
	return db.execOne(ctx, `UPDATE apps SET deploy_key = ?, deploy_key_public = ? WHERE id = ?`, sealed, public, id)
}

// SetAppWebhook stores the webhook secret (encrypted) and the auto-deploy switch.
func (db *DB) SetAppWebhook(ctx context.Context, id int64, sealed []byte, autoDeploy bool) error {
	return db.execOne(ctx, `UPDATE apps SET webhook_secret = ?, auto_deploy = ? WHERE id = ?`, sealed, autoDeploy, id)
}

// SetAppEnv stores the encrypted environment (nil clears it).
func (db *DB) SetAppEnv(ctx context.Context, id int64, sealed []byte) error {
	return db.execOne(ctx, `UPDATE apps SET env = ? WHERE id = ?`, sealed, id)
}

// SetAppDomains replaces the domains an app answers to, on every server
// it runs on.
func (db *DB) SetAppDomains(ctx context.Context, appID int64, domains []string) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	routes, err := appRoutes(ctx, tx, appID)
	if err != nil {
		return err
	}
	if len(routes) == 0 {
		return ErrNotFound
	}
	for _, r := range routes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM proxy_route_domains WHERE route_id = ?`, r.RouteID); err != nil {
			return err
		}
		if err := insertDomains(ctx, tx, r.ServerID, r.RouteID, domains); err != nil {
			return err
		}
		if err := bumpProxyRevision(ctx, tx, r.ServerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteApp removes the app, its routes and deployments (by cascade) and
// the health checks of its placements. It returns the servers it ran on.
func (db *DB) DeleteApp(ctx context.Context, id int64) ([]int64, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	routes, err := appRoutes(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, id); err != nil {
		return nil, err
	}
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
	return servers, tx.Commit()
}

// AppPlacement is a server an app runs on: its route there and the health
// check that route follows.
type AppPlacement struct {
	ServerID    int64
	RouteID     int64
	Primary     bool
	HealthCheck int64  // 0: none
	CheckStatus string // of HealthCheck
}

func appRoutes(ctx context.Context, q queryer, appID int64) ([]AppPlacement, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT r.server_id, r.id, r.server_id = a.server_id, COALESCE(r.health_check_id, 0), COALESCE(h.status, '')
		 FROM proxy_routes r JOIN apps a ON a.id = r.app_id
		 LEFT JOIN health_checks h ON h.id = r.health_check_id
		 WHERE r.app_id = ? ORDER BY r.server_id != a.server_id, r.server_id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppPlacement
	for rows.Next() {
		var p AppPlacement
		if err := rows.Scan(&p.ServerID, &p.RouteID, &p.Primary, &p.HealthCheck, &p.CheckStatus); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AppPlacements lists the servers an app runs on, the primary first.
func (db *DB) AppPlacements(ctx context.Context, appID int64) ([]AppPlacement, error) {
	return appRoutes(ctx, db.R, appID)
}

// ErrReplicaExists: the app already runs on that server.
var ErrReplicaExists = errors.New("the app already runs on this server")

// AddAppReplica places an app on one more server: a route there with the
// app's domains, following check (created here). primaryCheck, when not
// nil, is created for the primary route too (the first replica).
func (db *DB) AddAppReplica(ctx context.Context, appID, serverID int64, check, primaryCheck *HealthCheck) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var primaryRoute, projectID int64
	var tls string
	err = tx.QueryRowContext(ctx,
		`SELECT r.id, r.tls, a.project_id FROM apps a JOIN proxy_routes r ON r.app_id = a.id AND r.server_id = a.server_id WHERE a.id = ?`,
		appID).Scan(&primaryRoute, &tls, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// The checks of its placements belong to the app's project.
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
		`INSERT INTO proxy_routes (server_id, kind, tls, app_id, health_check_id) VALUES (?, 'reverse_proxy', ?, ?, ?) RETURNING id`,
		serverID, tls, appID, check.ID).Scan(&routeID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrReplicaExists
		}
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proxy_route_domains (server_id, domain, route_id) SELECT ?, domain, ? FROM proxy_route_domains WHERE route_id = ?`,
		serverID, routeID, primaryRoute); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return errors.New("one of the app's domains is already served by something else on that server")
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
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	routes, err := appRoutes(ctx, tx, appID)
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
	if len(routes) == 2 && routes[0].Primary && routes[0].HealthCheck != 0 {
		drop = append(drop, routes[0].HealthCheck)
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

// --- deployments ---

type Deployment struct {
	ID            int64
	AppID         int64
	Image         string
	CommitSHA     string // git apps
	CommitMessage string
	Status        string // running, succeeded, failed
	Error         string
	Log           string
	ImageDigest   string
	// Compose apps: the compose file deployed (encrypted) and the image
	// each service ran.
	ComposeSealed []byte
	Digests       map[string]string
	ActorID       *int64
	CreatedAt     time.Time
	FinishedAt    *time.Time
}

// CreateDeployment records a running deployment, or returns
// ErrDeployInProgress if one is already running for the app.
func (db *DB) CreateDeployment(ctx context.Context, appID int64, image string, actorID *int64) (*Deployment, error) {
	d := &Deployment{AppID: appID, Image: image, Status: "running", ActorID: actorID}
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO deployments (app_id, image, status, actor_id) VALUES (?, ?, 'running', ?) RETURNING id, created_at`,
		appID, image, actorID).Scan(&d.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrDeployInProgress
		}
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	return d, nil
}

// DeploymentResult is the outcome of a deployment.
type DeploymentResult struct {
	OK            bool
	Error         string
	Log           string
	ImageDigest   string
	CommitSHA     string
	CommitMessage string
	ComposeSealed []byte
	Digests       map[string]string
}

// FinishDeployment records the outcome of a running deployment.
func (db *DB) FinishDeployment(ctx context.Context, id int64, r DeploymentResult) error {
	status := "failed"
	if r.OK {
		status = "succeeded"
	}
	digests := []byte("{}")
	if len(r.Digests) > 0 {
		var err error
		if digests, err = json.Marshal(r.Digests); err != nil {
			return err
		}
	}
	return db.execOne(ctx,
		`UPDATE deployments SET status = ?, error = ?, log = ?, image_digest = ?, commit_sha = ?, commit_message = ?,
		 compose = ?, digests = ?, finished_at = unixepoch() WHERE id = ? AND status = 'running'`,
		status, r.Error, r.Log, r.ImageDigest, r.CommitSHA, r.CommitMessage, r.ComposeSealed, string(digests), id)
}

// FailUnfinishedDeployments marks deployments interrupted by a panel
// restart as failed. Called at startup.
func (db *DB) FailUnfinishedDeployments(ctx context.Context) (int64, error) {
	res, err := db.W.ExecContext(ctx,
		`UPDATE deployments SET status = 'failed', error = 'interrupted by a panel restart', finished_at = unixepoch()
		 WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const deploymentColumns = `id, app_id, image, commit_sha, commit_message, status, error, log, image_digest, actor_id,
	created_at, finished_at, compose, digests`

func scanDeployment(row interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var created int64
	var finished sql.NullInt64
	var digests string
	err := row.Scan(&d.ID, &d.AppID, &d.Image, &d.CommitSHA, &d.CommitMessage, &d.Status, &d.Error, &d.Log,
		&d.ImageDigest, &d.ActorID, &created, &finished, &d.ComposeSealed, &digests)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	d.FinishedAt = timePtr(finished)
	if err := json.Unmarshal([]byte(digests), &d.Digests); err != nil {
		return nil, err
	}
	return &d, nil
}

// Deployments returns the newest deployments of an app first (logs included).
func (db *DB) Deployments(ctx context.Context, appID int64, limit int) ([]*Deployment, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (db *DB) DeploymentByID(ctx context.Context, appID, id int64) (*Deployment, error) {
	return scanDeployment(db.R.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE app_id = ? AND id = ?`, appID, id))
}

// SucceededImages lists the images of an app's successful deployments,
// newest first. Compose deployments (several images) are not listed.
func (db *DB) SucceededImages(ctx context.Context, appID int64) ([]agentproto.ImageRef, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT image, image_digest FROM deployments WHERE app_id = ? AND status = 'succeeded' AND image != ''
		 ORDER BY id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agentproto.ImageRef
	for rows.Next() {
		var r agentproto.ImageRef
		if err := rows.Scan(&r.Ref, &r.Digest); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAppTemplateRevision records the template revision an app now runs.
func (db *DB) SetAppTemplateRevision(ctx context.Context, id int64, revision int) error {
	return db.execOne(ctx, `UPDATE apps SET template_revision = ? WHERE id = ?`, revision, id)
}
