package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dbengine"
	"github.com/wiefix/basalt/internal/mailserver"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

const (
	backupTimeout   = 12 * time.Hour
	pruneEvery      = 7 * 24 * time.Hour
	backupCheckTime = 2 * time.Minute
)

func storageSecretAD(id int64) []byte   { return fmt.Appendf(nil, "backup-storage-secret:%d", id) }
func storagePasswordAD(id int64) []byte { return fmt.Appendf(nil, "backup-storage-password:%d", id) }

type storageView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	store.StorageConfig
	// SSHPublicKey must be authorized on the SFTP server.
	SSHPublicKey string    `json:"ssh_public_key,omitempty"`
	Policies     int       `json:"policies"`
	CreatedAt    time.Time `json:"created_at"`
}

func viewStorage(st *store.BackupStorage) storageView {
	return storageView{ID: st.ID, Name: st.Name, Kind: st.Kind, StorageConfig: st.Config,
		SSHPublicKey: st.SSHPublicKey, Policies: st.Policies, CreatedAt: st.CreatedAt}
}

type storageRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	store.StorageConfig
	SecretKey string `json:"secret_key"` // S3; empty on update keeps it
}

// settings validates a storage request; the agent checks the same rules.
func (req *storageRequest) settings(kind string) (store.StorageConfig, error) {
	c := req.StorageConfig
	c.Endpoint, c.Bucket, c.Region = strings.TrimSpace(c.Endpoint), strings.TrimSpace(c.Bucket), strings.TrimSpace(c.Region)
	c.AccessKey, c.Host, c.User = strings.TrimSpace(c.AccessKey), strings.TrimSpace(c.Host), strings.TrimSpace(c.User)
	c.Path = strings.TrimSpace(c.Path)
	if !serverNameRE.MatchString(strings.TrimSpace(req.Name)) {
		return c, errors.New("name must be 1-63 characters: letters, digits, dot, dash, underscore")
	}
	probe := agentproto.BackupRepo{Kind: kind, Password: "x", Endpoint: c.Endpoint, Bucket: c.Bucket, Region: c.Region,
		AccessKey: c.AccessKey, SecretKey: "x", Host: c.Host, Port: c.Port, User: c.User,
		SSHKey: "-----BEGIN OPENSSH PRIVATE KEY-----", Path: c.Path}
	switch kind {
	case agentproto.StorageS3:
		c.Host, c.Port, c.User = "", 0, ""
		c.Endpoint = strings.TrimSuffix(c.Endpoint, "/")
		probe.Endpoint = c.Endpoint
	case agentproto.StorageSFTP:
		c.Endpoint, c.Bucket, c.Region, c.AccessKey = "", "", "", ""
		if c.Port == 0 {
			c.Port, probe.Port = 22, 22
		}
	case agentproto.StorageLocal:
		c = store.StorageConfig{Path: c.Path}
	}
	return c, probe.Validate()
}

func (s *Server) handleListStorages(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.BackupStorages(r.Context())
	if err != nil {
		s.internalError(w, r, "list backup storages", err)
		return
	}
	out := make([]storageView, len(list))
	for i, st := range list {
		out[i] = viewStorage(st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"storages": out})
}

func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request, p *principal) {
	var req storageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cfg, err := req.settings(req.Kind)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	st := &store.BackupStorage{Name: strings.TrimSpace(req.Name), Kind: req.Kind, Config: cfg}
	var secret []byte
	switch req.Kind {
	case agentproto.StorageS3:
		secret = []byte(strings.TrimSpace(req.SecretKey))
		if len(secret) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "the secret key is required")
			return
		}
	case agentproto.StorageSFTP:
		// The storage gets its own key; the user authorizes it on the host.
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			s.internalError(w, r, "generate SSH key", err)
			return
		}
		blk, err := ssh.MarshalPrivateKey(priv, "basalt backups "+st.Name)
		if err != nil {
			s.internalError(w, r, "encode SSH key", err)
			return
		}
		sshPub, _ := ssh.NewPublicKey(pub)
		secret = pem.EncodeToMemory(blk)
		st.SSHPublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " basalt-backups-" + st.Name
	}
	// Repository password: without it, backups cannot be read.
	password := rand.Text() + rand.Text()
	err = s.db.CreateBackupStorage(r.Context(), st, func(id int64) ([]byte, []byte) {
		var sealed []byte
		if secret != nil {
			sealed = s.box.Seal(secret, storageSecretAD(id))
		}
		return sealed, s.box.Seal([]byte(password), storagePasswordAD(id))
	})
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a storage with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create backup storage", err)
		return
	}
	s.audit(r, &p.user.ID, "backup_storage.created", map[string]any{"storage_id": st.ID, "name": st.Name, "kind": st.Kind})
	writeJSON(w, http.StatusCreated, map[string]any{"storage": viewStorage(st)})
}

func (s *Server) pathStorage(w http.ResponseWriter, r *http.Request) (*store.BackupStorage, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	st, err := s.db.BackupStorageByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "storage not found")
		return nil, false
	}
	if err != nil {
		s.internalError(w, r, "load backup storage", err)
		return nil, false
	}
	return st, true
}

// handleUpdateStorage changes a storage's name, location or S3 secret.
// Moving it does not move existing backups.
func (s *Server) handleUpdateStorage(w http.ResponseWriter, r *http.Request, p *principal) {
	st, ok := s.pathStorage(w, r)
	if !ok {
		return
	}
	var req storageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cfg, err := req.settings(st.Kind)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	st.Name, st.Config, st.SecretSealed = strings.TrimSpace(req.Name), cfg, nil
	if v := strings.TrimSpace(req.SecretKey); v != "" && st.Kind == agentproto.StorageS3 {
		st.SecretSealed = s.box.Seal([]byte(v), storageSecretAD(st.ID))
	}
	err = s.db.UpdateBackupStorage(r.Context(), st)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a storage with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "update backup storage", err)
		return
	}
	s.audit(r, &p.user.ID, "backup_storage.updated", map[string]any{"storage_id": st.ID, "name": st.Name})
	writeJSON(w, http.StatusOK, map[string]any{"storage": viewStorage(st)})
}

func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request, p *principal) {
	st, ok := s.pathStorage(w, r)
	if !ok {
		return
	}
	if pol, err := s.db.PanelBackupPolicy(r.Context()); err == nil && pol.StorageID == st.ID {
		writeError(w, http.StatusConflict, "the panel is backed up to this storage; change that first")
		return
	}
	err := s.db.DeleteBackupStorage(r.Context(), st.ID)
	if errors.Is(err, store.ErrStorageInUse) {
		writeError(w, http.StatusConflict, "sites or apps are backed up to this storage; change their backup settings first")
		return
	}
	if err != nil {
		s.internalError(w, r, "delete backup storage", err)
		return
	}
	s.audit(r, &p.user.ID, "backup_storage.deleted", map[string]any{"storage_id": st.ID, "name": st.Name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStoragePassword reveals the repository password, which restores
// backups without the panel (admins only, audited).
func (s *Server) handleStoragePassword(w http.ResponseWriter, r *http.Request, p *principal) {
	st, ok := s.pathStorage(w, r)
	if !ok {
		return
	}
	pw, err := s.box.Open(st.PasswordSealed, storagePasswordAD(st.ID))
	if err != nil {
		s.internalError(w, r, "decrypt repository password", err)
		return
	}
	s.audit(r, &p.user.ID, "backup_storage.password_viewed", map[string]any{"storage_id": st.ID})
	writeJSON(w, http.StatusOK, map[string]any{"password": string(pw)})
}

// repoFor describes the repository a server keeps in a storage.
func (s *Server) repoFor(st *store.BackupStorage, serverID int64) (agentproto.BackupRepo, error) {
	return s.repoAt(st, "server-"+strconv.FormatInt(serverID, 10))
}

// repoAt describes the repository in folder sub of a storage.
func (s *Server) repoAt(st *store.BackupStorage, sub string) (agentproto.BackupRepo, error) {
	pw, err := s.box.Open(st.PasswordSealed, storagePasswordAD(st.ID))
	if err != nil {
		return agentproto.BackupRepo{}, fmt.Errorf("decrypt repository password: %w", err)
	}
	var secret []byte
	if st.SecretSealed != nil {
		if secret, err = s.box.Open(st.SecretSealed, storageSecretAD(st.ID)); err != nil {
			return agentproto.BackupRepo{}, fmt.Errorf("decrypt storage secret: %w", err)
		}
	}
	c := st.Config
	repo := agentproto.BackupRepo{Kind: st.Kind, Endpoint: c.Endpoint, Bucket: c.Bucket, Region: c.Region,
		AccessKey: c.AccessKey, Host: c.Host, Port: c.Port, User: c.User, Password: string(pw),
		Path: path.Join(c.Path, sub)}
	if strings.HasPrefix(c.Path, "/") {
		repo.Path = "/" + strings.TrimPrefix(repo.Path, "/")
	} else {
		repo.Path = strings.TrimPrefix(repo.Path, "/")
	}
	switch st.Kind {
	case agentproto.StorageS3:
		repo.SecretKey = string(secret)
	case agentproto.StorageSFTP:
		repo.SSHKey = string(secret)
	}
	return repo, nil
}

type checkStorageRequest struct {
	ServerID int64 `json:"server_id"`
}

// handleCheckStorage opens (or creates) the server's repository in the
// storage, proving the location and credentials work.
func (s *Server) handleCheckStorage(w http.ResponseWriter, r *http.Request, p *principal) {
	st, ok := s.pathStorage(w, r)
	if !ok {
		return
	}
	var req checkStorageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.backupsAvailable(w, r, req.ServerID) {
		return
	}
	repo, err := s.repoFor(st, req.ServerID)
	if err != nil {
		s.internalError(w, r, "prepare repository", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupCheckTime)
	defer cancel()
	if err := s.agents.Call(ctx, req.ServerID, agentproto.TypeBackupCheck, repo, nil); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": agentErrorText(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// backupsAvailable checks that a server is online and has restic.
func (s *Server) backupsAvailable(w http.ResponseWriter, r *http.Request, serverID int64) bool {
	v, err := s.db.ServerRestic(r.Context(), serverID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "server not found")
	case err != nil:
		s.internalError(w, r, "load server", err)
	case !s.agents.Connected(serverID):
		writeError(w, http.StatusConflict, "the server is offline")
	case v == "":
		writeError(w, http.StatusConflict, "restic is not installed on the server (re-run the installer, or install restic 0.17 or newer)")
	default:
		return true
	}
	return false
}

// --- sites and apps ---

// backupTarget is a site or app being backed up.
type backupTarget struct {
	store.BackupTarget
	ServerID int64
	Name     string
	site     *store.Site
	app      *store.App
}

func (t *backupTarget) resource() string {
	if t.site != nil {
		return "site-" + strconv.FormatInt(t.site.ID, 10)
	}
	return "app-" + strconv.FormatInt(t.app.ID, 10)
}

func siteTarget(site *store.Site) *backupTarget {
	return &backupTarget{BackupTarget: store.BackupTarget{SiteID: site.ID}, ServerID: site.ServerID, Name: site.Name, site: site}
}

func appTarget(app *store.App) *backupTarget {
	return &backupTarget{BackupTarget: store.BackupTarget{AppID: app.ID}, ServerID: app.ServerID, Name: app.Name, app: app}
}

// targetResolver finds the target a request is about.
type targetResolver func(w http.ResponseWriter, r *http.Request) (*backupTarget, bool)

func (s *Server) pathSiteTarget(w http.ResponseWriter, r *http.Request) (*backupTarget, bool) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return nil, false
	}
	return siteTarget(site), true
}

func (s *Server) pathAppTarget(w http.ResponseWriter, r *http.Request) (*backupTarget, bool) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return nil, false
	}
	return appTarget(app), true
}

// backupRoutes registers the backup API of sites or apps under prefix
// ("/api/v1/sites/{id}").
func (s *Server) backupRoutes(mux *router, prefix string, t targetResolver) {
	mux.Handle("GET "+prefix+"/backups", s.requireRole(store.RoleViewer, s.handleBackups(t)))
	mux.Handle("POST "+prefix+"/backups", s.requireRole(store.RoleMember, s.handleRunBackup(t)))
	mux.Handle("GET "+prefix+"/backups/{bid}", s.requireRole(store.RoleMember, s.handleGetBackup(t)))
	mux.Handle("GET "+prefix+"/backups/{bid}/stream", s.requireRole(store.RoleMember, s.handleBackupStream(t)))
	mux.Handle("PUT "+prefix+"/backup-policy", s.requireRole(store.RoleAdmin, s.handleSetBackupPolicy(t)))
	mux.Handle("DELETE "+prefix+"/backup-policy", s.requireRole(store.RoleAdmin, s.handleDeleteBackupPolicy(t)))
	mux.Handle("GET "+prefix+"/snapshots", s.requireRole(store.RoleMember, s.handleSnapshots(t)))
	mux.Handle("POST "+prefix+"/restore", s.requireRole(store.RoleAdmin, s.handleRestore(t)))
	mux.Handle("GET "+prefix+"/snapshots/{sid}/files", s.requireRole(store.RoleMember, s.handleSnapshotFiles(t)))
	mux.Handle("GET "+prefix+"/snapshots/{sid}/download", s.requireRole(store.RoleMember, s.handleSnapshotDownload(t)))
	mux.Handle("POST "+prefix+"/snapshots/{sid}/restore-copy", s.requireRole(store.RoleMember, s.handleRestoreCopy(t)))
}

type handler = func(w http.ResponseWriter, r *http.Request, p *principal)

type policyView struct {
	StorageID   int64     `json:"storage_id"`
	Schedule    string    `json:"schedule"`
	AtMinute    int       `json:"at_minute"`
	KeepLast    int       `json:"keep_last"`
	KeepDaily   int       `json:"keep_daily"`
	KeepWeekly  int       `json:"keep_weekly"`
	KeepMonthly int       `json:"keep_monthly"`
	NextRun     time.Time `json:"next_run,omitzero"`
}

func (s *Server) viewPolicy(p *store.BackupPolicy) *policyView {
	if p == nil {
		return nil
	}
	v := &policyView{StorageID: p.StorageID, Schedule: p.Schedule, AtMinute: p.AtMinute, KeepLast: p.KeepLast,
		KeepDaily: p.KeepDaily, KeepWeekly: p.KeepWeekly, KeepMonthly: p.KeepMonthly}
	v.NextRun = nextBackup(p, s.now())
	return v
}

type backupView struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Trigger    string          `json:"trigger"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	StorageID  int64           `json:"storage_id,omitempty"`
	Snapshots  json.RawMessage `json:"snapshots" openapi:"backupParts"`
	Bytes      uint64          `json:"bytes"`
	DataAdded  uint64          `json:"data_added"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Log        *string         `json:"log,omitempty"`
}

// backupPartView describes (for the API description) a part of a run's
// snapshots: a backup's snapshot (id, sizes) or a restore's (snapshot).
type backupPartView struct {
	Item      agentproto.BackupItem `json:"item"`
	ID        string                `json:"id,omitempty"`
	Snapshot  string                `json:"snapshot,omitempty"`
	Bytes     uint64                `json:"bytes,omitempty"`
	DataAdded uint64                `json:"data_added,omitempty"`
}

func viewBackup(b *store.Backup, withLog bool) backupView {
	v := backupView{ID: b.ID, Kind: b.Kind, Trigger: b.Trigger, Status: b.Status, Error: b.Error, StorageID: b.StorageID,
		Snapshots: b.Snapshots, Bytes: b.Bytes, DataAdded: b.DataAdded, CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt}
	if len(v.Snapshots) == 0 {
		v.Snapshots = json.RawMessage("[]") // a run just started
	}
	if withLog {
		v.Log = &b.Log
	}
	return v
}

// items lists what a backup of the target holds now.
func (s *Server) items(ctx context.Context, t *backupTarget) ([]agentproto.BackupItem, error) {
	if t.site != nil {
		items := []agentproto.BackupItem{{Kind: agentproto.ItemSiteFiles, SiteID: t.site.ID}}
		dbs, err := s.db.Databases(ctx, t.site.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range dbs {
			items = append(items, agentproto.BackupItem{Kind: agentproto.ItemMariaDB, Database: d.Name})
		}
		return items, nil
	}
	app := t.app
	var items []agentproto.BackupItem
	volume := func(name string) {
		items = append(items, agentproto.BackupItem{Kind: agentproto.ItemVolume, AppID: app.ID, Volume: name})
	}
	switch app.Source {
	case store.SourceCompose:
		for _, v := range app.Summary.Volumes {
			volume(v)
		}
	case store.SourceMail:
		for _, v := range mailserver.Volumes {
			volume(v)
		}
	case store.SourceDatabase:
		e, ok := dbengine.Get(app.Engine)
		if !ok {
			return nil, errors.New("unknown database engine")
		}
		if !e.SQL {
			volume(dbengine.Volume) // Redis: its data files
			break
		}
		env, err := s.appEnv(app)
		if err != nil {
			return nil, err
		}
		c := e.CredentialsFrom(env)
		items = append(items, agentproto.BackupItem{Kind: agentproto.ItemDBDump, AppID: app.ID, Service: app.Name,
			Engine: e.ID, Database: c.Database, User: c.User})
	default:
		for _, v := range app.Volumes {
			volume(v.Name)
		}
	}
	return items, nil
}

// itemKey identifies a part of a target across backups.
func itemKey(it agentproto.BackupItem) string { return it.Kind + ":" + it.Volume + ":" + it.Database }

func (s *Server) handleBackups(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, ok := resolve(w, r)
		if !ok {
			return
		}
		policy, err := s.db.BackupPolicy(r.Context(), t.BackupTarget)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load backup policy", err)
			return
		}
		list, err := s.db.Backups(r.Context(), t.BackupTarget, 50)
		if err != nil {
			s.internalError(w, r, "list backups", err)
			return
		}
		items, err := s.items(r.Context(), t)
		if err != nil {
			s.internalError(w, r, "list backup items", err)
			return
		}
		restic, _ := s.db.ServerRestic(r.Context(), t.ServerID)
		out := make([]backupView, len(list))
		for i, b := range list {
			out[i] = viewBackup(b, false)
		}
		writeJSON(w, http.StatusOK, map[string]any{"policy": s.viewPolicy(policy), "backups": out, "restic": restic,
			"items": nonNil(items)})
	}
}

type policyRequest struct {
	StorageID   int64  `json:"storage_id"`
	Schedule    string `json:"schedule"`
	AtMinute    int    `json:"at_minute"`
	KeepLast    int    `json:"keep_last"`
	KeepDaily   int    `json:"keep_daily"`
	KeepWeekly  int    `json:"keep_weekly"`
	KeepMonthly int    `json:"keep_monthly"`
}

func (s *Server) handleSetBackupPolicy(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, ok := resolve(w, r)
		if !ok {
			return
		}
		var req policyRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		switch req.Schedule {
		case store.ScheduleManual, store.ScheduleHourly, store.ScheduleDaily:
		default:
			writeError(w, http.StatusUnprocessableEntity, `schedule must be "manual", "hourly" or "daily"`)
			return
		}
		if req.AtMinute < 0 || req.AtMinute >= 24*60 {
			writeError(w, http.StatusUnprocessableEntity, "time of day must be between 00:00 and 23:59")
			return
		}
		keep := agentproto.Keep{Last: req.KeepLast, Daily: req.KeepDaily, Weekly: req.KeepWeekly, Monthly: req.KeepMonthly}
		if err := keep.Validate(); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if _, err := s.db.BackupStorageByID(r.Context(), req.StorageID); errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "storage not found")
			return
		} else if err != nil {
			s.internalError(w, r, "load backup storage", err)
			return
		}
		pol := &store.BackupPolicy{SiteID: t.SiteID, AppID: t.AppID, StorageID: req.StorageID, Schedule: req.Schedule,
			AtMinute: req.AtMinute, KeepLast: keep.Last, KeepDaily: keep.Daily, KeepWeekly: keep.Weekly, KeepMonthly: keep.Monthly}
		if err := s.db.SetBackupPolicy(r.Context(), pol); err != nil {
			s.internalError(w, r, "set backup policy", err)
			return
		}
		s.audit(r, &p.user.ID, "backup_policy.set", map[string]any{"resource": t.resource(), "storage_id": req.StorageID, "schedule": req.Schedule})
		saved, err := s.db.BackupPolicy(r.Context(), t.BackupTarget)
		if err != nil {
			s.internalError(w, r, "load backup policy", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"policy": s.viewPolicy(saved)})
	}
}

func (s *Server) handleDeleteBackupPolicy(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, ok := resolve(w, r)
		if !ok {
			return
		}
		if err := s.db.DeleteBackupPolicy(r.Context(), t.BackupTarget); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "delete backup policy", err)
			return
		}
		s.audit(r, &p.user.ID, "backup_policy.deleted", map[string]any{"resource": t.resource()})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// handleRunBackup backs the target up now with its policy's storage and
// retention.
func (s *Server) handleRunBackup(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, ok := resolve(w, r)
		if !ok {
			return
		}
		policy, err := s.db.BackupPolicy(r.Context(), t.BackupTarget)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "choose where to back it up first")
			return
		}
		if err != nil {
			s.internalError(w, r, "load backup policy", err)
			return
		}
		if !s.backupsAvailable(w, r, t.ServerID) {
			return
		}
		b, err := s.startBackup(r.Context(), t, policy, "manual", &p.user.ID)
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
		s.audit(r, &p.user.ID, "backup.started", map[string]any{"resource": t.resource(), "backup_id": b.ID})
		writeJSON(w, http.StatusAccepted, map[string]any{"backup": viewBackup(b, false)})
	}
}

func keepOf(p *store.BackupPolicy) agentproto.Keep {
	return agentproto.Keep{Last: p.KeepLast, Daily: p.KeepDaily, Weekly: p.KeepWeekly, Monthly: p.KeepMonthly}
}

// startBackup records a backup of the target and runs it in the background.
func (s *Server) startBackup(ctx context.Context, t *backupTarget, policy *store.BackupPolicy, trigger string, actor *int64) (*store.Backup, error) {
	items, err := s.items(ctx, t)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "this app keeps no data: it has no volumes to back up"}
	}
	st, err := s.db.BackupStorageByID(ctx, policy.StorageID)
	if err != nil {
		return nil, err
	}
	repo, err := s.repoFor(st, t.ServerID)
	if err != nil {
		return nil, err
	}
	last, err := s.db.LastPrune(ctx, st.ID, t.ServerID)
	if err != nil {
		return nil, err
	}
	req := agentproto.BackupRequest{Repo: repo, Resource: t.resource(), Items: items, Keep: keepOf(policy),
		Prune: s.now().Sub(last) > pruneEvery}
	b := &store.Backup{SiteID: t.SiteID, AppID: t.AppID, ServerID: t.ServerID, StorageID: st.ID, Kind: "backup",
		Trigger: trigger, ActorID: actor}
	if err := s.db.CreateBackup(ctx, b); err != nil {
		return nil, err
	}
	live := s.backupLive.start(b.ID)
	go s.runBackup(b, req, live)
	return b, nil
}

func (s *Server) runBackup(b *store.Backup, req agentproto.BackupRequest, live *liveLog) {
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
	defer cancel()
	var resp agentproto.BackupResponse
	err := s.agents.Stream(ctx, b.ServerID, agentproto.TypeBackupRun, req, &resp, func(raw json.RawMessage) error {
		var c agentproto.LogChunk
		if json.Unmarshal(raw, &c) == nil {
			live.append(c.Text)
		}
		return nil
	})
	if err != nil {
		resp = agentproto.BackupResponse{Error: "server unreachable: " + err.Error()}
	}
	var bytes, added uint64
	for _, sn := range resp.Snapshots {
		bytes += sn.Bytes
		added += sn.DataAdded
	}
	if err := s.db.FinishBackup(context.Background(), b.ID, resp.OK, truncate(resp.Error, 2000), resp.Log,
		resp.Snapshots, bytes, added); err != nil {
		s.log.Error("record backup", "err", err, "backup_id", b.ID)
	}
	if resp.OK && req.Prune {
		if err := s.db.SetPruned(context.Background(), b.StorageID, b.ServerID, s.now()); err != nil {
			s.log.Error("record prune", "err", err)
		}
	}
	s.backupLive.end(b.ID)
	action := "backup.failed"
	if resp.OK {
		action = "backup.succeeded"
	} else {
		name, link := s.resourceName(b)
		s.notify(notify.Message{Event: EventBackupFailed, Title: "Backup of " + name + " failed",
			Text: truncate(resp.Error, 1000), Link: link})
	}
	if err := s.db.Audit(context.Background(), nil, action, "", map[string]any{
		"backup_id": b.ID, "resource": req.Resource, "error": truncate(resp.Error, 500),
	}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	s.log.Info("backup finished", "backup_id", b.ID, "resource", req.Resource, "ok", resp.OK)
}

func (s *Server) pathBackup(w http.ResponseWriter, r *http.Request, resolve targetResolver) (*backupTarget, *store.Backup, bool) {
	t, ok := resolve(w, r)
	if !ok {
		return nil, nil, false
	}
	id, _ := strconv.ParseInt(r.PathValue("bid"), 10, 64)
	b, err := s.db.BackupByID(r.Context(), t.BackupTarget, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "backup not found")
		return nil, nil, false
	}
	if err != nil {
		s.internalError(w, r, "load backup", err)
		return nil, nil, false
	}
	return t, b, true
}

func (s *Server) handleGetBackup(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		if _, b, ok := s.pathBackup(w, r, resolve); ok {
			writeJSON(w, http.StatusOK, map[string]any{"backup": viewBackup(b, true)})
		}
	}
}

// handleBackupStream sends a run's log as server-sent events while it runs
// ("log" {text}), then its outcome ("end", a backup).
func (s *Server) handleBackupStream(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, b, ok := s.pathBackup(w, r, resolve)
		if !ok {
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
		sent := false
		sendLog := func(text string) error {
			sent = true
			return ev.send("log", agentproto.LogChunk{Text: text})
		}
		for b.Status == "running" {
			if live := s.backupLive.get(b.ID); live != nil {
				if live.follow(ctx, sendLog) != nil {
					return
				}
			} else {
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
			if b, err = s.db.BackupByID(ctx, t.BackupTarget, b.ID); err != nil {
				return
			}
		}
		if !sent && b.Log != "" {
			sendLog(b.Log)
		}
		ev.send("end", viewBackup(b, false))
	}
}

// handleSnapshots lists the target's snapshots in its policy's storage.
func (s *Server) handleSnapshots(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, ok := resolve(w, r)
		if !ok {
			return
		}
		snaps, _, ok := s.snapshots(w, r, t)
		if ok {
			writeJSON(w, http.StatusOK, map[string]any{"snapshots": nonNil(snaps)})
		}
	}
}

// snapshots asks the agent for the target's snapshots; it writes the error
// response itself.
func (s *Server) snapshots(w http.ResponseWriter, r *http.Request, t *backupTarget) ([]agentproto.SnapshotInfo, agentproto.BackupRepo, bool) {
	repo, ok := s.targetRepo(w, r, t)
	if !ok {
		return nil, agentproto.BackupRepo{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), backupCheckTime)
	defer cancel()
	var resp agentproto.SnapshotsResponse
	if err := s.agents.Call(ctx, t.ServerID, agentproto.TypeBackupSnapshots,
		agentproto.SnapshotsRequest{Repo: repo, Resource: t.resource()}, &resp); err != nil {
		writeAgentError(w, err)
		return nil, agentproto.BackupRepo{}, false
	}
	return resp.Snapshots, repo, true
}

// targetRepo is the repository the target's backups go to, on a connected
// server; it writes the error response itself.
func (s *Server) targetRepo(w http.ResponseWriter, r *http.Request, t *backupTarget) (agentproto.BackupRepo, bool) {
	policy, err := s.db.BackupPolicy(r.Context(), t.BackupTarget)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "it has no backup storage")
		return agentproto.BackupRepo{}, false
	}
	if err != nil {
		s.internalError(w, r, "load backup policy", err)
		return agentproto.BackupRepo{}, false
	}
	st, err := s.db.BackupStorageByID(r.Context(), policy.StorageID)
	if err != nil {
		s.internalError(w, r, "load backup storage", err)
		return agentproto.BackupRepo{}, false
	}
	repo, err := s.repoFor(st, t.ServerID)
	if err != nil {
		s.internalError(w, r, "prepare repository", err)
		return agentproto.BackupRepo{}, false
	}
	if !s.agents.Connected(t.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return agentproto.BackupRepo{}, false
	}
	return repo, true
}

type restoreRequest struct {
	Snapshots []string `json:"snapshots"`
}

// handleRestore restores snapshots of the target's parts: site files
// exactly as they were (newer files are deleted), databases from dumps,
// volumes (the containers using them stop meanwhile).
func (s *Server) handleRestore(resolve targetResolver) handler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		t, ok := resolve(w, r)
		if !ok {
			return
		}
		var req restoreRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if len(req.Snapshots) == 0 || len(req.Snapshots) > 200 {
			writeError(w, http.StatusUnprocessableEntity, "choose what to restore")
			return
		}
		if !s.backupsAvailable(w, r, t.ServerID) {
			return
		}
		snaps, repo, ok := s.snapshots(w, r, t)
		if !ok {
			return
		}
		current, err := s.items(r.Context(), t)
		if err != nil {
			s.internalError(w, r, "list backup items", err)
			return
		}
		byKey := map[string]agentproto.BackupItem{}
		for _, it := range current {
			byKey[itemKey(it)] = it
		}
		var items []agentproto.RestoreItem
		seen := map[string]bool{}
		for _, id := range req.Snapshots {
			i := slices.IndexFunc(snaps, func(sn agentproto.SnapshotInfo) bool { return sn.ID == id })
			if i < 0 {
				writeError(w, http.StatusUnprocessableEntity, "snapshot not found: "+truncate(id, 64))
				return
			}
			key := itemKey(snaps[i].Item)
			if seen[key] {
				writeError(w, http.StatusUnprocessableEntity, "choose one snapshot of each part")
				return
			}
			seen[key] = true
			// Restore into what exists now, as the backup would back it up.
			it, ok := byKey[key]
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, "this part no longer exists here: "+key)
				return
			}
			items = append(items, agentproto.RestoreItem{Item: it, Snapshot: id})
		}
		b := &store.Backup{SiteID: t.SiteID, AppID: t.AppID, ServerID: t.ServerID, Kind: "restore", Trigger: "manual", ActorID: &p.user.ID}
		if policy, err := s.db.BackupPolicy(r.Context(), t.BackupTarget); err == nil {
			b.StorageID = policy.StorageID
		}
		if err := s.db.CreateBackup(r.Context(), b); errors.Is(err, store.ErrBackupInProgress) {
			writeError(w, http.StatusConflict, err.Error())
			return
		} else if err != nil {
			s.internalError(w, r, "start restore", err)
			return
		}
		s.audit(r, &p.user.ID, "restore.started", map[string]any{"resource": t.resource(), "backup_id": b.ID, "snapshots": req.Snapshots})
		live := s.backupLive.start(b.ID)
		go s.runRestore(b, agentproto.RestoreRequest{Repo: repo, Resource: t.resource(), Items: items}, live)
		writeJSON(w, http.StatusAccepted, map[string]any{"backup": viewBackup(b, false)})
	}
}

func (s *Server) runRestore(b *store.Backup, req agentproto.RestoreRequest, live *liveLog) {
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
	defer cancel()
	var resp agentproto.RestoreResponse
	err := s.agents.Stream(ctx, b.ServerID, agentproto.TypeBackupRestore, req, &resp, func(raw json.RawMessage) error {
		var c agentproto.LogChunk
		if json.Unmarshal(raw, &c) == nil {
			live.append(c.Text)
		}
		return nil
	})
	if err != nil {
		resp = agentproto.RestoreResponse{Error: "server unreachable: " + err.Error()}
	}
	if err := s.db.FinishBackup(context.Background(), b.ID, resp.OK, truncate(resp.Error, 2000), resp.Log, req.Items, 0, 0); err != nil {
		s.log.Error("record restore", "err", err, "backup_id", b.ID)
	}
	s.backupLive.end(b.ID)
	action := "restore.failed"
	if resp.OK {
		action = "restore.succeeded"
	}
	if err := s.db.Audit(context.Background(), nil, action, "", map[string]any{
		"backup_id": b.ID, "resource": req.Resource, "error": truncate(resp.Error, 500),
	}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
}

// --- schedule ---

// nextBackup is when a policy's next scheduled backup is due (zero for
// manual policies). A slot that passed without a backup is due now.
func nextBackup(p *store.BackupPolicy, now time.Time) time.Time {
	slot := lastSlot(p, now)
	if slot.IsZero() {
		return time.Time{}
	}
	if slot.After(p.LastScheduled) && slot.After(p.UpdatedAt) {
		return slot
	}
	if p.Schedule == store.ScheduleHourly {
		return slot.Add(time.Hour)
	}
	return slot.AddDate(0, 0, 1)
}

// lastSlot is the latest scheduled time not after now.
func lastSlot(p *store.BackupPolicy, now time.Time) time.Time {
	now = now.UTC()
	var slot time.Time
	switch p.Schedule {
	case store.ScheduleDaily:
		slot = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(time.Duration(p.AtMinute) * time.Minute)
		if slot.After(now) {
			slot = slot.AddDate(0, 0, -1)
		}
	case store.ScheduleHourly:
		slot = now.Truncate(time.Hour).Add(time.Duration(p.AtMinute%60) * time.Minute)
		if slot.After(now) {
			slot = slot.Add(-time.Hour)
		}
	}
	return slot
}

// backupDue reports whether a scheduled backup should start now: its
// latest slot passed after the policy was set and no scheduled backup has
// started since. A slot missed while the server was offline runs once it
// is back.
func backupDue(p *store.BackupPolicy, now time.Time) bool {
	slot := lastSlot(p, now)
	return !slot.IsZero() && slot.After(p.LastScheduled) && slot.After(p.UpdatedAt)
}

func (s *Server) backupLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.runDueBackups(ctx)
		s.runDuePanelBackup(ctx)
		s.runDueChecks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) runDueBackups(ctx context.Context) {
	policies, err := s.db.ScheduledBackupPolicies(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("list backup policies", "err", err)
		}
		return
	}
	now := s.now()
	for _, p := range policies {
		if !backupDue(p, now) || !s.agents.Connected(p.ServerID) {
			continue
		}
		if v, err := s.db.ServerRestic(ctx, p.ServerID); err != nil || v == "" {
			continue
		}
		var t *backupTarget
		if p.SiteID > 0 {
			site, err := s.db.SiteByID(ctx, p.SiteID)
			if err != nil {
				continue
			}
			t = siteTarget(site)
		} else {
			app, err := s.db.AppByID(ctx, p.AppID)
			if err != nil {
				continue
			}
			t = appTarget(app)
		}
		if _, err := s.startBackup(ctx, t, p, "schedule", nil); err != nil && !errors.Is(err, store.ErrBackupInProgress) {
			s.log.Error("start scheduled backup", "err", err, "resource", t.resource())
			s.recordMissedBackup(ctx, t, p, err)
		}
	}
}

// recordMissedBackup records a scheduled backup that could not start (an
// app without volumes, say) as failed, and notifies: the slot counts as
// taken, so it is not retried every 30 seconds.
func (s *Server) recordMissedBackup(ctx context.Context, t *backupTarget, p *store.BackupPolicy, cause error) {
	msg := cause.Error()
	var de *deployError
	if !errors.As(cause, &de) {
		msg = "the backup could not start: " + msg
	}
	b := &store.Backup{SiteID: t.SiteID, AppID: t.AppID, ServerID: t.ServerID, StorageID: p.StorageID, Kind: "backup", Trigger: "schedule"}
	if err := s.db.CreateBackup(ctx, b); err != nil {
		return
	}
	if err := s.db.FinishBackup(ctx, b.ID, false, truncate(msg, 2000), "", nil, 0, 0); err != nil {
		s.log.Error("record backup", "err", err, "backup_id", b.ID)
	}
	name, link := s.resourceName(b)
	s.notify(notify.Message{Event: EventBackupFailed, Title: "Backup of " + name + " failed", Text: truncate(msg, 1000), Link: link})
}
