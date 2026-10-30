package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

// An app can run on several servers (replicas): the same containers on
// each, behind the same domains. DNS answers with the address of every
// server whose copy passes its health check, and certificates come through
// the DNS-01 challenge (an HTTP challenge could reach another server).
// Replicas have no shared state, so only stateless apps qualify: from an
// image, a git repository, or a compose file without volumes (each server
// runs all its services; a job runs once per server and deployment).

// placementView is a server an app runs on.
type placementView struct {
	ServerID    int64  `json:"server_id"`
	Primary     bool   `json:"primary"`
	HealthCheck int64  `json:"health_check,omitempty"`
	Status      string `json:"status,omitempty"` // of the health check
}

func viewPlacements(ps []store.AppPlacement) []placementView {
	out := make([]placementView, len(ps))
	for i, p := range ps {
		out[i] = placementView{ServerID: p.ServerID, Primary: p.Primary, HealthCheck: p.HealthCheck, Status: p.CheckStatus}
	}
	return out
}

// replicaProblem says why app cannot run on several servers ("" if it can).
func (s *Server) replicaProblem(ctx context.Context, app *store.App, domains []string) (string, error) {
	switch {
	case app.Source != store.SourceImage && app.Source != store.SourceGit && app.Source != store.SourceCompose:
		return "only apps from an image, a git repository or a compose file can run on several servers", nil
	case len(app.Volumes) > 0:
		return "an app with volumes keeps its data on one server: remove them to run it on several", nil
	case app.IsCompose() && len(app.Summary.Volumes) > 0:
		return "the compose file keeps data in volumes (" + strings.Join(app.Summary.Volumes, ", ") +
			"), which stay on one server: remove them to run the app on several", nil
	case len(domains) == 0:
		return "add the app's domains first: traffic reaches the servers through them", nil
	}
	dbs, err := s.db.LinkedDatabaseNames(ctx, app.ID)
	if err != nil {
		return "", err
	}
	if len(dbs) > 0 {
		return "the app uses a database on its server (" + strings.Join(dbs, ", ") + "): unlink it to run the app on several servers", nil
	}
	return s.domainsOutsideZones(ctx, domains)
}

// domainsOutsideZones explains which domains are not in a zone under
// Basalt's DNS: failover and DNS-01 need them there.
func (s *Server) domainsOutsideZones(ctx context.Context, domains []string) (string, error) {
	zones, err := s.db.DNSZones(ctx)
	if err != nil {
		return "", err
	}
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	var missing []string
	for _, d := range domains {
		if dns.ZoneOf(strings.TrimPrefix(d, "*."), names) == "" {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		return "on several servers the domains must be in a zone under DNS here (for failover and certificates): " +
			strings.Join(missing, ", "), nil
	}
	return "", nil
}

// replicaCheck is the health check of an app's copy on srv: HTTPS through
// that server's proxy, with the app's domain.
func replicaCheck(app *store.App, srv *store.Server) (*store.HealthCheck, error) {
	return placementCheck("app", app.ID, app.Name, app.Domains, srv)
}

// placementCheck is the health check of the copy of a site or app (kind)
// on srv.
func placementCheck(kind string, id int64, name string, domains []string, srv *store.Server) (*store.HealthCheck, error) {
	addrs := srv.DNSAddrs()
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no known public address: set it on the server's page", srv.Name)
	}
	host := domains[0]
	if proxy.IsWildcard(host) {
		host = "basalt-check" + strings.TrimPrefix(host, "*")
	}
	port := srv.HTTPSPort
	if port == 0 {
		port = 443
	}
	check := fmt.Sprintf("%s-%d-%s-on-%s", kind, id, name, srv.Name)
	if len(check) > 63 {
		check = check[:63]
	}
	check = strings.TrimRight(check, ".-_")
	// Insecure: it checks that the copy answers, not the certificate (a
	// new replica has none yet).
	return &store.HealthCheck{Name: check, Kind: agentproto.HealthHTTPS, Host: addrs[0].String(), Port: port, Path: "/",
		HostHeader: host, Insecure: true, Interval: 30, Timeout: 5}, nil
}

type replicaRequest struct {
	ServerID int64 `json:"server_id"`
}

func (s *Server) handleAddReplica(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	var req replicaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg, err := s.replicaProblem(r.Context(), app, app.AllDomains()); err != nil {
		s.internalError(w, r, "check app", err)
		return
	} else if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if req.ServerID == app.ServerID || slices.Contains(app.Replicas, req.ServerID) {
		writeError(w, http.StatusConflict, "the app already runs on this server")
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
		writeError(w, http.StatusUnprocessableEntity, "Docker is not installed on "+srv.Name)
		return
	}
	check, err := replicaCheck(app, srv)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var primaryCheck *store.HealthCheck
	if len(app.Replicas) == 0 {
		primary, err := s.db.ServerByID(r.Context(), app.ServerID)
		if err != nil {
			s.internalError(w, r, "load server", err)
			return
		}
		if primaryCheck, err = replicaCheck(app, primary); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	err = s.db.AddAppReplica(r.Context(), app.ID, srv.ID, check, primaryCheck)
	switch {
	case errors.Is(err, store.ErrReplicaExists):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "a health check named "+check.Name+" exists: rename it under Monitoring")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "app not found")
		return
	case err != nil && strings.Contains(err.Error(), "already served"):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "add replica", err)
		return
	}
	s.audit(r, &p.user.ID, "app.replica_added", map[string]any{"app_id": app.ID, "server_id": srv.ID, "check_id": check.ID})
	s.healthChanged(r.Context())
	// Every route of the app now takes its certificates through DNS-01.
	s.PushProxy(r.Context(), app.ServerID)
	s.PushProxy(r.Context(), srv.ID)
	s.syncAutoDNS(r.Context())
	resp := map[string]any{"deploying": false}
	if app, err = s.db.AppByID(r.Context(), app.ID); err == nil {
		resp["app"] = s.viewAppFull(app)
		// A deployed app runs its current version there too.
		if deps, err := s.db.Deployments(r.Context(), app.ID, 1); err == nil && len(deps) > 0 {
			if _, err := s.startDeployment(r.Context(), app, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "replica"}); err != nil {
				resp["deploy_error"] = err.Error()
			} else {
				resp["deploying"] = true
			}
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleRemoveReplica(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil || !slices.Contains(app.Replicas, sid) {
		writeError(w, http.StatusNotFound, "the app has no replica on this server")
		return
	}
	if err := s.db.RemoveAppReplica(r.Context(), app.ID, sid); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the app has no replica on this server")
		return
	} else if err != nil {
		s.internalError(w, r, "remove replica", err)
		return
	}
	s.audit(r, &p.user.ID, "app.replica_removed", map[string]any{"app_id": app.ID, "server_id": sid})
	s.healthChanged(r.Context())
	s.syncAutoDNS(r.Context())
	s.PushProxy(r.Context(), sid)
	s.PushProxy(r.Context(), app.ServerID)
	resp := map[string]any{}
	if err := s.removeAppFrom(r.Context(), sid, app.ID, true, nil); err != nil {
		resp["cleanup_error"] = err.Error()
	}
	if app, err = s.db.AppByID(r.Context(), app.ID); err == nil {
		resp["app"] = s.viewAppFull(app)
	}
	writeJSON(w, http.StatusOK, resp)
}

// appServer is the server of ?server= (one the app runs on), by default
// its primary one.
func appServer(w http.ResponseWriter, r *http.Request, app *store.App) (int64, bool) {
	v := r.URL.Query().Get("server")
	if v == "" {
		return app.ServerID, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || (id != app.ServerID && !slices.Contains(app.Replicas, id)) {
		writeError(w, http.StatusUnprocessableEntity, "the app does not run on this server")
		return 0, false
	}
	return id, true
}
