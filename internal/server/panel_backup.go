package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/restic"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/store"
)

// Backups of the panel itself: a consistent copy of its database and the
// master key encrypted with the owner's recovery passphrase, written by
// the panel process with restic to <storage>/panel. `basalt server
// recover` turns such a snapshot back into a panel.

const panelBackupHost = "basalt-panel"

// Files in a panel snapshot.
const (
	PanelBackupDB      = "basalt.db"
	PanelBackupKey     = "master-key.json"
	PanelBackupReadme  = "RESTORE.txt"
	panelBackupStaging = "panel-backup"
	panelBackupMaxRuns = 30
)

const restoreText = `Basalt panel backup
===================

This snapshot holds the panel's database (basalt.db) and its master key
encrypted with the recovery passphrase (master-key.json). Without the
passphrase the secrets in the database cannot be read.

To restore the panel on a new server:

 1. Install the panel:  sh install.sh panel --domain <same domain>
 2. Stop it:            systemctl stop basalt-server
 3. Fetch the backup:   restic -r <repository>/panel restore latest --target /root/basalt-restore
    (the repository password is shown under Backups -> Password in the old panel;
    keep a copy of it outside the panel)
 4. Recover:            basalt server recover --data-dir /var/lib/basalt/server \
                          --from /root/basalt-restore/<path of this folder>
    It asks for the recovery passphrase.
 5. Start it:           systemctl start basalt-server

Agents reconnect by themselves: the panel's certificate authority is part
of the database.
`

// panelRepo is the panel's repository: <storage path>/panel.
func (s *Server) panelRepo(st *store.BackupStorage) (restic.Repo, error) {
	spec, err := s.repoAt(st, "panel")
	if err != nil {
		return restic.Repo{}, err
	}
	if spec.Kind == agentproto.StorageLocal {
		if err := os.MkdirAll(spec.Path, 0o700); err != nil {
			return restic.Repo{}, fmt.Errorf("create the backup directory (it must be writable by the panel's user): %w", err)
		}
	}
	return restic.FromSpec(spec, filepath.Join(s.dataDir, "backup-ssh"))
}

func (s *Server) panelRunner(line func(string)) *restic.Runner {
	return &restic.Runner{Binary: s.restic, CacheDir: filepath.Join(s.dataDir, "restic-cache"), Line: line}
}

type panelBackupView struct {
	ID         int64      `json:"id"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StorageID  int64      `json:"storage_id,omitempty"`
	Snapshot   string     `json:"snapshot,omitempty"`
	Bytes      uint64     `json:"bytes"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Log        *string    `json:"log,omitempty"`
}

func viewPanelBackup(b *store.PanelBackup, withLog bool) panelBackupView {
	v := panelBackupView{ID: b.ID, Trigger: b.Trigger, Status: b.Status, Error: b.Error, StorageID: b.StorageID,
		Snapshot: b.Snapshot, Bytes: b.Bytes, CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt}
	if withLog {
		v.Log = &b.Log
	}
	return v
}

func (s *Server) handlePanelBackup(w http.ResponseWriter, r *http.Request, p *principal) {
	ctx := r.Context()
	resp := map[string]any{"recovery": map[string]any{"set": false}, "policy": nil}
	if _, at, err := s.db.RecoveryKey(ctx); err == nil {
		resp["recovery"] = map[string]any{"set": true, "set_at": at}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "load recovery key", err)
		return
	}
	if pol, err := s.db.PanelBackupPolicy(ctx); err == nil {
		resp["policy"] = s.viewPolicy(pol)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "load panel backup policy", err)
		return
	}
	list, err := s.db.PanelBackups(ctx, panelBackupMaxRuns)
	if err != nil {
		s.internalError(w, r, "list panel backups", err)
		return
	}
	out := make([]panelBackupView, len(list))
	for i, b := range list {
		out[i] = viewPanelBackup(b, false)
	}
	resp["backups"] = out
	v, err := s.panelRunner(nil).Version(ctx)
	resp["restic"] = v
	if err != nil {
		resp["restic_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

type recoveryRequest struct {
	Password   string `json:"password"` // the owner's, to confirm
	Passphrase string `json:"passphrase"`
}

// handleSetRecovery stores the master key encrypted with a new recovery
// passphrase. Owners only, after confirming their password; the
// passphrase itself is not kept.
func (s *Server) handleSetRecovery(w http.ResponseWriter, r *http.Request, p *principal) {
	var req recoveryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ok, _, err := s.hasher.Verify(r.Context(), p.user.PasswordHash, req.Password)
	if err != nil {
		s.internalError(w, r, "verify password", err)
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "wrong password")
		return
	}
	if len(req.Passphrase) < secrets.MinPassphrase || len(req.Passphrase) > 1024 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the recovery passphrase must have %d to 1024 characters", secrets.MinPassphrase))
		return
	}
	if strings.Contains(strings.ToLower(req.Passphrase), strings.ToLower(req.Password)) && len(req.Password) >= 6 {
		writeError(w, http.StatusUnprocessableEntity, "do not reuse your password in the recovery passphrase")
		return
	}
	wrapped, err := s.box.WrapKey(req.Passphrase)
	if err != nil {
		s.internalError(w, r, "wrap master key", err)
		return
	}
	if err := s.db.SetRecoveryKey(r.Context(), wrapped, s.now()); err != nil {
		s.internalError(w, r, "store recovery key", err)
		return
	}
	s.audit(r, &p.user.ID, "panel.recovery_passphrase_set", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetPanelBackupPolicy(w http.ResponseWriter, r *http.Request, p *principal) {
	var req policyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	pol, msg := s.checkPolicy(r, req)
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if err := s.db.SetPanelBackupPolicy(r.Context(), pol, s.now()); err != nil {
		s.internalError(w, r, "set panel backup policy", err)
		return
	}
	s.audit(r, &p.user.ID, "backup_policy.set", map[string]any{"resource": "panel", "storage_id": req.StorageID, "schedule": req.Schedule})
	saved, err := s.db.PanelBackupPolicy(r.Context())
	if err != nil {
		s.internalError(w, r, "load panel backup policy", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": s.viewPolicy(saved)})
}

// checkPolicy validates a policy request: a policy, or why it is refused.
func (s *Server) checkPolicy(r *http.Request, req policyRequest) (*store.BackupPolicy, string) {
	switch req.Schedule {
	case store.ScheduleManual, store.ScheduleHourly, store.ScheduleDaily:
	default:
		return nil, `schedule must be "manual", "hourly" or "daily"`
	}
	if req.AtMinute < 0 || req.AtMinute >= 24*60 {
		return nil, "time of day must be between 00:00 and 23:59"
	}
	keep := agentproto.Keep{Last: req.KeepLast, Daily: req.KeepDaily, Weekly: req.KeepWeekly, Monthly: req.KeepMonthly}
	if err := keep.Validate(); err != nil {
		return nil, err.Error()
	}
	if _, err := s.db.BackupStorageByID(r.Context(), req.StorageID); errors.Is(err, store.ErrNotFound) {
		return nil, "storage not found"
	} else if err != nil {
		return nil, "could not load the storage"
	}
	return &store.BackupPolicy{StorageID: req.StorageID, Schedule: req.Schedule, AtMinute: req.AtMinute,
		KeepLast: keep.Last, KeepDaily: keep.Daily, KeepWeekly: keep.Weekly, KeepMonthly: keep.Monthly}, ""
}

func (s *Server) handleDeletePanelBackupPolicy(w http.ResponseWriter, r *http.Request, p *principal) {
	if err := s.db.DeletePanelBackupPolicy(r.Context()); err != nil {
		s.internalError(w, r, "delete panel backup policy", err)
		return
	}
	s.audit(r, &p.user.ID, "backup_policy.deleted", map[string]any{"resource": "panel"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRunPanelBackup(w http.ResponseWriter, r *http.Request, p *principal) {
	b, err := s.startPanelBackup(r.Context(), "manual", &p.user.ID)
	var de *deployError
	switch {
	case errors.As(err, &de):
		writeError(w, de.status, de.msg)
		return
	case errors.Is(err, store.ErrBackupInProgress):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "start panel backup", err)
		return
	}
	s.audit(r, &p.user.ID, "backup.started", map[string]any{"resource": "panel", "backup_id": b.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"backup": viewPanelBackup(b, false)})
}

func (s *Server) handleGetPanelBackup(w http.ResponseWriter, r *http.Request, p *principal) {
	id, _ := strconv.ParseInt(r.PathValue("bid"), 10, 64)
	b, err := s.db.PanelBackupByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load panel backup", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup": viewPanelBackup(b, true)})
}

// startPanelBackup checks that a backup can be made and runs it in the
// background.
func (s *Server) startPanelBackup(ctx context.Context, trigger string, actor *int64) (*store.PanelBackup, error) {
	pol, err := s.db.PanelBackupPolicy(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "choose where the panel is backed up first"}
	}
	if err != nil {
		return nil, err
	}
	wrapped, _, err := s.db.RecoveryKey(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "the owner must set a recovery passphrase first"}
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.panelRunner(nil).Version(ctx); err != nil {
		return nil, &deployError{status: http.StatusConflict, msg: err.Error()}
	}
	st, err := s.db.BackupStorageByID(ctx, pol.StorageID)
	if err != nil {
		return nil, err
	}
	b := &store.PanelBackup{StorageID: st.ID, Trigger: trigger, ActorID: actor}
	if err := s.db.CreatePanelBackup(ctx, b); err != nil {
		return nil, err
	}
	go s.runPanelBackup(b, st, pol, wrapped)
	return b, nil
}

func (s *Server) runPanelBackup(b *store.PanelBackup, st *store.BackupStorage, pol *store.BackupPolicy, wrapped []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
	defer cancel()
	lg := &deployLogLines{}
	snapshot, bytes, err := s.panelBackup(ctx, st, pol, wrapped, lg)
	msg := ""
	if err != nil {
		msg = err.Error()
		lg.printf("Backup failed: %v", err)
	} else {
		lg.printf("Backup finished")
	}
	if err := s.db.FinishPanelBackup(context.Background(), b.ID, err == nil, truncate(msg, 2000), lg.String(), snapshot, bytes); err != nil {
		s.log.Error("record panel backup", "err", err)
	}
	action := "backup.succeeded"
	if err != nil {
		action = "backup.failed"
		s.notify(notify.Message{Event: EventBackupFailed, Title: "Backup of the panel failed", Text: truncate(msg, 1000), Link: "/backups"})
	}
	if err := s.db.Audit(context.Background(), nil, action, "", map[string]any{"resource": "panel", "backup_id": b.ID,
		"error": truncate(msg, 500)}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	s.log.Info("panel backup finished", "backup_id", b.ID, "ok", err == nil)
}

// panelBackup stages the database copy, the encrypted key and the restore
// notes, and stores them as one snapshot.
func (s *Server) panelBackup(ctx context.Context, st *store.BackupStorage, pol *store.BackupPolicy, wrapped []byte, lg *deployLogLines) (string, uint64, error) {
	staging := filepath.Join(s.dataDir, panelBackupStaging)
	if err := os.RemoveAll(staging); err != nil {
		return "", 0, err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(staging)
	lg.printf("Copying the database")
	if err := s.db.SnapshotTo(ctx, filepath.Join(staging, PanelBackupDB)); err != nil {
		return "", 0, fmt.Errorf("copy the database: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, PanelBackupKey), wrapped, 0o600); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(staging, PanelBackupReadme), []byte(restoreText), 0o600); err != nil {
		return "", 0, err
	}
	repo, err := s.panelRepo(st)
	if err != nil {
		return "", 0, err
	}
	r := s.panelRunner(func(l string) { lg.printf("restic: %s", l) })
	lg.printf("Opening the repository")
	if created, err := r.Init(ctx, repo); err != nil {
		return "", 0, err
	} else if created {
		lg.printf("Created a new repository")
	}
	sum, err := r.Backup(ctx, repo, restic.BackupOptions{Host: panelBackupHost, Tags: []string{"res=panel"}, Paths: []string{staging}})
	if err != nil {
		return "", 0, err
	}
	lg.printf("Snapshot %s: %d bytes", sum.SnapshotID[:8], sum.BytesProcessed)
	if err := r.Forget(ctx, repo, panelBackupHost, []string{"res=panel"}, restic.Keep(keepOf(pol))); err != nil {
		return sum.SnapshotID, sum.BytesProcessed, err
	}
	if s.now().Sub(s.db.PanelLastPrune(ctx)) > pruneEvery {
		lg.printf("Freeing the space of removed snapshots")
		if err := r.Prune(ctx, repo); err != nil {
			return sum.SnapshotID, sum.BytesProcessed, err
		}
		s.db.SetPanelPruned(ctx, s.now())
	}
	return sum.SnapshotID, sum.BytesProcessed, nil
}

// deployLogLines collects timestamped log lines.
type deployLogLines struct{ strings.Builder }

func (l *deployLogLines) printf(format string, a ...any) {
	fmt.Fprintf(l, "%s %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// runDuePanelBackup starts the panel's scheduled backup when due.
func (s *Server) runDuePanelBackup(ctx context.Context) {
	pol, err := s.db.PanelBackupPolicy(ctx)
	if err != nil || !backupDue(pol, s.now()) {
		return
	}
	if _, err := s.startPanelBackup(ctx, "schedule", nil); err != nil && !errors.Is(err, store.ErrBackupInProgress) {
		s.log.Error("start scheduled panel backup", "err", err)
		// Record the miss so the slot is not retried every 30 seconds.
		b := &store.PanelBackup{StorageID: pol.StorageID, Trigger: "schedule"}
		if s.db.CreatePanelBackup(ctx, b) == nil {
			s.db.FinishPanelBackup(ctx, b.ID, false, truncate(err.Error(), 2000), "", "", 0)
		}
	}
}
