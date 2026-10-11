package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/store"
)

// templateView is a catalog template for the UI, compose file included
// (it holds no secrets: they are variables).
type templateView struct {
	*catalog.Template
	Compose string `json:"compose"`
}

func (s *Server) templates() ([]*catalog.Template, error) { return catalog.Builtin() }

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.templates()
	if err != nil {
		s.internalError(w, r, "load catalog", err)
		return
	}
	out := make([]templateView, len(list))
	for i, t := range list {
		out[i] = templateView{Template: t, Compose: t.Compose}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

type installRequest struct {
	ServerID int64             `json:"server_id"`
	Name     string            `json:"name"`
	Inputs   map[string]string `json:"inputs"`
}

// handleInstall installs a template as a compose app and deploys it.
func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.templates()
	if err != nil {
		s.internalError(w, r, "load catalog", err)
		return
	}
	t := catalog.Find(list, r.PathValue("id"))
	if t == nil {
		writeError(w, http.StatusNotFound, "no such app in the catalog")
		return
	}
	var req installRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = t.ID
	}
	if !serverNameRE.MatchString(name) {
		writeError(w, http.StatusUnprocessableEntity, "name must be 1-63 characters: letters, digits, dot, dash, underscore")
		return
	}
	in, err := t.Prepare(req.Inputs)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	cf, err := s.parseCompose(r.Context(), []byte(t.Compose), in.Env, t.Expose.Service, t.Expose.Port)
	if err != nil {
		s.internalError(w, r, "parse template "+t.ID, err)
		return
	}
	domains, tls, err := normalizeAppDomains([]string{in.Domain}, "", cf.port)
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
	st := store.AppSettings{Source: store.SourceCompose, Port: cf.port, Template: t.ID, TemplateRevision: t.Revision}
	app, err := s.db.CreateApp(r.Context(), srv.ID, name, st, domains, tls)
	var taken *store.ErrDomainTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, taken.Error())
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "an app with this name already exists: choose another name")
		return
	case err != nil:
		s.internalError(w, r, "create app", err)
		return
	}
	b, _ := json.Marshal(in.Env)
	sealed := s.box.Seal(b, envAD(app.ID))
	if err := s.db.SetAppEnv(r.Context(), app.ID, sealed); err != nil {
		s.internalError(w, r, "store app environment", err)
		return
	}
	app.EnvSealed = sealed
	if err := s.saveCompose(r.Context(), app, cf); err != nil {
		s.internalError(w, r, "store compose file", err)
		return
	}
	s.audit(r, &p.user.ID, "app.installed", map[string]any{"app_id": app.ID, "name": app.Name, "template": t.ID,
		"revision": t.Revision, "domain": in.Domain})
	s.PushProxy(r.Context(), app.ServerID)
	resp := map[string]any{"app": viewApp(app), "notes": t.Notes}
	dep, err := s.startDeployment(r.Context(), app, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "manual"})
	if err != nil {
		resp["deploy_error"] = err.Error()
	} else {
		resp["deployment"] = viewDeployment(dep, false)
	}
	writeJSON(w, http.StatusCreated, resp)
}
