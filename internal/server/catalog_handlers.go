package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/store"
)

// templateView is a catalog template for the UI, compose file included
// (it holds no secrets: they are variables).
type templateView struct {
	*catalog.Template
	Compose string `json:"compose"`
}

// templates returns the catalog: the built-in templates merged with the
// downloaded ones (tests replace the built-in ones through Options).
func (s *Server) templates() ([]*catalog.Template, error) {
	builtin := catalog.Builtin
	if s.catalog != nil {
		builtin = s.catalog
	}
	list, err := builtin()
	if err != nil {
		return nil, err
	}
	s.catalogMu.Lock()
	remote := s.remote
	s.catalogMu.Unlock()
	if remote == nil {
		return catalog.Merge(list, nil, ""), nil
	}
	return catalog.Merge(list, remote.templates, remote.source), nil
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.templates()
	if err != nil {
		s.internalError(w, r, "load catalog", err)
		return
	}
	out := make([]templateView, len(list))
	for i, t := range list {
		// Lists are never null in the API.
		c := *t
		c.Inputs, c.Secrets, c.Services, c.Volumes = nonNil(c.Inputs), nonNil(c.Secrets), nonNil(c.Services), nonNil(c.Volumes)
		out[i] = templateView{Template: &c, Compose: t.Compose}
	}
	src, err := s.viewCatalogSource(r.Context())
	if err != nil {
		s.internalError(w, r, "describe catalog source", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out, "source": src})
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
	var mailApp *store.App
	if t.Mail {
		mailApp, err = s.db.MailApp(r.Context())
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, t.Name+" uses the mail server: set it up under Mail first")
			return
		} else if err != nil {
			s.internalError(w, r, "load mail server", err)
			return
		}
		in.Env[catalog.MailHostKey] = mailApp.Hostname
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
	if mailApp != nil && mailApp.ServerID == app.ServerID {
		// The app reaches the mail server on its own network, under the
		// name its certificate has (as linked databases are reached).
		if err := s.db.LinkApp(r.Context(), mailApp.ID, app.ID); err != nil {
			s.internalError(w, r, "link the mail server", err)
			return
		}
		if mailApp, err = s.db.AppByID(r.Context(), mailApp.ID); err == nil {
			if err := s.applyLinks(r.Context(), mailApp); err != nil {
				resp["attach_error"] = err.Error()
			}
		}
	}
	dep, err := s.startDeployment(r.Context(), app, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "manual"})
	if err != nil {
		resp["deploy_error"] = err.Error()
	} else {
		resp["deployment"] = viewDeployment(dep, false)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// templateUpdate is an update of an installed app to a newer revision of
// its template, while it runs: the backup first, then the deployment.
type templateUpdate struct {
	Revision int    `json:"revision"`
	State    string `json:"state"` // backing_up, failed
	BackupID int64  `json:"backup_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) templateUpdate(appID int64) *templateUpdate {
	s.tplMu.Lock()
	defer s.tplMu.Unlock()
	if u := s.tplUpdates[appID]; u != nil {
		c := *u
		return &c
	}
	return nil
}

func (s *Server) setTemplateUpdate(appID int64, u *templateUpdate) {
	s.tplMu.Lock()
	defer s.tplMu.Unlock()
	if u == nil {
		delete(s.tplUpdates, appID)
	} else {
		s.tplUpdates[appID] = u
	}
}

// appTemplate returns the catalog template of an installed app (nil if
// the app is not from the catalog or the template is gone).
func (s *Server) appTemplate(app *store.App) *catalog.Template {
	if app.Template == "" {
		return nil
	}
	list, err := s.templates()
	if err != nil {
		return nil
	}
	return catalog.Find(list, app.Template)
}

// viewAppFull is viewApp with whether a template update is available.
func (s *Server) viewAppFull(app *store.App) appView {
	v := viewApp(app)
	if t := s.appTemplate(app); t != nil && t.Revision > app.TemplateRevision {
		v.TemplateUpdate = t.Version
	}
	return v
}

type imageChange struct {
	Service string `json:"service"`
	From    string `json:"from,omitempty"` // empty: a new service
	To      string `json:"to,omitempty"`   // empty: a removed service
}

// imageChanges compares the images an app runs with a template's.
func imageChanges(app *store.App, t *catalog.Template) []imageChange {
	out := []imageChange{}
	old := map[string]string{}
	for _, sv := range app.Summary.Services {
		old[sv.Name] = sv.Image
	}
	for _, sv := range t.Services {
		if old[sv.Name] != sv.Image {
			out = append(out, imageChange{Service: sv.Name, From: old[sv.Name], To: sv.Image})
		}
		delete(old, sv.Name)
	}
	for name, img := range old {
		out = append(out, imageChange{Service: name, From: img})
	}
	return out
}

// handleAppTemplate describes an installed app's template and its update.
func (s *Server) handleAppTemplate(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	if app.Template == "" {
		writeError(w, http.StatusNotFound, "this app was not installed from the catalog")
		return
	}
	out := map[string]any{"installed_revision": app.TemplateRevision, "template": (*templateInfoView)(nil), "update_available": false,
		"update": s.templateUpdate(app.ID)}
	t := s.appTemplate(app)
	if t != nil {
		out["template"] = &templateInfoView{ID: t.ID, Name: t.Name, Version: t.Version, Revision: t.Revision,
			Notes: t.Notes, Website: t.Website}
		if t.Revision > app.TemplateRevision {
			env, err := s.appEnv(app)
			if err != nil {
				s.internalError(w, r, "read app environment", err)
				return
			}
			out["update_available"] = true
			out["images"] = imageChanges(app, t)
			out["needs"] = nonNil(t.Missing(env))
		}
	}
	_, err := s.db.BackupPolicy(r.Context(), appTarget(app).BackupTarget)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "load backup policy", err)
		return
	}
	out["backups"] = err == nil
	writeJSON(w, http.StatusOK, out)
}

// templateInfoView is the catalog template of an installed app.
type templateInfoView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Revision int    `json:"revision"`
	Notes    string `json:"notes"`
	Website  string `json:"website"`
}

type templateUpdateRequest struct {
	Inputs map[string]string `json:"inputs"`
	// Backup backs up the app's volumes first (needs a backup policy).
	Backup bool `json:"backup"`
}

// handleTemplateUpdate updates an installed app to the newest revision of
// its template: optionally a backup first, then the new compose file with
// the app's values kept (secrets the template added are generated), then
// a deployment. Rolling back the deployment goes back to the old file.
func (s *Server) handleTemplateUpdate(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	var req templateUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	t := s.appTemplate(app)
	switch {
	case app.Template == "":
		writeError(w, http.StatusNotFound, "this app was not installed from the catalog")
		return
	case t == nil:
		writeError(w, http.StatusConflict, "the catalog no longer has this app")
		return
	case t.Revision <= app.TemplateRevision:
		writeError(w, http.StatusConflict, "the app is up to date")
		return
	}
	if u := s.templateUpdate(app.ID); u != nil && u.State == "backing_up" {
		writeError(w, http.StatusConflict, "an update is already running")
		return
	}
	env, err := s.appEnv(app)
	if err != nil {
		s.internalError(w, r, "read app environment", err)
		return
	}
	newEnv, added, err := t.Complete(env, req.Inputs)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, ok := newEnv[catalog.MailHostKey]; t.Mail && !ok {
		mailApp, err := s.db.MailApp(r.Context())
		if err != nil {
			writeError(w, http.StatusConflict, "the new version uses the mail server: set it up under Mail first")
			return
		}
		newEnv[catalog.MailHostKey] = mailApp.Hostname
	}
	cf, err := s.parseCompose(r.Context(), []byte(t.Compose), newEnv, t.Expose.Service, t.Expose.Port)
	if err != nil {
		s.internalError(w, r, "parse template "+t.ID, err)
		return
	}
	if len(app.Replicas) > 0 && len(cf.project.Volumes) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "the new version keeps data in volumes, but the app runs on several servers: remove the replicas first")
		return
	}
	u := &templateUpdate{Revision: t.Revision, State: "backing_up"}
	if req.Backup {
		target := appTarget(app)
		policy, err := s.db.BackupPolicy(r.Context(), target.BackupTarget)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "choose where to back it up under Backups first, or update without a backup")
			return
		} else if err != nil {
			s.internalError(w, r, "load backup policy", err)
			return
		}
		if !s.backupsAvailable(w, r, app.ServerID) {
			return
		}
		b, err := s.startBackup(r.Context(), target, policy, "manual", &p.user.ID)
		var de *deployError
		switch {
		case errors.As(err, &de):
			writeError(w, de.status, de.msg)
			return
		case errors.Is(err, store.ErrBackupInProgress):
			writeError(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			s.internalError(w, r, "start backup", err)
			return
		}
		u.BackupID = b.ID
	}
	s.setTemplateUpdate(app.ID, u)
	s.audit(r, &p.user.ID, "app.template_update", map[string]any{"app_id": app.ID, "template": t.ID,
		"from": app.TemplateRevision, "to": t.Revision, "backup_id": u.BackupID, "added": added})
	go s.runTemplateUpdate(app.ID, t, newEnv, cf, u, &p.user.ID, clientIP(r))
	writeJSON(w, http.StatusAccepted, map[string]any{"update": u})
}

// runTemplateUpdate waits for the backup, then applies the new template
// and deploys it. A failed backup leaves the app as it was.
func (s *Server) runTemplateUpdate(appID int64, t *catalog.Template, env map[string]string, cf *composeFile,
	u *templateUpdate, actor *int64, ip string) {
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout+time.Minute)
	defer cancel()
	fail := func(msg string) {
		s.setTemplateUpdate(appID, &templateUpdate{Revision: u.Revision, State: "failed", BackupID: u.BackupID, Error: msg})
		s.log.Warn("template update", "app_id", appID, "err", msg)
	}
	if u.BackupID != 0 {
		target := store.BackupTarget{AppID: appID}
		for {
			b, err := s.db.BackupByID(ctx, target, u.BackupID)
			if err != nil {
				fail("cannot follow the backup: " + err.Error())
				return
			}
			if b.Status == "succeeded" {
				break
			}
			if b.Status != "running" {
				fail("the backup failed, so nothing was changed: " + b.Error)
				return
			}
			select {
			case <-ctx.Done():
				fail("the backup took too long")
				return
			case <-time.After(time.Second):
			}
		}
	}
	app, err := s.db.AppByID(ctx, appID)
	if err != nil {
		fail("the app is gone")
		return
	}
	b, _ := json.Marshal(env)
	sealed := s.box.Seal(b, envAD(app.ID))
	if err := s.db.SetAppEnv(ctx, app.ID, sealed); err != nil {
		fail(err.Error())
		return
	}
	app.EnvSealed = sealed
	if err := s.saveCompose(ctx, app, cf); err != nil {
		fail(err.Error())
		return
	}
	if err := s.db.SetAppTemplateRevision(ctx, app.ID, t.Revision); err != nil {
		fail(err.Error())
		return
	}
	app.TemplateRevision = t.Revision
	if _, err := s.startDeployment(ctx, app, deployOptions{actorID: actor, ip: ip, trigger: "update"}); err != nil {
		fail("the template was applied, but the deployment did not start: " + err.Error())
		return
	}
	s.setTemplateUpdate(appID, nil)
}
