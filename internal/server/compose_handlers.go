package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/compose"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

func composeAD(appID int64) []byte           { return fmt.Appendf(nil, "app-compose:%d", appID) }
func deploymentComposeAD(depID int64) []byte { return fmt.Appendf(nil, "deployment-compose:%d", depID) }

// composeFile is a parsed compose file with the service it publishes.
type composeFile struct {
	content []byte
	project *compose.Project
	service string
	port    int
}

// parseCompose validates a compose file and picks the published service:
// the one given (at port, or the first port the file mentions for it), or
// by default the first service that mentions a port. routes are the other
// services with domains of their own (all must be in the file). repoDir:
// the file is from the app's git repository, in that directory, and its
// services may build ("": not from a repository).
func (s *Server) parseCompose(ctx context.Context, content []byte, env map[string]string, service string, port int, routes []store.AppRoute, repoDir string) (*composeFile, error) {
	var p *compose.Project
	var err error
	if repoDir != "" {
		p, err = compose.ParseRepo(ctx, content, env, repoDir)
	} else {
		p, err = compose.Parse(ctx, content, env)
	}
	if err != nil {
		return nil, err
	}
	if service == "" {
		service, port = p.DefaultPublished()
	} else if port == 0 {
		if i := slices.IndexFunc(p.Services, func(sv agentproto.ServiceSpec) bool { return sv.Name == service }); i >= 0 {
			if ports := p.Ports[service]; len(ports) > 0 {
				port = ports[0]
			}
		}
	}
	if port < 0 || port > 65535 {
		return nil, errors.New("port must be between 1 and 65535")
	}
	if service != "" && port == 0 {
		return nil, fmt.Errorf("set the port service %s listens on", service)
	}
	if err := p.Publish(service, port); err != nil {
		return nil, err
	}
	for _, rt := range routes {
		if err := p.Route(rt.Service, rt.Port); err != nil {
			return nil, err
		}
	}
	return &composeFile{content: content, project: p, service: service, port: port}, nil
}

func (cf *composeFile) summary() store.ComposeSummary {
	sum := store.ComposeSummary{Services: []store.ComposeService{}, Volumes: cf.project.Volumes}
	if sum.Volumes == nil {
		sum.Volumes = []string{}
	}
	for _, svc := range cf.project.Services {
		cs := store.ComposeService{Name: svc.Name, Image: svc.Image,
			Ports: cf.project.Ports[svc.Name], Job: svc.Job, MemoryMB: svc.MemoryMB, CPUs: svc.CPUs}
		if svc.Build != nil {
			cs.Image, cs.Build = "", svc.Build.Dockerfile
		}
		sum.Services = append(sum.Services, cs)
	}
	return sum
}

// saveCompose stores the compose file (encrypted) and what it publishes.
func (s *Server) saveCompose(ctx context.Context, app *store.App, cf *composeFile) error {
	sealed := s.box.Seal(cf.content, composeAD(app.ID))
	sum := cf.summary()
	if err := s.db.SetAppCompose(ctx, app.ID, sealed, sum, cf.service, cf.port); err != nil {
		return err
	}
	app.ComposeSealed, app.Summary, app.Service, app.Port = sealed, sum, cf.service, cf.port
	return nil
}

func (s *Server) appCompose(app *store.App) ([]byte, error) {
	if app.ComposeSealed == nil {
		return nil, errors.New("the app has no compose file")
	}
	return s.box.Open(app.ComposeSealed, composeAD(app.ID))
}

// composeDir is the directory of a git app's compose file in its
// repository ("" for apps not running one).
func composeDir(app *store.App) string {
	if !app.ComposeFromGit() {
		return ""
	}
	return path.Dir(app.Git.ComposeFile)
}

// composeApp resolves a compose app, or a git app running a compose file.
func (s *Server) composeApp(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	app, ok := s.pathApp(w, r)
	if ok && !app.IsCompose() {
		writeError(w, http.StatusNotFound, "this app is not a compose app")
		return nil, false
	}
	return app, ok
}

// handleGetCompose returns the compose file (admins only: it often holds
// passwords) and what is wrong with it under the current environment. For
// a git app, it is the file last read from the repository ("" before).
func (s *Server) handleGetCompose(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.composeApp(w, r)
	if !ok {
		return
	}
	if app.ComposeSealed == nil && app.ComposeFromGit() {
		writeJSON(w, http.StatusOK, map[string]any{"compose": "", "service": app.Service, "port": app.Port, "warnings": []string{}})
		return
	}
	content, err := s.appCompose(app)
	if err != nil {
		s.internalError(w, r, "decrypt compose file", err)
		return
	}
	env, err := s.appEnv(app)
	if err != nil {
		s.internalError(w, r, "decrypt app env", err)
		return
	}
	resp := map[string]any{"compose": string(content), "service": app.Service, "port": app.Port, "warnings": []string{}}
	if cf, err := s.parseCompose(r.Context(), content, env, app.Service, app.Port, app.Routes, composeDir(app)); err != nil {
		resp["error"] = err.Error()
	} else {
		resp["warnings"] = nonNil(cf.project.Warnings)
	}
	writeJSON(w, http.StatusOK, resp)
}

type composeRequest struct {
	Compose string `json:"compose"`
	Service string `json:"service"`
	Port    int    `json:"port"`
}

// handleSetCompose replaces the compose file and the published service;
// the next deployment applies them. A git app's file comes from its
// repository: only the published service changes (Compose is ignored).
func (s *Server) handleSetCompose(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.composeApp(w, r)
	if !ok {
		return
	}
	var req composeRequest
	if !decodeJSONLimit(w, r, &req, compose.MaxSize*2) {
		return
	}
	content := []byte(req.Compose)
	if app.ComposeFromGit() {
		if app.ComposeSealed == nil {
			writeError(w, http.StatusUnprocessableEntity, "load the compose file from the repository first")
			return
		}
		var err error
		if content, err = s.appCompose(app); err != nil {
			s.internalError(w, r, "decrypt compose file", err)
			return
		}
	}
	s.storeCompose(w, r, p, app, content, strings.TrimSpace(req.Service), req.Port, "")
}

// storeCompose validates and stores an app's compose file with the service
// it publishes. commit is the commit of a git app's file.
func (s *Server) storeCompose(w http.ResponseWriter, r *http.Request, p *principal, app *store.App, content []byte, service string, port int, commit string) {
	env, err := s.appEnv(app)
	if err != nil {
		s.internalError(w, r, "decrypt app env", err)
		return
	}
	cf, err := s.parseCompose(r.Context(), content, env, service, port, app.Routes, composeDir(app))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.checkComposeFile(r.Context(), app, cf); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.saveCompose(r.Context(), app, cf); err != nil {
		s.internalError(w, r, "store compose file", err)
		return
	}
	details := map[string]any{"app_id": app.ID, "service": cf.service, "port": cf.port}
	if commit != "" {
		details["commit"] = commit
	}
	s.audit(r, &p.user.ID, "app.compose_changed", details)
	resp := map[string]any{"app": viewApp(app), "warnings": nonNil(cf.project.Warnings)}
	if commit != "" {
		resp["commit"] = commit
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkComposeFile refuses a compose file the app cannot run as it is set up.
func (s *Server) checkComposeFile(ctx context.Context, app *store.App, cf *composeFile) error {
	linked, err := s.db.LinkedDatabaseNames(ctx, app.ID)
	if err != nil {
		return fmt.Errorf("list linked databases: %w", err)
	}
	for _, svc := range cf.project.Services {
		if slices.Contains(linked, svc.Name) || slices.ContainsFunc(svc.Aliases, func(a string) bool { return slices.Contains(linked, a) }) {
			return errors.New("service " + svc.Name + " has the name of a linked database; rename it or unlink the database")
		}
	}
	if len(app.Replicas) > 0 && len(cf.project.Volumes) > 0 {
		return errors.New("the app runs on several servers: its compose file cannot keep data in volumes (remove the replicas first)")
	}
	if cf.port == 0 && len(app.Domains) > 0 {
		return errors.New("the app has domains: choose the service and port they point to")
	}
	return nil
}

type appRoutesRequest struct {
	Routes []store.AppRoute `json:"routes"`
}

// handleSetAppRoutes replaces the other services of a compose app that have
// domains of their own. Their containers are labelled with the port when
// they deploy: a new service or port is served from the next deployment,
// new domains of a service already served at once.
func (s *Server) handleSetAppRoutes(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.composeApp(w, r)
	if !ok {
		return
	}
	var req appRoutesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	routes, err := s.normalizeRoutes(r.Context(), app, req.Routes)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var domains []string
	for _, rt := range routes {
		domains = append(domains, rt.Domains...)
	}
	if s.domainError(w, r, s.checkDomainProjects(r.Context(), domains, app.ProjectID)) {
		return
	}
	if len(app.Replicas) > 0 {
		msg, err := s.domainsOutsideZones(r.Context(), domains)
		if err != nil {
			s.internalError(w, r, "check domains", err)
			return
		}
		if msg != "" {
			writeError(w, http.StatusUnprocessableEntity, msg)
			return
		}
	}
	err = s.db.SetAppRoutes(r.Context(), app.ID, routes)
	var taken *store.ErrDomainTaken
	if errors.As(err, &taken) {
		writeError(w, http.StatusConflict, taken.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, "set app routes", err)
		return
	}
	s.audit(r, &p.user.ID, "app.routes_changed", map[string]any{"app_id": app.ID, "routes": routes})
	s.PushProxy(r.Context(), app.ServerID)
	for _, id := range app.Replicas {
		s.PushProxy(r.Context(), id)
	}
	app.Routes = routes
	writeJSON(w, http.StatusOK, map[string]any{"app": viewApp(app), "sync": s.proxySync(r.Context(), app.ServerID)})
}

// normalizeRoutes checks the routes of an app's services against its
// compose file (as last stored) and normalizes their domains. A route
// without a port gets the first one the file mentions for the service.
func (s *Server) normalizeRoutes(ctx context.Context, app *store.App, in []store.AppRoute) ([]store.AppRoute, error) {
	if len(in) > proxy.MaxServiceRoutes {
		return nil, fmt.Errorf("at most %d services can have domains of their own", proxy.MaxServiceRoutes)
	}
	out := []store.AppRoute{}
	seen := map[string]bool{}
	for _, d := range app.Domains {
		seen["domain "+d] = true
	}
	for _, rt := range in {
		rt.Service = strings.TrimSpace(rt.Service)
		i := slices.IndexFunc(app.Summary.Services, func(c store.ComposeService) bool { return c.Name == rt.Service })
		switch {
		case i < 0:
			return nil, fmt.Errorf("the compose file has no service %q", rt.Service)
		case app.Summary.Services[i].Job:
			return nil, fmt.Errorf("service %s runs to completion and cannot have domains", rt.Service)
		case rt.Service == app.Service:
			return nil, fmt.Errorf("service %s is the published service: its domains are the app's domains", rt.Service)
		case seen["service "+rt.Service]:
			return nil, fmt.Errorf("service %s is listed twice", rt.Service)
		}
		seen["service "+rt.Service] = true
		if ports := app.Summary.Services[i].Ports; rt.Port == 0 && len(ports) > 0 {
			rt.Port = ports[0]
		}
		if rt.Port < 1 || rt.Port > 65535 {
			return nil, fmt.Errorf("set the port service %s listens on", rt.Service)
		}
		if len(rt.Domains) == 0 {
			return nil, fmt.Errorf("service %s needs at least one domain", rt.Service)
		}
		domains, _, err := normalizeAppDomains(rt.Domains, app.TLS, rt.Port)
		if err == nil {
			err = s.checkWildcards(ctx, domains, app.TLS)
		}
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", rt.Service, err)
		}
		for _, d := range domains {
			if seen["domain "+d] {
				return nil, fmt.Errorf("domain %s is used twice by the app", d)
			}
			seen["domain "+d] = true
		}
		rt.Domains = domains
		out = append(out, rt)
	}
	slices.SortFunc(out, func(a, b store.AppRoute) int { return strings.Compare(a.Service, b.Service) })
	return out, nil
}

// handleLoadCompose reads a git app's compose file from its repository
// (the head of its branch) and stores it, so its services can be seen and
// the one to publish chosen before it deploys.
func (s *Server) handleLoadCompose(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.composeApp(w, r)
	if !ok {
		return
	}
	if !app.ComposeFromGit() {
		writeError(w, http.StatusUnprocessableEntity, "this app's compose file is not in a repository")
		return
	}
	if !s.agents.Connected(app.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	g, err := s.gitSource(r.Context(), app)
	if err != nil {
		var de *deployError
		if errors.As(err, &de) {
			writeError(w, de.status, de.msg)
			return
		}
		s.internalError(w, r, "prepare git source", err)
		return
	}
	resp, err := s.readComposeFile(r.Context(), app, g)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.storeCompose(w, r, p, app, []byte(resp.Compose), app.Service, app.Port, resp.Commit)
}

// readComposeFile asks the app's server for the compose file in its
// repository, at g.Commit or the head of its branch.
func (s *Server) readComposeFile(ctx context.Context, app *store.App, g *agentproto.GitSource) (*agentproto.ComposeFileResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, composeFileTimeout)
	defer cancel()
	var resp agentproto.ComposeFileResponse
	if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeComposeFile,
		agentproto.ComposeFileRequest{AppID: app.ID, Git: g}, &resp); err != nil {
		return nil, fmt.Errorf("read %s from the repository: %w", g.ComposeFile, err)
	}
	if !agentproto.ValidCommit(resp.Commit) || len(resp.Compose) > compose.MaxSize {
		return nil, errors.New("the server sent an invalid compose file")
	}
	return &resp, nil
}

// composeFileTimeout bounds fetching a repository for its compose file.
const composeFileTimeout = 5 * time.Minute

// composeSpec prepares a compose deployment: content is the compose file
// to run (the app's, or an earlier deployment's for a rollback, with the
// images that deployment ran in pinned). Services of a git app that build
// get a tag of their own, and in a rollback the image built then.
func (s *Server) composeSpec(ctx context.Context, app *store.App, content []byte, pinned map[string]string, spec *agentproto.AppSpec) (*composeFile, error) {
	env, err := s.appEnv(app)
	if err != nil {
		return nil, fmt.Errorf("decrypt app env: %w", err)
	}
	cf, err := s.parseCompose(ctx, content, env, app.Service, app.Port, app.Routes, composeDir(app))
	if err != nil {
		return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "compose file: " + err.Error(), cause: err}
	}
	var b [5]byte
	rand.Read(b[:])
	hosts := map[string]bool{}
	for i := range cf.project.Services {
		svc := &cf.project.Services[i]
		if svc.Build != nil {
			svc.Image = fmt.Sprintf("%s%s-%s", builtImagePrefix(app.ID), svc.Name, hex.EncodeToString(b[:]))
			if d := pinned[svc.Name]; strings.HasPrefix(d, builtImagePrefix(app.ID)) && docker.ValidRef(d) {
				svc.Build.ReuseImage = d
			}
			continue
		}
		if d := pinned[svc.Name]; strings.Contains(d, "@sha256:") && docker.ValidRef(d) {
			svc.Image = d
		}
		hosts[docker.RegistryHost(svc.Image)] = true
	}
	for host := range hosts {
		auth, err := s.registryAuth(ctx, host+"/x")
		if err != nil {
			return nil, fmt.Errorf("load registry credentials: %w", err)
		}
		if auth != nil {
			spec.RegistryAuths = append(spec.RegistryAuths, *auth)
		}
	}
	spec.Services, spec.ComposeVolumes = cf.project.Services, cf.project.Volumes
	if app.ComposeFromGit() {
		previous, err := s.db.SucceededDigests(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		spec.PruneImages = builtImagesToPrune(app.ID, previous, spec.Services, s.imagesKept)
	}
	return cf, nil
}

// builtImagePrefix starts the tags of the images built for an app.
func builtImagePrefix(appID int64) string { return fmt.Sprintf("basalt/app-%d:", appID) }

// builtImagesToPrune lists the images earlier deployments of a git compose
// app built (succeeded, newest first) that are no longer kept: those of
// all but its keep-1 latest, not used by services.
func builtImagesToPrune(appID int64, succeeded []map[string]string, services []agentproto.ServiceSpec, keep int) []agentproto.ImageRef {
	keep = max(keep-1, 0)
	kept := map[string]bool{}
	for _, svc := range services {
		if svc.Build != nil && svc.Build.ReuseImage != "" {
			kept[svc.Build.ReuseImage] = true
		}
	}
	for i, d := range succeeded {
		if i < keep {
			for _, ref := range d {
				kept[ref] = true
			}
		}
	}
	var out []agentproto.ImageRef
	seen := map[string]bool{}
	for _, d := range succeeded[min(keep, len(succeeded)):] {
		for _, svc := range slices.Sorted(maps.Keys(d)) {
			ref := d[svc]
			if kept[ref] || seen[ref] || !strings.HasPrefix(ref, builtImagePrefix(appID)) || !docker.ValidRef(ref) ||
				len(out) == agentproto.MaxPruneImages {
				continue
			}
			seen[ref] = true
			out = append(out, agentproto.ImageRef{Ref: ref})
		}
	}
	return out
}

// cleanDigests keeps the per-service images an agent reported that look valid.
func cleanDigests(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if len(out) < agentproto.MaxServices && agentproto.ServiceNameRE.MatchString(k) &&
			(docker.ValidRef(v) || strings.HasPrefix(v, "sha256:") && len(v) == 71) {
			out[k] = v
		}
	}
	return out
}
