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
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

const (
	deployTimeout  = 30 * time.Minute
	appCallTimeout = 30 * time.Second
	maxLogTail     = 5000
)

type appView struct {
	ID        int64                  `json:"id"`
	ServerID  int64                  `json:"server_id"`
	Name      string                 `json:"name"`
	Image     string                 `json:"image"`
	Port      int                    `json:"port"`
	Domains   []string               `json:"domains"`
	TLS       proxy.TLSMode          `json:"tls"`
	Volumes   []agentproto.AppVolume `json:"volumes"`
	MemoryMB  int                    `json:"memory_mb"`
	CPUs      float64                `json:"cpus"`
	CreatedAt time.Time              `json:"created_at"`
}

func viewApp(a *store.App) appView {
	vols := a.Volumes
	if vols == nil {
		vols = []agentproto.AppVolume{}
	}
	return appView{ID: a.ID, ServerID: a.ServerID, Name: a.Name, Image: a.Image, Port: a.Port, Domains: a.Domains,
		TLS: a.TLS, Volumes: vols, MemoryMB: a.MemoryMB, CPUs: a.CPUs, CreatedAt: a.CreatedAt}
}

type deploymentView struct {
	ID          int64      `json:"id"`
	Image       string     `json:"image"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	ImageDigest string     `json:"image_digest,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Log         *string    `json:"log,omitempty"`
}

func viewDeployment(d *store.Deployment, withLog bool) deploymentView {
	v := deploymentView{ID: d.ID, Image: d.Image, Status: d.Status, Error: d.Error, ImageDigest: d.ImageDigest,
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

// appSettingsRequest is shared by create and update.
type appSettingsRequest struct {
	Image    string                 `json:"image"`
	Port     int                    `json:"port"`
	Volumes  []agentproto.AppVolume `json:"volumes"`
	MemoryMB int                    `json:"memory_mb"`
	CPUs     float64                `json:"cpus"`
}

func (req appSettingsRequest) settings() (store.AppSettings, error) {
	st := store.AppSettings{Image: strings.TrimSpace(req.Image), Port: req.Port, Volumes: req.Volumes,
		MemoryMB: req.MemoryMB, CPUs: req.CPUs}
	switch {
	case !docker.ValidRef(st.Image):
		return st, errors.New("image must be a valid reference, e.g. nginx:1.27 or ghcr.io/org/app:v1")
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
	st, err := req.settings()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	domains, tls, err := normalizeAppDomains(req.Domains, req.TLS, st.Port)
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
	s.audit(r, &p.user.ID, "app.created", map[string]any{"app_id": app.ID, "name": app.Name, "image": app.Image})
	s.PushProxy(r.Context(), app.ServerID)
	writeJSON(w, http.StatusCreated, map[string]any{"app": viewApp(app), "sync": s.proxySync(r.Context(), app.ServerID)})
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request, p *principal) {
	apps, err := s.db.Apps(r.Context())
	if err != nil {
		s.internalError(w, r, "list apps", err)
		return
	}
	out := make([]appView, len(apps))
	for i, a := range apps {
		out[i] = viewApp(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": viewApp(app), "sync": s.proxySync(r.Context(), app.ServerID)})
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
	st, err := req.settings()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if st.Port == 0 && len(app.Domains) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "remove the app's domains before clearing its port")
		return
	}
	if err := s.db.UpdateAppSettings(r.Context(), app.ID, st); err != nil {
		s.internalError(w, r, "update app", err)
		return
	}
	s.audit(r, &p.user.ID, "app.updated", map[string]any{"app_id": app.ID, "image": st.Image, "port": st.Port})
	app.Image, app.Port, app.Volumes, app.MemoryMB, app.CPUs = st.Image, st.Port, st.Volumes, st.MemoryMB, st.CPUs
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
	image := app.Image
	if req.RollbackTo > 0 {
		prev, err := s.db.DeploymentByID(r.Context(), app.ID, req.RollbackTo)
		if err != nil || prev.Status != "succeeded" {
			writeError(w, http.StatusUnprocessableEntity, "only a successful deployment of this app can be rolled back to")
			return
		}
		image = prev.Image
		if docker.ValidRef(prev.ImageDigest) && strings.Contains(prev.ImageDigest, "@sha256:") {
			image = prev.ImageDigest
		}
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
	dep, err := s.db.CreateDeployment(r.Context(), app.ID, image, &p.user.ID)
	if errors.Is(err, store.ErrDeployInProgress) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, "create deployment", err)
		return
	}
	s.audit(r, &p.user.ID, "app.deploy_started", map[string]any{"app_id": app.ID, "deployment_id": dep.ID, "image": image})
	spec := agentproto.AppSpec{
		AppID: app.ID, DeploymentID: dep.ID, Image: image, Port: app.Port, Env: env,
		Volumes: app.Volumes, MemoryMB: app.MemoryMB, CPUs: app.CPUs,
	}
	// The deployment outlives the request; a panel restart marks it failed.
	// Its output can be followed live from now on.
	live := s.live.start(dep.ID)
	go s.runDeployment(app.ServerID, dep.ID, spec, live)
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment": viewDeployment(dep, false)})
}

func (s *Server) runDeployment(serverID, depID int64, spec agentproto.AppSpec, live *liveLog) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	var resp agentproto.AppDeployResponse
	err := s.agents.Stream(ctx, serverID, agentproto.TypeAppDeploy, spec, &resp, func(raw json.RawMessage) error {
		var c agentproto.LogChunk
		if json.Unmarshal(raw, &c) == nil {
			live.append(c.Text)
		}
		return nil
	})
	if err != nil {
		resp = agentproto.AppDeployResponse{Error: "server unreachable: " + err.Error()}
	}
	if err := s.db.FinishDeployment(context.Background(), depID, resp.OK, resp.Error, resp.Log, resp.ImageDigest); err != nil {
		s.log.Error("record deployment", "err", err, "deployment_id", depID)
	}
	// Followers now read the outcome from the database.
	s.live.end(depID)
	action := "app.deploy_failed"
	if resp.OK {
		action = "app.deployed"
	}
	if err := s.db.Audit(context.Background(), nil, action, "", map[string]any{
		"app_id": spec.AppID, "deployment_id": depID, "error": resp.Error,
	}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	s.log.Info("deployment finished", "app_id", spec.AppID, "deployment_id", depID, "ok", resp.OK)
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
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var st agentproto.AppStatus
	if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeAppStatus, agentproto.AppRequest{AppID: app.ID}, &st); err != nil {
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
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var resp agentproto.AppLogsResponse
	if err := s.agents.Call(ctx, app.ServerID, agentproto.TypeAppLogs, agentproto.AppRequest{AppID: app.ID, Tail: tail}, &resp); err != nil {
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
		agentproto.AppRequest{AppID: app.ID, Tail: tail}, nil, func(raw json.RawMessage) error {
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
		req := agentproto.AppRequest{AppID: app.ID, DeleteVolumes: volumes}
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
