package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dbengine"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/store"
)

// Databases are apps (source "database") with one service the panel
// defines from the engine (internal/dbengine). Their credentials are the
// app's environment, generated at creation. A database is reachable from
// the apps linked to it: its container joins their networks, under its name.

func (s *Server) handleListEngines(w http.ResponseWriter, r *http.Request, p *principal) {
	writeJSON(w, http.StatusOK, map[string]any{"engines": dbengine.All()})
}

// databaseSettings validates the engine and version of a new database.
func databaseSettings(st store.AppSettings, engine, version string) (store.AppSettings, error) {
	e, ok := dbengine.Get(engine)
	if !ok {
		return st, errors.New("engine must be postgres, mysql, mariadb or redis")
	}
	if version == "" {
		version = e.Versions[0]
	}
	if !e.ValidVersion(version) {
		return st, errors.New(e.Name + " versions offered: " + strings.Join(e.Versions, ", "))
	}
	st.Engine, st.EngineVersion, st.Image = e.ID, version, e.Image(version)
	st.Port, st.Volumes = 0, nil
	return st, nil
}

// validDatabaseName checks a database name, which apps use as host name.
func validDatabaseName(name string) error {
	if !dbengine.NameRE.MatchString(name) || name == "app" {
		return errors.New("a database name is its host name for apps: 2-42 lowercase letters, digits and dashes, starting with a letter (not \"app\")")
	}
	return nil
}

// initDatabase generates the credentials of a new database.
func (s *Server) initDatabase(ctx context.Context, app *store.App) error {
	e, _ := dbengine.Get(app.Engine)
	b, _ := json.Marshal(e.Env(e.NewCredentials(app.Name)))
	sealed := s.box.Seal(b, envAD(app.ID))
	if err := s.db.SetAppEnv(ctx, app.ID, sealed); err != nil {
		return err
	}
	app.EnvSealed = sealed
	return nil
}

// databaseSpec prepares the deployment of a database; pinned are the
// images of the deployment rolled back to.
func (s *Server) databaseSpec(app *store.App, version string, pinned map[string]string, spec *agentproto.AppSpec) error {
	e, ok := dbengine.Get(app.Engine)
	if !ok {
		return errors.New("unknown database engine " + app.Engine)
	}
	env, err := s.appEnv(app)
	if err != nil {
		return err
	}
	svc, err := e.Service(app.Name, version, env, dbengine.Limits{MemoryMB: app.MemoryMB, CPUs: app.CPUs})
	if err != nil {
		return err
	}
	if d := pinned[svc.Name]; strings.Contains(d, "@sha256:") && docker.ValidRef(d) {
		svc.Image = d
	}
	spec.Services = []agentproto.ServiceSpec{svc}
	spec.ComposeVolumes = []string{dbengine.Volume}
	spec.Attach = app.Links
	return nil
}

func (s *Server) databaseApp(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	app, ok := s.pathApp(w, r)
	if ok && app.Source != store.SourceDatabase {
		writeError(w, http.StatusNotFound, "this app is not a database")
		return nil, false
	}
	return app, ok
}

type connectionView struct {
	Host        string               `json:"host"`
	Port        int                  `json:"port"`
	URL         string               `json:"url"`
	EnvVar      string               `json:"env_var"`
	Credentials dbengine.Credentials `json:"credentials"`
	// Public is how clients outside the server connect; null when the
	// database is reachable from linked apps only.
	Public *publicAccessView `json:"public"`
}

type publicAccessView struct {
	Port  int      `json:"port"`
	Allow []string `json:"allow"`
	// Host is the server's public address ("" when unknown) and URL the
	// connection URL with it.
	Host string `json:"host"`
	URL  string `json:"url"`
}

func (s *Server) connection(ctx context.Context, app *store.App) (connectionView, error) {
	e, ok := dbengine.Get(app.Engine)
	if !ok {
		return connectionView{}, errors.New("unknown database engine")
	}
	env, err := s.appEnv(app)
	if err != nil {
		return connectionView{}, err
	}
	c := e.CredentialsFrom(env)
	v := connectionView{Host: app.Name, Port: e.Port, URL: e.URL(app.Name, c), EnvVar: e.EnvVar, Credentials: c}
	if app.PublicPort > 0 {
		pub := &publicAccessView{Port: app.PublicPort, Allow: app.PublicAllow}
		if srv, err := s.db.ServerByID(ctx, app.ServerID); err == nil {
			pub.Host = serverHost(srv)
		}
		if pub.Host != "" {
			pub.URL = e.URLPort(pub.Host, app.PublicPort, c)
		}
		v.Public = pub
	}
	return v, nil
}

// serverHost is the address clients outside a server reach it at: its
// public address (IPv4 first), else the one its agent connects from.
func serverHost(srv *store.Server) string {
	addrs := srv.DNSAddrs()
	slices.SortStableFunc(addrs, func(a, b netip.Addr) int {
		if a.Is4() == b.Is4() {
			return 0
		}
		if a.Is4() {
			return -1
		}
		return 1
	})
	if len(addrs) > 0 {
		return addrs[0].String()
	}
	if a, err := netip.ParseAddr(srv.LastIP); err == nil {
		return a.Unmap().String()
	}
	return ""
}

type publicAccessRequest struct {
	// Port on the server; 0 makes the database private again.
	Port int `json:"port"`
	// Allow lists the addresses and CIDR prefixes that may connect
	// (0.0.0.0/0 and ::/0 for any).
	Allow []string `json:"allow"`
}

// reservedPublicPorts are Basalt's own ports, never given to a database.
var reservedPublicPorts = []int{2222, 8080, 8443}

// handleDatabasePublicAccess makes a database reachable from outside its
// server: its agent listens on the port and forwards connections from the
// allowed addresses to the container.
func (s *Server) handleDatabasePublicAccess(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	var req publicAccessRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var allow []string
	if req.Port != 0 {
		srv, err := s.db.ServerByID(r.Context(), app.ServerID)
		if err != nil {
			s.internalError(w, r, "load server", err)
			return
		}
		if req.Port < agentproto.MinDBExposePort || req.Port > 65535 {
			writeError(w, http.StatusUnprocessableEntity, "the port must be between 1024 and 65535")
			return
		}
		if slices.Contains(reservedPublicPorts, req.Port) || req.Port == srv.HTTPSPort || req.Port == srv.PeerPort {
			writeError(w, http.StatusUnprocessableEntity, "Basalt uses this port on the server")
			return
		}
		prefixes, err := agentproto.ParseAllow(req.Allow)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		for _, p := range prefixes {
			allow = append(allow, p.String())
		}
	}
	err := s.db.SetAppPublicAccess(r.Context(), app.ID, req.Port, allow)
	switch {
	case errors.Is(err, store.ErrPublicPortTaken):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "set public access", err)
		return
	}
	s.audit(r, &p.user.ID, "database.public_access", map[string]any{"database_id": app.ID, "port": req.Port, "allow": allow})
	resp := map[string]any{}
	if errs, err := s.PushDBExpose(r.Context(), app.ServerID); err != nil {
		var re *agentproto.RemoteError
		if errors.As(err, &re) {
			err = errors.New(re.Msg)
		}
		resp["apply_error"] = err.Error()
	} else if msg := errs[app.ID]; msg != "" {
		resp["apply_error"] = msg
	}
	if app, err = s.db.AppByID(r.Context(), app.ID); err != nil {
		s.internalError(w, r, "load app", err)
		return
	}
	conn, err := s.connection(r.Context(), app)
	if err != nil {
		s.internalError(w, r, "database connection", err)
		return
	}
	resp["connection"] = conn
	writeJSON(w, http.StatusOK, resp)
}

// PushDBExpose gives a server's agent the complete list of its public
// databases; it returns why entries could not listen, by app.
func (s *Server) PushDBExpose(ctx context.Context, serverID int64) (map[int64]string, error) {
	if !s.agents.Connected(serverID) {
		return nil, errors.New("the server is offline; the change applies when it connects")
	}
	apps, err := s.db.PublicDatabases(ctx, serverID)
	if err != nil {
		return nil, err // never send a partial list: the agent would close the others
	}
	req := agentproto.DBExposeApply{Entries: []agentproto.DBExpose{}}
	for _, a := range apps {
		e, ok := dbengine.Get(a.Engine)
		if !ok {
			continue
		}
		req.Entries = append(req.Entries, agentproto.DBExpose{AppID: a.ID, Service: a.Name, TargetPort: e.Port, Port: a.PublicPort, Allow: a.PublicAllow})
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var resp agentproto.DBExposeResponse
	if err := s.agents.Call(ctx, serverID, agentproto.TypeDBExpose, req, &resp); err != nil {
		return nil, err
	}
	errs := map[int64]string{}
	for _, st := range resp.Entries {
		if st.Error != "" {
			errs[st.AppID] = st.Error
		}
	}
	return errs, nil
}

// handleDatabaseConnection returns how linked apps connect: admins only,
// it holds the password.
func (s *Server) handleDatabaseConnection(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	c, err := s.connection(r.Context(), app)
	if err != nil {
		s.internalError(w, r, "database connection", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": c})
}

type linkRequest struct {
	AppID int64 `json:"app_id"`
	// EnvVar, when set, also stores the connection URL in this variable of
	// the app's environment (e.g. DATABASE_URL).
	EnvVar string `json:"env_var"`
}

// handleLinkApp makes the database reachable from an app.
func (s *Server) handleLinkApp(w http.ResponseWriter, r *http.Request, p *principal) {
	dbApp, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	var req linkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	target, err := s.db.AppByID(r.Context(), req.AppID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && target.ProjectID != dbApp.ProjectID:
		// An app of another project is not there for this database.
		writeError(w, http.StatusUnprocessableEntity, "app not found")
		return
	case err != nil:
		s.internalError(w, r, "load app", err)
		return
	case target.Source == store.SourceDatabase:
		writeError(w, http.StatusUnprocessableEntity, "a database cannot be linked to another database")
		return
	case target.ServerID != dbApp.ServerID:
		writeError(w, http.StatusUnprocessableEntity, "the app runs on another server")
		return
	case len(target.Replicas) > 0:
		writeError(w, http.StatusUnprocessableEntity, "the app runs on several servers; a database is reachable on one")
		return
	case len(dbApp.Links) >= agentproto.MaxAttach && !slices.Contains(dbApp.Links, target.ID):
		writeError(w, http.StatusUnprocessableEntity, "too many linked apps")
		return
	}
	if target.IsCompose() && slices.ContainsFunc(target.Summary.Services, func(c store.ComposeService) bool {
		return c.Name == dbApp.Name
	}) {
		writeError(w, http.StatusConflict, "the app has a service named "+dbApp.Name+"; the database would not be reachable by that name")
		return
	}
	envVar := strings.TrimSpace(req.EnvVar)
	var conn connectionView
	if envVar != "" {
		if err := agentproto.ValidateEnv(map[string]string{envVar: ""}); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if conn, err = s.connection(r.Context(), dbApp); err != nil {
			s.internalError(w, r, "database connection", err)
			return
		}
	}
	if err := s.db.LinkApp(r.Context(), dbApp.ID, target.ID); err != nil {
		s.internalError(w, r, "link app", err)
		return
	}
	if envVar != "" {
		env, err := s.appEnv(target)
		if err != nil {
			s.internalError(w, r, "decrypt app env", err)
			return
		}
		env[envVar] = conn.URL
		if err := agentproto.ValidateEnv(env); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		b, _ := json.Marshal(env)
		if err := s.db.SetAppEnv(r.Context(), target.ID, s.box.Seal(b, envAD(target.ID))); err != nil {
			s.internalError(w, r, "set app env", err)
			return
		}
	}
	s.audit(r, &p.user.ID, "database.linked", map[string]any{"database_id": dbApp.ID, "app_id": target.ID, "env_var": envVar})
	s.respondLinks(w, r, dbApp.ID)
}

func (s *Server) handleUnlinkApp(w http.ResponseWriter, r *http.Request, p *principal) {
	dbApp, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	appID, _ := strconv.ParseInt(r.PathValue("app"), 10, 64)
	err := s.db.UnlinkApp(r.Context(), dbApp.ID, appID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the app is not linked")
		return
	}
	if err != nil {
		s.internalError(w, r, "unlink app", err)
		return
	}
	s.audit(r, &p.user.ID, "database.unlinked", map[string]any{"database_id": dbApp.ID, "app_id": appID})
	s.respondLinks(w, r, dbApp.ID)
}

// respondLinks applies a database's links on its server and returns it.
func (s *Server) respondLinks(w http.ResponseWriter, r *http.Request, id int64) {
	app, err := s.db.AppByID(r.Context(), id)
	if err != nil {
		s.internalError(w, r, "load app", err)
		return
	}
	resp := map[string]any{"app": viewApp(app)}
	if err := s.applyLinks(r.Context(), app); err != nil {
		var re *agentproto.RemoteError
		if errors.As(err, &re) {
			err = errors.New(re.Msg)
		}
		resp["attach_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) applyLinks(ctx context.Context, app *store.App) error {
	if !s.agents.Connected(app.ServerID) {
		return errors.New("the server is offline; the links apply on the next deployment")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return s.agents.Call(ctx, app.ServerID, agentproto.TypeAppAttach, agentproto.AppAttach{AppID: app.ID, Attach: app.Links}, nil)
}

type dbPasswordRequest struct {
	// RedeployApps redeploys the linked apps whose environment held the
	// old connection URL, so they connect with the new password (default).
	RedeployApps *bool `json:"redeploy_apps"`
}

// handleDatabasePassword gives a container database a new password. SQL
// databases change it in place (connections already open stay); Redis is
// redeployed with it. Linked apps whose variables held the old connection
// URL get the new one.
func (s *Server) handleDatabasePassword(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	var req dbPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	e, ok := dbengine.Get(app.Engine)
	if !ok {
		s.internalError(w, r, "database engine", errors.New("unknown engine "+app.Engine))
		return
	}
	if !s.agents.Connected(app.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	env, err := s.appEnv(app)
	if err != nil {
		s.internalError(w, r, "decrypt app env", err)
		return
	}
	old := e.CredentialsFrom(env)
	next := old
	next.Password = rand.Text()
	if e.SQL {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeDBPassword, agentproto.ContainerDBPassword{
			AppID: app.ID, Service: app.Name, Engine: e.ID, Database: old.Database, User: old.User, Password: next.Password,
		}, nil); err != nil {
			writeAgentError(w, err)
			return
		}
	}
	for k, v := range e.Env(next) {
		env[k] = v
	}
	b, _ := json.Marshal(env)
	sealed := s.box.Seal(b, envAD(app.ID))
	if err := s.db.SetAppEnv(r.Context(), app.ID, sealed); err != nil {
		s.internalError(w, r, "set app env", err)
		return
	}
	app.EnvSealed = sealed
	resp := map[string]any{}
	if !e.SQL {
		// Redis takes its password from the command line: a new container.
		if _, err := s.startDeployment(r.Context(), app, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "manual"}); err != nil {
			resp["deploy_error"] = err.Error()
		}
	}

	oldURL, newURL := e.URL(app.Name, old), e.URL(app.Name, next)
	updated, redeploying := []string{}, []string{}
	for _, id := range app.Links {
		target, err := s.db.AppByID(r.Context(), id)
		if err != nil {
			continue
		}
		tenv, err := s.appEnv(target)
		if err != nil {
			continue
		}
		changed := false
		for k, v := range tenv {
			if v == oldURL {
				tenv[k], changed = newURL, true
			}
		}
		if !changed {
			continue
		}
		b, _ := json.Marshal(tenv)
		sealed := s.box.Seal(b, envAD(target.ID))
		if err := s.db.SetAppEnv(r.Context(), target.ID, sealed); err != nil {
			s.internalError(w, r, "set app env", err)
			return
		}
		// The deployment below reads the environment from target.
		target.EnvSealed = sealed
		updated = append(updated, target.Name)
		if req.RedeployApps == nil || *req.RedeployApps {
			if deps, err := s.db.Deployments(r.Context(), target.ID, 1); err == nil && len(deps) > 0 {
				if _, err := s.startDeployment(r.Context(), target, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "manual"}); err == nil {
					redeploying = append(redeploying, target.Name)
				}
			}
		}
	}
	s.audit(r, &p.user.ID, "database.password_changed", map[string]any{"database_id": app.ID, "apps_updated": updated})
	conn, err := s.connection(r.Context(), app)
	if err != nil {
		s.internalError(w, r, "database connection", err)
		return
	}
	resp["connection"], resp["updated_apps"], resp["redeploying"] = conn, updated, redeploying
	writeJSON(w, http.StatusOK, resp)
}
