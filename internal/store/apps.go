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
	ID       int64
	ServerID int64
	Name     string
	Source   string // "image" or "git"
	Image    string // for source "image"
	Git      GitSettings
	// Deploy key (encrypted OpenSSH private key and its public line) and
	// webhook secret (encrypted) of a git app; nil until generated.
	DeployKey       []byte
	DeployKeyPublic string
	WebhookSecret   []byte
	AutoDeploy      bool

	Port      int
	EnvSealed []byte // encrypted JSON object; nil when empty
	Volumes   []agentproto.AppVolume
	MemoryMB  int
	CPUs      float64
	RouteID   int64
	Domains   []string
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
	SourceImage = "image"
	SourceGit   = "git"
)

// AppSettings are the fields an app is deployed with, other than env.
// Source is fixed when the app is created.
type AppSettings struct {
	Source   string
	Image    string
	Git      GitSettings
	Port     int
	Volumes  []agentproto.AppVolume
	MemoryMB int
	CPUs     float64
}

var ErrDeployInProgress = errors.New("a deployment of this app is already running")

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
	a := &App{ServerID: serverID, Name: name, Source: st.Source, Image: st.Image, Git: st.Git, Port: st.Port,
		Volumes: st.Volumes, MemoryMB: st.MemoryMB, CPUs: st.CPUs, Domains: domains, TLS: tls}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO apps (server_id, name, source, image, git_url, git_branch, git_dockerfile, git_context,
		 port, volumes, memory_mb, cpus) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING id, created_at`,
		serverID, name, st.Source, st.Image, st.Git.URL, st.Git.Branch, st.Git.Dockerfile, st.Git.Context,
		st.Port, vols, st.MemoryMB, st.CPUs).Scan(&a.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: apps.name") {
			return nil, ErrNameTaken
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

const appQuery = `SELECT a.id, a.server_id, a.name, a.source, a.image, a.git_url, a.git_branch, a.git_dockerfile,
	a.git_context, a.deploy_key, a.deploy_key_public, a.webhook_secret, a.auto_deploy,
	a.port, a.env, a.volumes, a.memory_mb, a.cpus, a.created_at, r.id, r.tls, COALESCE(d.domain, '')
	FROM apps a JOIN proxy_routes r ON r.app_id = a.id
	LEFT JOIN proxy_route_domains d ON d.route_id = r.id`

func scanApps(rows *sql.Rows) ([]*App, error) {
	defer rows.Close()
	var out []*App
	for rows.Next() {
		var a App
		var created int64
		var vols, domain string
		if err := rows.Scan(&a.ID, &a.ServerID, &a.Name, &a.Source, &a.Image, &a.Git.URL, &a.Git.Branch,
			&a.Git.Dockerfile, &a.Git.Context, &a.DeployKey, &a.DeployKeyPublic, &a.WebhookSecret, &a.AutoDeploy,
			&a.Port, &a.EnvSealed, &vols, &a.MemoryMB, &a.CPUs, &created, &a.RouteID, &a.TLS, &domain); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].ID == a.ID {
			out[n-1].Domains = append(out[n-1].Domains, domain)
			continue
		}
		if err := json.Unmarshal([]byte(vols), &a.Volumes); err != nil {
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

// AppIDsOnServer lists the IDs of all apps placed on a server.
func (db *DB) AppIDsOnServer(ctx context.Context, serverID int64) ([]int64, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT id FROM apps WHERE server_id = ? ORDER BY id`, serverID)
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
		 port = ?, volumes = ?, memory_mb = ?, cpus = ? WHERE id = ?`,
		st.Image, st.Git.URL, st.Git.Branch, st.Git.Dockerfile, st.Git.Context, st.Port, vols, st.MemoryMB, st.CPUs, id)
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

// SetAppDomains replaces the domains an app answers to.
func (db *DB) SetAppDomains(ctx context.Context, appID int64, domains []string) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var serverID, routeID int64
	err = tx.QueryRowContext(ctx,
		`SELECT a.server_id, r.id FROM apps a JOIN proxy_routes r ON r.app_id = a.id WHERE a.id = ?`,
		appID).Scan(&serverID, &routeID)
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

// DeleteApp removes the app, its route and deployments (by cascade).
func (db *DB) DeleteApp(ctx context.Context, id int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var serverID int64
	err = tx.QueryRowContext(ctx, `DELETE FROM apps WHERE id = ? RETURNING server_id`, id).Scan(&serverID)
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
}

// FinishDeployment records the outcome of a running deployment.
func (db *DB) FinishDeployment(ctx context.Context, id int64, r DeploymentResult) error {
	status := "failed"
	if r.OK {
		status = "succeeded"
	}
	return db.execOne(ctx,
		`UPDATE deployments SET status = ?, error = ?, log = ?, image_digest = ?, commit_sha = ?, commit_message = ?,
		 finished_at = unixepoch() WHERE id = ? AND status = 'running'`,
		status, r.Error, r.Log, r.ImageDigest, r.CommitSHA, r.CommitMessage, id)
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
	created_at, finished_at`

func scanDeployment(row interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var created int64
	var finished sql.NullInt64
	err := row.Scan(&d.ID, &d.AppID, &d.Image, &d.CommitSHA, &d.CommitMessage, &d.Status, &d.Error, &d.Log,
		&d.ImageDigest, &d.ActorID, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	d.FinishedAt = timePtr(finished)
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
// newest first.
func (db *DB) SucceededImages(ctx context.Context, appID int64) ([]agentproto.ImageRef, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT image, image_digest FROM deployments WHERE app_id = ? AND status = 'succeeded' ORDER BY id DESC`, appID)
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
