package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/compose"
	"github.com/wiefix/basalt/internal/docker"
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
// by default the first service that mentions a port.
func (s *Server) parseCompose(ctx context.Context, content []byte, env map[string]string, service string, port int) (*composeFile, error) {
	p, err := compose.Parse(ctx, content, env)
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
	return &composeFile{content: content, project: p, service: service, port: port}, nil
}

func (cf *composeFile) summary() store.ComposeSummary {
	sum := store.ComposeSummary{Services: []store.ComposeService{}, Volumes: cf.project.Volumes}
	if sum.Volumes == nil {
		sum.Volumes = []string{}
	}
	for _, svc := range cf.project.Services {
		sum.Services = append(sum.Services, store.ComposeService{Name: svc.Name, Image: svc.Image,
			Ports: cf.project.Ports[svc.Name], Job: svc.Job})
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

func (s *Server) composeApp(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	app, ok := s.pathApp(w, r)
	if ok && app.Source != store.SourceCompose {
		writeError(w, http.StatusNotFound, "this app is not a compose app")
		return nil, false
	}
	return app, ok
}

// handleGetCompose returns the compose file (admins only: it often holds
// passwords) and what is wrong with it under the current environment.
func (s *Server) handleGetCompose(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.composeApp(w, r)
	if !ok {
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
	if cf, err := s.parseCompose(r.Context(), content, env, app.Service, app.Port); err != nil {
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
// the next deployment applies them.
func (s *Server) handleSetCompose(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.composeApp(w, r)
	if !ok {
		return
	}
	var req composeRequest
	if !decodeJSONLimit(w, r, &req, compose.MaxSize*2) {
		return
	}
	env, err := s.appEnv(app)
	if err != nil {
		s.internalError(w, r, "decrypt app env", err)
		return
	}
	cf, err := s.parseCompose(r.Context(), []byte(req.Compose), env, strings.TrimSpace(req.Service), req.Port)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	linked, err := s.db.LinkedDatabaseNames(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, "list linked databases", err)
		return
	}
	for _, svc := range cf.project.Services {
		if slices.Contains(linked, svc.Name) || slices.ContainsFunc(svc.Aliases, func(a string) bool { return slices.Contains(linked, a) }) {
			writeError(w, http.StatusUnprocessableEntity, "service "+svc.Name+" has the name of a linked database; rename it or unlink the database")
			return
		}
	}
	if len(app.Replicas) > 0 && len(cf.project.Volumes) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "the app runs on several servers: its compose file cannot keep data in volumes (remove the replicas first)")
		return
	}
	if cf.port == 0 && len(app.Domains) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "the app has domains: choose the service and port they point to")
		return
	}
	if err := s.saveCompose(r.Context(), app, cf); err != nil {
		s.internalError(w, r, "store compose file", err)
		return
	}
	s.audit(r, &p.user.ID, "app.compose_changed", map[string]any{"app_id": app.ID, "service": cf.service, "port": cf.port})
	writeJSON(w, http.StatusOK, map[string]any{"app": viewApp(app), "warnings": nonNil(cf.project.Warnings)})
}

// composeSpec prepares a compose deployment: content is the compose file
// to run (the app's, or an earlier deployment's for a rollback, with the
// images that deployment ran in pinned).
func (s *Server) composeSpec(ctx context.Context, app *store.App, content []byte, pinned map[string]string, spec *agentproto.AppSpec) ([]string, error) {
	env, err := s.appEnv(app)
	if err != nil {
		return nil, fmt.Errorf("decrypt app env: %w", err)
	}
	cf, err := s.parseCompose(ctx, content, env, app.Service, app.Port)
	if err != nil {
		return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "compose file: " + err.Error(), cause: err}
	}
	hosts := map[string]bool{}
	for i := range cf.project.Services {
		svc := &cf.project.Services[i]
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
	return cf.project.Warnings, nil
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
