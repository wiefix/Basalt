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
	"github.com/wiefix/basalt/internal/forge"
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
	ID        int64    `json:"id"`
	ServerID  int64    `json:"server_id"`
	ProjectID int64    `json:"project_id"`
	Name      string   `json:"name"`
	Source    string   `json:"source"` // image or git
	Image     string   `json:"image"`
	Git       *gitView `json:"git,omitempty"`
	// DeployKey is the public key to grant read access to the repository.
	DeployKey  string `json:"deploy_key,omitempty"`
	AutoDeploy bool   `json:"auto_deploy"`
	// Compose apps (and git apps running a compose file): the published
	// service (Port is its port) and what the compose file contains, without
	// its secrets; for a git app, the file last read from the repository.
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
	// Previews: where the app's pull requests are deployed; a preview is of
	// an app, for a pull request.
	PreviewDomain string `json:"preview_domain,omitempty"`
	PreviewOf     int64  `json:"preview_of,omitempty"`
	PRNumber      int    `json:"pr_number,omitempty"`

	Port    int      `json:"port"`
	Domains []string `json:"domains"`
	// Replicas are the other servers the app runs on.
	Replicas  []int64                `json:"replicas"`
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
	v := appView{ID: a.ID, ServerID: a.ServerID, ProjectID: a.ProjectID, Name: a.Name, Source: a.Source, Image: a.Image, Port: a.Port,
		Domains: a.Domains, TLS: a.TLS, Volumes: vols, MemoryMB: a.MemoryMB, CPUs: a.CPUs, ReadOnly: a.ReadOnly, CreatedAt: a.CreatedAt,
		Template: a.Template, TemplateRevision: a.TemplateRevision, Replicas: nonNil(a.Replicas),
		PreviewDomain: a.PreviewDomain, PreviewOf: a.PreviewOf, PRNumber: a.PRNumber}
	switch a.Source {
	case store.SourceGit:
		v.Git = &gitView{URL: a.Git.URL, Branch: a.Git.Branch, Dockerfile: a.Git.Dockerfile, Context: a.Git.Context,
			SourceID: a.Git.SourceID, Repo: a.Git.Repo, Builder: a.Git.Builder, BuildEnv: a.Git.BuildEnv, ComposeFile: a.Git.ComposeFile}
		v.DeployKey, v.AutoDeploy = a.DeployKeyPublic, a.AutoDeploy
	}
	if a.IsCompose() {
		sum := a.Summary
		if sum.Services == nil {
			sum.Services = []store.ComposeService{}
		}
		if sum.Volumes == nil {
			sum.Volumes = []string{}
		}
		v.Service, v.Compose = a.Service, &sum
	}
	switch a.Source {
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
	// SourceID is the git source (GitHub App) the repository Repo
	// (owner/name) is reached through; URL then comes from GitHub.
	SourceID int64  `json:"source_id,omitempty"`
	Repo     string `json:"repo,omitempty"`
	// Builder: "" builds Dockerfile; "railpack" works out how to build the
	// context without one (Node, Python, Go, PHP, Ruby, Java, static sites…);
	// "compose" runs ComposeFile, building the services it builds.
	Builder     string `json:"builder,omitempty"`
	ComposeFile string `json:"compose_file,omitempty"` // path in the repository
	// BuildEnv names variables of the app's environment its build gets, as
	// secrets: Railpack passes them to its steps, a Dockerfile reads them
	// with RUN --mount=type=secret,id=NAME,env=NAME.
	BuildEnv []string `json:"build_env,omitempty"`
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
	// The mail server serves every project: only global roles reach it.
	if app.Source == store.SourceMail {
		if !s.checkGlobal(w, r, "app not found") {
			return nil, false
		}
	} else if !s.checkProject(w, r, app.ProjectID, "app not found") {
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
			Dockerfile: strings.TrimSpace(req.Git.Dockerfile), Context: strings.TrimSpace(req.Git.Context), Builder: req.Git.Builder,
			ComposeFile: strings.TrimSpace(req.Git.ComposeFile)}
		if req.Git.SourceID != 0 {
			// The URL comes from GitHub (resolveGitSource).
			if req.Git.SourceID < 0 || !forge.ValidRepo(req.Git.Repo) {
				return st, errors.New("a repository from a git source needs the source and owner/name")
			}
			g.URL = "https://github.com/" + req.Git.Repo + ".git"
		}
		if g.Branch == "" {
			g.Branch = "main"
		}
		if g.Dockerfile == "" {
			g.Dockerfile = "Dockerfile"
		}
		if g.Context == "" {
			g.Context = "."
		}
		if g.Builder == agentproto.BuilderCompose && g.ComposeFile == "" {
			g.ComposeFile = "compose.yaml"
		}
		if err := g.Validate(); err != nil {
			return st, err
		}
		st.Git = store.GitSettings{URL: g.URL, Branch: g.Branch, Dockerfile: g.Dockerfile, Context: g.Context, Builder: g.Builder,
			ComposeFile: g.ComposeFile}
		seen := map[string]bool{}
		for _, k := range req.Git.BuildEnv {
			k = strings.TrimSpace(k)
			if err := agentproto.ValidBuildEnvName(k); err != nil {
				return st, err
			}
			if !seen[k] {
				seen[k] = true
				st.Git.BuildEnv = append(st.Git.BuildEnv, k)
			}
		}
		if len(st.Git.BuildEnv) > agentproto.MaxBuildEnv {
			return st, fmt.Errorf("at most %d build variables", agentproto.MaxBuildEnv)
		}
		if req.Git.SourceID != 0 {
			st.Git.SourceID, st.Git.Repo = req.Git.SourceID, req.Git.Repo
		}
		if g.Builder == agentproto.BuilderCompose {
			// The compose file sets the rest; Port is its published
			// service's (set with the service).
			st.Volumes, st.MemoryMB, st.CPUs, st.ReadOnly = nil, 0, 0, false
		}
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
	// default the first one that mentions a port). Git apps running a
	// compose file (builder "compose") may name the service, with Port.
	Compose string `json:"compose"`
	Service string `json:"service"`
	// Databases: the engine (postgres, mysql, mariadb, redis) and its
	// version (by default the newest offered).
	Engine    string        `json:"engine"`
	Version   string        `json:"version"`
	Source    string        `json:"source"`
	ProjectID int64         `json:"project_id"` // 0: the default project
	ServerID  int64         `json:"server_id"`
	Name      string        `json:"name"`
	Domains   []string      `json:"domains"`
	TLS       proxy.TLSMode `json:"tls"`
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
	project, ok := s.checkCreate(w, r, p, req.ProjectID)
	if !ok {
		return
	}
	req.ProjectID = project
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
	if err == nil {
		err = s.resolveGitSource(r.Context(), &st)
	}
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
	service := strings.TrimSpace(req.Service)
	if st.Git.Builder == agentproto.BuilderCompose {
		// The compose file is read from the repository when the app
		// deploys; the service to publish may be named now.
		switch {
		case service == "":
			st.Port = 0
		case !agentproto.ServiceNameRE.MatchString(service):
			writeError(w, http.StatusUnprocessableEntity, "invalid service name")
			return
		case st.Port == 0:
			writeError(w, http.StatusUnprocessableEntity, "set the port service "+service+" listens on")
			return
		}
	}
	var cf *composeFile
	if req.Source == store.SourceCompose {
		// No environment yet: unset variables are only warnings.
		if cf, err = s.parseCompose(r.Context(), []byte(req.Compose), nil, req.Service, req.Port, ""); err != nil {
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
	if s.domainError(w, r, s.checkDomainProjects(r.Context(), domains, req.ProjectID)) {
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
	if st.Git.Builder == agentproto.BuilderRailpack && srv.Railpack == "" {
		writeError(w, http.StatusUnprocessableEntity, "this server cannot build without a Dockerfile: re-run the installer on it with --with-docker (it adds Railpack)")
		return
	}
	st.ProjectID = req.ProjectID
	app, err := s.db.CreateApp(r.Context(), srv.ID, name, st, domains, tls)
	var taken *store.ErrDomainTaken
	switch {
	case projectError(w, err):
		return
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
		// On GitLab or Gitea the panel connects the repository itself.
		if err := s.connectForge(r.Context(), app); err != nil {
			s.deleteApp(r.Context(), app, true)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if app.Source == store.SourceDatabase {
		if err := s.initDatabase(r.Context(), app); err != nil {
			s.internalError(w, r, "create database credentials", err)
			return
		}
	}
	if service != "" && cf == nil {
		if err := s.db.SetAppCompose(r.Context(), app.ID, nil, store.ComposeSummary{}, service, app.Port); err != nil {
			s.internalError(w, r, "set published service", err)
			return
		}
		app.Service = service
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
	visible, err := s.visibleIn(r, p)
	if err != nil {
		s.internalError(w, r, "load project roles", err)
		return
	}
	out := []appView{}
	for _, a := range apps {
		// The mail server is in no project: global roles see it.
		if a.Source == store.SourceMail && !p.user.Role.AtLeast(store.RoleViewer) || a.Source != store.SourceMail && !visible(a.ProjectID) {
			continue
		}
		out = append(out, s.viewAppFull(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	placements, err := s.db.AppPlacements(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, "list app placements", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": s.viewAppFull(app), "sync": s.proxySync(r.Context(), app.ServerID),
		"placements": viewPlacements(placements)})
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
	if req.Git != nil && app.Source == store.SourceGit && (req.Git.Builder == agentproto.BuilderCompose) != app.ComposeFromGit() {
		writeError(w, http.StatusUnprocessableEntity, "an app cannot switch between a compose file and one image: create a new app")
		return
	}
	st, err := req.settings(app.Source)
	if err == nil {
		err = s.resolveGitSource(r.Context(), &st)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if app.ComposeFromGit() {
		st.Port = app.Port // its published service's, set under Compose
	}
	if st.Port == 0 && len(app.Domains) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "remove the app's domains before clearing its port")
		return
	}
	if len(st.Volumes) > 0 && len(app.Replicas) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "the app runs on several servers: it cannot keep data in volumes")
		return
	}
	if app.Source == store.SourceDatabase {
		st.Engine, st.EngineVersion, st.Image = app.Engine, app.EngineVersion, app.Image
	}
	if app.Source == store.SourceMail {
		st.Hostname, st.Image = app.Hostname, app.Image
	}
	moved := app.Source == store.SourceGit && (app.Git.SourceID != st.Git.SourceID || !strings.EqualFold(app.Git.Repo, st.Git.Repo))
	if moved {
		s.disconnectForge(r.Context(), app, nil) // from the repository it leaves
	}
	if err := s.db.UpdateAppSettings(r.Context(), app.ID, st); err != nil {
		s.internalError(w, r, "update app", err)
		return
	}
	if moved {
		app.Git = st.Git
		if err := s.connectForge(r.Context(), app); err != nil {
			writeError(w, http.StatusBadGateway, "saved, but "+err.Error())
			return
		}
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
	if s.domainError(w, r, s.checkDomainProjects(r.Context(), domains, app.ProjectID)) {
		return
	}
	if len(app.Replicas) > 0 {
		msg, err := s.domainsOutsideZones(r.Context(), domains)
		if err == nil && len(domains) == 0 {
			msg = "the app runs on several servers: it needs a domain (remove the replicas first)"
		}
		if err != nil {
			s.internalError(w, r, "check domains", err)
			return
		}
		if msg != "" {
			writeError(w, http.StatusUnprocessableEntity, msg)
			return
		}
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
	for _, id := range app.Replicas {
		s.PushProxy(r.Context(), id)
	}
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

// handleCancelDeployment stops a running deployment: the server undoes
// what it started, and the app keeps running its previous deployment.
func (s *Server) handleCancelDeployment(w http.ResponseWriter, r *http.Request, p *principal) {
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
	if d.Status != "running" || !s.stopDeployment(d.ID, p.user.Name) {
		writeError(w, http.StatusConflict, "the deployment is not running")
		return
	}
	s.audit(r, &p.user.ID, "app.deploy_canceled", map[string]any{"app_id": app.ID, "deployment_id": d.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{})
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
	serverID, ok := appServer(w, r, app)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var st agentproto.AppStatus
	if err := s.agents.Call(ctx, serverID, agentproto.TypeAppStatus, agentproto.AppRequest{AppID: app.ID, Service: service}, &st); err != nil {
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
	serverID, ok := appServer(w, r, app)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var resp agentproto.AppLogsResponse
	if err := s.agents.Call(ctx, serverID, agentproto.TypeAppLogs, agentproto.AppRequest{AppID: app.ID, Tail: tail, Service: service}, &resp); err != nil {
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
	serverID, ok := appServer(w, r, app)
	if !ok {
		return
	}
	if !s.agents.Connected(serverID) {
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
	err = s.agents.Stream(ctx, serverID, agentproto.TypeAppLogsFollow,
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
	// Its previews go first, with their data.
	previews, err := s.db.Previews(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, "list previews", err)
		return
	}
	var cleanup []string
	for _, pv := range previews {
		c, err := s.deleteApp(r.Context(), pv, true)
		if err != nil {
			s.internalError(w, r, "delete preview", err)
			return
		}
		cleanup = append(cleanup, c...)
	}
	c, err := s.deleteApp(r.Context(), app, volumes)
	if err != nil {
		s.internalError(w, r, "delete app", err)
		return
	}
	cleanup = append(cleanup, c...)
	s.audit(r, &p.user.ID, "app.deleted", map[string]any{"app_id": app.ID, "name": app.Name, "volumes": volumes, "previews": len(previews)})
	resp := map[string]any{"sync": s.proxySync(r.Context(), app.ServerID)}
	if len(cleanup) > 0 {
		resp["cleanup_error"] = strings.Join(cleanup, "; ")
	}
	writeJSON(w, http.StatusOK, resp)
}

// deleteApp removes an app: its record, its routes, then its containers
// (and volumes, if asked) on every server. It returns what could not be
// cleaned up yet (an offline server cleans up when it reconnects).
func (s *Server) deleteApp(ctx context.Context, app *store.App, volumes bool) ([]string, error) {
	images, err := s.db.SucceededImages(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	s.disconnectForge(ctx, app, nil)
	servers, err := s.db.DeleteApp(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if len(servers) > 1 {
		s.healthChanged(ctx) // the checks of its replicas are gone
		s.syncAutoDNS(ctx)
	}
	// Stop routing first, then remove the containers.
	for _, id := range servers {
		s.PushProxy(ctx, id)
	}
	var cleanup []string
	for _, id := range servers {
		if err := s.removeAppFrom(ctx, id, app.ID, volumes, imagesToPrune(images, 0)); err != nil {
			cleanup = append(cleanup, s.serverLabel(ctx, id, len(servers))+err.Error())
		}
	}
	return cleanup, nil
}

// removeAppFrom removes an app's containers (and, if asked, its volumes)
// from one server.
func (s *Server) removeAppFrom(ctx context.Context, serverID, appID int64, volumes bool, prune []agentproto.ImageRef) error {
	if !s.agents.Connected(serverID) {
		return errors.New("server offline: containers are removed when it reconnects; data volumes are kept")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req := agentproto.AppRequest{AppID: appID, DeleteVolumes: volumes, PruneImages: prune}
	return s.agents.Call(ctx, serverID, agentproto.TypeAppDelete, req, nil)
}

// serverLabel prefixes a message about one of several servers with its name.
func (s *Server) serverLabel(ctx context.Context, serverID int64, servers int) string {
	if servers < 2 {
		return ""
	}
	if srv, err := s.db.ServerByID(ctx, serverID); err == nil {
		return srv.Name + ": "
	}
	return fmt.Sprintf("server #%d: ", serverID)
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
// the last ?minutes= (default 60, at most maxMetricsMinutes).
func (s *Server) handleAppMetrics(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxMetricsMinutes {
			writeError(w, http.StatusUnprocessableEntity, "minutes must be between 1 and 43200 (30 days)")
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
