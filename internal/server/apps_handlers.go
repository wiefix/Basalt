package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/mailserver"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

const (
	deployTimeout  = 30 * time.Minute
	appCallTimeout = 30 * time.Second
	maxLogTail     = 5000
)

type appView struct {
	ID       int64    `json:"id"`
	ServerID int64    `json:"server_id"`
	Name     string   `json:"name"`
	Source   string   `json:"source"` // image or git
	Image    string   `json:"image"`
	Git      *gitView `json:"git,omitempty"`
	// DeployKey is the public key to grant read access to the repository.
	DeployKey  string `json:"deploy_key,omitempty"`
	AutoDeploy bool   `json:"auto_deploy"`
	// Compose apps: the published service (Port is its port) and what the
	// compose file contains, without its secrets.
	Service string                `json:"service,omitempty"`
	Compose *store.ComposeSummary `json:"compose,omitempty"`
	// Databases: engine, major version and the apps that reach it.
	Engine  string  `json:"engine,omitempty"`
	Version string  `json:"version,omitempty"`
	Links   []int64 `json:"links,omitzero"` // never nil for a database or the mail server
	// Template is the catalog template the app was installed from.
	Template         string `json:"template,omitempty"`
	TemplateRevision int    `json:"template_revision,omitempty"`
	// TemplateUpdate is the version a newer template revision brings.
	TemplateUpdate string `json:"template_update,omitempty"`

	Port      int                    `json:"port"`
	Domains   []string               `json:"domains"`
	TLS       proxy.TLSMode          `json:"tls"`
	Volumes   []agentproto.AppVolume `json:"volumes"`
	MemoryMB  int                    `json:"memory_mb"`
	CPUs      float64                `json:"cpus"`
	ReadOnly  bool                   `json:"read_only"`
	CreatedAt time.Time              `json:"created_at"`
}

func viewApp(a *store.App) appView {
	vols := a.Volumes
	if vols == nil {
		vols = []agentproto.AppVolume{}
	}
	v := appView{ID: a.ID, ServerID: a.ServerID, Name: a.Name, Source: a.Source, Image: a.Image, Port: a.Port,
		Domains: a.Domains, TLS: a.TLS, Volumes: vols, MemoryMB: a.MemoryMB, CPUs: a.CPUs, ReadOnly: a.ReadOnly, CreatedAt: a.CreatedAt,
		Template: a.Template, TemplateRevision: a.TemplateRevision}
	switch a.Source {
	case store.SourceGit:
		v.Git = &gitView{URL: a.Git.URL, Branch: a.Git.Branch, Dockerfile: a.Git.Dockerfile, Context: a.Git.Context}
		v.DeployKey, v.AutoDeploy = a.DeployKeyPublic, a.AutoDeploy
	case store.SourceCompose:
		sum := a.Summary
		if sum.Services == nil {
			sum.Services = []store.ComposeService{}
		}
		if sum.Volumes == nil {
			sum.Volumes = []string{}
		}
		v.Service, v.Compose = a.Service, &sum
	case store.SourceDatabase:
		v.Engine, v.Version, v.Links = a.Engine, a.EngineVersion, nonNil(a.Links)
	case store.SourceMail:
		v.Links = nonNil(a.Links) // webmail apps that reach it
	}
	return v
}

type gitView struct {
	URL        string `json:"url"`
	Branch     string `json:"branch"`
	Dockerfile string `json:"dockerfile"`
	Context    string `json:"context"`
}

type deploymentView struct {
	ID            int64             `json:"id"`
	Image         string            `json:"image"`
	Digests       map[string]string `json:"digests,omitempty"` // compose: image per service
	Commit        string            `json:"commit,omitempty"`
	CommitMessage string            `json:"commit_message,omitempty"`
	Status        string            `json:"status"`
	Error         string            `json:"error,omitempty"`
	ImageDigest   string            `json:"image_digest,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	Log           *string           `json:"log,omitempty"`
}

func viewDeployment(d *store.Deployment, withLog bool) deploymentView {
	v := deploymentView{ID: d.ID, Image: d.Image, Digests: d.Digests, Commit: d.CommitSHA, CommitMessage: d.CommitMessage,
		Status: d.Status, Error: d.Error, ImageDigest: d.ImageDigest,
		CreatedAt: d.CreatedAt, FinishedAt: d.FinishedAt}
	if withLog {
		v.Log = &d.Log
	}
	return v
}

func (s *Server) pathApp(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "app not found")
		return nil, false
	}
	app, err := s.db.AppByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "load app", err)
		return nil, false
	}
	return app, true
}

func envAD(appID int64) []byte { return fmt.Appendf(nil, "app-env:%d", appID) }

func (s *Server) appEnv(a *store.App) (map[string]string, error) {
	env := map[string]string{}
	if a.EnvSealed == nil {
		return env, nil
	}
	b, err := s.box.Open(a.EnvSealed, envAD(a.ID))
	if err != nil {
		return nil, err
	}
	return env, json.Unmarshal(b, &env)
}

// appSettingsRequest is shared by create and update. Image is for apps
// from an image, Git for apps built from a repository.
type appSettingsRequest struct {
	Image    string                 `json:"image"`
	Git      *gitView               `json:"git"`
	Port     int                    `json:"port"`
	Volumes  []agentproto.AppVolume `json:"volumes"`
	MemoryMB int                    `json:"memory_mb"`
	CPUs     float64                `json:"cpus"`
	ReadOnly bool                   `json:"read_only"`
}

func (req appSettingsRequest) settings(source string) (store.AppSettings, error) {
	st := store.AppSettings{Source: source, Port: req.Port, Volumes: req.Volumes, MemoryMB: req.MemoryMB, CPUs: req.CPUs,
		ReadOnly: req.ReadOnly}
	switch source {
	case store.SourceImage:
		st.Image = strings.TrimSpace(req.Image)
		if !docker.ValidRef(st.Image) {
			return st, errors.New("image must be a valid reference, e.g. nginx:1.27 or ghcr.io/org/app:v1")
		}
	case store.SourceGit:
		if req.Git == nil {
			return st, errors.New("git repository settings are required")
		}
		g := agentproto.GitSource{URL: strings.TrimSpace(req.Git.URL), Branch: strings.TrimSpace(req.Git.Branch),
			Dockerfile: strings.TrimSpace(req.Git.Dockerfile), Context: strings.TrimSpace(req.Git.Context)}
		if g.Branch == "" {
			g.Branch = "main"
		}
		if g.Dockerfile == "" {
			g.Dockerfile = "Dockerfile"
		}
		if g.Context == "" {
			g.Context = "."
		}
		if err := g.Validate(); err != nil {
			return st, err
		}
		st.Git = store.GitSettings{URL: g.URL, Branch: g.Branch, Dockerfile: g.Dockerfile, Context: g.Context}
	case store.SourceCompose:
		// Everything else comes from the compose file.
		st.Volumes, st.MemoryMB, st.CPUs, st.ReadOnly = nil, 0, 0, false
		return st, nil
	case store.SourceDatabase:
		// The engine (checked on creation) sets everything but the limits.
		st.Port, st.Volumes, st.ReadOnly = 0, nil, false
	case store.SourceMail:
		// Basalt defines the mail server; only its limits are settings.
		st.Image, st.Port, st.Volumes, st.ReadOnly = mailserver.Image, mailserver.HTTPPort, nil, false
	default:
		return st, errors.New(`source must be "image", "git", "compose" or "database"`)
	}
	switch {
	case st.Port < 0 || st.Port > 65535:
		return st, errors.New("port must be between 1 and 65535, or 0 if the app serves no HTTP")
	case st.MemoryMB != 0 && (st.MemoryMB < 16 || st.MemoryMB > 1<<20):
		return st, errors.New("memory limit must be at least 16 MB")
	case st.CPUs < 0 || st.CPUs > 1024:
		return st, errors.New("invalid CPU limit")
	}
	return st, agentproto.ValidateVolumes(st.Volumes)
}

type createAppRequest struct {
	appSettingsRequest
	// Compose apps: the compose file and the service to publish (by
	// default the first one that mentions a port).
	Compose string `json:"compose"`
	Service string `json:"service"`
	// Databases: the engine (postgres, mysql, mariadb, redis) and its
	// version (by default the newest offered).
	Engine   string        `json:"engine"`
	Version  string        `json:"version"`
	Source   string        `json:"source"`
	ServerID int64         `json:"server_id"`
	Name     string        `json:"name"`
	Domains  []string      `json:"domains"`
	TLS      proxy.TLSMode `json:"tls"`
}

// normalizeAppDomains validates domains for an app route; none is allowed.
func normalizeAppDomains(domains []string, tls proxy.TLSMode, port int) ([]string, proxy.TLSMode, error) {
	if len(domains) == 0 {
		if tls == "" {
			tls = proxy.TLSAuto
		}
		if tls != proxy.TLSAuto && tls != proxy.TLSInternal {
			return nil, "", fmt.Errorf("tls must be %q or %q", proxy.TLSAuto, proxy.TLSInternal)
		}
		return []string{}, tls, nil
	}
	if port == 0 {
		return nil, "", errors.New("set the app's HTTP port before adding domains")
	}
	r := proxy.Route{Kind: proxy.KindReverseProxy, AppID: 1, Domains: domains, TLS: tls}
	if err := r.Validate(""); err != nil {
		return nil, "", err
	}
	return r.Domains, r.TLS, nil
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createAppRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !serverNameRE.MatchString(name) {
		writeError(w, http.StatusUnprocessableEntity, "name must be 1-63 characters: letters, digits, dot, dash, underscore")
		return
	}
	if req.Source == "" {
		req.Source = store.SourceImage
	}
	if req.Source == store.SourceMail {
		writeError(w, http.StatusUnprocessableEntity, "set up the mail server under Mail")
		return
	}
	st, err := req.settings(req.Source)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.Source == store.SourceDatabase {
		if err := validDatabaseName(name); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if len(req.Domains) > 0 {
			writeError(w, http.StatusUnprocessableEntity, "a database has no domains: link it to the apps that use it")
			return
		}
		if st, err = databaseSettings(st, req.Engine, strings.TrimSpace(req.Version)); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	var cf *composeFile
	if req.Source == store.SourceCompose {
		// No environment yet: unset variables are only warnings.
		if cf, err = s.parseCompose(r.Context(), []byte(req.Compose), nil, req.Service, req.Port); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		st.Port = cf.port
	}
	domains, tls, err := normalizeAppDomains(req.Domains, req.TLS, st.Port)
	if err == nil {
		err = s.checkWildcards(r.Context(), domains, tls)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	srv, err := s.db.ServerByID(r.Context(), req.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "server not found")
		return
	} else if err != nil {
		s.internalError(w, r, "load server", err)
		return
	}
	if srv.Docker == "" {
		writeError(w, http.StatusUnprocessableEntity, "Docker is not installed on this server")
		return
	}
	app, err := s.db.CreateApp(r.Context(), srv.ID, name, st, domains, tls)
	var taken *store.ErrDomainTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, taken.Error())
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "an app with this name already exists")
		return
	case err != nil:
		s.internalError(w, r, "create app", err)
		return
	}
	if app.Source == store.SourceGit {
		// Deploy key and webhook secret come with the app, so the
		// repository can be connected right away.
		if err := s.newDeployKey(r.Context(), app); err != nil {
			s.internalError(w, r, "create deploy key", err)
			return
		}
		if err := s.newWebhookSecret(r.Context(), app, true); err != nil {
			s.internalError(w, r, "create webhook secret", err)
			return
		}
	}
	if app.Source == store.SourceDatabase {
		if err := s.initDatabase(r.Context(), app); err != nil {
			s.internalError(w, r, "create database credentials", err)
			return
		}
	}
	var warnings []string
	if cf != nil {
		if err := s.saveCompose(r.Context(), app, cf); err != nil {
			s.internalError(w, r, "store compose file", err)
			return
		}
		warnings = cf.project.Warnings
	}
	s.audit(r, &p.user.ID, "app.created", map[string]any{"app_id": app.ID, "name": app.Name, "image": app.Image,
		"git": app.Git.URL, "source": app.Source, "engine": app.Engine, "version": app.EngineVersion})
	s.PushProxy(r.Context(), app.ServerID)
	writeJSON(w, http.StatusCreated, map[string]any{"app": viewApp(app), "sync": s.proxySync(r.Context(), app.ServerID),
		"warnings": nonNil(warnings)})
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request, p *principal) {
	apps, err := s.db.Apps(r.Context())
	if err != nil {
		s.internalError(w, r, "list apps", err)
		return
	}
	out := make([]appView, len(apps))
	for i, a := range apps {
		out[i] = s.viewAppFull(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": s.viewAppFull(app), "sync": s.proxySync(r.Context(), app.ServerID)})
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	var req appSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if app.Source == store.SourceCompose {
		writeError(w, http.StatusUnprocessableEntity, "a compose app is configured by its compose file")
		return
	}
	st, err := req.settings(app.Source)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if st.Port == 0 && len(app.Domains) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "remove the app's domains before clearing its port")
		return
	}
	if app.Source == store.SourceDatabase {
		st.Engine, st.EngineVersion, st.Image = app.Engine, app.EngineVersion, app.Image
	}
	if app.Source == store.SourceMail {
		st.Hostname, st.Image = app.Hostname, app.Image
	}
	if err := s.db.UpdateAppSettings(r.Context(), app.ID, st); err != nil {
		s.internalError(w, r, "update app", err)
		return
	}
	s.audit(r, &p.user.ID, "app.updated", map[string]any{"app_id": app.ID, "image": st.Image, "git": st.Git.URL, "port": st.Port})
	app.Image, app.Git, app.Port, app.Volumes, app.MemoryMB, app.CPUs = st.Image, st.Git, st.Port, st.Volumes, st.MemoryMB, st.CPUs
	app.ReadOnly = st.ReadOnly
	writeJSON(w, http.StatusOK, map[string]any{"app": viewApp(app)})
}

type appDomainsRequest struct {
	Domains []string `json:"domains"`
}

func (s *Server) handleSetAppDomains(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	var req appDomainsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	domains, _, err := normalizeAppDomains(req.Domains, app.TLS, app.Port)
	if err == nil {
		err = s.checkWildcards(r.Context(), domains, app.TLS)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	err = s.db.SetAppDomains(r.Context(), app.ID, domains)
	var taken *store.ErrDomainTaken
	if errors.As(err, &taken) {
		writeError(w, http.StatusConflict, taken.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, "set app domains", err)
		return
	}
	s.audit(r, &p.user.ID, "app.domains_changed", map[string]any{"app_id": app.ID, "domains": domains})
	s.PushProxy(r.Context(), app.ServerID)
	app.Domains = domains
	writeJSON(w, http.StatusOK, map[string]any{"app": viewApp(app), "sync": s.proxySync(r.Context(), app.ServerID)})
}

// handleGetAppEnv returns the variables in clear: admins only, as they
// usually hold secrets.
func (s *Server) handleGetAppEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	env, err := s.appEnv(app)
	if err != nil {
		s.internalError(w, r, "decrypt app env", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": env})
}

type appEnvRequest struct {
	Env map[string]string `json:"env"`
}

func (s *Server) handleSetAppEnv(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	if app.Source == store.SourceDatabase {
		// The image applies credentials only when it creates the database.
		writeError(w, http.StatusUnprocessableEntity, "a database's credentials are set when it is created")
		return
	}
	if app.Source == store.SourceMail {
		writeError(w, http.StatusUnprocessableEntity, "the mail server's environment is managed by Basalt")
		return
	}
	var req appEnvRequest
	if !decodeJSONLimit(w, r, &req, 1<<20) {
		return
	}
	if err := agentproto.ValidateEnv(req.Env); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var sealed []byte
	if len(req.Env) > 0 {
		b, _ := json.Marshal(req.Env)
		sealed = s.box.Seal(b, envAD(app.ID))
	}
	if err := s.db.SetAppEnv(r.Context(), app.ID, sealed); err != nil {
		s.internalError(w, r, "set app env", err)
		return
	}
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	// Names only: values are secrets.
	s.audit(r, &p.user.ID, "app.env_changed", map[string]any{"app_id": app.ID, "keys": keys})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type deployRequest struct {
	// RollbackTo redeploys the image of an earlier successful deployment
	// (by digest), with the app's current settings.
	RollbackTo int64 `json:"rollback_to"`
}

func (s *Server) handleDeployApp(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	var req deployRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trigger := "manual"
	if req.RollbackTo > 0 {
		trigger = "rollback"
	}
	dep, err := s.startDeployment(r.Context(), app, deployOptions{
		actorID: &p.user.ID, ip: clientIP(r), rollbackTo: req.RollbackTo, trigger: trigger,
	})
	var de *deployError
	if errors.As(err, &de) {
		writeError(w, de.status, de.msg)
		return
	}
	if err != nil {
		s.internalError(w, r, "start deployment", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment": viewDeployment(dep, false)})
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	list, err := s.db.Deployments(r.Context(), app.ID, 50)
	if err != nil {
		s.internalError(w, r, "list deployments", err)
		return
	}
	out := make([]deploymentView, len(list))
	for i, d := range list {
		out[i] = viewDeployment(d, false)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out})
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("did"), 10, 64)
	d, err := s.db.DeploymentByID(r.Context(), app.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load deployment", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": viewDeployment(d, true)})
}

func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	service, ok := s.pathService(w, r, app)
	if !ok {
		return
	}
	if service == "" && app.Source == store.SourceDatabase {
		service = app.Name // its only service
	}
	if service == "" && app.Source == store.SourceMail {
		service = mailserver.Service
	}
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var st agentproto.AppStatus
	if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeAppStatus, agentproto.AppRequest{AppID: app.ID, Service: service}, &st); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": st})
}

// handleAppLogs returns recent container output. Members only: apps often
// print secrets on start.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogTail {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("tail must be between 1 and %d", maxLogTail))
			return
		}
		tail = n
	}
	service, ok := s.logService(w, r, app)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var resp agentproto.AppLogsResponse
	if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeAppLogs, agentproto.AppRequest{AppID: app.ID, Tail: tail, Service: service}, &resp); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": resp.Log})
}

// handleAppLogsStream follows the app's container output as server-sent
// events: "log" {text}, and "end" {error} if the server stops it.
func (s *Server) handleAppLogsStream(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogTail {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("tail must be between 1 and %d", maxLogTail))
			return
		}
		tail = n
	}
	service, ok := s.logService(w, r, app)
	if !ok {
		return
	}
	if !s.agents.Connected(app.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	if !s.acquireStream(p.user.ID) {
		writeError(w, http.StatusTooManyRequests, "too many open log streams")
		return
	}
	defer s.releaseStream(p.user.ID)
	ev, err := startSSE(w)
	if err != nil {
		return
	}
	ctx := r.Context()
	stop := ev.keepAlive(ctx)
	defer stop()
	err = s.agents.Stream(ctx, app.ServerID, agentproto.TypeAppLogsFollow,
		agentproto.AppRequest{AppID: app.ID, Tail: tail, Service: service}, nil, func(raw json.RawMessage) error {
			var c agentproto.LogChunk
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			return ev.send("log", c)
		})
	if ctx.Err() == nil {
		ev.send("end", map[string]string{"error": agentErrorText(err)})
	}
}

// handleDeploymentStream sends a deployment's log as server-sent events
// while it runs ("log" {text}) and then its outcome ("end", a deployment).
func (s *Server) handleDeploymentStream(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("did"), 10, 64)
	d, err := s.db.DeploymentByID(r.Context(), app.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load deployment", err)
		return
	}
	if !s.acquireStream(p.user.ID) {
		writeError(w, http.StatusTooManyRequests, "too many open log streams")
		return
	}
	defer s.releaseStream(p.user.ID)
	ev, err := startSSE(w)
	if err != nil {
		return
	}
	ctx := r.Context()
	stop := ev.keepAlive(ctx)
	defer stop()
	sent := false // live output was sent; the stored log would repeat it
	sendLog := func(text string) error {
		sent = true
		return ev.send("log", agentproto.LogChunk{Text: text})
	}
	for d.Status == "running" {
		if live := s.live.get(d.ID); live != nil {
			if live.follow(ctx, sendLog) != nil {
				return
			}
		} else {
			// Finishing right now: its outcome is about to be stored.
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
		if d, err = s.db.DeploymentByID(ctx, app.ID, id); err != nil {
			return
		}
	}
	if !sent && d.Log != "" {
		sendLog(d.Log)
	}
	ev.send("end", viewDeployment(d, false))
}

// pathService reads the optional ?service= of a compose app or database.
func (s *Server) pathService(w http.ResponseWriter, r *http.Request, app *store.App) (string, bool) {
	service := r.URL.Query().Get("service")
	if service == "" {
		return "", true
	}
	if !app.HasServices() || !agentproto.ServiceNameRE.MatchString(service) {
		writeError(w, http.StatusUnprocessableEntity, "unknown service")
		return "", false
	}
	return service, true
}

// logService is the service whose logs are shown: the one asked for, or by
// default the published service (or the first one) of a compose app.
func (s *Server) logService(w http.ResponseWriter, r *http.Request, app *store.App) (string, bool) {
	service, ok := s.pathService(w, r, app)
	if !ok || service != "" || !app.HasServices() {
		return service, ok
	}
	if app.Source == store.SourceDatabase {
		return app.Name, true
	}
	if app.Source == store.SourceMail {
		return mailserver.Service, true
	}
	if app.Service != "" {
		return app.Service, true
	}
	if len(app.Summary.Services) > 0 {
		return app.Summary.Services[0].Name, true
	}
	return "", true
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// agentErrorText is the message shown for a failed agent call.
func agentErrorText(err error) string {
	var re *agentproto.RemoteError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &re):
		return re.Msg
	default:
		return "server unreachable: " + err.Error()
	}
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	volumes := r.URL.Query().Get("volumes") == "1"
	images, err := s.db.SucceededImages(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, "list deployed images", err)
		return
	}
	if err := s.db.DeleteApp(r.Context(), app.ID); err != nil {
		s.internalError(w, r, "delete app", err)
		return
	}
	s.audit(r, &p.user.ID, "app.deleted", map[string]any{"app_id": app.ID, "name": app.Name, "volumes": volumes})
	// Stop routing first, then remove the containers.
	s.PushProxy(r.Context(), app.ServerID)
	resp := map[string]any{"sync": s.proxySync(r.Context(), app.ServerID)}
	if s.agents.Connected(app.ServerID) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		req := agentproto.AppRequest{AppID: app.ID, DeleteVolumes: volumes, PruneImages: imagesToPrune(images, 0)}
		if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeAppDelete, req, nil); err != nil {
			resp["cleanup_error"] = err.Error()
		}
	} else {
		resp["cleanup_error"] = "server offline: containers are removed when it reconnects; data volumes are kept"
	}
	writeJSON(w, http.StatusOK, resp)
}

// SyncApps tells the agent which apps exist so it can remove containers of
// apps deleted while it was offline.
func (s *Server) SyncApps(ctx context.Context, serverID int64) error {
	ids, err := s.db.AppIDsOnServer(ctx, serverID)
	if err != nil {
		return err // never send a partial list: the agent would delete apps
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.agents.Call(ctx, serverID, agentproto.TypeAppsSync, agentproto.AppsSync{AppIDs: ids}, nil)
}

type appMetricsView struct {
	At       time.Time `json:"at"`
	CPUPct   float64   `json:"cpu_pct"`
	MemUsed  uint64    `json:"mem_used"`
	MemLimit uint64    `json:"mem_limit"`
	NetRx    uint64    `json:"net_rx"`
	NetTx    uint64    `json:"net_tx"`
}

// handleAppMetrics returns the resource usage of an app's containers over
// the last ?minutes= (default 60, at most 48 hours).
func (s *Server) handleAppMetrics(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 48*60 {
			writeError(w, http.StatusUnprocessableEntity, "minutes must be between 1 and 2880")
			return
		}
		minutes = n
	}
	samples, err := s.db.AppMetricsSince(r.Context(), app.ID, s.now().Add(-time.Duration(minutes)*time.Minute), 20000)
	if err != nil {
		s.internalError(w, r, "load app metrics", err)
		return
	}
	out := make([]appMetricsView, len(samples))
	for i, m := range samples {
		out[i] = appMetricsView{At: m.At, CPUPct: m.CPUPct, MemUsed: m.MemUsed, MemLimit: m.MemLimit, NetRx: m.NetRx, NetTx: m.NetTx}
	}
	writeJSON(w, http.StatusOK, map[string]any{"metrics": out})
}
