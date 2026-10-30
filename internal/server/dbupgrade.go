package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dbengine"
	"github.com/wiefix/basalt/internal/store"
)

// Major upgrades of container databases: a deployment of the newer
// version (optionally after a backup). PostgreSQL's data is dumped and
// loaded into a new cluster by the agent during that deployment
// (agentproto.DBUpgrade); the others read their old files. The app's
// version changes only when the deployment succeeds, and no rollback goes
// back to an older version afterwards.

// dbUpgrade is an upgrade waiting for its backup, or one that failed
// before its deployment started (the deployment reports for itself).
type dbUpgrade struct {
	Version  string `json:"version"`
	State    string `json:"state"` // backing_up, failed
	BackupID int64  `json:"backup_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) dbUpgradeState(appID int64) *dbUpgrade {
	s.tplMu.Lock()
	defer s.tplMu.Unlock()
	if u := s.dbUpgrades[appID]; u != nil {
		c := *u
		return &c
	}
	return nil
}

func (s *Server) setDBUpgrade(appID int64, u *dbUpgrade) {
	s.tplMu.Lock()
	defer s.tplMu.Unlock()
	if u == nil {
		delete(s.dbUpgrades, appID)
	} else {
		s.dbUpgrades[appID] = u
	}
}

// databaseUpgrade adds to a database's deployment of version to (newer
// than its own) what the agent needs to move the data (PostgreSQL).
func (s *Server) databaseUpgrade(app *store.App, to string, spec *agentproto.AppSpec) error {
	e, ok := dbengine.Get(app.Engine)
	if !ok {
		return errors.New("unknown database engine " + app.Engine)
	}
	if !slices.Contains(e.UpgradeTargets(app.EngineVersion), to) {
		return errors.New("this database cannot be upgraded to that version")
	}
	if !e.DumpUpgrade() {
		return nil
	}
	env, err := s.appEnv(app)
	if err != nil {
		return err
	}
	from, err := e.Service(app.Name, app.EngineVersion, env, dbengine.Limits{MemoryMB: app.MemoryMB, CPUs: app.CPUs})
	if err != nil {
		return err
	}
	spec.DBUpgrade = &agentproto.DBUpgrade{Service: app.Name, From: from, To: to}
	return nil
}

type dbUpgradeView struct {
	Version string   `json:"version"`
	Targets []string `json:"targets"` // versions it can move to
	// Dump: the data is copied into a new cluster (the database is down
	// meanwhile, for as long as the copy takes).
	Dump    bool       `json:"dump"`
	Backups bool       `json:"backups"` // the database has a backup policy
	Upgrade *dbUpgrade `json:"upgrade"`
}

// handleDatabaseUpgradeInfo reports the versions a database can be
// upgraded to and an upgrade under way.
func (s *Server) handleDatabaseUpgradeInfo(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	e, ok := dbengine.Get(app.Engine)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "unknown database engine")
		return
	}
	_, err := s.db.BackupPolicy(r.Context(), appTarget(app).BackupTarget)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "load backup policy", err)
		return
	}
	writeJSON(w, http.StatusOK, dbUpgradeView{Version: app.EngineVersion, Targets: nonNil(e.UpgradeTargets(app.EngineVersion)),
		Dump: e.DumpUpgrade(), Backups: err == nil, Upgrade: s.dbUpgradeState(app.ID)})
}

type dbUpgradeRequest struct {
	Version string `json:"version"`
	// Backup backs up the database first (needs a backup policy); the
	// upgrade starts only if the backup succeeds.
	Backup bool `json:"backup"`
}

// dbUpgradeStarted: the deployment at once, or the upgrade waiting for
// its backup.
type dbUpgradeStarted struct {
	Upgrade    *dbUpgrade      `json:"upgrade"`
	Deployment *deploymentView `json:"deployment"`
}

// handleDatabaseUpgrade moves a database to a newer major version.
func (s *Server) handleDatabaseUpgrade(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.databaseApp(w, r)
	if !ok {
		return
	}
	var req dbUpgradeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	e, ok := dbengine.Get(app.Engine)
	if !ok || !slices.Contains(e.UpgradeTargets(app.EngineVersion), req.Version) {
		writeError(w, http.StatusUnprocessableEntity, "this database cannot be upgraded to that version")
		return
	}
	if u := s.dbUpgradeState(app.ID); u != nil && u.State == "backing_up" {
		writeError(w, http.StatusConflict, "an upgrade is already under way")
		return
	}
	if !s.agents.Connected(app.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	u := &dbUpgrade{Version: req.Version, State: "backing_up"}
	if req.Backup {
		if u.BackupID, ok = s.backupFirst(w, r, p, app); !ok {
			return
		}
	}
	s.audit(r, &p.user.ID, "database.upgrade", map[string]any{"app_id": app.ID, "from": app.EngineVersion, "to": req.Version,
		"backup_id": u.BackupID})
	if u.BackupID == 0 {
		dep, err := s.startDeployment(r.Context(), app, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "upgrade", upgradeTo: req.Version})
		var de *deployError
		if errors.As(err, &de) {
			writeError(w, de.status, de.msg)
			return
		}
		if err != nil {
			s.internalError(w, r, "start deployment", err)
			return
		}
		s.setDBUpgrade(app.ID, nil)
		dv := viewDeployment(dep, false)
		writeJSON(w, http.StatusAccepted, dbUpgradeStarted{Deployment: &dv})
		return
	}
	s.setDBUpgrade(app.ID, u)
	go s.runDBUpgrade(app.ID, *u, &p.user.ID, clientIP(r))
	writeJSON(w, http.StatusAccepted, dbUpgradeStarted{Upgrade: u})
}

// runDBUpgrade waits for the backup, then deploys the newer version.
func (s *Server) runDBUpgrade(appID int64, u dbUpgrade, actor *int64, ip string) {
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout+time.Minute)
	defer cancel()
	fail := func(msg string) {
		u.State, u.Error = "failed", msg
		s.setDBUpgrade(appID, &u)
		s.log.Warn("database upgrade", "app_id", appID, "err", msg)
	}
	if err := s.waitBackup(ctx, appID, u.BackupID); err != nil {
		fail(err.Error())
		return
	}
	app, err := s.db.AppByID(ctx, appID)
	if err != nil {
		fail("the database is gone")
		return
	}
	if _, err := s.startDeployment(ctx, app, deployOptions{actorID: actor, ip: ip, trigger: "upgrade", upgradeTo: u.Version}); err != nil {
		fail("the backup succeeded, but the deployment did not start: " + err.Error())
		return
	}
	s.setDBUpgrade(appID, nil)
}
